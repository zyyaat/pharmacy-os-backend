package handlers

// Task 152 — DB-gated suite for the Zero-Permission Fail-Open fix.
//
// Runs against a DISPOSABLE database built by applying the real migration
// chain (migrations 1→40) — the classification asserted here is the LIVE
// catalog's, not a stub:
//
//      TEST_DATABASE_URL=postgres://.../pharmacy_scratch \
//        go test ./internal/handlers/ -run 'TestT152' -v
//
// Skipped when TEST_DATABASE_URL is unset (contract tests in
// permissions_gate_test.go still run everywhere).
//
// Fixtures: account → pharmacy → employee(s) with explicit
// permissions_initialized (the column is NOT NULL by design — every writer
// must state it consciously, which is itself part of the fix being tested).

import (
        "context"
        "fmt"
        "net/http"
        "net/http/httptest"
        "os"
        "testing"
        "time"

        "github.com/gin-gonic/gin"
        "github.com/jackc/pgx/v5/pgxpool"
)

type t152Fixture struct {
        pool     *pgxpool.Pool
        account  string
        pharmacy string
        h        *Handler
}

func t152Connect(t *testing.T) *t152Fixture {
        t.Helper()
        dsn := os.Getenv("TEST_DATABASE_URL")
        if dsn == "" {
                t.Skip("TEST_DATABASE_URL not set — these tests need a real disposable PostgreSQL")
        }
        pool, err := pgxpool.New(context.Background(), dsn)
        if err != nil {
                t.Fatalf("connect: %v", err)
        }
        t.Cleanup(pool.Close)
        f := &t152Fixture{pool: pool, h: &Handler{db: pool}}

        // Tenant fixture on the PARENT scope: account → pharmacy, shared by every
        // subtest, removed once when the whole test finishes (ON DELETE CASCADE
        // takes pharmacies → employees → employee_permissions with it).
        ctx := context.Background()
        stamp := time.Now().UnixNano()
        if err := pool.QueryRow(ctx, `
                INSERT INTO accounts (company_name, contact_email)
                VALUES ($1, $2) RETURNING id::text
        `, fmt.Sprintf("T152 Account %d", stamp), fmt.Sprintf("t152-root-%d@fixture.test", stamp),
        ).Scan(&f.account); err != nil {
                t.Fatalf("fixture account: %v", err)
        }
        if err := pool.QueryRow(ctx, `
                INSERT INTO pharmacies (account_id, name)
                VALUES ($1::uuid, $2) RETURNING id::text
        `, f.account, fmt.Sprintf("T152 Pharmacy %d", stamp),
        ).Scan(&f.pharmacy); err != nil {
                t.Fatalf("fixture pharmacy: %v", err)
        }
        t.Cleanup(func() {
                _, _ = pool.Exec(context.Background(),
                        `DELETE FROM accounts WHERE id = $1::uuid`, f.account)
        })
        return f
}

func (f *t152Fixture) newEmployee(t *testing.T, initialized bool) string {
        t.Helper()
        ctx := context.Background()
        var id string
        if err := f.pool.QueryRow(ctx, `
                INSERT INTO employees (account_id, pharmacy_id, email, first_name, last_name, permissions_initialized)
                VALUES ($1::uuid, $2::uuid, $3, 'T152', 'Fixture', $4) RETURNING id::text
        `, f.account, f.pharmacy,
                fmt.Sprintf("t152-%d-%v@fixture.test", time.Now().UnixNano(), initialized), initialized,
        ).Scan(&id); err != nil {
                t.Fatalf("fixture employee: %v", err)
        }
        return id
}

// grant inserts an active permission row; revoke marks every row of the
// employee revoked (soft delete — the history the three-state rule reads).
func (f *t152Fixture) grant(t *testing.T, employeeID, key string) {
        t.Helper()
        if _, err := f.pool.Exec(context.Background(), `
                INSERT INTO employee_permissions (employee_id, permission_id, notes)
                VALUES ($1::uuid, (SELECT id FROM permissions WHERE key = $2), 't152 fixture')
                ON CONFLICT (employee_id, permission_id) WHERE revoked_at IS NULL DO NOTHING
        `, employeeID, key); err != nil {
                t.Fatalf("fixture grant %s: %v", key, err)
        }
}

