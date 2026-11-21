package handlers

import (
        "log"
        "net/http"
        "time"

        "github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// Database migration ledger (سجل ترحيل قاعدة البيانات).
// The migrator applies the versioned chain automatically on every backend boot
// (advisory-locked, one transaction per migration, recorded in
// public.schema_migrations). The ledger is the observability half of the
// industry-standard migration workflow (Flyway-style info command).
//
// Task 165 — الملكية الصحيحة للأداة: schema_migrations دفتر البنية التحتية
// للمنصة كلها (قاعدة واحدة مشتركة) — عناوين الترحيلات تكشف خارطة طريق
// المنتج (passkeys, payments, support system...) وطوابع applied_at تكشف
// إيقاع النشر. لم يعد يُخدَم لأي شركة مستأجرة:
//   - GET /pharmacy/system/migrations يبقى موجودًا حيال عقد Flutter المجمّد
//     (mobile/lib/core/api_client.dart systemMigrations()) لكنه الآن
//     لأعضاء شركة المنصة وحدهم — أي صيدلية مستأجرة تُرد 403 product_dev_tool؛
//   - GET /platform-admin/system/migrations هو البيت الطبيعي للأداة على
//     سطح تطوير المنتج (لوحة المنصة) — بوابة الMiddleware لعالم المنصة
//     على المجموعة كاملة هي الحارس.
// ---------------------------------------------------------------------------

// writeMigrationsLedger يقرأ الدفتر ويكتب الاستجابة الموحدة {data:{items,total}}
// — نفس شكل السلك الذي تعتمده الواجهات منذ الولادة.
func (h *Handler) writeMigrationsLedger(c *gin.Context) {
        rows, err := h.db.Query(c.Request.Context(), `
                SELECT version::text, applied_at
                FROM public.schema_migrations
                ORDER BY version ASC
        `)
        if err != nil {
                log.Printf("[MIGRATIONS] ledger query failed: %v", err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "migrations_query_failed", "message": "تعذر قراءة سجل الترحيلات"})
                return
        }
        defer rows.Close()

        items := make([]gin.H, 0)
        for rows.Next() {
                var version string
                var appliedAt time.Time
                if err := rows.Scan(&version, &appliedAt); err != nil {
                        log.Printf("[MIGRATIONS] ledger scan failed: %v", err)
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "migrations_query_failed", "message": "تعذر قراءة سجل الترحيلات"})
                        return
                }
                items = append(items, gin.H{"version": version, "applied_at": appliedAt})
        }
        if err := rows.Err(); err != nil {
                log.Printf("[MIGRATIONS] ledger rows failed: %v", err)
                c.JSON(http.StatusInternalServerError, gin.H{"error": "migrations_query_failed", "message": "تعذر قراءة سجل الترحيلات"})
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": gin.H{"items": items, "total": len(items)}})
}

// GetSystemMigrations → GET /pharmacy/system/migrations — مسار محفوظ حيال
// عقد Flutter المجمّد، مقفول لأعضاء شركة المنصة وحدهم (Task 165).
func (h *Handler) GetSystemMigrations(c *gin.Context) {
        if !h.requesterIsPlatformCompany(c) {
                return
        }
        h.writeMigrationsLedger(c)
}

// GetPlatformSystemMigrations → GET /platform-admin/system/migrations —
// بيت الأداة على سطح تطوير المنتج؛ حارس المجموعة (Middleware لعالم
// المنصة) هو التصريح الوحيد المطلوب.
func (h *Handler) GetPlatformSystemMigrations(c *gin.Context) {
        h.writeMigrationsLedger(c)
}
