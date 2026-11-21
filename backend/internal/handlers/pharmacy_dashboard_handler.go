package handlers

import (
        "context"
        "errors"
        "math"
        "net/http"
        "strings"
        "time"

        "github.com/gin-gonic/gin"
        "github.com/jackc/pgx/v5/pgxpool"
        "github.com/pharmacy-os/backend/internal/auth"
        "github.com/pharmacy-os/backend/internal/repository"
)

// PharmacyDashboardStats is deliberately derived from the authenticated
// principal. A pharmacy id from the query string is never trusted.
func (h *Handler) GetPharmacyDashboardStats(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }

        const query = `
                SELECT
                        (SELECT COUNT(*)::int FROM pharmacy_products WHERE pharmacy_id = $1 AND is_active),
                        (SELECT COUNT(*)::int
                         FROM pharmacy_products pp
                         WHERE pp.pharmacy_id = $1 AND pp.is_active AND pp.min_stock_level > 0
                           AND GREATEST(FLOOR(COALESCE((SELECT SUM(ci.quantity) FROM current_inventory ci
                                               WHERE ci.pharmacy_product_id = pp.id), 0)
                                     / GREATEST(COALESCE(pp.units_per_box, 1), 1)), 0) < pp.min_stock_level),
                        (SELECT COUNT(*)::int FROM employees WHERE pharmacy_id = $1 AND status = 'active'),
                        (SELECT COUNT(*)::int
                         FROM attendance_records
                         WHERE pharmacy_id = $1 AND clock_in::date = CURRENT_DATE),
                        (SELECT ROUND(COALESCE(SUM(ABS(sm.quantity)), 0))::int8
                         FROM stock_movements sm
                         JOIN inventory_batches ib ON ib.id = sm.batch_id
                         JOIN pharmacy_products pp ON pp.id = ib.pharmacy_product_id
                         WHERE pp.pharmacy_id = $1
                           AND sm.movement_type = 'sale'
                           AND sm.created_at::date = CURRENT_DATE)
        `

        var totalProducts, lowStock, activeEmployees, activeToday int
        var salesUnits int64
        if err := h.db.QueryRow(c.Request.Context(), query, pharmacyID).
                Scan(&totalProducts, &lowStock, &activeEmployees, &activeToday, &salesUnits); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{
                        "error":   "pharmacy_dashboard_query_failed",
                        "message": "Could not load pharmacy dashboard statistics",
                })
                return
        }

        lowStockItems, err := h.lowStockItems(c, pharmacyID)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "inventory_query_failed", "message": "Could not load low stock items"})
                return
        }

        c.JSON(http.StatusOK, gin.H{
                "totalProducts":   totalProducts,
                "lowStockCount":   lowStock,
                "activeEmployees": activeEmployees,
                "activeToday":     activeToday,
                "salesUnitsToday": salesUnits,
                "lowStockItems":   lowStockItems,
        })
}

func (h *Handler) GetPharmacyDashboardActivity(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }

        const query = `
                SELECT
                        al.id::text,
                        al.action,
                        COALESCE(al.changes_summary, al.action),
                        COALESCE(NULLIF(al.actor_display_name, ''), NULLIF(al.actor_email, ''), 'System'),
                        al.created_at
                FROM audit_logs al
                WHERE al.pharmacy_id = $1
                ORDER BY al.created_at DESC
                LIMIT 10
        `
        rows, err := h.db.Query(c.Request.Context(), query, pharmacyID)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "activity_query_failed", "message": "Could not load pharmacy activity"})
                return
        }
        defer rows.Close()

        activities := make([]dashboardActivity, 0)
        for rows.Next() {
                var item dashboardActivity
                var timestamp time.Time
                if err := rows.Scan(&item.ID, &item.Type, &item.Description, &item.UserName, &timestamp); err != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "activity_query_failed", "message": "Could not read pharmacy activity"})
                        return
                }
                item.Timestamp = timestamp.UTC().Format(time.RFC3339)
                activities = append(activities, item)
        }
        if err := rows.Err(); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "activity_query_failed", "message": "Could not read pharmacy activity"})
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": activities})
}

