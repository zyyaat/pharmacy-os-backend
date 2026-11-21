// D1+D3+D4 catalog wave regression gate (api_level 122, migration 53).
//
// This suite pins the LOCAL-product contract end to end against a real
// schema chain, from the outside, on top of migration 48's effective
// identity (overrides) and migration 49's invoice snapshots:
//
//   - SCHEMA: the origin constraints exist and are VALID, the catalog link
//     is nullable, and current_inventory was rebuilt with the LEFT JOIN
//     catalog (the migration's own self-checks, re-read from the outside),
//   - D1 INVARIANT: a local row (no catalog link, name_override set)
//     inserts; each of the three violations (local WITH a catalog link,
//     local WITHOUT a name, central WITHOUT a link) is rejected 23514,
//   - D1 IDENTITY: services.LoadPOSPricingBatch resolves the local row
//     through its override columns with no catalog row at all,
//   - D5 INTEGRATION: services.PersistPOSItems freezes the local row's
//     identity into sale_items.product_name_snapshot,
//   - D4 UNLINK: the transactional core enforces the confirm gate, detaches
//     the catalog row (origin flips local, global_product_id NULL, the
//     effective identity freezes into the override columns), FREEZES the
//     historical invoice name of a legacy NULL-snapshot line in the same
//     transaction, and refuses a second unlink with already_local,
//   - D3 PRICE HISTORY: recordPriceChangeTx writes old->new only when the
//     price actually moved (a no-op re-save writes nothing; a baseline row
//     carries NULL old price).
//
// Same harness as the other DB gates:
//
//	TEST_DATABASE_URL=postgres://.../pharmacy_scratch go test ./internal/handlers/ -run TestLocalProductsDB
package handlers

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pharmacy-os/backend/internal/auth"
	"github.com/pharmacy-os/backend/internal/money"
	"github.com/pharmacy-os/backend/internal/services"
)

