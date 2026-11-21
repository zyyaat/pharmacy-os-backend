package handlers

// Live router probes for the AUD-016 closure of /debug/schema. The audit's
// acceptance criterion is behavioral — "Unauthenticated debug 401/403" — so
// the gated registration helper is driven through the REAL middleware chain:
//
//   - APP_DEBUG unset (production default): the route is never mounted → 404;
//   - APP_DEBUG=true: the route mounts, but an unauthenticated caller dies at
//     the platform auth middleware with 401 authentication_required — the
//     schema payload is unreachable without a session, and (one gate deeper,
//     enforced by requirePlatformSuperAdmin) without the super-admin role.
//
// The auth service is constructed without a pool: the unauthenticated probe
// aborts at "no token" BEFORE any database access, so nil is safe here by
// construction (same pattern the auth tests rely on via &Service{}).

import (
        "net/http"
        "net/http/httptest"
        "strings"
        "testing"

        "github.com/gin-gonic/gin"
        "github.com/pharmacy-os/backend/internal/auth"
)

func debugSchemaTestRouter(t *testing.T) *gin.Engine {
        t.Helper()
        gin.SetMode(gin.TestMode)
        r := gin.New()
        v1 := r.Group("/api/v1")
        h := &Handler{auth: auth.NewHandler(nil, auth.Config{})}
        h.registerDebugSchema(v1)
        return r
}

func TestDebugSchemaHiddenWithoutAppDebug(t *testing.T) {
        t.Setenv("APP_DEBUG", "")
        r := debugSchemaTestRouter(t)

        w := httptest.NewRecorder()
        req := httptest.NewRequest(http.MethodGet, "/api/v1/debug/schema", nil)
        r.ServeHTTP(w, req)

        if w.Code != http.StatusNotFound {
                t.Fatalf("APP_DEBUG unset: expected 404 for the unmounted debug route, got %d", w.Code)
        }
}

func TestDebugSchemaUnauthenticatedRejected(t *testing.T) {
        t.Setenv("APP_DEBUG", "true")
        r := debugSchemaTestRouter(t)

        w := httptest.NewRecorder()
        req := httptest.NewRequest(http.MethodGet, "/api/v1/debug/schema", nil)
        r.ServeHTTP(w, req)

        if w.Code != http.StatusUnauthorized {
                t.Fatalf("APP_DEBUG=true without a session: expected 401 (the audit's acceptance probe), got %d", w.Code)
        }
        if !strings.Contains(w.Body.String(), "authentication_required") {
                t.Fatalf("expected the auth middleware's authentication_required error, got: %s", w.Body.String())
        }
}
