package handlers

import (
        "errors"
        "net/http"
        "strings"

        "github.com/gin-gonic/gin"
        "github.com/jackc/pgx/v5"
        "github.com/jackc/pgx/v5/pgconn"

        "github.com/pharmacy-os/backend/internal/auth"
        "github.com/pharmacy-os/backend/internal/services"
        "github.com/pharmacy-os/backend/internal/barcode"
        "github.com/pharmacy-os/backend/internal/money"
)

// updatePharmacyProductRequest carries the editable product fields. Money
// arrives as integer piastres exactly like creation, and the same validation
// rules apply, so a product can never hold a fractional or negative amount.
// Strength and dosage form are catalog fields (global_products), exactly like
// name — the edit form matches the create form 1:1 (نفس صفحة الإضافة).
type updatePharmacyProductRequest struct {
        Name                        string `json:"name"`
        GenericName                 string `json:"generic_name"`
        DosageForm                  string `json:"dosage_form"`
        Strength                    string `json:"strength"`
        Barcode                     string `json:"barcode"`
        PackagingType               string `json:"packaging_type"`
        UnitsPerBox                 int64  `json:"units_per_box"`
        CostPricePiastres           int64  `json:"cost_price_piastres"`
        SellingPricePiastres        int64  `json:"selling_price_piastres"`
        PartialSellingPricePiastres *int64 `json:"partial_selling_price_piastres"`
        MinStockLevel               int64  `json:"min_stock_level"`
        IsActive                    *bool  `json:"is_active"`
}

// GetPharmacyProduct returns one product of the authenticated pharmacy with
// the exact editable fields, so the edit form always opens with the real
// stored values instead of estimates derived from batch rows.
// GET /pharmacy/products/:id
func (h *Handler) GetPharmacyProduct(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }

        var (
                name, genericName, barcode, barcodeType, packagingType string
                strength, dosageForm                                   string
                unitsPerBox                                            int64
                costPrice, sellingPrice                                money.Piastres
                partialPrice                                           *int64
                minStockLevel                                          int64
                isActive                                               bool
        )
        // Effective identity (migration 48): the pharmacy's local override
        // wins; the official catalog row is the fallback. What the edit form
        // shows is exactly what POS, inventory and invoices show.
        err := h.db.QueryRow(c.Request.Context(), `
                SELECT COALESCE(pp.name_override, gp.name::text, ''),
                       COALESCE(pp.generic_name_override, gp.generic_name::text, ''),
                       COALESCE(pp.barcode_override, gp.barcode::text, ''),
                       COALESCE(pp.barcode_type_override, gp.barcode_type::text, ''),
                       COALESCE(pp.strength_override, gp.strength::text, ''),
                       COALESCE(pp.dosage_form_override, gp.dosage_form)::text,
                       COALESCE(pp.packaging_type::text, ''), COALESCE(pp.units_per_box::int8, 1),
                       pp.cost_price::int8, pp.selling_price::int8, pp.partial_selling_price::int8,
                       pp.min_stock_level::int8, pp.is_active
                FROM pharmacy_products pp
                JOIN global_products gp ON gp.id = pp.global_product_id
                WHERE pp.id = $1::uuid AND pp.pharmacy_id = $2
        `, idFromParam(c, "id"), pharmacyID).
                Scan(&name, &genericName, &barcode, &barcodeType, &strength, &dosageForm, &packagingType, &unitsPerBox,
                        &costPrice, &sellingPrice, &partialPrice, &minStockLevel, &isActive)
        if errors.Is(err, pgx.ErrNoRows) {
                c.JSON(http.StatusNotFound, gin.H{"error": "product_not_found", "message": "المنتج غير موجود في هذه الصيدلية"})
                return
        }
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "product_query_failed", "message": "تعذر تحميل بيانات المنتج"})
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": gin.H{
                "id": idFromParam(c, "id"), "name": name, "generic_name": genericName,
                "barcode": barcode, "barcode_type": barcodeType, "strength": strength, "dosage_form": dosageForm,
                "packaging_type": packagingType,
                "units_per_box": unitsPerBox, "cost_price_piastres": costPrice,
                "selling_price_piastres": sellingPrice,
                "partial_selling_price_piastres": partialPrice,
                "min_stock_level": minStockLevel, "is_active": isActive,
        }})
}

