package handlers

import (
        "context"
        "errors"
        "log"
        "net/http"
        "strings"
        "time"

        "github.com/gin-gonic/gin"
        "github.com/jackc/pgx/v5"
        "github.com/jackc/pgx/v5/pgconn"

        "github.com/pharmacy-os/backend/internal/auth"
        "github.com/pharmacy-os/backend/internal/barcode"
        "github.com/pharmacy-os/backend/internal/models"
        "github.com/pharmacy-os/backend/internal/money"
        "github.com/pharmacy-os/backend/internal/services"
)

const (
        minSaleQuantity = 1
        maxSaleQuantity = 1_000_000
)

// All money in this file is money.Piastres: an integer count of piastres
// (1 EGP = 100 piastres). No floating point value ever touches an amount.

type createPharmacyProductRequest struct {
        Name                        string `json:"name"`
        GenericName                 string `json:"generic_name"`
        DosageForm                  string `json:"dosage_form"`
        Strength                    string `json:"strength"`
        Barcode                     string `json:"barcode"`
        // OriginType (D1, api_level 122): central (default) mints a shared
        // catalog row exactly as before; local creates a fully tenant-owned
        // product - no global_products row, identity in the migration-48
        // override columns, invisible to every other pharmacy.
        OriginType                  string `json:"origin_type"`
        // GenerateBarcode asks the server to mint an internal RCN EAN-13 for
        // this product inside the creation transaction (Final Decision 6).
        // Only honored when the client did not supply a barcode.
        GenerateBarcode             bool   `json:"generate_barcode"`
        PackagingType               string `json:"packaging_type"`
        UnitsPerBox                 int64  `json:"units_per_box"`
        CostPricePiastres           int64  `json:"cost_price_piastres"`
        SellingPricePiastres        int64  `json:"selling_price_piastres"`
        PartialSellingPricePiastres *int64 `json:"partial_selling_price_piastres"`
        MinStockLevel               int64  `json:"min_stock_level"`
        InitialBoxes                int64  `json:"initial_boxes"`
        InitialStrips               int64  `json:"initial_strips"`
        BatchNumber                 string `json:"batch_number"`
        ExpiryDate                  string `json:"expiry_date"`
}

func (h *Handler) ListPharmacyProducts(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }

        search := strings.TrimSpace(c.Query("search"))
        // missing_barcode=true backs the settings batch-print panel: it lists
        // the products of this pharmacy that cannot be scanned yet.
        missingBarcode := c.Query("missing_barcode") == "true"
        rows, err := h.db.Query(c.Request.Context(), `
                SELECT pp.id::text,
                       COALESCE(pp.name_override::text, gp.name::text, ''),
                       COALESCE(pp.generic_name_override::text, gp.generic_name::text, ''),
                       COALESCE(pp.barcode_override::text, gp.barcode::text, ''),
                       COALESCE(pp.barcode_type_override::text, gp.barcode_type::text, ''),
                       COALESCE(pp.packaging_type::text, ''), COALESCE(pp.units_per_box::int8, 1),
                       pp.selling_price::int8, COALESCE(pp.partial_selling_price::int8, 0),
                       ROUND(COALESCE(SUM(ci.quantity), 0))::int8,
                       pp.origin_type::text
                FROM pharmacy_products pp
                LEFT JOIN global_products gp ON gp.id = pp.global_product_id
                LEFT JOIN current_inventory ci ON ci.pharmacy_product_id = pp.id
                WHERE pp.pharmacy_id = $1
                  AND pp.is_active = true
                  AND ($2 = '' OR COALESCE(pp.name_override, gp.name) ILIKE '%' || $2 || '%'
                       OR COALESCE(pp.barcode_override, gp.barcode) ILIKE '%' || $2 || '%')
                  AND ($3 = false OR COALESCE(pp.barcode_override, gp.barcode) IS NULL)
                GROUP BY pp.id, pp.name_override, gp.name, pp.generic_name_override, gp.generic_name,
                         pp.barcode_override, gp.barcode, pp.barcode_type_override, gp.barcode_type,
                         pp.packaging_type, pp.units_per_box, pp.selling_price, pp.partial_selling_price,
                         pp.origin_type
                ORDER BY COALESCE(pp.name_override, gp.name)
                LIMIT 500
        `, pharmacyID, search, missingBarcode)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "products_query_failed", "message": "تعذر تحميل المنتجات"})
                return
        }
        defer rows.Close()

        products := make([]gin.H, 0)
        for rows.Next() {
                var (
                        id, name, genericName, barcode, barcodeType, packagingType string
                        originType                                                 string
                        unitsPerBox                                                int64
                        sellingPrice, partialPrice, stock                          money.Piastres
                )
                // stock reuses the Piastres scan type only because both are
                // int64 columns here; it is a count, not an amount.
                if err := rows.Scan(&id, &name, &genericName, &barcode, &barcodeType, &packagingType, &unitsPerBox, &sellingPrice, &partialPrice, &stock, &originType); err != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "products_query_failed", "message": "تعذر قراءة المنتجات"})
                        return
                }
                products = append(products, gin.H{
                        "id": id, "name": name, "generic_name": genericName, "barcode": barcode,
                        "barcode_type": barcodeType, "origin_type": originType,
                        "packaging_type": packagingType, "units_per_box": unitsPerBox,
                        "selling_price_piastres": sellingPrice, "partial_selling_price_piastres": partialPrice,
                        "stock": stock,
                })
        }
        if err := rows.Err(); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "products_query_failed", "message": "تعذر قراءة المنتجات"})
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": products})
}

