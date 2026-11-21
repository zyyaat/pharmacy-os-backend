// P8 regression gate for ledger item 1 (ADR-0005, api_level 108):
// services.CreatePOSSale — the whole POS checkout moved out of
// product_pos_handler.go. The P1 gate (TestPOSPersistDB, handlers package)
// still pins PersistPOSItems semantics through the moved function; THIS
// suite pins the ORCHESTRATION the service now owns, against the real
// migrated schema:
//   - the happy path writes the exact invoice: FEFO drain, per-line
//     largest-remainder amounts summing to the net total, sales.total_amount
//     and sales.discount_amount matching the outcome, movements chained
//     before/after and attributed to the principal's actor column,
//   - the AUD-015 idempotent replay returns the ORIGINAL invoice
//     (Replayed=true, same sale id, one sale row, no double drain),
//   - a stale client price intent is rejected with *POSSalePriceChanged
//     carrying the AUTHORITATIVE prices, and the rollback leaves no rows,
//   - insufficient stock is the sentinel contract with zero partial writes,
//   - AUD-009: a customer UUID outside the pharmacy is ErrPOSCustomerForeign,
//   - a discount larger than the subtotal is ErrPOSDiscountExceedsTotal.
//
// Every business rejection here maps 1:1 onto the wire body the handler
// emits (respondPOSSaleFailure) — the handler's own protocol branches
// (binding, key shape, unit vocabulary) are pinned by the source-scan gate
// TestPOSSaleOwnershipAndIdempotencyContract and the unchanged route.
//
// Same harness as the other DB gates:
//
//	TEST_DATABASE_URL=postgres://.../pharmacy_scratch go test ./internal/services/ -run TestPOSSaleServiceDB
package services

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pharmacy-os/backend/internal/auth"
	"github.com/pharmacy-os/backend/internal/money"
)

