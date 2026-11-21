package handlers

import (
        "fmt"
        "net/http"
        "sort"
        "strings"
        "time"

        "github.com/gin-gonic/gin"
        "github.com/pharmacy-os/backend/internal/auth"
        "github.com/pharmacy-os/backend/internal/models"
        "golang.org/x/crypto/bcrypt"
)

// Flexible employee permissions (Task 42 + Task 152 fail-open fix):
// - permissions catalog grouped by module (Arabic labels from name_ar),
//   classified into tiers by migration 40 (operational/administrative/
//   privileged/platform — permissions.tier is the single source of truth)
// - role templates (قوالب جاهزة) with their permission keys
// - per-employee permission toggles: GET/PUT /pharmacy/employees/:id/permissions
// - creating a new employee with an explicit permission set
// - enforcement middleware: requirePharmacyPermission(key)
//
// Three-state rollout (Task 152 — closes the zero-permission fail-open):
//   - UNINITIALIZED (zero historical permission rows, revoked included):
//     fallback to the OPERATIONAL tier only.
//   - INITIALIZED with zero active rows (conscious empty save / all revoked):
//     Deny All — fail closed.
//   - INITIALIZED with active rows: exact key match; pharmacy.admin master bypass.
// The state lives in employees.permissions_initialized (migration 40 backfill:
// EXISTS over ALL historical rows). Company admins/managers always pass.

// ============================================================================
// Enforcement middleware
// ============================================================================

const permissionAdminKey = "pharmacy.admin"

// requirePharmacyPermission checks the current principal against the flexible
// permission system. Resolution order (Task 152 three-state model):
//  0. plan gate (Task 90): the company's subscription must be active/trial
//     AND its plan must include the key — this bounds owners and fallback
//     employees alike, because the plan is the company ceiling.
//  1. company_admin / company_manager principals: always allowed (owners).
//  2. UNINITIALIZED employees (zero historical permission rows, revoked
//     included): allowed iff the key is operational-tier — the migration-40
//     fallback. Anything else fails closed (reason=uninitialized_employee).
//  3. INITIALIZED employees with zero active rows: Deny All (reason=
//     permissions_not_initialized) — the zero-permission fail-open this closes.
//  4. INITIALIZED employees with active rows: exact key match, or the
//     pharmacy.admin master bypass (reason=no_matching_permission otherwise).
func (h *Handler) requirePharmacyPermission(permissionKey string) gin.HandlerFunc {
        return func(c *gin.Context) {
                principal, ok := auth.PrincipalFromContext(c)
                if !ok || principal.ID == "" {
                        c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
                                "error": "authentication_required", "message": "Authentication required",
                        })
                        return
                }

                // Plan gate first: a paid-for permission set bounds everyone in the
                // company regardless of their RBAC grants. Super admins bypass.
                if !h.enforcePlanGate(c, principal, permissionKey) {
                        return
                }

                // Owners (company admin/manager) keep full control of their pharmacy.
                if principal.Type == auth.CompanyUserPrincipal &&
                        (principal.Role == "company_admin" || principal.Role == "company_manager") {
                        c.Next()
                        return
                }
                if principal.Type != auth.EmployeePrincipal {
                        c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
                                "error": "permission_denied",
                                "message": "لا تملك صلاحية الوصول لهذا القسم",
                                "code": "PERMISSION_DENIED",
                                "required_permission": permissionKey,
                        })
                        return
                }

                // Task 152 — three-state resolution in ONE query: the active keys
                // plus the authoritative permissions_initialized flag (never
                // derived from the row count — a conscious empty save must read
                // initialized, not uninitialized).
                keys, initialized, err := h.employeePermissionState(c, principal.ID)
                if err != nil {
                        c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
                                "error": "permission_check_failed", "message": "تعذر التحقق من الصلاحيات",
                        })
                        return
                }

                // Fail-closed tier lens: an unknown key or a tier lookup failure
                // must never read as operational (the uninitialized fallback).
                tierOf := func(key string) string {
                        tier, err := h.permissionTier(c, key)
                        if err != nil {
                                return ""
                        }
                        return tier
                }

                denial := ""
                switch evaluatePermissionGate(initialized, keys, permissionKey, tierOf) {
                case gateAllow:
                        c.Next()
                        return
                case gateDenyUninitialized:
                        denial = "uninitialized_employee"
                case gateDenyNoActiveKeys:
                        denial = "permissions_not_initialized"
                default:
                        denial = "no_matching_permission"
                }

                c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
                        "error":   "permission_denied",
                        "message": "لا تملك صلاحية الوصول لهذا القسم",
                        "code":    "PERMISSION_DENIED",
                        "required_permission": permissionKey,
                        "reason":  denial,
                })
                return
        }
}

// ============================================================================
// Three-state permission gate (Task 152 — zero-permission fail-open fix)
// ============================================================================

// gateDecision is the outcome of the pure permission gate. A plain enum so
// evaluatePermissionGate stays unit-testable without a database.
type gateDecision int

const (
        gateAllow         gateDecision = iota // access granted
        gateDenyUninitialized                 // uninitialized employee + non-operational key
        gateDenyNoActiveKeys                  // initialized employee with zero active rows → Deny All
        gateDenyNoMatch                       // initialized employee, rows present, none match
)

