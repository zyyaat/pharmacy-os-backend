// Package middleware — panic containment with structured reporting.
//
// R-9 (api_level 103): the previous Recovery swallowed panics into a
// silent 500 ("TODO: Implement proper error reporting") — the operator
// learned about panics only from client complaints. Same contract as
// gin.Recovery (500 + abort, no panic leaks past the edge, generic body),
// but the panic now lands in the structured log with the correlation ID
// and the stack: the one line an on-call greps first.
package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"
)

// Recovery returns a middleware that recovers from panics, logs the panic
// (value + stack + request identity) through the structured logger, and
// answers 500 with the generic internal-error body. Mounted by NewServer
// in place of gin.Recovery.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				requestLogger.LogAttrs(c.Request.Context(), slog.LevelError,
					"http_panic",
					slog.String("panic", fmt.Sprint(rec)),
					slog.String("stack", string(debug.Stack())),
					slog.String("method", c.Request.Method),
					slog.String("route", routeLabel(c)),
					slog.String("ip", c.ClientIP()),
					slog.String("request_id", RequestIDFrom(c)),
				)
				c.AbortWithStatusJSON(http.StatusInternalServerError,
					gin.H{"error": "internal_error", "message": "حدث خطأ داخلي"})
			}
		}()
		c.Next()
	}
}
