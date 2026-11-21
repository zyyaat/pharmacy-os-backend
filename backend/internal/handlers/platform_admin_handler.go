package handlers

import (
        "fmt"
        "net/http"
        "strconv"
        "strings"
        "time"

        "github.com/gin-gonic/gin"
        "github.com/pharmacy-os/backend/internal/auth"
        "github.com/pharmacy-os/backend/internal/services"
)

// requirePlatformSuperAdmin is intentionally separate from company permission
// checks. A platform dashboard is global by definition and must never fall
// back to the authenticated user's company scope.
func requirePlatformSuperAdmin() gin.HandlerFunc {
        return func(c *gin.Context) {
                principal, ok := auth.PrincipalFromContext(c)
                if !ok || principal.Type != auth.CompanyUserPrincipal {
                        c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
                                "error":   "platform_admin_required",
                                "message": "A platform administrator account is required",
                        })
                        return
                }
                if principal.Role != "super_admin" {
                        c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
                                "error":   "super_admin_required",
                                "message": "This area is restricted to platform super administrators",
                        })
                        return
                }
                c.Next()
        }
}

// GetPlatformAdminStats returns global platform metrics for the SaaS owner.
// ADR-0005 (api_level 107): the aggregate query lives in
// services.PlatformStats now — this handler is protocol only, and the wire
// shape is byte-identical to the pre-extraction version (same keys, same
// error bodies; pinned by TestPlatformStatsServiceDB).
func (h *Handler) GetPlatformAdminStats(c *gin.Context) {
        stats, err := services.PlatformStats(c.Request.Context(), h.db)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{
                        "error":   "platform_stats_query_failed",
                        "message": "Could not load platform statistics",
                })
                return
        }

        activity, err := h.platformActivity(c)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{
                        "error":   "platform_activity_query_failed",
                        "message": "Could not load platform activity",
                })
                return
        }

        c.JSON(http.StatusOK, gin.H{
                "totalCompanies":     stats.TotalCompanies,
                "activeCompanies":    stats.ActiveCompanies,
                "suspendedCompanies": stats.SuspendedCompanies,
                "totalAccounts":      stats.TotalAccounts,
                "totalPharmacies":    stats.TotalPharmacies,
                "totalUsers":         stats.TotalUsers,
                "activeUsers":        stats.ActiveUsers,
                "recentActivity":     activity,
        })
}

// GetPlatformSettings returns settings that apply to newly provisioned
// companies. This endpoint is intentionally restricted to Super Admins.
func (h *Handler) GetPlatformSettings(c *gin.Context) {
        var defaultTrialDays int
        err := h.db.QueryRow(c.Request.Context(), `
                SELECT COALESCE((setting_value->>'default_trial_days')::int, 30)
                FROM platform_settings
                WHERE setting_key = 'trial'
        `).Scan(&defaultTrialDays)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{
                        "error": "platform_settings_query_failed", "message": "Could not load platform settings",
                })
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": gin.H{"default_trial_days": defaultTrialDays}})
}

func (h *Handler) UpdatePlatformSettings(c *gin.Context) {
        var request struct {
                DefaultTrialDays int `json:"default_trial_days"`
        }
        if err := c.ShouldBindJSON(&request); err != nil || request.DefaultTrialDays < 1 || request.DefaultTrialDays > 3650 {
                c.JSON(http.StatusBadRequest, gin.H{
                        "error": "invalid_trial_days", "message": "مدة التجربة يجب أن تكون بين يوم واحد و3650 يومًا",
                })
                return
        }
        principal, ok := auth.PrincipalFromContext(c)
        if !ok || principal.ID == "" {
                c.JSON(http.StatusForbidden, gin.H{"error": "super_admin_required"})
                return
        }
        // jsonb_set (not object replace) so sibling keys — e.g.
        // default_plan_slug used by SaaS registrations (Task 90) — survive.
        if _, err := h.db.Exec(c.Request.Context(), `
                INSERT INTO platform_settings (setting_key, setting_value, updated_by, updated_at)
                VALUES ('trial', jsonb_build_object('default_trial_days', $1), $2, NOW())
                ON CONFLICT (setting_key) DO UPDATE SET
                        setting_value = jsonb_set(platform_settings.setting_value,
                                                 '{default_trial_days}', to_jsonb($1::int), TRUE),
                        updated_by = EXCLUDED.updated_by,
                        updated_at = NOW()
        `, request.DefaultTrialDays, principal.ID); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{
                        "error": "platform_settings_update_failed", "message": "Could not save platform settings",
                })
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": gin.H{"default_trial_days": request.DefaultTrialDays}})
}

