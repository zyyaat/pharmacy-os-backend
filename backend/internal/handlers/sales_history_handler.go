package handlers

// Sales history + POS returns (credit notes) — protocol only (ADR-0005,
// api_level 109): the return transaction (lease context, idempotent replay,
// serialized concurrency via the invoice row lock, largest-remainder
// refunds, exact-batch restock, movement chain, invoice status machine) and
// the invoice detail read live in internal/services/sales_returns.go. This
// file binds requests, validates request shape, calls the service and maps
// each typed error onto the exact wire body this route always emitted.
// ListPOSSales still owns its listing SQL because the columns/scan/returns
// helpers are shared verbatim with the delta-sync endpoint (sync_rows.go) —
// that pair moves together in ledger item 6.
//
// All money in this file is money.Piastres: an integer count of piastres
// (1 EGP = 100 piastres). No floating point value ever touches an amount.

import (
        "errors"
        "log"
        "net/http"
        "strings"
        "time"

        "github.com/gin-gonic/gin"

        "github.com/pharmacy-os/backend/internal/auth"
        "github.com/pharmacy-os/backend/internal/services"
)

const (
        maxReturnQuantity  = 1_000_000
        maxReturnReasonLen = 500
        defaultSalesLimit  = 20
        maxSalesLimit      = 100
)

// isUUID reports whether s looks like a canonical UUID. The backend does not
// vendor a uuid package; this check keeps malformed ids away from SQL casts
// so they can be answered with a clean 4xx instead of a cast error.
func isUUID(s string) bool {
        if len(s) != 36 {
                return false
        }
        for i, r := range s {
                if i == 8 || i == 13 || i == 18 || i == 23 {
                        if r != '-' {
                                return false
                        }
                        continue
                }
                isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
                if !isHex {
                        return false
                }
        }
        return true
}

func parseLimitOffset(c *gin.Context) (int, int) {
        limit := defaultSalesLimit
        if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
                if v := parseIntInRange(raw, 1, maxSalesLimit); v > 0 {
                        limit = v
                }
        }
        offset := 0
        if raw := strings.TrimSpace(c.Query("offset")); raw != "" {
                if v := parseIntInRange(raw, 0, 1_000_000_000); v > 0 {
                        offset = v
                }
        }
        return limit, offset
}

func parseIntInRange(raw string, min, max int) int {
        var v int
        neg := false
        for i, r := range raw {
                if i == 0 && r == '-' {
                        neg = true
                        continue
                }
                if r < '0' || r > '9' {
                        return -1
                }
                v = v*10 + int(r-'0')
                if v > max {
                        return max + 1
                }
        }
        if neg || v < min {
                return -1
        }
        return v
}

