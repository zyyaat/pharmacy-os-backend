package middleware

// Request body cap (AUD-002, HIGH): config.MaxRequestSize existed since
// config.go's early days but had ZERO references — the setting was dead and
// any handler could buffer an arbitrarily large body on a 1 vCPU / 1 GB box.
//
// Two layers:
//   - An upfront ContentLength rejection gives clean 413s for the common
//     (non-chunked) case without the handler ever reading the body.
//   - The http.MaxBytesReader wrap catches chunked bodies that hide their
//     size; the bound is enforced while reading.

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// MaxRequestBody caps every request body at maxBytes. maxBytes <= 0
// disables the cap (test convenience; production always sets it).
func MaxRequestBody(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if maxBytes > 0 && c.Request.Body != nil {
			if c.Request.ContentLength > maxBytes {
				c.Header("Retry-After", "0")
				c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
					"error":   "request_too_large",
					"message": "Request body exceeds the allowed size",
					"code":    "REQUEST_TOO_LARGE",
				})
				return
			}
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		}
		c.Next()
	}
}