func (f *t152Fixture) revokeAll(t *testing.T, employeeID string) {
        t.Helper()
        if _, err := f.pool.Exec(context.Background(), `
                UPDATE employee_permissions SET revoked_at = NOW()
                WHERE employee_id = $1::uuid AND revoked_at IS NULL
        `, employeeID); err != nil {
                t.Fatalf("fixture revokeAll: %v", err)
        }
}

func (f *t152Fixture) ginCtx(t *testing.T) *gin.Context {
        t.Helper()
        gin.SetMode(gin.TestMode)
        ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
        ctx.Request = httptest.NewRequest(http.MethodGet, "/", nil)
        return ctx
}

// liveTierOf wires the pure classifier to the REAL catalog tiers.
func (f *t152Fixture) liveTierOf(ctx *gin.Context) func(string) string {
        return func(key string) string {
                tier, err := f.h.permissionTier(ctx, key)
                if err != nil {
                        return "" // the same fail-closed lens the middleware uses
                }
                return tier
        }
}

// ─────────────────────────────────────────────────────────────────────────────
// The three states, against real rows.
// ─────────────────────────────────────────────────────────────────────────────

func TestT152EmployeePermissionStateThreeStates(t *testing.T) {
        f := t152Connect(t)
        ctx := f.ginCtx(t)

        t.Run("zero_history_is_uninitialized", func(t *testing.T) {
                id := f.newEmployee(t, false) // backfill rule: zero rows ⇒ false
                keys, initialized, err := f.h.employeePermissionState(ctx, id)
                if err != nil {
                        t.Fatalf("state: %v", err)
                }
                if initialized {
                        t.Errorf("zero-history employee must be uninitialized, got initialized=true")
                }
                if len(keys) != 0 {
                        t.Errorf("zero-history employee must have no active keys, got %v", keys)
                }
        })

        t.Run("active_rows_are_initialized_with_keys", func(t *testing.T) {
                id := f.newEmployee(t, false)
                f.grant(t, id, "pos.access")
                f.grant(t, id, "sales.view")
                keys, initialized, err := f.h.employeePermissionState(ctx, id)
                if err != nil {
                        t.Fatalf("state: %v", err)
                }
                if !initialized {
                        t.Errorf("employee with active rows must be initialized")
                }
                if len(keys) != 2 {
                        t.Errorf("active keys = %v, want exactly pos.access+sales.view", keys)
                }
        })

        t.Run("only_revoked_rows_initialized_with_empty_keys", func(t *testing.T) {
                // The heart of contract 4: soft-deleted history is still history —
                // an employee whose permissions were all revoked is INITIALIZED and
                // empty ⇒ Deny All, never the old fail-open full access.
                id := f.newEmployee(t, false)
                f.grant(t, id, "pos.access")
                f.revokeAll(t, id)
                keys, initialized, err := f.h.employeePermissionState(ctx, id)
                if err != nil {
                        t.Fatalf("state: %v", err)
                }
                if !initialized {
                        t.Errorf("revoked history must count: initialized must be true")
                }
                if len(keys) != 0 {
                        t.Errorf("revoked rows must not surface as active keys, got %v", keys)
                }
        })
}

// ─────────────────────────────────────────────────────────────────────────────
// The seven contract scenarios through the REAL classifier + REAL tiers.
// ─────────────────────────────────────────────────────────────────────────────

