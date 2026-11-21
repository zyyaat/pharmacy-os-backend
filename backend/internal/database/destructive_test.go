package database

import (
        "strings"
        "testing"

        "github.com/pharmacy-os/backend/migrations"
)

func TestScanDestructiveStatements_Table(t *testing.T) {
        cases := []struct {
                name    string
                sql     string
                wantN   int
                wantSub string // expected Kind substring of the first finding; "" when none
        }{
                {
                        name:    "drop table always flagged even with IF EXISTS",
                        sql:     "DROP TABLE IF EXISTS legacy_rows;\nSELECT 1;",
                        wantN:   1,
                        wantSub: "DROP TABLE",
                },
                {
                        name:    "drop column via ALTER TABLE",
                        sql:     "ALTER TABLE inventory_batches DROP COLUMN total_cost;",
                        wantN:   1,
                        wantSub: "DROP COLUMN",
                },
                {
                        name:    "drop column with IF EXISTS still flagged",
                        sql:     "ALTER TABLE t DROP COLUMN IF EXISTS c;",
                        wantN:   1,
                        wantSub: "DROP COLUMN",
                },
                {
                        name:    "truncate table",
                        sql:     "TRUNCATE TABLE staging_imports;",
                        wantN:   1,
                        wantSub: "TRUNCATE",
                },
                {
                        name:    "truncate without TABLE keyword",
                        sql:     "TRUNCATE staging_imports;",
                        wantN:   1,
                        wantSub: "TRUNCATE",
                },
                {
                        name:    "delete from full wipe",
                        sql:     "DELETE FROM role_permissions;",
                        wantN:   1,
                        wantSub: "DELETE FROM",
                },
                {
                        name:    "delete from scoped still data removal",
                        sql:     "DELETE FROM role_permissions WHERE role_id = 7;",
                        wantN:   1,
                        wantSub: "DELETE FROM",
                },
                {
                        name:    "alter column type",
                        sql:     "ALTER TABLE companies ALTER COLUMN plan TYPE VARCHAR(50) USING plan::text;",
                        wantN:   1,
                        wantSub: "ALTER COLUMN",
                },
                {
                        name:    "alter column set data type verbose form",
                        sql:     "ALTER TABLE t ALTER COLUMN x SET DATA TYPE bigint;",
                        wantN:   1,
                        wantSub: "ALTER COLUMN",
                },
                {
                        name:    "dynamic plpgsql format string caught",
                        sql:     "fmt := 'ALTER TABLE %I ALTER COLUMN %I TYPE BIGINT USING ROUND(%I * 100)::bigint';",
                        wantN:   1,
                        wantSub: "ALTER COLUMN",
                },
                {
                        name:    "lowercase keywords",
                        sql:     "drop table old_backup;\ndelete from cache_rows;",
                        wantN:   2,
                        wantSub: "DROP TABLE",
                },
                {
                        name:    "unguarded conditional drop flagged",
                        sql:     "DROP INDEX idx_legacy_search;",
                        wantN:   1,
                        wantSub: "DROP INDEX",
                },
                {
                        name:    "guarded housekeeping drops ignored",
                        sql: "DROP TRIGGER IF EXISTS trg_support_tickets_updated_at ON support_tickets;\n" +
                                "ALTER TABLE payment_transactions DROP CONSTRAINT IF EXISTS txn_type_check;\n" +
                                "DROP INDEX IF EXISTS idx_old;\nDROP POLICY IF EXISTS p ON t;\n" +
                                "DROP VIEW IF EXISTS v;\nDROP FUNCTION IF EXISTS f();\n" +
                                "DROP CONSTRAINT IF EXISTS c;\nDROP SCHEMA IF EXISTS s;\n" +
                                "DROP TYPE IF EXISTS ty;\nDROP SEQUENCE IF EXISTS seq;\n" +
                                "DROP MATERIALIZED VIEW IF EXISTS mv;\nDROP PROCEDURE IF EXISTS pr();\n" +
                                "DROP DATABASE IF EXISTS db;",
                        wantN:   0,
                        wantSub: "",
                },
                {
                        name:    "benign ddl untouched",
                        sql:     "CREATE TABLE new_things (id uuid PRIMARY KEY);\nCREATE INDEX idx_new ON new_things (id);\nINSERT INTO new_things SELECT * FROM staging;\nALTER TABLE t ADD COLUMN flag boolean DEFAULT false;\nUPDATE t SET flag = true WHERE id = $1;\nALTER TABLE t ALTER COLUMN flag SET DEFAULT false;",
                        wantN:   0,
                        wantSub: "",
                },
                {
                        name:    "comments never trip the gate",
                        sql:     "-- DROP TABLE mentioned_in_comment;\n/* DROP TABLE block_comment; */\nSELECT '-- DELETE FROM not real --' AS label;",
                        wantN:   0,
                        wantSub: "",
                },
                {
                        name:    "line comment after statement stripped",
                        sql:     "DELETE FROM rows_cache; -- rebuild below",
                        wantN:   1,
                        wantSub: "DELETE FROM",
                },
        }

        for _, tc := range cases {
                t.Run(tc.name, func(t *testing.T) {
                        got := scanDestructiveStatements(tc.sql)
                        if len(got) != tc.wantN {
                                t.Fatalf("findings = %d (%v), want %d", len(got), got, tc.wantN)
                        }
                        if tc.wantN > 0 && !strings.Contains(got[0].Kind, tc.wantSub) {
                                t.Fatalf("first kind = %q, want substring %q", got[0].Kind, tc.wantSub)
                        }
                })
        }
}