// R-8 paging bounds for the inventory list: the silent LIMIT 500 becomes a
// real page (default stays 500 so existing clients see zero behavior change,
// the hard ceiling keeps a hostile limit from widening the query).
const (
        inventoryDefaultLimit = 500
        inventoryMaxLimit     = 1000
)

// inventorySearchPredicate mirrors the client-side filter the inventory page
// applied before R-8 (product_name, generic_name, brand_name, strength,
// barcode, batch_number) so server-side paging never shrinks what search can
// reach. $2 is the trimmed search term ('' disables the predicate entirely,
// which also keeps the no-search plan identical to the pre-R-8 one).
const inventorySearchPredicate = `
                  AND ($2 = '' OR product_name ILIKE '%' || $2 || '%'
                       OR COALESCE(generic_name::text, '') ILIKE '%' || $2 || '%'
                       OR COALESCE(brand_name::text, '') ILIKE '%' || $2 || '%'
                       OR COALESCE(strength::text, '') ILIKE '%' || $2 || '%'
                       OR COALESCE(barcode::text, '') ILIKE '%' || $2 || '%'
                       OR COALESCE(batch_number::text, '') ILIKE '%' || $2 || '%')`

// inventoryUnstockedSelectColumns is the synthetic-row SELECT for pharmacy
// products that have NO batch yet — the shape library imports create by red
// line («الاستيراد يضيف المنتجات بأسعارها الرسمية دون أي مخزون»). Before
// api_level 119 those products were invisible to the inventory page (the
// view is batch-based) and stranded: adjust needs an existing batch, the
// edit page does not manage stock, and re-creating the product would
// duplicate the global product. The column list mirrors
// inventorySelectColumns arity, order and types exactly (23 columns) so the
// UNION below cannot drift: NULL batch_id, NULL expiry/days, zero cost,
// product's selling price and packaging, '' batch_number/branch_name and
// the dedicated 'no_stock' status. الهوية الخمسة (اسم/عام/ماركة/باركود/شكل/تركيز)
// تتبع قاعدة الهوية الفعالة للـview نفسه (migration 48/53): override المحلي
// أولًا ثم الكتالوج — منتج محلي بلا صف كتالوج كان يعطي صف no_stock باسم
// فارغ يخفيه البحث (api_level 125). gp.id (global_product_id) is COALESCE'd
// to '': منتج محلي يجعل LEFT JOIN يعيد NULL، والوجهة النصية في
// scanInventoryListRow كانت تمسحها فتقتل القايمة كلها بتوقيع «cannot scan
// NULL into *string». وROUND(COALESCE(pp.min_stock_level,0)) لأن العمود
// يقبل NULL في المخطط. Served ONLY behind include=unstocked — the default
// response stays byte-identical because the mobile cache upserts inventory
// rows keyed by batch_id.
const inventoryUnstockedSelectColumns = `
       NULL::text, pp.id::text, COALESCE(gp.id::text, ''),
       COALESCE(pp.name_override::text, gp.name::text, ''),
       COALESCE(pp.generic_name_override::text, gp.generic_name::text, ''), COALESCE(gp.brand_name::text, ''), COALESCE(pp.barcode_override::text, gp.barcode::text, ''),
       COALESCE(pp.dosage_form_override::text, gp.dosage_form::text, ''), COALESCE(pp.strength_override::text, gp.strength::text, ''),
       '', CASE WHEN pp.packaging_type = 'BOX_STRIP' THEN 'strip' ELSE 'box' END,
       0::int8, 0::int8, 0::int8,
       NULL::date, NULL::int, pp.selling_price::int8, COALESCE(pp.partial_selling_price::int8, 0),
       COALESCE(pp.packaging_type::text, ''), COALESCE(pp.units_per_box::int8, 1), ROUND(COALESCE(pp.min_stock_level, 0))::int8,
       '', 'no_stock'`

