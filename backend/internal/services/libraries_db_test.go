// P10 regression gates for ledger item 3 (ADR-0005, api_level 110): the
// central libraries moved out of platform_libraries_products_handler.go and
// pharmacy_libraries_handler.go. THESE suites pin the ORCHESTRATION the
// services now own, against the real migrated schema (1→44):
//
//   - platform half: verified catalog creation with the barcode-collision
//     dedup, the entry upsert (added / price_updated / unchanged) with its
//     change-log rows, the whole-file import (barcode-first, then
//     normalized-name, in-file dedupe, brand-new products born verified),
//     the manual price edit vocabulary, and the metadata_changed
//     propagation into EVERY containing library with the draft-version
//     semantics (unpublished tags the current version, published tags
//     version+1),
//   - pharmacy half: the country visibility predicate, the three-gated
//     import (create / exists-price-restore / explicit link, plan-limit
//     skips, no_official_price skips, not_in_library), the red lines (zero
//     official price never nulls a pharmacy price, «removed» is
//     informational, cost & stock untouched), the one-tap pull (price
//     updates, confident-twin linking by barcode, sync pointer GREATEST),
//     and the tenant audit rows written inside the service transaction
//     (auditlog.Tenant).
//
// Every business rejection here maps 1:1 onto the wire body the handlers
// emit (respondLibrariesFailure / respondPharmacyLibraryFailure) — the
// protocol branches are pinned by the source-scan gates
// TestLibraryPlatformContract / TestLibraryPharmacyContract.
//
// Same harness as the other DB gates:
//
//	TEST_DATABASE_URL=postgres://.../pharmacy_scratch go test ./internal/services/ -run 'TestLibraryPlatformServiceDB|TestLibraryPharmacyServiceDB'
package services

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pharmacy-os/backend/internal/auth"
)

func int64p(v int64) *int64 { return &v }

func strp(v string) *string { return &v }

// makeLibrary inserts a product_libraries row and returns its id.
func makeLibrary(t *testing.T, ctx context.Context, pool *pgxpool.Pool, suffix, name, country string, published bool) string {
	t.Helper()
	var id string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO product_libraries (name, description, country_code, currency, is_published, version, published_at)
		VALUES ($1, 'p10 gate', NULLIF($2, ''), 'EGP', $3, 1, CASE WHEN $3 THEN NOW() ELSE NULL END)
		RETURNING id::text
	`, name, country, published).Scan(&id))
	return id
}

// makeCatalogProduct inserts a verified global_products row and returns its id.
func makeCatalogProduct(t *testing.T, ctx context.Context, pool *pgxpool.Pool, suffix, name, barcode string) string {
	t.Helper()
	var id string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO global_products (name, dosage_form, product_category, requires_prescription, barcode, is_verified, source)
		VALUES ($1, 'tablet', 'medication', 'no', NULLIF($2, ''), true, 'platform_admin')
		RETURNING id::text
	`, name, barcode).Scan(&id))
	return id
}

func makeLibraryEntry(t *testing.T, ctx context.Context, pool *pgxpool.Pool, libraryID, gpID string, price int64) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO library_products (library_id, global_product_id, official_price_piastres)
		VALUES ($1::uuid, $2::uuid, $3)
	`, libraryID, gpID, price)
	require.NoError(t, err)
}

func entryIDOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, libraryID, gpID string) string {
	t.Helper()
	var id string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id::text FROM library_products WHERE library_id = $1::uuid AND global_product_id = $2::uuid`,
		libraryID, gpID).Scan(&id))
	return id
}