func TestT152GateContractsAgainstLiveCatalog(t *testing.T) {
        f := t152Connect(t)
        ctx := f.ginCtx(t)
        tierOf := f.liveTierOf(ctx)

        revokedOnly := f.newEmployee(t, false)
        f.grant(t, revokedOnly, "pos.access") // active rows ⇒ initialized below
        f.revokeAll(t, revokedOnly)

        worker := f.newEmployee(t, true) // conscious save, then a working set
        f.grant(t, worker, "sales.view")

        admin := f.newEmployee(t, true)
        f.grant(t, admin, "pharmacy.admin")

        t.Run("contract1_initialized_active_unchanged_exact_match", func(t *testing.T) {
                keys, initialized, _ := f.h.employeePermissionState(ctx, worker)
                if !initialized || len(keys) == 0 {
                        t.Fatalf("fixture broken: worker must be initialized with keys")
                }
                if got := evaluatePermissionGate(initialized, keys, "sales.view", tierOf); got != gateAllow {
                        t.Errorf("worker with sales.view denied sales.view: %d", got)
                }
        })

        t.Run("contract2_uninitialized_operational_fallback_only", func(t *testing.T) {
                id := f.newEmployee(t, false)
                keys, initialized, _ := f.h.employeePermissionState(ctx, id)
                if initialized {
                        t.Fatalf("fixture broken: fresh employee must be uninitialized")
                }
                if got := evaluatePermissionGate(initialized, keys, "pos.access", tierOf); got != gateAllow {
                        t.Errorf("uninitialized employee denied operational pos.access: %d", got)
                }
                if got := evaluatePermissionGate(initialized, keys, "employees.create", tierOf); got != gateDenyUninitialized {
                        t.Errorf("uninitialized employee allowed employees.create: %d (the escalation gate!)", got)
                }
        })

        t.Run("contract3_initialized_empty_deny_all", func(t *testing.T) {
                id := f.newEmployee(t, true) // conscious empty save
                keys, initialized, _ := f.h.employeePermissionState(ctx, id)
                if !initialized || len(keys) != 0 {
                        t.Fatalf("fixture broken: want initialized with zero active keys")
                }
                if got := evaluatePermissionGate(initialized, keys, "pos.access", tierOf); got != gateDenyNoActiveKeys {
                        t.Errorf("initialized-empty employee allowed pos.access: %d", got)
                }
        })

        t.Run("contract4_only_revoked_deny_all", func(t *testing.T) {
                keys, initialized, _ := f.h.employeePermissionState(ctx, revokedOnly)
                if !initialized || len(keys) != 0 {
                        t.Fatalf("fixture broken: want revoked-only history ⇒ initialized, empty")
                }
                if got := evaluatePermissionGate(initialized, keys, "sales.view", tierOf); got != gateDenyNoActiveKeys {
                        t.Errorf("revoked-only employee allowed sales.view: %d", got)
                }
        })

        t.Run("contract5_active_keys_bypass_preserved", func(t *testing.T) {
                keys, initialized, _ := f.h.employeePermissionState(ctx, admin)
                if got := evaluatePermissionGate(initialized, keys, "employees.view", tierOf); got != gateAllow {
                        t.Errorf("pharmacy.admin holder denied employees.view: %d (bypass regressed)", got)
                }
        })

        t.Run("contract6_uninitialized_never_uses_pharmacy_admin", func(t *testing.T) {
                id := f.newEmployee(t, false)
                keys, initialized, _ := f.h.employeePermissionState(ctx, id)
                if got := evaluatePermissionGate(initialized, keys, "pharmacy.admin", tierOf); got != gateDenyUninitialized {
                        t.Errorf("uninitialized employee used pharmacy.admin: %d", got)
                }
        })

        t.Run("contract7_new_employee_empty_list_deny_all", func(t *testing.T) {
                // Mirrors CreatePharmacyEmployee's INSERT: permissions_initialized=TRUE
                // with no permission rows (the handler's source is pinned by
                // TestFailOpenBranchesAreGone). Result: initialized + Deny All.
                id := f.newEmployee(t, true)
                var flag bool
                if err := f.pool.QueryRow(context.Background(),
                        `SELECT permissions_initialized FROM employees WHERE id = $1::uuid`, id).Scan(&flag); err != nil {
                        t.Fatalf("read flag: %v", err)
                }
                if !flag {
                        t.Errorf("created employee must carry permissions_initialized=true")
                }
                keys, initialized, _ := f.h.employeePermissionState(ctx, id)
                if !initialized || len(keys) != 0 {
                        t.Fatalf("want initialized-empty state, got initialized=%v keys=%v", initialized, keys)
                }
                if got := evaluatePermissionGate(initialized, keys, "pos.access", tierOf); got != gateDenyNoActiveKeys {
                        t.Errorf("created-empty employee allowed pos.access: %d", got)
                }
        })
}

// ─────────────────────────────────────────────────────────────────────────────
// The grant guard (requirement 5) against the live catalog.
// ─────────────────────────────────────────────────────────────────────────────