// inventoryListColumnNames names the union subquery's 23 output columns
// explicitly: without it PostgreSQL would name the COALESCE/CASE columns
// after their functions («coalesce», «case»), and the outer SELECT — which
// reuses inventorySelectColumns verbatim to stay drift-proof — could not
// resolve product_name/generic_name/... against them.
const inventoryListColumnNames = `
                batch_id, pharmacy_product_id, global_product_id, product_name,
                generic_name, brand_name, barcode, dosage_form, strength,
                batch_number, unit, quantity, cost_per_unit, total_cost,
                expiry_date, days_until_expiry, selling_price, partial_selling_price,
                packaging_type, units_per_box, min_stock_level,
                branch_name, status`

// inventoryUnstockedUnion is the batch rows + one synthetic row per batchless
// product, with the search predicate applied over the named union columns.
const inventoryUnstockedUnion = `
                SELECT ` + inventorySelectColumns + ` FROM current_inventory WHERE pharmacy_id = $1
                UNION ALL
                SELECT ` + inventoryUnstockedSelectColumns + `
                FROM pharmacy_products pp
                LEFT JOIN global_products gp ON gp.id = pp.global_product_id
                WHERE pp.pharmacy_id = $1
                  AND NOT EXISTS (
                        SELECT 1 FROM inventory_batches ib
                        WHERE ib.pharmacy_product_id = pp.id
                  )`

// inventoryPage runs the ONE shared inventory list query: true total over
// the search window + one ordered page. Both the HTTP handler and the R-8
// DB regression gate go through this function so the response contract and
// its gate can never drift apart.
func inventoryPage(ctx context.Context, db *pgxpool.Pool, pharmacyID, search string, limit, offset int) ([]map[string]interface{}, int64, error) {
        return inventoryPageExtended(ctx, db, pharmacyID, search, limit, offset, false)
}

// inventoryPageExtended is the single implementation behind inventoryPage.
// includeUnstocked=false keeps the pre-119 SQL byte-for-byte (the mobile
// contract); true unions one synthetic row per batchless product and pages
// the combined set with the same search predicate and deterministic order.
func inventoryPageExtended(ctx context.Context, db *pgxpool.Pool, pharmacyID, search string, limit, offset int, includeUnstocked bool) ([]map[string]interface{}, int64, error) {
        // total يُحسم من الصفحة الأولى فقط (offset=0). العدّاد الحقيقي يجري فوق
        // الاتحاد الكامل (تشغيلات + صفوف no_stock) بمصفاة البحث — وإعادة هذا
        // العد الكامل مع كل دفعة تمرير كانت عملاً مكررًا بلا مستفيد: الواجهة
        // تقرأ total من الدفعة الأولى وحدها وتبني الباقي عليها (getNextPageParam
        // من allPages[0]). صفحات الإزاحة الأعلى تعيد -1 كإشارة عقد موثقة
        // «العدّاد غير محسوب هنا» — وحدها تختصر كل دفعة تمرير إلى استعلام واحد.
        var total int64
        if offset > 0 {
                total = -1
        } else {
                var totalQuery string
                if includeUnstocked {
                        totalQuery = `
                SELECT count(*) FROM (` + inventoryUnstockedUnion + `
                ) AS u(` + inventoryListColumnNames + `)
                WHERE 1=1` + inventorySearchPredicate
                } else {
                        totalQuery = `
                SELECT count(*) FROM current_inventory
                WHERE pharmacy_id = $1` + inventorySearchPredicate
                }
                if err := db.QueryRow(ctx, totalQuery,
                        pharmacyID, search).Scan(&total); err != nil {
                        return nil, 0, err
                }
        }

        var pageQuery string
        if includeUnstocked {
                // Same union, paged. The tiebreakers (batch_id NULLS LAST,
                // pharmacy_product_id) make the walk deterministic when a
                // name group mixes stocked and unstocked rows.
                pageQuery = `
                SELECT ` + inventorySelectColumns + ` FROM (` + inventoryUnstockedUnion + `
                ) AS u(` + inventoryListColumnNames + `)
                WHERE 1=1` + inventorySearchPredicate + `
                ORDER BY product_name, expiry_date NULLS LAST, batch_id NULLS LAST, pharmacy_product_id
                LIMIT $3 OFFSET $4
        `
        } else {
                // Columns + scan live in sync_rows.go — shared with the delta-sync
                // endpoint so the two can never drift apart. The tiebreakers are
                // not cosmetic: product_name repeats freely (same product, many
                // batches, identical expiry) and Postgres gives tied rows NO
                // stability guarantee across separate LIMIT/OFFSET queries, so
                // pages could repeat or skip rows between walks. batch_id is
                // non-NULL in the view path (one row per batch) and unique per
                // row; reordering ties is a no-op for the mobile cache, which
                // upserts inventory rows keyed by batch_id.
                pageQuery = `
                SELECT ` + inventorySelectColumns + `
                FROM current_inventory
                WHERE pharmacy_id = $1` + inventorySearchPredicate + `
                ORDER BY product_name, expiry_date NULLS LAST, batch_id, pharmacy_product_id
                LIMIT $3 OFFSET $4
        `
        }
        rows, err := db.Query(ctx, pageQuery, pharmacyID, search, limit, offset)
        if err != nil {
                return nil, 0, err
        }
        defer rows.Close()

        items := make([]map[string]interface{}, 0)
        scan := scanInventoryRow
        if includeUnstocked {
                scan = scanInventoryListRow
        }
        for rows.Next() {
                item, err := scan(rows)
                if err != nil {
                        return nil, 0, err
                }
                items = append(items, item)
        }
        return items, total, rows.Err()
}

