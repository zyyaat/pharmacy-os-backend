package handlers

import (
        "context"
        "fmt"
        "testing"
        "time"

        "github.com/jackc/pgx/v5/pgxpool"
        "github.com/stretchr/testify/require"

        "github.com/pharmacy-os/backend/internal/repository"
)

// api_level 119 regression gates — «الحقايق» على صفحة المخزون.
//
// Library imports create pharmacy products with official prices and NO stock
// (documented red line). Those products were invisible to the inventory page
// (current_inventory is batch-based) and had no receiving path at all: adjust
// needs an existing batch, the edit page does not manage stock, and creating
// the product again would duplicate the global product. The wave:
//
//   1. GET /pharmacy/inventory?include=unstocked — one synthetic row per
//      batchless product (NULL batch_id, status 'no_stock'). Opt-IN only:
//      the default response stays byte-identical because the mobile cache
//      upserts inventory rows keyed by batch_id.
//   2. POST /pharmacy/products/:id/opening-stock — the first batch through
//      the same ledger contract as every writer: one batch + one 'purchase'
//      movement whose sum IS the cached quantity, so the adjust history gate
//      accepts the batch immediately.
//
// Same harness as the other DB gates:
//
//        TEST_DATABASE_URL=postgres://.../pharmacy_scratch go test ./internal/handlers/ -run 'TestInventoryUnstockedDB|TestOpeningStockDB'
func unstockedFixtures(t *testing.T, pool *pgxpool.Pool, suffix string) (pharmacyID, branchID, companyUserID string) {
        t.Helper()
        ctx := context.Background()
        var accountID, companyID string
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO accounts (company_name, contact_email, status)
                VALUES ($1, $2, 'active') RETURNING id::text
        `, "a119 "+suffix, "a119-"+suffix+"@test.local").Scan(&accountID))
        t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, accountID) })
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO companies (name, email) VALUES ($1, $2) RETURNING id::text
        `, "a119-co "+suffix, "a119co-"+suffix+"@test.local").Scan(&companyID))
        t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM companies WHERE id = $1`, companyID) })
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacies (account_id, name, license_number, is_active)
                VALUES ($1, $2, $3, true) RETURNING id::text
        `, accountID, "a119-ph "+suffix, "A119-"+suffix).Scan(&pharmacyID))
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO branches (pharmacy_id, name, code, is_active)
                VALUES ($1, 'main', $2, true) RETURNING id::text
        `, pharmacyID, "A119B-"+suffix).Scan(&branchID))
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO company_users (company_id, email, password_hash, first_name, last_name)
                VALUES ($1, $2, 'x', 'a', '119') RETURNING id::text
        `, companyID, "a119-user-"+suffix+"@test.local").Scan(&companyUserID))
        return pharmacyID, branchID, companyUserID
}

func unstockedProduct(t *testing.T, pool *pgxpool.Pool, pharmacyID, name string, boxStrip bool, unitsPerBox int) string {
        t.Helper()
        ctx := context.Background()
        packaging := "WHOLE_ONLY"
        if boxStrip {
                packaging = "BOX_STRIP"
        }
        var globalID, productID string
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO global_products (name, generic_name, dosage_form, strength, product_category, default_unit, is_active)
                VALUES ($1, 'a119', 'tablet', '5mg', 'medication', 'tablet', true) RETURNING id::text
        `, name).Scan(&globalID))
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacy_products (pharmacy_id, global_product_id, selling_price, packaging_type, units_per_box, min_stock_level, is_active)
                VALUES ($1, $2, 1500, $3, $4, 0, true) RETURNING id::text
        `, pharmacyID, globalID, packaging, unitsPerBox).Scan(&productID))
        return productID
}

func stockedProduct(t *testing.T, pool *pgxpool.Pool, pharmacyID, branchID, name string) string {
        t.Helper()
        ctx := context.Background()
        productID := unstockedProduct(t, pool, pharmacyID, name, false, 1)
        _, err := pool.Exec(ctx, `
                INSERT INTO inventory_batches (pharmacy_product_id, branch_id, batch_number, quantity, unit, cost_per_unit, expiry_date, received_date)
                VALUES ($1, $2, $3, 10, 'box', 1000, '2028-01-01', CURRENT_DATE)
        `, productID, branchID, "BATCH-"+name)
        require.NoError(t, err)
        return productID
}

