package handlers

import (
        "compress/gzip"
        "context"
        "encoding/json"
        "log"
        "net/http"
        "strconv"
        "strings"

        "github.com/gin-gonic/gin"
        "github.com/jackc/pgx/v5/pgxpool"
)

// POS barcode map (api_level 120) — the scan-speed lever for the cashier.
//
// WHY IT EXISTS: the scan-to-cart path pays one server round trip per scan,
// and on the production topology (browser → Vercel rewrite → backend →
// PostgreSQL) that trip costs 300-700ms even with perfect SQL — the user
// experiences «المنتج بيتأخر حوالي ثانية». No query tuning can break that
// floor; the only way to a blink-of-the-eye scan is answering from the
// BROWSER. This endpoint publishes the pharmacy's {barcode → product} map
// once (and then only on catalog change via ETag/304) so the POS resolves
// scans locally in ~0ms and falls back to the network lookup only on a
// cache miss.
//
// WHY IT IS SAFE TO CACHE:
//   - Money never comes from the map. CreatePOSSale re-reads authoritative
//     prices inside the checkout transaction (LoadPOSPricingBatch) — the
//     web client sends product+unit+quantity only, so a stale price in the
//     map can never be billed, it can only be displayed a moment longer.
//   - Stock in the map is display-only. The checkout's locked sweep is the
//     stock authority (insufficient_stock 409). The client decrements the
//     map locally after each of its own sales and refetches on focus.
//
// FRESHNESS CONTRACT: version = active-product count + MAX(updated_at).
// Every identity/price write path (product edit, barcode generate/replace,
// library import price sync) bumps updated_at, so the version moves exactly
// when the cached identity/price data could be stale. Stock writes do NOT
// move it — that is what makes 304 revalidation possible between catalog
// changes.
//
// SHAPE: items mirror the LookupPOSProduct wire object field-for-field
// (same effective-identity COALESCE, same SUM(current_inventory) stock
// semantics) so the client treats a cache hit and a network hit identically.
// Products without an effective barcode cannot be scanned and are absent.

// posBarcodeMapLimit is a self-defense cap, not a feature: a catalog beyond
// ~25k products would risk the Vercel response ceiling; overflow products
// simply miss the cache and take the (always available) network path.
const posBarcodeMapLimit = 25000

type posBarcodeMapItem struct {
        Barcode       string
        ID            string
        Name          string
        GenericName   string
        Strength      string
        BarcodeType   string
        PackagingType string
        UnitsPerBox   int64
        SellingPrice  int64
        PartialPrice  int64
        Stock         int64
}

// posBarcodeMapVersion is the catalog epoch: count + MAX(updated_at) of the
// pharmacy's active products. Deterministic, cheap (one index-only-ish scan
// over pharmacy_id), and stable across stock-only activity. The timestamp
// keeps FULL microsecond precision on purpose: an int-second epoch would let
// two edits inside the same wall-clock second move updated_at without moving
// the version, and a 304 could then hide a genuine price change forever.
func posBarcodeMapVersion(ctx context.Context, db *pgxpool.Pool, pharmacyID string) (string, error) {
        var count int64
        var maxUpdated float64
        err := db.QueryRow(ctx, `
                SELECT COUNT(*)::int8,
                       COALESCE(EXTRACT(EPOCH FROM MAX(pp.updated_at))::float8, 0)
                FROM pharmacy_products pp
                WHERE pp.pharmacy_id = $1 AND pp.is_active = true
        `, pharmacyID).Scan(&count, &maxUpdated)
        if err != nil {
                return "", err
        }
        return "v" + strconv.FormatInt(count, 10) + "." + strconv.FormatFloat(maxUpdated, 'f', 6, 64), nil
}