func TestLibraryPlatformServiceDB(t *testing.T) {
	ctx := context.Background()
	pool := platformStatsPool(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	platform := &auth.Principal{
		ID: "p10-platform-actor", Email: "platform@p10.test",
		DisplayName: "P10 Platform", Role: "super_admin",
	}

	libraryID := makeLibrary(t, ctx, pool, "p10plat-lib-", "EGP Master "+suffix, "", false)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM product_libraries WHERE description = 'p10 gate'`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM global_products WHERE name LIKE 'P10 %'`)
	})

	// ---- 1) catalog creation is verified + platform_admin sourced --------
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	np := NewCatalogProduct{Name: "P10 Gate Drug", DosageForm: "tablet", ProductCategory: "medication",
		RequiresPrescription: "no", GenerateBarcode: true}
	gpID, err := CreateCatalogProduct(ctx, tx, &np)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	var source, barcode string
	var verified bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT source, is_verified, COALESCE(barcode, '') FROM global_products WHERE id = $1::uuid`, gpID,
	).Scan(&source, &verified, &barcode))
	assert.Equal(t, "platform_admin", source)
	assert.True(t, verified)
	assert.Len(t, barcode, 13, "generated RCN EAN-13")

	// ---- 2) barcode collision — the frozen (dead-branch) reality ----------
	// The comment on CreateCatalogProduct promises "collision → link", but
	// inside ONE transaction a failed INSERT aborts the statement chain
	// (PostgreSQL aborts the whole tx on error), so the fallback SELECT can
	// never succeed — the collision path answers the original error instead.
	// MOVED VERBATIM per ADR-0005 rule 3 (no wire change during extraction):
	// this gate pins the CURRENT behavior and the dead branch is flagged for
	// a future savepoint-based semantic fix, NOT silently changed here.
	tx, err = pool.Begin(ctx)
	require.NoError(t, err)
	dup := NewCatalogProduct{Name: "Duplicate By Barcode", DosageForm: "tablet", ProductCategory: "medication",
		RequiresPrescription: "no", Barcode: barcode}
	_, dupErr := CreateCatalogProduct(ctx, tx, &dup)
	assert.Error(t, dupErr, "the collision fallback is a dead branch inside one tx — pinned as-is (frozen wire)")
	require.NoError(t, tx.Rollback(ctx))

	// ---- 3) AddLibraryProduct: added → price_updated → unchanged ----------
	out, err := AddLibraryProduct(ctx, pool, platform, libraryID, AddLibraryProductInput{
		GlobalProductID: gpID, OfficialPricePiastres: int64p(1500),
	})
	require.NoError(t, err)
	assert.Equal(t, "added", out.Action)
	out, err = AddLibraryProduct(ctx, pool, platform, libraryID, AddLibraryProductInput{
		GlobalProductID: gpID, OfficialPricePiastres: int64p(2000),
	})
	require.NoError(t, err)
	assert.Equal(t, "price_updated", out.Action)
	var price int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT official_price_piastres FROM library_products WHERE library_id = $1::uuid AND global_product_id = $2::uuid`,
		libraryID, gpID).Scan(&price))
	assert.Equal(t, int64(2000), price)
	out, err = AddLibraryProduct(ctx, pool, platform, libraryID, AddLibraryProductInput{
		GlobalProductID: gpID, OfficialPricePiastres: int64p(2000),
	})
	require.NoError(t, err)
	assert.Equal(t, "unchanged", out.Action)

	var changes int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM library_changes WHERE library_id = $1::uuid AND global_product_id = $2::uuid`,
		libraryID, gpID).Scan(&changes))
	assert.Equal(t, 2, changes, "added + price_changed change-log rows")
	var platformAudits int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM platform_audit_logs WHERE action = 'library.product_add' AND entity_id = $1`,
		libraryID+"/"+gpID).Scan(&platformAudits))
	assert.Equal(t, 3, platformAudits, "all three upsert outcomes (added/price_updated/unchanged) are audited — original behavior")

	// ---- 4) foreign catalog id is a typed 404, and the tx rolled back -----
	_, err = AddLibraryProduct(ctx, pool, platform, libraryID, AddLibraryProductInput{
		GlobalProductID: "00000000-0000-0000-0000-0000000000aa",
	})
	assert.ErrorIs(t, err, ErrCatalogProductNotFound)
	var entries int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM library_products WHERE library_id = $1::uuid`, libraryID).Scan(&entries))
	assert.Equal(t, 1, entries, "the failed add left no partial rows")

	// ---- 5) manual price edit logs the exact manual summary ---------------
	changed, err := UpdateLibraryProduct(ctx, pool, platform, libraryID, entryIDOf(t, ctx, pool, libraryID, gpID), UpdateLibraryProductInput{
		OfficialPricePiastres: int64p(2500),
	})
	require.NoError(t, err)
	assert.True(t, changed)
	var summary string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT summary FROM library_changes WHERE library_id = $1::uuid ORDER BY created_at DESC LIMIT 1`,
		libraryID).Scan(&summary))
	assert.Equal(t, "تعديل يدوي للسعر الرسمي", summary)

	// ---- 6) metadata fix propagates to EVERY containing library with the
	// draft-version semantics (unpublished tags version, published tags v+1)
	publishedLib := makeLibrary(t, ctx, pool, "p10plat-pub-", "Published Lib "+suffix, "EG", true)
	_, err = pool.Exec(ctx,
		`UPDATE product_libraries SET version = 3 WHERE id = $1::uuid`, publishedLib)
	require.NoError(t, err)
	makeLibraryEntry(t, ctx, pool, publishedLib, gpID, 3000)
	err = UpdateCatalogProduct(ctx, pool, platform, gpID, CatalogProductUpdate{
		Name: strp("P10 Gate Drug — renamed"),
	})
	require.NoError(t, err)
	var draftRows, pubRows int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM library_changes WHERE global_product_id = $1::uuid AND library_id = $2::uuid AND change_type = 'metadata_changed' AND version = 1`,
		gpID, libraryID).Scan(&draftRows))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM library_changes WHERE global_product_id = $1::uuid AND library_id = $2::uuid AND change_type = 'metadata_changed' AND version = 4`,
		gpID, publishedLib).Scan(&pubRows))
	assert.Equal(t, 1, draftRows, "unpublished library drafts the CURRENT version")
	assert.Equal(t, 1, pubRows, "published library drafts version+1")

	// ---- 6b) a no-op catalog save propagates NOTHING (Task 166 audit) ----
	// The UI round-trips unchanged values; every such save used to append
	// metadata_changed rows into EVERY containing library, inflating
	// pending_changes and enabling an «empty release» on the next publish.
	var metaBefore int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM library_changes WHERE global_product_id = $1::uuid AND change_type = 'metadata_changed'`,
		gpID).Scan(&metaBefore))
	require.Equal(t, 2, metaBefore, "station 6 left one row per containing library")
	err = UpdateCatalogProduct(ctx, pool, platform, gpID, CatalogProductUpdate{
		Name: strp("P10 Gate Drug — renamed"), // the exact value station 6 stored
	})
	require.NoError(t, err)
	var metaAfter int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM library_changes WHERE global_product_id = $1::uuid AND change_type = 'metadata_changed'`,
		gpID).Scan(&metaAfter))
	assert.Equal(t, metaBefore, metaAfter, "a no-op save appends zero metadata_changed rows")

	// one REAL field change still propagates into both containing libraries
	err = UpdateCatalogProduct(ctx, pool, platform, gpID, CatalogProductUpdate{
		StorageInstructions: strp("p166 keep below 30 C"),
	})
	require.NoError(t, err)
	var metaReal int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM library_changes WHERE global_product_id = $1::uuid AND change_type = 'metadata_changed'`,
		gpID).Scan(&metaReal))
	assert.Equal(t, metaBefore+2, metaReal, "a real change reaches every containing library")

	// ---- 7) import execution: barcode-first, name-second, born-verified ---
	known := makeCatalogProduct(t, ctx, pool, "p10plat-known-", "P10 Known Drug", "1111111111116")
	// The entry must ALREADY exist in the library for the barcode row to
	// count as price_updated (otherwise the import adds it).
	makeLibraryEntry(t, ctx, pool, libraryID, known, 1500)
	report, err := ExecuteLibraryImport(ctx, pool, platform, libraryID, []LibraryImportRow{
		{Name: "P10 Known Drug", Barcode: "1111111111116", PricePiastres: 1800, HasPrice: true},
		{Name: "p10   known drug", PricePiastres: 1800, HasPrice: true}, // normalized-name match → same product
		{Name: "P10 Brand New", DosageForm: "syrup", PricePiastres: 900, HasPrice: true, ManufacturerName: "P10 Pharma"},
	}, 3, nil)
	require.NoError(t, err)
	assert.Equal(t, 3, report.TotalRows)
	assert.Equal(t, 1, report.PriceUpdated, "barcode row updates the existing entry's price")
	assert.Equal(t, 1, report.Unchanged, "normalized-name row is an in-file dedupe → unchanged")
	assert.Equal(t, 1, report.ProductsCreated, "brand-new row creates a verified catalog product")
	assert.Equal(t, 1, report.Added)
	assert.Nil(t, report.Errors, "clean import keeps the wire's null errors")
	var newVerified bool
	var newSource string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT is_verified, source FROM global_products WHERE name = 'P10 Brand New'`).Scan(&newVerified, &newSource))
	assert.True(t, newVerified)
	assert.Equal(t, "platform_admin", newSource)
}