func TestInventoryUnstockedDB(t *testing.T) {
        ctx := context.Background()
        pool := inventoryPagingPool(t)
        suffix := fmt.Sprintf("%d", time.Now().UnixNano())
        pharmacyID, branchID, companyUserID := unstockedFixtures(t, pool, suffix)

        // Two stocked products + two batchless (the library-import shape).
        stockedProduct(t, pool, pharmacyID, branchID, "A119-ALPHA-"+suffix)
        stockedProduct(t, pool, pharmacyID, branchID, "A119-BETA-"+suffix)
        wholeOnly := unstockedProduct(t, pool, pharmacyID, "A119-IMPORTED-WHOLE-"+suffix, false, 1)
        boxStrip := unstockedProduct(t, pool, pharmacyID, "A119-IMPORTED-STRIP-"+suffix, true, 3)

        // ---- 1) the default path stays the pre-119 contract -----------------
        items, total, err := inventoryPage(ctx, pool, pharmacyID, "", 500, 0)
        require.NoError(t, err)
        require.Len(t, items, 2, "default path: batch rows only")
        require.EqualValues(t, 2, total)
        for _, row := range items {
                require.NotEqual(t, "no_stock", row["status"], "default path never serves the synthetic status")
        }

        // ---- 2) include=unstocked: 4 rows, honest shapes --------------------
        items, total, err = inventoryPageExtended(ctx, pool, pharmacyID, "", 500, 0, true)
        require.NoError(t, err)
        require.Len(t, items, 4, "one row per batch + one per batchless product")
        require.EqualValues(t, 4, total, "total counts the combined window")

        byStatus := map[string][]map[string]interface{}{}
        for _, row := range items {
                byStatus[row["status"].(string)] = append(byStatus[row["status"].(string)], row)
        }
        require.Len(t, byStatus["no_stock"], 2, "exactly two synthetic rows")
        for _, row := range byStatus["no_stock"] {
                require.Nil(t, row["batch_id"], "synthetic rows carry a NULL batch_id")
                require.Nil(t, row["expiry_date"])
                require.Nil(t, row["days_until_expiry"])
                require.EqualValues(t, 0, row["quantity"])
                require.Equal(t, "", row["batch_number"])
                require.Equal(t, "", row["branch_name"])
                require.EqualValues(t, 1500, row["selling_price_piastres"], "the product's official price rides the row")
                require.NotEmpty(t, row["pharmacy_product_id"], "the action key is present")
        }
        // Packaging-derived fields survive the union.
        var stripRow, wholeRow map[string]interface{}
        for _, row := range byStatus["no_stock"] {
                switch row["packaging_type"] {
                case "BOX_STRIP":
                        stripRow = row
                        require.Equal(t, "strip", row["unit"])
                        require.EqualValues(t, 3, row["units_per_box"])
                case "WHOLE_ONLY":
                        wholeRow = row
                        require.Equal(t, "box", row["unit"])
                }
        }
        require.NotNil(t, stripRow)
        require.NotNil(t, wholeRow)
        _ = wholeOnly
        _ = boxStrip

        // ---- 3) search reaches the synthetic rows with the same predicate ---
        found, totalFound, err := inventoryPageExtended(ctx, pool, pharmacyID, "IMPORTED-", 500, 0, true)
        require.NoError(t, err)
        require.Len(t, found, 2, "search finds both imported products")
        require.EqualValues(t, 2, totalFound)
        // ...and stocked rows by batch number, unchanged.
        found, totalFound, err = inventoryPageExtended(ctx, pool, pharmacyID, "BATCH-A119-ALPHA", 500, 0, true)
        require.NoError(t, err)
        require.Len(t, found, 1)
        require.EqualValues(t, 1, totalFound)
        _, totalFound, err = inventoryPageExtended(ctx, pool, pharmacyID, "NO-SUCH-"+suffix, 500, 0, true)
        require.NoError(t, err)
        require.EqualValues(t, 0, totalFound)

        // ---- 4) paging walks the combined set without gaps or overlap -------
        page1, total1, err := inventoryPageExtended(ctx, pool, pharmacyID, "", 3, 0, true)
        require.NoError(t, err)
        require.Len(t, page1, 3)
        require.EqualValues(t, 4, total1)
        page2, total2, err := inventoryPageExtended(ctx, pool, pharmacyID, "", 3, 3, true)
        require.NoError(t, err)
        require.Len(t, page2, 1)
        require.EqualValues(t, -1, total2, "offset pages skip the count — total is authoritative at offset=0 only (api_level 121)")
        seen := map[string]bool{}
        for _, row := range append(append([]map[string]interface{}{}, page1...), page2...) {
                // Synthetic rows share a NULL batch_id — the identity is the pair.
                key := fmt.Sprintf("%v|%v", row["batch_id"], row["pharmacy_product_id"])
                require.False(t, seen[key], "paging never repeats a row")
                seen[key] = true
        }

        // ---- 5) foreign isolation covers synthetic rows too -----------------
        otherPharmacy, otherBranch, _ := unstockedFixtures(t, pool, "o"+suffix)
        unstockedProduct(t, pool, otherPharmacy, "A119-FOREIGN-IMPORTED-"+suffix, false, 1)
        stockedProduct(t, pool, otherPharmacy, otherBranch, "A119-FOREIGN-STOCKED-"+suffix)
        _, totalForeign, err := inventoryPageExtended(ctx, pool, pharmacyID, "", 500, 0, true)
        require.NoError(t, err)
        require.EqualValues(t, 4, totalForeign, "another pharmacy's unstocked rows never leak")

        // ---- 6) opening stock converts a synthetic row into a batch row ------
        opened, err := repository.NewStockMovementRepository(pool).OpenProductStock(ctx, repository.OpeningStockInput{
                ProductID: wholeOnly, PharmacyID: pharmacyID, BranchID: branchID,
                CompanyUserID: companyUserID,
                InitialBoxes:  4, IdempotencyKey: "a119-open-" + suffix,
        })
        require.NoError(t, err)
        require.NotEmpty(t, opened.BatchID)

        items, total, err = inventoryPageExtended(ctx, pool, pharmacyID, "", 500, 0, true)
        require.NoError(t, err)
        require.Len(t, items, 4, "same row count: the opened product switched shapes")
        require.EqualValues(t, 4, total)
        noStock := 0
        batchRows := 0
        for _, row := range items {
                if row["status"] == "no_stock" {
                        noStock++
                } else {
                        batchRows++
                }
        }
        require.Equal(t, 1, noStock, "one batchless product remains")
        require.Equal(t, 3, batchRows, "the opened product is now a real batch row")

        // ---- 7) the mobile contract: current_inventory stays batch-only -----
        var viewRows int
        require.NoError(t, pool.QueryRow(ctx, `
                SELECT count(*) FROM current_inventory WHERE pharmacy_id = $1
        `, pharmacyID).Scan(&viewRows))
        require.Equal(t, 3, viewRows, "current_inventory never serves synthetic rows (mobile upserts by batch_id)")
        _ = stripRow
}