// ListPOSSales returns the pharmacy's invoices, newest first, each with its
// returns (credit notes) attached so the history tab can render the reverse
// invoice directly under the invoice it reverses.
//
// Optional from/to (YYYY-MM-DD, inclusive) restrict the invoice creation date
// — used by the reports module and available to the history tab.
func (h *Handler) ListPOSSales(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }
        limit, offset := parseLimitOffset(c)
        search := strings.TrimSpace(c.Query("search"))
        fromRaw := strings.TrimSpace(c.Query("from"))
        toRaw := strings.TrimSpace(c.Query("to"))
        var fromDay, toDay string
        if fromRaw != "" {
                parsed, err := time.Parse("2006-01-02", fromRaw)
                if err != nil {
                        c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_from_date", "message": "تاريخ البداية غير صحيح (المتوقع YYYY-MM-DD)"})
                        return
                }
                fromDay = parsed.Format("2006-01-02")
        }
        if toRaw != "" {
                parsed, err := time.Parse("2006-01-02", toRaw)
                if err != nil {
                        c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_to_date", "message": "تاريخ النهاية غير صحيح (المتوقع YYYY-MM-DD)"})
                        return
                }
                toDay = parsed.Format("2006-01-02")
        }

        total := 0
        countErr := h.db.QueryRow(c.Request.Context(), `
                SELECT COUNT(*)::int FROM sales s
                WHERE s.pharmacy_id = $1
                  AND (
                      $2 = ''
                      OR s.invoice_number::text LIKE '%' || $2 || '%'
                      OR EXISTS (
                          SELECT 1
                          FROM sale_items si
                          JOIN pharmacy_products pp ON pp.id = si.pharmacy_product_id
                          LEFT JOIN global_products gp ON gp.id = pp.global_product_id
                          WHERE si.sale_id = s.id
                            AND COALESCE(si.product_name_snapshot, pp.name_override, gp.name) ILIKE '%' || $2 || '%'
                      )
                  )
                  AND ($3 = '' OR s.created_at::date >= $3::date)
                  AND ($4 = '' OR s.created_at::date <= $4::date)
        `, pharmacyID, search, fromDay, toDay).Scan(&total)
        if countErr != nil {
                log.Printf("[SALES] count failed: %v", countErr)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "sales_query_failed", "message": "تعذر قراءة سجل البيع"})
                return
        }

        // Columns + scan + returns attachment live in sync_rows.go — shared
        // with the delta-sync endpoint so the two can never drift apart.
        rows, err := h.db.Query(c.Request.Context(), `
                `+saleSummarySelectColumns+`
                WHERE s.pharmacy_id = $1
                  AND (
                      $2 = ''
                      OR s.invoice_number::text LIKE '%' || $2 || '%'
                      OR EXISTS (
                          SELECT 1
                          FROM sale_items si
                          JOIN pharmacy_products pp ON pp.id = si.pharmacy_product_id
                          LEFT JOIN global_products gp ON gp.id = pp.global_product_id
                          WHERE si.sale_id = s.id
                            AND COALESCE(si.product_name_snapshot, pp.name_override, gp.name) ILIKE '%' || $2 || '%'
                      )
                  )
                  AND ($5 = '' OR s.created_at::date >= $5::date)
                  AND ($6 = '' OR s.created_at::date <= $6::date)
                ORDER BY s.created_at DESC, s.invoice_number DESC
                LIMIT $3 OFFSET $4
        `, pharmacyID, search, limit, offset, fromDay, toDay)
        if err != nil {
                log.Printf("[SALES] list failed: %v", err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "sales_query_failed", "message": "تعذر قراءة سجل البيع"})
                return
        }

        sales := make([]*saleSummaryRow, 0)
        saleIDs := make([]string, 0)
        for rows.Next() {
                s, err := scanSaleSummaryRow(rows)
                if err != nil {
                        rows.Close()
                        log.Printf("[SALES] scan failed: %v", err)
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "sales_query_failed", "message": "تعذر قراءة سجل البيع"})
                        return
                }
                sales = append(sales, s)
                saleIDs = append(saleIDs, s.ID)
        }
        rows.Close()
        if err := rows.Err(); err != nil {
                log.Printf("[SALES] rows failed: %v", err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "sales_query_failed", "message": "تعذر قراءة سجل البيع"})
                return
        }

        if err := attachSaleReturns(c.Request.Context(), h.db, saleIDs, sales); err != nil {
                log.Printf("[SALES] returns list failed: %v", err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "sales_query_failed", "message": "تعذر قراءة سجل البيع"})
                return
        }

        payload := make([]gin.H, 0, len(sales))
        for _, s := range sales {
                payload = append(payload, s.Row)
        }
        c.JSON(http.StatusOK, gin.H{"data": gin.H{
                "sales":  payload,
                "total":  total,
                "limit":  limit,
                "offset": offset,
        }})
}