func (h *Handler) platformActivity(c *gin.Context) ([]dashboardActivity, error) {
        rows, err := h.db.Query(c.Request.Context(), `
                SELECT
                        al.id::text,
                        al.action,
                        COALESCE(NULLIF(al.changes_summary, ''), al.action),
                        COALESCE(NULLIF(al.actor_display_name, ''), NULLIF(al.actor_email, ''), 'System'),
                        al.created_at
                FROM audit_logs al
                ORDER BY al.created_at DESC
                LIMIT 10
        `)
        if err != nil {
                return nil, err
        }
        defer rows.Close()

        activity := make([]dashboardActivity, 0)
        for rows.Next() {
                var item dashboardActivity
                var timestamp time.Time
                if err := rows.Scan(&item.ID, &item.Type, &item.Description, &item.UserName, &timestamp); err != nil {
                        return nil, err
                }
                item.Timestamp = timestamp.UTC().Format(time.RFC3339)
                activity = append(activity, item)
        }
        return activity, rows.Err()
}

// ListPlatformCompanies returns every active company with its global counts.
func (h *Handler) ListPlatformCompanies(c *gin.Context) {
        page, pageSize := pagination(c)
        search := strings.TrimSpace(c.Query("search"))
        status := strings.TrimSpace(c.Query("status"))

        // Task 164: the platform shell company is infrastructure — never a
        // row in the customer list (count and summary share this predicate).
        where := []string{"c.deleted_at IS NULL", "NOT c.is_platform"}
        args := make([]interface{}, 0, 4)
        if search != "" {
                args = append(args, "%"+search+"%")
                where = append(where, fmt.Sprintf("(c.name ILIKE $%d OR c.name_ar ILIKE $%d OR c.email ILIKE $%d)", len(args), len(args), len(args)))
        }
        if status != "" {
                args = append(args, status)
                where = append(where, fmt.Sprintf("c.status = $%d", len(args)))
        }
        whereSQL := strings.Join(where, " AND ")

        var total int
        if err := h.db.QueryRow(c.Request.Context(),
                "SELECT COUNT(*) FROM companies c WHERE "+whereSQL, args...).Scan(&total); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "platform_companies_query_failed", "message": "Could not count companies"})
                return
        }
        var activeCount, trialCount, suspendedCount int
        if err := h.db.QueryRow(c.Request.Context(), fmt.Sprintf(`
                SELECT
                        COUNT(*) FILTER (WHERE c.status = 'active')::int,
                        COUNT(*) FILTER (WHERE c.status = 'trial')::int,
                        COUNT(*) FILTER (WHERE c.status = 'suspended')::int
                FROM companies c WHERE %s
        `, whereSQL), args...).Scan(&activeCount, &trialCount, &suspendedCount); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "platform_companies_query_failed", "message": "Could not summarize companies"})
                return
        }

        dataArgs := append(append([]interface{}{}, args...), pageSize, (page-1)*pageSize)
        rows, err := h.db.Query(c.Request.Context(), fmt.Sprintf(`
                SELECT c.id::text, COALESCE(c.name_ar, c.name), c.name, c.email,
                       COALESCE(c.phone, ''), c.status::text, c.plan::text,
                       c.max_users_per_account, c.created_at,
                       COUNT(DISTINCT a.id) FILTER (WHERE a.deleted_at IS NULL),
                       COUNT(DISTINCT cu.id) FILTER (WHERE cu.deleted_at IS NULL AND cu.is_active = true)
                FROM companies c
                LEFT JOIN accounts a ON a.company_id = c.id
                LEFT JOIN company_users cu ON cu.company_id = c.id
                WHERE %s
                GROUP BY c.id
                ORDER BY c.created_at DESC
                LIMIT $%d OFFSET $%d
        `, whereSQL, len(args)+1, len(args)+2), dataArgs...)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "platform_companies_query_failed", "message": "Could not load companies"})
                return
        }
        defer rows.Close()

        companies := make([]gin.H, 0)
        for rows.Next() {
                var id, name, nameEN, email, phone, statusValue, plan string
                var maxUsers, totalAccounts, totalUsers int
                var createdAt time.Time
                if err := rows.Scan(&id, &name, &nameEN, &email, &phone, &statusValue, &plan,
                        &maxUsers, &createdAt, &totalAccounts, &totalUsers); err != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "platform_companies_query_failed", "message": "Could not read companies"})
                        return
                }
                companies = append(companies, gin.H{
                        "id": id, "name": name, "name_en": nameEN, "email": email, "phone": phone,
                        "status": statusValue, "plan": plan, "max_users_per_account": maxUsers,
                        "total_accounts": totalAccounts, "total_users": totalUsers,
                        "created_at": createdAt.UTC().Format(time.RFC3339),
                })
        }
        if err := rows.Err(); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "platform_companies_query_failed", "message": "Could not read companies"})
                return
        }

        c.JSON(http.StatusOK, gin.H{
                "data": companies,
                "summary": gin.H{
                        "total": total, "active": activeCount, "trial": trialCount, "suspended": suspendedCount,
                },
                "pagination": gin.H{
                        "total": total, "page": page, "page_size": pageSize,
                        "total_pages": (total + pageSize - 1) / pageSize,
                },
        })
}

