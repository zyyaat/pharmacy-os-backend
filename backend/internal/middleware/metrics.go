// Package middleware — Prometheus metrics (R-9, api_level 103).
//
// The audit's scaling analysis is explicit: every capacity decision is
// blind without metrics ("مراقبة فعلية (R-9) لأن أي قرار هنا أعمى بلا
// metrics"). This file adds the HTTP surface:
//
//	http_requests_total{method,route,status}     — traffic + error rate
//	http_request_duration_seconds{method,route}  — latency distribution
//	http_requests_in_flight                      — current concurrency
//
// Cardinality discipline (this box is 1 vCPU / 1 GB — label explosion is a
// real outage class, not a style point): `route` is the gin ROUTE TEMPLATE
// ("UNMATCHED" for 404 scanners), `method` and `status` are closed sets.
// No path segments, no query strings, no headers. The Go runtime and
// process collectors (memory, GC, FDs, CPU) ride along on the default
// registry — exactly what the container analysis needs.
//
// /metrics is fail-closed: without METRICS_TOKEN the endpoint answers 404
// (feature off — same posture as /debug/schema); with a token it requires
// `Authorization: Bearer <token>`. The operational data (per-route rates,
// error mix) is not for anonymous edges.
package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	unmatchedRoute = "UNMATCHED"
	metricsPath    = "/metrics"
)

// latencyBuckets cover the server-side processing spread seen across the
// P1/P2 waves (reads ~50-450ms server-side, writes higher; tail room to
// 10s for heavy reports/uploads) without exploding series count.
var latencyBuckets = []float64{0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type metricsBundle struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inFlight prometheus.Gauge
	registry *prometheus.Registry
}

func newMetricsBundle(reg *prometheus.Registry) *metricsBundle {
	b := &metricsBundle{
		requests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "pharmacyos", Subsystem: "http", Name: "requests_total",
				Help: "HTTP requests processed, by method, route template and status code.",
			},
			[]string{"method", "route", "status"},
		),
		duration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "pharmacyos", Subsystem: "http", Name: "request_duration_seconds",
				Help:    "Server-side request latency by method and route template.",
				Buckets: latencyBuckets,
			},
			[]string{"method", "route"},
		),
		inFlight: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: "pharmacyos", Subsystem: "http", Name: "requests_in_flight",
				Help: "Requests currently being served.",
			},
		),
		registry: reg,
	}
	return b
}

func (b *metricsBundle) mustRegister() {
	b.registry.MustRegister(b.requests)
	b.registry.MustRegister(b.duration)
	b.registry.MustRegister(b.inFlight)
}

// defaultBundle registers once on the process-wide registry (go runtime +
// process collectors come with it). NewServer is also called by tests —
// a second MustRegister would panic, so the once-guard is load-bearing.
var (
	defaultBundleOnce sync.Once
	defaultBundle     *metricsBundle
)

func defaultMetrics() *metricsBundle {
	defaultBundleOnce.Do(func() {
		defaultBundle = newMetricsBundle(prometheus.DefaultRegisterer.(*prometheus.Registry))
		defaultBundle.mustRegister()
	})
	return defaultBundle
}

// Metrics instruments every request on the engine. Mount after RequestID
// (the ID rides the log, not the metrics) and before Recovery.
func Metrics() gin.HandlerFunc {
	b := defaultMetrics()
	return func(c *gin.Context) {
		if c.Request.URL.Path == metricsPath {
			c.Next() // never instrument the scrape endpoint itself
			return
		}
		route := routeLabel(c)
		b.inFlight.Inc()
		start := time.Now()
		c.Next()
		b.inFlight.Dec()

		b.requests.WithLabelValues(c.Request.Method, route, statusLabel(c.Writer.Status())).Inc()
		b.duration.WithLabelValues(c.Request.Method, route).Observe(latencySeconds(time.Since(start)))
	}
}

// MetricsHandler serves the Prometheus exposition, fail-closed: empty
// token → 404 (feature off), wrong token → 401 (without echoing why).
func MetricsHandler(token string) gin.HandlerFunc {
	if token == "" {
		// Static 404 regardless of method/path shape — no config probe.
		return func(c *gin.Context) { c.Status(http.StatusNotFound) }
	}
	handler := promhttp.HandlerFor(defaultMetrics().registry, promhttp.HandlerOpts{})
	expected := "Bearer " + token
	return func(c *gin.Context) {
		if c.GetHeader("Authorization") != expected {
			c.Header("WWW-Authenticate", `Bearer realm="metrics"`)
			c.Status(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(c.Writer, c.Request)
	}
}
