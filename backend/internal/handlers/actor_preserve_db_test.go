// P2 regression gate for the actor-preservation re-aim (api_level 102,
// migration 44). Migration 13 pinned sales.company_user_id and
// stock_movements.created_by_company_user_id on ON DELETE CASCADE, so a
// hard company delete (soft delete exists; hard delete is possible) erased
// historical sales and movement rows one hop over — the ledger loss
// migration 36 exists to prevent, through the one door 36's checklist did
// not cover. Migration 44 re-aims both FKs to SET NULL and moves the
// write-time actor invariant from migration 12's CHECK constraints (which
// would trip on the referential action's own UPDATE and abort company
// deletion with 23514) to BEFORE triggers that enforce it only for
// application-initiated writes: pg_trigger_depth() < 2 is the documented
// exemption for RI actions (an RI action's row trigger runs at depth >= 2).
//
// This suite pins, against a real schema chain:
//   - schema: both actor FKs are named *_preserve_fkey with confdeltype
//     'n' (SET NULL), both depth-aware triggers exist, and NO ledger FK on
//     the guarded tables/columns is CASCADE anymore (36's checklist,
//     extended with the actor columns — the migration's own gate, re-read
//     here from the outside),
//   - write-time invariant unchanged for applications: INSERT with both
//     actor columns NULL fails with 23514 (same code the old CHECK raised),
//     and so does an application UPDATE of the actor columns to both-NULL,
//   - THE HOLE, now closed: DELETE of the acting company_user succeeds
//     (no 23514 from the actor CHECKs), the sale and the movement rows
//     SURVIVE with the actor column detached to NULL,
//   - detached rows stay updatable on non-actor columns (the trigger
//     scopes to UPDATE OF the actor columns).
//
// Same harness as the other DB gates:
//
//	TEST_DATABASE_URL=postgres://.../pharmacy_scratch go test ./internal/handlers/ -run TestActorPreserveDB
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

func actorPreservePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — actor preservation gates need a disposable PostgreSQL")
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

func requireCheckViolation(t *testing.T, err error, what string) {
	t.Helper()
	require.Error(t, err, "%s: expected the actor invariant to reject this write", what)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "%s: expected a PostgreSQL error", what)
	require.Equal(t, "23514", pgErr.Code,
		"%s: the old CHECK raised check_violation and the trigger must keep that contract", what)
}

