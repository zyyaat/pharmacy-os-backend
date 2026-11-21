package middleware

// Unit locks for AUD-018 (CORS hardening). The audit criterion: exact
// scheme+host matching and no wildcard-with-credentials hole. These tests
// pin the contract so a future refactor cannot quietly reintroduce
// scheme-blind or port-blind matching:
//   - the http twin of an https allow-list entry is a DIFFERENT origin;
//   - wildcard entries (*.vercel.app) are HTTPS-only and never match the
//     bare domain or a lookalike TLD;
//   - ports match only their scheme's default or an explicitly listed one;
//   - "*" keeps working for development (config.Validate refuses it in
//     production) and the middleware never mints an empty ACAO header.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestExactOriginRequiresScheme(t *testing.T) {
	list := []string{"https://pharmacy-os-app.vercel.app"}
	cases := []struct {
		origin string
		want   bool
	}{
		{"https://pharmacy-os-app.vercel.app", true},
		{"https://pharmacy-os-app.vercel.app:443", true}, // implicit default port
		{"HTTPS://PHARMACY-OS-APP.VERCEL.APP", true},     // origins are case-insensitive
		{"http://pharmacy-os-app.vercel.app", false},     // AUD-018: the http twin is a different origin
		{"https://pharmacy-os-app.vercel.app:8443", false},
		{"https://pharmacy-os-app.vercel.app.evil.com", false},
		{"https://evil.com", false},
	}
	for _, tc := range cases {
		if got := originAllowed(tc.origin, list); got != tc.want {
			t.Errorf("originAllowed(%q) = %v, want %v", tc.origin, got, tc.want)
		}
	}
}

func TestWildcardEntryIsHTTPSOnly(t *testing.T) {
	list := []string{"*.vercel.app"}
	cases := []struct {
		origin string
		want   bool
	}{
		{"https://pharmacy-os-app.vercel.app", true},
		{"https://project-git-fix.vercel.app", true},
		{"https://a.b.vercel.app", true}, // any depth
		{"https://pharmacy-os-app.vercel.app:443", true},
		{"http://pharmacy-os-app.vercel.app", false}, // AUD-018: https-only wildcard
		{"https://vercel.app", false},                // the bare domain itself never matches
		{"https://vercel.app.evil.com", false},       // suffix lookalike
		{"https://evil-vercel-app.com", false},
	}
	for _, tc := range cases {
		if got := originAllowed(tc.origin, list); got != tc.want {
			t.Errorf("originAllowed(%q) = %v, want %v", tc.origin, got, tc.want)
		}
	}
}

func TestWildcardWithExplicitPort(t *testing.T) {
	list := []string{"*.preview.example.com:8443"}
	if !originAllowed("https://team-a.preview.example.com:8443", list) {
		t.Error("wildcard entry with explicit port must match that port")
	}
	if originAllowed("https://team-a.preview.example.com", list) {
		t.Error("wildcard entry with explicit port must not match the default port")
	}
}

func TestBareHostLegacyEntryMatchesEitherScheme(t *testing.T) {
	list := []string{"localhost:3000"}
	if !originAllowed("http://localhost:3000", list) || !originAllowed("https://localhost:3000", list) {
		t.Error("legacy bare-host entry must match its host+port on either scheme")
	}
	if originAllowed("http://localhost:3001", list) || originAllowed("http://evil.com", list) {
		t.Error("bare-host entry must not broaden to other hosts or ports")
	}
}

func TestStarEntryStillAllowsDevelopment(t *testing.T) {
	if !originAllowed("https://anything.example", []string{"*"}) {
		t.Error("* must keep working in development (config.Validate refuses it in production)")
	}
}

func TestMiddlewareMintsNoACAOForUnallowedOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CORS("https://pharmacy-os-app.vercel.app"))
	r.GET("/ping", func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set("Origin", "https://evil.com")
	r.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("disallowed origin must get no Access-Control-Allow-Origin, got %q", got)
	}
}

func TestMiddlewareSameOriginRequestGetsNoACAO(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CORS("https://pharmacy-os-app.vercel.app"))
	r.GET("/ping", func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil) // no Origin header
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("same-origin (no Origin header) must pass through, got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("no Origin header must mint no empty ACAO header, got %q", got)
	}
}