func (h *Handler) CreatePharmacyProduct(c *gin.Context) {
        principal, ok := auth.PrincipalFromContext(c)
        if !ok || principal.PharmacyID == "" || principal.ID == "" {
                c.JSON(http.StatusForbidden, gin.H{"error": "pharmacy_mutation_account_required", "message": "حساب مدير أو موظف صيدلية مطلوب"})
                return
        }
        // Plan limit (Task 90): products is a commercial ceiling.
        if !h.enforcePlanLimit(c, principal, models.LimitKeyProducts) {
                return
        }

        var request createPharmacyProductRequest
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
        request.BatchNumber = strings.TrimSpace(request.BatchNumber)
        request.OriginType = strings.TrimSpace(request.OriginType)
        if request.OriginType == "" {
                request.OriginType = "central"
        }
        if request.OriginType != "central" && request.OriginType != "local" {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_origin_type", "message": "نوع أصل المنتج يجب أن يكون central أو local"})
                return
        }
        isLocal := request.OriginType == "local"
        if request.DosageForm == "" {
                request.DosageForm = "tablet"
        }
        if request.PackagingType == "" {
                request.PackagingType = services.PackagingWholeOnly
        }

        boxCost := money.Piastres(request.CostPricePiastres)
        boxPrice := money.Piastres(request.SellingPricePiastres)
        validMoney := boxCost.Valid() && boxPrice.Valid() && request.MinStockLevel >= 0 &&
                request.InitialBoxes >= 0 && request.InitialStrips >= 0
        // The barcode is now optional (the import path always allowed an
        // empty one — this removes the contradiction). An absent barcode
        // either triggers server-side generation or stays empty.
        if request.Name == "" ||
                (request.PackagingType != services.PackagingWholeOnly && request.PackagingType != services.PackagingBoxStrip) ||
                !validMoney {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_product", "message": "يرجى إدخال اسم المنتج والأسعار والقيم غير السالبة"})
                return
        }
        if request.PackagingType == services.PackagingWholeOnly {
                request.UnitsPerBox = 1
                request.PartialSellingPricePiastres = nil
                if request.InitialStrips != 0 {
                        c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_initial_stock", "message": "المنتج الذي يباع كعبوة كاملة لا يقبل كمية شرائط"})
                        return
                }
        } else {
                if request.UnitsPerBox < 2 {
                        c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_units_per_box", "message": "عدد الشرائط داخل العلبة يجب أن يكون 2 على الأقل"})
                        return
                }
                // The strip price is explicit and mandatory: deriving it by
                // dividing the box price would create fractional piastres,
                // which this system does not allow.
                if request.PartialSellingPricePiastres == nil || !money.Piastres(*request.PartialSellingPricePiastres).Valid() {
                        c.JSON(http.StatusBadRequest, gin.H{"error": "partial_price_required", "message": "سعر بيع الشريط مطلوب ويجب أن يكون مبلغًا صحيحًا بالقروش"})
                        return
                }
        }

        baseQuantity := request.InitialBoxes*request.UnitsPerBox + request.InitialStrips
        baseUnit := "box"
        if request.PackagingType == services.PackagingBoxStrip {
                baseUnit = "strip"
        }
        if baseQuantity > 0 && request.BatchNumber == "" {
                request.BatchNumber = "OPENING-" + time.Now().UTC().Format("20060102150405")
        }
        // Cost per base unit: the only cost division in the system. It uses
        // the documented half-up rounding so a box cost that does not divide
        // evenly contributes at most half a piastre of cost rounding.
        unitCost := boxCost
        if request.PackagingType == services.PackagingBoxStrip {
                unitCost = boxCost.DivRoundHalfUp(request.UnitsPerBox)
        }
        employeeID, companyUserID := auth.ActorIDs(principal)

        tx, err := h.db.Begin(c.Request.Context())
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "product_create_failed", "message": "تعذر بدء حفظ المنتج"})
                return
        }
        defer func() { _ = tx.Rollback(c.Request.Context()) }()

        if _, err := tx.Exec(c.Request.Context(), `
                SELECT set_config('app.current_pharmacy_id', $1, true),
                       set_config('app.current_user_id', $2, true)
        `, principal.PharmacyID, principal.ID); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "product_create_failed", "message": "تعذر إعداد نطاق الصيدلية"})
                return
        }

        defaultUnit := baseUnit
        var globalProductID string

        // Barcode resolution order (Final Decisions 1/4/6):
        //   manual code    -> type derived mechanically, stored as-is
        //   generated code -> drawn from the atomic platform sequence inside
        //                     this transaction, type = RCN_EAN13
        //   none           -> NULL/NULL (product without barcode)
        insertBarcode := request.Barcode
        barcodeType := ""
        if insertBarcode != "" {
                barcodeType = barcode.DeriveType(insertBarcode)
        } else if request.GenerateBarcode {
                insertBarcode, err = services.DrawInternalBarcode(c.Request.Context(), tx)
                if err != nil {
                        log.Printf("[PRODUCTS] internal barcode draw failed: %v", err)
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "barcode_generate_failed", "message": "تعذر توليد الباركود الداخلي"})
                        return
                }
                barcodeType = barcode.TypeRCNEAN13
        }
        var barcodeTypeArg any
        if barcodeType != "" {
                barcodeTypeArg = barcodeType
        }

        if isLocal {
                // D1: a manual local code must not collide with an EFFECTIVE
                // barcode of another product in the same pharmacy — one scan,
                // one answer. Generated codes retry below off the same index.
                if insertBarcode != "" && insertBarcode == request.Barcode {
                        conflict, conflictErr := localBarcodeConflictTx(c.Request.Context(), tx,
                                principal.PharmacyID, "", insertBarcode)
                        if conflictErr != nil {
                                c.JSON(http.StatusInternalServerError, gin.H{"error": "product_create_failed", "message": "تعذر التحقق من تفرد الباركود"})
                                return
                        }
                        if conflict {
                                c.JSON(http.StatusConflict, gin.H{"error": "barcode_already_exists", "message": "هذا الباركود مستخدم من منتج آخر في نفس الصيدلية"})
                                return
                        }
                }
                var pharmacyProductID string
                err = tx.QueryRow(c.Request.Context(), `
                        INSERT INTO pharmacy_products (
                                pharmacy_id, origin_type, name_override, barcode_override, barcode_type_override,
                                cost_price, selling_price, partial_selling_price, min_stock_level,
                                packaging_type, units_per_box, is_active, is_discontinued
                        ) VALUES ($1, 'local', $2, NULLIF($3, ''), $4, $5, $6, $7, $8, $9, $10, true, false)
                        RETURNING id::text
                `, principal.PharmacyID, request.Name, insertBarcode, barcodeTypeArg,
                        boxCost, boxPrice, request.PartialSellingPricePiastres, request.MinStockLevel,
                        request.PackagingType, request.UnitsPerBox).Scan(&pharmacyProductID)
                if err != nil {
                        var pgErr *pgconn.PgError
                        if errors.As(err, &pgErr) && pgErr.Code == "23505" {
                                // idx_pharmacy_products_barcode_override (migration 48):
                                // the only unique index this INSERT can hit.
                                c.JSON(http.StatusConflict, gin.H{"error": "barcode_already_exists", "message": "هذا الباركود مستخدم من منتج محلي آخر في نفس الصيدلية"})
                                return
                        }
                        log.Printf("[PRODUCTS] local product insert failed: %v", err)
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "product_create_failed", "message": "تعذر حفظ المنتج المحلي"})
                        return
                }
                if insertBarcode != "" && insertBarcode != request.Barcode {
                        // Generated code — same audit event as the dedicated
                        // endpoint, entity = the local row.
                        if auditErr := writeAuditLog(c.Request.Context(), tx, principal,
                                "barcode.generated", "create", "pharmacy_product", pharmacyProductID,
                                map[string]any{"barcode": insertBarcode, "barcode_type": barcodeType,
                                        "product_name": request.Name, "origin_type": "local"},
                                "توليد باركود داخلي عند إنشاء منتج محلي: " + request.Name); auditErr != nil {
                                auditFailure("barcode.generated", auditErr)
                        }
                }
                // D3 baseline: the birth price is the first history row.
                if priceErr := recordPriceChangeTx(c.Request.Context(), tx, principal.PharmacyID,
                        pharmacyProductID, money.Piastres(0), boxPrice, nil, request.PartialSellingPricePiastres,
                        priceSourceProductCreate, principal); priceErr != nil {
                        log.Printf("[PRODUCTS] price history baseline failed: %v", priceErr)
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "product_create_failed", "message": "تعذر تسجيل تاريخ السعر"})
                        return
                }
                if baseQuantity > 0 {
                        if openErr := h.openLocalProductBatch(c, tx, principal, request, pharmacyProductID,
                                baseQuantity, baseUnit, unitCost, employeeID); openErr != "" {
                                if openErr == "branch_required" {
                                        c.JSON(http.StatusBadRequest, gin.H{"error": openErr, "message": "يجب تحديد فرع قبل إضافة المخزون"})
                                } else {
                                        c.JSON(http.StatusInternalServerError, gin.H{"error": openErr, "message": "تعذر إنشاء رصيد المخزون"})
                                }
                                return
                        }
                }
                if err := tx.Commit(c.Request.Context()); err != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "product_create_failed", "message": "تعذر تأكيد حفظ المنتج"})
                        return
                }
                c.JSON(http.StatusCreated, gin.H{
                        "data": gin.H{
                                "id": pharmacyProductID, "origin_type": "local",
                                "global_product_id": "",
                                "barcode": insertBarcode, "barcode_type": barcodeType,
                                "packaging_type": request.PackagingType, "units_per_box": request.UnitsPerBox,
                                "initial_base_quantity": baseQuantity,
                        },
                })
                return
        }

        for attempt := 0; ; attempt++ {
                err = tx.QueryRow(c.Request.Context(), `
                        INSERT INTO global_products (
                                name, generic_name, dosage_form, strength, barcode, barcode_type, default_unit,
                                product_category, requires_prescription, is_active, created_by
                        ) VALUES ($1, NULLIF($2, ''), $3::dosage_form, NULLIF($4, ''), NULLIF($5, ''), $8, $6::unit_type,
                                  'medication'::product_category, 'no'::prescription_required, true, NULLIF($7, '')::uuid)
                        RETURNING id::text
                `, request.Name, request.GenericName, request.DosageForm, request.Strength, insertBarcode, defaultUnit, employeeID, barcodeTypeArg).Scan(&globalProductID)
                if err == nil {
                        break
                }
                var pgErr *pgconn.PgError
                if errors.As(err, &pgErr) && pgErr.Code == "23505" {
                        // Unique index hit. Manual codes surface the conflict to
                        // the user; generated codes retry with a fresh sequence
                        // value (the index is the final guarantee, review §7.1).
                        if insertBarcode == "" || insertBarcode != request.Barcode {
                                if attempt < barcodeGenerateRetries-1 {
                                        insertBarcode, err = services.DrawInternalBarcode(c.Request.Context(), tx)
                                        if err != nil {
                                                log.Printf("[PRODUCTS] internal barcode redraw failed: %v", err)
                                                c.JSON(http.StatusInternalServerError, gin.H{"error": "barcode_generate_failed", "message": "تعذر توليد الباركود الداخلي"})
                                                return
                                        }
                                        barcodeType = barcode.TypeRCNEAN13
                                        barcodeTypeArg = barcodeType
                                        continue
                                }
                                c.JSON(http.StatusConflict, gin.H{"error": "barcode_already_exists", "message": "تعذر توليد باركود فريد بعد عدة محاولات"})
                                return
                        }
                        c.JSON(http.StatusConflict, gin.H{"error": "barcode_already_exists", "message": "هذا الباركود مستخدم من قبل"})
                        return
                }
                log.Printf("[PRODUCTS] insert failed: %v", err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "product_create_failed", "message": "تعذر حفظ بيانات المنتج"})
                return
        }

        if insertBarcode != "" && insertBarcode != request.Barcode {
                // The create form asked for server-side generation — same
                // audit event as the dedicated endpoint (Final Decision 14).
                if auditErr := writeAuditLog(c.Request.Context(), tx, principal,
                        "barcode.generated", "create", "global_product", globalProductID,
                        map[string]any{"barcode": insertBarcode, "barcode_type": barcodeType, "product_name": request.Name},
                        "توليد باركود داخلي عند إنشاء المنتج: " + request.Name); auditErr != nil {
                        auditFailure("barcode.generated", auditErr)
                }
        }

        var pharmacyProductID string
        err = tx.QueryRow(c.Request.Context(), `
                INSERT INTO pharmacy_products (
                        pharmacy_id, global_product_id, cost_price, selling_price,
                        partial_selling_price, min_stock_level, packaging_type, units_per_box,
                        is_active, is_discontinued
                ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true, false)
                RETURNING id::text
        `, principal.PharmacyID, globalProductID, boxCost, boxPrice,
                request.PartialSellingPricePiastres, request.MinStockLevel, request.PackagingType, request.UnitsPerBox).Scan(&pharmacyProductID)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "product_create_failed", "message": "تعذر ربط المنتج بالصيدلية"})
                return
        }

        // D3 baseline for central products too — the history starts at birth.
        if priceErr := recordPriceChangeTx(c.Request.Context(), tx, principal.PharmacyID,
                pharmacyProductID, money.Piastres(0), boxPrice, nil, request.PartialSellingPricePiastres,
                priceSourceProductCreate, principal); priceErr != nil {
                log.Printf("[PRODUCTS] price history baseline failed: %v", priceErr)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "product_create_failed", "message": "تعذر تسجيل تاريخ السعر"})
                return
        }

        if baseQuantity > 0 {
                branchID, branchErr := branchForPrincipal(c, tx, principal)
                if branchErr != nil {
                        c.JSON(http.StatusBadRequest, gin.H{"error": "branch_required", "message": "يجب تحديد فرع قبل إضافة المخزون"})
                        return
                }
                var batchID string
                err = tx.QueryRow(c.Request.Context(), `
                        INSERT INTO inventory_batches (
                                pharmacy_product_id, branch_id, batch_number, quantity, unit,
                                cost_per_unit, expiry_date, received_by, reference_type
                        ) VALUES ($1, $2, $3, $4, $5::unit_type, $6, NULLIF($7, '')::date, NULLIF($8, '')::uuid, 'opening_balance')
                        RETURNING id::text
                `, pharmacyProductID, branchID, request.BatchNumber, baseQuantity, baseUnit, unitCost, request.ExpiryDate, employeeID).Scan(&batchID)
                if err != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "product_create_failed", "message": "تعذر إنشاء رصيد المخزون"})
                        return
                }
                if _, err = tx.Exec(c.Request.Context(), `
                        INSERT INTO stock_movements (
                                batch_id, movement_type, quantity, unit, quantity_before,
                                quantity_after, unit_cost, total_cost, created_by, created_by_company_user_id, reason
                        ) VALUES ($1, 'purchase'::movement_type, $2, $3::unit_type, 0, $2, $4, $5, NULLIF($6, '')::uuid, NULLIF($7, '')::uuid, 'opening_balance')
                `, batchID, baseQuantity, baseUnit, unitCost, unitCost.MulQty(baseQuantity), employeeID, companyUserID); err != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "product_create_failed", "message": "تعذر تسجيل حركة المخزون"})
                        return
                }
        }

        if err := tx.Commit(c.Request.Context()); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "product_create_failed", "message": "تعذر تأكيد حفظ المنتج"})
                return
        }
        c.JSON(http.StatusCreated, gin.H{
                "data": gin.H{
                        "id": pharmacyProductID, "global_product_id": globalProductID,
                        "origin_type": "central",
                        "barcode": insertBarcode, "barcode_type": barcodeType,
                        "packaging_type": request.PackagingType, "units_per_box": request.UnitsPerBox,
                        "initial_base_quantity": baseQuantity,
                },
        })
}

