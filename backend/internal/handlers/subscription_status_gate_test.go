package handlers

// Regression lock for AUD T-B6 (MEDIUM, subscription-status gate) — the
// three pharmacy-realm reads that carry no single permission key used to
// bypass the subscription lockout entirely:
//
//	GET /pharmacy/sync     — sections self-filter by employee RBAC keys,
//	                         but nothing asked the company subscription, so
//	                         an expired tenant kept pulling full deltas.
//	GET /pharmacy/context  — the bootstrap read.
//	GET /pharmacy/settings — the receipt-settings read.
//
// Every perm()-guarded route already checks the status through the plan
// gate (requirePharmacyPermission → enforcePlanGate → statusActive); this
// test drives the REAL handlers through three probes:
//
//  1. An expired company gets 403 subscription_expired on all three routes,
//     and the gate was actually consulted (the fake counts EnforceStatus
//     calls) — before the fix the handlers never asked.
//  2. An active company passes the gate: /sync answers its cursor-bootstrap
//     200 (server_time) with zero database access; for /context and
//     /settings (which need a real DB past the gate) the helper itself is
//     probed and must pass without aborting.
//  3. A platform super admin bypasses the lockout even when the company is
//     expired (enforcePlanStatus policy) — the probe asserts the helper
//     returns true without ever consulting the gate.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pharmacy-os/backend/internal/auth"
	"github.com/pharmacy-os/backend/internal/models"
)

// principalContextKeyValue pins the string value of the unexported
// auth principal key (contextKey("auth_principal") in
// internal/auth/middleware.go). If the middleware ever renames it, every
// probe here fails with 403 pharmacy_account_required instead of silently
// passing — the failure mode is loud by design.
const principalContextKeyValue = "auth_principal"

// fakeSubs implements SubscriptionGate. Only the status gate is real;
// CompanyIDForPharmacy resolves the stable pharmacy→company mapping the
// real service caches. Every other method is a loud failure so an
// unexpected call fails the test instead of passing silently.
type fakeSubs struct {
	t            *testing.T
	status       string // active | expired | suspended | pending
	enforceCalls int
}

func (f *fakeSubs) EnforceStatus(c *gin.Context, companyID string) bool {
	f.enforceCalls++
	if f.status == "active" {
		return true
	}
	code := map[string]string{
		"expired":   "subscription_expired",
		"suspended": "subscription_suspended",
		"pending":   "subscription_pending",
	}[f.status]
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": code})
	return false
}

func (f *fakeSubs) EnforcePermission(c *gin.Context, companyID, permissionKey string) bool {
	f.t.Fatalf("unexpected EnforcePermission call")
	return false
}

func (f *fakeSubs) EnforceLimit(c *gin.Context, companyID, limitKey string) bool {
	f.t.Fatalf("unexpected EnforceLimit call")
	return false
}

func (f *fakeSubs) GetEffective(ctx context.Context, companyID string) (*models.EffectivePlan, error) {
	f.t.Fatalf("unexpected GetEffective call")
	return nil, nil
}

func (f *fakeSubs) Invalidate(companyID string) { f.t.Fatalf("unexpected Invalidate call") }

func (f *fakeSubs) InvalidateAll() { f.t.Fatalf("unexpected InvalidateAll call") }

func (f *fakeSubs) CompanyIDForPharmacy(ctx context.Context, pharmacyID string) (string, error) {
	return "22222222-2222-2222-2222-222222222222", nil
}

func (f *fakeSubs) CheckLimit(ctx context.Context, companyID, limitKey string) (int, int, bool, error) {
	f.t.Fatalf("unexpected CheckLimit call")
	return 0, 0, false, nil
}

func (f *fakeSubs) UsageAll(ctx context.Context, companyID string) (map[string]int, error) {
	f.t.Fatalf("unexpected UsageAll call")
	return nil, nil
}

func principalStub(p *auth.Principal) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(principalContextKeyValue, p)
		c.Next()
	}
}

