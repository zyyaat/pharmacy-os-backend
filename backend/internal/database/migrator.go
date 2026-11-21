// Package database contains database bootstrap and migration helpers.
package database

import (
        "context"
        "fmt"
        "log"

        "github.com/jackc/pgx/v5/pgconn"
        "github.com/jackc/pgx/v5/pgxpool"
        "github.com/pharmacy-os/backend/migrations"
)

const migrationLockKey int64 = 849273641

type migration struct {
        name string
        // legacyMarkers lists tables created by the migration. When a database
        // already contains them but has no ledger row (e.g. a schema applied
        // manually before the runner existed), the migration is adopted instead
        // of replayed. Migrations that only alter existing objects leave this
        // empty so they are always executed when missing from the ledger.
        legacyMarkers []string
}

// migrationChain is the ordered schema history for Pharmacy OS. The
// *_init.sql and *_permissions_system.sql files are legacy placeholders
// from an earlier schema and are intentionally not part of this chain.
var migrationChain = []migration{
        {
                name:          "00000000000001_foundation.sql",
                legacyMarkers: []string{"accounts", "pharmacies", "employees", "branches"},
        },
        {
                name:          "00000000000002_products_inventory.sql",
                legacyMarkers: []string{"global_products", "pharmacy_products", "inventory_batches", "stock_movements"},
        },
        {
                name:          "00000000000003_permissions_auth.sql",
                legacyMarkers: []string{"permissions", "roles", "employee_permissions"},
        },
        {
                name:          "00000000000004_audit_logs.sql",
                legacyMarkers: []string{"audit_logs", "attendance_records"},
        },
        {
                name:          "00000000000005_holding_company.sql",
                legacyMarkers: []string{"companies", "company_users", "company_user_permissions"},
        },
        {
                name:          "00000000000006_go_auth.sql",
                legacyMarkers: []string{"auth_sessions", "auth_email_tokens"},
        },
        {
                name: "00000000000007_inventory_idempotency.sql",
        },
        {
                name: "00000000000008_auth_realms.sql",
        },
        {
                name: "00000000000009_publish_compatible_views.sql",
        },
        {
                name:          "00000000000010_platform_super_admin_singleton.sql",
                legacyMarkers: []string{"platform_bootstrap_state"},
        },
        {
                name:          "00000000000011_packaging_and_sales.sql",
                legacyMarkers: []string{"sales", "sale_items"},
        },
        {
                name:          "00000000000012_platform_trial_settings.sql",
                legacyMarkers: []string{"platform_settings"},
        },
        {
                name: "00000000000013_company_actor_cleanup.sql",
        },
        {
                name: "00000000000014_money_piastres.sql",
        },
        {
                name: "00000000000015_sales_history_returns.sql",
        },
        {
                name: "00000000000016_keep_out_of_stock_visible.sql",
        },
        {
                name: "00000000000017_fuzzy_search_trgm.sql",
        },
        {
                name: "00000000000018_pharmacy_settings.sql",
        },
        {
                name: "00000000000019_sales_discount_customers.sql",
        },
        {
                name: "00000000000020_flexible_permissions.sql",
        },
        {
                name: "00000000000021_account_locale.sql",
        },
        {
                name: "00000000000022_internal_barcode.sql",
        },
        {
                name: "00000000000023_label_settings_permission.sql",
        },
        {
                name: "00000000000024_sale_quantity_snapshot.sql",
        },
        {
                name: "00000000000025_delta_sync.sql",
        },
        {
                name: "00000000000026_saas_plans.sql",
        },
        {
                // Wired retroactively: the file existed on disk but was never
                // in this chain, so payments.idempotency_key (and its
                // replay-protection index) were missing while the manual
                // payment endpoint already referenced the column — any
                // rebuild would 42703 on first manual payment.
                name: "00000000000027_billing_hardening.sql",
        },
        {
                name: "00000000000028_saas_plan_seed_repair.sql",
        },
        {
                // Per-company entitlement overrides (plan baseline + per-account
                // grants/denies/limit tweaks) + the platform audit log table.
                name: "00000000000029_company_entitlements.sql",
        },
        {
                // Platform actions were silently un-audited: the tenant
                // writeAuditLog needs a pharmacy scope a platform principal
                // does not have. This table hosts global billing events with
                // an optional company_id for the per-company logs page.
                name: "00000000000030_platform_audit_logs.sql",
        },
        {
                // Central product libraries: named, country-targeted,
                // versioned catalogs with official prices per library entry,
                // an append-only change log powering pharmacy diffs, and
                // per-pharmacy sync pointers. global_products gains
                // is_verified + source provenance.
                name:          "00000000000031_product_libraries.sql",
                legacyMarkers: []string{"product_libraries", "library_products", "library_changes", "pharmacy_library_syncs"},
        },
        {
                // Payment settlement & subscription reconciliation: separates
                // CONFIRMED (provider captured — webhook/sync/manual) from
                // SETTLED (money reached our bank — operator-verified from
                // the XPay payout batch, since XPay has no payout API/webhook).
                // payment_settlements (one row per online payment) + payments
                // confirmation/failure/review/refund-amount columns + SUB-/PAY-
                // human references via sequence triggers + the event ledger's
                // sync/settlement/review/status_change txn types.
                name:          "00000000000032_payment_settlements.sql",
                legacyMarkers: []string{"payment_settlements"},
        },
        {
                // Support system (live chat + tickets): support_conversations
                // (the per-company chat channel with per-side unread counters
                // and close semantics), append-only support_messages (system
                // rows record lifecycle inside the chat), support_attachments
                // (png/jpeg/webp/pdf <= 2 MiB inline) and support_tickets
                // (TKT-00001 references via sequence trigger, the full
                // open/in_progress/waiting_customer/resolved/closed lifecycle
                // with reopen). Support is never plan-gated.
                name:          "00000000000033_support_system.sql",
                legacyMarkers: []string{"support_conversations", "support_messages", "support_tickets", "support_attachments"},
        },
        {
                // Email-verification attempt wall (AUD-004, Task 120):
                // per-email fixed-window counter consumed BEFORE any code
                // comparison. Production already runs this table (applied
                // manually during the Task 135 P0 repair era) — the marker
                // adopts it so the ledger catches up without replaying.
                name:          "00000000000034_email_verify_attempts.sql",
                legacyMarkers: []string{"auth_verify_attempts"},
        },
        {
                // Task 135 P0 repair — deterministic rebuild of the attempt
                // wall (the drifted table that migration 34's IF NOT EXISTS
                // could not heal). DESTRUCTIVE by design (DROP TABLE):
                // every database this chain has ever touched adopts it via
                // the marker (the table exists there); only a truly fresh
                // install executes it, where the destructive guard bypasses
                // (nothing to lose).
                name:          "00000000000035_verify_attempts_repair.sql",
                legacyMarkers: []string{"auth_verify_attempts"},
        },
        {
                // Ledger FK preservation (AUD-007 P0, Task 138): the six
                // ledger/history FKs become ON DELETE SET NULL with nullable
                // columns — deleting a pharmacy/company/batch can no longer
                // silently erase sales, returns, stock movements, payments,
                // or audit logs. Self-verifying (refuses any remaining
                // CASCADE). ALTER-only by construction: no legacy marker
                // (nothing new to adopt); the dynamic constraint drop
                // carries IF EXISTS so the scanner treats it as replay-safe
                // housekeeping and the wall never holds production hostage.
                name: "00000000000036_ledger_fk_preservation.sql",
        },
        {
                // Payment webhook dedup at the database (AUD-010/032, Task
                // 139): partial unique index on provider_transaction_id for
                // webhook rows — a replayed provider event can no longer
                // write two rows. Refuses loudly on existing duplicates
                // (named repair step, wall philosophy: never silently
                // rewrite ledger rows), replays as a no-op (IF NOT EXISTS).
                name: "00000000000037_payment_txn_dedup.sql",
        },
        {
                // Outbound email fair-use budget (Task 148): per-recipient
                // hour/day counters (auth_email_send_budget) + the
                // platform-wide daily ceiling (platform_email_sends).
                name: "00000000000038_email_send_budget.sql",
        },
        {
                // WebAuthn passkeys (Task 151): passkey_credentials (public
                // key material only), auth_challenges (the single-use,
                // purpose-bound challenge ledger behind registration, login,
                // recovery and step-up) and auth_stepup_grants (scoped,
                // single-use step-up proofs). Additive-only: the existing
                // password paths are untouched.
                name: "00000000000039_passkeys.sql",
        },
        {
                // Task 152 — Zero-Permission Fail-Open fix (additive-only, no
                // legacy markers: no pre-existing database can already carry
                // these columns). Adds permissions.tier (the catalog-level
                // classification driving the uninitialized-employee fallback:
                // 27 operational / 14 administrative / 2 privileged pharmacy
                // keys + 17 platform keys, fail-closed default) and
                // employees.permissions_initialized (no DEFAULT on purpose:
                // backfill = EXISTS over ALL historical employee_permissions
                // rows including revoked ones, then SET NOT NULL). Grants
                // nothing, revokes nothing, deletes nothing — currently-active
                // permission rows keep working unchanged.
                name: "00000000000040_permission_tiers_initialization.sql",
        },
        {
                // Task 153 — SECURITY DEFINER hardening + deny-by-default
                // database boundary (the layer beneath the Task 152 gates).
                // The eight SECURITY DEFINER functions (migrations 1/3/4/5)
                // ran with PostgreSQL defaults: EXECUTE granted to PUBLIC and
                // no pinned search_path. On the managed provider the `public`
                // schema is served through the PostgREST Data API by default,
                // so grant_permission_to_employee (same-pharmacy check only,
                // no caller-authority check) was callable by any bearer of
                // the project key — the same escalation class Task 152 closed
                // at the handler layer, one layer deeper. This migration pins
                // search_path (public, pg_temp) on all eight, revokes EXECUTE
                // from PUBLIC and the provider's JWT roles, revokes CREATE on
                // schema public, enables deny-by-default RLS (zero policies)
                // on every public table lacking it (owner/runtime role is
                // unaffected — migrations run with the runtime DATABASE_URL,
                // so it owns every object), and locks the four owner-
                // privileged views from PUBLIC/JWT reads. The backend calls
                // none of the eight (repo-wide: zero call sites), so this
                // cannot break the app; the migration self-verifies and
                // refuses to boot on drift. Additive-only (REVOKE/ALTER/ENABLE).
                name: "00000000000041_security_definer_hardening.sql",
        },
        {
                // M1/G3 (api_level 98) — janitor support for the two WebAuthn
                // ephemeral tables. auth/janitor.go now reaps expired
                // challenges and dead step-up grants every 10 minutes (they
                // previously accumulated forever); these indexes keep the
                // reap predicates index-driven (grants had no expiry index
                // at all). Additive-only, replay-safe, no legacy markers.
                name: "00000000000042_auth_artifact_janitor.sql",
        },
        {
                // P1 (api_level 101) — R-1: current_inventory reads the
                // transactionally-maintained inventory_batches.quantity once
                // per row (was: up to 4 SUM-subplans per row = up to 2000
                // aggregate subplans per inventory request at LIMIT 500).
                // Same columns, order and types as migration 16; status
                // semantics unchanged. Plus the two statement-path indexes
                // supporting R-6 (sales customer/credit, payments customer).
                // Additive-only: view recreation + two CREATE INDEX IF NOT
                // EXISTS; no legacy markers needed (idempotent replay-safe).
                name: "00000000000043_current_inventory_cache_read.sql",
        },
        {
                // P2 (api_level 102) — R-10 + R-7. R-10: thirteen hot-FK
                // indexes (sale_items.batch_id — the hottest join — returns
                // path, sales branch/employee, returns actors, movements
                // actor, customer_payments.pharmacy_id, plan RESTRICTs);
                // PostgreSQL never auto-indexes FK columns. R-7: the two
                // ACTOR columns migration 13 pinned on CASCADE
                // (sales.company_user_id,
                // stock_movements.created_by_company_user_id) become
                // SET NULL like migration 36's six organizational ledger
                // FKs — a hard company delete can no longer silently erase
                // sales/movement history one hop over. The wrinkle 36 never
                // had: migration 12's write-time CHECKs
                // (sales_actor_required / stock_movements_actor_required)
                // would trip on the referential action's own UPDATE and
                // abort company deletion with 23514, so the invariant moves
                // to BEFORE triggers enforcing it only for application-
                // initiated writes (pg_trigger_depth() < 2 — the documented
                // exemption for RI actions; detached rows stay updatable on
                // other columns because the trigger scopes to UPDATE OF the
                // actor columns). Self-verification = 36's checklist
                // EXTENDED with the actor columns + trigger presence gate.
                // Additive/re-aiming only: no column dropped, no data
                // rewritten, index builds replay-safe.
                name: "00000000000044_hot_fk_indexes_actor_preserve.sql",
        },
        {
                // Task 164 (api_level 103) — platform company isolation: the
                // bootstrap shell company hosting the super admin gains the
                // is_platform discriminator, gets marked on existing
                // databases, and is frozen by a BEFORE UPDATE/DELETE trigger
                // (status, soft delete, unmarking, hard delete). The
                // application layer (Task 164 guards) returns clean 403s and
                // excludes the company from every customer list; this
                // migration is the invariant's backstop.
                name: "00000000000045_platform_company_isolation.sql",
        },
        {
                // Task 167 — the Egyptian drug library: seeds the first REAL
                // platform library (published, EG/EGP, version 1) with ~27K
                // market drugs from the open eg-drugs dataset (CC BY 4.0) —
                // checksum-verified EAN-13 barcodes, manufacturers, active
                // ingredients, retail prices. Additive-only data seed with
                // idempotent guards (ON CONFLICT barcode / NOT EXISTS name);
                // no legacy markers — a database that already ran it is
                // protected by the ledger, and the guards keep a partial
                // manual application from double-inserting.
                name: "00000000000046_egyptian_drug_library.sql",
        },
        {
                // Task 168 — planner statistics after the wholesale seed:
                // migration 46 bulk-inserted ~27K catalog rows and ended
                // WITHOUT ANALYZE, so every library flow ran on stale row
                // estimates (the wholesale dedup went from seconds to minutes
                // on the measured scratch). Additive ANALYZE only — no shape
                // change, no data touch, safe to re-run via the ledger.
                name: "00000000000047_library_seed_statistics.sql",
        },
        {
                // Product ownership («المنتج ملك الصيدلية», api_level 117) —
                // pharmacy_products gains the local identity override columns
                // (name/generic/barcode/type/strength/dosage form) so every
                // pharmacy edit of a product — library-imported or official —
                // stays pharmacy-local and global_products is never written
                // from pharmacy context again (the T-B1 403 retires). Plus
                // source_library_id provenance (which library CREATED the
                // row) powering the pharmacy-side library removal, a
                // per-pharmacy unique index on local barcodes, and
                // current_inventory rebuilt with effective identity
                // (COALESCE override → official) preserving migrations
                // 16/43's exact column contract. Additive + view recreation;
                // replay-safe. (Originally authored as 45/46; renumbered
                // above the Tasks-164/167/47 chain on merge.)
                name: "00000000000048_product_ownership.sql",
        },
        {
                // Invoice identity snapshot — sale_items freezes the effective
                // product name/barcode/strength at sale time (with the local
                // override of that moment), so editing a product never
                // rewrites yesterday's invoices; legacy rows stay NULL and
                // keep the live-read fallback, same contract as migration
                // 24's quantity snapshot. Additive-only.
                name: "00000000000049_sale_identity_snapshot.sql",
        },
        {
                // Provenance backfill — the Egypt-library imports that landed
                // between Tasks 167/168 and this wave hold rows without
                // source_library_id, so the removal feature would see 0/0
                // for them. Conservative stamp: membership in a library the
                // pharmacy has a sync pointer for AND the row was created at
                // or after that pointer's FIRST import time — pre-library
                // products stay NULL and are therefore KEPT on removal (the
                // owner's safe-direction decision). Idempotent.
                name: "00000000000050_removal_provenance_backfill.sql",
        },
        {
                // River Queue background jobs, part 1 of 2 (api_level 118) —
                // the official River v0.51.2 main migrations 001..004
                // embedded so the system keeps ONE chain (rivermigrate is
                // never run at runtime). Split from the 005..008 half
                // (migration 52) because one file runs inside ONE
                // transaction and PostgreSQL ≤ 16 — production's engine —
                // forbids USING an ALTER TYPE ADD VALUE inside the
                // transaction that added it (SQLSTATE 55P04): migration 52's
                // function and index reference 'pending', which this file
                // adds. PostgreSQL 17 relaxed the restriction, which is why
                // the originally-combined file passed every local gate and
                // CI while failing the production boot. Additive-only.
                name: "00000000000051_river_background_jobs.sql",
        },
        {
                // River Queue background jobs, part 2 of 2 (api_level 118) —
                // the official River main migrations 005..008: unique-jobs
                // columns/indexes, river_job_state_in_bitmask (references
                // the 'pending' value committed by migration 51), the
                // notification outbox, final column defaults — plus the
                // river_migration ledger stamp 1..8, the dedicated
                // imports/mail queue rows, and library_import_runs, the run
                // ledger the status endpoint reads so the wire never touches
                // river_job internals. Additive-only.
                name: "00000000000052_river_background_jobs_unique.sql",
        },
        {
                // D1+D3 catalog wave (api_level 122) on top of migration 48's
                // effective identity: REAL local products (origin_type;
                // global_product_id now NULLABLE; a local row carries NO
                // catalog link and its identity lives in the migration-48
                // override columns, name_override required — a CHECK pins
                // both shapes and pre-existing rows backfill 'central') and
                // the append-only selling-price ledger
                // (pharmacy_product_price_history with RLS from day one,
                // written by the app inside the mutating transaction).
                // current_inventory rebuilt with migration 48's shape
                // preserved verbatim, INNER JOIN catalog -> LEFT JOIN so a
                // local product appears in every list/report/sync. D4 (unlink)
                // and D6 (barcode fast path) are application flows on these
                // columns. Self-verification refuses to boot on drift;
                // replay-safe (guarded policy block).
                name: "00000000000053_local_products_price_history.sql",
        },
        {
                // تسجيل الدخول بجوجل (api_level 123): company_users.google_sub
                // مع فهرس فريد جزئي — عمود الربط الدائم لهوية جوجل. Additive-only
                // (ADD COLUMN + فهرس IF NOT EXISTS)، بلا legacy markers (لا يوجد
                // نشر سابق أنشأه يدويًا).
                name: "00000000000054_google_identity.sql",
        },
        {
                // الطريقة الثالثة للتحقق الثانوي (رمز بريد 6 أرقام لحسابات
                // جوجل بلا كلمة مرور/مفاتيح): توسيع قيد purpose في
                // auth_email_tokens بقيمة stepup_email — إعادة إنشاء القيد
                // بقائمة أوسع بلا مساس بالصفوف القائمة. Additive بحت.
                name: "00000000000055_stepup_email_purpose.sql",
        },
}