func TestLibraryPharmacyServiceDB(t *testing.T) {
	ctx := context.Background()
	pool := platformStatsPool(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	// ---- fixtures: account → pharmacy (EG) → branch → employee ------------
	var accountID, pharmacyID, branchID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO accounts (company_name, contact_email, status)
		VALUES ($1, $2, 'active') RETURNING id::text
	`, "p10ph-gate "+suffix, "p10ph-"+suffix+"@test.local").Scan(&accountID))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, accountID)
	})
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO pharmacies (account_id, name, license_number, is_active, country)
		VALUES ($1, $2, $3, true, 'EG') RETURNING id::text
	`, accountID, "p10ph-gate-ph "+suffix, "P10-"+suffix).Scan(&pharmacyID))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO branches (pharmacy_id, name, code, is_active)
		VALUES ($1, 'main', $2, true) RETURNING id::text
	`, pharmacyID, "P10B-"+suffix).Scan(&branchID))
	var employeeID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO employees (account_id, pharmacy_id, branch_id, email, first_name, last_name, status, permissions_initialized)
		VALUES ($1, $2, $3, $4, 'p10', 'gate', 'active', TRUE) RETURNING id::text
	`, accountID, pharmacyID, branchID, "p10ph-emp-"+suffix+"@test.local").Scan(&employeeID))
	employee := &auth.Principal{
		ID: employeeID, Type: auth.EmployeePrincipal,
		PharmacyID: pharmacyID, IsActive: true, EmailVerified: true,
		Email: "p10ph-emp-" + suffix + "@test.local", DisplayName: "p10 gate",
	}

	var _err_ error
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM product_libraries WHERE description = 'p10 gate'`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM global_products WHERE name LIKE 'P10 %'`)
	})
	libraryID := makeLibrary(t, ctx, pool, "p10ph-lib-", "EGP Retail "+suffix, "EG", true)
	_, _err_ = pool.Exec(ctx, `UPDATE product_libraries SET version = 2 WHERE id = $1::uuid`, libraryID)
	require.NoError(t, _err_)

	gpA := makeCatalogProduct(t, ctx, pool, "p10ph-a-", "P10 Alpha", "2222222222224")
	gpB := makeCatalogProduct(t, ctx, pool, "p10ph-b-", "P10 Beta", "3333333333332")
	gpC := makeCatalogProduct(t, ctx, pool, "p10ph-c-", "P10 Gamma", "")
	makeLibraryEntry(t, ctx, pool, libraryID, gpA, 1000)
	makeLibraryEntry(t, ctx, pool, libraryID, gpB, 0) // missing data — never nulls a pharmacy price
	makeLibraryEntry(t, ctx, pool, libraryID, gpC, 500)

	unlimited := PlanState{Used: 0, Limit: -1}

	// ---- 1) visibility: country match yes, foreign country no -------------
	libs, err := ListPharmacyLibraries(ctx, pool, pharmacyID)
	require.NoError(t, err)
	// Task 167: the seeded Egyptian library («الدليل الدوائي المصري») is
	// visible to EVERY EG pharmacy — the catalog is no longer empty. Locate
	// THIS test's library by id instead of counting the whole list.
	require.GreaterOrEqual(t, len(libs), 1)
	var mine *PharmacyLibraryRow
	for i := range libs {
		if libs[i].ID == libraryID {
			mine = &libs[i]
		}
	}
	require.NotNil(t, mine, "the test's own EG library must be visible")
	assert.Equal(t, "not_imported", mine.SyncStatus)

	saLib := makeLibrary(t, ctx, pool, "p10ph-sa-", "SA Only "+suffix, "SA", true)
	_, err = GetPharmacyLibrary(ctx, pool, pharmacyID, saLib)
	assert.ErrorIs(t, err, ErrLibraryNotVisible)

	// ---- 2) import: create + zero-price skip + unknown skip ---------------
	out, err := ExecutePharmacyImport(ctx, pool, employee, pharmacyID, libraryID, []PharmacyImportItem{
		{GlobalProductID: gpA, Action: "create"},
		{GlobalProductID: gpB, Action: "create"},
		{GlobalProductID: "00000000-0000-0000-0000-0000000000bb", Action: "create"},
	}, unlimited)
	require.NoError(t, err)
	assert.Equal(t, 2, out.LibraryVersion)
	assert.Equal(t, 1, out.Applied)
	assert.Equal(t, 1, out.ProductsCreated)
	var sellingPrice, costPrice int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT selling_price, cost_price FROM pharmacy_products WHERE pharmacy_id = $1::uuid AND global_product_id = $2::uuid`,
		pharmacyID, gpA).Scan(&sellingPrice, &costPrice))
	assert.Equal(t, int64(1000), sellingPrice)
	assert.Equal(t, int64(0), costPrice, "cost belongs to the pharmacy — never written")
	require.Len(t, out.Skipped, 2)
	assert.Equal(t, "no_official_price", out.Skipped[0].Reason)
	assert.Equal(t, "not_in_library", out.Skipped[1].Reason)
	var syncVersion int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT last_synced_version FROM pharmacy_library_syncs WHERE pharmacy_id = $1::uuid AND library_id = $2::uuid`,
		pharmacyID, libraryID).Scan(&syncVersion))
	assert.Equal(t, 2, syncVersion, "import moves the pointer to the current version")

	// ---- 3) exists case: re-import restores the official price only -------
	_, _err_ = pool.Exec(ctx,
		`UPDATE pharmacy_products SET selling_price = 777 WHERE pharmacy_id = $1::uuid AND global_product_id = $2::uuid`,
		pharmacyID, gpA)
	require.NoError(t, _err_)
	out, err = ExecutePharmacyImport(ctx, pool, employee, pharmacyID, libraryID, []PharmacyImportItem{
		{GlobalProductID: gpA, Action: "create"},
	}, unlimited)
	require.NoError(t, err)
	assert.Equal(t, 1, out.PricesUpdated)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT selling_price FROM pharmacy_products WHERE pharmacy_id = $1::uuid AND global_product_id = $2::uuid`,
		pharmacyID, gpA).Scan(&sellingPrice))
	assert.Equal(t, int64(1000), sellingPrice, "the pull restores the official value")

	// ---- 4) explicit link to a foreign/missing row is skipped, not fatal --
	out, err = ExecutePharmacyImport(ctx, pool, employee, pharmacyID, libraryID, []PharmacyImportItem{
		{GlobalProductID: gpC, Action: "link", PharmacyProductID: strp("00000000-0000-0000-0000-0000000000cc")},
	}, unlimited)
	require.NoError(t, err)
	require.Len(t, out.Skipped, 1)
	assert.Equal(t, "link_target_invalid", out.Skipped[0].Reason)

	// ---- 5) plan limit: one remaining slot, two creates → one skip --------
	gpD := makeCatalogProduct(t, ctx, pool, "p10ph-d-", "P10 Delta", "")
	gpE := makeCatalogProduct(t, ctx, pool, "p10ph-e-", "P10 Epsilon", "")
	makeLibraryEntry(t, ctx, pool, libraryID, gpD, 700)
	makeLibraryEntry(t, ctx, pool, libraryID, gpE, 800)
	out, err = ExecutePharmacyImport(ctx, pool, employee, pharmacyID, libraryID, []PharmacyImportItem{
		{GlobalProductID: gpD, Action: "create"},
		{GlobalProductID: gpE, Action: "create"},
	}, PlanState{Used: 0, Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, 1, out.ProductsCreated)
	require.Len(t, out.Skipped, 1)
	assert.Equal(t, "plan_limit", out.Skipped[0].Reason)
	assert.Equal(t, "P10 Epsilon", out.Skipped[0].ProductName)

	// ---- 6) sync: not-imported guard (a VISIBLE library never imported),
	// nothing-to-do, price pull, red line. The SA library answers
	// library_not_visible BEFORE the not-imported guard (visibility is
	// part of the same locked lookup).
	neverImported := makeLibrary(t, ctx, pool, "p10ph-never-", "EGP Fresh "+suffix, "EG", true)
	_, err = SyncPharmacyLibrary(ctx, pool, employee, pharmacyID, neverImported, unlimited)
	assert.ErrorIs(t, err, ErrLibraryNotImported)

	syncOut, err := SyncPharmacyLibrary(ctx, pool, employee, pharmacyID, libraryID, unlimited)
	require.NoError(t, err)
	assert.True(t, syncOut.NothingToDo, "pointer already at the library version")

	// price_changed since the pointer → the OWNED product's price follows.
	_, _err_ = pool.Exec(ctx,
		`UPDATE library_products SET official_price_piastres = 1200 WHERE library_id = $1::uuid AND global_product_id = $2::uuid`,
		libraryID, gpA)
	require.NoError(t, _err_)
	_, _err_ = pool.Exec(ctx,
		`INSERT INTO library_changes (library_id, global_product_id, version, change_type, old_price_piastres, new_price_piastres, summary)
		 VALUES ($1::uuid, $2::uuid, 3, 'price_changed', 1000, 1200, 'p10 gate price change')`,
		libraryID, gpA)
	require.NoError(t, _err_)
	// removed → INFORMATIONAL only: the pharmacy KEEPS its imported product
	// (gpD WAS imported in station 5 — the platform then pulled it from the
	// library; the pull must report it and never delete the pharmacy's row).
	_, _err_ = pool.Exec(ctx,
		`INSERT INTO library_changes (library_id, global_product_id, version, change_type, summary)
		 VALUES ($1::uuid, $2::uuid, 3, 'removed', 'p10 gate removal')`,
		libraryID, gpD)
	require.NoError(t, _err_)
	_, _err_ = pool.Exec(ctx, `DELETE FROM library_products WHERE library_id = $1::uuid AND global_product_id = $2::uuid`, libraryID, gpD)
	require.NoError(t, _err_)
	_, _err_ = pool.Exec(ctx, `UPDATE product_libraries SET version = 3 WHERE id = $1::uuid`, libraryID)
	require.NoError(t, _err_)

	syncOut, err = SyncPharmacyLibrary(ctx, pool, employee, pharmacyID, libraryID, unlimited)
	require.NoError(t, err)
	assert.False(t, syncOut.NothingToDo)
	assert.Equal(t, 1, syncOut.PricesUpdated, "official price change pulled onto the owned product")
	var gpAPrice int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT selling_price FROM pharmacy_products WHERE pharmacy_id = $1::uuid AND global_product_id = $2::uuid`,
		pharmacyID, gpA).Scan(&gpAPrice))
	assert.Equal(t, int64(1200), gpAPrice)
	require.Len(t, syncOut.Removed, 1)
	assert.Equal(t, gpD, syncOut.Removed[0].GlobalProductID)
	var stillOwned int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM pharmacy_products WHERE pharmacy_id = $1::uuid AND global_product_id = $2::uuid`,
		pharmacyID, gpD).Scan(&stillOwned))
	assert.Equal(t, 1, stillOwned, "RED LINE: «removed» from a library never deletes the pharmacy's product")

	// ---- 7) added since the pointer with a confident twin → LINK, not dup -
	// Global barcodes are UNIQUE, so the pharmacy's twin carries a DIFFERENT
	// catalog row whose NAME contains the library product's name
	// (name_contains → confident twin) — the sync links it instead of
	// creating a duplicate.
	gpTwin := makeCatalogProduct(t, ctx, pool, "p10ph-twin-", "P10 Beta XL", "")
	_, _err_ = pool.Exec(ctx, `
		INSERT INTO pharmacy_products (pharmacy_id, global_product_id, selling_price, cost_price, min_stock_level)
		VALUES ($1::uuid, $2::uuid, 555, 0, 0)
	`, pharmacyID, gpTwin)
	require.NoError(t, _err_)
	// gpB was skipped at import (zero official price) but its library entry
	// exists — give it a real price now so the added change can link it.
	_, _err_ = pool.Exec(ctx,
		`UPDATE library_products SET official_price_piastres = 444 WHERE library_id = $1::uuid AND global_product_id = $2::uuid`,
		libraryID, gpB)
	require.NoError(t, _err_)
	_, _err_ = pool.Exec(ctx,
		`INSERT INTO library_changes (library_id, global_product_id, version, change_type, summary)
		 VALUES ($1::uuid, $2::uuid, 4, 'added', 'p10 gate re-add')`,
		libraryID, gpB)
	require.NoError(t, _err_)
	_, _err_ = pool.Exec(ctx, `UPDATE product_libraries SET version = 4 WHERE id = $1::uuid`, libraryID)
	require.NoError(t, _err_)

	syncOut, err = SyncPharmacyLibrary(ctx, pool, employee, pharmacyID, libraryID, unlimited)
	require.NoError(t, err)
	assert.Equal(t, 1, syncOut.ProductsLinked, "barcode-identical twin is linked, never duplicated")
	var linkedCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM pharmacy_products WHERE pharmacy_id = $1::uuid AND global_product_id = $2::uuid`,
		pharmacyID, gpB).Scan(&linkedCount))
	assert.Equal(t, 1, linkedCount, "the twin row now points at gpB")
	var twinPrice int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT pp.selling_price FROM pharmacy_products pp JOIN global_products g ON g.id = pp.global_product_id
		 WHERE pp.pharmacy_id = $1::uuid AND g.barcode = '3333333333332'`,
		pharmacyID).Scan(&twinPrice))
	assert.Equal(t, int64(444), twinPrice, "linked twin takes the official price")

	// ---- 8) tenant audit rows were written inside the service tx ----------
	var audits int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_logs WHERE pharmacy_id = $1::uuid AND action IN ('libraries.import','libraries.sync')`,
		pharmacyID).Scan(&audits))
	assert.GreaterOrEqual(t, audits, 4, "imports + syncs audited through auditlog.Tenant")

	// ---- 9) the whole flow never created inventory rows (red line) --------
	var batches int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM inventory_batches b JOIN branches br ON br.id = b.branch_id WHERE br.pharmacy_id = $1::uuid`,
		pharmacyID).Scan(&batches))
	assert.Equal(t, 0, batches, "importing from a library creates zero inventory rows")
}

// TestLibrarySyncConflictingBarcodeGuardDB — Task 166 audit gate: the sync's
// name-similarity twin path must never re-point a pharmacy row that carries
// its OWN manufacturer barcode. Two different barcodes are stronger
// evidence of two different drugs than a similar name is of one drug — the
// old query linked on name alone and silently turned a real product into
// another drug (wrong identity, wrong official price). Barcode-less rows
// keep the shipped linking behavior (station 7 of the main pharmacy gate).
func TestLibrarySyncConflictingBarcodeGuardDB(t *testing.T) {
	ctx := context.Background()
	pool := platformStatsPool(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	var accountID, pharmacyID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO accounts (company_name, contact_email, status)
		VALUES ($1, $2, 'active') RETURNING id::text
	`, "p166-guard "+suffix, "p166-"+suffix+"@test.local").Scan(&accountID))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, accountID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM product_libraries WHERE description = 'p166 gate'`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM global_products WHERE name LIKE 'P166 %'`)
	})
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO pharmacies (account_id, name, license_number, is_active, country)
		VALUES ($1, $2, $3, true, 'EG') RETURNING id::text
	`, accountID, "p166-guard-ph "+suffix, "P166-"+suffix).Scan(&pharmacyID))

	libraryID := makeLibrary(t, ctx, pool, "p166-lib-", "P166 Lib "+suffix, "EG", true)
	_, err := pool.Exec(ctx, `UPDATE product_libraries SET version = 2 WHERE id = $1::uuid`, libraryID)
	require.NoError(t, err)

	// The library publishes «P166 Same Drug» WITH its own manufacturer
	// barcode; the pharmacy already owns «P166 Same Drug Plus» with a
	// DIFFERENT barcode — names are trgm-near (containment-grade), barcodes
	// disagree. The guard must keep them separate.
	gpLib := makeCatalogProduct(t, ctx, pool, "p166-libp-", "P166 Same Drug", "4444444444441")
	gpOwn := makeCatalogProduct(t, ctx, pool, "p166-own-", "P166 Same Drug Plus", "5555555555556")
	makeLibraryEntry(t, ctx, pool, libraryID, gpLib, 1000)
	var sim float64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT similarity('P166 Same Drug Plus', 'P166 Same Drug')`).Scan(&sim))
	require.GreaterOrEqual(t, sim, NameAutoLinkThreshold,
		"fixture names must clear the auto-link threshold — otherwise this gate proves nothing")

	var ownPrice int64 = 900
	_, err = pool.Exec(ctx, `
		INSERT INTO pharmacy_products (pharmacy_id, global_product_id, selling_price, cost_price, min_stock_level)
		VALUES ($1::uuid, $2::uuid, $3, 0, 0)
	`, pharmacyID, gpOwn, ownPrice)
	require.NoError(t, err)

	// The added change reaches the pharmacy's sync window. The pointer sits
	// at v1 (a past import) so the v2 «added» row is inside the pull.
	_, err = pool.Exec(ctx, `
		INSERT INTO pharmacy_library_syncs (pharmacy_id, library_id, last_synced_version)
		VALUES ($1::uuid, $2::uuid, 1)
	`, pharmacyID, libraryID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO library_changes (library_id, global_product_id, version, change_type, summary)
		VALUES ($1::uuid, $2::uuid, 2, 'added', 'p166 guard add')
	`, libraryID, gpLib)
	require.NoError(t, err)

	out, err := SyncPharmacyLibrary(ctx, pool, &auth.Principal{ID: "p166-actor", Email: "p166@test.local"}, pharmacyID, libraryID, PlanState{Used: 0, Limit: -1})
	require.NoError(t, err)
	assert.Equal(t, 0, out.ProductsLinked, "conflicting barcodes are never name-linked")
	assert.Equal(t, 1, out.ProductsAdded, "the library product arrives as its own new row")

	var stillOwn int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM pharmacy_products WHERE pharmacy_id = $1::uuid AND global_product_id = $2::uuid`,
		pharmacyID, gpOwn).Scan(&stillOwn))
	assert.Equal(t, 1, stillOwn, "the pharmacy's own barcoded row keeps its identity")

	var ownRowPrice int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT selling_price FROM pharmacy_products WHERE pharmacy_id = $1::uuid AND global_product_id = $2::uuid`,
		pharmacyID, gpOwn).Scan(&ownRowPrice))
	assert.Equal(t, ownPrice, ownRowPrice, "the owned row's price was never overwritten")

	var libRowCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM pharmacy_products WHERE pharmacy_id = $1::uuid AND global_product_id = $2::uuid`,
		pharmacyID, gpLib).Scan(&libRowCount))
	assert.Equal(t, 1, libRowCount, "the library product exists as its own row at the official price")

	// Positive control on a FRESH pair: a barcode-LESS pharmacy row next to a
	// barcoded library product still links (the shipped station-7 behavior
	// the guard must preserve — the library product here is new to v3 so the
	// sync's exists branch cannot absorb it).
	gpLib2 := makeCatalogProduct(t, ctx, pool, "p166-lib2-", "P166 Other Drug", "6666666666661")
	makeLibraryEntry(t, ctx, pool, libraryID, gpLib2, 800)
	gpTwin := makeCatalogProduct(t, ctx, pool, "p166-twin-", "P166 Other Drug XL", "")
	_, err = pool.Exec(ctx, `
		INSERT INTO pharmacy_products (pharmacy_id, global_product_id, selling_price, cost_price, min_stock_level)
		VALUES ($1::uuid, $2::uuid, 555, 0, 0)
	`, pharmacyID, gpTwin)
	require.NoError(t, err)
	var sim2 float64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT similarity('P166 Other Drug XL', 'P166 Other Drug')`).Scan(&sim2))
	require.GreaterOrEqual(t, sim2, NameAutoLinkThreshold,
		"control names must clear the auto-link threshold — otherwise the control proves nothing")
	_, err = pool.Exec(ctx, `
		INSERT INTO library_changes (library_id, global_product_id, version, change_type, summary)
		VALUES ($1::uuid, $2::uuid, 3, 'added', 'p166 guard re-add')
	`, libraryID, gpLib2)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE product_libraries SET version = 3 WHERE id = $1::uuid`, libraryID)
	require.NoError(t, err)

	out, err = SyncPharmacyLibrary(ctx, pool, &auth.Principal{ID: "p166-actor", Email: "p166@test.local"}, pharmacyID, libraryID, PlanState{Used: 0, Limit: -1})
	require.NoError(t, err)
	assert.Equal(t, 1, out.ProductsLinked, "barcode-less twins still auto-link (shipped behavior)")
	var twinLinked int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM pharmacy_products WHERE pharmacy_id = $1::uuid AND global_product_id = $2::uuid`,
		pharmacyID, gpLib2).Scan(&twinLinked))
	assert.Equal(t, 1, twinLinked, "the twin row re-pointed onto the library product")
}