// openLocalProductBatch creates the opening batch + movement for a LOCAL
// product inside the caller's transaction — the same ledger shape as the
// central path, factored out so both create branches share it.
func (h *Handler) openLocalProductBatch(c *gin.Context, tx pgx.Tx, principal *auth.Principal,
        request createPharmacyProductRequest, pharmacyProductID string,
        baseQuantity int64, baseUnit string, unitCost money.Piastres, employeeID string) string {
        branchID, branchErr := branchForPrincipal(c, tx, principal)
        if branchErr != nil {
                return "branch_required"
        }
        var batchID string
        err := tx.QueryRow(c.Request.Context(), `
                INSERT INTO inventory_batches (
                        pharmacy_product_id, branch_id, batch_number, quantity, unit,
                        cost_per_unit, expiry_date, received_by, reference_type
                ) VALUES ($1, $2, $3, $4, $5::unit_type, $6, NULLIF($7, '')::date, NULLIF($8, '')::uuid, 'opening_balance')
                RETURNING id::text
        `, pharmacyProductID, branchID, request.BatchNumber, baseQuantity, baseUnit, unitCost,
                request.ExpiryDate, employeeID).Scan(&batchID)
        if err != nil {
                return "product_create_failed"
        }
        _, companyUserID := auth.ActorIDs(principal)
        if _, err = tx.Exec(c.Request.Context(), `
                INSERT INTO stock_movements (
                        batch_id, movement_type, quantity, unit, quantity_before,
                        quantity_after, unit_cost, total_cost, created_by, created_by_company_user_id, reason
                ) VALUES ($1, 'purchase'::movement_type, $2, $3::unit_type, 0, $2, $4, $5, NULLIF($6, '')::uuid, NULLIF($7, '')::uuid, 'opening_balance')
        `, batchID, baseQuantity, baseUnit, unitCost, unitCost.MulQty(baseQuantity), employeeID, companyUserID); err != nil {
                return "product_create_failed"
        }
        return ""
}