// evaluatePermissionGate is the pure classifier behind requirePharmacyPermission
// and pharmacyAllows. State semantics (migration 40):
//   - initialized=false: the employee has NO historical permission rows at all
//     (never granted anything, never revoked anything). Fallback: the required
//     key must be operational-tier; administrative/privileged/platform/unknown
//     keys are denied. The pharmacy.admin bypass does NOT apply — holding rows
//     would make the employee initialized in the first place.
//   - initialized=true with zero active keys: Deny All (conscious empty save or
//     every row revoked) — the fail-open this closes.
//   - initialized=true with active keys: exact match, or the pharmacy.admin
//     master bypass.
// tierOf MUST fail closed: an unknown or errored tier must never read
// "operational", or the fallback silently widens.
func evaluatePermissionGate(initialized bool, activeKeys []string, requiredKey string, tierOf func(string) string) gateDecision {
        if !initialized {
                if tierOf != nil && tierOf(requiredKey) == "operational" {
                        return gateAllow
                }
                return gateDenyUninitialized
        }
        if len(activeKeys) == 0 {
                return gateDenyNoActiveKeys
        }
        for _, key := range activeKeys {
                if key == requiredKey {
                        return gateAllow
                }
        }
        // The pharmacy-wide admin key unlocks everything.
        for _, key := range activeKeys {
                if key == permissionAdminKey {
                        return gateAllow
                }
        }
        return gateDenyNoMatch
}

// employeePermissionState loads the three-state inputs in ONE round trip: the
// employee's active permission keys plus the initialization decision under
// UNION semantics (Task 152):
//
//      initialized = employees.permissions_initialized (migration 40 backfill:
//                    EXISTS over ALL historical rows) OR has-any-history
//
// The union is deliberate — the two signals can never disagree in a way that
// widens access: active rows imply history implies initialized (contract 1:
// employees with active permissions behave exactly as before the fix, even if
// some pre-migration path left their flag false), while the flag alone is what
// turns a conscious EMPTY save or a created-empty employee into Deny All
// instead of the fallback.
//   - no flag, no history        → uninitialized      → operational fallback
//   - flag=true, no rows         → initialized + empty → Deny All
//   - flag=true, only revoked    → initialized + empty → Deny All (the FILTER
//     excludes them from the aggregate)
//   - history (flag either way)  → initialized         → match / bypass
// An unknown employee yields no rows → scan error → callers fail closed (500).
func (h *Handler) employeePermissionState(c *gin.Context, employeeID string) (keys []string, initialized bool, err error) {
        rows, err := h.db.Query(c.Request.Context(), `
                SELECT e.permissions_initialized OR COALESCE(bool_or(ep.id IS NOT NULL), false),
                       COALESCE(array_agg(DISTINCT p.key) FILTER (WHERE ep.revoked_at IS NULL AND p.key IS NOT NULL), '{}')
                FROM employees e
                LEFT JOIN employee_permissions ep ON ep.employee_id = e.id
                LEFT JOIN permissions p ON p.id = ep.permission_id
                WHERE e.id = $1
                GROUP BY e.permissions_initialized
        `, employeeID)
        if err != nil {
                return nil, false, err
        }
        defer rows.Close()

        keys = make([]string, 0)
        for rows.Next() {
                if err := rows.Scan(&initialized, &keys); err != nil {
                        return nil, false, err
                }
        }
        return keys, initialized, rows.Err()
}

// permissionTier reads one key's classification from the catalog. Only the
// fallback/guard paths call it (uninitialized employees), so initialized
// employees pay nothing extra on the hot path. Unknown key or lookup failure
// reads as "" — callers must treat that as NOT operational (fail closed).
func (h *Handler) permissionTier(c *gin.Context, key string) (string, error) {
        var tier string
        err := h.db.QueryRow(c.Request.Context(),
                `SELECT tier FROM permissions WHERE key = $1`, key).Scan(&tier)
        if err != nil {
                return "", err
        }
        return tier, nil
}

// operationalKeys returns the OPERATIONAL tier of the catalog — the effective
// permission set of an uninitialized employee. Used by /permissions/me so the
// sidebar mirrors the gate exactly (login-time call, not the hot path).
func (h *Handler) operationalKeys(c *gin.Context) ([]string, error) {
        rows, err := h.db.Query(c.Request.Context(), `
                SELECT key FROM permissions WHERE tier = 'operational' ORDER BY key
        `)
        if err != nil {
                return nil, err
        }
        defer rows.Close()

        keys := make([]string, 0)
        for rows.Next() {
                var key string
                if err := rows.Scan(&key); err != nil {
                        return nil, err
                }
                keys = append(keys, key)
        }
        return keys, rows.Err()
}

// uninitializedGrantBlocked implements requirement 5 of the Task 152 fix: an
// UNINITIALIZED employee grantor may never grant non-operational keys to
// anyone, themselves included (the escalation chain the fail-open enabled:
// create employee + grant pharmacy.admin in one request). Initialized grantors
// pass — full tier enforcement on grants is a later phase, out of scope here.
// A tier lookup failure BLOCKS (fail closed): an unknown classification must
// never slip through the guard. Company-user grantors never reach this helper
// (callers check the principal type first).
func (h *Handler) uninitializedGrantBlocked(c *gin.Context, grantorID string, keys []string) (blocked bool, blockedKey string, err error) {
        _, grantorInitialized, stateErr := h.employeePermissionState(c, grantorID)
        if stateErr != nil {
                return false, "", stateErr
        }
        if grantorInitialized {
                return false, "", nil
        }
        for _, key := range keys {
                tier, tierErr := h.permissionTier(c, key)
                if tierErr != nil || tier != "operational" {
                        return true, key, nil
                }
        }
        return false, "", nil
}

