// P1 regression gate for the compressed POS write path (api_level 101).
//
// persistPOSItems replaced the per-line/per-take statements with a fixed
// handful of batched statements; this suite pins the SEMANTICS that must
// survive the compression, byte for byte:
//   - FEFO batch order (earliest expiry first, NULLS LAST),
//   - duplicate products across lines sharing one locked batch state,
//   - largest-remainder allocation summing EXACTLY to each line total,
//   - stock_movements quantity_before/after chains per batch,
//   - inventory_batches.quantity == the chain's final value,
//   - last_sold_at stamped for every sold product,
//   - insufficient stock -> services.ErrPOSSaleInsufficientStock (409 upstream),
//   - the multi-row VALUES statements behave under the SERVER's exec mode
//     (QueryExecModeExec — the Task-158 lesson; production never Describes
//     statements, so every parameter rides as TEXT chosen from the Go type).
//
// Same harness as the other DB gates:
//
//	TEST_DATABASE_URL=postgres://.../pharmacy_scratch go test ./internal/handlers/ -run TestPOSPersistDB
package handlers

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pharmacy-os/backend/internal/auth"
	"github.com/pharmacy-os/backend/internal/money"
	"github.com/pharmacy-os/backend/internal/services"
)

func posPersistPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — POS persist gates need a disposable PostgreSQL")
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

