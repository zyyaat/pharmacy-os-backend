package handlers

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
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
			 FROM current_inventory
			 WHERE pharmacy_id = $1 AND quantity <= min_stock_level),
			(SELECT COUNT(*)::int FROM employees WHERE pharmacy_id = $1 AND status = 'active'),
			(SELECT COUNT(*)::int
			 FROM attendance_records
			 WHERE pharmacy_id = $1 AND clock_in::date = CURRENT_DATE),
			(SELECT COALESCE(SUM(ABS(sm.quantity)), 0)::float8
			 FROM stock_movements sm
			 JOIN inventory_batches ib ON ib.id = sm.batch_id
			 JOIN pharmacy_products pp ON pp.id = ib.pharmacy_product_id
			 WHERE pp.pharmacy_id = $1
			   AND sm.movement_type = 'sale'
			   AND sm.created_at::date = CURRENT_DATE)
	`

	var totalProducts, lowStock, activeEmployees, activeToday int
	var salesUnits float64
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

func (h *Handler) GetPharmacyInventory(c *gin.Context) {
	pharmacyID, ok := pharmacyScope(c)
	if !ok {
		return
	}

	// Task 122 — إصلاح «جاري تحميل البيانات» الأبدي (الويب) و«تعدُر الاتصال»
	// (الموبايل): الفيو current_inventory يستدعي calculate_batch_current_stock
	// ثلاثًا لكل صف (الكمية + حالة status + شرط WHERE) وORDER BY يفرض تقييم
	// الدالة على كل تشغيلات الصيدلية قبل LIMIT — استعلام واحد كان يولّد آلاف
	// التجميعات الفرعية على stock_movements فتتجاوز مدة الرد مهلة الموبايل
	// (40 ثانية) ويعلّق المتصفح بلا مهلة أصلًا.
	// هنا: تجميع LATERAL واحد لكل صف بالفهرس الغلاف
	// idx_stock_movements_batch_qty (batch_id, quantity) + COUNT(*) OVER()
	// للمجموع الكلي + تنفيذ limit/offset/search التي كان الخادم يتلقاها من
	// العميلين ويتجاهلها نهائيًا (عقد R-8 وapi_level 121 الموثق في طرفي
	// العميلين). شكل الرد والأعمدة والأسماء كما هي حرفيًا — بلا كسر.
	limit := 500
	if value, err := strconv.Atoi(c.Query("limit")); err == nil && value > 0 && value <= 500 {
		limit = value
	}
	offset := 0
	if value, err := strconv.Atoi(c.Query("offset")); err == nil && value > 0 {
		offset = value
	}
	search := strings.TrimSpace(c.Query("search"))

	const query = `
		SELECT ib.id, pp.id, gp.id, gp.name,
		       gp.generic_name, gp.brand_name, gp.barcode,
		       gp.dosage_form::text, gp.strength,
		       ib.batch_number, ib.unit::text,
		       COALESCE(sm.qty, 0), ib.cost_per_unit, ib.total_cost,
		       ib.expiry_date, ib.days_until_expiry,
		       pp.selling_price, pp.partial_selling_price,
		       pp.packaging_type::text, pp.units_per_box,
		       pp.min_stock_level, b.name,
		       CASE
		           WHEN COALESCE(sm.qty, 0) <= pp.min_stock_level THEN 'low_stock'
		           WHEN ib.expiry_date IS NOT NULL
		               AND ib.expiry_date <= CURRENT_DATE + INTERVAL '90 days' THEN 'expiring_soon'
		           WHEN ib.is_quarantined THEN 'quarantined'
		           ELSE 'normal'
		       END,
		       COUNT(*) OVER()
		FROM inventory_batches ib
		JOIN pharmacy_products pp ON ib.pharmacy_product_id = pp.id
		JOIN global_products gp ON pp.global_product_id = gp.id
		LEFT JOIN branches b ON ib.branch_id = b.id
		LEFT JOIN LATERAL (
			SELECT SUM(s.quantity)::numeric(12,4) AS qty
			FROM stock_movements s
			WHERE s.batch_id = ib.id
		) sm ON true
		WHERE pp.pharmacy_id = $1
		  AND (ib.quantity > 0 OR COALESCE(sm.qty, 0) > 0)
		  AND ($3::text = '' OR gp.name ILIKE '%' || $3 || '%'
		       OR COALESCE(gp.generic_name, '') ILIKE '%' || $3 || '%'
		       OR COALESCE(gp.barcode, '') ILIKE '%' || $3 || '%'
		       OR COALESCE(pp.internal_sku, '') ILIKE '%' || $3 || '%')
		ORDER BY gp.name, ib.expiry_date NULLS LAST
		LIMIT $2 OFFSET $4
	`
	rows, err := h.db.Query(c.Request.Context(), query, pharmacyID, limit, search, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "inventory_query_failed", "message": "Could not load inventory"})
		return
	}
	defer rows.Close()

	items := make([]map[string]interface{}, 0)
	total := 0
	for rows.Next() {
		var (
			batchID, pharmacyProductID, globalProductID, name, dosageForm, batchNumber, unit, status string
			genericName, brandName, barcode, strength, branchName, expiryDate, daysUntilExpiry       interface{}
			quantity, costPerUnit, totalCost, sellingPrice, partialSellingPrice, minStockLevel       float64
			packagingType                                                                            string
			unitsPerBox                                                                              int
			totalCount                                                                               int64
		)
		if err := rows.Scan(
			&batchID, &pharmacyProductID, &globalProductID, &name,
			&genericName, &brandName, &barcode, &dosageForm, &strength,
			&batchNumber, &unit, &quantity, &costPerUnit, &totalCost,
			&expiryDate, &daysUntilExpiry, &sellingPrice, &partialSellingPrice,
			&packagingType, &unitsPerBox, &minStockLevel,
			&branchName, &status,
			&totalCount,
		); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "inventory_query_failed", "message": "Could not read inventory"})
			return
		}
		total = int(totalCount)
		items = append(items, gin.H{
			"batch_id": batchID, "pharmacy_product_id": pharmacyProductID,
			"global_product_id": globalProductID, "product_name": name,
			"generic_name": genericName, "brand_name": brandName, "barcode": barcode,
			"dosage_form": dosageForm, "strength": strength, "batch_number": batchNumber,
			"unit": unit, "quantity": quantity, "cost_per_unit": costPerUnit,
			"total_cost": totalCost, "expiry_date": expiryDate,
			"days_until_expiry": daysUntilExpiry, "selling_price": sellingPrice,
			"partial_selling_price": partialSellingPrice, "packaging_type": packagingType,
			"units_per_box": unitsPerBox, "min_stock_level": minStockLevel,
			"branch_name": branchName, "status": status,
		})
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "inventory_query_failed", "message": "Could not read inventory"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "total": total, "limit": limit, "offset": offset})
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

func (h *Handler) lowStockItems(c *gin.Context, pharmacyID string) ([]map[string]interface{}, error) {
	const query = `
		SELECT product_name, COALESCE(generic_name, ''), quantity, min_stock_level, status
		FROM current_inventory
		WHERE pharmacy_id = $1 AND quantity <= min_stock_level
		ORDER BY quantity ASC, product_name
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
		var quantity, minStockLevel float64
		if err := rows.Scan(&name, &genericName, &quantity, &minStockLevel, &status); err != nil {
			return nil, err
		}
		items = append(items, map[string]interface{}{
			"name": name, "generic_name": genericName, "quantity": quantity,
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