// loadPOSBarcodeMap returns the version plus every active, barcode-carrying
// product of the pharmacy. The stock SUM mirrors LookupPOSProduct verbatim —
// a cache hit and a network hit must never disagree on a number.
func loadPOSBarcodeMap(ctx context.Context, db *pgxpool.Pool, pharmacyID string, limit int) (string, []posBarcodeMapItem, error) {
        version, err := posBarcodeMapVersion(ctx, db, pharmacyID)
        if err != nil {
                return "", nil, err
        }

        rows, err := db.Query(ctx, `
                SELECT COALESCE(pp.barcode_override, gp.barcode)::text AS barcode,
                       pp.id::text,
                       COALESCE(pp.name_override, gp.name::text, ''),
                       COALESCE(pp.generic_name_override, gp.generic_name::text, ''),
                       COALESCE(pp.strength_override, gp.strength::text, ''),
                       COALESCE(pp.barcode_type_override, gp.barcode_type::text, ''),
                       COALESCE(pp.packaging_type::text, ''),
                       COALESCE(pp.units_per_box::int8, 1),
                       pp.selling_price::int8,
                       COALESCE(pp.partial_selling_price::int8, 0),
                       ROUND(COALESCE(SUM(ci.quantity), 0))::int8
                FROM pharmacy_products pp
                LEFT JOIN global_products gp ON gp.id = pp.global_product_id
                LEFT JOIN current_inventory ci ON ci.pharmacy_product_id = pp.id
                WHERE pp.pharmacy_id = $1
                  AND pp.is_active = true
                  AND (gp.id IS NULL OR gp.is_active = true)
                  AND COALESCE(pp.barcode_override, gp.barcode) <> ''
                GROUP BY pp.id, pp.name_override, gp.name, pp.generic_name_override, gp.generic_name,
                         pp.strength_override, gp.strength, pp.barcode_override, gp.barcode,
                         pp.barcode_type_override, gp.barcode_type, pp.packaging_type,
                         pp.units_per_box, pp.selling_price, pp.partial_selling_price
                ORDER BY pp.id
                LIMIT $2
        `, pharmacyID, limit)
        if err != nil {
                return "", nil, err
        }
        defer rows.Close()

        items := make([]posBarcodeMapItem, 0, 64)
        for rows.Next() {
                var it posBarcodeMapItem
                if err := rows.Scan(&it.Barcode, &it.ID, &it.Name, &it.GenericName, &it.Strength,
                        &it.BarcodeType, &it.PackagingType, &it.UnitsPerBox, &it.SellingPrice,
                        &it.PartialPrice, &it.Stock); err != nil {
                        return "", nil, err
                }
                items = append(items, it)
        }
        return version, items, rows.Err()
}

// posBarcodeMapETag is a WEAK etag on purpose: the gzip and identity
// representations of the same version are semantically equal, and weak
// comparison is exactly the RFC contract for that.
func posBarcodeMapETag(version string) string {
        return `W/"pos-bmap-` + version + `"`
}

// GetPOSBarcodeMap serves GET /pharmacy/pos/barcode-map (perm pos.access).
// Conditional GET with If-None-Match returns 304 when the catalog epoch is
// unchanged; the body gzips when the client offers it (the map is the one
// multi-hundred-KB read in POS — stdlib gzip, no middleware fan-out).
func (h *Handler) GetPOSBarcodeMap(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }

        version, items, err := loadPOSBarcodeMap(c.Request.Context(), h.db, pharmacyID, posBarcodeMapLimit)
        if err != nil {
                log.Printf("[POS_BARCODE_MAP] load error: %v", err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "pos_barcode_map_failed", "message": "تعذر تحضير خريطة الباركود"})
                return
        }

        etag := posBarcodeMapETag(version)
        c.Header("ETag", etag)
        c.Header("Cache-Control", "private, no-cache")
        c.Header("Vary", "Accept-Encoding")
        if c.GetHeader("If-None-Match") == etag {
                c.Status(http.StatusNotModified)
                return
        }

        wire := make([]gin.H, 0, len(items))
        for _, it := range items {
                wire = append(wire, gin.H{
                        "id":                             it.ID,
                        "name":                           it.Name,
                        "generic_name":                   it.GenericName,
                        "strength":                       it.Strength,
                        "barcode":                        it.Barcode,
                        "barcode_type":                   it.BarcodeType,
                        "packaging_type":                 it.PackagingType,
                        "units_per_box":                  it.UnitsPerBox,
                        "selling_price_piastres":         it.SellingPrice,
                        "partial_selling_price_piastres": it.PartialPrice,
                        "stock":                          it.Stock,
                })
        }
        payload := gin.H{"data": gin.H{"version": version, "items": wire}}

        if !strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
                c.JSON(http.StatusOK, payload)
                return
        }

        c.Header("Content-Encoding", "gzip")
        c.Header("Content-Type", "application/json; charset=utf-8")
        c.Status(http.StatusOK)
        gz := gzip.NewWriter(c.Writer)
        encodeErr := json.NewEncoder(gz).Encode(payload)
        closeErr := gz.Close()
        if encodeErr != nil || closeErr != nil {
                // Headers and a partial body may already be gone; nothing sound can
                // be sent after a broken stream — log for observability only.
                log.Printf("[POS_BARCODE_MAP] gzip encode error: %v / %v", encodeErr, closeErr)
        }
}
