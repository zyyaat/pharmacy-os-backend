package handlers

// Behavioral locks for TRUSTED_PROXIES parsing (Task 126): the wildcard
// escape hatch must restore gin's trust-all vocabulary, the default private
// CIDR list must keep ignoring X-Forwarded-For from untrusted (public)
// proxies, and a garbage value must still fail the boot loudly — the exact
// trio that the DockHosting incident of 2026-10-30 exposed (TRUSTED_PROXIES=*
// panic killed the API after a green build).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pharmacy-os/backend/internal/config"
)

// probeClientIP builds a server from the given TRUSTED_PROXIES value, adds a
// route reporting gin's ClientIP, and answers what a proxied request from
// remoteAddr (a PUBLIC edge IP — never in the default private list) carrying
// the given X-Forwarded-For resolves to.
func probeClientIP(t *testing.T, trustedProxies, remoteAddr, xff string) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		MaxRequestSize: 1024,
		TrustedProxies: trustedProxies,
	}
	srv := New(cfg).NewServer("127.0.0.1:0")
	engine, ok := srv.Handler.(*gin.Engine)
	if !ok {
		t.Fatalf("server handler is %T, want *gin.Engine", srv.Handler)
	}
	engine.GET("/probe", func(c *gin.Context) {
		c.String(http.StatusOK, c.ClientIP())
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.RemoteAddr = remoteAddr
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("probe status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func TestTrustedProxiesWildcardTrustsAllProxies(t *testing.T) {
	// The PaaS contract: TRUSTED_PROXIES="*" means every edge proxy is
	// trusted, so the forwarded client IP wins — this is what unblocks the
	// DockHosting deployment without touching the server env.
	got := probeClientIP(t, "*", "203.0.113.9:443", "198.51.100.7")
	if got != "198.51.100.7" {
		t.Fatalf("wildcard TRUSTED_PROXIES ClientIP = %q, want the forwarded 198.51.100.7", got)
	}
}

func TestTrustedProxiesDefaultIgnoresXFFFromPublicProxy(t *testing.T) {
	// The AUD-002 default: a public-addressed proxy is NOT in the private
	// trusted list, so its forwarded header must be ignored — the immediate
	// peer is the answer, never the spoofable header.
	got := probeClientIP(t, "127.0.0.1/8,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16", "203.0.113.9:443", "198.51.100.7")
	if got != "203.0.113.9" {
		t.Fatalf("default TRUSTED_PROXIES ClientIP = %q, want the peer 203.0.113.9 (XFF ignored)", got)
	}
}

func TestTrustedProxiesEmptyDisablesHeaderTrust(t *testing.T) {
	// An explicitly empty value = trust no proxy: fail-closed, ClientIP is
	// the direct peer even if the peer sits in a trusted-by-default range.
	got := probeClientIP(t, "", "10.1.2.3:443", "198.51.100.7")
	if got != "10.1.2.3" {
		t.Fatalf("empty TRUSTED_PROXIES ClientIP = %q, want the peer 10.1.2.3", got)
	}
}

func TestTrustedProxiesGarbageStillPanicsAtBoot(t *testing.T) {
	// The fail-loud contract survives: a value that is neither the wildcard
	// nor parseable IP/CIDR kills the boot (the panic message must name the
	// offending config so the operator sees what to fix).
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a boot panic for a garbage TRUSTED_PROXIES value")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "TRUSTED_PROXIES") {
			t.Fatalf("panic = %v, want a TRUSTED_PROXIES config error", r)
		}
	}()
	cfg := &config.Config{
		MaxRequestSize: 1024,
		TrustedProxies: "definitely not an ip",
	}
	New(cfg).NewServer("127.0.0.1:0")
}