// GetPOSSale returns one invoice with row-level detail (one row per batch
// line) plus everything already returned, so the UI can offer exact
// returnable quantities per row. ADR-0005 (api_level 109): the three reads
// and their scans live in services.POSSaleDetailByID — this handler is
// protocol only and maps the typed rows onto the same response keys.
func (h *Handler) GetPOSSale(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }
        saleID := strings.TrimSpace(c.Param("sale_id"))
        if !isUUID(saleID) {
                c.JSON(http.StatusNotFound, gin.H{"error": "sale_not_found", "message": "الفاتورة غير موجودة"})
                return
        }

        detail, err := services.POSSaleDetailByID(c.Request.Context(), h.db, pharmacyID, saleID)
        if err != nil {
                var fail *services.POSSaleDetailFailure
                switch {
                case errors.Is(err, services.ErrPOSSaleNotFound):
                        c.JSON(http.StatusNotFound, gin.H{"error": "sale_not_found", "message": "الفاتورة غير موجودة"})
                case errors.As(err, &fail):
                        log.Printf("[SALES] detail failed at stage %d: %v", fail.Stage, fail.Err)
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "sale_query_failed", "message": "تعذر قراءة الفاتورة"})
                default:
                        log.Printf("[SALES] detail failed: %v", err)
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "sale_query_failed", "message": "تعذر قراءة الفاتورة"})
                }
                return
        }

        returns := make([]gin.H, 0, len(detail.Returns))
        for _, r := range detail.Returns {
                returns = append(returns, gin.H{
                        "id":                    r.ID,
                        "return_number":         r.ReturnNumber,
                        "total_amount_piastres": r.Total,
                        "reason":                r.Reason,
                        "created_at":            r.CreatedAt,
                        "quantity_base":         r.QuantityBase,
                })
        }

        items := make([]gin.H, 0, len(detail.Items))
        for _, it := range detail.Items {
                items = append(items, gin.H{
                        "sale_item_id":             it.SaleItemID,
                        "pharmacy_product_id":      it.ProductID,
                        "product_name":             it.ProductName,
                        "generic_name":             it.GenericName,
                        "strength":                 it.Strength,
                        "barcode":                  it.Barcode,
                        "packaging_type":           it.PackagingType,
                        "units_per_box":            it.UnitsPerBox,
                        "sale_unit":                it.SaleUnit,
                        "batch_number":             it.BatchNumber,
                        "quantity_base":            it.QuantityBase,
                        "unit_price_piastres":      it.UnitPrice,
                        "amount_piastres":          it.Amount,
                        "returned_quantity_base":   it.ReturnedQty,
                        "returnable_quantity_base": it.QuantityBase - it.ReturnedQty,
                        "returned_amount_piastres": it.ReturnedAmount,
                        // Quantity snapshot (Final Decision 12): NULL for rows
                        // written before migration 24 — clients fall back to the
                        // documented legacy interpretation.
                        "sale_quantity":          it.SaleQuantity,
                        "units_per_box_snapshot": it.UnitsPerBoxSnapshot,
                })
        }

        c.JSON(http.StatusOK, gin.H{"data": gin.H{
                "sale": gin.H{
                        "id":                       detail.Sale.ID,
                        "invoice_number":           detail.Sale.InvoiceNumber,
                        "status":                   detail.Sale.Status,
                        "total_amount_piastres":    detail.Sale.TotalAmount,
                        "discount_amount_piastres": detail.Sale.DiscountAmount,
                        "payment_type":             detail.Sale.PaymentType,
                        "customer_name":            detail.Sale.CustomerName,
                        "created_at":               detail.Sale.CreatedAt,
                        "returned_amount_piastres": detail.ReturnedTotal,
                        "returns":                  returns,
                },
                "items": items,
        }})
}

// CreatePOSSaleReturn records a return (credit note) against an invoice.
// Accepts full or partial returns: the caller lists sale_item rows with the
// base quantity (strips) to take back from each. The invoice itself is never
// modified; its status moves to partially_returned / returned instead.
// ADR-0005 (api_level 109): the whole transaction — lease context (ADR-0002),
// the idempotent replay, the serialized concurrency behind the invoice row
// lock, the largest-remainder refunds, the exact-batch restock and the
// movement chain — lives in services.CreatePOSSaleReturn. This handler is
// protocol only: it binds the request, validates its shape and maps each
// typed service error onto the exact wire body this route always emitted.
func (h *Handler) CreatePOSSaleReturn(c *gin.Context) {
        principal, ok := auth.PrincipalFromContext(c)
        if !ok || principal.PharmacyID == "" || principal.ID == "" {
                c.JSON(http.StatusForbidden, gin.H{"error": "pharmacy_mutation_account_required", "message": "حساب مدير أو موظف صيدلية مطلوب"})
                return
        }
        saleID := strings.TrimSpace(c.Param("sale_id"))
        if !isUUID(saleID) {
                c.JSON(http.StatusNotFound, gin.H{"error": "sale_not_found", "message": "الفاتورة غير موجودة"})
                return
        }

        var request services.POSReturnRequest
        if err := c.ShouldBindJSON(&request); err != nil || len(request.Items) == 0 || len(request.Items) > 100 {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_return", "message": "يجب تحديد صنف واحد على الأقل للاسترجاع"})
                return
        }
        request.Reason = strings.TrimSpace(request.Reason)
        if len(request.Reason) > maxReturnReasonLen {
                c.JSON(http.StatusBadRequest, gin.H{"error": "reason_too_long", "message": "سبب الاسترجاع طويل جداً"})
                return
        }
        request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
        if request.IdempotencyKey != "" &&
                (len(request.IdempotencyKey) < 8 || len(request.IdempotencyKey) > 128) {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_idempotency_key", "message": "مفتاح إعادة المحاولة غير صالح"})
                return
        }
        seen := make(map[string]bool, len(request.Items))
        for _, item := range request.Items {
                if !isUUID(item.SaleItemID) || item.Quantity < 1 || item.Quantity > maxReturnQuantity {
                        c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_return_item", "message": "بيانات أحد أصناف الاسترجاع غير صحيحة"})
                        return
                }
                if seen[item.SaleItemID] {
                        c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate_return_item", "message": "لا يمكن تكرار نفس الصنف في طلب الاسترجاع"})
                        return
                }
                seen[item.SaleItemID] = true
        }

        outcome, err := services.CreatePOSSaleReturn(c.Request.Context(), h.db, principal, saleID, request)
        if err != nil {
                h.respondPOSReturnFailure(c, err)
                return
        }
        if outcome.Replayed {
                c.JSON(http.StatusOK, gin.H{"data": gin.H{
                        "return_id":             outcome.ReturnID,
                        "return_number":         outcome.ReturnNumber,
                        "total_amount_piastres": outcome.Total,
                        "sale_status":           outcome.SaleStatus,
                        "replayed":              true,
                }})
                return
        }
        responseItems := make([]gin.H, 0, len(outcome.Items))
        for _, it := range outcome.Items {
                responseItems = append(responseItems, gin.H{
                        "sale_item_id":    it.SaleItemID,
                        "quantity":        it.Quantity,
                        "amount_piastres": it.Amount,
                })
        }
        c.JSON(http.StatusCreated, gin.H{"data": gin.H{
                "return_id":             outcome.ReturnID,
                "return_number":         outcome.ReturnNumber,
                "total_amount_piastres": outcome.Total,
                "sale_status":           outcome.SaleStatus,
                "replayed":              false,
                "items":                 responseItems,
        }})
}

