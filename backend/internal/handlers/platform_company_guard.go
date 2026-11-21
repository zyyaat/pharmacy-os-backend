// Platform-company isolation (Task 164, migration 45).
//
// The company hosting the platform Super Admin (auth/bootstrap.go) is
// infrastructure, not a SaaS customer: it must never appear among customer
// companies/accounts/users/subscriptions/stats, and no billing or admin
// mutation may suspend, re-plan, soft-delete or hard-delete it — suspending
// it would flip the operator's own tenant (the suspend action copies the
// subscription status onto companies.status) and desync the Task-90
// bootstrap assumptions.
//
// Two layers enforce the invariant:
//   - migration 45 backstops it at the DATABASE level with a BEFORE
//     UPDATE/DELETE trigger (protect_platform_company) refusing status
//     changes, soft delete, unmarking and hard delete — even for raw SQL;
//   - the guards here give the HTTP surface clean 403s before any statement
//     runs, and the list handlers exclude is_platform companies so the shell
//     never renders as a customer row.
package handlers

import (
        "context"
        "net/http"

        "github.com/gin-gonic/gin"
        "github.com/jackc/pgx/v5"
        "github.com/jackc/pgx/v5/pgxpool"

        "github.com/pharmacy-os/backend/internal/auth"
)

// queryRower is the subset of *pgxpool.Pool (and pgx.Tx, for in-transaction
// callers) the platform-company guard needs.
type queryRower interface {
        QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// platformCompanyGuard reports whether companyID hosts the platform Super
// Admin (companies.is_platform). When true the request is answered with 403
// platform_company_immutable and the caller must return. Unknown companies
// return false so the caller's own lookup keeps producing its natural 404.
func platformCompanyGuard(c *gin.Context, q queryRower, companyID string) bool {
        var isPlatform bool
        err := q.QueryRow(c.Request.Context(),
                `SELECT is_platform FROM companies WHERE id = $1::uuid`, companyID,
        ).Scan(&isPlatform)
        if err == pgx.ErrNoRows {
                return false
        }
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{
                        "error":   "platform_company_guard_failed",
                        "message": "Could not verify the company guard",
                })
                return true
        }
        if !isPlatform {
                return false
        }
        c.JSON(http.StatusForbidden, gin.H{
                "error":   "platform_company_immutable",
                "message": "شركة المنصة (حساب مدير النظام) بنية تحتية وليست عميلًا — لا يمكن تعديل اشتراكها أو حالتها",
        })
        return true
}

// rejectPlatformCompanyMutation is the guard for *Handler routes.
func (h *Handler) rejectPlatformCompanyMutation(c *gin.Context, companyID string) bool {
        return platformCompanyGuard(c, h.db, companyID)
}

// rejectPlatformCompanyMutationViaContext is the guard for handlers holding
// no *Handler (CompanyHandler): the company realm mounts the shared pool via
// appmiddleware.CompanyDBPoolContext under "company_db_pool". If the pool is
// not mounted the guard stays silent — the migration-45 trigger backstops
// the write at the database level, so fail-open here cannot lift the freeze.
func rejectPlatformCompanyMutationViaContext(c *gin.Context, companyID string) bool {
        pool, _ := c.Get("company_db_pool")
        pgPool, ok := pool.(*pgxpool.Pool)
        if !ok || pgPool == nil {
                return false
        }
        return platformCompanyGuard(c, pgPool, companyID)
}

// requesterIsPlatformCompany — حارس الأدوات التطويرية (Task 165): يقرر هل
// مقدم الطلب عضو في شركة المنصة (companies.is_platform). يكتب ال403 بنفسه
// ويعيد false عند الرفض. لا شركة على الإطلاق غير شركة المنصة تمر — الغير
// حامل CompanyID يُرفض أيضًا: الأداة بنية تحتية داخلية لا تُخدَم لغير
// أعضاء الشركة المضيفة للمدير الأعلى.
func (h *Handler) requesterIsPlatformCompany(c *gin.Context) bool {
        principal, ok := auth.PrincipalFromContext(c)
        if !ok || principal.ID == "" {
                c.JSON(http.StatusUnauthorized, gin.H{
                        "error": "authentication_required", "message": "Authentication required",
                })
                return false
        }
        if principal.CompanyID == "" {
                c.JSON(http.StatusForbidden, gin.H{
                        "error":   "product_dev_tool",
                        "message": "أدوات تطوير المنتج ليست جزءًا من واجهة المستأجرين",
                })
                return false
        }
        var isPlatform bool
        if err := h.db.QueryRow(c.Request.Context(),
                `SELECT is_platform FROM companies WHERE id = $1::uuid`, principal.CompanyID,
        ).Scan(&isPlatform); err != nil {
                // شركة غير موجودة = عميل قديم/غير صالح — الرفض هو الرد الطبيعي،
                // أما خطأ القاعدة الحقيقي فيُرد 500 لا يكشف شيئًا.
                if err == pgx.ErrNoRows {
                        c.JSON(http.StatusForbidden, gin.H{
                                "error":   "product_dev_tool",
                                "message": "أدوات تطوير المنتج ليست جزءًا من واجهة المستأجرين",
                        })
                        return false
                }
                c.JSON(http.StatusInternalServerError, gin.H{
                        "error":   "platform_company_guard_failed",
                        "message": "Could not verify the company guard",
                })
                return false
        }
        if !isPlatform {
                c.JSON(http.StatusForbidden, gin.H{
                        "error":   "product_dev_tool",
                        "message": "أدوات تطوير المنتج ليست جزءًا من واجهة المستأجرين",
                })
                return false
        }
        return true
}