func TestOpeningStockDB(t *testing.T) {
        ctx := context.Background()
        pool := inventoryPagingPool(t)
        suffix := fmt.Sprintf("%d", time.Now().UnixNano())
        pharmacyID, branchID, companyUserID := unstockedFixtures(t, pool, suffix)
        repo := repository.NewStockMovementRepository(pool)

        wholeOnly := unstockedProduct(t, pool, pharmacyID, "A119-OPEN-WHOLE-"+suffix, false, 1)
        strip := unstockedProduct(t, pool, pharmacyID, "A119-OPEN-STRIP-"+suffix, true, 3)
        hasBatch := stockedProduct(t, pool, pharmacyID, branchID, "A119-OPEN-HASBATCH-"+suffix)

        // ---- 1) WHOLE_ONLY opening: exact ledger columns ---------------------
        res, err := repo.OpenProductStock(ctx, repository.OpeningStockInput{
                ProductID: wholeOnly, PharmacyID: pharmacyID, BranchID: branchID,
                CompanyUserID: companyUserID, InitialBoxes: 2, CostPricePiastres: 1005,
                BatchNumber: "A119-LOT-1", ExpiryDate: "2028-06-30",
                IdempotencyKey: "a119-whole-" + suffix,
        })
        require.NoError(t, err)
        require.False(t, res.Replayed)
        require.Equal(t, "box", res.Unit)
        require.EqualValues(t, 2, res.Quantity)

        var qty float64
        var unit, refType, batchNumber string
        var costPerUnit float64
        var expiry *time.Time
        var receivedBy *string
        require.NoError(t, pool.QueryRow(ctx, `
                SELECT quantity::float8, unit::text, cost_per_unit::float8, expiry_date, received_by::text, reference_type::text, batch_number
                FROM inventory_batches WHERE id = $1
        `, res.BatchID).Scan(&qty, &unit, &costPerUnit, &expiry, &receivedBy, &refType, &batchNumber))
        require.Equal(t, 2.0, qty)
        require.Equal(t, "box", unit)
        require.Equal(t, 1005.0, costPerUnit, "WHOLE_ONLY keeps the box cost per unit")
        require.NotNil(t, expiry)
        require.Equal(t, "2028-06-30", expiry.Format("2006-01-02"))
        require.Nil(t, receivedBy, "received_by rides employees only — a company-user actor stays NULL")
        require.Equal(t, "opening_balance", refType)
        require.Equal(t, "A119-LOT-1", batchNumber)

        var mQty, mBefore, mAfter float64
        var mType, mReason string
        var mTotal float64
        var mUnitCost float64
        var mCompanyUser *string
        require.NoError(t, pool.QueryRow(ctx, `
                SELECT quantity::float8, quantity_before::float8, quantity_after::float8,
                       movement_type::text, reason, total_cost::float8, unit_cost::float8,
                       created_by_company_user_id::text
                FROM stock_movements WHERE id = $1
        `, res.MovementID).Scan(&mQty, &mBefore, &mAfter, &mType, &mReason, &mTotal, &mUnitCost, &mCompanyUser))
        require.Equal(t, 2.0, mQty)
        require.Equal(t, 0.0, mBefore)
        require.Equal(t, 2.0, mAfter)
        require.Equal(t, "purchase", mType)
        require.Equal(t, "opening_balance", mReason, "empty reason falls back to the reference")
        require.Equal(t, 2010.0, mTotal)
        require.Equal(t, 1005.0, mUnitCost)
        require.NotNil(t, mCompanyUser)

        // ---- 2) the ledger baseline: SUM(movements) == cached quantity -------
        var movementSum float64
        require.NoError(t, pool.QueryRow(ctx, `
                SELECT COALESCE(SUM(quantity), 0)::float8 FROM stock_movements WHERE batch_id = $1
        `, res.BatchID).Scan(&movementSum))
        require.Equal(t, qty, movementSum, "the batch opens with its ledger established")

        // ---- 3) replay: same key returns the same movement, no new rows ------
        replay, err := repo.OpenProductStock(ctx, repository.OpeningStockInput{
                ProductID: wholeOnly, PharmacyID: pharmacyID, BranchID: branchID,
                CompanyUserID: companyUserID, InitialBoxes: 2, IdempotencyKey: "a119-whole-" + suffix,
        })
        require.NoError(t, err)
        require.True(t, replay.Replayed)
        require.Equal(t, res.MovementID, replay.MovementID)
        require.Equal(t, res.BatchID, replay.BatchID)

        // ---- 4) same key, different quantity → conflict ----------------------
        _, err = repo.OpenProductStock(ctx, repository.OpeningStockInput{
                ProductID: wholeOnly, PharmacyID: pharmacyID, BranchID: branchID,
                CompanyUserID: companyUserID, InitialBoxes: 5, IdempotencyKey: "a119-whole-" + suffix,
        })
        require.ErrorIs(t, err, repository.ErrIdempotencyConflict)

        // ---- 5) one-batch rule: adjust is the path after the opening ---------
        _, err = repo.OpenProductStock(ctx, repository.OpeningStockInput{
                ProductID: wholeOnly, PharmacyID: pharmacyID, BranchID: branchID,
                CompanyUserID: companyUserID, InitialBoxes: 1, IdempotencyKey: "a119-second-" + suffix,
        })
        require.ErrorIs(t, err, repository.ErrOpeningStockExists)
        _, err = repo.OpenProductStock(ctx, repository.OpeningStockInput{
                ProductID: hasBatch, PharmacyID: pharmacyID, BranchID: branchID,
                CompanyUserID: companyUserID, InitialBoxes: 1, IdempotencyKey: "a119-hasbatch-" + suffix,
        })
        require.ErrorIs(t, err, repository.ErrOpeningStockExists)

        // ---- 6) packaging rules mirror create --------------------------------
        _, err = repo.OpenProductStock(ctx, repository.OpeningStockInput{
                ProductID: wholeOnly, PharmacyID: pharmacyID, BranchID: branchID,
                CompanyUserID: companyUserID, InitialStrips: 3, IdempotencyKey: "a119-stripswhole-" + suffix,
        })
        require.ErrorIs(t, err, repository.ErrInvalidOpeningStock)
        _, err = repo.OpenProductStock(ctx, repository.OpeningStockInput{
                ProductID: wholeOnly, PharmacyID: pharmacyID, BranchID: branchID,
                CompanyUserID: companyUserID, InitialBoxes: 0, IdempotencyKey: "a119-zero-" + suffix,
        })
        require.ErrorIs(t, err, repository.ErrInvalidOpeningStock)

        // ---- 7) BOX_STRIP: half-up cost division + strips arithmetic ---------
        resStrip, err := repo.OpenProductStock(ctx, repository.OpeningStockInput{
                ProductID: strip, PharmacyID: pharmacyID, BranchID: branchID,
                CompanyUserID: companyUserID, InitialBoxes: 1, InitialStrips: 2,
                CostPricePiastres: 1007, IdempotencyKey: "a119-strip-" + suffix,
        })
        require.NoError(t, err)
        require.Equal(t, "strip", resStrip.Unit)
        require.EqualValues(t, 5, resStrip.Quantity, "1 box = 3 strips + 2 strips")
        var stripCost float64
        var stripTotal float64
        require.NoError(t, pool.QueryRow(ctx, `
                SELECT cost_per_unit::float8, total_cost::float8 FROM inventory_batches WHERE id = $1
        `, resStrip.BatchID).Scan(&stripCost, &stripTotal))
        require.Equal(t, 336.0, stripCost, "1007/3 rounds half-up to 336 per strip")
        require.Equal(t, 1680.0, stripTotal, "total = 336 × 5 strips")

        // ---- 8) foreign product never opens ----------------------------------
        otherPharmacy, _, _ := unstockedFixtures(t, pool, "x"+suffix)
        foreignProduct := unstockedProduct(t, pool, otherPharmacy, "A119-FOREIGN-OPEN-"+suffix, false, 1)
        _, err = repo.OpenProductStock(ctx, repository.OpeningStockInput{
                ProductID: foreignProduct, PharmacyID: pharmacyID, BranchID: branchID,
                CompanyUserID: companyUserID, InitialBoxes: 1, IdempotencyKey: "a119-foreign-" + suffix,
        })
        require.ErrorIs(t, err, repository.ErrOpeningStockProductNotFound)

        // ---- 9) the adjust gate accepts the opened batch immediately ---------
        adjusted, err := repo.AdjustBatchStock(ctx, repository.StockAdjustmentInput{
                BatchID: res.BatchID, PharmacyID: pharmacyID, BranchID: branchID,
                CompanyUserID: companyUserID, Delta: 5,
                IdempotencyKey: "a119-adjust-" + suffix,
        })
        require.NoError(t, err, "the opening movement IS the history the adjust gate requires")
        require.Equal(t, 7.0, adjusted.NewQuantity)

        // ---- 10) ...and the opened product left the synthetic window ---------
        _, totalAfter, err := inventoryPageExtended(ctx, pool, pharmacyID, "A119-OPEN-WHOLE", 500, 0, true)
        require.NoError(t, err)
        require.EqualValues(t, 1, totalAfter, "search sees the product once — as a batch row")
        items, _, err := inventoryPageExtended(ctx, pool, pharmacyID, "A119-OPEN-WHOLE", 500, 0, true)
        require.NoError(t, err)
        require.Equal(t, "normal", items[0]["status"])
        require.NotNil(t, items[0]["batch_id"])
}