func TestScanDestructiveStatements_LineNumbersTrue(t *testing.T) {
        sql := "-- header comment\nSELECT 1;\n\nDELETE FROM historical_rows;"
        findings := scanDestructiveStatements(sql)
        if len(findings) != 1 {
                t.Fatalf("findings = %d, want 1", len(findings))
        }
        if findings[0].Line != 4 {
                t.Fatalf("line = %d, want 4 (block comments keep line structure true)", findings[0].Line)
        }
}

// TestScanRealMigrations_Backtest pins the scanner against the real,
// frozen migration files: the historically destructive ones must stay
// detected and the clean ones must stay silent — a scanner regression in
// either direction is a failed build, not a production surprise.
func TestScanRealMigrations_Backtest(t *testing.T) {
        read := func(t *testing.T, name string) string {
                t.Helper()
                b, err := migrations.FS.ReadFile(name)
                if err != nil {
                        t.Fatalf("read %s: %v", name, err)
                }
                return string(b)
        }

        t.Run("migration 14 money_piastres is destructive", func(t *testing.T) {
                findings := scanDestructiveStatements(read(t, "00000000000014_money_piastres.sql"))
                if len(findings) < 2 {
                        t.Fatalf("findings = %d, want >= 2 (DROP COLUMN total_cost + ALTER COLUMN TYPE rewrites)", len(findings))
                }
                var kinds []string
                for _, f := range findings {
                        kinds = append(kinds, f.Kind)
                }
                joined := strings.Join(kinds, ",")
                if !strings.Contains(joined, "DROP COLUMN") || !strings.Contains(joined, "ALTER COLUMN") {
                        t.Fatalf("kinds = %q, want DROP COLUMN and ALTER COLUMN families", joined)
                }
        })

        t.Run("migration 20 flexible_permissions wipes a table", func(t *testing.T) {
                findings := scanDestructiveStatements(read(t, "00000000000020_flexible_permissions.sql"))
                if len(findings) != 1 || findings[0].Kind != "DELETE FROM" {
                        t.Fatalf("findings = %+v, want exactly one DELETE FROM (role_permissions rebuild)", findings)
                }
        })

        t.Run("migration 26 saas_plans retypes a column", func(t *testing.T) {
                findings := scanDestructiveStatements(read(t, "00000000000026_saas_plans.sql"))
                if len(findings) != 1 || findings[0].Kind != "ALTER COLUMN ... TYPE" {
                        t.Fatalf("findings = %+v, want exactly one ALTER COLUMN ... TYPE (companies.plan)", findings)
                }
        })

        for _, name := range []string{
                "00000000000019_sales_discount_customers.sql",
                "00000000000027_billing_hardening.sql",
                "00000000000028_saas_plan_seed_repair.sql",
                "00000000000029_company_entitlements.sql",
                "00000000000030_platform_audit_logs.sql",
                "00000000000031_product_libraries.sql",
                "00000000000032_payment_settlements.sql",
                "00000000000033_support_system.sql",
                "00000000000034_email_verify_attempts.sql",
                "00000000000036_ledger_fk_preservation.sql",
                "00000000000037_payment_txn_dedup.sql",
                "00000000000038_email_send_budget.sql",
        } {
                t.Run("clean migration "+name, func(t *testing.T) {
                        if findings := scanDestructiveStatements(read(t, name)); len(findings) != 0 {
                                t.Fatalf("clean migration flagged: %+v — the scanner regressed", findings)
                        }
                })
        }
}