func TestLocalProductsDB(t *testing.T) {
	ctx := context.Background()
	pool := posPersistPool(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	// ---- fixtures --------------------------------------------------------
	var accountID, pharmacyID, branchID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO accounts (company_name, contact_email, status)
		VALUES ($1, $2, 'active') RETURNING id::text
	`, "d1-gate "+suffix, "d1gate-"+suffix+"@test.local").Scan(&accountID))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM accounts WHERE id = $1`, accountID) // ON DELETE CASCADE sweeps the rest
	})
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO pharmacies (account_id, name, license_number, is_active)
		VALUES ($1, $2, $3, true) RETURNING id::text
	`, accountID, "d1-gate-ph "+suffix, "D1-"+suffix).Scan(&pharmacyID))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO branches (pharmacy_id, name, code, is_active)
		VALUES ($1, 'main', $2, true) RETURNING id::text
	`, pharmacyID, "D1B-"+suffix).Scan(&branchID))

	// ---- schema state: the migration's gates, re-read from outside -------
	var notValidConstraints int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_constraint
		WHERE conrelid = 'pharmacy_products'::regclass
		  AND conname IN ('pharmacy_products_origin_type_check',
		                  'pharmacy_products_origin_identity_check')
		  AND NOT convalidated
	`).Scan(&notValidConstraints))
	require.Zero(t, notValidConstraints, "the origin constraints must be VALID (full-table verification)")

	var stillNotNull int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_attribute
		WHERE attrelid = 'pharmacy_products'::regclass
		  AND attname = 'global_product_id' AND attnotnull
	`).Scan(&stillNotNull))
	require.Zero(t, stillNotNull, "global_product_id must be nullable for local products")

	var viewHasLeftJoin int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_views
		WHERE viewname = 'current_inventory' AND definition LIKE '%LEFT JOIN global_products%'
	`).Scan(&viewHasLeftJoin))
	require.Equal(t, 1, viewHasLeftJoin, "current_inventory must be local-product aware")

	// ---- D1: the invariant in both directions ----------------------------
	var centralGP string
	// NOTE: global_products rows are NOT swept by the accounts cascade (the
	// catalog is platform-wide), so every fixture code must be suffix-unique
	// to survive re-runs on the same scratch database.
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO global_products (name, dosage_form, product_category, default_unit, barcode, is_active)
		VALUES ($1, 'tablet', 'medication', 'tablet', $2, true) RETURNING id::text
	`, "D1-GATE-CENTRAL-"+suffix, "6220000000"+suffix).Scan(&centralGP))

	var centralPP string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO pharmacy_products (pharmacy_id, global_product_id, origin_type, cost_price, selling_price, is_active)
		VALUES ($1, $2, 'central', 1000, 1500, true) RETURNING id::text
	`, pharmacyID, centralGP).Scan(&centralPP))

	var localPP string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO pharmacy_products (pharmacy_id, origin_type, name_override, barcode_override,
		                               cost_price, selling_price, is_active)
		VALUES ($1, 'local', $2, $3, 500, 800, true)
		RETURNING id::text
	`, pharmacyID, "منتج محلي بوابة D1", "2000000000"+suffix).Scan(&localPP), "a fully tenant-owned local row must insert")

	requireViolation(t, pool, "23514",
		`INSERT INTO pharmacy_products (pharmacy_id, global_product_id, origin_type, selling_price, is_active)
		 VALUES ($1, $2, 'local', 100, true)`,
		"a local row WITH a catalog link must violate the identity invariant",
		pharmacyID, centralGP)
	requireViolation(t, pool, "23514",
		`INSERT INTO pharmacy_products (pharmacy_id, origin_type, selling_price, is_active)
		 VALUES ($1, 'local', 100, true)`,
		"a local row WITHOUT a name must violate the identity invariant",
		pharmacyID)
	requireViolation(t, pool, "23514",
		`INSERT INTO pharmacy_products (pharmacy_id, origin_type, selling_price, is_active)
		 VALUES ($1, 'central', 100, true)`,
		"a central row WITHOUT a catalog link must violate the identity invariant",
		pharmacyID)

	// ---- D1 identity + D5 snapshot through the real service paths --------
	var employeeID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO employees (account_id, pharmacy_id, branch_id, email, first_name, last_name, status, permissions_initialized)
		VALUES ($1, $2, $3, $4, 'd1', 'gate', 'active', TRUE) RETURNING id::text
	`, accountID, pharmacyID, branchID, "d1gate-emp-"+suffix+"@test.local").Scan(&employeeID))
	principal := &auth.Principal{
		ID: employeeID, Type: auth.EmployeePrincipal,
		PharmacyID: pharmacyID, BranchID: branchID, IsActive: true, EmailVerified: true,
	}

	var saleID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO sales (pharmacy_id, branch_id, idempotency_key, employee_id, payment_type)
		VALUES ($1, $2, $3, $4, 'cash') RETURNING id::text
	`, pharmacyID, branchID, "d1-gate-sale-"+suffix, employeeID).Scan(&saleID))

	// A legacy invoice line (pre-snapshot): the unlink must freeze it.
	var legacyBatch string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO inventory_batches (pharmacy_product_id, branch_id, batch_number, quantity, unit, cost_per_unit, received_date)
		VALUES ($1, $2, 'D1-LEGACY', 5, 'box', 1000, CURRENT_DATE) RETURNING id::text
	`, centralPP, branchID).Scan(&legacyBatch))
	_, legacyLineErr := pool.Exec(ctx, `
		INSERT INTO sale_items (sale_id, pharmacy_product_id, batch_id, sale_unit, quantity, base_quantity,
		                        unit_price, unit_cost, amount_piastres, cost_amount_piastres)
		VALUES ($1, $2, $3, 'box', 1, 1, 1500, 1000, 1500, 1000)
	`, saleID, centralPP, legacyBatch)
	require.NoError(t, legacyLineErr, "legacy sale line")

	// An override on the central row: the pharmacy renames what its cashiers
	// see without touching the shared row (migration 48 contract).
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `UPDATE pharmacy_products SET name_override = 'اسم العرض المخصص' WHERE id = $1::uuid`, centralPP)
	require.NoError(t, err)
	pricings, pricingErr := services.LoadPOSPricingBatch(ctx, tx, pharmacyID, []string{centralPP, localPP})
	require.NoError(t, pricingErr)
	require.Len(t, pricings, 2, "both products must resolve — the local one without any catalog row")
	require.NoError(t, tx.Rollback(ctx))

	assert.Equal(t, "اسم العرض المخصص", pricings[centralPP].Name,
		"D2: the display override wins on central rows")
	assert.Equal(t, "منتج محلي بوابة D1", pricings[localPP].Name,
		"D1: a local row's name_override IS its name (no catalog row needed)")

	// D5: freeze the identity into the invoice via the real write path.
	items := []services.POSSaleItemRequest{
		{PharmacyProductID: centralPP, SaleUnit: "box", Quantity: 1},
		{PharmacyProductID: localPP, SaleUnit: "box", Quantity: 2},
	}
	snapshots := []services.POSPricingSnapshot{pricings[centralPP], pricings[localPP]}
	lineAmounts := []money.Piastres{1500, 1600}

	var localBatch string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO inventory_batches (pharmacy_product_id, branch_id, batch_number, quantity, unit, cost_per_unit, received_date)
		VALUES ($1, $2, 'D1-LOCAL-1', 5, 'box', 500, CURRENT_DATE) RETURNING id::text
	`, localPP, branchID).Scan(&localBatch))

	tx, err = pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, services.PersistPOSItems(ctx, tx, principal, branchID, saleID, items, snapshots, lineAmounts))
	require.NoError(t, tx.Commit(ctx))

	var centralSnapshot, localSnapshot *string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT
		    (SELECT product_name_snapshot FROM sale_items
		      WHERE sale_id = $1::uuid AND pharmacy_product_id = $2::uuid
		        AND units_per_box_snapshot IS NOT NULL LIMIT 1),
		    (SELECT product_name_snapshot FROM sale_items
		      WHERE sale_id = $1::uuid AND pharmacy_product_id = $3::uuid LIMIT 1)
	`, saleID, centralPP, localPP).Scan(&centralSnapshot, &localSnapshot))
	require.NotNil(t, centralSnapshot)
	assert.Equal(t, "اسم العرض المخصص", *centralSnapshot, "D5: central lines freeze the effective name")
	require.NotNil(t, localSnapshot)
	assert.Equal(t, "منتج محلي بوابة D1", *localSnapshot, "D5: local lines freeze the tenant name")

	// ---- D4: unlink -------------------------------------------------------
	// confirm gate first (no writes may happen).
	tx, err = pool.Begin(ctx)
	require.NoError(t, err)
	_, failure := unlinkPharmacyProductTx(ctx, tx, principal, centralPP, false)
	require.NoError(t, tx.Rollback(ctx), "always release the tx before asserting")
	require.NotNil(t, failure)
	assert.Equal(t, "confirm_required", failure.code, "D4: no confirm, no unlink")
	assert.Equal(t, 400, failure.status)

	// The real unlink: the legacy invoice line (NULL snapshot) must be
	// frozen inside the same transaction.
	tx, err = pool.Begin(ctx)
	require.NoError(t, err)
	data, failure := unlinkPharmacyProductTx(ctx, tx, principal, centralPP, true)
	if failure != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("unlink of a central row must succeed: %+v", failure)
	}
	require.NoError(t, tx.Commit(ctx))
	assert.Equal(t, "local", data["origin_type"])

	var postOrigin, postGP, postName, postBarcode string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT origin_type::text, COALESCE(global_product_id::text, ''), name_override, COALESCE(barcode_override, '')
		FROM pharmacy_products WHERE id = $1::uuid
	`, centralPP).Scan(&postOrigin, &postGP, &postName, &postBarcode))
	assert.Equal(t, "local", postOrigin, "D4: the row flips to local")
	assert.Empty(t, postGP, "D4: the catalog link is detached")
	assert.Equal(t, "D1-GATE-CENTRAL-"+suffix, postName, "D4: the effective name freezes onto the tenant row")
	assert.Equal(t, "6220000000"+suffix, postBarcode, "D4: the effective barcode freezes onto the tenant row")

	var frozenName *string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT product_name_snapshot FROM sale_items
		WHERE sale_id = $1::uuid AND pharmacy_product_id = $2::uuid
		  AND product_name_snapshot = 'D1-GATE-CENTRAL-' || $3
		LIMIT 1
	`, saleID, centralPP, suffix).Scan(&frozenName), "D4: the legacy invoice name must be frozen at unlink time")

	// Second unlink is refused.
	tx, err = pool.Begin(ctx)
	require.NoError(t, err)
	_, failure = unlinkPharmacyProductTx(ctx, tx, principal, centralPP, true)
	require.NoError(t, tx.Rollback(ctx), "always release the tx before asserting")
	require.NotNil(t, failure)
	assert.Equal(t, "already_local", failure.code)
	assert.Equal(t, 409, failure.status)

	// ---- D3: price history ------------------------------------------------
	var historyBefore int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM pharmacy_product_price_history WHERE pharmacy_product_id = $1::uuid
	`, localPP).Scan(&historyBefore))

	tx, err = pool.Begin(ctx)
	require.NoError(t, err)
	// A no-op re-save: same price, no partial change -> NO row.
	require.NoError(t, recordPriceChangeTx(ctx, tx, pharmacyID, localPP,
		money.Piastres(800), money.Piastres(800), nil, nil,
		priceSourceManualEdit, principal))
	// A real change: 800 -> 1200 with the strip price moving too.
	newPartial := int64(150)
	require.NoError(t, recordPriceChangeTx(ctx, tx, pharmacyID, localPP,
		money.Piastres(800), money.Piastres(1200), nil, &newPartial,
		priceSourceManualEdit, principal))
	// A creation baseline: always written, old price NULL.
	require.NoError(t, recordPriceChangeTx(ctx, tx, pharmacyID, localPP,
		money.Piastres(0), money.Piastres(300), nil, nil,
		priceSourceProductCreate, principal))
	require.NoError(t, tx.Commit(ctx))

	var historyAfter, baselineRows int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE change_source = 'product_create' AND old_selling_price IS NULL)
		FROM pharmacy_product_price_history WHERE pharmacy_product_id = $1::uuid
	`, localPP).Scan(&historyAfter, &baselineRows))
	assert.Equal(t, historyBefore+2, historyAfter,
		"D3: the no-op re-save writes nothing; the real change and the baseline each write one row")
	assert.Equal(t, 1, baselineRows, "D3: exactly one baseline row with NULL old price")

	var movedNew, movedPartial *float64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT new_selling_price::float8, new_partial_selling_price::float8
		FROM pharmacy_product_price_history
		WHERE pharmacy_product_id = $1::uuid AND change_source = 'manual_edit'
		  AND new_selling_price = 1200
	`, localPP).Scan(&movedNew, &movedPartial))
	require.NotNil(t, movedNew)
	assert.EqualValues(t, 1200, *movedNew)
	require.NotNil(t, movedPartial)
	assert.EqualValues(t, 150, *movedPartial, "D3: the strip price rides along in the same event")
}

// requireViolation asserts that the given single-row INSERT fails with the
// expected SQLSTATE (the invariant CHECKs raise check_violation, 23514).
func requireViolation(t *testing.T, pool *pgxpool.Pool, code, stmt, msg string, args ...any) {
	t.Helper()
	_, err := pool.Exec(context.Background(), stmt, args...)
	require.Error(t, err, "%s: expected SQLSTATE %s", msg, code)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: expected a PostgreSQL error, got %T: %v", msg, err, err)
	}
	if pgErr.Code != code {
		t.Fatalf("%s: expected SQLSTATE %s, got %s: %s (detail: %s)", msg, code, pgErr.Code, pgErr.Message, pgErr.Detail)
	}
}
