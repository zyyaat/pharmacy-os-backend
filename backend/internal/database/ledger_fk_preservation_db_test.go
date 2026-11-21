package database

// Regression gate for migration 36 (AUD-007, P0): the ledger/history tables
// must never again hang on the organizational tree with ON DELETE CASCADE.
// The audit's acceptance criterion is behavioral — "deleting a company
// preserves sales + stock_movements" — and that behavior is owned entirely
// by the FK delete actions, so this test pins the schema contract itself:
// every ledger FK from migration 36 must read ON DELETE SET NULL with a
// nullable column, and no CASCADE may survive on those columns.
//
// Needs a real PostgreSQL (it runs the full migration chain). Point
// TEST_DATABASE_URL at a DISPOSABLE database — the migrator applies every
// migration to it:
//
//      TEST_DATABASE_URL=postgres://.../pharmacy_scratch go test ./internal/database/ -run TestLedgerFKs -v

import (
        "context"
        "testing"
        "time"

        "github.com/jackc/pgx/v5/pgxpool"
)

func TestLedgerFKsPreserveHistory(t *testing.T) {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
        defer cancel()

        pool, err := pgxpool.New(ctx, integrationDSN(t))
        if err != nil {
                t.Fatalf("connect: %v", err)
        }
        defer pool.Close()

        if err := RunMigrations(ctx, pool); err != nil {
                t.Fatalf("apply migrations: %v", err)
        }

        // (table, column, referenced table) — the six ledger FKs of migration 36.
        plan := [][3]string{
                {"sales", "pharmacy_id", "pharmacies"},
                {"sale_returns", "pharmacy_id", "pharmacies"},
                {"stock_movements", "batch_id", "inventory_batches"},
                {"payments", "company_id", "companies"},
                {"audit_logs", "pharmacy_id", "pharmacies"},
                {"audit_logs", "account_id", "accounts"},
        }

        for _, p := range plan {
                tbl, col, ref := p[0], p[1], p[2]

                var deltype, conname, confrelid string
                var notnull bool
                err := pool.QueryRow(ctx, `
                        SELECT con.confdeltype::text, con.conname, confrel.relname, a.attnotnull
                        FROM pg_constraint con
                        JOIN pg_attribute a
                          ON a.attrelid = con.conrelid AND a.attnum = ANY (con.conkey)
                        JOIN pg_class confrel ON confrel.oid = con.confrelid
                        WHERE con.conrelid = $1::regclass
                          AND con.contype  = 'f'
                          AND a.attname    = $2
                        LIMIT 1`,
                        "public."+tbl, col).Scan(&deltype, &conname, &confrelid, &notnull)
                if err != nil {
                        t.Fatalf("%s.%s: expected an FK constraint after migration 36, found none: %v", tbl, col, err)
                }
                if confrelid != ref {
                        t.Errorf("%s.%s: FK references %s, want %s", tbl, col, confrelid, ref)
                }
                if deltype != "n" { // 'n' = SET NULL; 'c' = CASCADE — the AUD-007 killer
                        t.Errorf("%s.%s: FK delete action is %q — ledger history would be ERASED on parent delete (AUD-007 regression)", tbl, col, deltype)
                }
                if notnull {
                        t.Errorf("%s.%s: column is NOT NULL — SET NULL cannot detach history rows on parent delete", tbl, col)
                }
        }

        // Guard the guard: the self-verification inside migration 36 must have
        // left zero CASCADE FKs on any of the ledger columns.
        var cascades int
        if err := pool.QueryRow(ctx, `
                SELECT count(*)
                FROM pg_constraint con
                JOIN pg_attribute a
                  ON a.attrelid = con.conrelid AND a.attnum = ANY (con.conkey)
                WHERE con.contype = 'f'
                  AND con.confdeltype = 'c'
                  AND con.conrelid IN ('sales'::regclass, 'sale_returns'::regclass,
                                       'stock_movements'::regclass, 'payments'::regclass,
                                       'audit_logs'::regclass)
                  AND a.attname IN ('pharmacy_id', 'batch_id', 'company_id', 'account_id')`).Scan(&cascades); err != nil {
                t.Fatalf("count cascades: %v", err)
        }
        if cascades != 0 {
                t.Fatalf("%d CASCADE FK(s) alive on ledger columns — AUD-007 is open again", cascades)
        }
}
