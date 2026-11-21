package ratelimit

// Unit locks for the R-2 keyed guard (api_level 106): the key function is
// honored (identity isolation on ONE shared IP — the property that makes a
// per-principal shield the right answer for the authenticated surfaces,
// where a ClientIP key is both spoofable and shared under
// TRUSTED_PROXIES="*"), the fail-closed fallback to the caller IP when the
// identity is absent, and the 429 wire shape shared with the AUD-004 guard
// (R-14 envelope family).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func keyedTestRouter(l *IPRateLimiter, keyFn func(*gin.Context) string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/ping", l.MiddlewareForKey(keyFn), func(c *gin.Context) {
		c.String(http.StatusOK, "pong")
	})
	return r
}

// identityKey stands in for handlers.principalShieldKey: a header-carried
// identity mapped into the namespaced principal key, empty when absent.
func identityKey(c *gin.Context) string {
	if u := c.GetHeader("X-Test-User"); u != "" {
		return "p:employee:" + u
	}
	return ""
}

func TestKeyedGuardIsolatesIdentitiesOnOneIP(t *testing.T) {
	l := NewIPRateLimiter(60, 3) // 1 rps refill, burst 3 — deterministic
	defer l.Stop()
	r := keyedTestRouter(l, identityKey)

	hit := func(user string) int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/ping", nil)
		if user != "" {
			req.Header.Set("X-Test-User", user)
		}
		r.ServeHTTP(w, req)
		return w.Code
	}

	// The abuser exhausts its own principal bucket...
	for i := 0; i < 3; i++ {
		if code := hit("abuser"); code != http.StatusOK {
			t.Fatalf("abuser request %d: got %d want 200", i+1, code)
		}
	}
	if code := hit("abuser"); code != http.StatusTooManyRequests {
		t.Fatalf("abuser beyond burst: got %d want 429", code)
	}

	// ...while a colleague behind the SAME IP sails through — the property
	// a ClientIP key cannot give (audit §4.5.4): one noisy principal must
	// never starve another.
	if code := hit("colleague"); code != http.StatusOK {
		t.Fatalf("colleague sharing the IP starved: got %d want 200", code)
	}
	// And the abuser stays blocked — the wall did not reset because the
	// colleague's traffic moved a different bucket.
	if code := hit("abuser"); code != http.StatusTooManyRequests {
		t.Fatalf("abuser after colleague traffic: got %d want 429", code)
	}
}

func TestKeyedGuardFallsBackToIPWhenIdentityAbsent(t *testing.T) {
	l := NewIPRateLimiter(60, 2)
	defer l.Stop()
	r := keyedTestRouter(l, identityKey)

	// Requests with no identity at all (keyFn returns "") collapse into the
	// ClientIP bucket — fail-closed AUD-004 semantics, never a silent pass.
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/ping", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("unkeyed request %d: got %d want 200", i+1, w.Code)
		}
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/ping", nil))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("third unkeyed request beyond IP burst: got %d want 429", w.Code)
	}
}

func TestKeyedGuard429Shape(t *testing.T) {
	l := NewIPRateLimiter(60, 1)
	defer l.Stop()
	r := keyedTestRouter(l, identityKey)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ping", nil)
	req.Header.Set("X-Test-User", "u1")
	r.ServeHTTP(w, req) // consumes the single burst token
	if w.Code != http.StatusOK {
		t.Fatalf("first request: got %d want 200", w.Code)
	}

	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/ping", nil)
	req2.Header.Set("X-Test-User", "u1")
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request beyond burst: got %d want 429", w2.Code)
	}
	if got := w2.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q want 60", got)
	}
	body := w2.Body.String()
	for _, want := range []string{`"error":"rate_limited"`, `"code":"RATE_LIMITED"`, `"message":`} {
		if !strings.Contains(body, want) {
			t.Fatalf("429 body missing %s: %s", want, body)
		}
	}
}