// ListPlatformUsers returns company users and pharmacy employees in one
// platform-wide view, preserving which authentication domain owns each user.
func (h *Handler) ListPlatformUsers(c *gin.Context) {
        page, pageSize := pagination(c)
        search := strings.TrimSpace(c.Query("search"))
        role := strings.TrimSpace(c.Query("role"))

        where := []string{"1 = 1"}
        args := make([]interface{}, 0, 3)
        if search != "" {
                args = append(args, "%"+search+"%")
                where = append(where, fmt.Sprintf("(u.email ILIKE $%d OR u.display_name ILIKE $%d OR u.company_name ILIKE $%d)", len(args), len(args), len(args)))
        }
        if role != "" {
                args = append(args, role)
                where = append(where, fmt.Sprintf("u.role = $%d", len(args)))
        }
        whereSQL := strings.Join(where, " AND ")

        var total int
        countQuery := fmt.Sprintf(`
                SELECT COUNT(*) FROM (
                        SELECT cu.id, cu.email, COALESCE(cu.display_name, cu.first_name || ' ' || cu.last_name) AS display_name,
                               c.name AS company_name, cu.role::text AS role
                        FROM company_users cu JOIN companies c ON c.id = cu.company_id
                        WHERE cu.deleted_at IS NULL AND c.deleted_at IS NULL AND NOT c.is_platform
                        UNION ALL
                        SELECT e.id, e.email, COALESCE(e.display_name, e.first_name || ' ' || e.last_name),
                               COALESCE(c.name, a.company_name, ''), 'employee'
                        FROM employees e
                        LEFT JOIN accounts a ON a.id = e.account_id
                        LEFT JOIN companies c ON c.id = a.company_id
                ) u WHERE %s
        `, whereSQL)
        if err := h.db.QueryRow(c.Request.Context(), countQuery, args...).Scan(&total); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "platform_users_query_failed", "message": "Could not count users"})
                return
        }

        dataArgs := append(append([]interface{}{}, args...), pageSize, (page-1)*pageSize)
        rows, err := h.db.Query(c.Request.Context(), fmt.Sprintf(`
                WITH all_users AS (
                        SELECT cu.id::text, 'company_user' AS account_type, cu.email,
                               COALESCE(cu.display_name, cu.first_name || ' ' || cu.last_name) AS display_name,
                               c.name AS company_name, cu.role::text AS role, cu.is_active,
                               (cu.email_verified_at IS NOT NULL) AS email_verified,
                               cu.last_login_at, cu.created_at,
                               COUNT(cup.id) FILTER (WHERE cup.is_active = true)::int AS permissions_count
                        FROM company_users cu
                        JOIN companies c ON c.id = cu.company_id
                        LEFT JOIN company_user_permissions cup ON cup.company_user_id = cu.id
                        WHERE cu.deleted_at IS NULL AND c.deleted_at IS NULL AND NOT c.is_platform
                        GROUP BY cu.id, c.name
                        UNION ALL
                        SELECT e.id::text, 'employee', e.email,
                               COALESCE(e.display_name, e.first_name || ' ' || e.last_name),
                               COALESCE(c.name, a.company_name, ''), 'employee', e.is_active,
                               (e.email_verified_at IS NOT NULL) AS email_verified,
                               NULL::timestamptz, e.created_at,
                               COUNT(ep.id) FILTER (WHERE ep.is_active = true)::int
                        FROM employees e
                        LEFT JOIN accounts a ON a.id = e.account_id
                        LEFT JOIN companies c ON c.id = a.company_id
                        LEFT JOIN employee_permissions ep ON ep.employee_id = e.id
                        GROUP BY e.id, c.name, a.company_name
                )
                SELECT id, account_type, email, display_name, company_name, role,
                       is_active, email_verified, last_login_at, created_at, permissions_count
                FROM all_users u
                WHERE %s
                ORDER BY created_at DESC
                LIMIT $%d OFFSET $%d
        `, whereSQL, len(args)+1, len(args)+2), dataArgs...)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "platform_users_query_failed", "message": "Could not load users"})
                return
        }
        defer rows.Close()

        users := make([]gin.H, 0)
        for rows.Next() {
                var id, accountType, email, displayName, companyName, roleValue string
                var isActive, emailVerified bool
                var lastLogin, createdAt *time.Time
                var permissionsCount int
                if err := rows.Scan(&id, &accountType, &email, &displayName, &companyName, &roleValue,
                        &isActive, &emailVerified, &lastLogin, &createdAt, &permissionsCount); err != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "platform_users_query_failed", "message": "Could not read users"})
                        return
                }
                user := gin.H{
                        "id": id, "account_type": accountType, "email": email, "display_name": displayName,
                        "company_name": companyName, "role": roleValue, "is_active": isActive,
                        "email_verified": emailVerified,
                        "permissions_count": permissionsCount,
                }
                if lastLogin != nil {
                        user["last_login_at"] = lastLogin.UTC().Format(time.RFC3339)
                }
                if createdAt != nil {
                        user["created_at"] = createdAt.UTC().Format(time.RFC3339)
                }
                users = append(users, user)
        }
        if err := rows.Err(); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "platform_users_query_failed", "message": "Could not read users"})
                return
        }

        c.JSON(http.StatusOK, gin.H{
                "data": users,
                "pagination": gin.H{
                        "total": total, "page": page, "page_size": pageSize,
                        "total_pages": (total + pageSize - 1) / pageSize,
                },
        })
}