func TestPOSPersistDB(t *testing.T) {
	ctx := context.Background()
	pool := posPersistPool(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	// ---- fixtures --------------------------------------------------------
	var accountID, pharmacyID, branchID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO accounts (company_name, contact_email, status)
		VALUES ($1, $2, 'active') RETURNING id::text
	`, "p1-gate "+suffix, "p1gate-"+suffix+"@test.local").Scan(&accountID))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM accounts WHERE id = $1`, accountID) // ON DELETE CASCADE sweeps the rest
	})
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO pharmacies (account_id, name, license_number, is_active)
		VALUES ($1, $2, $3, true) RETURNING id::text
	`, accountID, "p1-gate-ph "+suffix, "P1-"+suffix).Scan(&pharmacyID))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO branches (pharmacy_id, name, code, is_active)
		VALUES ($1, 'main', $2, true) RETURNING id::text
	`, pharmacyID, "P1B-"+suffix).Scan(&branchID))

	var employeeID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO employees (account_id, pharmacy_id, branch_id, email, first_name, last_name, status, permissions_initialized)
		VALUES ($1, $2, $3, $4, 'p1', 'gate', 'active', TRUE) RETURNING id::text
	`, accountID, pharmacyID, branchID, "p1gate-emp-"+suffix+"@test.local").Scan(&employeeID))

	var globalP, globalQ string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO global_products (name, generic_name, dosage_form, strength, product_category, default_unit, is_active)
		VALUES ($1, 'gate', 'tablet', '1mg', 'medication', 'tablet', true) RETURNING id::text
	`, "P1-GATE-P-"+suffix).Scan(&globalP))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO global_products (name, generic_name, dosage_form, strength, product_category, default_unit, is_active)
		VALUES ($1, 'gate', 'tablet', '1mg', 'medication', 'tablet', true) RETURNING id::text
	`, "P1-GATE-Q-"+suffix).Scan(&globalQ))

	var productP, productQ string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO pharmacy_products (pharmacy_id, global_product_id, cost_price, selling_price, min_stock_level, is_active)
		VALUES ($1, $2, 1000, 1500, 0, true) RETURNING id::text
	`, pharmacyID, globalP).Scan(&productP))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO pharmacy_products (pharmacy_id, global_product_id, cost_price, selling_price, min_stock_level, is_active)
		VALUES ($1, $2, 2000, 3000, 0, true) RETURNING id::text
	`, pharmacyID, globalQ).Scan(&productQ))

	// P: batchP2 expires EARLIER than batchP1, so FEFO drains batchP2 first.
	var batchP1, batchP2, batchQ1, batchQ2 string
	p1Batch := func(productID, num string, qty int, expiry string, cost int64) string {
		var id string
		require.NoError(t, pool.QueryRow(ctx, `
			INSERT INTO inventory_batches (pharmacy_product_id, branch_id, batch_number, quantity, unit, cost_per_unit, expiry_date, received_date)
			VALUES ($1, $2, $3, $4, 'box', $5, $6::date, CURRENT_DATE) RETURNING id::text
		`, productID, branchID, "P1-"+num, qty, cost, expiry).Scan(&id))
		return id
	}
	batchP1 = p1Batch(productP, "B1", 5, "2028-01-01", 1000)
	batchP2 = p1Batch(productP, "B2", 10, "2027-01-01", 1000)
	batchQ1 = p1Batch(productQ, "Q1", 2, "2026-12-01", 2000)
	batchQ2 = p1Batch(productQ, "Q2", 10, "2027-06-01", 2000)

	var saleID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO sales (pharmacy_id, branch_id, idempotency_key, employee_id, payment_type)
		VALUES ($1, $2, $3, $4, 'cash') RETURNING id::text
	`, pharmacyID, branchID, "p1-gate-sale-"+suffix, employeeID).Scan(&saleID))

	principal := &auth.Principal{
		ID: employeeID, Type: auth.EmployeePrincipal,
		PharmacyID: pharmacyID, BranchID: branchID, IsActive: true, EmailVerified: true,
	}
	wholeBox := services.POSPricingSnapshot{PackagingType: services.PackagingWholeOnly, UnitsPerBox: 1, BoxPrice: 1500}

	// 3 lines; product P appears TWICE (shared locked batch state), line 3
	// splits across two batches of Q.
	items := []services.POSSaleItemRequest{
		{PharmacyProductID: productP, SaleUnit: "box", Quantity: 3},
		{PharmacyProductID: productP, SaleUnit: "box", Quantity: 4},
		{PharmacyProductID: productQ, SaleUnit: "box", Quantity: 5},
	}
	snapshots := []services.POSPricingSnapshot{wholeBox, wholeBox,
		{PackagingType: services.PackagingWholeOnly, UnitsPerBox: 1, BoxPrice: 3000}}
	lineAmounts := []money.Piastres{4500, 6000, 15000}

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	require.NoError(t, services.PersistPOSItems(ctx, tx, principal, branchID, saleID, items, snapshots, lineAmounts))
	require.NoError(t, tx.Commit(ctx))

	// ---- sale_items: 4 takes (3,4 from P-B2; 2,3 from Q) ----------------
	type itemRow struct {
		batchID   string
		quantity  int64
		saleQty   int64
		unitPrice int64
		amount    int64
		cost      int64
	}
	var items4 []itemRow
	var sumAmount, sumCost int64
	gr, err := pool.Query(ctx, `
		SELECT batch_id::text, quantity::int8, sale_quantity::int8, unit_price::int8,
		       amount_piastres::int8, cost_amount_piastres::int8
		FROM sale_items WHERE sale_id = $1 ORDER BY quantity::int8, batch_id::text`, saleID)
	require.NoError(t, err)
	for gr.Next() {
		var r itemRow
		require.NoError(t, gr.Scan(&r.batchID, &r.quantity, &r.saleQty, &r.unitPrice, &r.amount, &r.cost))
		items4 = append(items4, r)
		sumAmount += r.amount
		sumCost += r.cost
	}
	gr.Close()
	require.NoError(t, gr.Err())
	require.Len(t, items4, 4, "3 lines spanning 4 batch takes -> exactly 4 sale_items rows")
	assert.Equal(t, int64(4500+6000+15000), sumAmount, "allocated take amounts sum EXACTLY to the line totals")
	assert.Equal(t, int64(3*1000+4*1000+2*2000+3*2000), sumCost, "cost rows sum exactly")

	// ---- FEFO + cache: P drains batchP2 (2027) twice; Q splits 2+3 ------
	p1Qty := func(batch string) float64 {
		var q float64
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT quantity::float8 FROM inventory_batches WHERE id = $1`, batch).Scan(&q))
		return q
	}
	assert.Equal(t, 5.0, p1Qty(batchP1), "untouched batch keeps its quantity")
	assert.Equal(t, 3.0, p1Qty(batchP2), "10 - 3 - 4 = 3 — FEFO drained the earlier-expiry batch twice")
	assert.Equal(t, 0.0, p1Qty(batchQ1), "split line drains Q1 fully")
	assert.Equal(t, 7.0, p1Qty(batchQ2), "split line takes the remainder from Q2")

	// ---- movements: before/after chains per batch -----------------------
	type mv struct {
		batchID string
		qty     int64
		before  int64
		after   int64
	}
	var mvs []mv
	mr, err := pool.Query(ctx, `
		SELECT batch_id::text, quantity::int8, quantity_before::int8, quantity_after::int8
		FROM stock_movements WHERE reference_id = $1 AND movement_type = 'sale'
		ORDER BY quantity_before::int8 DESC, batch_id::text`, saleID)
	require.NoError(t, err)
	for mr.Next() {
		var m mv
		require.NoError(t, mr.Scan(&m.batchID, &m.qty, &m.before, &m.after))
		mvs = append(mvs, m)
	}
	mr.Close()
	require.NoError(t, mr.Err())
	require.Len(t, mvs, 4)
	assert.ElementsMatch(t, []mv{
		{batchP2, -3, 10, 7},
		{batchP2, -4, 7, 3},
		{batchQ1, -2, 2, 0},
		{batchQ2, -3, 10, 7},
	}, mvs, "movement chains must continue per batch")

	// ---- view agrees with the cache (R-1 invariant) ----------------------
	var viewQty float64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT quantity::float8 FROM current_inventory WHERE batch_id = $1`, batchP2).Scan(&viewQty))
	assert.Equal(t, 3.0, viewQty, "current_inventory must read the maintained cache (R-1)")
	var viewStatus string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status::text FROM current_inventory WHERE batch_id = $1`, batchQ1).Scan(&viewStatus))
	assert.Equal(t, "out_of_stock", viewStatus, "drained batch shows out_of_stock (migration-16 semantics kept)")

	// ---- last_sold_at stamped on both products ---------------------------
	var unsold int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM pharmacy_products
		WHERE id = ANY($1::uuid[]) AND last_sold_at IS NULL
	`, []string{productP, productQ}).Scan(&unsold))
	assert.Zero(t, unsold, "every sold product gets last_sold_at")

	// ---- insufficient stock -> sentinel + no partial writes --------------
	var sale2 string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO sales (pharmacy_id, branch_id, idempotency_key, employee_id, payment_type)
		VALUES ($1, $2, $3, $4, 'cash') RETURNING id::text
	`, pharmacyID, branchID, "p1-gate-sale2-"+suffix, employeeID).Scan(&sale2))
	tx2, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx2.Rollback(ctx) }()
	err = services.PersistPOSItems(ctx, tx2, principal, branchID, sale2,
		[]services.POSSaleItemRequest{{PharmacyProductID: productP, SaleUnit: "box", Quantity: 1000}},
		[]services.POSPricingSnapshot{wholeBox}, []money.Piastres{1_500_000})
	assert.ErrorIs(t, err, services.ErrPOSSaleInsufficientStock)
	require.NoError(t, tx2.Rollback(ctx))
	var itemCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM sale_items WHERE sale_id = $1`, sale2).Scan(&itemCount))
	assert.Zero(t, itemCount, "failed sale leaves no rows")
}
