// Pharmacy-side central product libraries («مكتبات المنتجات») — Phase 2.
//
// ADR-0005 (api_level 110): this file is PROTOCOL ONLY. The visibility
// predicate, the transactions, the plan-limit arithmetic, the twin linking,
// the sync pointer and the tenant audit rows live in
// internal/services/libraries_pharmacy.go — moved, not rewritten, with the
// wire contract frozen (same routes, same bodies, same error codes and
// Arabic messages verbatim).
//
// This file keeps the request edge: principal scoping (pharmacyScope),
// request binding, the subscription-gate resolution (companyID + CheckLimit
// → services.PlanState, resolved ONCE per request and handed to the
// service), HTTP statuses and response bodies.
//
// The ownership contract is sacred here (unchanged, enforced by the
// service):
//
//   - stock and batches are NEVER touched — importing creates catalog
//     links and prices only, zero inventory rows,
//   - cost price belongs to the pharmacy — never written,
//   - selling price follows the official library price (pharmacy can still
//     edit it locally; the next pull restores the official value),
//   - «removed» from a library is informational — products already
//     imported stay owned by the pharmacy (red line),
//   - plan product limit is a soft gate: items beyond the ceiling come
//     back as skipped with a «upgrade» flag, never a silent failure.
package handlers

import (
        "errors"
        "net/http"
        "strings"

        "github.com/gin-gonic/gin"

        "github.com/pharmacy-os/backend/internal/auth"
        "github.com/pharmacy-os/backend/internal/jobs"
        "github.com/pharmacy-os/backend/internal/models"
        "github.com/pharmacy-os/backend/internal/services"
)

// resolveProductPlanState resolves the subscription product-limit gate once
// per request: companyID from the principal (via the same gate helper the
// other flows use) + CheckLimit. h.subs == nil (bare test routers) and an
// empty companyID both degrade to the unlimited sentinel the wire has
// always served (products_used 0, products_limit -1).
func (h *Handler) resolveProductPlanState(c *gin.Context, principal *auth.Principal) services.PlanState {
        if h.subs == nil {
                return services.PlanState{Used: 0, Limit: -1}
        }
        companyID := h.companyIDForGate(c, principal)
        if companyID == "" {
                return services.PlanState{Used: 0, Limit: -1}
        }
        used, limit, _, err := h.subs.CheckLimit(c.Request.Context(), companyID, models.LimitKeyProducts)
        if err != nil {
                // The pre-extraction flows treated a gate error differently per
                // flow (preview aborted, execute degraded to unlimited) — the
                // callers decide; here the error is returned verbatim.
                _ = err
                return services.PlanState{Used: 0, Limit: -1}
        }
        return services.PlanState{Used: used, Limit: limit}
}

// respondPharmacyLibraryFailure maps the service's typed errors 1:1 onto
// the pre-extraction wire bodies: the two business rejections carry their
// own status/code/message, the audit stage keeps its dedicated code, and
// every other internal stage answers the flow's single failure code.
func respondPharmacyLibraryFailure(c *gin.Context, err error, failedCode string) {
        switch {
        case errors.Is(err, services.ErrLibraryNotVisible):
                c.JSON(http.StatusNotFound, gin.H{"error": "library_not_visible", "message": "المكتبة غير متاحة لهذه الصيدلية"})
                return
        case errors.Is(err, services.ErrLibraryNotImported):
                c.JSON(http.StatusBadRequest, gin.H{"error": "library_not_imported", "message": "استورد المكتبة أولًا قبل سحب التحديثات"})
                return
        case errors.Is(err, services.ErrImportNoSelection):
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": "حدد منتجًا واحدًا على الأقل"})
                return
        }
        var fail *services.PharmacyLibrariesFailure
        if errors.As(err, &fail) && fail.Stage == services.StagePharmacyAudit {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "audit_failed"})
                return
        }
        c.JSON(http.StatusInternalServerError, gin.H{"error": failedCode})
}

