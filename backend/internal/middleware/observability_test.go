// R-9 (api_level 103) — gates for the observability chain: request-ID
// minting/honoring/sanitizing, the structured request log line (fields a
// log driver can index), the slog Recovery contract, and the metrics
// middleware's bounded-label behavior + the fail-closed /metrics gate.
package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testEngine(t *testing.T, routes func(r *gin.Engine)) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestID(), Logger(), Metrics(), Recovery())
	routes(r)
	return r
}

func swapLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := requestLogger
	requestLogger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	t.Cleanup(func() { requestLogger = old })
	return &buf
}

func swapErrorOnlyLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := requestLogger
	requestLogger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))
	t.Cleanup(func() { requestLogger = old })
	return &buf
}

func TestRequestIDMintedAndEchoed(t *testing.T) {
	r := testEngine(t, func(r *gin.Engine) {
		r.GET("/ping", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"id": RequestIDFrom(c)})
		})
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/ping", nil))
	require.Equal(t, http.StatusOK, w.Code)

	id := w.Header().Get("X-Request-ID")
	require.Len(t, id, 32, "minted ID is 16 random bytes hex")
	assert.NotEqual(t, strings.Repeat("0", 32), id)
	assert.JSONEq(t, `{"id":"`+id+`"}`, w.Body.String(), "handler saw the same ID")
}

func TestRequestIDHonorsSaneInbound(t *testing.T) {
	r := testEngine(t, func(r *gin.Engine) {
		r.GET("/ping", func(c *gin.Context) { c.Status(http.StatusOK) })
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ping", nil)
	req.Header.Set("X-Request-ID", "caller-trace-abc123")
	r.ServeHTTP(w, req)
	assert.Equal(t, "caller-trace-abc123", w.Header().Get("X-Request-ID"),
		"a sane caller ID survives the hop (correlation across services)")
}

func TestRequestIDRejectsUnsafeInbound(t *testing.T) {
	r := testEngine(t, func(r *gin.Engine) {
		r.GET("/ping", func(c *gin.Context) { c.Status(http.StatusOK) })
	})
	for name, v := range map[string]string{
		"oversized":  strings.Repeat("x", 129),
		"crlf":       "abc\r\nX-Evil: 1",
		"nonprint":   "abc\u00e9def",
		"whitespace": "   ",
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/ping", nil)
		req.Header.Set("X-Request-ID", v)
		r.ServeHTTP(w, req)
		got := w.Header().Get("X-Request-ID")
		assert.Len(t, got, 32, "%s: unsafe inbound must be replaced by a minted ID", name)
		assert.NotEqual(t, v, got, name)
	}
}

func TestLoggerEmitsStructuredLine(t *testing.T) {
	buf := swapLogger(t)

	r := testEngine(t, func(r *gin.Engine) {
		r.GET("/api/v1/things/:id", func(c *gin.Context) { c.String(http.StatusTeapot, "x") })
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/things/9", nil)
	req.Header.Set("X-Request-ID", "log-line-1")
	r.ServeHTTP(w, req)

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line), "log line must be valid JSON: %q", buf.String())
	assert.Equal(t, "http_request", line["msg"])
	assert.Equal(t, "GET", line["method"])
	assert.Equal(t, "/api/v1/things/:id", line["route"], "ROUTE TEMPLATE, not the raw path")
	assert.EqualValues(t, 418, line["status"])
	assert.Equal(t, "log-line-1", line["request_id"])
	assert.NotZero(t, line["latency_ms"])
}

func TestLogger5xxLogsAtError(t *testing.T) {
	buf := swapErrorOnlyLogger(t) // INFO dropped: proves 5xx is ERROR-level

	r := testEngine(t, func(r *gin.Engine) {
		r.GET("/boom", func(c *gin.Context) { c.Status(http.StatusBadGateway) })
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/boom", nil))
	require.Contains(t, buf.String(), `"http_request"`, "5xx must survive an ERROR-only filter")
}

func TestRecoveryLogsPanicAndReturns500(t *testing.T) {
	buf := swapLogger(t)

	r := testEngine(t, func(r *gin.Engine) {
		r.GET("/panic", func(c *gin.Context) { panic("boom-1234") })
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/panic", nil))
	assert.Equal(t, http.StatusInternalServerError, w.Code, "gin.Recovery's 500 contract kept")
	assert.NotContains(t, w.Body.String(), "boom-1234", "no panic detail leaks to the client")

	var line map[string]any
	// Two structured lines land in the buffer: the panic line AND the
	// request line (Recovery aborts with 500, then Logger logs it — both
	// carrying the SAME request_id, which is the correlation win).
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var obj map[string]any
		require.NoError(t, json.Unmarshal([]byte(l), &obj), "every log line must be valid JSON: %q", l)
		if obj["msg"] == "http_panic" {
			line = obj
		}
	}
	require.NotNil(t, line, "a http_panic line must be present: %q", buf.String())
	assert.Contains(t, line["panic"], "boom-1234")
	assert.NotEmpty(t, line["stack"], "stack lands in the log, not the response")
	assert.Len(t, line["request_id"], 32, "even panic 500s carry the correlation ID")
}

func TestMetricsBoundedLabelsAndSeries(t *testing.T) {
	r := testEngine(t, func(r *gin.Engine) {
		r.GET("/api/v1/thing/:id", func(c *gin.Context) { c.Status(http.StatusOK) })
	})
	for _, p := range []string{"/api/v1/thing/1", "/api/v1/thing/2", "/api/v1/thing/999"} {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", p, nil))
	}
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/no-such-route", nil))

	scrape := gin.New()
	scrape.GET("/metrics", MetricsHandler("testtok"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer testtok")
	scrape.ServeHTTP(w, req)
	body := w.Body.String()
	require.Equal(t, http.StatusOK, w.Code)

	assert.Contains(t, body, `pharmacyos_http_requests_total{method="GET",route="/api/v1/thing/:id",status="200"} 3`,
		"three distinct raw paths collapse into ONE bounded route-template series")
	assert.Contains(t, body, `route="UNMATCHED"`, "404 scanners land under the bounded series")
	assert.NotContains(t, body, `route="/api/v1/thing/1"`, "no per-path series may exist")
	assert.Contains(t, body, "pharmacyos_http_request_duration_seconds")
	assert.Contains(t, body, "pharmacyos_http_requests_in_flight")
}

func TestMetricsGate(t *testing.T) {
	r := gin.New()
	r.GET("/metrics", MetricsHandler(""))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	assert.Equal(t, http.StatusNotFound, w.Code, "no token: endpoint is OFF (404), not 403")

	r2 := gin.New()
	r2.GET("/metrics", MetricsHandler("sekrit"))
	w2 := httptest.NewRecorder()
	r2.ServeHTTP(w2, httptest.NewRequest("GET", "/metrics", nil))
	assert.Equal(t, http.StatusUnauthorized, w2.Code, "wrong token: 401")

	w3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("GET", "/metrics", nil)
	req3.Header.Set("Authorization", "Bearer sekrit")
	r2.ServeHTTP(w3, req3)
	assert.Equal(t, http.StatusOK, w3.Code, "correct bearer: exposition served")
}