// UpdatePharmacyProduct edits the catalog data and pricing of an existing
// product of the authenticated pharmacy. Stock quantities are intentionally
// NOT touched here: every stock change must flow through stock_movements via
// the dedicated /inventory/:batch_id/adjust endpoint so the movement ledger
// stays the single source of truth for quantities.
// PUT /pharmacy/products/:id
func (h *Handler) UpdatePharmacyProduct(c *gin.Context) {
        principal, ok := auth.PrincipalFromContext(c)
        if !ok || principal.PharmacyID == "" || principal.ID == "" {
                c.JSON(http.StatusForbidden, gin.H{"error": "pharmacy_mutation_account_required", "message": "حساب مدير أو موظف صيدلية مطلوب"})
                return
        }

        var request updatePharmacyProductRequest
        if err := c.ShouldBindJSON(&request); err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_product", "message": "بيانات المنتج غير صحيحة"})
                return
        }
        request.Name = strings.TrimSpace(request.Name)
        request.GenericName = strings.TrimSpace(request.GenericName)
        request.DosageForm = strings.TrimSpace(request.DosageForm)
        request.Strength = strings.TrimSpace(request.Strength)
        request.Barcode = strings.TrimSpace(request.Barcode)
        request.PackagingType = strings.TrimSpace(request.PackagingType)
        if request.DosageForm == "" {
                request.DosageForm = "tablet"
        }

        boxCost := money.Piastres(request.CostPricePiastres)
        boxPrice := money.Piastres(request.SellingPricePiastres)
        validMoney := boxCost.Valid() && boxPrice.Valid() && request.MinStockLevel >= 0
        // Barcode optional on edit too (parity with creation and import).
        if request.Name == "" ||
                (request.PackagingType != services.PackagingWholeOnly && request.PackagingType != services.PackagingBoxStrip) ||
                !validMoney {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_product", "message": "يرجى إدخال اسم المنتج والأسعار والقيم غير السالبة"})
                return
        }
        if request.PackagingType == services.PackagingWholeOnly {
                request.UnitsPerBox = 1
                request.PartialSellingPricePiastres = nil
        } else {
                if request.UnitsPerBox < 2 {
                        c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_units_per_box", "message": "عدد الشرائط داخل العلبة يجب أن يكون 2 على الأقل"})
                        return
                }
                // Same rule as creation: the strip price is explicit and mandatory,
                // because deriving it by division could create fractional piastres.
                if request.PartialSellingPricePiastres == nil || !money.Piastres(*request.PartialSellingPricePiastres).Valid() {
                        c.JSON(http.StatusBadRequest, gin.H{"error": "partial_price_required", "message": "سعر بيع الشريط مطلوب ويجب أن يكون مبلغًا صحيحًا بالقروش"})
                        return
                }
        }

        productID := idFromParam(c, "id")

        tx, err := h.db.Begin(c.Request.Context())
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "product_update_failed", "message": "تعذر بدء حفظ التعديلات"})
                return
        }
        defer func() { _ = tx.Rollback(c.Request.Context()) }()

        if _, err := tx.Exec(c.Request.Context(), `
                SELECT set_config('app.current_pharmacy_id', $1, true),
                       set_config('app.current_user_id', $2, true)
        `, principal.PharmacyID, principal.ID); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "product_update_failed", "message": "تعذر إعداد نطاق الصيدلية"})
                return
        }

        // Lock the pharmacy product together with its catalog row and verify
        // ownership in the same statement: a product of another pharmacy is
        // indistinguishable from a missing one (404 for both). The current
        // EFFECTIVE identity is read in the same breath (local overrides
        // first, official catalog fallback — migration 48) so a replacement
        // (or removal) of a barcode can be audit-logged with its previous
        // value, and only real identity changes pay for a write.
        var (
                globalProductID, previousBarcode                               string
                storedName, storedGenericName, storedStrength, storedDosageForm string
        )
        var originType string
        var oldSelling, oldPartial *int64
        err = tx.QueryRow(c.Request.Context(), `
                SELECT pp.origin_type::text,
                       COALESCE(pp.global_product_id::text, ''),
                       COALESCE(pp.barcode_override, gp.barcode::text, ''),
                       COALESCE(pp.name_override, gp.name::text, ''),
                       COALESCE(pp.generic_name_override, gp.generic_name::text, ''),
                       COALESCE(pp.strength_override, gp.strength::text, ''),
                       COALESCE(pp.dosage_form_override, gp.dosage_form)::text,
                       pp.selling_price::int8, pp.partial_selling_price::int8
                FROM pharmacy_products pp
                LEFT JOIN global_products gp ON gp.id = pp.global_product_id
                WHERE pp.id = $1::uuid AND pp.pharmacy_id = $2
                FOR UPDATE OF pp
        `, productID, principal.PharmacyID).Scan(&originType, &globalProductID, &previousBarcode,
                &storedName, &storedGenericName, &storedStrength, &storedDosageForm,
                &oldSelling, &oldPartial)
        if errors.Is(err, pgx.ErrNoRows) {
                c.JSON(http.StatusNotFound, gin.H{"error": "product_not_found", "message": "المنتج غير موجود في هذه الصيدلية"})
                return
        }
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "product_update_failed", "message": "تعذر قراءة المنتج"})
                return
        }

        barcodeType := ""
        if request.Barcode != "" {
                barcodeType = barcode.DeriveType(request.Barcode)
        }
        var barcodeTypeArg any
        if barcodeType != "" {
                barcodeTypeArg = barcodeType
        }

        // Migration 48 — the pharmacy owns its product's presentation. A
        // catalog identity change is now a LOCAL write: it lands in the
        // pharmacy_products override columns and never touches the shared
        // global_products row, so other pharmacies and the platform
        // libraries keep the official identity untouched. There is no
        // governed-row refusal anymore: the write simply has no path to the
        // shared catalog. Clearing a field (empty string) stores NULL and
        // falls back to the official value.
        requestedIdentity := catalogIdentity{
                name:        request.Name,
                genericName: request.GenericName,
                barcode:     request.Barcode,
                strength:    request.Strength,
                dosageForm:  request.DosageForm,
        }
        storedIdentity := catalogIdentity{
                name:        storedName,
                genericName: storedGenericName,
                barcode:     previousBarcode,
                strength:    storedStrength,
                dosageForm:  storedDosageForm,
        }
        catalogChanged := catalogFieldsChanged(requestedIdentity, storedIdentity)
        if catalogChanged {
                // A local barcode must stay unique among this pharmacy's
                // EFFECTIVE barcodes (overrides and official codes alike) —
                // scanning has to resolve to exactly one product.
                if request.Barcode != "" {
                        var conflictID string
                        err := tx.QueryRow(c.Request.Context(), `
                                SELECT pp.id::text
                                FROM pharmacy_products pp
                                LEFT JOIN global_products gp ON gp.id = pp.global_product_id
                                WHERE pp.pharmacy_id = $1 AND pp.id <> $2::uuid
                                  AND COALESCE(pp.barcode_override, gp.barcode::text, '') = $3
                                LIMIT 1
                        `, principal.PharmacyID, productID, request.Barcode).Scan(&conflictID)
                        if err == nil {
                                c.JSON(http.StatusConflict, gin.H{"error": "barcode_already_exists", "message": "هذا الباركود مستخدم من قبل منتج آخر في صيدليتك"})
                                return
                        }
                        if !errors.Is(err, pgx.ErrNoRows) {
                                c.JSON(http.StatusInternalServerError, gin.H{"error": "product_update_failed", "message": "تعذر التحقق من الباركود"})
                                return
                        }
                }
                if _, err := tx.Exec(c.Request.Context(), `
                        UPDATE pharmacy_products
                        SET name_override = NULLIF($2, ''),
                            generic_name_override = NULLIF($3, ''),
                            barcode_override = NULLIF($4, ''),
                            barcode_type_override = $5,
                            strength_override = NULLIF($6, ''),
                            dosage_form_override = NULLIF($7, '')::dosage_form,
                            updated_at = NOW()
                        WHERE id = $1::uuid
                `, productID, request.Name, request.GenericName, request.Barcode,
                        barcodeTypeArg, request.Strength, request.DosageForm); err != nil {
                        var pgErr *pgconn.PgError
                        if errors.As(err, &pgErr) && pgErr.Code == "23505" {
                                c.JSON(http.StatusConflict, gin.H{"error": "barcode_already_exists", "message": "هذا الباركود مستخدم من قبل منتج آخر في صيدليتك"})
                                return
                        }
                        if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
                                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_dosage_form", "message": "الشكل الدوائي غير صحيح"})
                                return
                        }
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "product_update_failed", "message": "تعذر حفظ بيانات المنتج"})
                        return
                }
        }

        // Replacement or removal of a barcode is a conscious decision in the
        // edit form — record it with the old and new values (Final Decision
        // 14: barcode.replaced). Generation never happens here.
        if previousBarcode != "" && previousBarcode != request.Barcode {
                auditEntity, auditEntityID := "global_product", globalProductID
                if originType == "local" {
                        auditEntity, auditEntityID = "pharmacy_product", productID
                }
                if auditErr := writeAuditLog(c.Request.Context(), tx, principal,
                        "barcode.replaced", "update", auditEntity, auditEntityID,
                        map[string]any{
                                "old_barcode": previousBarcode, "new_barcode": request.Barcode,
                                "new_barcode_type": barcodeType, "product_name": request.Name,
                        },
                        "استبدال باركود المنتج: " + request.Name); auditErr != nil {
                        auditFailure("barcode.replaced", auditErr)
                }
        }

        if _, err := tx.Exec(c.Request.Context(), `
                UPDATE pharmacy_products
                SET cost_price = $2, selling_price = $3, partial_selling_price = $4,
                    min_stock_level = $5, packaging_type = $6, units_per_box = $7,
                    is_active = COALESCE($8, is_active)
                WHERE id = $1::uuid
        `, productID, boxCost, boxPrice, request.PartialSellingPricePiastres,
                request.MinStockLevel, request.PackagingType, request.UnitsPerBox,
                request.IsActive); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "product_update_failed", "message": "تعذر حفظ أسعار وإعدادات المنتج"})
                return
        }

        // D3 (api_level 122): one history row when the price actually moved —
        // old values were read under the same lock.
        if priceErr := recordPriceChangeTx(c.Request.Context(), tx, principal.PharmacyID,
                productID, PiastresOrZero(oldSelling), boxPrice,
                oldPartial, request.PartialSellingPricePiastres,
                priceSourceManualEdit, principal); priceErr != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "product_update_failed", "message": "تعذر تسجيل تاريخ السعر"})
                return
        }

        var isActive bool
        if err := tx.QueryRow(c.Request.Context(), `
                SELECT is_active FROM pharmacy_products WHERE id = $1::uuid
        `, productID).Scan(&isActive); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "product_update_failed", "message": "تعذر قراءة حالة المنتج بعد الحفظ"})
                return
        }

        if err := tx.Commit(c.Request.Context()); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "product_update_failed", "message": "تعذر تأكيد حفظ التعديلات"})
                return
        }

        c.JSON(http.StatusOK, gin.H{"data": gin.H{
                "id": productID, "name": request.Name, "generic_name": request.GenericName,
                "barcode": request.Barcode, "barcode_type": barcodeType, "strength": request.Strength, "dosage_form": request.DosageForm,
                "packaging_type": request.PackagingType,
                "units_per_box": request.UnitsPerBox,
                "cost_price_piastres": boxCost, "selling_price_piastres": boxPrice,
                "partial_selling_price_piastres": request.PartialSellingPricePiastres,
                "min_stock_level":                request.MinStockLevel,
                "is_active":                      isActive,
        }})
}
