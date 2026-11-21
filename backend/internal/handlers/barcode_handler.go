package handlers

import (
        "errors"
        "net/http"
        "strconv"

        "github.com/gin-gonic/gin"
        "github.com/jackc/pgx/v5"
        "github.com/jackc/pgx/v5/pgconn"

        "github.com/pharmacy-os/backend/internal/auth"
        "github.com/pharmacy-os/backend/internal/barcode"
        "github.com/pharmacy-os/backend/internal/services"
)

// Internal barcode generation (barcode system v1 — Final Decisions 1, 4, 5).
//
// The generated code is an INTERNAL RCN EAN-13 (prefix 20). Generation is
// server-only: nextval() on the platform sequence is atomic and never
// re-issues a value, so generated codes cannot collide with each other.
// The final guarantee for ALL writes (generated and manual) stays the
// existing unique partial index idx_global_products_unique_barcode.
//
// Every generation and replacement is audit-logged (barcode.*) through the
// audit_logs table — the events are the first writers of that ledger.

const barcodeGenerateRetries = 3

// NOTE (ADR-0005, api_level 110): drawInternalBarcode moved to
// internal/services (services.DrawInternalBarcode) with the catalog
// product creation it serves — the two callers here are repointed.
// GenerateProductBarcode assigns a fresh internal barcode to a product of
// the authenticated pharmacy that has NO barcode yet. A product that already
// carries one (manufacturer GTIN or internal) must use the product edit form
// — replacement is a conscious, audit-logged decision, never a side effect.
//
// POST /pharmacy/products/:id/barcode/generate
func (h *Handler) GenerateProductBarcode(c *gin.Context) {
        principal, ok := auth.PrincipalFromContext(c)
        if !ok || principal.PharmacyID == "" || principal.ID == "" {
                c.JSON(http.StatusForbidden, gin.H{"error": "pharmacy_mutation_account_required", "message": "حساب مدير أو موظف صيدلية مطلوب"})
                return
        }
        productID := idFromParam(c, "id")
        if !isUUID(productID) {
                c.JSON(http.StatusNotFound, gin.H{"error": "product_not_found", "message": "المنتج غير موجود في هذه الصيدلية"})
                return
        }

        tx, err := h.db.Begin(c.Request.Context())
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "barcode_generate_failed", "message": "تعذر بدء توليد الباركود"})
                return
        }
        defer func() { _ = tx.Rollback(c.Request.Context()) }()

        if _, err := tx.Exec(c.Request.Context(), `
                SELECT set_config('app.current_pharmacy_id', $1, true),
                       set_config('app.current_user_id', $2, true)
        `, principal.PharmacyID, principal.ID); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "barcode_generate_failed", "message": "تعذر إعداد نطاق الصيدلية"})
                return
        }

        // Lock the pharmacy product so ownership and serialization happen in
        // one statement: another generate call on the same product waits
        // instead of racing. Migration 48: the internal barcode is a
        // PHARMACY-LOCAL code — it lands in barcode_override and the shared
        // catalog row is never touched, so generating is allowed on every
        // product regardless of provenance.
        var pharmacyProductID, catalogProductID, productName, existingBarcode string
        err = tx.QueryRow(c.Request.Context(), `
                SELECT pp.id::text, gp.id::text,
                       COALESCE(pp.name_override, gp.name::text, ''),
                       COALESCE(pp.barcode_override, gp.barcode::text, '')
                FROM pharmacy_products pp
                LEFT JOIN global_products gp ON gp.id = pp.global_product_id
                WHERE pp.id = $1::uuid AND pp.pharmacy_id = $2
                FOR UPDATE OF pp
        `, productID, principal.PharmacyID).Scan(&pharmacyProductID, &catalogProductID, &productName, &existingBarcode)
        if errors.Is(err, pgx.ErrNoRows) {
                c.JSON(http.StatusNotFound, gin.H{"error": "product_not_found", "message": "المنتج غير موجود في هذه الصيدلية"})
                return
        }
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "barcode_generate_failed", "message": "تعذر قراءة المنتج"})
                return
        }
        if existingBarcode != "" {
                c.JSON(http.StatusConflict, gin.H{"error": "barcode_already_set", "message": "هذا المنتج لديه باركود بالفعل — الاستبدال يتم من نموذج تعديل المنتج"})
                return
        }

        var code string
        for attempt := 0; attempt < barcodeGenerateRetries; attempt++ {
                code, err = services.DrawInternalBarcode(c.Request.Context(), tx)
                if err != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "barcode_generate_failed", "message": "تعذر توليد الباركود"})
                        return
                }
                if _, err = tx.Exec(c.Request.Context(), `
                        UPDATE pharmacy_products
                        SET barcode_override = $2, barcode_type_override = $3, updated_at = NOW()
                        WHERE id = $1::uuid
                `, productID, code, barcode.TypeRCNEAN13); err != nil {
                        var pgErr *pgconn.PgError
                        // 23505 = unique_violation on the per-pharmacy override
                        // index: theoretically impossible between generated
                        // codes (sequence never repeats), reachable only if a
                        // manual entry consumed the same value earlier. Retry
                        // draws a fresh value.
                        if errors.As(err, &pgErr) && pgErr.Code == "23505" {
                                continue
                        }
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "barcode_generate_failed", "message": "تعذر حفظ الباركود"})
                        return
                }
                break
        }
        if err != nil {
                // All retries exhausted against unique violations.
                c.JSON(http.StatusConflict, gin.H{"error": "barcode_already_exists", "message": "تعذر توليد باركود فريد بعد عدة محاولات"})
                return
        }

        if err := writeAuditLog(c.Request.Context(), tx, principal,
                "barcode.generated", "create", "pharmacy_product", productID,
                map[string]any{"barcode": code, "barcode_type": barcode.TypeRCNEAN13, "product_name": productName},
                "توليد باركود داخلي للمنتج: " + productName); err != nil {
                auditFailure("barcode.generated", err)
        }

        if err := tx.Commit(c.Request.Context()); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "barcode_generate_failed", "message": "تعذر تأكيد توليد الباركود"})
                return
        }

        c.JSON(http.StatusCreated, gin.H{"data": gin.H{
                "pharmacy_product_id": productID,
                "global_product_id":   catalogProductID,
                "barcode":             code,
                "barcode_type":        barcode.TypeRCNEAN13,
                "internal":            true,
        }})
}