// VerifyPlatformUserEmail — POST /platform-admin/users/verify-email — Task 132.
// Manual email verification as the operator's same-minute unblock for the
// failure mode the live probe proved (2026-10-31): Brevo accepts every send
// (message_id logged) while NO message reaches any external inbox —
// shared-IP reputation / sender-domain state is invisible to the application
// and the user stays locked out of a freshly registered account. The
// transactional pipeline stays the default path; this endpoint exists so the
// super-admin never has to open a SQL console to flip email_verified_at.
//
// Both authentication domains are covered in one call (company_users and
// employees — the same pair ListPlatformUsers unions), every update runs
// inside one transaction with an audited write, and the response distinguishes
// "verified now" from "was already verified" so the UI can say something true.
func (h *Handler) VerifyPlatformUserEmail(c *gin.Context) {
        var request struct {
                Email string `json:"email" binding:"required,email"`
        }
        if err := c.ShouldBindJSON(&request); err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "validation_error", "message": "A valid email is required"})
                return
        }
        email := strings.ToLower(strings.TrimSpace(request.Email))
        actor, ok := auth.PrincipalFromContext(c)
        if !ok || actor.ID == "" {
                c.JSON(http.StatusForbidden, gin.H{"error": "super_admin_required"})
                return
        }

        ctx := c.Request.Context()
        tx, err := h.db.Begin(ctx)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "verify_email_failed", "message": "Could not start the transaction"})
                return
        }
        defer func() { _ = tx.Rollback(ctx) }()

        // Snapshot first: how many live accounts carry this address, and how
        // many were already verified — the response must not confuse "nothing
        // matched" with "nothing left to change".
        var cuTotal, cuVerified, empTotal, empVerified int
        if err := tx.QueryRow(ctx, `
                SELECT COUNT(*)::int, COUNT(email_verified_at)::int
                FROM company_users
                WHERE LOWER(email) = $1 AND deleted_at IS NULL
        `, email).Scan(&cuTotal, &cuVerified); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "verify_email_failed", "message": "Could not read company users"})
                return
        }
        if err := tx.QueryRow(ctx, `
                SELECT COUNT(*)::int, COUNT(email_verified_at)::int
                FROM employees
                WHERE LOWER(email) = $1
        `, email).Scan(&empTotal, &empVerified); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "verify_email_failed", "message": "Could not read employees"})
                return
        }
        total := cuTotal + empTotal
        if total == 0 {
                c.JSON(http.StatusNotFound, gin.H{"error": "user_not_found", "message": "No account carries this email"})
                return
        }
        alreadyVerified := cuVerified + empVerified

        // Same shape as the auth service's MarkEmailVerified: company users
        // also drop their pending verification token; employees only stamp the
        // timestamp (no token column on that table).
        var companyUsersUpdated, employeesUpdated int64
        if cuTotal > cuVerified {
                tag, err := tx.Exec(ctx, `
                        UPDATE company_users
                        SET email_verified_at = NOW(), email_verification_token = NULL
                        WHERE LOWER(email) = $1 AND deleted_at IS NULL AND email_verified_at IS NULL
                `, email)
                if err != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "verify_email_failed", "message": "Could not verify company users"})
                        return
                }
                companyUsersUpdated = tag.RowsAffected()
        }
        if empTotal > empVerified {
                tag, err := tx.Exec(ctx, `
                        UPDATE employees
                        SET email_verified_at = NOW()
                        WHERE LOWER(email) = $1 AND email_verified_at IS NULL
                `, email)
                if err != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "verify_email_failed", "message": "Could not verify employees"})
                        return
                }
                employeesUpdated = tag.RowsAffected()
        }
        updated := companyUsersUpdated + employeesUpdated

        if err := writePlatformAuditLog(ctx, tx, actor, "user.verify_email", "user_management", "user", email, "",
                map[string]any{
                        "email":                 email,
                        "company_users_updated": companyUsersUpdated,
                        "employees_updated":     employeesUpdated,
                        "already_verified":      alreadyVerified,
                },
                fmt.Sprintf("توثيق يدوي لبريد %s (حسابات محدثة: %d)", email, updated)); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "verify_email_failed", "message": "Could not write the audit entry"})
                return
        }
        if err := tx.Commit(ctx); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "verify_email_failed", "message": "Could not commit the verification"})
                return
        }
        c.JSON(http.StatusOK, gin.H{
                "data": gin.H{
                        "email":            email,
                        "verified":         updated,
                        "already_verified": updated == 0 && alreadyVerified > 0,
                },
        })
}