func (h *Handler) GetPharmacyInventory(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }

        // R-8: real pages instead of a silent cap. Without params the query is
        // byte-for-byte the pre-R-8 one (default limit 500, offset 0, no search)
        // plus the additive total/limit/offset keys in the response.
        limit, offset := parseListPaging(c, inventoryDefaultLimit, inventoryMaxLimit)
        search := strings.TrimSpace(c.Query("search"))
        // api_level 119: include=unstocked opts the caller into the synthetic
        // rows for products with no batch yet (library imports). Opt-IN only —
        // the default response must stay byte-identical because the mobile
        // cache upserts inventory rows keyed by batch_id.
        includeUnstocked := c.Query("include") == "unstocked"

        items, total, err := inventoryPageExtended(c.Request.Context(), h.db, pharmacyID, search, limit, offset, includeUnstocked)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "inventory_query_failed", "message": "Could not load inventory"})
                return
        }
        // Additive keys only: pre-R-8 clients read `data` and ignore the rest.
        // total = -1 عند offset>0 (العدّاد يُحسم من الصفحة الأولى فقط) — ورد
        // الصفحة يضغط gzip تلقائيًا للعميل الذي يقبله (respondJSON).
        respondJSON(c, http.StatusOK, gin.H{"data": items, "total": total, "limit": limit, "offset": offset})
}

type adjustInventoryRequest struct {
        Delta  float64 `json:"delta"`
        Reason string  `json:"reason"`
}

