package handlers

// Shared large-JSON responder — the barcode-map gzip pattern generalized.
//
// WHY: a 100-row inventory page is ~60-90KB uncompressed JSON. After the
// network RTT floor, that body is the dominant transfer cost on the
// pharmacy's link — the owner reported every 100-row batch «بيتحملوا بشكل
// بطىء». Gzipping the body at the handler (stdlib, no middleware fan-out)
// shrinks it to roughly a tenth. apiFetch uses fetch(), so browsers offer
// Accept-Encoding: gzip and decode transparently; clients that don't offer
// it get the plain c.JSON path byte-for-byte.

import (
	"compress/gzip"
	"encoding/json"
	"log"
	"strings"

	"github.com/gin-gonic/gin"
)

// respondJSON writes payload as JSON, gzipping when the client offers gzip.
// Vary is set unconditionally so caches never serve a gzip body to a
// non-gzip client. A broken encode after headers/partial body is unfixable
// mid-stream — logged for observability only, exactly like the barcode map.
func respondJSON(c *gin.Context, status int, payload gin.H) {
	c.Header("Vary", "Accept-Encoding")
	if !strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
		c.JSON(status, payload)
		return
	}
	c.Header("Content-Encoding", "gzip")
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Status(status)
	gz := gzip.NewWriter(c.Writer)
	encodeErr := json.NewEncoder(gz).Encode(payload)
	closeErr := gz.Close()
	if encodeErr != nil || closeErr != nil {
		log.Printf("[RESPOND_JSON] gzip encode error: %v / %v", encodeErr, closeErr)
	}
}