// api_level 125 regression gate — include=unstocked over LOCAL products in
// both opening states (الرصيد الافتتاحي: مغلق = بلا تشغيلة بعد، مفتوح =
// استُلم عبر OpenProductStock). Pre-fix both states killed the list: the
// closed state through the synthetic row's gp.id::text (LEFT JOIN miss on a
// local product — pgx «cannot scan NULL into *string») and the opened state
// through the view row's global_product_id::text NULL — بالضبط بلاغ المالك
// «مش بتحمل المنتجات وبتطلب اعادة تحميل». The synthetic row must also carry
// the effective identity (override المحلي أولًا — الاسم العربي الفعلي، ليس
// سلسلة فارغة تُخفي المنتج عن كل بحث). والحركة الافتتاحية خلف التشغيلة
// بملاحظات NULL — أسوأ صف إنتاجي — لا يعطل شيئًا.
// نفس الحزام: TEST_DATABASE_URL مع تخطٍ محلي.
//
//        TEST_DATABASE_URL=postgres://.../pharmacy_scratch go test ./internal/handlers/ -run TestInventoryUnstockedLocalOpeningDB
func TestInventoryUnstockedLocalOpeningDB(t *testing.T) {
        ctx := context.Background()
        pool := inventoryPagingPool(t)
        suffix := fmt.Sprintf("%d", time.Now().UnixNano())
        pharmacyID, branchID, companyUserID := unstockedFixtures(t, pool, suffix)
        repo := repository.NewStockMovementRepository(pool)

        localProduct := func(name string, boxStrip bool, unitsPerBox int) string {
                packaging := "WHOLE_ONLY"
                if boxStrip {
                        packaging = "BOX_STRIP"
                }
                var productID string
                require.NoError(t, pool.QueryRow(ctx, `
                        INSERT INTO pharmacy_products (pharmacy_id, origin_type, name_override, selling_price, packaging_type, units_per_box, min_stock_level, is_active)
                        VALUES ($1, 'local', $2, 1500, $3, $4, 0, true) RETURNING id::text
                `, pharmacyID, name, packaging, unitsPerBox).Scan(&productID))
                return productID
        }

        // مغلق: منتج محلي عربي بلا تشغيلة بعد — الصف الاصطناعي شكله كامل.
        closedName := "بروفين محلي " + suffix
        closed := localProduct(closedName, false, 1)
        // مفتوح: منتج محلي سيستلم رصيده الافتتاحي في نفس الاختبار.
        openName := "بنادول محلي " + suffix
        open := localProduct(openName, true, 3)
        _ = closed

        // ---- قبل الفتح: صفّان اصطناعيان بلا 500 وبالهوية الفعالة --------------
        items, total, err := inventoryPageExtended(ctx, pool, pharmacyID, "", 500, 0, true)
        require.NoError(t, err, "pre-fix: the synthetic row of a local product 500'd the union")
        require.Len(t, items, 2)
        require.EqualValues(t, 2, total, "{data,total} contract at offset=0")
        for _, row := range items {
                require.Equal(t, "no_stock", row["status"])
                require.Nil(t, row["batch_id"])
                require.Equal(t, "", row["global_product_id"], "pre-fix: gp.id::text NULL killed the list here")
                require.Nil(t, row["expiry_date"])
                require.EqualValues(t, 1500, row["selling_price_piastres"])
        }
        // البحث العربي يصل الصفوف الاصطناعية المحلية باسمها الفعلي (كان مستحيلًا
        // باسم فارغ، ورمي 500 قبل تحصين gp.id).
        found, totalFound, err := inventoryPageExtended(ctx, pool, pharmacyID, "بروفين", 500, 0, true)
        require.NoError(t, err)
        require.Len(t, found, 1)
        require.EqualValues(t, 1, totalFound)
        require.Equal(t, closedName, found[0]["product_name"])

        // ---- الفتح: نفس عقد الدفتر على منتج محلي -----------------------------
        opened, err := repo.OpenProductStock(ctx, repository.OpeningStockInput{
                ProductID: open, PharmacyID: pharmacyID, BranchID: branchID,
                CompanyUserID: companyUserID,
                InitialBoxes:  4, IdempotencyKey: "a125-open-" + suffix,
        })
        require.NoError(t, err)
        require.NotEmpty(t, opened.BatchID)

        // حركة الافتتاح خلف التشغيلة بملاحظات NULL وقصدها المرجع الافتتاحي —
        // أسوأ صف إنتاجي خلف التشغيلة.
        var movementNotes *string
        var movementReason string
        require.NoError(t, pool.QueryRow(ctx, `
                SELECT notes, reason FROM stock_movements WHERE batch_id = $1
        `, opened.BatchID).Scan(&movementNotes, &movementReason))
        require.Nil(t, movementNotes, "the opening movement rides notes NULL — the worst-case row behind the batch")
        require.Equal(t, "opening_balance", movementReason)

        // ---- بعد الفتح: المساران يعرضان المنتج المفتوح بلا 500 ----------------
        items, total, err = inventoryPageExtended(ctx, pool, pharmacyID, "", 500, 0, true)
        require.NoError(t, err, "pre-fix: the opened local product's view row 500'd the union")
        require.Len(t, items, 2, "same count: one opened (batch row) + one closed (synthetic)")
        require.EqualValues(t, 2, total)
        _, offsetTotal, err := inventoryPageExtended(ctx, pool, pharmacyID, "", 2, 2, true)
        require.NoError(t, err)
        require.EqualValues(t, -1, offsetTotal, "offset pages skip the count (api_level 121 contract)")

        var openedRow map[string]interface{}
        noStock := 0
        for _, row := range items {
                if row["status"] == "no_stock" {
                        noStock++
                } else {
                        openedRow = row
                }
        }
        require.Equal(t, 1, noStock, "only the still-batchless product stays synthetic")
        require.NotNil(t, openedRow, "the opened product left the synthetic window")
        require.NotNil(t, openedRow["batch_id"], "the mobile upsert key is a real batch id now")
        require.Equal(t, "", openedRow["global_product_id"])
        require.Equal(t, openName, openedRow["product_name"])
        require.EqualValues(t, 12, openedRow["quantity"], "BOX_STRIP: 4 boxes × 3 strips = 12 base units")
        require.EqualValues(t, 0, openedRow["cost_per_unit_piastres"], "opening without cost rides the 0 floor (schema forbids NULL cost)")
        require.Equal(t, "normal", openedRow["status"])

        // المسار الافتراضي (بلا include): منتج محلي بتشغيلة هو بالضبط شكل الـ500
        // الإنتاجي — القايمة كلها كانت تموت من هذا الصف وحده.
        defaultItems, defaultTotal, err := inventoryPage(ctx, pool, pharmacyID, "", 500, 0)
        require.NoError(t, err, "pre-fix: THE production 500 — a local product with a batch")
        require.Len(t, defaultItems, 1)
        require.EqualValues(t, 1, defaultTotal)
        require.Equal(t, openName, defaultItems[0]["product_name"])
        require.Equal(t, "", defaultItems[0]["global_product_id"])
}