func (h *Handler) LookupPOSProduct(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }
        barcode := strings.TrimSpace(c.Query("barcode"))
        if barcode == "" {
                c.JSON(http.StatusBadRequest, gin.H{"error": "barcode_required", "message": "الباركود مطلوب"})
                return
        }

        var (
                id, name, genericName, productBarcode, packagingType string
                strength                                             string
                unitsPerBox                                          int64
                sellingPrice, partialPrice, stock                    money.Piastres
        )
        // Effective identity (migration 48): the scan matches the pharmacy's
        // local barcode override when one exists, else the official code.
        err := h.db.QueryRow(c.Request.Context(), `
                SELECT pp.id::text, COALESCE(pp.name_override, gp.name::text, ''),
                       COALESCE(pp.generic_name_override, gp.generic_name::text, ''),
                       COALESCE(pp.strength_override, gp.strength::text, ''),
                       COALESCE(pp.barcode_override, gp.barcode::text, ''),
                       COALESCE(pp.packaging_type::text, ''), COALESCE(pp.units_per_box::int8, 1),
                       pp.selling_price::int8, COALESCE(pp.partial_selling_price::int8, 0),
                       ROUND(COALESCE(SUM(ci.quantity), 0))::int8
                FROM pharmacy_products pp
                LEFT JOIN global_products gp ON gp.id = pp.global_product_id
                LEFT JOIN current_inventory ci ON ci.pharmacy_product_id = pp.id
                WHERE pp.pharmacy_id = $1 AND COALESCE(pp.barcode_override, gp.barcode) = $2
                  AND pp.is_active = true AND (gp.id IS NULL OR gp.is_active = true)
                GROUP BY pp.id, pp.name_override, gp.name, pp.generic_name_override, gp.generic_name,
                         pp.strength_override, gp.strength, pp.barcode_override, gp.barcode, pp.packaging_type,
                         pp.units_per_box, pp.selling_price, pp.partial_selling_price
        `, pharmacyID, barcode).Scan(&id, &name, &genericName, &strength, &productBarcode, &packagingType, &unitsPerBox, &sellingPrice, &partialPrice, &stock)
        if errors.Is(err, pgx.ErrNoRows) {
                c.JSON(http.StatusNotFound, gin.H{"error": "product_not_found", "message": "لم يتم العثور على منتج بهذا الباركود"})
                return
        }
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "pos_lookup_failed", "message": "تعذر البحث عن المنتج"})
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": gin.H{
                "id": id, "name": name, "generic_name": genericName, "strength": strength, "barcode": productBarcode,
                "packaging_type": packagingType, "units_per_box": unitsPerBox,
                "selling_price_piastres": sellingPrice, "partial_selling_price_piastres": partialPrice,
                "stock": stock,
        }})
}

