// Platform-admin central product libraries — library products + catalog.
//
// ADR-0005 (api_level 110): this file is PROTOCOL ONLY. The transactions,
// the catalog dedup, the change-log semantics, the draft-version tagging,
// the metadata_changed propagation and every audit row live in
// internal/services/libraries_platform.go — moved, not rewritten, with the
// wire contract frozen (same routes, same bodies, same error codes
// verbatim — see respondLibrariesFailure and the per-flow vocabularies
// below, which are the exact code set this file emitted before the
// extraction).
//
// This file keeps the request edge: multipart parsing, the Excel column
// mapping suggestion, the pure Arabic-reason row validator (request-shape
// validation per ADR-0005 rule 2), page parsing, HTTP statuses and
// response bodies.
//
// House rules honored here (unchanged):
//   - the OFFICIAL price lives on library_products (per library/currency),
//     never on global_products,
//   - every write is audited inside the service's transaction,
//   - import price values are major units ×100 (piastres); currencies with
//     3 decimals need their own divisor before those libraries go live —
//     Phase 1 activates EGP only.
package handlers

import (
        "encoding/json"
        "errors"
        "fmt"
        "net/http"
        "strings"

        "github.com/gin-gonic/gin"

        "github.com/pharmacy-os/backend/internal/auth"
        "github.com/pharmacy-os/backend/internal/services"
)

// ifManufacturer — حقل إضافي خاص بمكتبات المنصة (الشركة المنتجة) يُدمج مع
// مرادفات الاستيراد المشتركة من product_import_handler.go.
const ifManufacturer = "manufacturer"

// libraryImportFieldOrder — ترتيب الحقول المعروضة في معالج الربط بالواجهة.
var libraryImportFieldOrder = []string{
        ifName, ifBarcode, ifSellingPrice, ifGenericName, ifStrength,
        ifDosageForm, ifManufacturer,
}

var libraryImportSynonymsNorm = func() map[string]string {
        m := make(map[string]string)
        for k, v := range map[string]string{
                "الشركةالمنتجة": ifManufacturer, "الشركة": ifManufacturer, "المصنع": ifManufacturer,
                "المنتج": "", // never map this here — it collides with the NAME synonym
                "manufacturer": ifManufacturer, "company": ifManufacturer, "producer": ifManufacturer,
                "manufacturercountry": "manufacturer_country",
                "بلدالمنشأ":           "manufacturer_country", "بلدالمنشا": "manufacturer_country",
        } {
                if v == "" {
                        continue
                }
                m[normalizeHeaderKey(k)] = v
        }
        return m
}()

// suggestLibraryImportMapping — يقترح الربط انطلاقًا من المرادفات المشتركة
// ثم يضيف حقول المكتبة الخاصة (الشركة المنتجة).
func suggestLibraryImportMapping(headers []string) map[string]int {
        mapping := suggestImportMapping(headers)
        for i, h := range headers {
                if v, ok := libraryImportSynonymsNorm[normalizeHeaderKey(h)]; ok {
                        if _, taken := mapping[v]; !taken {
                                mapping[v] = i
                        }
                }
        }
        return mapping
}

// validateLibraryImportRow — يتحقق من صف خام ويعيد الصف المحوَّل أو سبب الرفض بالعربية.
// Pure request-shape validation (ADR-0005 rule 2) — the service receives
// only rows that passed, and re-validates nothing.
func validateLibraryImportRow(row []string, mapping map[string]int) (services.LibraryImportRow, string) {
        name := cell(row, mapping[ifName])
        if name == "" {
                return services.LibraryImportRow{}, "الاسم مطلوب"
        }
        r := services.LibraryImportRow{
                Name:             name,
                Barcode:          cell(row, mapping[ifBarcode]),
                GenericName:      cell(row, mapping[ifGenericName]),
                Strength:         cell(row, mapping[ifStrength]),
                DosageForm:       mapImportDosageForm(cell(row, mapping[ifDosageForm])),
                ManufacturerName: cell(row, mapping[ifManufacturer]),
        }
        if raw := cell(row, mapping[ifSellingPrice]); raw != "" {
                v, ok := parseArabicNumber(raw)
                if !ok || v < 0 {
                        return services.LibraryImportRow{}, fmt.Sprintf("سعر غير مفهوم: %s", raw)
                }
                r.PricePiastres = egpToPiastres(v)
                r.HasPrice = true
        }
        return r, ""
}

