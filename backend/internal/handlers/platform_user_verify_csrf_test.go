package handlers

// Regression lock for Task 132 — POST /platform-admin/users/verify-email sits
// behind auth.CSRF(PlatformRealm) like every other platform mutation. The
// endpoint flips email_verified_at, i.e. it opens the login door: a missing
// CSRF guard would let a cross-site page ride the operator's SameSite=None
// session and silently verify attacker-controlled accounts. This test drives
// the REAL middleware pair used in production registration (CSRF first, then
// a stub that proves the chain continues) through two probes:
//
//  1. No X-CSRF-Token header → 403 csrf_failed (the request never reaches the
//     handler, so no DB is needed in this bare router).
//  2. A matching platform_csrf cookie + X-CSRF-Token header pair passes the
//     CSRF layer and reaches the stub (anything but csrf_failed proves the
//     guard no longer blocks legitimate callers).
//
// The cookie name mirrors csrfCookieName(PlatformRealm) in
// internal/auth/middleware.go; if it ever changes, probe 2 fails loudly.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pharmacy-os/backend/internal/auth"
)

func platformVerifyEmailTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	v1 := r.Group("/api/v1")
	platformAdmin := v1.Group("/platform-admin")
	platformAdmin.POST("/users/verify-email",
		auth.CSRF(auth.PlatformRealm),
		func(c *gin.Context) { c.String(http.StatusOK, "REACHED_VERIFY") },
	)
	return r
}

func TestPlatformVerifyEmailRequiresCSRF(t *testing.T) {
	r := platformVerifyEmailTestRouter()

	// Probe 1 — the cross-site-style mutation with no CSRF header must die.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/platform-admin/users/verify-email",
		strings.NewReader(`{"email":"owner@pharmacy.com"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("POST /platform-admin/users/verify-email without CSRF header: got %d want 403 (unguarded verify-email is an account-opening CSRF)", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "csrf_failed") {
		t.Fatalf("POST without CSRF header: body %q want csrf_failed", body)
	}

	// Probe 2 — the legitimate pair passes CSRF and reaches the handler.
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/platform-admin/users/verify-email",
		strings.NewReader(`{"email":"owner@pharmacy.com"}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-CSRF-Token", "test-token")
	req2.AddCookie(&http.Cookie{Name: "platform_csrf", Value: "test-token"})
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), "REACHED_VERIFY") {
		t.Fatalf("POST with valid CSRF pair: got %d body %q want 200 REACHED_VERIFY (the guard must not block legitimate callers)", w2.Code, w2.Body.String())
	}
}