// RunMigrations creates the migration ledger and applies any missing
// migrations. It is safe to call on every application startup.
func RunMigrations(ctx context.Context, db *pgxpool.Pool) error {
        conn, err := db.Acquire(ctx)
        if err != nil {
                return fmt.Errorf("acquire migration connection: %w", err)
        }
        defer conn.Release()

        if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
                return fmt.Errorf("lock migrations: %w", err)
        }
        defer func() {
                _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockKey)
        }()

        if _, err := conn.Exec(ctx, `
                CREATE TABLE IF NOT EXISTS public.schema_migrations (
                        version TEXT PRIMARY KEY,
                        applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
                )
        `); err != nil {
                return fmt.Errorf("create migration ledger: %w", err)
        }

        // AUD-007 — a database with no pre-existing tables has nothing to
        // lose: the destructive guard stands down for the very first
        // bootstrap so a fresh install can run the full historical chain
        // (which contains destructive migrations) unattended.
        freshInstall, err := databaseIsFresh(ctx, conn)
        if err != nil {
                return fmt.Errorf("inspect database freshness: %w", err)
        }
        appEnv := appEnvName()
        allowRaw := allowDestructiveRaw()

        for _, item := range migrationChain {
                applied, err := migrationApplied(ctx, conn, item.name)
                if err != nil {
                        return fmt.Errorf("check migration %s: %w", item.name, err)
                }
                if applied {
                        log.Printf("[MIGRATIONS] Skipping already applied migration %s", item.name)
                        continue
                }

                // Older deployments may already contain the complete schema but not
                // the ledger. In that case, adopt the existing schema instead of
                // replaying non-idempotent legacy SQL against production data.
                if len(item.legacyMarkers) > 0 {
                        exists, err := tablesExist(ctx, conn, item.legacyMarkers)
                        if err != nil {
                                return fmt.Errorf("inspect migration %s: %w", item.name, err)
                        }
                        if exists {
                                if err := recordMigration(ctx, conn, item.name); err != nil {
                                        return fmt.Errorf("record existing migration %s: %w", item.name, err)
                                }
                                log.Printf("[MIGRATIONS] Existing schema detected; recording %s as applied", item.name)
                                continue
                        }
                }

                sqlBytes, err := migrations.FS.ReadFile(item.name)
                if err != nil {
                        return fmt.Errorf("read migration %s: %w", item.name, err)
                }

                // AUD-007 — gate destructive migrations before execution.
                if findings := scanDestructiveStatements(string(sqlBytes)); len(findings) > 0 {
                        log.Printf("[MIGRATIONS] %s is DESTRUCTIVE (%d statement(s)): %s",
                                item.name, len(findings), summarizeFindings(findings))

                        gate := destructiveGate{
                                Migration:    item.name,
                                Findings:     findings,
                                FreshInstall: freshInstall,
                                AppEnv:       appEnv,
                                AllowRaw:     allowRaw,
                        }
                        decision, blockErr := gate.decision()
                        switch decision {
                        case decisionFreshInstall:
                                log.Printf("[MIGRATIONS] %s: destructive guard bypassed (fresh install, nothing to lose)", item.name)
                        case decisionDevelopment:
                                log.Printf("[MIGRATIONS] %s: destructive guard allows development (disposable data) — production would require ALLOW_DESTRUCTIVE_MIGRATIONS", item.name)
                        case decisionOverride:
                                log.Printf("[MIGRATIONS] %s: destructive guard passed via ALLOW_DESTRUCTIVE_MIGRATIONS override", item.name)
                        default:
                                return blockErr
                        }

                        // AUD-005 — best-effort logical snapshot before the
                        // destructive statements touch anything (skipped on a
                        // fresh install: there is nothing to protect).
                        if !freshInstall {
                                path, attempted, snapErr := snapshotBeforeDestructive(ctx, db.Config().ConnConfig.ConnString(), item.name)
                                switch {
                                case !attempted:
                                        log.Printf("[MIGRATIONS] %s: no pg_dump client available (or snapshots disabled) — proceeding without pre-migration snapshot; the operator gate remains the protection", item.name)
                                case snapErr != nil:
                                        if snapshotProductionStrict(appEnv) {
                                                return fmt.Errorf("pre-migration snapshot failed for %s: %w (fix the snapshot environment — MIGRATION_SNAPSHOT_DIR/PGDUMP_PATH — or set MIGRATION_SNAPSHOT_DIR=off to acknowledge running without a snapshot)", item.name, snapErr)
                                        }
                                        log.Printf("[MIGRATIONS] %s: pre-migration snapshot failed (non-fatal in %s): %v", item.name, appEnv, snapErr)
                                default:
                                        log.Printf("[MIGRATIONS] %s: pre-migration snapshot saved to %s", item.name, path)
                                }
                        }
                }

                tx, err := conn.Begin(ctx)
                if err != nil {
                        return fmt.Errorf("begin migration %s: %w", item.name, err)
                }
                if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
                        _ = tx.Rollback(ctx)
                        return fmt.Errorf("execute migration %s: %w", item.name, err)
                }
                // AUD T-C1 — the version row commits in the SAME transaction as
                // the DDL. The old shape (record after Commit) had a crash window:
                // an applied-but-unrecorded migration re-ran on the next boot and
                // boot-looped on its first already-applied statement. In
                // PostgreSQL, DDL is transactional, so both sides roll back or
                // neither does.
                if err := recordMigration(ctx, tx, item.name); err != nil {
                        _ = tx.Rollback(ctx)
                        return fmt.Errorf("record migration %s: %w", item.name, err)
                }
                if err := tx.Commit(ctx); err != nil {
                        return fmt.Errorf("commit migration %s: %w", item.name, err)
                }
                log.Printf("[MIGRATIONS] Applied migration %s", item.name)
        }

        warnPlanPermissionDrift(ctx, conn)
        return nil
}

