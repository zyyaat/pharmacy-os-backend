package handlers

import (
        "context"
        "fmt"
        "os"
        "testing"
        "time"

        "github.com/jackc/pgx/v5"
        "github.com/jackc/pgx/v5/pgxpool"
        "github.com/stretchr/testify/require"
)

// R-8 regression gate for the real-paging contract on the inventory list
// (api_level 104). Pre-R-8 the endpoint silently capped at LIMIT 500 and the
// page filtered client-side, so a pharmacy past the cap lost rows without any
// signal. The contract is now: limit/offset pages + a true total over the
// same search window, and a server-side search mirroring the six text fields
// the client used to filter (product_name, generic_name, brand_name,
// strength, barcode, batch_number).
//
// The gate drives inventoryPage — the same function the HTTP handler uses —
// against a real migrated schema, so the SQL under test cannot drift from
// what production serves. Pinned:
//   - total reflects the FULL filtered set, not the page length, and is
//     authoritative at offset=0 ONLY — later offset pages return total=-1
//     (the count re-runs the full union per walk; the feed reads total from
//     its first page, so re-counting every scroll batch was pure wasted
//     work — api_level 121 makes each scroll batch a single query),
//   - limit/offset walk the deterministic (product_name, expiry_date) order
//     without overlap or gaps,
//   - the server-side search reaches the same six fields the old client
//     filter did (name, generic, brand, strength, barcode, batch number),
//   - search='' + offset=0 stays byte-for-byte the pre-R-8 plan (default
//     limit 500, no predicate).
//
// Same harness as the other DB gates:
//
//        TEST_DATABASE_URL=postgres://.../pharmacy_scratch go test ./internal/handlers/ -run TestInventoryPagingDB
func inventoryPagingPool(t *testing.T) *pgxpool.Pool {
        t.Helper()
        dsn := os.Getenv("TEST_DATABASE_URL")
        if dsn == "" {
                t.Skip("TEST_DATABASE_URL not set — inventory paging gates need a disposable PostgreSQL")
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

func TestInventoryPagingDB(t *testing.T) {
        ctx := context.Background()
        pool := inventoryPagingPool(t)
        suffix := fmt.Sprintf("%d", time.Now().UnixNano())

        // ---- fixtures (same chain as the POS persist gate) -------------------
        var accountID, pharmacyID, branchID string
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO accounts (company_name, contact_email, status)
                VALUES ($1, $2, 'active') RETURNING id::text
        `, "r8-gate "+suffix, "r8gate-"+suffix+"@test.local").Scan(&accountID))
        t.Cleanup(func() {
                _, _ = pool.Exec(context.Background(),
                        `DELETE FROM accounts WHERE id = $1`, accountID) // ON DELETE CASCADE sweeps the rest
        })
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacies (account_id, name, license_number, is_active)
                VALUES ($1, $2, $3, true) RETURNING id::text
        `, accountID, "r8-gate-ph "+suffix, "R8-"+suffix).Scan(&pharmacyID))
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO branches (pharmacy_id, name, code, is_active)
                VALUES ($1, 'main', $2, true) RETURNING id::text
        `, pharmacyID, "R8B-"+suffix).Scan(&branchID))

        // Three products with DISTINCT names; name order != insertion order so
        // the paging walk really follows the ORDER BY.
        names := []string{"R8-GATE-ALPHA-" + suffix, "R8-GATE-BETA-" + suffix, "R8-GATE-GAMMA-" + suffix}
        for _, name := range names {
                var globalID, productID string
                require.NoError(t, pool.QueryRow(ctx, `
                        INSERT INTO global_products (name, generic_name, dosage_form, strength, product_category, default_unit, is_active)
                        VALUES ($1, 'gate', 'tablet', '5mg', 'medication', 'tablet', true) RETURNING id::text
                `, name).Scan(&globalID))
                require.NoError(t, pool.QueryRow(ctx, `
                        INSERT INTO pharmacy_products (pharmacy_id, global_product_id, cost_price, selling_price, min_stock_level, is_active)
                        VALUES ($1, $2, 1000, 1500, 0, true) RETURNING id::text
                `, pharmacyID, globalID).Scan(&productID))
                _, err := pool.Exec(ctx, `
                        INSERT INTO inventory_batches (pharmacy_product_id, branch_id, batch_number, quantity, unit, cost_per_unit, expiry_date, received_date)
                        VALUES ($1, $2, $3, 10, 'box', 1000, '2028-01-01', CURRENT_DATE)
                `, productID, branchID, "R8-BATCH-"+name)
                require.NoError(t, err)
        }

        // ---- contract --------------------------------------------------------
        // 1) no params: total = 3 (true total, NOT the page length), full page.
        items, total, err := inventoryPage(ctx, pool, pharmacyID, "", 500, 0)
        require.NoError(t, err)
        require.Len(t, items, 3)
        require.EqualValues(t, 3, total, "total counts the filtered set, not the page")

        // 2) paging: limit=2 offset=0 then offset=2 — no overlap, no gap, same total.
        page1, total1, err := inventoryPage(ctx, pool, pharmacyID, "", 2, 0)
        require.NoError(t, err)
        require.Len(t, page1, 2)
        require.EqualValues(t, 3, total1)
        page2, total2, err := inventoryPage(ctx, pool, pharmacyID, "", 2, 2)
        require.NoError(t, err)
        require.Len(t, page2, 1)
        require.EqualValues(t, -1, total2, "offset pages skip the count — total is authoritative at offset=0 only (api_level 121)")
        firstName := page1[0]["product_name"].(string)
        require.Equal(t, firstName, items[0]["product_name"], "page 1 starts at the deterministic order")
        require.Equal(t, items[2]["product_name"].(string), page2[0]["product_name"].(string), "page 2 continues exactly where page 1 stopped")

        // 3) server-side search mirrors the old client filter: the batch number
        //    embeds the product name, so searching ALPHA reaches batch_number too.
        alpha := "ALPHA-" + suffix
        found, totalFound, err := inventoryPage(ctx, pool, pharmacyID, alpha, 500, 0)
        require.NoError(t, err)
        require.Len(t, found, 1, "search by batch-number fragment finds exactly one")
        require.EqualValues(t, 1, totalFound)
        require.Contains(t, found[0]["product_name"].(string), alpha)
        require.Contains(t, found[0]["batch_number"].(string), alpha)

        // 4) search hits generic_name and strength as well (fields the old client
        //    filter covered) — the predicate must reach all six.
        byGeneric, _, err := inventoryPage(ctx, pool, pharmacyID, "gate", 500, 0)
        require.NoError(t, err)
        require.Len(t, byGeneric, 3, "generic_name 'gate' matches all three")
        byStrength, _, err := inventoryPage(ctx, pool, pharmacyID, "5mg", 500, 0)
        require.NoError(t, err)
        require.Len(t, byStrength, 3, "strength '5mg' matches all three")

        // 5) no match → empty page, honest zero total.
        _, totalNone, err := inventoryPage(ctx, pool, pharmacyID, "NO-SUCH-TERM-"+suffix, 500, 0)
        require.NoError(t, err)
        require.EqualValues(t, 0, totalNone, "no match reports total=0")

        // 6) another pharmacy's rows never leak: a second pharmacy with one batch
        //    stays invisible to the first pharmacy's queries.
        var otherAccount, otherPharmacy, otherBranch, otherGlobal, otherProduct string
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO accounts (company_name, contact_email, status)
                VALUES ($1, $2, 'active') RETURNING id::text
        `, "r8-gate-other "+suffix, "r8gate-other-"+suffix+"@test.local").Scan(&otherAccount))
        t.Cleanup(func() {
                _, _ = pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, otherAccount)
        })
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacies (account_id, name, license_number, is_active)
                VALUES ($1, $2, $3, true) RETURNING id::text
        `, otherAccount, "r8-gate-other-ph "+suffix, "R8O-"+suffix).Scan(&otherPharmacy))
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO branches (pharmacy_id, name, code, is_active)
                VALUES ($1, 'main', $2, true) RETURNING id::text
        `, otherPharmacy, "R8OB-"+suffix).Scan(&otherBranch))
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO global_products (name, generic_name, dosage_form, strength, product_category, default_unit, is_active)
                VALUES ($1, 'gate', 'tablet', '5mg', 'medication', 'tablet', true) RETURNING id::text
        `, "R8-GATE-FOREIGN-"+suffix).Scan(&otherGlobal))
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacy_products (pharmacy_id, global_product_id, cost_price, selling_price, min_stock_level, is_active)
                VALUES ($1, $2, 1000, 1500, 0, true) RETURNING id::text
        `, otherPharmacy, otherGlobal).Scan(&otherProduct))
        _, err = pool.Exec(ctx, `
                INSERT INTO inventory_batches (pharmacy_product_id, branch_id, batch_number, quantity, unit, cost_per_unit, expiry_date, received_date)
                VALUES ($1, $2, 'R8-FOREIGN', 10, 'box', 1000, '2028-01-01', CURRENT_DATE)
        `, otherProduct, otherBranch)
        require.NoError(t, err)

        itemsAfter, totalAfter, err := inventoryPage(ctx, pool, pharmacyID, "", 500, 0)
        require.NoError(t, err)
        require.Len(t, itemsAfter, 3, "foreign pharmacy rows never leak into the list")
        require.EqualValues(t, 3, totalAfter)

        // 7) worst-case ties: five batches share the SAME name and the SAME
        //    expiry — exactly the rows OFFSET paging used to repeat or skip
        //    (Postgres gives tied rows no stability guarantee across separate
        //    LIMIT/OFFSET queries; the owner saw duplicates between pages).
        //    The walked windows must reproduce the full ordered page exactly:
        //    no duplicate batch, no missing batch, same order.
        var tieGlobal, tieProduct string
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO global_products (name, generic_name, dosage_form, strength, product_category, default_unit, is_active)
                VALUES ($1, 'gate', 'tablet', '5mg', 'medication', 'tablet', true) RETURNING id::text
        `, "R8-GATE-TIE-"+suffix).Scan(&tieGlobal))
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacy_products (pharmacy_id, global_product_id, cost_price, selling_price, min_stock_level, is_active)
                VALUES ($1, $2, 1000, 1500, 0, true) RETURNING id::text
        `, pharmacyID, tieGlobal).Scan(&tieProduct))
        for i := 0; i < 5; i++ {
                _, err = pool.Exec(ctx, `
                        INSERT INTO inventory_batches (pharmacy_product_id, branch_id, batch_number, quantity, unit, cost_per_unit, expiry_date, received_date)
                        VALUES ($1, $2, $3, 10, 'box', 1000, '2028-01-01', CURRENT_DATE)
                `, tieProduct, branchID, fmt.Sprintf("R8-TIE-%02d", i))
                require.NoError(t, err)
        }

        full, tieTotal, err := inventoryPage(ctx, pool, pharmacyID, "R8-GATE-TIE-"+suffix, 500, 0)
        require.NoError(t, err)
        require.Len(t, full, 5, "all five tied batches form one search window")
        require.EqualValues(t, 5, tieTotal)

        var walked []string
        for tieOffset := 0; tieOffset < 5; tieOffset += 2 {
                tiePage, _, pageErr := inventoryPage(ctx, pool, pharmacyID, "R8-GATE-TIE-"+suffix, 2, tieOffset)
                require.NoError(t, pageErr)
                for _, item := range tiePage {
                        walked = append(walked, item["batch_id"].(string))
                }
        }
        var expected []string
        for _, item := range full {
                expected = append(expected, item["batch_id"].(string))
        }
        require.Equal(t, expected, walked, "offset walk over fully tied rows repeats nothing and skips nothing")
}