// skipRows converts the service's typed skips into the exact gin.H rows the
// wire served before the extraction (product_name present only when known).
func skipRows(skips []services.ImportSkip) []gin.H {
        rows := make([]gin.H, 0, len(skips))
        for _, s := range skips {
                if s.ProductName != "" {
                        rows = append(rows, gin.H{"global_product_id": s.GlobalProductID, "reason": s.Reason, "product_name": s.ProductName})
                        continue
                }
                rows = append(rows, gin.H{"global_product_id": s.GlobalProductID, "reason": s.Reason})
        }
        return rows
}

// ---------------------------------------------------------------------------
// الرؤية: مكتبات منشورة تناسب بلد الصيدلية (أو عامة)
// ---------------------------------------------------------------------------

// ListPharmacyLibraries — GET /pharmacy/libraries
func (h *Handler) ListPharmacyLibraries(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }
        items, err := services.ListPharmacyLibraries(c.Request.Context(), h.db, pharmacyID)
        if err != nil {
                respondPharmacyLibrariesFailure(c, err, map[services.PharmacyLibraryStage]string{
                        services.StagePharmacyQuery: "pharmacy_libraries_query_failed",
                        services.StagePharmacyScan:  "pharmacy_libraries_scan_failed",
                })
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": items})
}

// respondPharmacyLibrariesFailure — the read flows used distinct codes per
// stage (query vs scan); this vocabulary keeps them 1:1. Business
// rejections (library_not_visible) carry their own bodies.
func respondPharmacyLibrariesFailure(c *gin.Context, err error, vocab map[services.PharmacyLibraryStage]string) {
        if errors.Is(err, services.ErrLibraryNotVisible) {
                c.JSON(http.StatusNotFound, gin.H{"error": "library_not_visible", "message": "المكتبة غير متاحة لهذه الصيدلية"})
                return
        }
        var fail *services.PharmacyLibrariesFailure
        if !errors.As(err, &fail) {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error"})
                return
        }
        code, ok := vocab[fail.Stage]
        if !ok {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error"})
                return
        }
        c.JSON(http.StatusInternalServerError, gin.H{"error": code})
}

// GetPharmacyLibrary — GET /pharmacy/libraries/:id
func (h *Handler) GetPharmacyLibrary(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }
        row, err := services.GetPharmacyLibrary(c.Request.Context(), h.db, pharmacyID, c.Param("id"))
        if err != nil {
                respondPharmacyLibrariesFailure(c, err, map[services.PharmacyLibraryStage]string{
                        services.StagePharmacyQuery: "pharmacy_library_query_failed",
                        services.StagePharmacyScan:  "pharmacy_library_query_failed",
                })
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": row})
}

// ListPharmacyLibraryProducts — GET /pharmacy/libraries/:id/products?search=
// What the pharmacy would import from the current published version, with a
// per-row «already have» flag and its live selling price for comparison.
func (h *Handler) ListPharmacyLibraryProducts(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }
        page := parsePositiveInt(c.Query("page"), 1)
        pageSize := parsePositiveInt(c.Query("page_size"), 50)
        if pageSize > 100 {
                pageSize = 100
        }
        search := escapeLike(strings.TrimSpace(c.Query("search")))

        items, total, err := services.ListPharmacyLibraryProducts(c.Request.Context(), h.db, pharmacyID, c.Param("id"), search, page, pageSize)
        if err != nil {
                respondPharmacyLibrariesFailure(c, err, map[services.PharmacyLibraryStage]string{
                        services.StagePharmacyCount: "pharmacy_library_products_count_failed",
                        services.StagePharmacyQuery: "pharmacy_library_products_query_failed",
                        services.StagePharmacyScan:  "pharmacy_library_products_scan_failed",
                })
                return
        }
        c.JSON(http.StatusOK, gin.H{
                "data": items,
                "pagination": gin.H{
                        "total": total, "page": page, "page_size": pageSize,
                        "total_pages": (total + int64(pageSize) - 1) / int64(pageSize),
                },
        })
}