// ---------------------------------------------------------------------------
// The wire vocabulary — every pre-extraction error code, mapped 1:1 from the
// service's typed stages. Frozen (ADR-0005 rule 3).
// ---------------------------------------------------------------------------

type libWire struct {
        status int
        code   string
}

func respondLibrariesFailure(c *gin.Context, err error, vocab map[services.LibraryStage]libWire) {
        switch err {
        case services.ErrLibraryNotFound:
                c.JSON(http.StatusNotFound, gin.H{"error": "library_not_found", "message": "المكتبة غير موجودة"})
                return
        case services.ErrCatalogProductNotFound:
                c.JSON(http.StatusNotFound, gin.H{"error": "catalog_product_not_found", "message": "المنتج غير موجود في الكتالوج المركزي"})
                return
        case services.ErrLibraryProductNotFound:
                c.JSON(http.StatusNotFound, gin.H{"error": "library_product_not_found", "message": "المنتج غير موجود في هذه المكتبة"})
                return
        case services.ErrBarcodeTaken:
                c.JSON(http.StatusConflict, gin.H{"error": "barcode_taken", "message": "الباركود مسجل بالفعل لمنتج آخر في الكتالوج"})
                return
        }
        var fail *services.LibrariesFailure
        if !errors.As(err, &fail) {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error"})
                return
        }
        wire, ok := vocab[fail.Stage]
        if !ok {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error"})
                return
        }
        c.JSON(wire.status, gin.H{"error": wire.code})
}

// ---------------------------------------------------------------------------
// Library products (the entry surface)
// ---------------------------------------------------------------------------