func (h *Handler) CreatePOSSale(c *gin.Context) {
        principal, ok := auth.PrincipalFromContext(c)
        if !ok || principal.PharmacyID == "" || principal.ID == "" {
                c.JSON(http.StatusForbidden, gin.H{"error": "pharmacy_mutation_account_required", "message": "حساب مدير أو موظف صيدلية مطلوب"})
                return
        }
        var request services.POSSaleRequest
        if err := c.ShouldBindJSON(&request); err != nil || len(request.Items) == 0 || len(request.Items) > 100 {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_sale", "message": "يجب إضافة منتج واحد على الأقل"})
                return
        }
        request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
        // AUD-015: the idempotency key is REQUIRED on POS sales — every
        // first-party client (pharmacy-app, pos-app, Flutter) mints one, and
        // the key is what turns a retried checkout into a replay of the
        // original invoice instead of a second charge. A keyless sale is now
        // a protocol violation, not a silent double-charge risk.
        if request.IdempotencyKey == "" {
                c.JSON(http.StatusBadRequest, gin.H{"error": "idempotency_key_required", "message": "مفتاح إعادة المحاولة مطلوب — حدّث الصفحة وأعد المحاولة"})
                return
        }
        if len(request.IdempotencyKey) < 8 || len(request.IdempotencyKey) > 128 {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_idempotency_key", "message": "مفتاح إعادة المحاولة غير صالح"})
                return
        }

        // نوع الدفع: نقدي افتراضياً، والآجل يتطلب عميلاً مسجلاً في نفس الصيدلية.
        paymentType := strings.TrimSpace(request.PaymentType)
        if paymentType == "" {
                paymentType = "cash"
        }
        if paymentType != "cash" && paymentType != "credit" {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_payment_type", "message": "نوع الدفع غير معروف"})
                return
        }
        request.PaymentType = paymentType
        customerID := strings.TrimSpace(request.CustomerID)
        if customerID != "" && !isUUID(customerID) {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_customer", "message": "بيانات العميل غير صحيحة"})
                return
        }
        request.CustomerID = customerID
        if paymentType == "credit" && customerID == "" {
                c.JSON(http.StatusBadRequest, gin.H{"error": "customer_required", "message": "البيع الآجل يتطلب اختيار عميل"})
                return
        }
        for _, item := range request.Items {
                if item.PharmacyProductID == "" || item.Quantity < minSaleQuantity || item.Quantity > maxSaleQuantity ||
                        (item.SaleUnit != "box" && item.SaleUnit != "strip") {
                        c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_sale_item", "message": "بيانات أحد أصناف الفاتورة غير صحيحة"})
                        return
                }
        }
        branchID, err := h.findBranch(c, principal)
        if err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "branch_required", "message": "يجب تحديد فرع قبل إتمام البيع"})
                return
        }

        // ADR-0005 (api_level 108): the checkout itself — lease context
        // (ADR-0002), AUD-009 customer ownership, AUD-015 idempotent replay,
        // price verification, discount allocation, the R-5 compressed
        // fulfillment and the transaction — lives in internal/services. This
        // handler is protocol only: it binds the request and maps each typed
        // service error onto the exact wire body this route always emitted.
        outcome, err := services.CreatePOSSale(c.Request.Context(), h.db, principal, branchID, request)
        if err != nil {
                h.respondPOSSaleFailure(c, err)
                return
        }
        if outcome.Replayed {
                c.JSON(http.StatusOK, gin.H{"data": gin.H{
                        "sale_id": outcome.SaleID, "total_amount_piastres": outcome.Total, "replayed": true,
                }})
                return
        }
        c.JSON(http.StatusCreated, gin.H{"data": gin.H{
                "sale_id": outcome.SaleID, "total_amount_piastres": outcome.Total, "discount_amount_piastres": outcome.Discount, "replayed": false,
        }})
}

