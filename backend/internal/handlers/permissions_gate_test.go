package handlers

// Task 152 — Zero-Permission Fail-Open fix: the contract tests.
//
// Two layers live here, both DB-free so CI runs them everywhere:
//
//  1. TestEvaluatePermissionGate — the pure three-state classifier behind
//     requirePharmacyPermission and pharmacyAllows. The seven scenarios are
//     the fix's acceptance contract agreed with the owner; a green build
//     means the GATE behaves, not that the classification is right — that is
//     layer 2's job (and the DB-gated suite's job against the live catalog).
//
//  2. TestMigration40TierClassification / TestFailOpenBranchesAreGone —
//     source contracts pinning migration 40's tier lists and the removal of
//     the fail-open branches. Tests can be built on a wrong classification;
//     these assert the classification itself (27 operational / 14
//     administrative / 2 privileged / 17 platform = the 60-key catalog),
//     including the two keys whose misclassification caused this round:
//     employees.create MUST be administrative, products.global.manage MUST
//     be privileged.

import (
        "os"
        "regexp"
        "sort"
        "strings"
        "testing"
)

// stubTierOf builds a fail-closed tier lens from a static map, mirroring what
// permissionTier + the runtime closure do against the live catalog.
func stubTierOf(tiers map[string]string) func(string) string {
        return func(key string) string {
                return tiers[key] // missing key → "" → never operational
        }
}

// minimalTierStub classifies only what a test needs; everything else fails
// closed. employees.create is deliberately administrative here — the exact
// misclassification the disputed draft made (and the escalation chain's gate).
func minimalTierStub() map[string]string {
        return map[string]string{
                "pos.access":       "operational",
                "sales.view":       "operational",
                "employees.view":   "operational",
                "employees.create": "administrative",
                "pharmacy.admin":   "privileged",
        }
}

func TestEvaluatePermissionGate(t *testing.T) {
        tierOf := stubTierOf(minimalTierStub())

        cases := []struct {
                name         string
                initialized  bool
                activeKeys   []string
                requiredKey  string
                want         gateDecision
                wantDenialRx string // only for deny cases (documentational)
        }{
                // ── Contract 1/5 — initialized + active rows behave exactly as before
                // the fix: exact key match allowed, pharmacy.admin master bypass kept.
                {
                        name:        "initialized_with_rows_exact_match_allowed_unchanged",
                        initialized: true,
                        activeKeys:  []string{"sales.view"},
                        requiredKey: "sales.view",
                        want:        gateAllow,
                },
                {
                        name:        "initialized_admin_holder_bypass_still_works",
                        initialized: true,
                        activeKeys:  []string{"sales.view", "pharmacy.admin"},
                        requiredKey: "pos.access",
                        want:        gateAllow,
                },
                {
                        name:        "initialized_without_match_denied",
                        initialized: true,
                        activeKeys:  []string{"sales.view"},
                        requiredKey: "pos.access",
                        want:        gateDenyNoMatch,
                },

                // ── Contract 2 — uninitialized + zero history: operational-only
                // fallback. employees.view settled operational by the owner.
                {
                        name:        "uninitialized_operational_key_allowed",
                        initialized: false,
                        activeKeys:  []string{},
                        requiredKey: "pos.access",
                        want:        gateAllow,
                },
                {
                        name:        "uninitialized_employees_view_operational_allowed",
                        initialized: false,
                        activeKeys:  []string{},
                        requiredKey: "employees.view",
                        want:        gateAllow,
                },

                // ── Contract 6 — uninitialized must never use administrative or
                // privileged keys: employees.create (the escalation gate) and
                // pharmacy.admin (the master bypass) are denied, bypass not applicable.
                {
                        name:        "uninitialized_employees_create_denied",
                        initialized: false,
                        activeKeys:  []string{},
                        requiredKey: "employees.create",
                        want:        gateDenyUninitialized,
                },
                {
                        name:        "uninitialized_pharmacy_admin_denied",
                        initialized: false,
                        activeKeys:  []string{},
                        requiredKey: "pharmacy.admin",
                        want:        gateDenyUninitialized,
                },

                // ── Contracts 3/4 — initialized + zero ACTIVE rows: Deny All, both
                // empty flavors (conscious empty save / only revoked rows). Holding
                // pharmacy.admin is impossible in this state (rows ⇒ initialized with
                // keys), so nothing may pass.
                {
                        name:        "initialized_empty_deny_all",
                        initialized: true,
                        activeKeys:  []string{},
                        requiredKey: "pos.access",
                        want:        gateDenyNoActiveKeys,
                },
                {
                        name:        "only_revoked_rows_modelled_as_initialized_empty_deny_all",
                        initialized: true,
                        activeKeys:  []string{},
                        requiredKey: "pharmacy.admin",
                        want:        gateDenyNoActiveKeys,
                },

                // ── Fail-closed edges: unknown key and a nil lens never read operational.
                {
                        name:        "uninitialized_unknown_key_denied",
                        initialized: false,
                        activeKeys:  []string{},
                        requiredKey: "catalog.key.not.in.stub",
                        want:        gateDenyUninitialized,
                },
                {
                        name:        "uninitialized_nil_tier_lens_denied",
                        initialized: false,
                        activeKeys:  []string{},
                        requiredKey: "pos.access",
                        want:        gateDenyUninitialized,
                },
        }

        for _, tc := range cases {
                tc := tc
                t.Run(tc.name, func(t *testing.T) {
                        lens := tierOf
                        if tc.name == "uninitialized_nil_tier_lens_denied" {
                                lens = nil
                        }
                        got := evaluatePermissionGate(tc.initialized, tc.activeKeys, tc.requiredKey, lens)
                        if got != tc.want {
                                t.Fatalf("evaluatePermissionGate(initialized=%v, keys=%v, required=%q) = %d, want %d",
                                        tc.initialized, tc.activeKeys, tc.requiredKey, got, tc.want)
                        }
                })
        }
}

