package handlers

// Behavior gate for the R-2 per-principal shield wiring (api_level 106):
// pins principalShieldKey against exactly what auth's setPrincipal plants
// (the string context keys "user_id"/"principal_type"), and pins the shield
// semantics end to end over a router: an abuser's own bucket blocks while a
// colleague sharing the same IP sails through (the property an IP key
// cannot give), and the 429 carries the shared RATE_LIMITED envelope.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pharmacy-os/backend/internal/ratelimit"
)

// shieldTestRouter stubs auth.Middleware's contract: it plants exactly the
// context keys setPrincipal plants, aborting unauthenticated requests — so
// the shield under test sees the same context shape the real chain gives it.
func shieldTestRouter(l *ratelimit.IPRateLimiter) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api/v1/pharmacy")
	g.Use(func(c *gin.Context) {
		id := c.GetHeader("X-Test-User")
		if id == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication_required"})
			return
		}
		c.Set("user_id", id)
		c.Set("principal_type", "employee")
		c.Next()
	})
	g.Use(l.MiddlewareForKey(principalShieldKey))
	g.GET("/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })
	return r
}

func TestPrincipalShieldKeyFormat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if principalShieldKey(c) != "" {
		t.Fatal("identity-less context must yield an empty key (IP fallback)")
	}
	c.Set("principal_type", "employee")
	c.Set("user_id", "abc")
	if got := principalShieldKey(c); got != "p:employee:abc" {
		t.Fatalf("key = %q want %q", got, "p:employee:abc")
	}
	// A principal_type-less identity still keys (type is decoration, not a
	// gate) — only a missing user_id falls back.
	c2, _ := gin.CreateTestContext(httptest.NewRecorder())
	c2.Set("user_id", "abc")
	if got := principalShieldKey(c2); got != "p::abc" {
		t.Fatalf("key without type = %q want %q", got, "p::abc")
	}
}

func TestPrincipalShieldBlocksAbuserNotColleague(t *testing.T) {
	l := ratelimit.NewIPRateLimiter(60, 3)
	defer l.Stop()
	r := shieldTestRouter(l)

	hit := func(user string) int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/v1/pharmacy/ping", nil)
		if user != "" {
			req.Header.Set("X-Test-User", user)
		}
		r.ServeHTTP(w, req)
		return w.Code
	}

	// Unauthenticated requests are rejected by the stub auth BEFORE the
	// shield (same order as production: shield mounts after the auth
	// middleware), so the 401 family is untouched by this wave.
	if code := hit(""); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: got %d want 401", code)
	}

	for i := 0; i < 3; i++ {
		if code := hit("abuser"); code != http.StatusOK {
			t.Fatalf("abuser request %d: got %d want 200", i+1, code)
		}
	}
	if code := hit("abuser"); code != http.StatusTooManyRequests {
		t.Fatalf("abuser beyond burst: got %d want 429", code)
	}
	if code := hit("colleague"); code != http.StatusOK {
		t.Fatalf("colleague sharing the IP starved: got %d want 200", code)
	}

	// The 429 must carry the shared wire shape (R-14 envelope family).
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/pharmacy/ping", nil)
	req.Header.Set("X-Test-User", "abuser")
	r.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `"code":"RATE_LIMITED"`) {
		t.Fatalf("429 body missing RATE_LIMITED code: %s", w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q want 60", got)
	}
}