// respondPOSSaleFailure maps the typed service errors onto the exact bodies
// the pre-extraction CreatePOSSale branches emitted — byte for byte
// (ADR-0005 rule 3: the wire contract is frozen during extraction). Internal
// failures log the stage and the real cause; the wire only ever sees the
// same generic sale_failed message that stage produced before.
func (h *Handler) respondPOSSaleFailure(c *gin.Context, err error) {
        var pch *services.POSSalePriceChanged
        var fail *services.POSFailure
        switch {
        case errors.Is(err, services.ErrPOSSaleInsufficientStock):
                c.JSON(http.StatusConflict, gin.H{"error": "insufficient_stock", "message": "الكمية المطلوبة غير متاحة في المخزون"})
        case errors.As(err, &pch):
                mismatches := make([]gin.H, 0, len(pch.Items))
                for _, m := range pch.Items {
                        mismatches = append(mismatches, gin.H{
                                "index":               m.Index,
                                "pharmacy_product_id": m.PharmacyProductID,
                                "sale_unit":           m.SaleUnit,
                                "quantity":            m.Quantity,
                                "unit_price_piastres": m.UnitPricePiastres,
                                "line_total_piastres": m.LineTotalPiastres,
                        })
                }
                c.JSON(http.StatusConflict, gin.H{
                        "error":   "price_changed",
                        "message": "أسعار بعض الأصناف تغيرت، راجع الفاتورة واعتمدها مرة أخرى",
                        "data":    gin.H{"items": mismatches},
                })
        case errors.Is(err, services.ErrPOSCustomerForeign):
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_customer", "message": "بيانات العميل غير صحيحة"})
        case errors.Is(err, services.ErrPOSUnknownProduct), errors.Is(err, services.ErrPOSSaleUnitForbidden):
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_sale_unit", "message": "وحدة البيع غير مسموحة لأحد الأصناف"})
        case errors.Is(err, services.ErrPOSProductPricing):
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_product_pricing", "message": "أسعار أحد الأصناف غير مضبوطة، يرجى تحديث المنتج"})
        case errors.Is(err, services.ErrPOSDiscountPercentRange):
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_discount", "message": "نسبة الخصم يجب أن تكون بين 0 و 100"})
        case errors.Is(err, services.ErrPOSDiscountNegative):
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_discount", "message": "قيمة الخصم لا يمكن أن تكون سالبة"})
        case errors.Is(err, services.ErrPOSDiscountKind):
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_discount", "message": "نوع الخصم غير معروف"})
        case errors.Is(err, services.ErrPOSDiscountExceedsTotal):
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_discount", "message": "الخصم أكبر من إجمالي الفاتورة"})
        case errors.Is(err, services.ErrPOSSaleNoItems):
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_sale", "message": "يجب إضافة منتج واحد على الأقل"})
        case errors.As(err, &fail):
                log.Printf("[POS] sale failed at stage %d: %v", fail.Stage, fail.Err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "sale_failed", "message": posStageMessage(fail.Stage)})
        default:
                log.Printf("[POS] sale failed: %v", err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "sale_failed", "message": "تعذر إنشاء الفاتورة"})
        }
}