// bulkBarcodeRequest carries the explicit selection from the settings batch
// print panel. The list is bounded so one request cannot churn the whole
// catalog; the UI pages its selection.
type bulkBarcodeRequest struct {
        IDs []string `json:"ids"`
}

// BulkGenerateProductBarcodes fills in internal barcodes for the selected
// products that still miss one. Products that already carry a barcode are
// skipped untouched (their barcode, if wrong, is replaced through the edit
// form with an audit record). The whole run is one transaction: either every
// selected item ends in its final state or nothing changes.
//
// POST /pharmacy/barcodes/bulk-generate
func (h *Handler) BulkGenerateProductBarcodes(c *gin.Context) {
        principal, ok := auth.PrincipalFromContext(c)
        if !ok || principal.PharmacyID == "" || principal.ID == "" {
                c.JSON(http.StatusForbidden, gin.H{"error": "pharmacy_mutation_account_required", "message": "حساب مدير أو موظف صيدلية مطلوب"})
                return
        }
        var request bulkBarcodeRequest
        if err := c.ShouldBindJSON(&request); err != nil || len(request.IDs) == 0 {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_bulk_request", "message": "يجب تحديد منتج واحد على الأقل"})
                return
        }
        if len(request.IDs) > 500 {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_bulk_request", "message": "أقصى عدد للمنتجات في الطلب الواحد 500 منتج"})
                return
        }
        seen := make(map[string]bool, len(request.IDs))
        ids := make([]string, 0, len(request.IDs))
        for _, raw := range request.IDs {
                id := trimSpaceArabic(raw)
                if !isUUID(id) || seen[id] {
                        continue
                }
                seen[id] = true
                ids = append(ids, id)
        }
        if len(ids) == 0 {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_bulk_request", "message": "قائمة المنتجات غير صحيحة"})
                return
        }

        tx, err := h.db.Begin(c.Request.Context())
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "barcode_generate_failed", "message": "تعذر بدء توليد الباركود"})
                return
        }
        defer func() { _ = tx.Rollback(c.Request.Context()) }()

        if _, err := tx.Exec(c.Request.Context(), `
                SELECT set_config('app.current_pharmacy_id', $1, true),
                       set_config('app.current_user_id', $2, true)
        `, principal.PharmacyID, principal.ID); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "barcode_generate_failed", "message": "تعذر إعداد نطاق الصيدلية"})
                return
        }

        // Verify ownership and lock the pharmacy products in one shot: every
        // selected id MUST belong to this pharmacy. An unknown or foreign id
        // fails the whole request instead of silently half-applying the
        // selection. Migration 48: the effective barcode (local override
        // first, official fallback) decides the "already has one" skip —
        // generation writes barcode_override and never the catalog.
        rows, err := tx.Query(c.Request.Context(), `
                SELECT pp.id::text, gp.id::text,
                       COALESCE(pp.name_override, gp.name::text, ''),
                       COALESCE(pp.barcode_override, gp.barcode::text, '')
                FROM pharmacy_products pp
                LEFT JOIN global_products gp ON gp.id = pp.global_product_id
                WHERE pp.id = ANY($1::uuid[]) AND pp.pharmacy_id = $2
                ORDER BY 3
                FOR UPDATE OF pp
        `, ids, principal.PharmacyID)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "barcode_generate_failed", "message": "تعذر قراءة المنتجات"})
                return
        }
        type target struct {
                pharmacyProductID, catalogProductID, name, existing string
        }
        targets := make([]target, 0, len(ids))
        for rows.Next() {
                var t target
                if err := rows.Scan(&t.pharmacyProductID, &t.catalogProductID, &t.name, &t.existing); err != nil {
                        rows.Close()
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "barcode_generate_failed", "message": "تعذر قراءة المنتجات"})
                        return
                }
                targets = append(targets, t)
        }
        rows.Close()
        if err := rows.Err(); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "barcode_generate_failed", "message": "تعذر قراءة المنتجات"})
                return
        }
        if len(targets) == 0 {
                c.JSON(http.StatusNotFound, gin.H{"error": "product_not_found", "message": "لم يتم العثور على أي منتج من القائمة في هذه الصيدلية"})
                return
        }

        items := make([]gin.H, 0, len(targets))
        generatedCount := 0
        for _, t := range targets {
                if t.existing != "" {
                        items = append(items, gin.H{
                                "pharmacy_product_id": t.pharmacyProductID,
                                "status":              "skipped",
                                "reason":              "barcode_already_set",
                                "barcode":             t.existing,
                        })
                        continue
                }
                var code string
                var genErr error
                for attempt := 0; attempt < barcodeGenerateRetries; attempt++ {
                        code, genErr = services.DrawInternalBarcode(c.Request.Context(), tx)
                        if genErr != nil {
                                break
                        }
                        if _, genErr = tx.Exec(c.Request.Context(), `
                                UPDATE pharmacy_products
                                SET barcode_override = $2, barcode_type_override = $3, updated_at = NOW()
                                WHERE id = $1::uuid
                        `, t.pharmacyProductID, code, barcode.TypeRCNEAN13); genErr == nil {
                                break
                        }
                        var pgErr *pgconn.PgError
                        if errors.As(genErr, &pgErr) && pgErr.Code == "23505" {
                                continue
                        }
                        break
                }
                if genErr != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "barcode_generate_failed", "message": "تعذر توليد الباركود للمنتج: " + t.name})
                        return
                }
                generatedCount++
                items = append(items, gin.H{
                        "pharmacy_product_id": t.pharmacyProductID,
                        "status":              "generated",
                        "barcode":             code,
                        "barcode_type":        barcode.TypeRCNEAN13,
                })
        }

        if generatedCount > 0 {
                if err := writeAuditLog(c.Request.Context(), tx, principal,
                        "barcode.bulk_generated", "create", "pharmacy_product", "",
                        map[string]any{"requested": len(ids), "generated": generatedCount},
                        "توليد باركود داخلي جماعي لعدد " + itoa(generatedCount) + " منتجًا"); err != nil {
                        auditFailure("barcode.bulk_generated", err)
                }
        }

        if err := tx.Commit(c.Request.Context()); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "barcode_generate_failed", "message": "تعذر تأكيد توليد الباركود"})
                return
        }

        c.JSON(http.StatusOK, gin.H{"data": gin.H{
                "requested": len(ids),
                "generated": generatedCount,
                "items":     items,
        }})
}

func itoa(v int) string { return strconv.Itoa(v) }
