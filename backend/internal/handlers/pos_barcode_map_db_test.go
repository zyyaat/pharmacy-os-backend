package handlers

import (
        "compress/gzip"
        "context"
        "encoding/json"
        "fmt"
        "io"
        "net/http"
        "net/http/httptest"
        "os"
        "strings"
        "testing"
        "time"

        "github.com/gin-gonic/gin"
        "github.com/jackc/pgx/v5"
        "github.com/jackc/pgx/v5/pgxpool"
        "github.com/stretchr/testify/require"

        "github.com/pharmacy-os/backend/internal/auth"
)

// Task 120 gate for the POS barcode map — the local-cache feed that makes
// scanner checkout instant. The gate drives loadPOSBarcodeMap + posBarcodeMapVersion
// — the same functions the HTTP handler serves — against a real migrated
// schema, so the SQL under test cannot drift from what production serves.
//
// Pinned:
//   - every ACTIVE product with a NON-EMPTY effective barcode is present,
//     shaped exactly like the LookupPOSProduct wire object,
//   - the effective identity rules: local barcode_override wins over the
//     official barcode (migration 48 COALESCE),
//   - inactive pharmacy rows, inactive catalog rows, barcode-less products
//     and foreign pharmacies never appear,
//   - stock is the SUM across the product's batches (multi-batch product),
//     byte-identical semantics to the network lookup path,
//   - the version epoch moves when the catalog changes (count / updated_at)
//     so client 304 revalidation can never hide a price or identity edit,
//   - LIMIT truncates deterministically (overflow products miss the cache
//     and fall back to the network path — degraded speed, never wrong data).
//
// Same harness as the other DB gates:
//
//      TEST_DATABASE_URL=postgres://.../pharmacy_scratch go test ./internal/handlers/ -run TestPOSBarcodeMapDB
func posBarcodeMapPool(t *testing.T) *pgxpool.Pool {
        t.Helper()
        dsn := os.Getenv("TEST_DATABASE_URL")
        if dsn == "" {
                t.Skip("TEST_DATABASE_URL not set — barcode map gates need a disposable PostgreSQL")
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

func TestPOSBarcodeMapDB(t *testing.T) {
        ctx := context.Background()
        pool := posBarcodeMapPool(t)
        suffix := fmt.Sprintf("%d", time.Now().UnixNano())

        // ---- fixtures (same chain as the other handler gates) ----------------
        var accountID, pharmacyID, branchID string
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO accounts (company_name, contact_email, status)
                VALUES ($1, $2, 'active') RETURNING id::text
        `, "bmap-gate "+suffix, "bmapgate-"+suffix+"@test.local").Scan(&accountID))
        t.Cleanup(func() {
                _, _ = pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, accountID)
        })
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacies (account_id, name, license_number, is_active)
                VALUES ($1, $2, $3, true) RETURNING id::text
        `, accountID, "bmap-gate-ph "+suffix, "BMAP-"+suffix).Scan(&pharmacyID))
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO branches (pharmacy_id, name, code, is_active)
                VALUES ($1, 'main', $2, true) RETURNING id::text
        `, pharmacyID, "BMAPB-"+suffix).Scan(&branchID))

        var catalogIDs []string
        insertGlobal := func(name, barcode string, active bool) string {
                var id string
                require.NoError(t, pool.QueryRow(ctx, `
                        INSERT INTO global_products (name, generic_name, dosage_form, strength, product_category, default_unit, barcode, barcode_type, is_active)
                        VALUES ($1, 'gate', 'tablet', '5mg', 'medication', 'tablet', NULLIF($2, ''), 'GTIN_EAN13', $3) RETURNING id::text
                `, name, barcode, active).Scan(&id))
                catalogIDs = append(catalogIDs, id)
                return id
        }
        t.Cleanup(func() {
                _, _ = pool.Exec(context.Background(), `DELETE FROM global_products WHERE id = ANY($1)`, catalogIDs)
        })
        insertProduct := func(globalID string, barcodeOverride string, active bool) string {
                var id string
                require.NoError(t, pool.QueryRow(ctx, `
                        INSERT INTO pharmacy_products (pharmacy_id, global_product_id, cost_price, selling_price, partial_selling_price, packaging_type, units_per_box, min_stock_level, barcode_override, barcode_type_override, is_active)
                        VALUES ($1, $2, 1000, 1500, 300, 'BOX_STRIP', 10, 0, NULLIF($3, ''), CASE WHEN $3 = '' THEN NULL ELSE 'RCN_EAN13' END, $4) RETURNING id::text
                `, pharmacyID, globalID, barcodeOverride, active).Scan(&id))
                return id
        }
        insertBatch := func(productID, batchNumber string, quantity int64) {
                _, err := pool.Exec(ctx, `
                        INSERT INTO inventory_batches (pharmacy_product_id, branch_id, batch_number, quantity, unit, cost_per_unit, expiry_date, received_date)
                        VALUES ($1, $2, $3, $4, 'box', 1000, '2028-01-01', CURRENT_DATE)
                `, productID, branchID, batchNumber, quantity)
                require.NoError(t, err)
        }

        // 1) official barcode only.
        officialGlobal := insertGlobal("BMAP-OFFICIAL-"+suffix, fmt.Sprintf("62200000000%s", suffix[len(suffix)-4:]), true)
        officialProduct := insertProduct(officialGlobal, "", true)
        insertBatch(officialProduct, "BMAP-B1", 10)
        insertBatch(officialProduct, "BMAP-B2", 5)

        // 2) local override wins over the official code.
        overrideGlobal := insertGlobal("BMAP-OVERRIDE-"+suffix, fmt.Sprintf("62211111111%s", suffix[len(suffix)-4:]), true)
        overrideProduct := insertProduct(overrideGlobal, fmt.Sprintf("20000000000%s", suffix[len(suffix)-4:]), true)
        insertBatch(overrideProduct, "BMAP-B3", 7)

        // 3) barcode-less, inactive product, inactive catalog row — all excluded.
        insertProduct(insertGlobal("BMAP-NOBARCODE-"+suffix, "", true), "", true)
        insertProduct(insertGlobal("BMAP-INACTIVE-"+suffix, fmt.Sprintf("62233333333%s", suffix[len(suffix)-4:]), true), "", false)
        insertProduct(insertGlobal("BMAP-CATOFF-"+suffix, fmt.Sprintf("62244444444%s", suffix[len(suffix)-4:]), false), "", true)

        // ---- contract --------------------------------------------------------
        version, items, err := loadPOSBarcodeMap(ctx, pool, pharmacyID, posBarcodeMapLimit)
        require.NoError(t, err)
        require.Len(t, items, 2, "only active products with an effective barcode appear")

        byBarcode := make(map[string]posBarcodeMapItem, len(items))
        for _, it := range items {
                byBarcode[it.Barcode] = it
        }

        officialBarcode := fmt.Sprintf("62200000000%s", suffix[len(suffix)-4:])
        overrideBarcode := fmt.Sprintf("20000000000%s", suffix[len(suffix)-4:])
        official, ok := byBarcode[officialBarcode]
        require.True(t, ok, "official-barcode product present under its official code")
        require.Equal(t, "BMAP-OFFICIAL-"+suffix, official.Name)
        require.EqualValues(t, 15, official.Stock, "stock sums every batch of the product (10+5)")
        require.EqualValues(t, 1500, official.SellingPrice)
        require.EqualValues(t, 300, official.PartialPrice)
        require.EqualValues(t, 10, official.UnitsPerBox)
        require.Equal(t, "BOX_STRIP", official.PackagingType)
        require.Equal(t, "GTIN_EAN13", official.BarcodeType)

        overridden, ok := byBarcode[overrideBarcode]
        require.True(t, ok, "override-barcode product present under its LOCAL code")
        require.Equal(t, "BMAP-OVERRIDE-"+suffix, overridden.Name)
        require.Equal(t, "RCN_EAN13", overridden.BarcodeType, "the effective barcode type follows the override")
        require.EqualValues(t, 7, overridden.Stock)
        _, collision := byBarcode[fmt.Sprintf("62211111111%s", suffix[len(suffix)-4:])]
        require.False(t, collision, "the official code hidden by an override must NOT resolve")

        _, leak := byBarcode["BMAP-NOBARCODE-"+suffix]
        require.False(t, leak, "barcode-less products cannot be scanned and are absent")
        require.NotContains(t, fmt.Sprint(items), "BMAP-INACTIVE-"+suffix, "inactive pharmacy rows excluded")
        require.NotContains(t, fmt.Sprint(items), "BMAP-CATOFF-"+suffix, "inactive catalog rows excluded")

        // ---- version epoch ---------------------------------------------------
        require.Contains(t, version, "v", "version is the catalog epoch count+max(updated_at)")

        versionBefore := version
        // A price edit bumps updated_at → the epoch must move, or a 304 could
        // hide a real price change from the cashier's cache.
        _, err = pool.Exec(ctx, `
                UPDATE pharmacy_products SET selling_price = 2000, updated_at = NOW() WHERE id = $1
        `, officialProduct)
        require.NoError(t, err)
        _, itemsAfterPrice, err := loadPOSBarcodeMap(ctx, pool, pharmacyID, posBarcodeMapLimit)
        require.NoError(t, err)
        versionAfterPrice, err := posBarcodeMapVersion(ctx, pool, pharmacyID)
        require.NoError(t, err)
        require.NotEqual(t, versionBefore, versionAfterPrice, "price edit moves the version epoch")
        for _, it := range itemsAfterPrice {
                if it.Barcode == officialBarcode {
                        require.EqualValues(t, 2000, it.SellingPrice, "map reload reflects the edited price")
                }
        }

        // Deactivating a product moves count → the epoch moves again.
        _, err = pool.Exec(ctx, `
                UPDATE pharmacy_products SET is_active = false WHERE id = $1
        `, overrideProduct)
        require.NoError(t, err)
        versionAfterDeactivate, err := posBarcodeMapVersion(ctx, pool, pharmacyID)
        require.NoError(t, err)
        require.NotEqual(t, versionAfterPrice, versionAfterDeactivate, "deactivation moves the version epoch")

        // ---- foreign pharmacy isolation --------------------------------------
        var otherAccount, otherPharmacy, otherBranch string
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO accounts (company_name, contact_email, status)
                VALUES ($1, $2, 'active') RETURNING id::text
        `, "bmap-gate-other "+suffix, "bmapgate-other-"+suffix+"@test.local").Scan(&otherAccount))
        t.Cleanup(func() {
                _, _ = pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, otherAccount)
        })
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacies (account_id, name, license_number, is_active)
                VALUES ($1, $2, $3, true) RETURNING id::text
        `, otherAccount, "bmap-gate-other-ph "+suffix, "BMAPO-"+suffix).Scan(&otherPharmacy))
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO branches (pharmacy_id, name, code, is_active)
                VALUES ($1, 'main', $2, true) RETURNING id::text
        `, otherPharmacy, "BMAPOB-"+suffix).Scan(&otherBranch))
        foreignGlobal := insertGlobal("BMAP-FOREIGN-"+suffix, fmt.Sprintf("62255555555%s", suffix[len(suffix)-4:]), true)
        // The catalog row is shared; the foreign pharmacy owns a product on it.
        var foreignProduct string
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacy_products (pharmacy_id, global_product_id, cost_price, selling_price, min_stock_level, is_active)
                VALUES ($1, $2, 1000, 1500, 0, true) RETURNING id::text
        `, otherPharmacy, foreignGlobal).Scan(&foreignProduct))
        insertBatch(foreignProduct, "BMAP-FOREIGN-B1", 3)

        _, ownItems, err := loadPOSBarcodeMap(ctx, pool, pharmacyID, posBarcodeMapLimit)
        require.NoError(t, err)
        require.NotContains(t, fmt.Sprint(ownItems), "BMAP-FOREIGN-"+suffix, "foreign pharmacy rows never leak")

        // ---- limit truncation degrades to the network path, never to error ---
        _, limited, err := loadPOSBarcodeMap(ctx, pool, pharmacyID, 1)
        require.NoError(t, err)
        require.Len(t, limited, 1, "LIMIT truncates deterministically")
}

// HTTP-layer gate for the same endpoint: the ETag/304 handshake and the gzip
// body are what the browser actually consumes (through the Vercel rewrite),
// so they are pinned here against a real migrated schema — the SQL behind
// them is pinned by TestPOSBarcodeMapDB above.
func TestPOSBarcodeMapHTTP(t *testing.T) {
        ctx := context.Background()
        pool := posBarcodeMapPool(t)
        suffix := fmt.Sprintf("%d", time.Now().UnixNano())

        var accountID, pharmacyID, branchID string
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO accounts (company_name, contact_email, status)
                VALUES ($1, $2, 'active') RETURNING id::text
        `, "bmap-http " + suffix, "bmaphttp-" + suffix + "@test.local").Scan(&accountID))
        t.Cleanup(func() {
                _, _ = pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, accountID)
        })
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacies (account_id, name, license_number, is_active)
                VALUES ($1, $2, $3, true) RETURNING id::text
        `, accountID, "bmap-http-ph " + suffix, "BMAPH-" + suffix).Scan(&pharmacyID))
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO branches (pharmacy_id, name, code, is_active)
                VALUES ($1, 'main', $2, true) RETURNING id::text
        `, pharmacyID, "BMAPHB-" + suffix).Scan(&branchID))

        var globalID, productID string
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO global_products (name, generic_name, dosage_form, strength, product_category, default_unit, barcode, barcode_type, is_active)
                VALUES ($1, 'gate', 'tablet', '5mg', 'medication', 'tablet', $2, 'GTIN_EAN13', true) RETURNING id::text
        `, "BMAP-HTTP-" + suffix, "62299900000" + suffix[len(suffix)-3:]).Scan(&globalID))
        t.Cleanup(func() {
                _, _ = pool.Exec(context.Background(), `DELETE FROM global_products WHERE id = $1`, globalID)
        })
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacy_products (pharmacy_id, global_product_id, cost_price, selling_price, min_stock_level, is_active)
                VALUES ($1, $2, 1000, 1500, 0, true) RETURNING id::text
        `, pharmacyID, globalID).Scan(&productID))
        _, err := pool.Exec(ctx, `
                INSERT INTO inventory_batches (pharmacy_product_id, branch_id, batch_number, quantity, unit, cost_per_unit, expiry_date, received_date)
                VALUES ($1, $2, 'BMAPH-B1', 4, 'box', 1000, '2028-01-01', CURRENT_DATE)
        `, productID, branchID)
        require.NoError(t, err)

        gin.SetMode(gin.TestMode)
        h := &Handler{db: pool}
        route := func() *gin.Engine {
                r := gin.New()
                r.GET("/api/v1/pharmacy/pos/barcode-map",
                        principalStub(&auth.Principal{ID: "emp-1", Type: auth.EmployeePrincipal, PharmacyID: pharmacyID}),
                        h.GetPOSBarcodeMap)
                return r
        }

        // 1) plain GET → 200 JSON with a weak catalog ETag and the item inside.
        w := httptest.NewRecorder()
        route().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/pharmacy/pos/barcode-map", nil))
        require.Equal(t, http.StatusOK, w.Code)
        etag := w.Header().Get("ETag")
        require.True(t, strings.HasPrefix(etag, `W/"pos-bmap-v`), "weak catalog etag served, got %q", etag)
        require.NotEmpty(t, w.Header().Get("Cache-Control"))
        var plain struct {
                Data struct {
                        Version string `json:"version"`
                        Items   []struct {
                                Barcode string `json:"barcode"`
                                Stock   int64  `json:"stock"`
                        } `json:"items"`
                } `json:"data"`
        }
        require.NoError(t, json.Unmarshal(w.Body.Bytes(), &plain))
        require.Len(t, plain.Data.Items, 1)
        require.EqualValues(t, 4, plain.Data.Items[0].Stock)

        // 2) conditional GET with the served etag → 304, zero bytes.
        w304 := httptest.NewRecorder()
        req304 := httptest.NewRequest(http.MethodGet, "/api/v1/pharmacy/pos/barcode-map", nil)
        req304.Header.Set("If-None-Match", etag)
        route().ServeHTTP(w304, req304)
        require.Equal(t, http.StatusNotModified, w304.Code)
        require.Empty(t, w304.Body.String(), "304 carries no body")

        // 3) gzip-offering client → Content-Encoding: gzip with decodable JSON.
        wgz := httptest.NewRecorder()
        reqgz := httptest.NewRequest(http.MethodGet, "/api/v1/pharmacy/pos/barcode-map", nil)
        reqgz.Header.Set("Accept-Encoding", "gzip")
        route().ServeHTTP(wgz, reqgz)
        require.Equal(t, http.StatusOK, wgz.Code)
        require.Equal(t, "gzip", wgz.Header().Get("Content-Encoding"))
        require.Equal(t, etag, wgz.Header().Get("ETag"), "the same catalog version serves the same etag across encodings")
        zr, err := gzip.NewReader(wgz.Body)
        require.NoError(t, err)
        decoded, err := io.ReadAll(zr)
        require.NoError(t, err)
        var fromGzip struct {
                Data struct {
                        Items []struct {
                                Barcode string `json:"barcode"`
                        } `json:"items"`
                } `json:"data"`
        }
        require.NoError(t, json.Unmarshal(decoded, &fromGzip))
        require.Len(t, fromGzip.Data.Items, 1, "gzipped body decodes to the same item set")

        // 4) a pharmacy with no catalog answers honestly-empty with its own etag
        //    (the map endpoint must never fabricate rows for an empty tenant).
        var otherAccount, otherPharmacy string
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO accounts (company_name, contact_email, status)
                VALUES ($1, $2, 'active') RETURNING id::text
        `, "bmap-http-other " + suffix, "bmaphttp-other-" + suffix + "@test.local").Scan(&otherAccount))
        t.Cleanup(func() {
                _, _ = pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, otherAccount)
        })
        require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacies (account_id, name, license_number, is_active)
                VALUES ($1, $2, $3, true) RETURNING id::text
        `, otherAccount, "bmap-http-other-ph " + suffix, "BMAPHO-" + suffix).Scan(&otherPharmacy))

        empty := gin.New()
        empty.GET("/api/v1/pharmacy/pos/barcode-map",
                principalStub(&auth.Principal{ID: "emp-2", Type: auth.EmployeePrincipal, PharmacyID: otherPharmacy}),
                h.GetPOSBarcodeMap)
        wEmpty := httptest.NewRecorder()
        empty.ServeHTTP(wEmpty, httptest.NewRequest(http.MethodGet, "/api/v1/pharmacy/pos/barcode-map", nil))
        require.Equal(t, http.StatusOK, wEmpty.Code)
        require.Contains(t, wEmpty.Body.String(), `"items":[]`)
        require.NotEqual(t, etag, wEmpty.Header().Get("ETag"), "an empty catalog carries its own etag")
}