// posStageMessage returns the exact failure message each pre-extraction
// branch emitted for its stage — the wire vocabulary preserved verbatim.
func posStageMessage(stage services.POSStage) string {
        switch stage {
        case services.POSStageBegin:
                return "تعذر بدء عملية البيع"
        case services.POSStageLease:
                return "تعذر إعداد نطاق الصيدلية"
        case services.POSStageReplayCheck:
                return "تعذر التحقق من الفاتورة"
        case services.POSStagePricingRead:
                return "تعذر قراءة أسعار الفاتورة"
        case services.POSStageInsert, services.POSStageInsertRaceRead:
                return "تعذر إنشاء الفاتورة"
        case services.POSStagePersistItems:
                return "تعذر تسجيل حركة البيع"
        case services.POSStageTotal:
                return "تعذر حفظ إجمالي الفاتورة"
        case services.POSStageCommit:
                return "تعذر تأكيد الفاتورة"
        }
        return "تعذر إنشاء الفاتورة"
}

func branchForPrincipal(c *gin.Context, tx pgx.Tx, principal *auth.Principal) (string, error) {
        if principal.BranchID != "" {
                return principal.BranchID, nil
        }
        return findBranchWithQuery(c, tx.QueryRow, principal.PharmacyID)
}

func (h *Handler) findBranch(c *gin.Context, principal *auth.Principal) (string, error) {
        if principal.BranchID != "" {
                return principal.BranchID, nil
        }
        return findBranchWithQuery(c, h.db.QueryRow, principal.PharmacyID)
}

func findBranchWithQuery(c *gin.Context, queryRow func(context.Context, string, ...interface{}) pgx.Row, pharmacyID string) (string, error) {
        var branchID string
        err := queryRow(c.Request.Context(), `
                SELECT id::text FROM branches
                WHERE pharmacy_id = $1 AND is_active = true
                ORDER BY is_main_branch DESC, created_at
                LIMIT 1
        `, pharmacyID).Scan(&branchID)
        return branchID, err
}