// ListPlatformLibraryProducts — GET /platform-admin/libraries/:id/products?search=&page=&page_size=
func (h *Handler) ListPlatformLibraryProducts(c *gin.Context) {
        page := parsePositiveInt(c.Query("page"), 1)
        pageSize := parsePositiveInt(c.Query("page_size"), 20)
        if pageSize > 100 {
                pageSize = 100
        }
        search := strings.TrimSpace(c.Query("search"))

        items, total, err := services.ListLibraryProducts(c.Request.Context(), h.db, c.Param("id"), search, page, pageSize)
        if err != nil {
                respondLibrariesFailure(c, err, map[services.LibraryStage]libWire{
                        services.StageCount: {http.StatusInternalServerError, "library_products_count_failed"},
                        services.StageQuery: {http.StatusInternalServerError, "library_products_query_failed"},
                        services.StageScan:  {http.StatusInternalServerError, "library_products_scan_failed"},
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

// AddPlatformLibraryProduct — POST /platform-admin/libraries/:id/products
// Upsert semantics: adding an existing entry with a new price UPDATES it and
// logs price_changed — bulk price-list imports rely on this behavior.
func (h *Handler) AddPlatformLibraryProduct(c *gin.Context) {
        var payload struct {
                GlobalProductID       string                      `json:"global_product_id"`
                OfficialPricePiastres *int64                      `json:"official_price_piastres"`
                Notes                 *string                     `json:"notes"`
                NewProduct            *services.NewCatalogProduct `json:"new_product"`
        }
        if err := c.ShouldBindJSON(&payload); err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": "صيغة الطلب غير صحيحة"})
                return
        }
        if payload.GlobalProductID == "" && payload.NewProduct == nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "product_source_required", "message": "حدّد منتجًا من الكتالوج أو أدخل منتجًا جديدًا"})
                return
        }
        if payload.OfficialPricePiastres != nil && *payload.OfficialPricePiastres < 0 {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_price", "message": "السعر الرسمي لا يمكن أن يكون سالبًا"})
                return
        }
        if payload.NewProduct != nil {
                payload.NewProduct.Normalize()
                if msg := payload.NewProduct.Validate(); msg != "" {
                        c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_product", "message": msg})
                        return
                }
        }

        principal, _ := auth.PrincipalFromContext(c)
        out, err := services.AddLibraryProduct(c.Request.Context(), h.db, principal, c.Param("id"), services.AddLibraryProductInput{
                GlobalProductID:       payload.GlobalProductID,
                OfficialPricePiastres: payload.OfficialPricePiastres,
                Notes:                 payload.Notes,
                NewProduct:            payload.NewProduct,
        })
        if err != nil {
                respondLibrariesFailure(c, err, map[services.LibraryStage]libWire{
                        services.StageTxBegin:        {http.StatusInternalServerError, "tx_begin_failed"},
                        services.StageCatalogCreate:  {http.StatusInternalServerError, "catalog_product_create_failed"},
                        services.StageCatalogCheck:   {http.StatusInternalServerError, "catalog_check_failed"},
                        services.StageDraftVersion:   {http.StatusInternalServerError, "library_query_failed"},
                        services.StageUpsert:         {http.StatusInternalServerError, "library_product_upsert_failed"},
                        services.StageTouch:          {http.StatusInternalServerError, "library_touch_failed"},
                        services.StageAudit:          {http.StatusInternalServerError, "audit_failed"},
                        services.StageTxCommit:       {http.StatusInternalServerError, "tx_commit_failed"},
                })
                return
        }
        c.JSON(http.StatusCreated, gin.H{"data": gin.H{
                "library_id":              out.LibraryID,
                "global_product_id":       out.GlobalProductID,
                "created_product":         out.CreatedProduct,
                "action":                  out.Action,
                "official_price_piastres": out.OfficialPricePiastres,
        }})
}

// UpdatePlatformLibraryProduct — PUT /platform-admin/libraries/:id/products/:pid
func (h *Handler) UpdatePlatformLibraryProduct(c *gin.Context) {
        var payload struct {
                OfficialPricePiastres *int64  `json:"official_price_piastres"`
                Notes                 *string `json:"notes"`
        }
        if err := c.ShouldBindJSON(&payload); err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": "صيغة الطلب غير صحيحة"})
                return
        }
        if payload.OfficialPricePiastres != nil && *payload.OfficialPricePiastres < 0 {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_price", "message": "السعر الرسمي لا يمكن أن يكون سالبًا"})
                return
        }

        principal, _ := auth.PrincipalFromContext(c)
        priceChanged, err := services.UpdateLibraryProduct(c.Request.Context(), h.db, principal, c.Param("id"), c.Param("pid"), services.UpdateLibraryProductInput{
                OfficialPricePiastres: payload.OfficialPricePiastres,
                Notes:                 payload.Notes,
        })
        if err != nil {
                respondLibrariesFailure(c, err, map[services.LibraryStage]libWire{
                        services.StageTxBegin:      {http.StatusInternalServerError, "tx_begin_failed"},
                        services.StageQuery:        {http.StatusInternalServerError, "library_product_query_failed"},
                        services.StageDraftVersion: {http.StatusInternalServerError, "library_query_failed"},
                        services.StageUpdate:       {http.StatusInternalServerError, "library_product_update_failed"},
                        services.StageChangeLog:    {http.StatusInternalServerError, "library_change_log_failed"},
                        services.StageTouch:        {http.StatusInternalServerError, "library_touch_failed"},
                        services.StageAudit:        {http.StatusInternalServerError, "audit_failed"},
                        services.StageTxCommit:     {http.StatusInternalServerError, "tx_commit_failed"},
                })
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": gin.H{"updated": true, "price_changed": priceChanged}})
}