// AdjustPharmacyInventory is the first real inventory mutation endpoint.
// It intentionally supports adjustments only; receiving, sales, and transfers
// will get separate contracts so their business rules cannot be conflated.
func (h *Handler) AdjustPharmacyInventory(c *gin.Context) {
        principal, ok := auth.PrincipalFromContext(c)
        if !ok || principal.PharmacyID == "" ||
                (principal.Type != auth.EmployeePrincipal &&
                        (principal.Type != auth.CompanyUserPrincipal ||
                                (principal.Role != "company_admin" && principal.Role != "company_manager"))) {
                c.JSON(http.StatusForbidden, gin.H{
                        "error":   "pharmacy_mutation_account_required",
                        "message": "A pharmacy manager or employee account is required for inventory adjustments",
                })
                return
        }

        var request adjustInventoryRequest
        if err := c.ShouldBindJSON(&request); err != nil ||
                math.IsNaN(request.Delta) || math.IsInf(request.Delta, 0) ||
                request.Delta == 0 || math.Abs(request.Delta) > 1000000000 {
                c.JSON(http.StatusBadRequest, gin.H{
                        "error":   "invalid_inventory_adjustment",
                        "message": "delta must be a finite non-zero number within the allowed range",
                })
                return
        }

        idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
        if len(idempotencyKey) < 8 || len(idempotencyKey) > 128 {
                c.JSON(http.StatusBadRequest, gin.H{
                        "error":   "idempotency_key_required",
                        "message": "Idempotency-Key must be between 8 and 128 characters",
                })
                return
        }

        hasPermission := principal.Type == auth.CompanyUserPrincipal
        err := error(nil)
        if principal.Type == auth.EmployeePrincipal {
                err = h.db.QueryRow(c.Request.Context(), `
                SELECT EXISTS (
                        SELECT 1
                        FROM employee_permissions ep
                        JOIN permissions p ON p.id = ep.permission_id
                        WHERE ep.employee_id = $1
                          AND p.key = 'inventory.adjust'
                          AND ep.is_active = true
                          AND ep.revoked_at IS NULL
                )
                `, principal.ID).Scan(&hasPermission)
        }
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{
                        "error":   "permission_check_failed",
                        "message": "Could not verify inventory permission",
                })
                return
        }
        if !hasPermission {
                c.JSON(http.StatusForbidden, gin.H{
                        "error":               "permission_denied",
                        "message":             "Inventory adjustment permission is required",
                        "required_permission": "inventory.adjust",
                })
                return
        }

        result, err := repository.NewStockMovementRepository(h.db).AdjustBatchStock(
                c.Request.Context(),
                repository.StockAdjustmentInput{
                        BatchID: idFromParam(c, "batch_id"), PharmacyID: principal.PharmacyID,
                        BranchID: principal.BranchID, EmployeeID: actorEmployeeID(principal),
                        CompanyUserID: actorCompanyUserID(principal),
                        Delta:         request.Delta, IdempotencyKey: idempotencyKey,
                        Reason:    strings.TrimSpace(request.Reason),
                        IPAddress: c.ClientIP(), UserAgent: c.GetHeader("User-Agent"),
                },
        )
        if err != nil {
                switch {
                case errors.Is(err, repository.ErrInventoryBatchNotFound):
                        c.JSON(http.StatusNotFound, gin.H{"error": "inventory_batch_not_found"})
                case errors.Is(err, repository.ErrInventoryHistoryRequired):
                        c.JSON(http.StatusConflict, gin.H{
                                "error":   "inventory_history_required",
                                "message": "The batch must have an established stock movement history before it can be adjusted",
                        })
                case errors.Is(err, repository.ErrInsufficientStock):
                        c.JSON(http.StatusConflict, gin.H{"error": "insufficient_stock"})
                case errors.Is(err, repository.ErrIdempotencyConflict):
                        c.JSON(http.StatusConflict, gin.H{
                                "error":   "idempotency_key_conflict",
                                "message": "This Idempotency-Key was already used for a different adjustment",
                        })
                default:
                        c.JSON(http.StatusInternalServerError, gin.H{
                                "error":   "inventory_adjustment_failed",
                                "message": "Could not apply inventory adjustment",
                        })
                }
                return
        }

        status := http.StatusOK
        c.JSON(status, gin.H{
                "data": gin.H{
                        "movement_id": result.MovementID, "batch_id": result.BatchID,
                        "unit": result.Unit, "previous_quantity": result.PreviousQuantity,
                        "new_quantity": result.NewQuantity, "created_at": result.CreatedAt.UTC(),
                        "replayed": result.Replayed,
                },
        })
}

// openingStockRequest is the body of POST /pharmacy/products/:id/opening-stock.
// Quantities follow the create-product shapes: boxes in full packs, strips in
// base units (BOX_STRIP only), cost per box in exact integer piastres.
type openingStockRequest struct {
        InitialBoxes      int64  `json:"initial_boxes"`
        InitialStrips     int64  `json:"initial_strips"`
        BatchNumber       string `json:"batch_number"`
        ExpiryDate        string `json:"expiry_date"`
        CostPricePiastres int64  `json:"cost_price_piastres"`
        Reason            string `json:"reason"`
}

