// Package middleware provides HTTP middleware for the API
package middleware

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// requestLogger is the structured request log (R-9, api_level 103). JSON
// on stdout, one line per request, container-agnostic: Docker/cloud log
// drivers index the fields (method, route, status, latency_ms, request_id)
// without regex. Swap point for boot (SetRequestLogger) and tests.
var requestLogger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
	Level: slog.LevelInfo,
}))

// SetRequestLogger redirects the request/panic logs to a custom slog
// handler (boot may bind LOG_LEVEL; tests bind buffers). Nil is ignored.
func SetRequestLogger(l *slog.Logger) {
	if l != nil {
		requestLogger = l
	}
}

// routeLabel returns the matched ROUTE TEMPLATE ("/api/v1/pos/sales/:id"),
// never the raw path: raw paths carry UUIDs and mint unbounded label
// cardinality — the classic Prometheus meltdown. Unmatched requests are
// bounded under "UNMATCHED" (scanners hitting the edge show up as one
// series, not one series per probed path).
func routeLabel(c *gin.Context) string {
	if p := c.FullPath(); p != "" {
		return p
	}
	return "UNMATCHED"
}

// Logger emits the structured request line. Replaces gin.Logger(): the
// same information (plus request_id, bytes, errors) in machine-readable
// form — one line per request, no doubled logging.
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		latency := time.Since(start)
		status := c.Writer.Status()

		attrs := []any{
			"method", c.Request.Method,
			"route", routeLabel(c),
			"status", status,
			"latency_ms", float64(latency.Nanoseconds()) / 1e6,
			"ip", c.ClientIP(),
			"request_id", RequestIDFrom(c),
			"bytes", c.Writer.Size(),
		}
		if len(c.Errors) > 0 {
			msgs := make([]string, 0, len(c.Errors))
			for _, e := range c.Errors {
				msgs = append(msgs, e.Error())
			}
			attrs = append(attrs, "errors", strings.Join(msgs, "; "))
		}
		// 5xx logs at ERROR (pager-adjacent), everything else at INFO.
		if status >= 500 {
			requestLogger.LogAttrs(c.Request.Context(), slog.LevelError,
				"http_request", attrsToGroup(attrs)...)
		} else {
			requestLogger.LogAttrs(c.Request.Context(), slog.LevelInfo,
				"http_request", attrsToGroup(attrs)...)
		}
	}
}

// attrsToGroup converts the variadic kv list into slog.Attr values.
func attrsToGroup(kv []any) []slog.Attr {
	out := make([]slog.Attr, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		if key, ok := kv[i].(string); ok {
			out = append(out, slog.Any(key, kv[i+1]))
		}
	}
	return out
}

// Recovery lives in recovery.go (it was a silent-500 stub there before
// R-9 — now it reports through the same structured logger).

// latencySeconds converts a duration for the histogram observe call.
func latencySeconds(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / 1e9
}

// statusLabel bounds the status label to the exact code (codes are a
// closed set; strconv keeps the label type string for Prometheus).
func statusLabel(code int) string {
	return strconv.Itoa(code)
}
