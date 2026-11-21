package handlers

import (
        "strings"

        "github.com/gin-gonic/gin"
)

// parseListPagingRaw parses limit/offset query values with hard clamps.
// Invalid or out-of-range input falls back to the default instead of failing
// the request — list endpoints never 400 because of paging noise (the same
// contract parseLimitOffset established for the sales history endpoint).
// The difference vs parseLimitOffset: a limit above max clamps to max instead
// of leaking max+1 (parseIntInRange's overflow sentinel is max+1, and the old
// callers passed it straight through).
func parseListPagingRaw(limitRaw, offsetRaw string, def, max int) (int, int) {
        limit, offset := def, 0
        if raw := strings.TrimSpace(limitRaw); raw != "" {
                if v := parseIntInRange(raw, 1, max); v > 0 {
                        if v > max {
                                v = max
                        }
                        limit = v
                }
        }
        if raw := strings.TrimSpace(offsetRaw); raw != "" {
                if v := parseIntInRange(raw, 0, 1_000_000_000); v > 0 {
                        offset = v
                }
        }
        return limit, offset
}

// parseListPaging is the gin-bound form used by list endpoints.
func parseListPaging(c *gin.Context, def, max int) (int, int) {
        return parseListPagingRaw(c.Query("limit"), c.Query("offset"), def, max)
}
