package handlers

// Product ownership regression gates (migration 48 + 49).
//
// Three contracts, all proven against the real migration chain:
//
//  1. OWNERSHIP — a pharmacy edit of a product's identity (name, barcode,
//     strength, dosage form) succeeds EVEN on a platform-governed catalog
//     row (the retired T-B1 403), lands in the pharmacy_products override
//     columns, and never mutates global_products. Effective identity flows
//     through every read lens: GET product, POS barcode lookup, and the
//     current_inventory view. A local barcode stays unique inside the
//     pharmacy; a pure price edit writes nothing.
//
//  2. INVOICE SNAPSHOT — a POS sale freezes the effective product identity
//     (override first, official fallback) into sale_items; later edits to
//     the catalog row AND later local renames never rewrite the frozen
//     invoice. Legacy rows (NULL snapshot) keep the documented live-read
//     fallback.
//
//  3. LIBRARY REMOVAL — importing a published library stamps provenance
//     (source_library_id) on the rows it CREATED. The removal preview
//     classifies honestly (has sales/returns/stock = keep, trace-free =
//     delete) and the execution deletes only the trace-free rows, detaches
//     the survivors, drops the sync pointer, and leaves the shared catalog
//     untouched.
//
// Run: TEST_DATABASE_URL=postgres://.../pharmacy_scratch go test ./internal/handlers/ -run 'TestProductOwnershipDB|TestSaleIdentitySnapshotDB|TestLibraryRemovalDB' -v

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pharmacy-os/backend/internal/auth"
	"github.com/pharmacy-os/backend/internal/money"
	"github.com/pharmacy-os/backend/internal/services"
)

type ownershipFixture struct {
	pool     *pgxpool.Pool
	h        *Handler
	account  string
	pharmacy string
	branch   string
	employee string
}