// respondPOSReturnFailure maps the typed service errors onto the exact bodies
// the pre-extraction CreatePOSSaleReturn branches emitted — byte for byte
// (ADR-0005 rule 3: the wire contract is frozen during extraction). Internal
// failures log the stage and the real cause; the wire only ever sees the
// same generic return_failed message that stage produced before.
func (h *Handler) respondPOSReturnFailure(c *gin.Context, err error) {
        var exceeds *services.POSReturnExceedsSold
        var fail *services.POSReturnFailure
        switch {
        case errors.Is(err, services.ErrPOSSaleNotFound):
                c.JSON(http.StatusNotFound, gin.H{"error": "sale_not_found", "message": "الفاتورة غير موجودة"})
        case errors.Is(err, services.ErrPOSReturnNoItems):
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_return", "message": "يجب تحديد صنف واحد على الأقل للاسترجاع"})
        case errors.Is(err, services.ErrPOSReturnItemNotFound):
                c.JSON(http.StatusNotFound, gin.H{"error": "return_item_not_found", "message": "أحد الأصناف المطلوب استرجاعها غير موجود في هذه الفاتورة"})
        case errors.As(err, &exceeds):
                c.JSON(http.StatusConflict, gin.H{
                        "error":   "return_exceeds_sold",
                        "message": "الكمية المطلوب استرجاعها أكبر من الكمية المتاحة للاسترجاع",
                        "data": gin.H{
                                "sale_item_id":             exceeds.SaleItemID,
                                "returnable_quantity_base": exceeds.ReturnableQuantityBase,
                        },
                })
        case errors.As(err, &fail):
                log.Printf("[SALES] return failed at stage %d: %v", fail.Stage, fail.Err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "return_failed", "message": posReturnStageMessage(fail.Stage)})
        default:
                log.Printf("[SALES] return failed: %v", err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "return_failed", "message": "تعذر إنشاء فاتورة الاسترجاع"})
        }
}

// posReturnStageMessage returns the exact failure message each pre-extraction
// branch emitted for its stage — the wire vocabulary preserved verbatim.
func posReturnStageMessage(stage services.POSReturnStage) string {
        switch stage {
        case services.POSReturnStageBegin:
                return "تعذر بدء عملية الاسترجاع"
        case services.POSReturnStageLease:
                return "تعذر إعداد نطاق الصيدلية"
        case services.POSReturnStageReplayCheck:
                return "تعذر التحقق من عملية الاسترجاع"
        case services.POSReturnStageSaleLock:
                return "تعذر قراءة الفاتورة"
        case services.POSReturnStageItemsRead:
                return "تعذر قراءة أصناف الفاتورة"
        case services.POSReturnStageInsert:
                return "تعذر إنشاء فاتورة الاسترجاع"
        case services.POSReturnStageItemInsert:
                return "تعذر حفظ أصناف الاسترجاع"
        case services.POSReturnStageRestock:
                return "تعذر إرجاع الكمية للمخزون"
        case services.POSReturnStageMovement:
                return "تعذر تسجيل حركة الاسترجاع"
        case services.POSReturnStageStatusUpdate:
                return "تعذر تحديث حالة الفاتورة"
        case services.POSReturnStageCommit:
                return "تعذر تأكيد عملية الاسترجاع"
        }
        return "تعذر إنشاء فاتورة الاسترجاع"
}