// requirePharmacyOwner restricts a route to pharmacy owners only
// (company_admin / company_manager principals). Used for system-level
// observability endpoints (e.g. the schema-migrations ledger) that the
// frontend exposes to the owner alone — even a pharmacy.admin employee
// must not read infrastructure metadata through the API.
func (h *Handler) requirePharmacyOwner() gin.HandlerFunc {
        return func(c *gin.Context) {
                principal, ok := auth.PrincipalFromContext(c)
                if !ok || principal.ID == "" {
                        c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
                                "error": "authentication_required", "message": "Authentication required",
                        })
                        return
                }
                if principal.Type == auth.CompanyUserPrincipal &&
                        (principal.Role == "company_admin" || principal.Role == "company_manager") {
                        c.Next()
                        return
                }
                c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
                        "error":   "permission_denied",
                        "message": "هذه الصفحة متاحة لصاحب الصيدلية فقط",
                        "code":    "OWNER_ONLY",
                })
        }
}

// employeePermissionKeys returns the active permission keys for an employee.
func (h *Handler) employeePermissionKeys(c *gin.Context, employeeID string) ([]string, error) {
        rows, err := h.db.Query(c.Request.Context(), `
                SELECT p.key
                FROM employee_permissions ep
                JOIN permissions p ON p.id = ep.permission_id
                WHERE ep.employee_id = $1 AND ep.revoked_at IS NULL
        `, employeeID)
        if err != nil {
                return nil, err
        }
        defer rows.Close()

        keys := make([]string, 0)
        for rows.Next() {
                var key string
                if err := rows.Scan(&key); err != nil {
                        return nil, err
                }
                keys = append(keys, key)
        }
        return keys, rows.Err()
}

// ============================================================================
// Catalog + templates
// ============================================================================

type permissionEntry struct {
        Key      string `json:"key"`
        NameAr   string `json:"name_ar"`
        Category string `json:"category"`
}

type permissionModule struct {
        Module      string            `json:"module"`
        Label       string            `json:"label"`
        Permissions []permissionEntry `json:"permissions"`
}

var moduleLabels = map[string]string{
        "dashboard":  "لوحة التحكم",
        "pos":        "نقطة البيع",
        "sales":      "المبيعات والفواتير",
        "inventory":  "المخزون والأدوية",
        "products":   "المنتجات",
        "customers":  "حسابات العملاء",
        "employees":  "الموظفون",
        "attendance": "الحضور والانصراف",
        "branches":   "الفروع",
        "reports":    "التقارير",
        "settings":   "الإعدادات",
        "pharmacy":   "إدارة الصيدلية",
        "companies":  "الشركات",
        "accounts":   "الحسابات",
        "platform":   "المنصة",
}

// GetPermissionCatalog returns every permission grouped by module with Arabic
// labels. GET /pharmacy/permissions/catalog
func (h *Handler) GetPermissionCatalog(c *gin.Context) {
        rows, err := h.db.Query(c.Request.Context(), `
                SELECT key, COALESCE(NULLIF(name_ar, ''), name), COALESCE(category, ''),
                       module, sort_order
                FROM permissions
                ORDER BY module, sort_order, key
        `)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "catalog_query_failed", "message": "تعذر تحميل كتالوج الصلاحيات"})
                return
        }
        defer rows.Close()

        moduleOrder := make([]string, 0)
        moduleMap := make(map[string]*permissionModule)
        for rows.Next() {
                var key, nameAr, category, module string
                var sortOrder int
                if err := rows.Scan(&key, &nameAr, &category, &module, &sortOrder); err != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "catalog_query_failed", "message": "تعذر قراءة كتالوج الصلاحيات"})
                        return
                }
                // Hide platform/company-only permissions from the pharmacy UI.
                if module == "companies" || module == "accounts" || module == "platform" || module == "products" || module == "company_users" {
                        continue
                }
                mod, exists := moduleMap[module]
                if !exists {
                        label := moduleLabels[module]
                        if label == "" {
                                label = module
                        }
                        mod = &permissionModule{Module: module, Label: label, Permissions: make([]permissionEntry, 0)}
                        moduleMap[module] = mod
                        moduleOrder = append(moduleOrder, module)
                }
                mod.Permissions = append(mod.Permissions, permissionEntry{Key: key, NameAr: nameAr, Category: category})
        }
        if err := rows.Err(); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "catalog_query_failed", "message": "تعذر قراءة كتالوج الصلاحيات"})
                return
        }

        modules := make([]permissionModule, 0, len(moduleOrder))
        for _, name := range moduleOrder {
                modules = append(modules, *moduleMap[name])
        }
        c.JSON(http.StatusOK, gin.H{"data": modules})
}

type roleTemplate struct {
        ID           string   `json:"id"`
        Name         string   `json:"name"`
        DisplayName  string   `json:"display_name"`
        DisplayNameAr string  `json:"display_name_ar"`
        DescriptionAr string  `json:"description_ar"`
        IsSystem     bool     `json:"is_system"`
        Permissions  []string `json:"permissions"`
}