// ─────────────────────────────────────────────────────────────────────────────
// Source contract: migration 40's classification IS the agreed design.
// ─────────────────────────────────────────────────────────────────────────────

const migration40File = "../../migrations/00000000000040_permission_tiers_initialization.sql"

// migration40TierKeys extracts the key list of each
// `UPDATE permissions SET tier = '<tier>' WHERE key IN (…)` block.
func migration40TierKeys(t *testing.T, sql string) map[string][]string {
        t.Helper()
        tiers := map[string][]string{}
        blockRx := regexp.MustCompile(`(?s)UPDATE permissions SET tier = '([a-z]+)' WHERE key IN \((.*?)\);`)
        // Same shape as the catalog's CHECK constraint (mig3): module.action or
        // module.area.action — multi-segment keys like products.pharmacy.add.
        keyRx := regexp.MustCompile(`'([a-z][a-z_]*(?:\.[a-z_]+)+)'`)
        for _, m := range blockRx.FindAllStringSubmatch(sql, -1) {
                tier, body := m[1], m[2]
                for _, k := range keyRx.FindAllStringSubmatch(body, -1) {
                        tiers[tier] = append(tiers[tier], k[1])
                }
        }
        return tiers
}

func loadMigration40(t *testing.T) string {
        t.Helper()
        sql, err := os.ReadFile(migration40File)
        if err != nil {
                t.Fatalf("read migration 40: %v", err)
        }
        return string(sql)
}