func TestDestructiveGate_DecisionTable(t *testing.T) {
        findings := []destructiveStatement{{Line: 3, Kind: "DROP TABLE", Snippet: "DROP TABLE x;"}}

        cases := []struct {
                name         string
                freshInstall bool
                appEnv       string
                allowRaw     string
                want         string
        }{
                {"fresh install always proceeds", true, "production", "", decisionFreshInstall},
                {"fresh install even with no env", true, "", "", decisionFreshInstall},
                {"production blocks by default", false, "production", "", decisionBlock},
                {"staging blocks by default", false, "staging", "", decisionBlock},
                {"production override by name", false, "production", "00000000000035_example.sql", decisionOverride},
                {"production override by star", false, "production", "*", decisionOverride},
                {"production override by list membership", false, "production", " 00000000000034_email_verify_attempts.sql , 00000000000035_example.sql ", decisionOverride},
                {"production name mismatch still blocks", false, "production", "00000000000036_other.sql", decisionBlock},
                {"development warns but proceeds", false, "development", "", decisionDevelopment},
                {"unset APP_ENV behaves as development", false, "", "", decisionDevelopment},
        }

        for _, tc := range cases {
                t.Run(tc.name, func(t *testing.T) {
                        gate := destructiveGate{
                                Migration:    "00000000000035_example.sql",
                                Findings:     findings,
                                FreshInstall: tc.freshInstall,
                                AppEnv:       tc.appEnv,
                                AllowRaw:     tc.allowRaw,
                        }
                        got, err := gate.decision()
                        if got != tc.want {
                                t.Fatalf("decision = %q (err=%v), want %q", got, err, tc.want)
                        }
                        if tc.want == decisionBlock && err == nil {
                                t.Fatalf("block decision must carry a remediation error, got nil")
                        }
                })
        }
}

func TestDestructiveGate_BlockErrorGuidesOperator(t *testing.T) {
        gate := destructiveGate{
                Migration: "00000000000035_example.sql",
                Findings: []destructiveStatement{
                        {Line: 12, Kind: "DROP TABLE", Snippet: "DROP TABLE old_ledger;"},
                },
                AppEnv: "production",
        }
        _, err := gate.decision()
        if err == nil {
                t.Fatal("expected block error")
        }
        for _, fragment := range []string{"00000000000035_example.sql", "ALLOW_DESTRUCTIVE_MIGRATIONS", "line 12: DROP TABLE", "snapshot"} {
                if !strings.Contains(err.Error(), fragment) {
                        t.Fatalf("block error missing %q: %v", fragment, err)
                }
        }
}

func TestSummarizeFindings_Capped(t *testing.T) {
        many := make([]destructiveStatement, 0, 10)
        for i := 0; i < 10; i++ {
                many = append(many, destructiveStatement{Line: i + 1, Kind: "DROP TABLE"})
        }
        got := summarizeFindings(many)
        if !strings.Contains(got, "+4 more") {
                t.Fatalf("summary = %q, want cap note (+4 more)", got)
        }
        if strings.Count(got, "line") != 6 {
                t.Fatalf("summary = %q, want exactly 6 shown findings", got)
        }
}
