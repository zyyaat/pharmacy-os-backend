// Task 165 regression gate — the migration ledger (public.schema_migrations)
// is PLATFORM infrastructure, not tenant surface: one shared database, so
// the version chain's titles reveal the product roadmap (passkeys, payments,
// support system...) and applied_at stamps reveal deployment cadence. Born
// in the tenant settings area (910a069) with a weak gate
// (requirePharmacyOwner = company_admin OR company_manager), it served the
// whole platform's build intelligence to any curious tenant owner — and the
// frontend hiding a tab is not backend protection.
//
// The pinned contract:
//   - GET /pharmacy/system/migrations answers 403 product_dev_tool to every
//     tenant company — the Flutter wire path survives (mobile/lib/core/
//     api_client.dart calls it) but only platform-company members pass;
//   - a requester with no principal gets 401, and one without a company
//     (a bare realm identity) gets 403 — fail-closed on every shape;
//   - a platform-company member gets 200 with the real ledger rows;
//   - GET /platform-admin/system/migrations (the tool's new home on the
//     product-development surface) answers 200 for the platform principal —
//     the group's PlatformRealm middleware is the only gate by design.
//
// Same harness as the other DB gates:
//
//      TEST_DATABASE_URL=postgres://.../pharmacy_scratch go test ./internal/handlers/ -run TestSystemMigrationsPlatformOnlyDB -v
package handlers

import (
        "context"
        "fmt"
        "net/http"
        "net/http/httptest"
        "strings"
        "testing"
        "time"

        "github.com/gin-gonic/gin"
        "github.com/pharmacy-os/backend/internal/auth"
        "github.com/stretchr/testify/require"
)

func TestSystemMigrationsPlatformOnlyDB(t *testing.T) {
        gin.SetMode(gin.TestMode)
        ctx := context.Background()
        pool := platformIsolationPool(t)
        suffix := fmt.Sprintf("%d", time.Now().UnixNano())

        // tenantCo: any paying pharmacy — the identity the ruling excludes.
        var tenantCoID string
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO companies (name, email) VALUES ($1, $2) RETURNING id::text
        `, "t165-tenant "+suffix, "t165-tenant-"+suffix+"@test.local").Scan(&tenantCoID))

        // platformCo: the shell as bootstrap.go creates it — marked from birth.
        // Frozen forever by the migration-45 trigger, so cleanup tolerates the
        // leftover (same hygiene doctrine as the isolation gate).
        var platformCoID string
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO companies (name, email, status, plan, is_platform)
                VALUES ($1, $2, 'active', 'enterprise', true) RETURNING id::text
        `, "t165-platform-shell "+suffix, "t165-shell-"+suffix+"@test.local").Scan(&platformCoID))

        t.Cleanup(func() {
                bg := context.Background()
                _, _ = pool.Exec(bg, `DELETE FROM companies WHERE id = $1`, tenantCoID)
                _, _ = pool.Exec(bg, `DELETE FROM companies WHERE id = $1`, platformCoID)
        })

        h := &Handler{db: pool}

        // Probe 1 — a tenant company owner: 403 product_dev_tool, never the ledger.
        r := gin.New()
        r.GET("/pharmacy/system/migrations", principalStub(&auth.Principal{
                ID: "cu-tenant", Type: auth.CompanyUserPrincipal, Role: "company_admin", CompanyID: tenantCoID,
        }), h.GetSystemMigrations)
        w := httptest.NewRecorder()
        r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/pharmacy/system/migrations", nil))
        require.Equal(t, http.StatusForbidden, w.Code, "a tenant company must never read the platform migration ledger: %s", w.Body.String())
        require.Contains(t, w.Body.String(), "product_dev_tool")

        // Probe 2 — a platform-company member: the ledger flows.
        r2 := gin.New()
        r2.GET("/pharmacy/system/migrations", principalStub(&auth.Principal{
                ID: "cu-platform", Type: auth.CompanyUserPrincipal, Role: "super_admin", CompanyID: platformCoID,
        }), h.GetSystemMigrations)
        w2 := httptest.NewRecorder()
        r2.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/pharmacy/system/migrations", nil))
        require.Equal(t, http.StatusOK, w2.Code, "platform members keep the tool: %s", w2.Body.String())
        require.Contains(t, w2.Body.String(), `"items"`)
        require.Contains(t, w2.Body.String(), `"total"`)

        // Probe 3 — no principal at all: 401, fail-closed.
        r3 := gin.New()
        r3.GET("/pharmacy/system/migrations", h.GetSystemMigrations)
        w3 := httptest.NewRecorder()
        r3.ServeHTTP(w3, httptest.NewRequest(http.MethodGet, "/pharmacy/system/migrations", nil))
        require.Equal(t, http.StatusUnauthorized, w3.Code)

        // Probe 4 — an identity without a company: 403, fail-closed.
        r4 := gin.New()
        r4.GET("/pharmacy/system/migrations", principalStub(&auth.Principal{
                ID: "bare", Type: auth.EmployeePrincipal,
        }), h.GetSystemMigrations)
        w4 := httptest.NewRecorder()
        r4.ServeHTTP(w4, httptest.NewRequest(http.MethodGet, "/pharmacy/system/migrations", nil))
        require.Equal(t, http.StatusForbidden, w4.Code)
        require.Contains(t, w4.Body.String(), "product_dev_tool")

        // Probe 5 — the product-development surface: the platform principal reads
        // the ledger on /platform-admin (in production the route group's
        // PlatformRealm middleware is the gate; wired directly here).
        r5 := gin.New()
        r5.GET("/platform-admin/system/migrations", principalStub(&auth.Principal{
                ID: "cu-platform", Type: auth.CompanyUserPrincipal, Role: "super_admin", CompanyID: platformCoID,
        }), h.GetPlatformSystemMigrations)
        w5 := httptest.NewRecorder()
        r5.ServeHTTP(w5, httptest.NewRequest(http.MethodGet, "/platform-admin/system/migrations", nil))
        require.Equal(t, http.StatusOK, w5.Code, "the tool's home must serve the ledger: %s", w5.Body.String())
        require.True(t, strings.Contains(w5.Body.String(), `"items"`))
}
