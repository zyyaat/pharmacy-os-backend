// Task 164 regression gate for the platform-company isolation (api_level
// 103, migration 45). The bootstrap (auth/bootstrap.go) shapes the company
// hosting the platform Super Admin as a full SaaS tenant — company, account,
// subscription, pharmacy, branch — so every platform list and stat rendered
// it as "a customer among customers", and the suspend action copies
// subscription status onto companies.status: the operator could suspend his
// own tenant by accident. Migration 45 adds companies.is_platform, marks the
// shell on existing databases (via the singleton super_admin company_user —
// migration 10's unique partial index allows exactly one such user in the
// whole database), and freezes it with a BEFORE UPDATE/DELETE trigger.
//
// This suite pins, against a real schema chain:
//   - schema: the is_platform column is NOT NULL DEFAULT false and the
//     protect_platform_company trigger exists (the migration's own contract,
//     re-read from the outside),
//   - THE MARK: re-running the migration's adoption predicate after planting
//     a live super_admin company_user marks exactly that company — the path
//     for databases bootstrapped before migration 45 (bootstrap.go replays
//     the same UPDATE on adoption),
//   - THE FREEZE on a marked company, each refused with 23514
//     check_violation even for raw SQL: status flip (the suspend hole),
//     unmarking, soft delete, hard delete,
//   - NOT OVER-FROZEN: profile edits (name) and same-value writes stay legal
//     on the marked company, and marking false→true (the bootstrap adoption
//     UPDATE) stays legal on an unmarked one,
//   - NO REGRESSION: a customer company keeps its normal lifecycle — status
//     changes, soft delete AND hard delete all succeed (the DELETE leg pins
//     the BEFORE-trigger return contract: a BEFORE DELETE trigger returning
//     NEW returns NULL and would silently skip every customer delete).
//
// Note on scratch-database hygiene: a marked company can never be deleted
// (that is the freeze), so this gate leaves its marked fixtures behind —
// they are harmless (excluded from nothing they did not already break, and
// the adoption predicate ignores already-marked rows). Stale super_admin
// users from a crashed prior run ARE removed up front: the unique partial
// index admits exactly one live super_admin, and a leftover one would fail
// this run's fixture INSERT.
//
// Same harness as the other DB gates:
//
//	TEST_DATABASE_URL=postgres://.../pharmacy_scratch go test ./internal/handlers/ -run TestPlatformCompanyIsolationDB
package handlers

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func platformIsolationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — platform isolation gate needs a disposable PostgreSQL")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func requirePlatformCheckViolation(t *testing.T, err error, what string) {
	t.Helper()
	require.Error(t, err, "%s: expected the platform-company freeze to reject this write", what)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "%s: expected a PostgreSQL error", what)
	require.Equal(t, "23514", pgErr.Code,
		"%s: the trigger raises check_violation and that contract is load-bearing", what)
}