// ListPlatformAccounts returns every pharmacy account with its company and
// branch counts. No client-supplied company id is used for scoping.
func (h *Handler) ListPlatformAccounts(c *gin.Context) {
        page, pageSize := pagination(c)
        search := strings.TrimSpace(c.Query("search"))
        args := make([]interface{}, 0, 2)
        // Task 164: the platform shell company's bootstrap account is
        // infrastructure — never a row in the customer account list (the
        // count query shares this predicate).
        where := []string{"a.deleted_at IS NULL",
                "NOT EXISTS (SELECT 1 FROM companies pc WHERE pc.id = a.company_id AND pc.is_platform)"}
        if search != "" {
                args = append(args, "%"+search+"%")
                where = append(where, fmt.Sprintf("(a.company_name ILIKE $%d OR a.contact_email ILIKE $%d OR COALESCE(c.name, '') ILIKE $%d)", len(args), len(args), len(args)))
        }
        whereSQL := strings.Join(where, " AND ")

        var total int
        if err := h.db.QueryRow(c.Request.Context(), "SELECT COUNT(*) FROM accounts a LEFT JOIN companies c ON c.id = a.company_id WHERE "+whereSQL, args...).Scan(&total); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "platform_accounts_query_failed", "message": "Could not count accounts"})
                return
        }

        dataArgs := append(append([]interface{}{}, args...), pageSize, (page-1)*pageSize)
        rows, err := h.db.Query(c.Request.Context(), fmt.Sprintf(`
                SELECT a.id::text, COALESCE(c.id::text, ''), COALESCE(c.name_ar, c.name, a.company_name),
                       a.company_name, a.contact_email, COALESCE(a.contact_phone, ''),
                       a.status::text, a.subscription_plan, a.created_at,
                       COUNT(DISTINCT p.id) FILTER (WHERE p.is_active = true)::int,
                       COUNT(DISTINCT b.id) FILTER (WHERE b.is_active = true)::int
                FROM accounts a
                LEFT JOIN companies c ON c.id = a.company_id
                LEFT JOIN pharmacies p ON p.account_id = a.id
                LEFT JOIN branches b ON b.pharmacy_id = p.id
                WHERE %s
                GROUP BY a.id, c.id
                ORDER BY a.created_at DESC
                LIMIT $%d OFFSET $%d
        `, whereSQL, len(args)+1, len(args)+2), dataArgs...)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "platform_accounts_query_failed", "message": "Could not load accounts"})
                return
        }
        defer rows.Close()

        accounts := make([]gin.H, 0)
        for rows.Next() {
                var id, companyID, companyName, accountName, email, phone, statusValue, plan string
                var pharmacyCount, branchCount int
                var createdAt time.Time
                if err := rows.Scan(&id, &companyID, &companyName, &accountName, &email, &phone,
                        &statusValue, &plan, &createdAt, &pharmacyCount, &branchCount); err != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "platform_accounts_query_failed", "message": "Could not read accounts"})
                        return
                }
                accounts = append(accounts, gin.H{
                        "id": id, "company_id": companyID, "company_name": companyName,
                        "name": accountName, "email": email, "phone": phone, "status": statusValue,
                        "plan": plan, "pharmacy_count": pharmacyCount, "branch_count": branchCount,
                        "created_at": createdAt.UTC().Format(time.RFC3339),
                })
        }
        if err := rows.Err(); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "platform_accounts_query_failed", "message": "Could not read accounts"})
                return
        }

        c.JSON(http.StatusOK, gin.H{
                "data": accounts,
                "pagination": gin.H{
                        "total": total, "page": page, "page_size": pageSize,
                        "total_pages": (total + pageSize - 1) / pageSize,
                },
        })
}