// RemovePlatformLibraryProduct — DELETE /platform-admin/libraries/:id/products/:pid
// Removing an entry logs «removed» so pharmacies (Phase 2) see it in the
// diff; products already imported into a pharmacy stay theirs — the log is
// informational, never a deletion order.
func (h *Handler) RemovePlatformLibraryProduct(c *gin.Context) {
        principal, _ := auth.PrincipalFromContext(c)
        err := services.RemoveLibraryProduct(c.Request.Context(), h.db, principal, c.Param("id"), c.Param("pid"))
        if err != nil {
                respondLibrariesFailure(c, err, map[services.LibraryStage]libWire{
                        services.StageTxBegin:      {http.StatusInternalServerError, "tx_begin_failed"},
                        services.StageQuery:        {http.StatusInternalServerError, "library_product_query_failed"},
                        services.StageDraftVersion: {http.StatusInternalServerError, "library_query_failed"},
                        services.StageDelete:       {http.StatusInternalServerError, "library_product_delete_failed"},
                        services.StageChangeLog:    {http.StatusInternalServerError, "library_change_log_failed"},
                        services.StageTouch:        {http.StatusInternalServerError, "library_touch_failed"},
                        services.StageAudit:        {http.StatusInternalServerError, "audit_failed"},
                        services.StageTxCommit:     {http.StatusInternalServerError, "tx_commit_failed"},
                })
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": gin.H{"removed": true}})
}

// PreviewPlatformLibraryImport — POST /platform-admin/libraries/:id/import/preview
// Same 2-step contract as the pharmacy import: parse the file, suggest the
// column mapping (accepting an override), and estimate validity — the UI
// shows the sample before the execute call commits anything.
func (h *Handler) PreviewPlatformLibraryImport(c *gin.Context) {
        fh, err := c.FormFile("file")
        if err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "file_required", "message": "يرجى رفع ملف Excel (xlsx) أو CSV"})
                return
        }
        headers, rows, fileType, perr := parseImportFile(fh)
        if perr != nil {
                msg := map[string]string{
                        "file_too_large":     "حجم الملف أكبر من 10 ميجابايت",
                        "unsupported_format": "صيغة غير مدعومة — يُقبل Excel بصيغة xlsx أو ملف CSV",
                        "invalid_xlsx":       "الملف ليس ملف Excel صالحًا",
                        "invalid_csv":        "ملف CSV غير صالح",
                        "empty_file":         "الملف لا يحتوي بيانات",
                        "cannot_read_file":   "تعذرت قراءة الملف",
                }[perr.Error()]
                if msg == "" {
                        msg = "تعذر تحليل الملف"
                }
                c.JSON(http.StatusBadRequest, gin.H{"error": perr.Error(), "message": msg})
                return
        }
        if len(rows) > importMaxRows {
                c.JSON(http.StatusBadRequest, gin.H{"error": "too_many_rows", "message": fmt.Sprintf("الحد الأقصى %d صفًا في الملف الواحد", importMaxRows)})
                return
        }

        mapping := suggestLibraryImportMapping(headers)
        if override := strings.TrimSpace(c.PostForm("mapping")); override != "" {
                var custom map[string]int
                if err := json.Unmarshal([]byte(override), &custom); err == nil {
                        for f, idx := range custom {
                                if idx >= len(headers) {
                                        idx = -1
                                }
                                mapping[f] = idx
                        }
                }
        }

        sample := rows
        if len(sample) > importSampleRows {
                sample = sample[:importSampleRows]
        }
        valid, invalid := 0, 0
        var invalidReasons []string
        for i, r := range rows {
                if _, verr := validateLibraryImportRow(r, mapping); verr == "" {
                        valid++
                } else {
                        invalid++
                        if len(invalidReasons) < 5 {
                                invalidReasons = append(invalidReasons, fmt.Sprintf("صف %d: %s", i+2, verr))
                        }
                }
        }

        sampleOut := make([]map[string]any, 0, len(sample))
        for _, r := range sample {
                sampleOut = append(sampleOut, gin.H{
                        "name":         cell(r, mapping[ifName]),
                        "barcode":      cell(r, mapping[ifBarcode]),
                        "price":        cell(r, mapping[ifSellingPrice]),
                        "generic_name": cell(r, mapping[ifGenericName]),
                        "strength":     cell(r, mapping[ifStrength]),
                        "dosage_form":  cell(r, mapping[ifDosageForm]),
                        "manufacturer": cell(r, mapping[ifManufacturer]),
                })
        }

        c.JSON(http.StatusOK, gin.H{"data": gin.H{
                "file_type":       fileType,
                "headers":         headers,
                "mapping":         mapping,
                "fields":          libraryImportFieldOrder,
                "total_rows":      len(rows),
                "valid_rows":      valid,
                "invalid_rows":    invalid,
                "invalid_reasons": invalidReasons,
                "sample":          sampleOut,
        }})
}