func ownershipConnect(t *testing.T) *ownershipFixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — ownership gates need a disposable PostgreSQL")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	f := &ownershipFixture{pool: pool, h: &Handler{db: pool}}

	ctx := context.Background()
	stamp := time.Now().UnixNano()
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO accounts (company_name, contact_email)
                VALUES ($1, $2) RETURNING id::text
        `, fmt.Sprintf("Own Account %d", stamp), fmt.Sprintf("own-%d@fixture.test", stamp),
	).Scan(&f.account))
	// country 'EG' makes the EG library visible to this pharmacy.
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacies (account_id, name, country)
                VALUES ($1::uuid, $2, 'EG') RETURNING id::text
        `, f.account, fmt.Sprintf("Own Pharmacy %d", stamp)).Scan(&f.pharmacy))
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO branches (pharmacy_id, name, code, is_active)
                VALUES ($1::uuid, 'main', $2, true) RETURNING id::text
        `, f.pharmacy, fmt.Sprintf("OWNB-%d", stamp)).Scan(&f.branch))
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO employees (account_id, pharmacy_id, branch_id, email, first_name, last_name, status, permissions_initialized)
                VALUES ($1::uuid, $2::uuid, $3::uuid, $4, 'own', 'fixture', 'active', TRUE) RETURNING id::text
        `, f.account, f.pharmacy, f.branch, fmt.Sprintf("own-emp-%d@fixture.test", stamp)).Scan(&f.employee))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1::uuid`, f.account)
	})
	return f
}

// newGlobalProduct inserts a catalog row and returns its id.
func (f *ownershipFixture) newGlobalProduct(t *testing.T, name, source string, verified bool) string {
	t.Helper()
	var id string
	require.NoError(t, f.pool.QueryRow(context.Background(), `
                INSERT INTO global_products (name, generic_name, dosage_form, strength, barcode,
                                             product_category, default_unit, is_active, source, is_verified)
                VALUES ($1, 'fixture generic', 'tablet', '500mg', NULL,
                        'medication', 'tablet', true, $2, $3)
                RETURNING id::text
        `, name, source, verified).Scan(&id))
	return id
}

// newPharmacyProduct links a catalog row into the pharmacy.
func (f *ownershipFixture) newPharmacyProduct(t *testing.T, gpID string, price int64) string {
	t.Helper()
	var id string
	require.NoError(t, f.pool.QueryRow(context.Background(), `
                INSERT INTO pharmacy_products (pharmacy_id, global_product_id, selling_price, cost_price, min_stock_level)
                VALUES ($1::uuid, $2::uuid, $3, 0, 0) RETURNING id::text
        `, f.pharmacy, gpID, price).Scan(&id))
	return id
}

// newOpeningBatch gives a product one stocked batch at the fixture branch.
func (f *ownershipFixture) newOpeningBatch(t *testing.T, ppID string, qty int64, num string) string {
	t.Helper()
	var id string
	require.NoError(t, f.pool.QueryRow(context.Background(), `
                INSERT INTO inventory_batches (pharmacy_product_id, branch_id, batch_number, quantity, unit, cost_per_unit, received_date)
                VALUES ($1::uuid, $2::uuid, $3, $4, 'box', 0, CURRENT_DATE) RETURNING id::text
        `, ppID, f.branch, num, qty).Scan(&id))
	return id
}

func (f *ownershipFixture) principal() *auth.Principal {
	return &auth.Principal{
		ID: f.employee, Type: auth.EmployeePrincipal,
		PharmacyID: f.pharmacy, BranchID: f.branch, IsActive: true, EmailVerified: true,
	}
}

// exec runs a fixture statement and fails the test on error.
func (f *ownershipFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	_, err := f.pool.Exec(context.Background(), sql, args...)
	require.NoError(t, err)
}

func (f *ownershipFixture) ginCtx(t *testing.T, method, path string, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = req
	// The auth middleware stores the principal under this literal key.
	ctx.Set("auth_principal", f.principal())
	return ctx, rec
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. Ownership: local overrides, catalog untouched.
// ─────────────────────────────────────────────────────────────────────────────

func TestProductOwnershipDB(t *testing.T) {
	f := ownershipConnect(t)
	ctx := context.Background()

	gpGov := f.newGlobalProduct(t, "OWN-OFFICIAL-PANADOL", "platform_admin", true)
	ppGov := f.newPharmacyProduct(t, gpGov, 2500)
	gpPlain := f.newGlobalProduct(t, "OWN-CROWD-OTHER", "pharmacy", false)
	ppOther := f.newPharmacyProduct(t, gpPlain, 1000)

	official := func(column string) string {
		var v string
		require.NoError(t, f.pool.QueryRow(ctx,
			fmt.Sprintf(`SELECT COALESCE(%s::text, '') FROM global_products WHERE id = $1::uuid`, column), gpGov).Scan(&v))
		return v
	}

	t.Run("governed_row_edit_writes_overrides_never_catalog", func(t *testing.T) {
		c, rec := f.ginCtx(t, http.MethodPut, "/pharmacy/products/"+ppGov, `{
                        "name": "بنادول إكسترا ٢٤",
                        "generic_name": "paracetamol",
                        "barcode": "6221048100111",
                        "strength": "500mg+65mg",
                        "dosage_form": "tablet",
                        "packaging_type": "WHOLE_ONLY",
                        "units_per_box": 1,
                        "cost_price_piastres": 2000,
                        "selling_price_piastres": 2500,
                        "min_stock_level": 3
                }`)
		c.Params = gin.Params{{Key: "id", Value: ppGov}}
		f.h.UpdatePharmacyProduct(c)
		require.Equal(t, http.StatusOK, rec.Code, "governed row edit must succeed now (T-B1 403 retired): %s", rec.Body.String())

		// Overrides landed.
		var nameOv, barcodeOv, strengthOv string
		require.NoError(t, f.pool.QueryRow(ctx, `
                        SELECT COALESCE(name_override, ''), COALESCE(barcode_override, ''), COALESCE(strength_override, '')
                        FROM pharmacy_products WHERE id = $1::uuid
                `, ppGov).Scan(&nameOv, &barcodeOv, &strengthOv))
		assert.Equal(t, "بنادول إكسترا ٢٤", nameOv)
		assert.Equal(t, "6221048100111", barcodeOv)
		assert.Equal(t, "500mg+65mg", strengthOv)

		// The shared catalog row is byte-identical to before the edit.
		assert.Equal(t, "OWN-OFFICIAL-PANADOL", official("name"))
		assert.Equal(t, "", official("barcode"), "catalog barcode must stay NULL — the local code lives in the override")
	})

	t.Run("get_returns_effective_identity", func(t *testing.T) {
		c, rec := f.ginCtx(t, http.MethodGet, "/pharmacy/products/"+ppGov, "")
		c.Params = gin.Params{{Key: "id", Value: ppGov}}
		f.h.GetPharmacyProduct(c)
		require.Equal(t, http.StatusOK, rec.Code)
		body := rec.Body.String()
		assert.Contains(t, body, "بنادول إكسترا ٢٤", "GET shows the local override")
		assert.Contains(t, body, "6221048100111", "GET shows the local barcode")
	})

	t.Run("effective_identity_flows_through_view_and_lookup", func(t *testing.T) {
		f.newOpeningBatch(t, ppGov, 10, "OWN-B1")

		var viewName, viewBarcode string
		require.NoError(t, f.pool.QueryRow(ctx, `
                        SELECT product_name, COALESCE(barcode, '') FROM current_inventory
                        WHERE pharmacy_product_id = $1::uuid LIMIT 1
                `, ppGov).Scan(&viewName, &viewBarcode))
		assert.Equal(t, "بنادول إكسترا ٢٤", viewName, "inventory shows the pharmacy's own naming")
		assert.Equal(t, "6221048100111", viewBarcode)

		// POS scan matches the override barcode and returns the override name.
		c, rec := f.ginCtx(t, http.MethodGet, "/pharmacy/pos/products?barcode=6221048100111", "")
		c.Request.URL.RawQuery = "barcode=6221048100111"
		f.h.LookupPOSProduct(c)
		require.Equal(t, http.StatusOK, rec.Code, "scan must resolve via the override: %s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), "بنادول إكسترا ٢٤")
	})

	t.Run("duplicate_local_barcode_conflicts", func(t *testing.T) {
		c, rec := f.ginCtx(t, http.MethodPut, "/pharmacy/products/"+ppOther, `{
                        "name": "OWN-CROWD-OTHER",
                        "generic_name": "fixture generic",
                        "barcode": "6221048100111",
                        "strength": "500mg",
                        "dosage_form": "tablet",
                        "packaging_type": "WHOLE_ONLY",
                        "units_per_box": 1,
                        "cost_price_piastres": 500,
                        "selling_price_piastres": 1000,
                        "min_stock_level": 0
                }`)
		c.Params = gin.Params{{Key: "id", Value: ppOther}}
		f.h.UpdatePharmacyProduct(c)
		require.Equal(t, http.StatusConflict, rec.Code, "second product cannot claim the same local barcode")
	})

	t.Run("price_only_edit_writes_nothing_identity", func(t *testing.T) {
		var beforeName, beforeBarcode *string
		require.NoError(t, f.pool.QueryRow(ctx, `
                        SELECT name_override, barcode_override FROM pharmacy_products WHERE id = $1::uuid
                `, ppOther).Scan(&beforeName, &beforeBarcode))

		c, rec := f.ginCtx(t, http.MethodPut, "/pharmacy/products/"+ppOther, `{
                        "name": "OWN-CROWD-OTHER",
                        "generic_name": "fixture generic",
                        "barcode": "",
                        "strength": "500mg",
                        "dosage_form": "tablet",
                        "packaging_type": "BOX_STRIP",
                        "units_per_box": 10,
                        "partial_selling_price_piastres": 120,
                        "cost_price_piastres": 500,
                        "selling_price_piastres": 1500,
                        "min_stock_level": 2
                }`)
		c.Params = gin.Params{{Key: "id", Value: ppOther}}
		f.h.UpdatePharmacyProduct(c)
		require.Equal(t, http.StatusOK, rec.Code, "price+packaging edit passes: %s", rec.Body.String())

		var afterName, afterBarcode *string
		require.NoError(t, f.pool.QueryRow(ctx, `
                        SELECT name_override, barcode_override FROM pharmacy_products WHERE id = $1::uuid
                `, ppOther).Scan(&afterName, &afterBarcode))
		assert.Equal(t, beforeName, afterName, "price-only edit must not mint a name override")
		assert.Equal(t, beforeBarcode, afterBarcode, "price-only edit must not mint a barcode override")

		var price int64
		require.NoError(t, f.pool.QueryRow(ctx, `SELECT selling_price::int8 FROM pharmacy_products WHERE id = $1::uuid`, ppOther).Scan(&price))
		assert.Equal(t, int64(1500), price)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Invoice identity snapshot.
// ─────────────────────────────────────────────────────────────────────────────

func TestSaleIdentitySnapshotDB(t *testing.T) {
	f := ownershipConnect(t)
	ctx := context.Background()

	gp := f.newGlobalProduct(t, "SNAP-ORIG-NAME", "platform_admin", true)
	pp := f.newPharmacyProduct(t, gp, 1200)
	batch := f.newOpeningBatch(t, pp, 50, "SNAP-B1")

	var saleID string
	require.NoError(t, f.pool.QueryRow(ctx, `
                INSERT INTO sales (pharmacy_id, branch_id, idempotency_key, employee_id, payment_type)
                VALUES ($1::uuid, $2::uuid, $3, $4::uuid, 'cash') RETURNING id::text
        `, f.pharmacy, f.branch, fmt.Sprintf("snap-sale-%d", time.Now().UnixNano()), f.employee).Scan(&saleID))

	// The pricing snapshot the sale request would carry: a LOCAL override is
	// already in place, so the effective identity is the override.
	f.exec(t, `
                UPDATE pharmacy_products SET name_override = 'اسمي المحلي', barcode_override = '6221000000009'
                WHERE id = $1::uuid
        `, pp)
	snap := services.POSPricingSnapshot{
		PackagingType: services.PackagingWholeOnly, UnitsPerBox: 1, BoxPrice: 1200,
		Name: "اسمي المحلي", Barcode: "6221000000009", Strength: "500mg",
	}
	tx, err := f.pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, services.PersistPOSItems(ctx, tx, f.principal(), f.branch, saleID,
		[]services.POSSaleItemRequest{{PharmacyProductID: pp, SaleUnit: "box", Quantity: 2}},
		[]services.POSPricingSnapshot{snap}, []money.Piastres{2400}))
	require.NoError(t, tx.Commit(ctx))

	var snapName, snapBarcode, snapStrength string
	require.NoError(t, f.pool.QueryRow(ctx, `
                SELECT COALESCE(product_name_snapshot, ''), COALESCE(product_barcode_snapshot, ''), COALESCE(product_strength_snapshot, '')
                FROM sale_items WHERE sale_id = $1::uuid LIMIT 1
        `, saleID).Scan(&snapName, &snapBarcode, &snapStrength))
	assert.Equal(t, "اسمي المحلي", snapName)
	assert.Equal(t, "6221000000009", snapBarcode)
	assert.Equal(t, "500mg", snapStrength)

	// Rewrite history everywhere a live read could look: the catalog row AND
	// the local override both change AFTER the sale.
	f.exec(t, `
                UPDATE global_products SET name = 'RENAMED-AFTER-SALE' WHERE id = $1::uuid
        `, gp)
	f.exec(t, `
                UPDATE pharmacy_products SET name_override = 'RELOCALIZED-AFTER-SALE', barcode_override = NULL
                WHERE id = $1::uuid
        `, pp)

	var frozenName, frozenBarcode string
	require.NoError(t, f.pool.QueryRow(ctx, `
                SELECT COALESCE(product_name_snapshot, ''), COALESCE(product_barcode_snapshot, '')
                FROM sale_items WHERE sale_id = $1::uuid LIMIT 1
        `, saleID).Scan(&frozenName, &frozenBarcode))
	assert.Equal(t, "اسمي المحلي", frozenName, "the invoice keeps the name it was created with")
	assert.Equal(t, "6221000000009", frozenBarcode)

	// The display lens reads the frozen snapshot first...
	var displayName string
	require.NoError(t, f.pool.QueryRow(ctx, `
                SELECT COALESCE(si.product_name_snapshot, pp.name_override, gp.name::text, '')
                FROM sale_items si
                JOIN pharmacy_products pp ON pp.id = si.pharmacy_product_id
                JOIN global_products gp ON gp.id = pp.global_product_id
                WHERE si.sale_id = $1::uuid LIMIT 1
        `, saleID).Scan(&displayName))
	assert.Equal(t, "اسمي المحلي", displayName)

	// ...while a legacy row (NULL snapshot, pre-migration-46) falls back to
	// the live read exactly as the documented contract promises.
	var legacySale string
	require.NoError(t, f.pool.QueryRow(ctx, `
                INSERT INTO sales (pharmacy_id, branch_id, idempotency_key, employee_id, payment_type)
                VALUES ($1::uuid, $2::uuid, $3, $4::uuid, 'cash') RETURNING id::text
        `, f.pharmacy, f.branch, fmt.Sprintf("snap-legacy-%d", time.Now().UnixNano()), f.employee).Scan(&legacySale))
	f.exec(t, `
                INSERT INTO sale_items (sale_id, pharmacy_product_id, batch_id, sale_unit, quantity, base_quantity, unit_price, amount_piastres)
                VALUES ($1::uuid, $2::uuid, $3::uuid, 'box', 1, 1, 1200, 1200)
        `, legacySale, pp, batch)
	require.NoError(t, f.pool.QueryRow(ctx, `
                SELECT COALESCE(si.product_name_snapshot, pp.name_override, gp.name::text, '')
                FROM sale_items si
                JOIN pharmacy_products pp ON pp.id = si.pharmacy_product_id
                JOIN global_products gp ON gp.id = pp.global_product_id
                WHERE si.sale_id = $1::uuid LIMIT 1
        `, legacySale).Scan(&displayName))
	assert.Equal(t, "RELOCALIZED-AFTER-SALE", displayName, "legacy row reads the CURRENT effective identity (live fallback)")
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Library removal: provenance + honest classification + safe execution.
// ─────────────────────────────────────────────────────────────────────────────

func TestLibraryRemovalDB(t *testing.T) {
	f := ownershipConnect(t)
	ctx := context.Background()
	stamp := time.Now().UnixNano()

	// Published EG library with two products at official prices.
	var libID string
	require.NoError(t, f.pool.QueryRow(ctx, `
                INSERT INTO product_libraries (name, description, country_code, currency, is_published, version, published_at)
                VALUES ($1, 'fixture', 'EG', 'EGP', true, 1, NOW()) RETURNING id::text
        `, fmt.Sprintf("Own Lib %d", stamp)).Scan(&libID))

	gpSold := f.newGlobalProduct(t, fmt.Sprintf("REM-SOLD-%d", stamp), "platform_admin", true)
	gpClean := f.newGlobalProduct(t, fmt.Sprintf("REM-CLEAN-%d", stamp), "platform_admin", true)
	gpManual := f.newGlobalProduct(t, fmt.Sprintf("REM-MANUAL-%d", stamp), "platform_admin", true)
	f.exec(t, `
                INSERT INTO library_products (library_id, global_product_id, official_price_piastres)
                VALUES ($1::uuid, $2::uuid, 2500), ($1::uuid, $3::uuid, 1800)
        `, libID, gpSold, gpClean)

	// The import flow itself: both library products become pharmacy rows
	// stamped with the library provenance.
	c, rec := f.ginCtx(t, http.MethodPost, "/pharmacy/libraries/"+libID+"/import/execute", fmt.Sprintf(`{
                "items": [
                        {"global_product_id": %q, "action": "create"},
                        {"global_product_id": %q, "action": "create"}
                ]
        }`, gpSold, gpClean))
	c.Params = gin.Params{{Key: "id", Value: libID}}
	f.h.ExecutePharmacyLibraryImport(c)
	require.Equal(t, http.StatusOK, rec.Code, "import must succeed: %s", rec.Body.String())

	var ppSold, ppClean string
	require.NoError(t, f.pool.QueryRow(ctx, `
                SELECT id::text FROM pharmacy_products WHERE pharmacy_id = $1::uuid AND global_product_id = $2::uuid
        `, f.pharmacy, gpSold).Scan(&ppSold))
	require.NoError(t, f.pool.QueryRow(ctx, `
                SELECT id::text FROM pharmacy_products WHERE pharmacy_id = $1::uuid AND global_product_id = $2::uuid
        `, f.pharmacy, gpClean).Scan(&ppClean))
	var provenanceCount int
	require.NoError(t, f.pool.QueryRow(ctx, `
                SELECT COUNT(*) FROM pharmacy_products
                WHERE pharmacy_id = $1::uuid AND source_library_id = $2::uuid
        `, f.pharmacy, libID).Scan(&provenanceCount))
	require.Equal(t, 2, provenanceCount, "both imported rows carry the library provenance")

	// A product the pharmacy owned BEFORE the library — no provenance, and a
	// library removal must never even look at it.
	ppManual := f.newPharmacyProduct(t, gpManual, 900)

	// The SOLD product gets the operational trace: stock + an invoice line
	// (sale_items RESTRICTs deletion at the database level — the hard core).
	f.newOpeningBatch(t, ppSold, 10, "REM-B1")
	var saleID, batchID string
	require.NoError(t, f.pool.QueryRow(ctx, `
                SELECT id::text FROM inventory_batches WHERE pharmacy_product_id = $1::uuid LIMIT 1
        `, ppSold).Scan(&batchID))
	require.NoError(t, f.pool.QueryRow(ctx, `
                INSERT INTO sales (pharmacy_id, branch_id, idempotency_key, employee_id, payment_type)
                VALUES ($1::uuid, $2::uuid, $3, $4::uuid, 'cash') RETURNING id::text
        `, f.pharmacy, f.branch, fmt.Sprintf("rem-sale-%d", stamp), f.employee).Scan(&saleID))
	f.exec(t, `
                INSERT INTO sale_items (sale_id, pharmacy_product_id, batch_id, sale_unit, quantity, base_quantity, unit_price, amount_piastres)
                VALUES ($1::uuid, $2::uuid, $3::uuid, 'box', 2, 2, 2500, 5000)
        `, saleID, ppSold, batchID)

	// Preview: sold → keep, clean → delete, and the manual product is absent.
	c, rec = f.ginCtx(t, http.MethodPost, "/pharmacy/libraries/"+libID+"/remove/preview", "")
	c.Params = gin.Params{{Key: "id", Value: libID}}
	f.h.PreviewPharmacyLibraryRemoval(c)
	require.Equal(t, http.StatusOK, rec.Code, "preview failed: %s", rec.Body.String())
	previewBody := rec.Body.String()
	assert.Contains(t, previewBody, `"will_delete"`)
	assert.Contains(t, previewBody, `"count":1`)
	assert.Contains(t, previewBody, fmt.Sprintf("%q", ppClean), "the trace-free library product is slated for deletion")

	// Execute.
	c, rec = f.ginCtx(t, http.MethodPost, "/pharmacy/libraries/"+libID+"/remove/execute", "")
	c.Params = gin.Params{{Key: "id", Value: libID}}
	f.h.ExecutePharmacyLibraryRemoval(c)
	require.Equal(t, http.StatusOK, rec.Code, "execute failed: %s", rec.Body.String())

	var cnt int
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM pharmacy_products WHERE id = $1::uuid`, ppClean).Scan(&cnt))
	assert.Zero(t, cnt, "trace-free library product is deleted")

	require.NoError(t, f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM pharmacy_products WHERE id = $1::uuid`, ppSold).Scan(&cnt))
	assert.Equal(t, 1, cnt, "product with sales survives")

	var provenance *string
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT source_library_id::text FROM pharmacy_products WHERE id = $1::uuid`, ppSold).Scan(&provenance))
	assert.Nil(t, provenance, "survivor is detached from the library — a plain owned product now")

	require.NoError(t, f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM pharmacy_products WHERE id = $1::uuid`, ppManual).Scan(&cnt))
	assert.Equal(t, 1, cnt, "pharmacy-owned product predating the library is untouched")

	require.NoError(t, f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM global_products WHERE id = $1::uuid`, gpClean).Scan(&cnt))
	assert.Equal(t, 1, cnt, "the shared catalog row survives — other pharmacies unaffected")

	require.NoError(t, f.pool.QueryRow(ctx, `
                SELECT COUNT(*) FROM pharmacy_library_syncs WHERE pharmacy_id = $1::uuid AND library_id = $2::uuid
        `, f.pharmacy, libID).Scan(&cnt))
	assert.Zero(t, cnt, "sync pointer dropped — the library reads as not imported again")
}