// warnPlanPermissionDrift is the permanent lesson of the seed-drift bug
// (migration 26 seeded plan_permissions for free/starter from an empty
// plan_features): a plan whose advertised features are missing their
// enforcement permissions produces exactly the confusing ticket of an
// ACTIVE subscription whose every module API answers
// plan_permission_denied. Detection is automatic and loud; correction is
// NEVER automatic — enforcement data changes go through audited
// migrations or the super admin's plan editor, never through a startup
// side-effect.
func warnPlanPermissionDrift(ctx context.Context, conn *pgxpool.Conn) {
        rows, err := conn.Query(ctx, `
                SELECT pl.slug,
                       array_agg(DISTINCT p2.key ORDER BY p2.key) AS missing
                FROM plans pl
                JOIN plan_features pf ON pf.plan_id = pl.id
                JOIN feature_permissions fp ON fp.feature_key = pf.feature_key
                JOIN permissions p2 ON p2.id = fp.permission_id
                WHERE NOT EXISTS (
                        SELECT 1 FROM plan_permissions pp
                        WHERE pp.plan_id = pl.id
                          AND pp.permission_id = p2.id)
                GROUP BY pl.slug`)
        if err != nil {
                log.Printf("[PLANS] consistency check failed (non-fatal): %v", err)
                return
        }
        defer rows.Close()
        for rows.Next() {
                var slug string
                var missing []string
                if err := rows.Scan(&slug, &missing); err != nil {
                        log.Printf("[PLANS] consistency scan failed (non-fatal): %v", err)
                        return
                }
                log.Printf("[PLANS] WARNING: plan %q advertises features whose permissions are missing (%d): %v — subscribers will see plan_permission_denied; repair via a migration or the plan editor", slug, len(missing), missing)
        }
}

