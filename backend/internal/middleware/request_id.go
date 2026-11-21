// Package middleware — request identification (R-9, api_level 103).
//
// Every HTTP request gets a correlation ID that travels through the whole
// observability surface: the structured request log line, the response
// header (so a client bug report carries the exact ID to grep for), and
// gin's context (so any handler or downstream middleware can stamp it onto
// its own errors). Before this, correlating a support report with the
// container's stdout meant timestamp archaeology.
package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

// contextKey is how handlers retrieve the ID (RequestIDFrom).
const requestIDKey = "request_id"

// fallbackSeq makes IDs unique even if crypto/rand ever fails — a correct
// ID that the operator can still correlate beats a panic at the gate.
var fallbackSeq atomic.Uint64

// requestIDMaxInbound bounds an inbound X-Request-ID: honored when sane
// (so a caller's own correlation chain survives the hop), replaced when
// absent, oversized, or carrying control characters (header-injection
// surface). 128 chars covers UUIDs, ULIDs, and w3c traceparent suffixes.
const requestIDMaxInbound = 128

func newRequestID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return hex.EncodeToString(buf[:])
	}
	return hex.EncodeToString([]byte("fb")) + // marker: fallback ID
		formatUint(fallbackSeq.Add(1))
}

func formatUint(v uint64) string {
	if v == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for v > 0 {
		i--
		digits[i] = byte('0' + v%10)
		v /= 10
	}
	return string(digits[i:])
}

func saneInboundID(v string) bool {
	if v == "" || len(v) > requestIDMaxInbound {
		return false
	}
	for _, r := range v {
		if r < 0x21 || r > 0x7e { // printable ASCII minus spaces/controls
			return false
		}
	}
	return true
}

// RequestID assigns (or honors) the correlation ID and mirrors it into the
// response. Runs OUTERMOST so even recovery-driven 500s carry the ID.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if !saneInboundID(id) {
			id = newRequestID()
		}
		id = strings.TrimSpace(id)
		c.Set(requestIDKey, id)
		c.Writer.Header().Set("X-Request-ID", id)
		c.Next()
	}
}

// RequestIDFrom returns the correlation ID of the current request (empty
// if the middleware is not mounted — callers must treat it as optional).
func RequestIDFrom(c *gin.Context) string {
	if c == nil {
		return ""
	}
	id, _ := c.Value(requestIDKey).(string)
	return id
}
