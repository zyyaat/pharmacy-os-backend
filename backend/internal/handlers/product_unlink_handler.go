package handlers

// D3 + D4 of the catalog audit (api_level 122, migration 53), completing the
// pair migration 48/49 started (D2 effective identity + D5 invoice snapshots):
//
//   D3 — pharmacy_product_price_history: the append-only selling-price
//        ledger. Until this release an edit erased the previous price
//        forever ("what did we sell this at last month" was unanswerable).
//        Every writer that changes a selling price records one row inside
//        its own transaction through recordPriceChangeTx, so the history is
//        committed (or discarded) atomically with the change it describes.
//
//   D4 — Unlink: the deliberate detachment of a pharmacy product from the
//        shared catalog row. After it, the product is a REAL local product
//        (origin_type='local', global_product_id NULL): its identity stops
//        being shared, future catalog events by anyone else can never touch
//        it, and library syncs stop re-linking it by catalog id. The three
//        safety legs, in order:
//          1. explicit confirm flag (the request body must carry it),
//          2. the reference-freeze: every historical sale line of this
//             product gets its name/barcode/strength frozen into migration
//             49's snapshot columns inside the same transaction (the
//             snapshots are what make detachment safe for invoice history —
//             legacy NULL rows would otherwise fall back to a join that can
//             no longer resolve),
//          3. a full audit row with the detached catalog identity and the
//             frozen-row counts.
//
// The detachment reuses migration 48's columns as the tenant-owned identity
// slice: the effective display name freezes into name_override (required by
// the D1 invariant), the effective barcode into barcode_override /
// barcode_type_override. Prices, packaging, stock settings and history stay
// untouched — only the identity stops being shared.

import (
        "context"
        "errors"
        "net/http"
        "strings"

        "github.com/gin-gonic/gin"
        "github.com/jackc/pgx/v5"

        "github.com/pharmacy-os/backend/internal/auth"
        "github.com/pharmacy-os/backend/internal/money"
)

// Price-history change sources — the locked vocabulary of
// pharmacy_product_price_history.change_source (migration 53's CHECK).
const (
        priceSourceProductCreate = "product_create"
        priceSourceManualEdit    = "manual_edit"
        priceSourceImport        = "import"
        priceSourceLibrarySync   = "library_sync"
)

// recordPriceChangeTx writes one pharmacy_product_price_history row inside
// the caller's transaction. Semantics:
//
//   - oldSelling == nil means "creation baseline" (the birth of the
//     product): the row is ALWAYS written, old columns NULL.
//   - otherwise the row is written only when something actually changed —
//     a re-save with the same price must not pollute the history.
//   - the partial (strip) price rides along: a change to either the box or
//     the strip price is one price event.
//
// A history write failure FAILS the caller's transaction on purpose: these
// price paths are transactional mutations, and a silently lost price event
// would make the ledger lie.
func recordPriceChangeTx(
        ctx context.Context,
        tx pgx.Tx,
        pharmacyID, productID string,
        oldSelling, newSelling money.Piastres,
        oldPartial, newPartial *int64,
        source string,
        principal *auth.Principal,
) error {
        baseline := source == priceSourceProductCreate
        if !baseline && oldSelling == newSelling && int64PtrEqual(oldPartial, newPartial) {
                return nil
        }

        employeeID, companyUserID := auth.ActorIDs(principal)
        var oldSellingArg, oldPartialArg, newPartialArg any
        if !baseline {
                oldSellingArg = oldSelling
        }
        if oldPartial != nil {
                oldPartialArg = *oldPartial
        }
        if newPartial != nil {
                newPartialArg = *newPartial
        }

        if _, err := tx.Exec(ctx, `
                INSERT INTO pharmacy_product_price_history (
                        pharmacy_id, pharmacy_product_id,
                        old_selling_price, new_selling_price,
                        old_partial_selling_price, new_partial_selling_price,
                        change_source, changed_by_employee_id, changed_by_company_user_id
                ) VALUES (
                        $1::uuid, $2::uuid, $3, $4, $5, $6, $7,
                        NULLIF($8, '')::uuid, NULLIF($9, '')::uuid
                )
        `, pharmacyID, productID, oldSellingArg, newSelling, oldPartialArg, newPartialArg,
                source, employeeID, companyUserID); err != nil {
                return err
        }
        return nil
}