// GetPermissionTemplates returns the ready-made role templates
// (قوالب الصلاحيات الجاهزة). GET /pharmacy/permissions/templates
func (h *Handler) GetPermissionTemplates(c *gin.Context) {
        roleRows, err := h.db.Query(c.Request.Context(), `
                SELECT id::text, name, COALESCE(display_name, ''),
                       COALESCE(display_name_ar, ''), COALESCE(description_ar, ''), is_system
                FROM roles
                WHERE is_active = true
                ORDER BY sort_order, name
        `)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "templates_query_failed", "message": "تعذر تحميل قوالب الصلاحيات"})
                return
        }
        defer roleRows.Close()

        templates := make([]*roleTemplate, 0)
        indexByID := make(map[string]int)
        for roleRows.Next() {
                var tpl roleTemplate
                if err := roleRows.Scan(&tpl.ID, &tpl.Name, &tpl.DisplayName, &tpl.DisplayNameAr, &tpl.DescriptionAr, &tpl.IsSystem); err != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "templates_query_failed", "message": "تعذر قراءة قوالب الصلاحيات"})
                        return
                }
                tpl.Permissions = make([]string, 0)
                if tpl.DisplayNameAr == "" {
                        tpl.DisplayNameAr = tpl.DisplayName
                }
                indexByID[tpl.ID] = len(templates)
                templates = append(templates, &tpl)
        }
        if err := roleRows.Err(); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "templates_query_failed", "message": "تعذر قراءة قوالب الصلاحيات"})
                return
        }

        permRows, err := h.db.Query(c.Request.Context(), `
                SELECT r.id::text, p.key
                FROM role_permissions rp
                JOIN roles r ON r.id = rp.role_id
                JOIN permissions p ON p.id = rp.permission_id
                ORDER BY p.module, p.key
        `)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "templates_query_failed", "message": "تعذر تحميل قوالب الصلاحيات"})
                return
        }
        defer permRows.Close()
        for permRows.Next() {
                var roleID, key string
                if err := permRows.Scan(&roleID, &key); err != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "templates_query_failed", "message": "تعذر قراءة قوالب الصلاحيات"})
                        return
                }
                if idx, exists := indexByID[roleID]; exists {
                        templates[idx].Permissions = append(templates[idx].Permissions, key)
                }
        }
        if err := permRows.Err(); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "templates_query_failed", "message": "تعذر قراءة قوالب الصلاحيات"})
                return
        }

        c.JSON(http.StatusOK, gin.H{"data": templates})
}

// GetMyPermissions returns the current principal's permission keys so the UI
// can hide sections the principal cannot open.
// GET /pharmacy/permissions/me
func (h *Handler) GetMyPermissions(c *gin.Context) {
        principal, ok := auth.PrincipalFromContext(c)
        if !ok || principal.ID == "" {
                c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication_required", "message": "Authentication required"})
                return
        }

        payload := gin.H{
                "principal_type": principal.Type,
                "role":           principal.Role,
                "permissions":    make([]string, 0),
                "full_access":    false,
        }

        if principal.Type == auth.CompanyUserPrincipal {
                if principal.Role == "company_admin" || principal.Role == "company_manager" {
                        payload["full_access"] = true
                }
                c.JSON(http.StatusOK, payload)
                return
        }

        keys, initialized, err := h.employeePermissionState(c, principal.ID)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "permissions_query_failed", "message": "تعذر تحميل الصلاحيات"})
                return
        }
        sort.Strings(keys)
        payload["permissions_initialized"] = initialized
        if !initialized {
                // Task 152 — uninitialized fallback: report the OPERATIONAL set
                // so the UI shows exactly what the gate enforces (no phantom
                // sections that would 403 on click).
                opKeys, opErr := h.operationalKeys(c)
                if opErr != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "permissions_query_failed", "message": "تعذر تحميل الصلاحيات"})
                        return
                }
                payload["permissions"] = opKeys
                payload["fallback_operational"] = true
                c.JSON(http.StatusOK, payload)
                return
        }
        payload["permissions"] = keys
        // Initialized + zero active rows = Deny All: deliberately NO full_access
        // flag here (the old len==0 → full_access mirror was the fail-open's
        // reporting face). pharmacy.admin still unlocks everything in the UI.
        for _, key := range keys {
                if key == permissionAdminKey {
                        payload["full_access"] = true
                }
        }
        c.JSON(http.StatusOK, payload)
}

// ============================================================================
// Employee permission toggles
// ============================================================================

// GetEmployeePermissions returns the current toggle state of one employee.
// GET /pharmacy/employees/:id/permissions
func (h *Handler) GetEmployeePermissions(c *gin.Context) {
        employeeID := c.Param("id")
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }

        var employeePharmacyID string
        var displayName, email string
        err := h.db.QueryRow(c.Request.Context(), `
                SELECT pharmacy_id::text, COALESCE(display_name, first_name || ' ' || last_name), email
                FROM employees WHERE id = $1
        `, employeeID).Scan(&employeePharmacyID, &displayName, &email)
        if err != nil {
                c.JSON(http.StatusNotFound, gin.H{"error": "employee_not_found", "message": "الموظف غير موجود"})
                return
        }
        if employeePharmacyID != pharmacyID {
                c.JSON(http.StatusForbidden, gin.H{"error": "employee_out_of_scope", "message": "الموظف لا ينتمي لهذه الصيدلية"})
                return
        }

        keys, initialized, err := h.employeePermissionState(c, employeeID)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "permissions_query_failed", "message": "تعذر تحميل صلاحيات الموظف"})
                return
        }
        sort.Strings(keys)
        holdsAdmin := false
        for _, key := range keys {
                if key == permissionAdminKey {
                        holdsAdmin = true
                }
        }

        c.JSON(http.StatusOK, gin.H{
                "data": gin.H{
                        "employee_id":  employeeID,
                        "display_name": displayName,
                        "email":        email,
                        "permissions":  keys,
                        "has_explicit": len(keys) > 0,
                        // Task 152 — honest reporting: "no rows" no longer means
                        // full access. full_access is the pharmacy.admin bypass;
                        // permissions_initialized distinguishes the two empty
                        // states (uninitialized fallback vs conscious Deny All).
                        "full_access":             holdsAdmin,
                        "permissions_initialized": initialized,
                },
        })
}