// OpenProductStock (api_level 119) creates the FIRST batch of a pharmacy
// product that has none — the stock-receiving half that library imports were
// missing. The import red line stays intact (imports still write zero
// stock); this endpoint is how the pharmacy then receives stock, through
// the same ledger contract as every other writer: one inventory_batches row
// + one stock_movements row ('purchase', reference 'opening_balance') in a
// single transaction, idempotent by header key, batch quantity kept equal to
// the movement sum so AdjustBatchStock's history gate accepts the batch.
// A product that already owns any batch answers 409 — adjust is the path.
func (h *Handler) OpenProductStock(c *gin.Context) {
        principal, ok := auth.PrincipalFromContext(c)
        if !ok || principal.PharmacyID == "" ||
                (principal.Type != auth.EmployeePrincipal &&
                        (principal.Type != auth.CompanyUserPrincipal ||
                                (principal.Role != "company_admin" && principal.Role != "company_manager"))) {
                c.JSON(http.StatusForbidden, gin.H{
                        "error":   "pharmacy_mutation_account_required",
                        "message": "A pharmacy manager or employee account is required for stock receiving",
                })
                return
        }

        var request openingStockRequest
        if err := c.ShouldBindJSON(&request); err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_opening_stock", "message": "Could not read the opening stock request"})
                return
        }
        batchNumber := strings.TrimSpace(request.BatchNumber)
        reason := strings.TrimSpace(request.Reason)
        switch {
        case request.InitialBoxes < 0 || request.InitialStrips < 0 ||
                (request.InitialBoxes == 0 && request.InitialStrips == 0):
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_opening_stock", "message": "الكمية الافتتاحية يجب أن تكون أكبر من صفر"})
                return
        case request.CostPricePiastres < 0:
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_opening_stock", "message": "التكلفة يجب أن تكون مبلغًا غير سالب بالقروش"})
                return
        case len(batchNumber) > 100:
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_opening_stock", "message": "رقم التشغيلة يجب أن يكون 100 حرف على الأكثر"})
                return
        case request.ExpiryDate != "":
                if _, err := time.Parse("2006-01-02", request.ExpiryDate); err != nil {
                        c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_opening_stock", "message": "تاريخ الانتهاء يجب أن يكون بصيغة YYYY-MM-DD"})
                        return
                }
        }
        idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
        if len(idempotencyKey) < 8 || len(idempotencyKey) > 128 {
                c.JSON(http.StatusBadRequest, gin.H{
                        "error":   "idempotency_key_required",
                        "message": "Idempotency-Key must be between 8 and 128 characters",
                })
                return
        }
        branchID, err := h.findBranch(c, principal)
        if err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "branch_required", "message": "يجب تحديد فرع قبل إضافة المخزون"})
                return
        }

        result, err := repository.NewStockMovementRepository(h.db).OpenProductStock(
                c.Request.Context(),
                repository.OpeningStockInput{
                        ProductID:         idFromParam(c, "id"),
                        PharmacyID:        principal.PharmacyID,
                        BranchID:          branchID,
                        EmployeeID:        actorEmployeeID(principal),
                        CompanyUserID:     actorCompanyUserID(principal),
                        InitialBoxes:      request.InitialBoxes,
                        InitialStrips:     request.InitialStrips,
                        CostPricePiastres: request.CostPricePiastres,
                        BatchNumber:       batchNumber,
                        ExpiryDate:        request.ExpiryDate,
                        Reason:            reason,
                        IdempotencyKey:    idempotencyKey,
                        IPAddress:         c.ClientIP(),
                        UserAgent:         c.GetHeader("User-Agent"),
                },
        )
        if err != nil {
                switch {
                case errors.Is(err, repository.ErrOpeningStockProductNotFound):
                        c.JSON(http.StatusNotFound, gin.H{"error": "opening_stock_product_not_found"})
                case errors.Is(err, repository.ErrOpeningStockExists):
                        c.JSON(http.StatusConflict, gin.H{
                                "error":   "opening_stock_exists",
                                "message": "هذا المنتج له مخزون بالفعل — استخدم تعديل المخزون",
                        })
                case errors.Is(err, repository.ErrInvalidOpeningStock):
                        c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_opening_stock", "message": "قيم المخزون الافتتاحي غير متوافقة مع نوع تعبئة المنتج"})
                case errors.Is(err, repository.ErrIdempotencyConflict):
                        c.JSON(http.StatusConflict, gin.H{
                                "error":   "idempotency_key_conflict",
                                "message": "This Idempotency-Key was already used for a different opening",
                        })
                default:
                        c.JSON(http.StatusInternalServerError, gin.H{
                                "error":   "opening_stock_failed",
                                "message": "Could not record the opening stock",
                        })
                }
                return
        }
        c.JSON(http.StatusCreated, gin.H{
                "data": gin.H{
                        "movement_id": result.MovementID, "batch_id": result.BatchID,
                        "unit": result.Unit, "quantity": result.Quantity,
                        "created_at": result.CreatedAt.UTC(), "replayed": result.Replayed,
                },
        })
}