// PiastresOrZero converts a nullable price column scan into a Piastres
// value (NULL = 0 for the history comparison).
func PiastresOrZero(p *int64) money.Piastres {
        if p == nil {
                return 0
        }
        return money.Piastres(*p)
}

func int64PtrEqual(a, b *int64) bool {
        if a == nil && b == nil {
                return true
        }
        if a == nil || b == nil {
                return false
        }
        return *a == *b
}

// ---------------------------------------------------------------------------
// D4 — Unlink
// ---------------------------------------------------------------------------

type unlinkPharmacyProductRequest struct {
        // Confirm is the deliberate-decision gate: a plain POST without
        // {"confirm": true} is rejected before any write. The frontend
        // dialog (and any API consumer) must opt in explicitly.
        Confirm bool `json:"confirm"`
}

// unlinkFailure carries the (status, code, message) triple the handler maps
// onto the JSON error contract, so the transactional core stays free of the
// HTTP layer and the DB regression gate can drive the exact same path.
type unlinkFailure struct {
        status  int
        code    string
        message string
}

// UnlinkPharmacyProduct handles POST /pharmacy/products/:id/unlink.
// Detaches a central (catalog-linked) product of the authenticated pharmacy
// from the shared global_products row and turns it into a real LOCAL
// product. Nothing is deleted: batches, stock, sales, price history and the
// audit trail all keep pointing at the same pharmacy_products row — only
// the catalog identity stops being shared.
func (h *Handler) UnlinkPharmacyProduct(c *gin.Context) {
        principal, ok := auth.PrincipalFromContext(c)
        if !ok || principal.PharmacyID == "" || principal.ID == "" {
                c.JSON(http.StatusForbidden, gin.H{"error": "pharmacy_mutation_account_required", "message": "حساب مدير أو موظف صيدلية مطلوب"})
                return
        }

        var request unlinkPharmacyProductRequest
        if err := c.ShouldBindJSON(&request); err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request", "message": "بيانات الطلب غير صحيحة"})
                return
        }

        tx, err := h.db.Begin(c.Request.Context())
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "unlink_failed", "message": "تعذر بدء عملية الفصل"})
                return
        }
        defer func() { _ = tx.Rollback(c.Request.Context()) }()

        if _, err := tx.Exec(c.Request.Context(), `
                SELECT set_config('app.current_pharmacy_id', $1, true),
                       set_config('app.current_user_id', $2, true)
        `, principal.PharmacyID, principal.ID); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "unlink_failed", "message": "تعذر إعداد نطاق الصيدلية"})
                return
        }

        data, failure := unlinkPharmacyProductTx(c.Request.Context(), tx,
                principal, idFromParam(c, "id"), request.Confirm)
        if failure != nil {
                c.JSON(failure.status, gin.H{"error": failure.code, "message": failure.message})
                return
        }
        if err := tx.Commit(c.Request.Context()); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "unlink_failed", "message": "تعذر تأكيد الفصل"})
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": data})
}

