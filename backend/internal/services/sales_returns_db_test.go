// P9 regression gate for ledger item 2 (ADR-0005, api_level 109):
// services.CreatePOSSaleReturn — the whole POS credit-note transaction moved
// out of sales_history_handler.go. THIS suite pins the ORCHESTRATION the
// service now owns, against the real migrated schema:
//   - a partial return refunds the largest-remainder share of the remaining
//     amount, writes the credit note + its items, restocks the EXACT batch
//     the strips were sold from, and chains the return_from_customer
//     movement (before/after) at the sale row's original unit cost,
//   - the idempotent replay returns the ORIGINAL credit note (Replayed=true,
//     same return id, no second refund, no extra rows),
//   - chained partial returns converge to the exact line amount, and the
//     invoice status machine flips partially_returned -> returned only when
//     every row is fully returned,
//   - a quantity above the returnable remainder is *POSReturnExceedsSold
//     with the exact returnable count and zero writes,
//   - the invoice lock is pharmacy-scoped: a foreign pharmacy's sale is
//     ErrPOSSaleNotFound, and an unknown sale item is ErrPOSReturnItemNotFound,
//   - the detail read (services.POSSaleDetailByID) exposes the same
//     returned aggregates the wire served before the extraction.
//
// Every business rejection here maps 1:1 onto the wire body the handler
// emits (respondPOSReturnFailure) — the handler's own protocol branches
// (binding, item bounds, key shape, duplicates) are pinned by the
// source-scan gate TestPOSReturnContract and the unchanged route.
//
// Same harness as the other DB gates:
//
//	TEST_DATABASE_URL=postgres://.../pharmacy_scratch go test ./internal/services/ -run TestPOSSaleReturnServiceDB
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

