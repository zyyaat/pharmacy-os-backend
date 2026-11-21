package handlers

// Regression lock for AUD-003 (HIGH, launch blocker) — the three company
// mutation routes (PUT /companies/:id, PATCH /companies/:id/status,
// DELETE /companies/:id) must sit behind auth.CSRF like every other
// authenticated mutation in the router. They were the single exception while
// production cookies are SameSite=None + Secure, so a victim visiting an
// attacker page could ride the attached session cookie into silent admin
// mutations. This test drives the REAL production registration
// (registerCompanyMutationRoutes) — not a copy — through three probes:
//
//  1. The audit's acceptance probe: a cross-site-style mutation with NO
//     X-CSRF-Token header must die with 403 csrf_failed. Before the fix this
//     request sailed straight to the handler.
//  2. All three verbs behave identically (PUT / PATCH-status / DELETE).
//  3. A matching platform_csrf cookie + X-CSRF-Token header pair passes the
//     CSRF layer: the request proceeds downstream and the NEXT gate answers
//     (401 company_auth_required from the permission middleware, which sees
//     no company session in this bare test router) — anything but
//     csrf_failed proves the guard no longer blocks legitimate callers.
//
// The cookie name mirrors csrfCookieName(PlatformRealm) in
// internal/auth/middleware.go; if it ever changes, probe 3 fails loudly.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pharmacy-os/backend/internal/auth"
)

func companyMutationTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	v1 := r.Group("/api/v1")
	company := v1.Group("/companies")
	registerCompanyMutationRoutes(company,
		func(c *gin.Context) { c.String(http.StatusOK, "REACHED_UPDATE") },
		func(c *gin.Context) { c.String(http.StatusOK, "REACHED_UPDATE_STATUS") },
		func(c *gin.Context) { c.String(http.StatusOK, "REACHED_DELETE") },
	)
	return r
}

func TestCompanyMutationsRequireCSRF(t *testing.T) {
	r := companyMutationTestRouter()

	// Probe 1 — the audit's acceptance criterion on the exact route it named.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/companies/abc/status", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("PATCH /companies/:id/status without CSRF header: got %d want 403 (the AUD-003 regression: unguarded chain lets mutations through)", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "csrf_failed") {
		t.Fatalf("PATCH without CSRF header: body %q want csrf_failed", body)
	}

	// Probe 2 — all three mutations sit behind the same guard.
	for _, tc := range []struct{ name, method, path string }{
		{"put_company", http.MethodPut, "/api/v1/companies/abc"},
		{"patch_status", http.MethodPatch, "/api/v1/companies/abc/status"},
		{"delete_company", http.MethodDelete, "/api/v1/companies/abc"},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "csrf_failed") {
			t.Fatalf("%s without CSRF header: got %d %q want 403 csrf_failed", tc.name, w.Code, w.Body.String())
		}
	}

	// Probe 3 — a valid cookie+header pair leaves the CSRF layer: the
	// company permission gate answers instead (no company session in this
	// bare router), proving legitimate callers are not blocked by CSRF.
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPatch, "/api/v1/companies/abc/status", nil)
	req2.AddCookie(&http.Cookie{Name: "platform_csrf", Value: "test-token"})
	req2.Header.Set(auth.CSRFHeaderName, "test-token")
	r.ServeHTTP(w2, req2)
	if body := w2.Body.String(); strings.Contains(body, "csrf_failed") {
		t.Fatalf("PATCH with matching cookie+header pair was still blocked by CSRF: %d %q", w2.Code, body)
	}
	if w2.Code == http.StatusOK {
		t.Fatalf("PATCH with CSRF pair reached the handler in a bare router — the permission gate vanished from the chain: %q", w2.Body.String())
	}
}