// unlinkPharmacyProductTx is the transactional core of D4. On success it
// returns the response payload; on failure a *unlinkFailure. The caller owns
// the commit/rollback.
func unlinkPharmacyProductTx(
        ctx context.Context,
        tx pgx.Tx,
        principal *auth.Principal,
        productID string,
        confirm bool,
) (gin.H, *unlinkFailure) {
        // Lock the product row so a concurrent edit/generate/import cannot
        // interleave with the detachment. The catalog row is only READ here
        // (its effective identity freezes onto the tenant row) and is never
        // written by the unlink — PostgreSQL rejects FOR UPDATE on the
        // nullable side of an outer join, so the lock clause stays on pp
        // alone.
        var (
                originType, nameOverride, globalProductID                string
                effectiveName, effectiveBarcode, effectiveBarcodeType     string
                gpSource                                                 string
                gpVerified                                               bool
        )
        err := tx.QueryRow(ctx, `
                SELECT pp.origin_type::text, COALESCE(pp.name_override::text, ''),
                       COALESCE(pp.global_product_id::text, ''),
                       COALESCE(pp.name_override::text, gp.name::text, ''),
                       COALESCE(pp.barcode_override::text, gp.barcode::text, ''),
                       COALESCE(pp.barcode_type_override::text, gp.barcode_type::text, ''),
                       COALESCE(gp.source::text, ''), COALESCE(gp.is_verified, false)
                FROM pharmacy_products pp
                LEFT JOIN global_products gp ON gp.id = pp.global_product_id
                WHERE pp.id = $1::uuid AND pp.pharmacy_id = $2::uuid
                FOR UPDATE OF pp
        `, productID, principal.PharmacyID).Scan(&originType, &nameOverride,
                &globalProductID, &effectiveName, &effectiveBarcode, &effectiveBarcodeType,
                &gpSource, &gpVerified)
        if errors.Is(err, pgx.ErrNoRows) {
                return nil, &unlinkFailure{
                        status:  http.StatusNotFound,
                        code:    "product_not_found",
                        message: "المنتج غير موجود في هذه الصيدلية",
                }
        }
        if err != nil {
                return nil, &unlinkFailure{
                        status:  http.StatusInternalServerError,
                        code:    "unlink_failed",
                        message: "تعذر قراءة المنتج",
                }
        }
        if originType != "central" {
                return nil, &unlinkFailure{
                        status:  http.StatusConflict,
                        code:    "already_local",
                        message: "المنتج محلي بالفعل وغير مرتبط بكتالوج المنصة",
                }
        }
        if !confirm {
                return nil, &unlinkFailure{
                        status:  http.StatusBadRequest,
                        code:    "confirm_required",
                        message: "الفصل قرار لا رجعة فيه للربط — أعد الطلب مع confirm=true بعد مراجعة العواقب",
                }
        }

        // Display name the product will carry as a local row: an explicit
        // override wins (it is what the pharmacy's cashiers already see),
        // otherwise the effective catalog name freezes into the local row.
        displayName := effectiveName
        if strings.TrimSpace(nameOverride) != "" {
                displayName = nameOverride
        }

        // Reference snapshot for the audit + response: what the detachment
        // freezes, and what stays untouched by design.
        var (
                saleLinesTotal, saleLinesToFreeze, batchesKept int
        )
        if err := tx.QueryRow(ctx, `
                SELECT
                    (SELECT COUNT(*) FROM sale_items WHERE pharmacy_product_id = $1::uuid),
                    (SELECT COUNT(*) FROM sale_items WHERE pharmacy_product_id = $1::uuid
                        AND product_name_snapshot IS NULL),
                    (SELECT COUNT(*) FROM inventory_batches WHERE pharmacy_product_id = $1::uuid)
        `, productID).Scan(&saleLinesTotal, &saleLinesToFreeze, &batchesKept); err != nil {
                return nil, &unlinkFailure{
                        status:  http.StatusInternalServerError,
                        code:    "unlink_failed",
                        message: "تعذر جرد مراجع المنتج",
                }
        }

        // The reference-freeze: historical invoice lines of this product read
        // their identity from migration 49's snapshot columns with a live-join
        // fallback. After the UPDATE below the join can never resolve again —
        // so the un-snapshotted lines must be frozen in the SAME transaction.
        if saleLinesToFreeze > 0 {
                if _, err := tx.Exec(ctx, `
                        UPDATE sale_items
                        SET product_name_snapshot = $2,
                            product_barcode_snapshot = $3,
                            product_strength_snapshot = NULL
                        WHERE pharmacy_product_id = $1::uuid AND product_name_snapshot IS NULL
                `, productID, displayName, effectiveBarcode); err != nil {
                        return nil, &unlinkFailure{
                                status:  http.StatusInternalServerError,
                                code:    "unlink_failed",
                                message: "تعذر تجميد أسماء الفواتير التاريخية",
                        }
                }
        }

        // The detachment itself. The pharmacy keeps its prices, packaging,
        // stock settings and history untouched — only the identity slice
        // flips to tenant-owned, through migration 48's own columns.
        if _, err = tx.Exec(ctx, `
                UPDATE pharmacy_products SET
                    origin_type = 'local',
                    global_product_id = NULL,
                    name_override = $2,
                    barcode_override = NULLIF($3, ''),
                    barcode_type_override = NULLIF($4, ''),
                    updated_at = NOW()
                WHERE id = $1::uuid
        `, productID, displayName, effectiveBarcode, effectiveBarcodeType); err != nil {
                return nil, &unlinkFailure{
                        status:  http.StatusInternalServerError,
                        code:    "unlink_failed",
                        message: "تعذر فصل المنتج عن الكتالوج",
                }
        }

        // Full audit: what was detached, from where, and what the detachment
        // froze. entity = the detached catalog row (the thing that stopped
        // being shared), payload carries both sides.
        if auditErr := writeAuditLog(ctx, tx, principal,
                "product.unlinked", "update", "global_product", globalProductID,
                map[string]any{
                        "pharmacy_product_id": productID,
                        "detached_global_id":  globalProductID,
                        "effective_name":      effectiveName,
                        "frozen_display_name": displayName,
                        "frozen_barcode":      effectiveBarcode,
                        "catalog_source":      gpSource,
                        "catalog_verified":    gpVerified,
                        "sale_lines_total":    saleLinesTotal,
                        "sale_lines_frozen":   saleLinesToFreeze,
                        "batches_kept":        batchesKept,
                },
                "فصل منتج عن الكتالوج المشترك وتحويله لمنتج محلي: " + displayName); auditErr != nil {
                // Same posture as the barcode audits: losing the audit row must
                // not abort the business mutation the user explicitly
                // confirmed; it is logged loudly instead.
                auditFailure("product.unlinked", auditErr)
        }

        return gin.H{
                "id":                         productID,
                "origin_type":                "local",
                "display_name":               displayName,
                "barcode":                    effectiveBarcode,
                "detached_global_product_id": globalProductID,
                "sale_lines_total":           saleLinesTotal,
                "sale_lines_frozen":          saleLinesToFreeze,
                "batches_kept":               batchesKept,
        }, nil
}

// isLocalProductOrigin reports whether a product row is fully tenant-owned.
func isLocalProductOrigin(originType string) bool {
        return originType == "local"
}

// localBarcodeConflictTx checks whether a manual barcode for a LOCAL product
// collides with an EFFECTIVE barcode of another product in the same pharmacy
// (scan resolution must stay single-answer). One product per scan, enforced
// at every write path: create, edit, unlink (the unique index covers the
// rest).
func localBarcodeConflictTx(ctx context.Context, tx pgx.Tx, pharmacyID, excludeProductID, code string) (bool, error) {
        var otherID string
        err := tx.QueryRow(ctx, `
                SELECT pp.id::text
                FROM pharmacy_products pp
                LEFT JOIN global_products gp ON gp.id = pp.global_product_id
                WHERE pp.pharmacy_id = $1::uuid
                  AND pp.id <> NULLIF($2, '')::uuid
                  AND COALESCE(pp.barcode_override, gp.barcode) = $3
                LIMIT 1
        `, pharmacyID, excludeProductID, code).Scan(&otherID)
        if errors.Is(err, pgx.ErrNoRows) {
                return false, nil
        }
        if err != nil {
                return false, err
        }
        return true, nil
}