func TestT152UninitializedGrantGuard(t *testing.T) {
        f := t152Connect(t)
        ctx := f.ginCtx(t)

        uninit := f.newEmployee(t, false)
        initialized := f.newEmployee(t, true)

        t.Run("uninitialized_grantor_cannot_grant_admin", func(t *testing.T) {
                blocked, key, err := f.h.uninitializedGrantBlocked(ctx, uninit, []string{"pharmacy.admin"})
                if err != nil {
                        t.Fatalf("guard: %v", err)
                }
                if !blocked || key != "pharmacy.admin" {
                        t.Errorf("uninitialized grantor passed pharmacy.admin: blocked=%v key=%q", blocked, key)
                }
        })

        t.Run("uninitialized_grantor_cannot_grant_employees_create", func(t *testing.T) {
                blocked, key, err := f.h.uninitializedGrantBlocked(ctx, uninit, []string{"employees.create"})
                if err != nil {
                        t.Fatalf("guard: %v", err)
                }
                if !blocked || key != "employees.create" {
                        t.Errorf("uninitialized grantor passed employees.create: blocked=%v key=%q", blocked, key)
                }
        })

        t.Run("uninitialized_grantor_may_grant_operational", func(t *testing.T) {
                blocked, _, err := f.h.uninitializedGrantBlocked(ctx, uninit, []string{"pos.access", "sales.view"})
                if err != nil {
                        t.Fatalf("guard: %v", err)
                }
                if blocked {
                        t.Errorf("operational keys must pass the uninitialized grant guard")
                }
        })

        t.Run("initialized_grantor_not_restricted_in_this_phase", func(t *testing.T) {
                // Full tier enforcement on grants is a later phase; this round only
                // pins the uninitialized invariant.
                blocked, _, err := f.h.uninitializedGrantBlocked(ctx, initialized, []string{"pharmacy.admin"})
                if err != nil {
                        t.Fatalf("guard: %v", err)
                }
                if blocked {
                        t.Errorf("initialized grantor must not be blocked in this phase")
                }
        })
}

// ─────────────────────────────────────────────────────────────────────────────
// The live catalog classification — the authoritative check the stubbed
// contract test mirrors.
// ─────────────────────────────────────────────────────────────────────────────

func TestT152LiveTierClassification(t *testing.T) {
        f := t152Connect(t)
        ctx := f.ginCtx(t)

        rows, err := f.pool.Query(ctx.Request.Context(), `SELECT key, tier FROM permissions`)
        if err != nil {
                t.Fatalf("catalog query: %v", err)
        }
        defer rows.Close()

        counts := map[string]int{}
        tierOf := map[string]string{}
        total := 0
        for rows.Next() {
                var key, tier string
                if err := rows.Scan(&key, &tier); err != nil {
                        t.Fatalf("scan: %v", err)
                }
                counts[tier]++
                tierOf[key] = tier
                total++
        }
        if err := rows.Err(); err != nil {
                t.Fatalf("rows: %v", err)
        }

        if total != 60 {
                t.Errorf("catalog size = %d, want 60", total)
        }
        for tier, want := range map[string]int{
                "operational": 27, "administrative": 14, "privileged": 2, "platform": 17,
        } {
                if counts[tier] != want {
                        t.Errorf("tier %s = %d keys, want %d", tier, counts[tier], want)
                }
        }

        spots := map[string]string{
                "employees.create":       "administrative", // the escalation gate
                "employees.view":         "operational",    // owner-settled
                "pharmacy.admin":         "privileged",     // master bypass
                "products.global.manage": "privileged",     // the misfiled key, restored
                "platform.admin":         "platform",
        }
        for key, want := range spots {
                if got := tierOf[key]; got != want {
                        t.Errorf("key %s tier = %q, want %q", key, got, want)
                }
        }

        // No NULL/unclassified tiers may exist (fail-closed default is administrative).
        var nulls int
        if err := f.pool.QueryRow(ctx.Request.Context(),
                `SELECT count(*) FROM permissions WHERE tier IS NULL`).Scan(&nulls); err != nil {
                t.Fatalf("null tier count: %v", err)
        }
        if nulls != 0 {
                t.Errorf("%d catalog rows without a tier (fail-closed default violated)", nulls)
        }
}