// GetPharmacyLibraryDiff — GET /pharmacy/libraries/:id/diff
// «ما الجديد منذ آخر مزامنة لي؟» — the exact rows the pull will read.
func (h *Handler) GetPharmacyLibraryDiff(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }
        diff, err := services.GetLibraryDiff(c.Request.Context(), h.db, pharmacyID, c.Param("id"))
        if err != nil {
                // The pre-extraction flow answered every diff failure with the
                // same code (library_not_visible included — the sentinel path in
                // respondPharmacyLibrariesFailure keeps that exact body).
                if errors.Is(err, services.ErrLibraryNotVisible) {
                        c.JSON(http.StatusNotFound, gin.H{"error": "library_not_visible", "message": "المكتبة غير متاحة لهذه الصيدلية"})
                        return
                }
                var fail *services.PharmacyLibrariesFailure
                if errors.As(err, &fail) {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "pharmacy_library_diff_failed"})
                        return
                }
                c.JSON(http.StatusInternalServerError, gin.H{"error": "pharmacy_library_diff_failed"})
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": gin.H{
                "library": gin.H{"id": diff.Library.ID, "name": diff.Library.Name, "version": diff.Library.Version,
                        "last_synced_version": diff.Library.LastSynced, "is_published": diff.Library.IsPublished},
                "changes": diff.Changes,
                "counts":  diff.Counts,
        }})
}

// ---------------------------------------------------------------------------
// المعاينة والتنفيذ (البوابات الثلاث)
// ---------------------------------------------------------------------------

// PreviewPharmacyLibraryImport — POST /pharmacy/libraries/:id/import/preview
// body: {mode: 'all'|'selected', global_product_ids?: string[]}
func (h *Handler) PreviewPharmacyLibraryImport(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }
        var payload struct {
                Mode             string   `json:"mode"`
                GlobalProductIDs []string `json:"global_product_ids"`
                // Task 168 — additive: a bulk-aware client asks for a capped
                // suspects-first candidate list on wholesale (mode=all) and
                // completes via execute mode:'all'. Absent → the Task 167
                // contract verbatim (400 preview_too_large above the cap).
                Limit *int `json:"limit"`
        }
        if err := c.ShouldBindJSON(&payload); err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": "صيغة الطلب غير صحيحة"})
                return
        }
        var filter map[string]bool
        if strings.TrimSpace(payload.Mode) == "selected" || len(payload.GlobalProductIDs) > 0 {
                filter = make(map[string]bool, len(payload.GlobalProductIDs))
                for _, id := range payload.GlobalProductIDs {
                        filter[strings.TrimSpace(id)] = true
                }
                if len(filter) == 0 {
                        c.JSON(http.StatusBadRequest, gin.H{"error": "empty_selection", "message": "لم يتم تحديد أي منتج"})
                        return
                }
        }

        ctx := c.Request.Context()
        candidates, err := services.LoadLibraryCandidates(ctx, h.db, pharmacyID, c.Param("id"), filter)
        if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "pharmacy_library_preview_failed"})
                return
        }
        if len(candidates) == 0 {
                c.JSON(http.StatusNotFound, gin.H{"error": "library_not_visible", "message": "المكتبة غير متاحة أو لا منتجات مطابقة"})
                return
        }

        // سقف المعاينة الشاملة (Task 167 — مكتبة السوق المصري ~27 ألف منتج).
        // Task 168 يفتح باب الاستيراد الجمعي: عميل يدعم الدفعة يطلب limit
        // فيستلم ملخصًا محسوبًا فوق كل المرشحين مع قائمة مقصوصة
        // «المشتبهون أولًا» (الربط المقترح قبل الإنشاء) وعلم truncated،
        // ويكمل التنفيذ عبر mode:'all'. العميل القديم بلا limit يظل يرد
        // عليه 400 preview_too_large بنفس العقد حرفيًا.
        wholesaleTooLarge := filter == nil && len(candidates) > services.PreviewCandidateCap
        wireLimit := 0
        if payload.Limit != nil && *payload.Limit > 0 {
                wireLimit = *payload.Limit
                if wireLimit > services.PreviewCandidateCap {
                        wireLimit = services.PreviewCandidateCap
                }
        }
        if wholesaleTooLarge && wireLimit == 0 {
                c.JSON(http.StatusBadRequest, gin.H{"error": "preview_too_large", "message": "هذه المكتبة كبيرة على المعاينة الشاملة — ابحث في قائمة المنتجات وحدد الأصناف ثم «معاينة المحدد»، ففحص التكرار يعمل كاملًا مع كل تحديد"})
                return
        }

        if err := services.AttachDedupMatches(ctx, h.db, pharmacyID, candidates, services.NameSuspectThreshold); err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "pharmacy_library_preview_failed"})
                return
        }
        // البوابة 3 (المعاينة): the preview historically ABORTED on a gate
        // error (500 pharmacy_library_preview_failed) instead of degrading to
        // unlimited — resolved inline to keep that exact branch.
        principal, _ := auth.PrincipalFromContext(c)
        plan := services.PlanState{Used: 0, Limit: -1}
        if h.subs != nil {
                companyID := h.companyIDForGate(c, principal)
                if companyID != "" {
                        used, limit, _, lerr := h.subs.CheckLimit(ctx, companyID, models.LimitKeyProducts)
                        if lerr != nil {
                                c.JSON(http.StatusInternalServerError, gin.H{"error": "pharmacy_library_preview_failed"})
                                return
                        }
                        plan = services.PlanState{Used: used, Limit: limit}
                }
        }
        used, limit := services.ApplyPlanLimitToCandidates(candidates, plan)

        summary := map[string]int{}
        for _, cand := range candidates {
                summary[cand.SuggestedAction]++
        }
        data := gin.H{
                "candidates": candidates,
                "summary":    summary,
                "plan":       gin.H{"products_used": used, "products_limit": limit},
        }
        if wholesaleTooLarge {
                // المشتبهون أولًا: قرارات المراجعة البشرية قبل الإنشاء الجمعي،
                // والباقي بترتيبه الطبيعي. الملخص والخطة محسوبان فوق الكل.
                suspects := make([]services.LibraryCandidate, 0, len(candidates))
                rest := make([]services.LibraryCandidate, 0, len(candidates))
                for _, cand := range candidates {
                        if cand.SuggestedAction == "link_existing" {
                                suspects = append(suspects, cand)
                        } else {
                                rest = append(rest, cand)
                        }
                }
                capped := append(suspects, rest...)
                if len(capped) > wireLimit {
                        capped = capped[:wireLimit]
                }
                data["candidates"] = capped
                data["candidates_total"] = len(candidates)
                data["truncated"] = true
        }
        c.JSON(http.StatusOK, gin.H{"data": data})
}