// UpdateEmployeePermissions replaces the employee's active permission set with
// the provided keys (the toggle state). The change runs in one transaction:
// revoke what disappeared, grant what is new, keep the rest untouched so the
// grant/revoke audit trail stays intact.
// PUT /pharmacy/employees/:id/permissions
func (h *Handler) UpdateEmployeePermissions(c *gin.Context) {
        employeeID := c.Param("id")
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }
        principal, _ := auth.PrincipalFromContext(c)

        var body struct {
                Permissions []string `json:"permissions"`
        }
        if err := c.ShouldBindJSON(&body); err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": "صيغة الطلب غير صحيحة"})
                return
        }

        // Validate + dedupe keys against the catalog.
        wanted := make([]string, 0)
        seen := make(map[string]bool)
        for _, key := range body.Permissions {
                key = strings.TrimSpace(key)
                if key == "" || seen[key] {
                        continue
                }
                seen[key] = true
                wanted = append(wanted, key)
        }

        // Task 152 — grant guard (requirement 5 of the fail-open fix): an
        // UNINITIALIZED employee grantor may never grant non-operational keys
        // to anyone, themselves included. The route gate already blocks this
        // path (employees.manage_permissions is administrative, so the fallback
        // never lets an uninitialized employee in), but the invariant stays
        // enforced at the grant site so a future route change cannot silently
        // reopen the escalation chain create-employee → grant pharmacy.admin.
        if principal != nil && principal.Type == auth.EmployeePrincipal {
                blocked, blockedKey, stateErr := h.uninitializedGrantBlocked(c, principal.ID, wanted)
                if stateErr != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "permission_check_failed", "message": "تعذر التحقق من صلاحيات المانح"})
                        return
                }
                if blocked {
                        c.JSON(http.StatusForbidden, gin.H{
                                "error":   "grant_blocked_uninitialized",
                                "message": "الحساب غير مهيأ بعد — لا يمكن منح صلاحيات غير تشغيلية",
                                "code":    "GRANT_BLOCKED_UNINITIALIZED",
                                "key":     blockedKey,
                        })
                        return
                }
        }

        tx, err := h.db.Begin(c.Request.Context())
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "transaction_failed", "message": "تعذر تنفيذ العملية"})
                return
        }
        defer tx.Rollback(c.Request.Context())

        // Tenant RLS context (T-B5 audit contract — see CreatePharmacyEmployee).
        if _, err := tx.Exec(c.Request.Context(), `
                SELECT set_config('app.current_pharmacy_id', $1, true),
                       set_config('app.current_user_id', $2, true)
        `, pharmacyID, principal.ID); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "scope_setup_failed", "message": "تعذر تجهيز سياق الصيدلية"})
                return
        }

        var employeePharmacyID string
        err = tx.QueryRow(c.Request.Context(), `
                SELECT pharmacy_id::text FROM employees WHERE id = $1
        `, employeeID).Scan(&employeePharmacyID)
        if err != nil {
                c.JSON(http.StatusNotFound, gin.H{"error": "employee_not_found", "message": "الموظف غير موجود"})
                return
        }
        if employeePharmacyID != pharmacyID {
                c.JSON(http.StatusForbidden, gin.H{"error": "employee_out_of_scope", "message": "الموظف لا ينتمي لهذه الصيدلية"})
                return
        }

        // Guard: the caller can never lock themselves out. The acting employee
        // must keep pharmacy.admin or employees.manage_permissions.
        if principal != nil && principal.Type == auth.EmployeePrincipal && principal.ID == employeeID {
                hasSelfGuard := false
                for _, key := range wanted {
                        if key == permissionAdminKey || key == "employees.manage_permissions" {
                                hasSelfGuard = true
                        }
                }
                if !hasSelfGuard {
                        c.JSON(http.StatusBadRequest, gin.H{
                                "error":   "self_lockout_blocked",
                                "message": "لا يمكن إزالة صلاحية إدارة الصلاحيات من حسابك الحالي",
                        })
                        return
                }
        }

        // Resolve current active set.
        current := make(map[string]bool)
        rows, err := tx.Query(c.Request.Context(), `
                SELECT p.key
                FROM employee_permissions ep
                JOIN permissions p ON p.id = ep.permission_id
                WHERE ep.employee_id = $1 AND ep.revoked_at IS NULL
        `, employeeID)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "permissions_query_failed", "message": "تعذر قراءة الصلاحيات الحالية"})
                return
        }
        for rows.Next() {
                var key string
                if err := rows.Scan(&key); err != nil {
                        rows.Close()
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "permissions_query_failed", "message": "تعذر قراءة الصلاحيات الحالية"})
                        return
                }
                current[key] = true
        }
        rows.Close()
        if err := rows.Err(); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "permissions_query_failed", "message": "تعذر قراءة الصلاحيات الحالية"})
                return
        }

        wantedSet := make(map[string]bool, len(wanted))
        for _, key := range wanted {
                wantedSet[key] = true
        }

        // Revoke keys that disappeared (soft delete keeps the audit trail).
        for key := range current {
                if wantedSet[key] {
                        continue
                }
                if _, err := tx.Exec(c.Request.Context(), `
                        UPDATE employee_permissions
                        SET revoked_at = NOW(), revoked_by = $2
                        WHERE employee_id = $1 AND permission_id = (SELECT id FROM permissions WHERE key = $3)
                          AND revoked_at IS NULL
                `, employeeID, nullablePrincipalID(principal), key); err != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "revoke_failed", "message": "تعذر تحديث الصلاحيات"})
                        return
                }
        }

        // Grant keys that appeared (reactivating a previously revoked row keeps
        // history compact and avoids unique-index conflicts).
        for _, key := range wanted {
                if current[key] {
                        continue
                }
                if _, err := tx.Exec(c.Request.Context(), `
                        INSERT INTO employee_permissions (employee_id, permission_id, granted_by, notes)
                        VALUES ($1, (SELECT id FROM permissions WHERE key = $2), $3, 'صلاحيات مرنة — تعديل يدوي')
                        ON CONFLICT (employee_id, permission_id) WHERE revoked_at IS NULL
                        DO NOTHING
                `, employeeID, key, nullablePrincipalID(principal)); err != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "grant_failed", "message": "تعذر تحديث الصلاحيات: صلاحية غير معروفة " + key})
                        return
                }
        }

        // Task 152 — a conscious save marks the employee initialized: an empty
        // set is a real decision (Deny All), not an uninitialized accident.
        if _, err := tx.Exec(c.Request.Context(), `
                UPDATE employees SET permissions_initialized = TRUE, updated_at = NOW()
                WHERE id = $1
        `, employeeID); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "initialization_failed", "message": "تعذر تحديث حالة التهيئة"})
                return
        }

        // permission_version bumps automatically through the trigger, refreshing
        // cached permission decisions for this employee.

        // T-B5 — permissions audit. The delta is computed against the set
        // read before the writes; the row lands inside the same transaction
        // so it commits (or rolls back) atomically with the change itself.
        granted := make([]string, 0)
        for _, key := range wanted {
                if !current[key] {
                        granted = append(granted, key)
                }
        }
        revoked := make([]string, 0)
        for key := range current {
                if !wantedSet[key] {
                        revoked = append(revoked, key)
                }
        }
        if len(granted) > 0 || len(revoked) > 0 {
                // The plan names the grant event; a pure revoke or a mixed
                // change gets the matching action so the feed never lies.
                action := "permissions.updated"
                switch {
                case len(granted) > 0 && len(revoked) == 0:
                        action = "permissions.granted"
                case len(revoked) > 0 && len(granted) == 0:
                        action = "permissions.revoked"
                }
                summary := fmt.Sprintf("تحديث صلاحيات الموظف — منح %d وسحب %d", len(granted), len(revoked))
                if err := writeAuditLog(c.Request.Context(), tx, principal,
                        action, "update", "employee", employeeID,
                        map[string]any{"granted": granted, "revoked": revoked},
                        summary); err != nil {
                        auditFailure(action, err)
                }
        }

        if err := tx.Commit(c.Request.Context()); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "commit_failed", "message": "تعذر حفظ الصلاحيات"})
                return
        }

        keys, err := h.employeePermissionKeys(c, employeeID)
        if err != nil {
                keys = wanted
        }
        sort.Strings(keys)
        c.JSON(http.StatusOK, gin.H{
                "data": gin.H{
                        "employee_id": employeeID,
                        "permissions": keys,
                        "updated_at":  time.Now().UTC().Format(time.RFC3339),
                },
        })
}

