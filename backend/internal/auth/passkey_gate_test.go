package auth

// M1/G1 — gate tests for the registration step-up requirement.
//
// The gate is the SAME RequireStepUp middleware that has guarded passkey
// deletion since Task 151 — what is new is WHERE it stands (in front of
// begin registration, both realms). These tests pin the contract the M2 UI
// will code against:
//
//   - no grant header at all → 403 stepup_required (DB-free path — the
//     consume short-circuits on an empty token, so this test needs no
//     database);
//   - a fresh manage_passkeys grant opens the gate EXACTLY once (single
//     use, owner rule #3);
//   - a manage_passkeys grant never opens a change_password gate (purpose
//     binding — a grant minted for one operation can never unlock another).

import (
        "context"
        "net/http"
        "net/http/httptest"
        "testing"
        "time"

        "github.com/gin-gonic/gin"
        "github.com/stretchr/testify/require"
)

func newStepUpGateRouter(principal *Principal, purpose string, service *Service, handler func(c *gin.Context)) *gin.Engine {
        gin.SetMode(gin.TestMode)
        router := gin.New()
        router.Use(func(c *gin.Context) {
                setPrincipal(c, principal)
                c.Next()
        })
        router.POST("/gated", service.RequireStepUp(purpose), handler)
        return router
}

func serveGated(router *gin.Engine, token string) *httptest.ResponseRecorder {
        request := httptest.NewRequest(http.MethodPost, "/gated", nil)
        if token != "" {
                request.Header.Set(StepUpHeaderName, token)
        }
        response := httptest.NewRecorder()
        router.ServeHTTP(response, request)
        return response
}

// DB-free: an absent grant header is answered 403 stepup_required without
// ever touching the database (a nil pool would panic if it did — the test
// doubles as a regression pin for that short-circuit).
func TestRequireStepUpGateRejectsMissingGrant(t *testing.T) {
        service := &Service{} // db deliberately nil
        router := newStepUpGateRouter(
                &Principal{Type: CompanyUserPrincipal, ID: "00000000-0000-0000-0000-000000000001"},
                StepUpPurposeManagePasskeys, service,
                func(c *gin.Context) { c.Status(http.StatusNoContent) })
        response := serveGated(router, "")
        require.Equal(t, http.StatusForbidden, response.Code)
        require.Contains(t, response.Body.String(), "stepup_required")
}

// DB-gated: the full grant lifecycle through the real middleware.
func TestRequireStepUpGateGrantLifecycle(t *testing.T) {
        pool := passkeyTestPool(t)
        service := &Service{db: pool}
        ctx := context.Background()
        owner := seedCompanyPrincipal(t, pool)

        mint := func() string {
                token, err := service.issueStepUpGrant(ctx, owner, StepUpPurposeManagePasskeys, "password", nil, time.Minute)
                require.NoError(t, err)
                return token
        }

        registerRouter := newStepUpGateRouter(owner, StepUpPurposeManagePasskeys, service,
                func(c *gin.Context) { c.Status(http.StatusNoContent) })

        // Fresh grant → the gate opens and the handler is reached.
        token := mint()
        require.Equal(t, http.StatusNoContent, serveGated(registerRouter, token).Code)
        // Single-use: the SAME token never opens the gate twice.
        require.Equal(t, http.StatusForbidden, serveGated(registerRouter, token).Code)
        // A second fresh grant opens it again — the gate blocks REPLAYS, not
        // legitimate re-verification.
        require.Equal(t, http.StatusNoContent, serveGated(registerRouter, mint()).Code)

        // Purpose binding: a fresh manage_passkeys grant NEVER opens a
        // change_password gate.
        changeRouter := newStepUpGateRouter(owner, StepUpPurposeChangePassword, service,
                func(c *gin.Context) { c.Status(http.StatusNoContent) })
        require.Equal(t, http.StatusForbidden, serveGated(changeRouter, mint()).Code)
}