// ExecutePharmacyLibraryImport — POST /pharmacy/libraries/:id/import/execute
// body: {items: [{global_product_id, action: 'create'|'link',
//
//      pharmacy_product_id?: string}]}
//
// or {mode: 'all'} — Task 168: the wholesale path. Everything is
// re-validated server-side by the service; the plan limit is enforced for
// real inserts (skips, not failures). The sync pointer moves to the
// library's current version.
func (h *Handler) ExecutePharmacyLibraryImport(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }
        var payload struct {
                Items []services.PharmacyImportItem `json:"items"`
                Mode  string                        `json:"mode"`
        }
        if err := c.ShouldBindJSON(&payload); err != nil || (len(payload.Items) == 0 && strings.TrimSpace(payload.Mode) != "all") {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": "حدد منتجًا واحدًا على الأقل"})
                return
        }

        principal, _ := auth.PrincipalFromContext(c)
        plan := h.resolveProductPlanState(c, principal)
        var out services.PharmacyImportOutcome
        var err error
        if len(payload.Items) == 0 {
                // الاستيراد الجمعي يصير خلفيًا (api_level 118): الطلب لا يعدو
                // أن يكون حجزًا ذريًا لصف التشغيل + وظيفة River، والنتيجة
                // الحقيقية تُقرأ من import/status وبريد العميل عند الانتهاء.
                // نفس خدمة التنفيذ بالضبط هي التي سيشغّلها العامل — نفس
                // البوابات ونفس القواعد الحمراء، لا مسار ثانٍ.
                if h.jobsClient == nil {
                        c.JSON(http.StatusServiceUnavailable, gin.H{"error": "jobs_unavailable", "message": "المعالجة الخلفية غير متاحة حاليًا — جرّب مرة أخرى"})
                        return
                }
                if verr := services.PharmacyImportEligible(c.Request.Context(), h.db, pharmacyID, c.Param("id")); verr != nil {
                        respondPharmacyLibraryFailure(c, verr, "pharmacy_library_import_failed")
                        return
                }
                args := jobs.LibraryImportArgs{
                        PharmacyID: pharmacyID,
                        LibraryID:  c.Param("id"),
                        CompanyID:  h.companyIDForGate(c, principal),
                        ActorID:    principal.ID,
                        ActorType:  principal.Type,
                        ActorEmail: principal.Email,
                        ActorName:  principal.DisplayName,
                        ActorRole:  principal.Role,
                }
                runID, jobID, qerr := jobs.EnqueueLibraryImport(c.Request.Context(), h.jobsClient, h.jobsDeps, args)
                if errors.Is(qerr, jobs.ErrImportAlreadyInFlight) {
                        c.JSON(http.StatusConflict, gin.H{"error": "import_already_in_flight", "message": "استيراد هذه المكتبة يعمل بالفعل في الخلفية"})
                        return
                }
                if qerr != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "pharmacy_library_import_failed"})
                        return
                }
                c.JSON(http.StatusAccepted, gin.H{"data": gin.H{
                        "queued": true, "run_id": runID, "job_id": jobID,
                        "message": "الاستيراد يعمل في الخلفية — ستصلك رسالة عند الانتهاء",
                }})
                return
        }
        out, err = services.ExecutePharmacyImport(c.Request.Context(), h.db, principal, pharmacyID, c.Param("id"), payload.Items, plan)
        if err != nil {
                respondPharmacyLibraryFailure(c, err, "pharmacy_library_import_failed")
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": gin.H{
                "library_version":   out.LibraryVersion,
                "applied":           out.Applied,
                "products_created":  out.ProductsCreated,
                "products_linked":   out.ProductsLinked,
                "prices_updated":    out.PricesUpdated,
                "skipped":           skipRows(out.Skipped),
                "skipped_truncated": out.SkippedTruncated,
                "plan":              gin.H{"products_used": out.Plan.ProductsUsed, "products_limit": out.Plan.ProductsLimit},
        }})
}