func TestMigration40TierClassification(t *testing.T) {
        tiers := migration40TierKeys(t, loadMigration40(t))

        op, adm, priv, plat := tiers["operational"], tiers["administrative"], tiers["privileged"], tiers["platform"]
        if len(op) != 27 {
                t.Errorf("operational = %d keys, want 27 (got %v)", len(op), op)
        }
        if len(adm) != 14 {
                t.Errorf("administrative = %d keys, want 14 (got %v)", len(adm), adm)
        }
        if len(priv) != 2 {
                t.Errorf("privileged = %d keys, want 2 (got %v)", len(priv), priv)
        }
        if len(plat) != 17 {
                t.Errorf("platform = %d keys, want 17 (got %v)", len(plat), plat)
        }
        if total := len(op) + len(adm) + len(priv) + len(plat); total != 60 {
                t.Errorf("classified keys = %d, want 60", total)
        }

        // The two keys this whole verification round exists for:
        if !contains(adm, "employees.create") {
                t.Errorf("employees.create MUST be administrative (the create-employee → grant-admin escalation gate)")
        }
        if !contains(priv, "products.global.manage") {
                t.Errorf("products.global.manage MUST be privileged (agreed design; the misfiled 'platform' bucket broke the 43/17 boundary)")
        }
        if !contains(priv, "pharmacy.admin") {
                t.Errorf("pharmacy.admin MUST be privileged (master bypass key)")
        }
        if !contains(op, "employees.view") {
                t.Errorf("employees.view MUST be operational (owner-settled decision)")
        }
        if contains(op, "employees.create") {
                t.Errorf("employees.create must NOT be operational")
        }

        // No key may appear in two tiers, and the pharmacy realm is exactly 43.
        seen := map[string]string{}
        for tier, keys := range tiers {
                for _, k := range keys {
                        if prev, dup := seen[k]; dup {
                                t.Errorf("key %s classified twice: %s and %s", k, prev, tier)
                        }
                        seen[k] = tier
                }
        }
        if realm := len(op) + len(adm) + len(priv); realm != 43 {
                t.Errorf("pharmacy realm = %d keys, want 43 (the catalog boundary the broken draft moved)", realm)
        }

        // Sorted stability: the lists must be sorted (keeps the migration diffable
        // and the contract readable).
        for tier, keys := range tiers {
                sorted := append([]string(nil), keys...)
                sort.Strings(sorted)
                if strings.Join(keys, ",") != strings.Join(sorted, ",") {
                        t.Errorf("tier %s list not sorted", tier)
                }
        }
}

func contains(list []string, want string) bool {
        for _, k := range list {
                if k == want {
                        return true
                }
        }
        return false
}

// TestFailOpenBranchesAreGone — tombstone for the exact branches this fix
// removed. If either string reappears, the fail-open is back.
func TestFailOpenBranchesAreGone(t *testing.T) {
        tombstones := []struct {
                file  string
                fingerprint string
        }{
                {"permissions_handler.go", "Legacy employees with zero explicit rows keep full access"},
                {"sync_handler.go", "return true // legacy employees with no explicit rows keep full access"},
        }
        for _, tc := range tombstones {
                src, err := os.ReadFile(tc.file)
                if err != nil {
                        t.Fatalf("read %s: %v", tc.file, err)
                }
                if strings.Contains(string(src), tc.fingerprint) {
                        t.Errorf("%s: the zero-permission fail-open branch is back: %q", tc.file, tc.fingerprint)
                }
        }

        // And the fix's fingerprints must exist: the three-state gate in both
        // enforcement paths, plus the conscious-initialization writes.
        required := []struct {
                file  string
                fingerprint string
        }{
                {"permissions_handler.go", "evaluatePermissionGate(initialized, keys, permissionKey, tierOf)"},
                {"sync_handler.go", "evaluatePermissionGate(initialized, keys, permissionKey, tierOf)"},
                {"permissions_handler.go", "permissions_initialized\n                ) VALUES"},
                {"permissions_handler.go", "permissions_initialized = TRUE, updated_at = NOW()"},
                {"permissions_handler.go", "uninitializedGrantBlocked(c, principal.ID, wanted)"},
                {"permissions_handler.go", "uninitializedGrantBlocked(c, principal.ID, keys)"},
        }
        for _, tc := range required {
                src, err := os.ReadFile(tc.file)
                if err != nil {
                        t.Fatalf("read %s: %v", tc.file, err)
                }
                if !strings.Contains(string(src), tc.fingerprint) {
                        t.Errorf("%s: missing required fingerprint: %q", tc.file, tc.fingerprint)
                }
        }
}

// TestMigration40RegisteredInChain — migration 40 must be part of the ordered
// migrationChain (the same registration slip that once bit migration 39).
func TestMigration40RegisteredInChain(t *testing.T) {
        src, err := os.ReadFile("../database/migrator.go")
        if err != nil {
                t.Fatalf("read migrator.go: %v", err)
        }
        if !strings.Contains(string(src), `"00000000000040_permission_tiers_initialization.sql"`) {
                t.Errorf("migration 40 is not registered in migrationChain (internal/database/migrator.go)")
        }
}