// api_level 125 regression gate — the production 500 the owner reported
// («صفحة المخزون مش بتحمل المنتجات وفي نفس الوقت بتطلب اعادة تحميل»).
// migration 53 made pharmacy_products.global_product_id nullable — a local
// row (origin_type='local') carries NO catalog link — while the shared
// inventory SELECT scanned column 3 (global_product_id::text) into a plain
// string. One such row — a local product created from POS WITH opening
// stock, or an unlinked product whose batches are kept — scanned NULL into
// *string (pgx: «cannot scan NULL into *string») and killed the WHOLE list,
// while CI stayed green because no test created a local product with a batch
// and then listed the inventory. The fixtures are the ORDINARY production
// pharmacy, not an exotic one:
//   - a catalog product with a batch (the shape that always worked),
//   - a LOCAL product with a batch parked at the nullable floor: expiry
//     NULL وdays_until_expiry NULL وcost_per_unit 0 (المخطط يمنع تكلفة NULL
//     بـNOT NULL DEFAULT 0 فالأسوأ المسموح صفر) وsupplier/location/
//     reference/received_by NULL — وحركة شراء بملاحظات NULL خلف التشغيلة
//     مغطاة في بوابة TestInventoryUnstockedLocalOpeningDB حيث يوجد فاعل،
//   - a LOCAL batchless product with an Arabic name (include=unstocked
//     territory) والبحث العربي على المسارين.
//
// The gate drives inventoryPage/inventoryPageExtended — the same functions
// the HTTP handler serves (the {data,total} contract: offset=0 = real
// total، offset>0 = -1) — so the SQL under test cannot drift from what
// production serves. require.NoError هنا هو تأكيد «200 بلا 500» حرفيًا:
// خطأ المسح قبل الإصلاح كان يظهر في هذه النقطة بالضبط.
//
//        TEST_DATABASE_URL=postgres://.../pharmacy_scratch go test ./internal/handlers/ -run TestInventoryLocalProductScanDB
func TestInventoryLocalProductScanDB(t *testing.T) {
        ctx := context.Background()
        pool := inventoryPagingPool(t)
        suffix := fmt.Sprintf("%d", time.Now().UnixNano())

        var accountID, pharmacyID, branchID string
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO accounts (company_name, contact_email, status)
                VALUES ($1, $2, 'active') RETURNING id::text
        `, "a125-gate "+suffix, "a125gate-"+suffix+"@test.local").Scan(&accountID))
        t.Cleanup(func() {
                _, _ = pool.Exec(context.Background(),
                        `DELETE FROM accounts WHERE id = $1`, accountID) // ON DELETE CASCADE sweeps the rest
        })
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacies (account_id, name, license_number, is_active)
                VALUES ($1, $2, $3, true) RETURNING id::text
        `, accountID, "a125-gate-ph "+suffix, "A125-"+suffix).Scan(&pharmacyID))
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO branches (pharmacy_id, name, code, is_active)
                VALUES ($1, 'main', $2, true) RETURNING id::text
        `, pharmacyID, "A125B-"+suffix).Scan(&branchID))

        // 1) control: catalog-linked product with a batch — the shape that
        //    always worked and must stay byte-identical.
        var catalogGlobal, catalogProduct string
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO global_products (name, generic_name, dosage_form, strength, product_category, default_unit, is_active)
                VALUES ($1, 'a125', 'tablet', '5mg', 'medication', 'tablet', true) RETURNING id::text
        `, "A125-CATALOG-"+suffix).Scan(&catalogGlobal))
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacy_products (pharmacy_id, global_product_id, cost_price, selling_price, min_stock_level, is_active)
                VALUES ($1, $2, 1000, 1500, 0, true) RETURNING id::text
        `, pharmacyID, catalogGlobal).Scan(&catalogProduct))
        _, err := pool.Exec(ctx, `
                INSERT INTO inventory_batches (pharmacy_product_id, branch_id, batch_number, quantity, unit, cost_per_unit, expiry_date, received_date)
                VALUES ($1, $2, $3, 10, 'box', 1000, '2028-01-01', CURRENT_DATE)
        `, catalogProduct, branchID, "A125-BATCH-CATALOG-"+suffix)
        require.NoError(t, err)

        // 2) THE regression shape: a LOCAL product (origin_type='local',
        //    global_product_id NULL) WITH a batch at the nullable floor.
        //    Pre-fix this single row 500'd every inventory list and every
        //    delta-sync for the pharmacy.
        arabicName := "بنادول اكسترا " + suffix
        var localProduct string
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacy_products (pharmacy_id, origin_type, name_override, selling_price, min_stock_level, is_active)
                VALUES ($1, 'local', $2, 1500, 0, true) RETURNING id::text
        `, pharmacyID, arabicName).Scan(&localProduct))
        _, err = pool.Exec(ctx, `
                INSERT INTO inventory_batches (pharmacy_product_id, branch_id, batch_number, quantity, unit, cost_per_unit, received_date)
                VALUES ($1, $2, $3, 10, 'box', 0, CURRENT_DATE)
        `, localProduct, branchID, "A125-BATCH-LOCAL-"+suffix)
        require.NoError(t, err)

        // 3) a LOCAL batchless product with an Arabic name — pre-fix the
        //    synthetic no_stock row itself 500'd through gp.id::text on the
        //    LEFT JOIN miss (and carried a blank name).
        var batchlessProduct string
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacy_products (pharmacy_id, origin_type, name_override, selling_price, min_stock_level, is_active)
                VALUES ($1, 'local', $2, 1200, 0, true) RETURNING id::text
        `, pharmacyID, "بروفين محلي "+suffix).Scan(&batchlessProduct))
        _ = batchlessProduct

        // ---- the default path lists the two batch rows without a 500 --------
        items, total, err := inventoryPage(ctx, pool, pharmacyID, "", 500, 0)
        require.NoError(t, err, "pre-fix: one NULL global_product_id row killed the whole list")
        require.Len(t, items, 2)
        require.EqualValues(t, 2, total, "{data,total} contract: offset=0 returns the real total")

        var localRow map[string]interface{}
        for _, row := range items {
                if row["batch_number"] == "A125-BATCH-LOCAL-"+suffix {
                        localRow = row
                }
        }
        require.NotNil(t, localRow, "the local product's batch row is served")
        require.Equal(t, "", localRow["global_product_id"], "a local row serves '' like the products list — never NULL, never a scan error")
        require.Nil(t, localRow["expiry_date"], "expiry NULL rides the row as JSON null (client-expected)")
        require.Nil(t, localRow["days_until_expiry"])
        require.EqualValues(t, 0, localRow["cost_per_unit_piastres"], "the cost floor is 0 — the schema forbids NULL cost")
        require.EqualValues(t, 0, localRow["total_cost_piastres"])
        require.Equal(t, "normal", localRow["status"])
        require.Equal(t, arabicName, localRow["product_name"], "الاسم العربي من name_override هو الهوية الفعالة في الـview")

        // offset>0 keeps the documented total=-1 signal.
        _, offsetTotal, err := inventoryPage(ctx, pool, pharmacyID, "", 2, 2)
        require.NoError(t, err)
        require.EqualValues(t, -1, offsetTotal, "offset pages skip the count (api_level 121 contract)")

        // البحث العربي على المسار الافتراضي: المسند يصل name_override عبر الـview.
        found, totalFound, err := inventoryPage(ctx, pool, pharmacyID, "بنادول", 500, 0)
        require.NoError(t, err)
        require.Len(t, found, 1, "the Arabic term finds exactly the local batch row")
        require.EqualValues(t, 1, totalFound)
        require.Equal(t, arabicName, found[0]["product_name"])

        // ---- include=unstocked: the batchless local product surfaces --------
        combined, combinedTotal, err := inventoryPageExtended(ctx, pool, pharmacyID, "", 500, 0, true)
        require.NoError(t, err)
        require.Len(t, combined, 3, "two batch rows + one synthetic no_stock row")
        require.EqualValues(t, 3, combinedTotal)
        _, offsetTotal, err = inventoryPageExtended(ctx, pool, pharmacyID, "", 2, 2, true)
        require.NoError(t, err)
        require.EqualValues(t, -1, offsetTotal)

        var syntheticRow map[string]interface{}
        for _, row := range combined {
                if row["status"] == "no_stock" {
                        syntheticRow = row
                }
        }
        require.NotNil(t, syntheticRow)
        require.Nil(t, syntheticRow["batch_id"], "synthetic rows carry a NULL batch_id (the mobile upsert key)")
        require.Equal(t, "", syntheticRow["global_product_id"], "pre-fix: gp.id::text NULL on a local product killed the union path too")
        require.Equal(t, "بروفين محلي "+suffix, syntheticRow["product_name"], "الهوية الفعالة: override المحلي أولًا — ليس اسمًا فارغًا")
        require.EqualValues(t, 1200, syntheticRow["selling_price_piastres"], "the product's official price rides the synthetic row")

        // البحث العربي يصل الصف الاصطناعي بنفس المسند (كان مستحيلًا باسم فارغ).
        foundSynth, totalSynth, err := inventoryPageExtended(ctx, pool, pharmacyID, "بروفين", 500, 0, true)
        require.NoError(t, err)
        require.Len(t, foundSynth, 1)
        require.EqualValues(t, 1, totalSynth)
}