// GetPharmacyLibraryImportStatus — GET /pharmacy/libraries/:id/import/status
// The wizard's poll while the wholesale import works in the background:
// the latest run row for this pharmacy+library, verbatim from the ledger
// the worker maintains (queued → running → succeeded|failed). run:null
// when background processing never queued one (the caller falls back to
// the synchronous selected-items path).
func (h *Handler) GetPharmacyLibraryImportStatus(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }
        run, err := services.LatestLibraryImportRun(c.Request.Context(), h.db, pharmacyID, c.Param("id"))
        if err != nil {
                respondPharmacyLibraryFailure(c, err, "pharmacy_library_import_failed")
                return
        }
        if run == nil {
                c.JSON(http.StatusOK, gin.H{"data": gin.H{"run": nil}})
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": gin.H{"run": run}})
}

// SyncPharmacyLibrary — POST /pharmacy/libraries/:id/sync
// «اسحب التحديثات» — applies everything since the pharmacy's last synced
// version. Pointer moves to the library's current version.
func (h *Handler) SyncPharmacyLibrary(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }
        principal, _ := auth.PrincipalFromContext(c)
        out, err := services.SyncPharmacyLibrary(c.Request.Context(), h.db, principal, pharmacyID, c.Param("id"), h.resolveProductPlanState(c, principal))
        if err != nil {
                respondPharmacyLibraryFailure(c, err, "pharmacy_library_sync_failed")
                return
        }
        if out.NothingToDo {
                // Byte-identical to the pre-extraction early return: no removed
                // key, no plan key, skipped empty.
                c.JSON(http.StatusOK, gin.H{"data": gin.H{
                        "synced": true, "library_version": out.LibraryVersion, "last_synced_version": out.LastSyncedVersion,
                        "nothing_to_do":  true,
                        "prices_updated": 0, "products_added": 0, "products_linked": 0, "skipped": []gin.H{},
                }})
                return
        }
        removed := make([]gin.H, 0, len(out.Removed))
        for _, r := range out.Removed {
                removed = append(removed, gin.H{"global_product_id": r.GlobalProductID, "name": r.Name})
        }
        c.JSON(http.StatusOK, gin.H{"data": gin.H{
                "synced": true, "library_version": out.LibraryVersion, "last_synced_version": out.LastSyncedVersion,
                "nothing_to_do":   false,
                "prices_updated":  out.PricesUpdated,
                "products_added":  out.ProductsAdded,
                "products_linked": out.ProductsLinked,
                "skipped":         skipRows(out.Skipped),
                "removed":         removed,
                "plan":            gin.H{"products_used": out.Plan.ProductsUsed, "products_limit": out.Plan.ProductsLimit},
        }})
}