// nullablePrincipalID adapts a company principal (no employees row) to the
// nullable granted_by/revoked_by columns.
func nullablePrincipalID(principal *auth.Principal) *string {
        if principal == nil || principal.ID == "" {
                return nil
        }
        // Only employee rows are valid FK targets for granted_by; company users
        // and other principals are recorded as NULL (notes keep the context).
        if principal.Type != auth.EmployeePrincipal {
                return nil
        }
        id := principal.ID
        return &id
}

// ============================================================================
// Create employee (with template/permission set) + activate/deactivate
// ============================================================================

// CreatePharmacyEmployee adds a new staff member with an optional ready-made
// template and an explicit permission set (the toggle state chosen in the UI).
// POST /pharmacy/employees
func (h *Handler) CreatePharmacyEmployee(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }
        principal, _ := auth.PrincipalFromContext(c)
        // Plan limit (Task 90): employees is a commercial ceiling.
        if !h.enforcePlanLimit(c, principal, models.LimitKeyEmployees) {
                return
        }

        var body struct {
                FirstName   string   `json:"first_name"`
                LastName    string   `json:"last_name"`
                Email       string   `json:"email"`
                Password    string   `json:"password"`
                Phone       string   `json:"phone"`
                JobTitle    string   `json:"job_title"`
        Role        string   `json:"role"`
                BranchID    string   `json:"branch_id"`
                TemplateID  string   `json:"template_id"`
                Permissions []string `json:"permissions"`
        }
        if err := c.ShouldBindJSON(&body); err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": "صيغة الطلب غير صحيحة"})
                return
        }

        body.FirstName = strings.TrimSpace(body.FirstName)
        body.LastName = strings.TrimSpace(body.LastName)
        body.Email = strings.TrimSpace(strings.ToLower(body.Email))
        body.Phone = strings.TrimSpace(body.Phone)
        body.JobTitle = strings.TrimSpace(body.JobTitle)
        body.BranchID = strings.TrimSpace(body.BranchID)
        body.TemplateID = strings.TrimSpace(body.TemplateID)
        body.Role = strings.TrimSpace(body.Role)
        switch body.Role {
        case "":
                body.Role = "pharmacist"
        case "pharmacist", "cashier", "inventory_manager", "accountant", "hr_manager", "pharmacy_admin":
                // allowed staff roles
        default:
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_role", "message": "الوظيفة غير معروفة"})
                return
        }

        if body.FirstName == "" || body.LastName == "" || body.Email == "" {
                c.JSON(http.StatusBadRequest, gin.H{"error": "missing_fields", "message": "الاسم والبريد الإلكتروني مطلوبان"})
                return
        }
        if len(body.Password) < 8 {
                c.JSON(http.StatusBadRequest, gin.H{"error": "weak_password", "message": "كلمة المرور يجب ألا تقل عن 8 أحرف"})
                return
        }

        // Employees log in with their email — it must not collide with the owner's
        // company-user login (login resolution checks company users first).
        var emailClash bool
        if err := h.db.QueryRow(c.Request.Context(), `
                SELECT EXISTS(
                        SELECT 1 FROM company_users cu
                        JOIN accounts a ON a.company_id = cu.company_id AND a.deleted_at IS NULL
                        JOIN pharmacies p ON p.account_id = a.id
                        WHERE p.id = $1 AND LOWER(cu.email) = LOWER($2) AND cu.deleted_at IS NULL
                )
        `, pharmacyID, body.Email).Scan(&emailClash); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "email_check_failed", "message": "تعذر التحقق من البريد الإلكتروني"})
                return
        }
        if emailClash {
                c.JSON(http.StatusConflict, gin.H{"error": "email_taken", "message": "هذا البريد مستخدم بالفعل في حساب الصيدلية"})
                return
        }

        var accountID string
        if err := h.db.QueryRow(c.Request.Context(), `
                SELECT account_id::text FROM pharmacies WHERE id = $1
        `, pharmacyID).Scan(&accountID); err != nil {
                c.JSON(http.StatusForbidden, gin.H{"error": "pharmacy_not_found", "message": "الصيدلية غير موجودة"})
                return
        }

        hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "hash_failed", "message": "تعذر تجهيز كلمة المرور"})
                return
        }

        tx, err := h.db.Begin(c.Request.Context())
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "transaction_failed", "message": "تعذر تنفيذ العملية"})
                return
        }
        defer tx.Rollback(c.Request.Context())

        // Tenant RLS context for the audit insert (same contract as every
        // other audit-scoped transaction — writeAuditLog's note; harmless
        // until batch D forces RLS, load-bearing afterwards).
        if _, err := tx.Exec(c.Request.Context(), `
                SELECT set_config('app.current_pharmacy_id', $1, true),
                       set_config('app.current_user_id', $2, true)
        `, pharmacyID, principal.ID); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "scope_setup_failed", "message": "تعذر تجهيز سياق الصيدلية"})
                return
        }

        // Owner-created staff accounts are immediately login-ready.
        var employeeID string
        var branchArg interface{}
        if body.BranchID != "" {
                branchArg = body.BranchID
        }
        err = tx.QueryRow(c.Request.Context(), `
                INSERT INTO employees (
                        account_id, pharmacy_id, branch_id, email, first_name, last_name,
                        display_name, phone, job_title, role, status,
                        password_hash, email_verified_at, permissions_initialized
                ) VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), NULLIF($9, ''), $10, 'active',
                          $11, NOW(), TRUE)
                RETURNING id::text
        `, accountID, pharmacyID, branchArg, body.Email, body.FirstName, body.LastName,
                strings.TrimSpace(body.FirstName+" "+body.LastName), body.Phone, body.JobTitle, body.Role,
                string(hash)).Scan(&employeeID)
        if err != nil {
                if strings.Contains(err.Error(), "employees_unique_email_per_pharmacy") || strings.Contains(err.Error(), "duplicate key") {
                        c.JSON(http.StatusConflict, gin.H{"error": "email_taken", "message": "هذا البريد مستخدم بالفعل لهذه الصيدلية"})
                        return
                }
                c.JSON(http.StatusInternalServerError, gin.H{"error": "employee_insert_failed", "message": "تعذر إضافة الموظف"})
                return
        }

        // Permission set: explicit list wins; otherwise fall back to template.
        // Task 152 — creation ALWAYS writes permissions_initialized = TRUE (in
        // the INSERT above): the owner consciously chose this set, and an empty
        // list is a real decision (Deny All), never an uninitialized accident.
        keys := make([]string, 0)
        seen := make(map[string]bool)
        for _, key := range body.Permissions {
                key = strings.TrimSpace(key)
                if key != "" && !seen[key] {
                        seen[key] = true
                        keys = append(keys, key)
                }
        }
        if len(keys) == 0 && body.TemplateID != "" {
                tplRows, err := tx.Query(c.Request.Context(), `
                        SELECT p.key
                        FROM role_permissions rp
                        JOIN permissions p ON p.id = rp.permission_id
                        WHERE rp.role_id = $1
                        ORDER BY p.key
                `, body.TemplateID)
                if err != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "template_query_failed", "message": "تعذر تحميل قالب الصلاحيات"})
                        return
                }
                for tplRows.Next() {
                        var key string
                        if err := tplRows.Scan(&key); err != nil {
                                tplRows.Close()
                                c.JSON(http.StatusInternalServerError, gin.H{"error": "template_query_failed", "message": "تعذر تحميل قالب الصلاحيات"})
                                return
                        }
                        if !seen[key] {
                                seen[key] = true
                                keys = append(keys, key)
                        }
                }
                tplRows.Close()
        }

        // Task 152 — grant guard (defense in depth for requirement 5): an
        // UNINITIALIZED employee grantor may never seed a new employee with
        // non-operational keys. This is the exact escalation chain the fail-open
        // enabled (create employee + grant pharmacy.admin in one request); the
        // route gate already blocks it (employees.create is administrative) but
        // the invariant is enforced at the grant site too.
        if principal != nil && principal.Type == auth.EmployeePrincipal {
                blocked, blockedKey, stateErr := h.uninitializedGrantBlocked(c, principal.ID, keys)
                if stateErr != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "permission_check_failed", "message": "تعذر التحقق من صلاحيات المانح"})
                        return
                }
                if blocked {
                        c.JSON(http.StatusForbidden, gin.H{
                                "error":   "grant_blocked_uninitialized",
                                "message": "الحساب غير مهيأ بعد — لا يمكن منح صلاحيات غير تشغيلية للموظف الجديد",
                                "code":    "GRANT_BLOCKED_UNINITIALIZED",
                                "key":     blockedKey,
                        })
                        return
                }
        }

        grantedBy := nullablePrincipalID(principal)
        for _, key := range keys {
                if _, err := tx.Exec(c.Request.Context(), `
                        INSERT INTO employee_permissions (employee_id, permission_id, granted_by, notes)
                        VALUES ($1, (SELECT id FROM permissions WHERE key = $2), $3, 'صلاحيات أولية عند إضافة الموظف')
                        ON CONFLICT (employee_id, permission_id) WHERE revoked_at IS NULL
                        DO NOTHING
                `, employeeID, key, grantedBy); err != nil {
                        c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_permission", "message": "صلاحية غير معروفة: " + key})
                        return
                }
        }

        // T-B5 — employee.created audit, inside the creation transaction so
        // the event is atomic with the new staff row (house pattern from the
        // barcode feature). The initial permission set is summarized in the
        // same row instead of a second permissions.granted event.
        if err := writeAuditLog(c.Request.Context(), tx, principal,
                "employee.created", "create", "employee", employeeID,
                map[string]any{
                        "email":             body.Email,
                        "role":              body.Role,
                        "branch_id":         body.BranchID,
                        "permissions_count": len(keys),
                },
                "إضافة موظف جديد للصيدلية"); err != nil {
                auditFailure("employee.created", err)
        }

        if err := tx.Commit(c.Request.Context()); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "commit_failed", "message": "تعذر حفظ الموظف"})
                return
        }

        finalKeys, err := h.employeePermissionKeys(c, employeeID)
        if err != nil {
                finalKeys = keys
        }
        sort.Strings(finalKeys)

        c.JSON(http.StatusCreated, gin.H{
                "data": gin.H{
                        "id":          employeeID,
                        "email":       body.Email,
                        "first_name":  body.FirstName,
                        "last_name":   body.LastName,
                        "permissions": finalKeys,
                        "has_explicit": len(finalKeys) > 0,
                },
        })
}

