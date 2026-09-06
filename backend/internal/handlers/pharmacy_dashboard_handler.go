package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pharmacy-os/backend/internal/auth"
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

	const query = `
		SELECT batch_id, pharmacy_product_id, global_product_id, product_name,
		       generic_name, brand_name, barcode, dosage_form, strength,
		       batch_number, unit, quantity, cost_per_unit, total_cost,
		       expiry_date, days_until_expiry, selling_price, min_stock_level,
		       branch_name, status
		FROM current_inventory
		WHERE pharmacy_id = $1
		ORDER BY product_name, expiry_date NULLS LAST
		LIMIT 500
	`
	rows, err := h.db.Query(c.Request.Context(), query, pharmacyID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "inventory_query_failed", "message": "Could not load inventory"})
		return
	}
	defer rows.Close()

	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var (
			batchID, pharmacyProductID, globalProductID, name, dosageForm, batchNumber, unit, status string
			genericName, brandName, barcode, strength, branchName, expiryDate, daysUntilExpiry       interface{}
			quantity, costPerUnit, totalCost, sellingPrice, minStockLevel                            float64
		)
		if err := rows.Scan(
			&batchID, &pharmacyProductID, &globalProductID, &name,
			&genericName, &brandName, &barcode, &dosageForm, &strength,
			&batchNumber, &unit, &quantity, &costPerUnit, &totalCost,
			&expiryDate, &daysUntilExpiry, &sellingPrice, &minStockLevel,
			&branchName, &status,
		); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "inventory_query_failed", "message": "Could not read inventory"})
			return
		}
		items = append(items, gin.H{
			"batch_id": batchID, "pharmacy_product_id": pharmacyProductID,
			"global_product_id": globalProductID, "product_name": name,
			"generic_name": genericName, "brand_name": brandName, "barcode": barcode,
			"dosage_form": dosageForm, "strength": strength, "batch_number": batchNumber,
			"unit": unit, "quantity": quantity, "cost_per_unit": costPerUnit,
			"total_cost": totalCost, "expiry_date": expiryDate,
			"days_until_expiry": daysUntilExpiry, "selling_price": sellingPrice,
			"min_stock_level": minStockLevel, "branch_name": branchName, "status": status,
		})
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "inventory_query_failed", "message": "Could not read inventory"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": items})
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