func actorEmployeeID(principal *auth.Principal) string {
        if principal != nil && principal.Type == auth.EmployeePrincipal {
                return principal.ID
        }
        return ""
}

func actorCompanyUserID(principal *auth.Principal) string {
        if principal != nil && principal.Type == auth.CompanyUserPrincipal {
                return principal.ID
        }
        return ""
}

func idFromParam(c *gin.Context, name string) string {
        return strings.TrimSpace(c.Param(name))
}

// lowStockItems feeds the dashboard "حالة المخزون" card. Same business rule as
// the header bell: حد الطلب is counted in FULL BOXES only (leftover strips of
// an opened box never count) — a product lists when
// floor(total base quantity / units_per_box) is below its min_stock_level.
func (h *Handler) lowStockItems(c *gin.Context, pharmacyID string) ([]map[string]interface{}, error) {
        const query = `
                SELECT COALESCE(ci.product_name::text, ''), COALESCE(ci.generic_name::text, ''),
                       GREATEST(FLOOR(COALESCE(SUM(ci.quantity), 0)
                             / GREATEST(COALESCE(pp.units_per_box, 1), 1)), 0)::int8,
                       GREATEST(COALESCE(SUM(ci.quantity), 0), 0)::int8 % GREATEST(COALESCE(pp.units_per_box, 1), 1)::int8,
                       pp.min_stock_level::int8,
                       CASE WHEN COALESCE(SUM(ci.quantity), 0) <= 0 THEN 'out_of_stock' ELSE 'low_stock' END
                FROM current_inventory ci
                JOIN pharmacy_products pp ON pp.id = ci.pharmacy_product_id
                WHERE ci.pharmacy_id = $1 AND pp.is_active = true AND pp.min_stock_level > 0
                GROUP BY pp.id, ci.product_name, ci.generic_name, pp.units_per_box, pp.min_stock_level
                HAVING GREATEST(FLOOR(COALESCE(SUM(ci.quantity), 0)
                             / GREATEST(COALESCE(pp.units_per_box, 1), 1)), 0) < pp.min_stock_level
                ORDER BY 3 ASC, ci.product_name
                LIMIT 10
        `
        rows, err := h.db.Query(c.Request.Context(), query, pharmacyID)
        if err != nil {
                return nil, err
        }
        defer rows.Close()

        items := make([]map[string]interface{}, 0)
        for rows.Next() {
                var name, genericName, status string
                var fullBoxes, strips, minStockLevel int64
                if err := rows.Scan(&name, &genericName, &fullBoxes, &strips, &minStockLevel, &status); err != nil {
                        return nil, err
                }
                items = append(items, map[string]interface{}{
                        "name": name, "generic_name": genericName, "quantity": fullBoxes, "strips": strips,
                        "min_stock_level": minStockLevel, "status": status,
                })
        }
        return items, rows.Err()
}

func pharmacyScope(c *gin.Context) (string, bool) {
        principal, ok := auth.PrincipalFromContext(c)
        if !ok || (principal.Type != auth.EmployeePrincipal && principal.Type != auth.CompanyUserPrincipal) {
                c.JSON(http.StatusForbidden, gin.H{"error": "pharmacy_account_required", "message": "A pharmacy account is required"})
                return "", false
        }
        if principal.PharmacyID == "" {
                c.JSON(http.StatusForbidden, gin.H{"error": "pharmacy_not_assigned", "message": "No pharmacy is assigned to this account"})
                return "", false
        }
        return principal.PharmacyID, true
}