func employeePrincipal() *auth.Principal {
	return &auth.Principal{
		ID:         "emp-1",
		Type:       auth.EmployeePrincipal,
		PharmacyID: "11111111-1111-1111-1111-111111111111",
	}
}

func TestSubscriptionStatusGateBlocksExpiredTenantsOnKeylessReads(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fake := &fakeSubs{t: t, status: "expired"}
	h := &Handler{subs: fake}

	r := gin.New()
	r.GET("/api/v1/pharmacy/sync", principalStub(employeePrincipal()), h.GetPharmacySync)

	// Probe 1 — the audit's acceptance route: expired tenants stop pulling deltas.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pharmacy/sync?since=2026-01-01T00:00:00Z", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "subscription_expired") {
		t.Fatalf("GET /sync with expired company: got %d %q want 403 subscription_expired (AUD T-B6 regression: keyless read bypassed the lockout)", w.Code, w.Body.String())
	}
	if fake.enforceCalls != 1 {
		t.Fatalf("EnforceStatus consulted %d times on /sync, want exactly 1", fake.enforceCalls)
	}

	// Probe 2 — /context and /settings sit behind the same lockout.
	r2 := gin.New()
	r2.GET("/api/v1/pharmacy/context", principalStub(employeePrincipal()), h.GetPharmacyContext)
	r2.GET("/api/v1/pharmacy/settings", principalStub(employeePrincipal()), h.GetPharmacySettings)
	for _, tc := range []struct{ name, path string }{
		{"context", "/api/v1/pharmacy/context"},
		{"settings", "/api/v1/pharmacy/settings"},
	} {
		w := httptest.NewRecorder()
		r2.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "subscription_expired") {
			t.Fatalf("GET %s with expired company: got %d %q want 403 subscription_expired", tc.name, w.Code, w.Body.String())
		}
	}
	if fake.enforceCalls != 3 {
		t.Fatalf("EnforceStatus consulted %d times across the three routes, want exactly 3", fake.enforceCalls)
	}
}

func TestSubscriptionStatusGatePassesActiveCompanies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fake := &fakeSubs{t: t, status: "active"}
	h := &Handler{subs: fake}

	// Route-level: /sync cursor bootstrap (no `since`) returns 200 with zero
	// DB access, so the full real handler chain can run here.
	r := gin.New()
	r.GET("/api/v1/pharmacy/sync", principalStub(employeePrincipal()), h.GetPharmacySync)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/pharmacy/sync", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "server_time") {
		t.Fatalf("GET /sync with active company: got %d %q want 200 server_time (gate falsely locked out a paying tenant)", w.Code, w.Body.String())
	}
	if fake.enforceCalls != 1 {
		t.Fatalf("EnforceStatus consulted %d times, want exactly 1", fake.enforceCalls)
	}

	// Helper-level for /context and /settings (they need a real DB past the
	// gate): the helper must pass without writing anything.
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pharmacy/context", nil)
	c.Set(principalContextKeyValue, employeePrincipal())
	if !h.enforceSubscriptionStatusForPharmacy(c) {
		t.Fatalf("helper locked out an active company: %d %q", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("helper wrote a response for an active company: %d %q", rec.Code, rec.Body.String())
	}
}

func TestSubscriptionStatusGateSuperAdminBypass(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fake := &fakeSubs{t: t, status: "expired"}
	h := &Handler{subs: fake}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pharmacy/context", nil)
	c.Set(principalContextKeyValue, &auth.Principal{
		ID:         "sa-1",
		Type:       auth.CompanyUserPrincipal,
		Role:       "super_admin",
		PharmacyID: "11111111-1111-1111-1111-111111111111",
	})
	if !h.enforceSubscriptionStatusForPharmacy(c) {
		t.Fatalf("super admin was locked out by the subscription gate (bypass policy broken): %d %q", rec.Code, rec.Body.String())
	}
	if fake.enforceCalls != 0 {
		t.Fatalf("super admin path consulted EnforceStatus %d times, want 0 (bypass happens before the gate)", fake.enforceCalls)
	}
}