// ExecutePlatformLibraryImport — POST /platform-admin/libraries/:id/import/execute
//
// Feeds a library from an Excel/CSV price list: the edge validates every
// row BEFORE the service opens the transaction (house rule), then the
// service owns the catalog resolution, the upserts, the change log and the
// audit — all-or-nothing in a single transaction, like the pharmacy import.
func (h *Handler) ExecutePlatformLibraryImport(c *gin.Context) {
        fh, err := c.FormFile("file")
        if err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "file_required", "message": "يرجى رفع ملف Excel (xlsx) أو CSV"})
                return
        }
        headers, rows, _, perr := parseImportFile(fh)
        if perr != nil {
                msg := map[string]string{
                        "file_too_large":     "حجم الملف أكبر من 10 ميجابايت",
                        "unsupported_format": "صيغة غير مدعومة — يُقبل Excel بصيغة xlsx أو ملف CSV",
                        "invalid_xlsx":       "الملف ليس ملف Excel صالحًا",
                        "invalid_csv":        "ملف CSV غير صالح",
                        "empty_file":         "الملف لا يحتوي بيانات",
                        "cannot_read_file":   "تعذرت قراءة الملف",
                }[perr.Error()]
                if msg == "" {
                        msg = "تعذر تحليل الملف"
                }
                c.JSON(http.StatusBadRequest, gin.H{"error": perr.Error(), "message": msg})
                return
        }
        if len(rows) > importMaxRows {
                c.JSON(http.StatusBadRequest, gin.H{"error": "too_many_rows", "message": fmt.Sprintf("الحد الأقصى %d صفًا في الملف الواحد", importMaxRows)})
                return
        }

        mapping := suggestLibraryImportMapping(headers)
        if override := strings.TrimSpace(c.PostForm("mapping")); override != "" {
                var custom map[string]int
                if err := json.Unmarshal([]byte(override), &custom); err == nil {
                        for f, idx := range custom {
                                if idx >= len(headers) {
                                        idx = -1
                                }
                                mapping[f] = idx
                        }
                }
        }

        // Validate every row BEFORE the service opens the transaction (house rule).
        parsed := make([]services.LibraryImportRow, 0, len(rows))
        var importErrors []string
        for i, r := range rows {
                pr, verr := validateLibraryImportRow(r, mapping)
                if verr != "" {
                        if len(importErrors) < importErrorCap {
                                importErrors = append(importErrors, fmt.Sprintf("صف %d: %s", i+2, verr))
                        }
                        continue
                }
                parsed = append(parsed, pr)
        }

        principal, _ := auth.PrincipalFromContext(c)
        report, err := services.ExecuteLibraryImport(c.Request.Context(), h.db, principal, c.Param("id"), parsed, len(rows), importErrors)
        if err != nil {
                respondLibrariesFailure(c, err, map[services.LibraryStage]libWire{
                        services.StageTxBegin:      {http.StatusInternalServerError, "tx_begin_failed"},
                        services.StageDraftVersion: {http.StatusInternalServerError, "library_query_failed"},
                        services.StageBarcodeIndex: {http.StatusInternalServerError, "catalog_barcode_query_failed"},
                        services.StageNameIndex:    {http.StatusInternalServerError, "catalog_name_query_failed"},
                        services.StageTouch:        {http.StatusInternalServerError, "library_touch_failed"},
                        services.StageAudit:        {http.StatusInternalServerError, "audit_failed"},
                        services.StageTxCommit:     {http.StatusInternalServerError, "tx_commit_failed"},
                })
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": gin.H{
                "total_rows":       report.TotalRows,
                "added":            report.Added,
                "price_updated":    report.PriceUpdated,
                "products_created": report.ProductsCreated,
                "unchanged":        report.Unchanged,
                "failed":           report.Failed,
                "errors":           report.Errors,
        }})
}