func TestPlatformCompanyIsolationDB(t *testing.T) {
	ctx := context.Background()
	pool := platformIsolationPool(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	// Crashed prior runs would hold the unique super_admin slot — clear the
	// family before claiming it (marked companies cannot be deleted and are
	// tolerated, see the hygiene note above).
	_, _ = pool.Exec(ctx, `DELETE FROM company_users WHERE email LIKE 't163-super-%'`)

	// ---- schema state: the gate the migration runs, re-read from outside --
	var notNull, hasDefault, triggerCount int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE is_nullable = 'NO'),
		       COUNT(*) FILTER (WHERE column_default LIKE '%false%')
		FROM information_schema.columns
		WHERE table_name = 'companies' AND column_name = 'is_platform'
	`).Scan(&notNull, &hasDefault))
	require.Equal(t, 1, notNull, "companies.is_platform must be NOT NULL")
	require.Equal(t, 1, hasDefault, "companies.is_platform must DEFAULT false — every pre-existing customer is a plain customer")

	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_trigger
		WHERE tgname = 'protect_platform_company' AND NOT tgisinternal
	`).Scan(&triggerCount))
	require.Equal(t, 1, triggerCount, "the protect_platform_company trigger must exist")

	// ---- fixtures --------------------------------------------------------
	// platformCo: the shell as bootstrap.go creates it on a fresh database —
	// marked from birth. Frozen for the whole test, so cleanup tolerates a
	// leftover row.
	var platformCoID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO companies (name, email, status, plan, is_platform)
		VALUES ($1, $2, 'active', 'enterprise', true) RETURNING id::text
	`, "t163-platform-shell "+suffix, "t163-shell-"+suffix+"@test.local").Scan(&platformCoID))

	// adoptedCo: a plain customer company (a pre-45 database's shape) that
	// the adoption predicate will mark below.
	var adoptedCoID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO companies (name, email) VALUES ($1, $2) RETURNING id::text
	`, "t163-adopted "+suffix, "t163-adopted-"+suffix+"@test.local").Scan(&adoptedCoID))

	// adoptableCo: stays unmarked until the false→true legality check.
	var adoptableCoID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO companies (name, email) VALUES ($1, $2) RETURNING id::text
	`, "t163-adoptable "+suffix, "t163-adoptable-"+suffix+"@test.local").Scan(&adoptableCoID))

	// customerCo: an untouched customer for the no-regression lifecycle.
	var customerCoID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO companies (name, email) VALUES ($1, $2) RETURNING id::text
	`, "t163-customer "+suffix, "t163-customer-"+suffix+"@test.local").Scan(&customerCoID))

	t.Cleanup(func() {
		bg := context.Background()
		// Unmarked fixtures delete normally; marked ones cannot (the freeze)
		// and are tolerated as leftovers. Children first (FK order).
		_, _ = pool.Exec(bg, `DELETE FROM company_users WHERE email LIKE 't163-super-%'`)
		for _, id := range []string{customerCoID, adoptableCoID, adoptedCoID, platformCoID} {
			_, _ = pool.Exec(bg, `DELETE FROM companies WHERE id = $1`, id)
		}
	})

	// ---- THE MARK: the migration's adoption predicate, replayed ----------
	// Migration 10's unique partial index admits exactly ONE live
	// role='super_admin' company_user in the whole database — the shell is
	// identified by hosting it. Plant that user in the plain customer
	// company and replay the predicate: exactly that company must gain the
	// mark (bootstrap.go's adoption UPDATE is the same statement).
	var superUserID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO company_users (company_id, email, password_hash, first_name, last_name, role)
		VALUES ($1, $2, 'x', 't163', 'gate', 'super_admin') RETURNING id::text
	`, adoptedCoID, "t163-super-"+suffix+"@test.local").Scan(&superUserID))

	cmd, err := pool.Exec(ctx, `
		UPDATE companies c
		SET is_platform = true
		WHERE NOT c.is_platform
		  AND EXISTS (
				SELECT 1 FROM company_users cu
				WHERE cu.company_id = c.id
				  AND cu.role = 'super_admin'
				  AND cu.deleted_at IS NULL
		  )
	`)
	require.NoError(t, err, "marking an unmarked company must stay legal")
	require.EqualValues(t, 1, cmd.RowsAffected(),
		"exactly the company hosting the singleton super_admin may gain the mark")

	var marked bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT is_platform FROM companies WHERE id = $1`, adoptedCoID).Scan(&marked))
	require.True(t, marked, "the super-admin-hosting company must be adopted as the platform shell")

	// ---- THE FREEZE on a marked company ----------------------------------
	_, err = pool.Exec(ctx,
		`UPDATE companies SET status = 'suspended' WHERE id = $1`, platformCoID)
	requirePlatformCheckViolation(t, err, "status flip (the suspend hole)")

	_, err = pool.Exec(ctx,
		`UPDATE companies SET is_platform = false WHERE id = $1`, platformCoID)
	requirePlatformCheckViolation(t, err, "unmarking")

	_, err = pool.Exec(ctx,
		`UPDATE companies SET deleted_at = NOW() WHERE id = $1`, platformCoID)
	requirePlatformCheckViolation(t, err, "soft delete")

	_, err = pool.Exec(ctx, `DELETE FROM companies WHERE id = $1`, platformCoID)
	requirePlatformCheckViolation(t, err, "hard delete")

	// The adopted company is frozen by exactly the same guard.
	_, err = pool.Exec(ctx,
		`UPDATE companies SET status = 'suspended' WHERE id = $1`, adoptedCoID)
	requirePlatformCheckViolation(t, err, "the adopted shell is frozen like the born-marked one")

	// ---- NOT over-frozen -------------------------------------------------
	// Profile edits stay legal on the shell (migration 45's contract).
	cmd, err = pool.Exec(ctx,
		`UPDATE companies SET name = $2 WHERE id = $1`, platformCoID, "t163-renamed "+suffix)
	require.NoError(t, err, "profile edits (name, contact, branding) must stay legal on the shell")
	require.EqualValues(t, 1, cmd.RowsAffected())

	// Same-value writes are not "changes" — the trigger scopes to actual
	// transitions, so rewriting the status it already has succeeds.
	cmd, err = pool.Exec(ctx,
		`UPDATE companies SET status = 'active' WHERE id = $1`, platformCoID)
	require.NoError(t, err, "a same-value status write must not be treated as a flip")
	require.EqualValues(t, 1, cmd.RowsAffected())

	// The bootstrap adoption UPDATE (false→true) stays legal on an
	// unmarked company.
	cmd, err = pool.Exec(ctx,
		`UPDATE companies SET is_platform = true WHERE id = $1 AND is_platform = false`, adoptableCoID)
	require.NoError(t, err, "marking an unmarked company must stay legal (bootstrap adoption)")
	require.EqualValues(t, 1, cmd.RowsAffected())

	// ---- NO REGRESSION: customer lifecycle is untouched ------------------
	cmd, err = pool.Exec(ctx,
		`UPDATE companies SET status = 'suspended' WHERE id = $1`, customerCoID)
	require.NoError(t, err, "customer companies keep their normal status lifecycle")
	require.EqualValues(t, 1, cmd.RowsAffected())

	cmd, err = pool.Exec(ctx,
		`UPDATE companies SET deleted_at = NOW() WHERE id = $1`, customerCoID)
	require.NoError(t, err, "customer companies keep soft delete")
	require.EqualValues(t, 1, cmd.RowsAffected())

	// The DELETE leg is load-bearing beyond isolation: a BEFORE DELETE
	// trigger must return OLD — returning NEW (NULL on DELETE) silently
	// skips the row, and every customer hard delete would no-op with zero
	// errors and zero affected rows.
	cmd, err = pool.Exec(ctx, `DELETE FROM companies WHERE id = $1`, customerCoID)
	require.NoError(t, err, "customer companies keep hard delete (no children here)")
	require.EqualValues(t, 1, cmd.RowsAffected(),
		"the delete must actually remove the row — a silently-skipped delete is the BEFORE-trigger return-contract bug this leg pins")
}