func migrationApplied(ctx context.Context, conn *pgxpool.Conn, name string) (bool, error) {
        var applied bool
        err := conn.QueryRow(ctx,
                `SELECT EXISTS (SELECT 1 FROM public.schema_migrations WHERE version = $1)`,
                name,
        ).Scan(&applied)
        return applied, err
}

// migrationExecutor abstracts the two surfaces recordMigration writes
// through: the pool connection (recording legacy pre-existing schema) and
// the migration transaction (the AUD T-C1 atomic path). *pgxpool.Conn and
// pgx.Tx both satisfy it.
type migrationExecutor interface {
        Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

func recordMigration(ctx context.Context, conn migrationExecutor, name string) error {
        _, err := conn.Exec(ctx, `
                INSERT INTO public.schema_migrations (version)
                VALUES ($1)
                ON CONFLICT (version) DO NOTHING
        `, name)
        return err
}

// databaseIsFresh reports whether the public schema holds no tables other
// than the migration ledger. A fresh database runs the whole chain
// unattended — there is no data a destructive statement could destroy —
// while any pre-existing table (legacy schema, previous install) keeps the
// AUD-007 gate fully active.
func databaseIsFresh(ctx context.Context, conn *pgxpool.Conn) (bool, error) {
        var n int
        err := conn.QueryRow(ctx, `
                SELECT count(*)
                FROM information_schema.tables
                WHERE table_schema = 'public'
                  AND table_name <> 'schema_migrations'
        `).Scan(&n)
        return n == 0, err
}

func tablesExist(ctx context.Context, conn *pgxpool.Conn, tables []string) (bool, error) {
        for _, table := range tables {
                var exists bool
                if err := conn.QueryRow(ctx,
                        `SELECT to_regclass($1) IS NOT NULL`,
                        "public."+table,
                ).Scan(&exists); err != nil {
                        return false, err
                }
                if !exists {
                        return false, nil
                }
        }
        return true, nil
}