// SetPharmacyEmployeeStatus activates or deactivates a staff account
// (تفعيل/إيقاف الموظف). Deactivation blocks login instantly.
// PATCH /pharmacy/employees/:id/status
func (h *Handler) SetPharmacyEmployeeStatus(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }
        employeeID := c.Param("id")

        var body struct {
                Status string `json:"status"`
        }
        if err := c.ShouldBindJSON(&body); err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": "صيغة الطلب غير صحيحة"})
                return
        }
        status := strings.ToLower(strings.TrimSpace(body.Status))
        if status != "active" && status != "inactive" {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_status", "message": "الحالة المسموحة: active أو inactive"})
                return
        }

        principal, _ := auth.PrincipalFromContext(c)
        if principal != nil && principal.Type == auth.EmployeePrincipal && principal.ID == employeeID && status == "inactive" {
                c.JSON(http.StatusBadRequest, gin.H{"error": "self_deactivation_blocked", "message": "لا يمكنك إيقاف حسابك الحالي"})
                return
        }

        tag, err := h.db.Exec(c.Request.Context(), `
                UPDATE employees SET status = $2::employee_status, is_active = ($2 = 'active'),
                updated_at = NOW()
                WHERE id = $1 AND pharmacy_id = $3
        `, employeeID, status, pharmacyID)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "status_update_failed", "message": "تعذر تحديث حالة الموظف"})
                return
        }
        if tag.RowsAffected() == 0 {
                c.JSON(http.StatusNotFound, gin.H{"error": "employee_not_found", "message": "الموظف غير موجود"})
                return
        }

        // Deactivated employees lose active sessions immediately.
        if status == "inactive" {
                if _, err := h.db.Exec(c.Request.Context(), `
                        DELETE FROM auth_sessions
                        WHERE principal_type = 'employee' AND principal_id = $1
                `, employeeID); err != nil {
                        // Session cleanup is best-effort; the login check still blocks.
                        fmt.Printf("[PERMISSIONS] session cleanup failed for %s: %v\n", employeeID, err)
                }
        }

        c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": employeeID, "status": status}})
}