func (h *Handler) ListPlatformPermissions(c *gin.Context) {
        permissionRows, err := h.db.Query(c.Request.Context(), `
                SELECT key, name, description, module, category, is_system, sort_order
                FROM permissions
                ORDER BY sort_order, key
        `)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "platform_permissions_query_failed", "message": "Could not load permissions"})
                return
        }
        defer permissionRows.Close()

        permissions := make([]gin.H, 0)
        for permissionRows.Next() {
                var key, name, description, module, category string
                var isSystem bool
                var sortOrder int
                if err := permissionRows.Scan(&key, &name, &description, &module, &category, &isSystem, &sortOrder); err != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "platform_permissions_query_failed", "message": "Could not read permissions"})
                        return
                }
                permissions = append(permissions, gin.H{
                        "key": key, "name": name, "description": description, "module": module,
                        "category": category, "is_system": isSystem, "sort_order": sortOrder,
                })
        }

        roleRows, err := h.db.Query(c.Request.Context(), `
                WITH role_catalog(role, name, description, is_system) AS (
                        VALUES
                                ('super_admin', 'مدير النظام', 'صلاحيات كاملة على منصة Pharmacy OS', true),
                                ('company_admin', 'مدير الشركة', 'إدارة الشركة التابعة ومستخدميها', true),
                                ('company_manager', 'مدير العمليات', 'إدارة العمليات اليومية للشركة', true),
                                ('company_viewer', 'مشاهد الشركة', 'عرض بيانات الشركة دون تعديل', true)
                )
                SELECT rc.role, rc.name, rc.description, rc.is_system,
                       COUNT(DISTINCT cu.id)::int,
                       CASE WHEN rc.role = 'super_admin' THEN
                         COALESCE((SELECT ARRAY_AGG(p.key ORDER BY p.sort_order, p.key) FROM permissions p), ARRAY[]::varchar[])
                       ELSE
                         COALESCE(ARRAY_AGG(DISTINCT p2.key ORDER BY p2.key) FILTER (WHERE p2.key IS NOT NULL), ARRAY[]::varchar[])
                       END
                FROM role_catalog rc
                LEFT JOIN company_users cu ON cu.role::text = rc.role AND cu.deleted_at IS NULL
                LEFT JOIN company_user_permissions cup ON cup.company_user_id = cu.id AND cup.is_active = true
                LEFT JOIN permissions p2 ON p2.id = cup.permission_id
                GROUP BY rc.role, rc.name, rc.description, rc.is_system
                ORDER BY rc.role
        `)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "platform_roles_query_failed", "message": "Could not load roles"})
                return
        }
        defer roleRows.Close()

        roles := make([]gin.H, 0)
        for roleRows.Next() {
                var role, name, description string
                var isSystem bool
                var userCount int
                var permissionKeys []string
                if err := roleRows.Scan(&role, &name, &description, &isSystem, &userCount, &permissionKeys); err != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "platform_roles_query_failed", "message": "Could not read roles"})
                        return
                }
                roles = append(roles, gin.H{
                        "id": role, "name": name, "description": description, "is_system": isSystem,
                        "user_count": userCount, "permission_keys": permissionKeys,
                })
        }

        c.JSON(http.StatusOK, gin.H{"permissions": permissions, "roles": roles})
}

func pagination(c *gin.Context) (int, int) {
        page, pageSize := 1, 20
        if value, err := strconv.Atoi(c.Query("page")); err == nil && value > 0 {
                page = value
        }
        if value, err := strconv.Atoi(c.Query("page_size")); err == nil && value > 0 && value <= 100 {
                pageSize = value
        }
        return page, pageSize
}