func TestPOSSaleReturnServiceDB(t *testing.T) {
	ctx := context.Background()
	pool := platformStatsPool(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	// ---- fixtures (account cascade sweeps everything) --------------------
	var accountID, pharmacyID, branchID string
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO accounts (company_name, contact_email, status)
                VALUES ($1, $2, 'active') RETURNING id::text
        `, "p9-gate "+suffix, "p9gate-"+suffix+"@test.local").Scan(&accountID))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM accounts WHERE id = $1`, accountID) // ON DELETE CASCADE sweeps the rest
	})
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacies (account_id, name, license_number, is_active)
                VALUES ($1, $2, $3, true) RETURNING id::text
        `, accountID, "p9-gate-ph "+suffix, "P9-"+suffix).Scan(&pharmacyID))
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO branches (pharmacy_id, name, code, is_active)
                VALUES ($1, 'main', $2, true) RETURNING id::text
        `, pharmacyID, "P9B-"+suffix).Scan(&branchID))

	var employeeID string
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO employees (account_id, pharmacy_id, branch_id, email, first_name, last_name, status, permissions_initialized)
                VALUES ($1, $2, $3, $4, 'p9', 'gate', 'active', TRUE) RETURNING id::text
        `, accountID, pharmacyID, branchID, "p9gate-emp-"+suffix+"@test.local").Scan(&employeeID))

	principal := &auth.Principal{
		ID: employeeID, Type: auth.EmployeePrincipal,
		PharmacyID: pharmacyID, BranchID: branchID, IsActive: true, EmailVerified: true,
	}

	var globalP, globalQ string
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO global_products (name, generic_name, dosage_form, strength, product_category, default_unit, is_active)
                VALUES ($1, 'gate', 'tablet', '1mg', 'medication', 'tablet', true) RETURNING id::text
        `, "P9-GATE-P-"+suffix).Scan(&globalP))
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO global_products (name, generic_name, dosage_form, strength, product_category, default_unit, is_active)
                VALUES ($1, 'gate', 'tablet', '1mg', 'medication', 'tablet', true) RETURNING id::text
        `, "P9-GATE-Q-"+suffix).Scan(&globalQ))

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
                `, productID, branchID, "P9-"+num, qty, 1000).Scan(&id))
		return id
	}
	batchP := addBatch(productP, "BP", 5)
	batchQ := addBatch(productQ, "BQ", 5)

	// The fixture invoice: P box x2 (3000) + Q box x1 (3000), no discount —
	// line amounts are exactly 3000/3000, built through the tested sale path.
	outSale, err := CreatePOSSale(ctx, pool, principal, branchID, POSSaleRequest{
		Items: []POSSaleItemRequest{
			{PharmacyProductID: productP, SaleUnit: "box", Quantity: 2},
			{PharmacyProductID: productQ, SaleUnit: "box", Quantity: 1},
		},
		IdempotencyKey: "p9-gate-sale-" + suffix,
		PaymentType:    "cash",
	})
	require.NoError(t, err)

	batchQty := func(batch string) float64 {
		var q float64
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT quantity::float8 FROM inventory_batches WHERE id = $1`, batch).Scan(&q))
		return q
	}
	returnCount := func() int {
		var n int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT COUNT(*)::int FROM sale_returns WHERE pharmacy_id = $1`, pharmacyID).Scan(&n))
		return n
	}
	itemOf := func(saleID, productID string) string {
		var id string
		require.NoError(t, pool.QueryRow(ctx, `
                        SELECT si.id::text FROM sale_items si
                        JOIN sales s ON s.id = si.sale_id
                        WHERE s.id = $1::uuid AND si.pharmacy_product_id = $2::uuid
                `, saleID, productID).Scan(&id))
		return id
	}
	itemP, itemQ := itemOf(outSale.SaleID, productP), itemOf(outSale.SaleID, productQ)

	// ---- 1) partial return: largest-remainder share of the remaining amount
	key1 := "p9-gate-ret1-" + suffix
	out, err := CreatePOSSaleReturn(ctx, pool, principal, outSale.SaleID, POSReturnRequest{
		Items:          []POSReturnItemRequest{{SaleItemID: itemP, Quantity: 1}},
		Reason:         "p9 partial",
		IdempotencyKey: key1,
	})
	require.NoError(t, err)
	assert.False(t, out.Replayed)
	assert.Equal(t, money.Piastres(1500), out.Total,
		"1 of 2 strips: Allocate(3000,[1,1])[0] = 1500 — same largest-remainder rule as sale time")
	assert.Equal(t, "partially_returned", out.SaleStatus)
	require.Len(t, out.Items, 1)
	assert.Equal(t, itemP, out.Items[0].SaleItemID)
	assert.Equal(t, int64(1), out.Items[0].Quantity)
	assert.Equal(t, money.Piastres(1500), out.Items[0].Amount)

	var retTotal, retNumber int64
	var retSale, retReason string
	require.NoError(t, pool.QueryRow(ctx, `
                SELECT total_amount_piastres::int8, return_number::int8, sale_id::text, reason
                FROM sale_returns WHERE id = $1::uuid
        `, out.ReturnID).Scan(&retTotal, &retNumber, &retSale, &retReason))
	assert.Equal(t, int64(1500), retTotal, "credit note stores the computed refund")
	assert.Equal(t, outSale.SaleID, retSale, "credit note points at the original invoice")
	assert.Equal(t, "p9 partial", retReason)
	assert.NotZero(t, retNumber, "return_number is assigned")

	var riQty, riAmount int64
	var riBatch string
	require.NoError(t, pool.QueryRow(ctx, `
                SELECT quantity::int8, amount_piastres::int8, batch_id::text
                FROM sale_return_items WHERE return_id = $1::uuid
        `, out.ReturnID).Scan(&riQty, &riAmount, &riBatch))
	assert.Equal(t, int64(1), riQty)
	assert.Equal(t, int64(1500), riAmount)
	assert.Equal(t, batchP, riBatch, "the return item points at the EXACT batch the strips were sold from")

	assert.Equal(t, 4.0, batchQty(batchP), "3 + 1 = 4 — restock into the exact batch")
	assert.Equal(t, 4.0, batchQty(batchQ), "Q untouched")

	var mvBefore, mvAfter int64
	var mvUnit, mvCreatedBy string
	var mvCost, mvTotalCost int64
	require.NoError(t, pool.QueryRow(ctx, `
                SELECT quantity_before::int8, quantity_after::int8, unit::text,
                       unit_cost::int8, total_cost::int8, created_by::text
                FROM stock_movements
                WHERE reference_id = $1::uuid AND movement_type = 'return_from_customer'
        `, out.ReturnID).Scan(&mvBefore, &mvAfter, &mvUnit, &mvCost, &mvTotalCost, &mvCreatedBy))
	assert.Equal(t, int64(3), mvBefore, "movement chain opens at the pre-restock quantity")
	assert.Equal(t, int64(4), mvAfter, "movement chain closes at the post-restock quantity")
	assert.Equal(t, "box", mvUnit)
	var saleUnitCost int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT unit_cost::int8 FROM sale_items WHERE id = $1::uuid`, itemP).Scan(&saleUnitCost))
	assert.Equal(t, saleUnitCost, mvCost, "the return movement runs at the sale row's original unit cost")
	assert.Equal(t, saleUnitCost*1, mvTotalCost, "total_cost = unit_cost * returned quantity")
	assert.Equal(t, employeeID, mvCreatedBy, "employee principal writes created_by (auth.ActorIDs)")

	// ---- 2) idempotent replay: same key returns the original credit note --
	out2, err := CreatePOSSaleReturn(ctx, pool, principal, outSale.SaleID, POSReturnRequest{
		Items:          []POSReturnItemRequest{{SaleItemID: itemP, Quantity: 1}},
		Reason:         "retry",
		IdempotencyKey: key1,
	})
	require.NoError(t, err)
	assert.True(t, out2.Replayed, "same key replays the original credit note")
	assert.Equal(t, out.ReturnID, out2.ReturnID, "replay carries the original return id")
	assert.Equal(t, money.Piastres(1500), out2.Total, "replay carries the original refund")
	assert.Equal(t, "partially_returned", out2.SaleStatus, "replay reports the current sale status")
	assert.Equal(t, 1, returnCount(), "no second credit note")
	assert.Equal(t, 4.0, batchQty(batchP), "no double restock on replay")

	// ---- 3) chained partial returns converge to the exact line amount ----
	out3, err := CreatePOSSaleReturn(ctx, pool, principal, outSale.SaleID, POSReturnRequest{
		Items:          []POSReturnItemRequest{{SaleItemID: itemP, Quantity: 1}},
		IdempotencyKey: "p9-gate-ret2-" + suffix,
	})
	require.NoError(t, err)
	assert.Equal(t, money.Piastres(1500), out3.Total,
		"the last returnable strip refunds exactly the remaining amount (3000 - 1500)")
	assert.Equal(t, "partially_returned", out3.SaleStatus, "Q is still outstanding")

	out4, err := CreatePOSSaleReturn(ctx, pool, principal, outSale.SaleID, POSReturnRequest{
		Items:          []POSReturnItemRequest{{SaleItemID: itemQ, Quantity: 1}},
		IdempotencyKey: "p9-gate-ret3-" + suffix,
	})
	require.NoError(t, err)
	assert.Equal(t, money.Piastres(3000), out4.Total, "full row refunds the whole remaining amount")
	assert.Equal(t, "returned", out4.SaleStatus, "every row fully returned -> the invoice status flips")
	assert.Equal(t, 3, returnCount(), "three credit notes for three accepted returns")
	assert.Equal(t, 5.0, batchQty(batchP), "2 strips went back to the original batch")
	assert.Equal(t, 5.0, batchQty(batchQ), "1 strip went back to the original batch")

	var saleStatus string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status::text FROM sales WHERE id = $1::uuid`, outSale.SaleID).Scan(&saleStatus))
	assert.Equal(t, "returned", saleStatus, "sales.status is authoritative in the table")

	// ---- 4) exceeding the returnable remainder is a typed reject ----------
	_, err = CreatePOSSaleReturn(ctx, pool, principal, outSale.SaleID, POSReturnRequest{
		Items:          []POSReturnItemRequest{{SaleItemID: itemP, Quantity: 1}},
		IdempotencyKey: "p9-gate-over-" + suffix,
	})
	require.Error(t, err)
	var exceeds *POSReturnExceedsSold
	require.ErrorAs(t, err, &exceeds, "over-return surfaces as the typed exceeds error")
	assert.Equal(t, itemP, exceeds.SaleItemID)
	assert.Equal(t, int64(0), exceeds.ReturnableQuantityBase, "nothing is returnable anymore")
	assert.Equal(t, 3, returnCount(), "the rejected return leaves no credit note")
	assert.Equal(t, 5.0, batchQty(batchP), "and no restock")

	// ---- 5) the invoice lock is pharmacy-scoped (cross-tenant sale) -------
	var account2, pharmacy2, branch2 string
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO accounts (company_name, contact_email, status)
                VALUES ($1, $2, 'active') RETURNING id::text
        `, "p9-gate-f2 "+suffix, "p9gate2-"+suffix+"@test.local").Scan(&account2))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, account2)
	})
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacies (account_id, name, license_number, is_active)
                VALUES ($1, $2, $3, true) RETURNING id::text
        `, account2, "p9-gate-ph2 "+suffix, "P9F-"+suffix).Scan(&pharmacy2))
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO branches (pharmacy_id, name, code, is_active)
                VALUES ($1, 'main', $2, true) RETURNING id::text
        `, pharmacy2, "P9B2-"+suffix).Scan(&branch2))
	var employee2 string
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO employees (account_id, pharmacy_id, branch_id, email, first_name, last_name, status, permissions_initialized)
                VALUES ($1, $2, $3, $4, 'p9', 'foreign', 'active', TRUE) RETURNING id::text
        `, account2, pharmacy2, branch2, "p9gate-emp2-"+suffix+"@test.local").Scan(&employee2))
	principal2 := &auth.Principal{
		ID: employee2, Type: auth.EmployeePrincipal,
		PharmacyID: pharmacy2, BranchID: branch2, IsActive: true, EmailVerified: true,
	}
	var globalR, productR string
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO global_products (name, generic_name, dosage_form, strength, product_category, default_unit, is_active)
                VALUES ($1, 'gate', 'tablet', '1mg', 'medication', 'tablet', true) RETURNING id::text
        `, "P9-GATE-R-"+suffix).Scan(&globalR))
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacy_products (pharmacy_id, global_product_id, cost_price, selling_price, min_stock_level, is_active, packaging_type, units_per_box)
                VALUES ($1, $2, 1000, 1500, 0, true, 'WHOLE_ONLY', 1) RETURNING id::text
        `, pharmacy2, globalR).Scan(&productR))
	// The addBatch closure above is bound to pharmacy1's branch — the foreign
	// batch needs its own insert on branch2.
	var foreignBatch string
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO inventory_batches (pharmacy_product_id, branch_id, batch_number, quantity, unit, cost_per_unit, expiry_date, received_date)
                VALUES ($1, $2, $3, 5, 'box', 1000, '2028-01-01'::date, CURRENT_DATE) RETURNING id::text
        `, productR, branch2, "P9-BR").Scan(&foreignBatch))
	assert.NotZero(t, foreignBatch)
	foreignSale, err := CreatePOSSale(ctx, pool, principal2, branch2, POSSaleRequest{
		Items:          []POSSaleItemRequest{{PharmacyProductID: productR, SaleUnit: "box", Quantity: 1}},
		IdempotencyKey: "p9-gate-foreign-sale-" + suffix,
		PaymentType:    "cash",
	})
	require.NoError(t, err)

	_, err = CreatePOSSaleReturn(ctx, pool, principal, foreignSale.SaleID, POSReturnRequest{
		Items:          []POSReturnItemRequest{{SaleItemID: itemOf(foreignSale.SaleID, productR), Quantity: 1}},
		IdempotencyKey: "p9-gate-foreign-ret-" + suffix,
	})
	require.ErrorIs(t, err, ErrPOSSaleNotFound,
		"another pharmacy's invoice does not exist behind this principal's lock")

	// ---- 6) a sale item outside the invoice is a typed reject -------------
	_, err = CreatePOSSaleReturn(ctx, pool, principal, outSale.SaleID, POSReturnRequest{
		Items:          []POSReturnItemRequest{{SaleItemID: "00000000-0000-0000-0000-0000000000f0", Quantity: 1}},
		IdempotencyKey: "p9-gate-unknown-item-" + suffix,
	})
	require.ErrorIs(t, err, ErrPOSReturnItemNotFound)
	assert.Equal(t, 3, returnCount(), "the unknown-item reject leaves no credit note")

	// ---- 7) direct service callers are guarded against an empty note ------
	_, err = CreatePOSSaleReturn(ctx, pool, principal, outSale.SaleID, POSReturnRequest{
		IdempotencyKey: "p9-gate-empty-" + suffix,
	})
	require.ErrorIs(t, err, ErrPOSReturnNoItems)

	// ---- 8) the detail read exposes the same aggregates the wire served ---
	detail, err := POSSaleDetailByID(ctx, pool, pharmacyID, outSale.SaleID)
	require.NoError(t, err)
	assert.Equal(t, "returned", detail.Sale.Status)
	assert.Equal(t, int64(6000), detail.ReturnedTotal, "1500+1500+3000 — the sum of the credit notes")
	assert.Equal(t, money.Piastres(6000), detail.Sale.TotalAmount)
	require.Len(t, detail.Returns, 3)
	require.Len(t, detail.Items, 2)
	byItem := map[string]POSSaleLine{}
	for _, line := range detail.Items {
		byItem[line.SaleItemID] = line
	}
	assert.Equal(t, int64(2), byItem[itemP].ReturnedQty, "both P strips show as returned")
	assert.Equal(t, int64(0), byItem[itemP].QuantityBase-byItem[itemP].ReturnedQty, "nothing returnable on P")
	assert.Equal(t, int64(3000), byItem[itemP].ReturnedAmount, "P returned amount = 1500+1500")
	assert.Equal(t, int64(1), byItem[itemQ].ReturnedQty)
	assert.Equal(t, int64(3000), byItem[itemQ].ReturnedAmount)

	// ---- 9) detail of an unknown invoice is the shared sentinel -----------
	_, err = POSSaleDetailByID(ctx, pool, pharmacyID, "00000000-0000-0000-0000-0000000000e0")
	require.ErrorIs(t, err, ErrPOSSaleNotFound)
}