// ListPlatformCatalogProducts — GET /platform-admin/catalog/products
// The central catalog browser backing the «إضافة منتج من الكتالوج» picker.
func (h *Handler) ListPlatformCatalogProducts(c *gin.Context) {
        page := parsePositiveInt(c.Query("page"), 1)
        pageSize := parsePositiveInt(c.Query("page_size"), 20)
        if pageSize > 100 {
                pageSize = 100
        }
        search := strings.TrimSpace(c.Query("search"))

        items, total, err := services.ListCatalogProducts(c.Request.Context(), h.db, search, page, pageSize)
        if err != nil {
                respondLibrariesFailure(c, err, map[services.LibraryStage]libWire{
                        services.StageCount: {http.StatusInternalServerError, "catalog_count_failed"},
                        services.StageQuery: {http.StatusInternalServerError, "catalog_query_failed"},
                        services.StageScan:  {http.StatusInternalServerError, "catalog_scan_failed"},
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

// UpdatePlatformCatalogProduct — PUT /platform-admin/catalog/products/:id
// Full metadata edit of a central catalog product + the verification badge.
func (h *Handler) UpdatePlatformCatalogProduct(c *gin.Context) {
        var payload services.CatalogProductUpdate
        if err := c.ShouldBindJSON(&payload); err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": "صيغة الطلب غير صحيحة"})
                return
        }
        if payload.DosageForm != nil && !services.ValidDosageForms[*payload.DosageForm] {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_dosage_form", "message": "الشكل الصيدلي غير معروف"})
                return
        }
        if payload.ProductCategory != nil && !services.ValidProductCategories[*payload.ProductCategory] {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_category", "message": "تصنيف المنتج غير معروف"})
                return
        }
        if payload.RequiresPrescription != nil {
                v := *payload.RequiresPrescription
                if v != "yes" && v != "no" && v != "otc_only" {
                        c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_prescription", "message": "قيمة وصفة الطبيب يجب أن تكون: yes أو no أو otc_only"})
                        return
                }
        }
        if payload.Barcode != nil {
                *payload.Barcode = strings.TrimSpace(*payload.Barcode)
        }
        if payload.Name != nil && strings.TrimSpace(*payload.Name) == "" {
                c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_name", "message": "اسم المنتج لا يمكن أن يكون فارغًا"})
                return
        }

        principal, _ := auth.PrincipalFromContext(c)
        err := services.UpdateCatalogProduct(c.Request.Context(), h.db, principal, c.Param("id"), payload)
        if err != nil {
                respondLibrariesFailure(c, err, map[services.LibraryStage]libWire{
                        services.StageTxBegin:      {http.StatusInternalServerError, "tx_begin_failed"},
                        services.StageQuery:        {http.StatusInternalServerError, "catalog_query_failed"},
                        services.StageUpdate:       {http.StatusInternalServerError, "catalog_update_failed"},
                        services.StagePropagation:  {http.StatusInternalServerError, "library_change_log_failed"},
                        services.StageAudit:        {http.StatusInternalServerError, "audit_failed"},
                        services.StageTxCommit:     {http.StatusInternalServerError, "tx_commit_failed"},
                })
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": gin.H{"updated": true}})
}