func TestPOSSaleServiceDB(t *testing.T) {
	ctx := context.Background()
	pool := platformStatsPool(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	// ---- fixtures (account cascade sweeps everything) --------------------
	var accountID, pharmacyID, branchID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO accounts (company_name, contact_email, status)
		VALUES ($1, $2, 'active') RETURNING id::text
	`, "p8-gate "+suffix, "p8gate-"+suffix+"@test.local").Scan(&accountID))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM accounts WHERE id = $1`, accountID) // ON DELETE CASCADE sweeps the rest
	})
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO pharmacies (account_id, name, license_number, is_active)
		VALUES ($1, $2, $3, true) RETURNING id::text
	`, accountID, "p8-gate-ph "+suffix, "P8-"+suffix).Scan(&pharmacyID))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO branches (pharmacy_id, name, code, is_active)
		VALUES ($1, 'main', $2, true) RETURNING id::text
	`, pharmacyID, "P8B-"+suffix).Scan(&branchID))

	var employeeID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO employees (account_id, pharmacy_id, branch_id, email, first_name, last_name, status, permissions_initialized)
		VALUES ($1, $2, $3, $4, 'p8', 'gate', 'active', TRUE) RETURNING id::text
	`, accountID, pharmacyID, branchID, "p8gate-emp-"+suffix+"@test.local").Scan(&employeeID))

	principal := &auth.Principal{
		ID: employeeID, Type: auth.EmployeePrincipal,
		PharmacyID: pharmacyID, BranchID: branchID, IsActive: true, EmailVerified: true,
	}

	var globalP, globalQ string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO global_products (name, generic_name, dosage_form, strength, product_category, default_unit, is_active)
		VALUES ($1, 'gate', 'tablet', '1mg', 'medication', 'tablet', true) RETURNING id::text
	`, "P8-GATE-P-"+suffix).Scan(&globalP))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO global_products (name, generic_name, dosage_form, strength, product_category, default_unit, is_active)
		VALUES ($1, 'gate', 'tablet', '1mg', 'medication', 'tablet', true) RETURNING id::text
	`, "P8-GATE-Q-"+suffix).Scan(&globalQ))

	var productP, productQ string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO pharmacy_products (pharmacy_id, global_product_id, cost_price, selling_price, min_stock_level, is_active, packaging_type, units_per_box)
		VALUES ($1, $2, 1000, 1500, 0, true, 'WHOLE_ONLY', 1) RETURNING id::text
	`, pharmacyID, globalP).Scan(&productP))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO pharmacy_products (pharmacy_id, global_product_id, cost_price, selling_price, min_stock_level, is_active, packaging_type, units_per_box)
		VALUES ($1, $2, 2000, 3000, 0, true, 'WHOLE_ONLY', 1) RETURNING id::text
	`, pharmacyID, globalQ).Scan(&productQ))

	addBatch := func(productID, num string, qty int) string {
		var id string
		require.NoError(t, pool.QueryRow(ctx, `
			INSERT INTO inventory_batches (pharmacy_product_id, branch_id, batch_number, quantity, unit, cost_per_unit, expiry_date, received_date)
			VALUES ($1, $2, $3, $4, 'box', $5, '2028-01-01'::date, CURRENT_DATE) RETURNING id::text
		`, productID, branchID, "P8-"+num, qty, 1000).Scan(&id))
		return id
	}
	batchP := addBatch(productP, "BP", 5)
	batchQ := addBatch(productQ, "BQ", 5)

	batchQty := func(batch string) float64 {
		var q float64
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT quantity::float8 FROM inventory_batches WHERE id = $1`, batch).Scan(&q))
		return q
	}
	saleRows := func(key string) int {
		var n int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT COUNT(*)::int FROM sales WHERE pharmacy_id = $1 AND idempotency_key = $2`,
			pharmacyID, key).Scan(&n))
		return n
	}

	// ---- 1) happy path: 2 lines, 10% percent discount ---------------------
	// P box x2 = 3000, Q box x1 = 3000 -> subtotal 6000, discount 600,
	// largest-remainder over weights (3000,3000) = (300,300) — exact split.
	key1 := "p8-gate-sale1-" + suffix
	out, err := CreatePOSSale(ctx, pool, principal, branchID, POSSaleRequest{
		Items: []POSSaleItemRequest{
			{PharmacyProductID: productP, SaleUnit: "box", Quantity: 2},
			{PharmacyProductID: productQ, SaleUnit: "box", Quantity: 1},
		},
		IdempotencyKey: key1,
		PaymentType:    "cash",
		Discount:       &POSSaleDiscountRequest{Kind: "percent", Value: 10},
	})
	require.NoError(t, err)
	assert.False(t, out.Replayed)
	assert.Equal(t, money.Piastres(600), out.Discount, "10% of 6000 = 600")
	assert.Equal(t, money.Piastres(5400), out.Total, "net total = 6000 - 600")

	var total, discount int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT total_amount::int8, discount_amount::int8 FROM sales WHERE id = $1::uuid
	`, out.SaleID).Scan(&total, &discount))
	assert.Equal(t, int64(5400), total, "sales.total_amount matches the outcome")
	assert.Equal(t, int64(600), discount, "sales.discount_amount matches the outcome")

	var itemSum int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_piastres),0)::int8 FROM sale_items WHERE sale_id = $1::uuid
	`, out.SaleID).Scan(&itemSum))
	assert.Equal(t, int64(5400), itemSum, "allocated line amounts sum EXACTLY to the net total")

	assert.Equal(t, 3.0, batchQty(batchP), "5 - 2 = 3 — FEFO drain via the service")
	assert.Equal(t, 4.0, batchQty(batchQ), "5 - 1 = 4")

	var mvBefore, mvAfter int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT quantity_before::int8, quantity_after::int8 FROM stock_movements
		WHERE reference_id = $1::uuid AND movement_type = 'sale' AND batch_id = $2::uuid
	`, out.SaleID, batchP).Scan(&mvBefore, &mvAfter))
	assert.Equal(t, int64(5), mvBefore, "movement chain opens at the locked quantity")
	assert.Equal(t, int64(3), mvAfter, "movement chain closes at the final quantity")

	var createdBy string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT created_by::text FROM stock_movements
		WHERE reference_id = $1::uuid AND movement_type = 'sale' LIMIT 1
	`, out.SaleID).Scan(&createdBy))
	assert.Equal(t, employeeID, createdBy, "employee principal writes created_by (auth.ActorIDs)")

	// ---- 2) AUD-015 idempotent replay: same key returns the original ------
	out2, err := CreatePOSSale(ctx, pool, principal, branchID, POSSaleRequest{
		Items: []POSSaleItemRequest{
			{PharmacyProductID: productP, SaleUnit: "box", Quantity: 2},
			{PharmacyProductID: productQ, SaleUnit: "box", Quantity: 1},
		},
		IdempotencyKey: key1,
		PaymentType:    "cash",
	})
	require.NoError(t, err)
	assert.True(t, out2.Replayed, "same key replays the original invoice")
	assert.Equal(t, out.SaleID, out2.SaleID, "replay carries the original sale id")
	assert.Equal(t, money.Piastres(5400), out2.Total, "replay carries the original total")
	assert.Equal(t, 1, saleRows(key1), "no second sale row")
	assert.Equal(t, 3.0, batchQty(batchP), "no double drain on replay")

	// ---- 3) stale price intent -> *POSSalePriceChanged, zero rows ---------
	stale := int64(999)
	_, err = CreatePOSSale(ctx, pool, principal, branchID, POSSaleRequest{
		Items: []POSSaleItemRequest{
			{PharmacyProductID: productP, SaleUnit: "box", Quantity: 2, ExpectedUnitPricePiastres: &stale},
		},
		IdempotencyKey: "p8-gate-stale-" + suffix,
		PaymentType:    "cash",
	})
	require.Error(t, err)
	var pch *POSSalePriceChanged
	require.ErrorAs(t, err, &pch, "stale intent surfaces as the typed price_changed error")
	require.Len(t, pch.Items, 1)
	assert.Equal(t, 0, pch.Items[0].Index)
	assert.Equal(t, productP, pch.Items[0].PharmacyProductID)
	assert.Equal(t, money.Piastres(1500), pch.Items[0].UnitPricePiastres, "mismatch carries the AUTHORITATIVE unit price")
	assert.Equal(t, money.Piastres(3000), pch.Items[0].LineTotalPiastres, "mismatch carries the AUTHORITATIVE line total")
	assert.Zero(t, saleRows("p8-gate-stale-"+suffix), "rejected checkout leaves no sale row")

	// ---- 4) insufficient stock -> sentinel, no partial writes -------------
	_, err = CreatePOSSale(ctx, pool, principal, branchID, POSSaleRequest{
		Items: []POSSaleItemRequest{
			{PharmacyProductID: productP, SaleUnit: "box", Quantity: 1000},
		},
		IdempotencyKey: "p8-gate-insuf-" + suffix,
		PaymentType:    "cash",
	})
	require.ErrorIs(t, err, ErrPOSSaleInsufficientStock, "the R-5 sentinel contract survives the extraction")
	assert.Zero(t, saleRows("p8-gate-insuf-"+suffix), "no sale row survives the rollback")
	assert.Equal(t, 3.0, batchQty(batchP), "no stock moved")

	// ---- 5) AUD-009: foreign customer UUID is rejected inside the tx ------
	_, err = CreatePOSSale(ctx, pool, principal, branchID, POSSaleRequest{
		Items: []POSSaleItemRequest{
			{PharmacyProductID: productP, SaleUnit: "box", Quantity: 1},
		},
		IdempotencyKey: "p8-gate-foreign-" + suffix,
		PaymentType:    "cash",
		CustomerID:     "00000000-0000-0000-0000-00000000000f",
	})
	require.ErrorIs(t, err, ErrPOSCustomerForeign, "a customer outside the pharmacy is invalid_customer")
	assert.Zero(t, saleRows("p8-gate-foreign-"+suffix), "foreign-customer checkout leaves no sale row")

	// ---- 6) discount exceeding the subtotal is a business reject ----------
	_, err = CreatePOSSale(ctx, pool, principal, branchID, POSSaleRequest{
		Items: []POSSaleItemRequest{
			{PharmacyProductID: productP, SaleUnit: "box", Quantity: 1},
		},
		IdempotencyKey: "p8-gate-disc-" + suffix,
		PaymentType:    "cash",
		Discount:       &POSSaleDiscountRequest{Kind: "amount", Value: 999999},
	})
	require.ErrorIs(t, err, ErrPOSDiscountExceedsTotal)
	assert.Zero(t, saleRows("p8-gate-disc-"+suffix), "over-discount checkout leaves no sale row")

	// ---- 7) the view agrees with the cache (R-1 invariant) ----------------
	var viewQty float64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT quantity::float8 FROM current_inventory WHERE batch_id = $1`, batchP).Scan(&viewQty))
	assert.Equal(t, 3.0, viewQty, "current_inventory reads the maintained cache (R-1)")
}