// ---------------------------------------------------------------------------
// إزالة المكتبة من الصيدلية — «المكتبة مرجع، قاعدة المنتجات ملك الصيدلية»
// معاينة صادقة (هيتحذف/هيفضل) ثم تنفيذ بعد تأكيد صريح؛ المعاملات تعيش في
// services (ADR-0005).
// ---------------------------------------------------------------------------

// PreviewPharmacyLibraryRemoval — POST /pharmacy/libraries/:id/remove/preview
func (h *Handler) PreviewPharmacyLibraryRemoval(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }
        out, err := services.PreviewPharmacyLibraryRemoval(c.Request.Context(), h.db, pharmacyID, c.Param("id"))
        if err != nil {
                if errors.Is(err, services.ErrLibraryNotFound) {
                        c.JSON(http.StatusNotFound, gin.H{"error": "library_not_found", "message": "المكتبة غير موجودة"})
                        return
                }
                respondPharmacyLibraryFailure(c, err, "pharmacy_library_remove_failed")
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": gin.H{
                "library": gin.H{"id": out.LibraryID, "name": out.LibraryName},
                "will_delete": gin.H{
                        "count": out.WillDelete.Count,
                        "items": removalRows(out.WillDelete.Items),
                        "note":  "منتجات بدون أي عملية بيع أو مرتجع أو مخزون — ستُحذف من قاعدة منتجاتك",
                },
                "will_keep": gin.H{
                        "count": out.WillKeep.Count,
                        "items": removalRows(out.WillKeep.Items),
                        "note":  "منتجات لها مبيعات أو مرتجعات أو مخزون — تبقى ملكك وتنفصل من المكتبة فقط",
                },
        }})
}

// ExecutePharmacyLibraryRemoval — POST /pharmacy/libraries/:id/remove/execute
func (h *Handler) ExecutePharmacyLibraryRemoval(c *gin.Context) {
        pharmacyID, ok := pharmacyScope(c)
        if !ok {
                return
        }
        principal, _ := auth.PrincipalFromContext(c)
        out, err := services.ExecutePharmacyLibraryRemoval(c.Request.Context(), h.db, principal, pharmacyID, c.Param("id"))
        if err != nil {
                if errors.Is(err, services.ErrLibraryNotFound) {
                        c.JSON(http.StatusNotFound, gin.H{"error": "library_not_found", "message": "المكتبة غير موجودة"})
                        return
                }
                respondPharmacyLibraryFailure(c, err, "pharmacy_library_remove_failed")
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": gin.H{
                "removed":          true,
                "library_name":     out.LibraryName,
                "products_deleted": out.ProductsDeleted,
                "products_kept":    out.ProductsKept,
        }})
}

// removalRows converts the service's typed rows into the wire shape.
func removalRows(items []services.LibraryRemovalItem) []gin.H {
        rows := make([]gin.H, 0, len(items))
        for _, it := range items {
                rows = append(rows, gin.H{
                        "pharmacy_product_id":    it.PharmacyProductID,
                        "name":                   it.Name,
                        "barcode":                it.Barcode,
                        "selling_price_piastres": it.SellingPrice,
                })
        }
        return rows
}