func TestActorPreserveDB(t *testing.T) {
	ctx := context.Background()
	pool := actorPreservePool(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	// ---- schema state: the gate the migration runs, re-read from outside --
	var badCascade, notSetNull, trigCount int
	require.NoError(t, pool.QueryRow(ctx, `
                SELECT count(*) FROM pg_constraint con
                JOIN pg_attribute a
                  ON a.attrelid = con.conrelid AND a.attnum = ANY (con.conkey)
                WHERE con.contype = 'f' AND con.confdeltype = 'c'
                  AND con.conrelid IN ('sales'::regclass, 'sale_returns'::regclass,
                                       'stock_movements'::regclass, 'payments'::regclass,
                                       'audit_logs'::regclass)
                  AND a.attname IN ('pharmacy_id', 'batch_id', 'company_id', 'account_id',
                                    'company_user_id', 'created_by_company_user_id')
        `).Scan(&badCascade))
	require.Zero(t, badCascade, "a ledger FK on the guarded columns is still ON DELETE CASCADE")

	require.NoError(t, pool.QueryRow(ctx, `
                SELECT count(*) FROM pg_constraint
                WHERE conname IN ('sales_company_user_id_preserve_fkey',
                                  'stock_movements_created_by_company_user_id_preserve_fkey')
                  AND confdeltype <> 'n'
        `).Scan(&notSetNull))
	require.Zero(t, notSetNull, "actor FKs must be exactly ON DELETE SET NULL")

	require.NoError(t, pool.QueryRow(ctx, `
                SELECT count(*) FROM pg_trigger
                WHERE tgname IN ('sales_actor_required_trigger',
                                 'stock_movements_actor_required_trigger') AND NOT tgisinternal
        `).Scan(&trigCount))
	require.Equal(t, 2, trigCount, "both depth-aware actor triggers must exist")

	// ---- fixtures --------------------------------------------------------
	var accountID, companyID, pharmacyID, branchID string
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO accounts (company_name, contact_email, status)
                VALUES ($1, $2, 'active') RETURNING id::text
        `, "p2-gate "+suffix, "p2gate-"+suffix+"@test.local").Scan(&accountID))
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, accountID) })
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO companies (name, email) VALUES ($1, $2) RETURNING id::text
        `, "p2-gate-co "+suffix, "p2gate-co-"+suffix+"@test.local").Scan(&companyID))
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM companies WHERE id = $1`, companyID) })
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacies (account_id, name, license_number, is_active)
                VALUES ($1, $2, $3, true) RETURNING id::text
        `, accountID, "p2-gate-ph "+suffix, "P2-"+suffix).Scan(&pharmacyID))
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO branches (pharmacy_id, name, code, is_active)
                VALUES ($1, 'main', $2, true) RETURNING id::text
        `, pharmacyID, "P2B-"+suffix).Scan(&branchID))

	var companyUserID string
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO company_users (company_id, email, password_hash, first_name, last_name)
                VALUES ($1, $2, 'x', 'p2', 'gate') RETURNING id::text
        `, companyID, "p2gate-user-"+suffix+"@test.local").Scan(&companyUserID))

	var globalP string
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO global_products (name, generic_name, dosage_form, strength, product_category, default_unit, is_active)
                VALUES ($1, 'gate', 'tablet', '1mg', 'medication', 'tablet', true) RETURNING id::text
        `, "P2-GATE-P-"+suffix).Scan(&globalP))
	var productP string
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacy_products (pharmacy_id, global_product_id, cost_price, selling_price, min_stock_level, is_active)
                VALUES ($1, $2, 1000, 1500, 0, true) RETURNING id::text
        `, pharmacyID, globalP).Scan(&productP))
	var batchID string
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO inventory_batches (pharmacy_product_id, branch_id, batch_number, quantity, unit, cost_per_unit, expiry_date, received_date)
                VALUES ($1, $2, 'P2-B1', 10, 'box', 1000, '2028-01-01'::date, CURRENT_DATE) RETURNING id::text
        `, productP, branchID).Scan(&batchID))

	// One sale rung up by the COMPANY USER only (employee_id NULL) — the
	// exact row shape migration 13's CASCADE would have erased.
	var saleID string
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO sales (pharmacy_id, branch_id, idempotency_key, company_user_id, payment_type)
                VALUES ($1, $2, $3, $4, 'cash') RETURNING id::text
        `, pharmacyID, branchID, "p2-gate-sale-"+suffix, companyUserID).Scan(&saleID))
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM sales WHERE id = $1`, saleID) })

	// One movement made by the COMPANY USER only (created_by NULL).
	var movementID string
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO stock_movements (batch_id, movement_type, quantity, unit,
                                             quantity_before, quantity_after, created_by_company_user_id)
                VALUES ($1, 'purchase', 5, 'box', 0, 5, $2) RETURNING id::text
        `, batchID, companyUserID).Scan(&movementID))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM stock_movements WHERE id = $1`, movementID)
	})

	// ---- write-time invariant holds for applications ---------------------
	_, err := pool.Exec(ctx, `
                INSERT INTO sales (pharmacy_id, branch_id, idempotency_key, payment_type)
                VALUES ($1, $2, $3, 'cash')`, pharmacyID, branchID, "p2-gate-orphan-"+suffix)
	requireCheckViolation(t, err, "sale INSERT with no actor")

	_, err = pool.Exec(ctx, `
                INSERT INTO stock_movements (batch_id, movement_type, quantity, unit,
                                             quantity_before, quantity_after)
                VALUES ($1, 'purchase', 1, 'box', 0, 1)`, batchID)
	requireCheckViolation(t, err, "movement INSERT with no actor")

	_, err = pool.Exec(ctx, `UPDATE sales SET company_user_id = NULL WHERE id = $1`, saleID)
	requireCheckViolation(t, err, "application UPDATE clearing the only actor")

	// ---- THE HOLE: delete the actor, the ledger must survive -------------
	cmd, err := pool.Exec(ctx, `DELETE FROM company_users WHERE id = $1`, companyUserID)
	require.NoError(t, err,
		"deleting a company user must not trip the actor invariant (RI action runs at trigger depth >= 2)")
	require.EqualValues(t, 1, cmd.RowsAffected())

	var saleActor, salePharmacy *string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT company_user_id::text, pharmacy_id::text FROM sales WHERE id = $1`, saleID).
		Scan(&saleActor, &salePharmacy))
	require.Nil(t, saleActor, "sale must survive with its actor detached")
	require.NotNil(t, salePharmacy, "the sale row itself must still be attached to its pharmacy")

	var mvActor *string
	var mvQty float64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT created_by_company_user_id::text, quantity::float8 FROM stock_movements WHERE id = $1`, movementID).
		Scan(&mvActor, &mvQty))
	require.Nil(t, mvActor, "movement must survive with its actor detached")
	require.Equal(t, 5.0, mvQty, "movement quantity must be untouched by the detachment")

	// ---- detached rows stay updatable on non-actor columns ---------------
	cmd, err = pool.Exec(ctx,
		`UPDATE sales SET total_amount = 123 WHERE id = $1`, saleID)
	require.NoError(t, err, "non-actor updates must not be gated by the trigger")
	require.EqualValues(t, 1, cmd.RowsAffected())
}
