// ADR-0005 (api_level 110) — ledger item 3 (pharmacy half): the pharmacy
// side of the central libraries — visibility, the three-gated import
// (dedup preview → explicit execution), and the one-tap update pull — moves
// out of pharmacy_libraries_handler.go under the layering policy. The code
// is moved, not rewritten: same visibility predicate, same FOR UPDATE OF l
// library lock, same red lines, same skip reasons, same sync-pointer
// GREATEST semantics — the handler maps each typed error 1:1 onto the exact
// wire body it emitted before. The wire contract is frozen during
// extraction (ADR-0005 rule 3): mobile APKs are not exported from this repo
// and a stale install must keep working.
//
// Ownership split (ADR-0005 rule 2): THIS package owns the transactions,
// the visibility checks inside them, the plan-limit arithmetic, the twin
// linking (barcode → strong-name similarity), the sync pointer, and the
// tenant audit rows (auditlog.Tenant — the single canonical writer). The
// handler keeps protocol only: principal scoping, request binding, the
// subscription-gate lookup (companyID + CheckLimit resolved ONCE and passed
// in as PlanState), HTTP statuses and response bodies.
//
// The ownership contract is sacred here (unchanged, audit-safe):
//   - stock and batches are NEVER touched — importing creates catalog
//     links and prices only, zero inventory rows,
//   - cost price belongs to the pharmacy — never written,
//   - selling price follows the official library price (pharmacy can still
//     edit it locally; the next pull restores the official value),
//   - «removed» from a library is informational — products already
//     imported stay owned by the pharmacy (red line),
//   - plan product limit is a soft gate: items beyond the ceiling come
//     back as skipped with a «upgrade» flag, never a silent failure.
package services

import (
        "context"
        "errors"
        "fmt"
        "sort"
        "strings"
        "time"

        "github.com/jackc/pgx/v5"
        "github.com/jackc/pgx/v5/pgxpool"

        "github.com/pharmacy-os/backend/internal/auditlog"
        "github.com/pharmacy-os/backend/internal/auth"
)

// NameSuspectThreshold — الاسم المشابه فوق هذا الحد يُعرض كمرشّح «ربما
// نفس المنتج» للمراجعة؛ أقل من ذلك يُعد منتجًا جديدًا بلا إزعاج.
const NameSuspectThreshold = 0.55

// PreviewCandidateCap — سقف المعاينة الشاملة (mode=all): فوق هذا العدد
// يعود باب المعاينة 400 preview_too_large بدل طلب يعالج ملايين أزواج فحص
// التكرار ويغرق الواجهة بآلاف البطاقات. السحب الجماعي يمر عبر البحث
// والتحديد من قائمة منتجات المكتبة (المعاينة المحددة تفحص التكرار كاملة
// دائمًا)، والتنفيذ يعيد التحقق من كل شيء على الخادم والمزامنة تربط
// التوائم الواثقة تلقائيًا — فلا مساس بأمان البيانات.
const PreviewCandidateCap = 2000

// NameAutoLinkThreshold — فوق هذا الحد المزامنة تربط تلقائيًا بدل إنشاء
// مكرر — لكن مسار التشابه الاسمي فقط عندما يكون صف الصيدلية بلا باركود:
// منتج يحمل باركود مُصنّع خاصًا به لا يُعاد توجيهه على دليل اسمي مهما قوي —
// باركودان مختلفان دليل انفصال أقوى من تشابه اسمين، وإعادة توجيهه كانت
// تحوّل صنفًا حقيقيًا لدى الصيدلية إلى دواء آخر بصمت بهوية وسعر رسمي
// لا يخصه. مسار الباركود يظل تطابقًا تامًا بلا شرط إضافي.
const NameAutoLinkThreshold = 0.72

var (
        // ErrLibraryNotVisible: the library is not published for this pharmacy
        // (or unknown) — 404 library_not_visible.
        ErrLibraryNotVisible = errors.New("library not visible to this pharmacy")
        // ErrLibraryNotImported: a pull was requested before any import — 400
        // library_not_imported.
        ErrLibraryNotImported = errors.New("library not imported yet")
        // ErrImportNoSelection: every item id was blank — 400 invalid_body with
        // the same message the protocol edge uses for an empty items array.
        ErrImportNoSelection = errors.New("import requires at least one product")
)

// PharmacyLibraryStage — every non-sentinel failure in the two pharmacy
// flows maps onto ONE wire code per flow (pharmacy_library_import_failed /
// pharmacy_library_sync_failed) except the audit stage (audit_failed); the
// stage value only sharpens the server-side log.
type PharmacyLibraryStage string

const (
        StagePharmacyBegin   PharmacyLibraryStage = "begin"
        StagePharmacyQuery   PharmacyLibraryStage = "query"
        StagePharmacyWrite   PharmacyLibraryStage = "write"
        StagePharmacyPointer PharmacyLibraryStage = "pointer"
        StagePharmacyAudit   PharmacyLibraryStage = "audit"
        StagePharmacyCommit  PharmacyLibraryStage = "commit"
        StagePharmacyScan    PharmacyLibraryStage = "scan"
        StagePharmacyCount   PharmacyLibraryStage = "count"
)

// PharmacyLibrariesFailure wraps a stage with its underlying error.
type PharmacyLibrariesFailure struct {
        Stage PharmacyLibraryStage
        Err   error
}

func (e *PharmacyLibrariesFailure) Error() string {
        return fmt.Sprintf("pharmacy libraries: stage %s: %v", e.Stage, e.Err)
}

func (e *PharmacyLibrariesFailure) Unwrap() error { return e.Err }

// tenantActor unwraps a pharmacy principal into the canonical auditlog
// identity: actor_id references employees(id) only — company-user
// principals have no employees row, so actor_id stays NULL and the
// denormalized identity columns carry the who (same rule the handlers
// wrapper has always applied via auth.ActorIDs).
func tenantActor(principal *auth.Principal) auditlog.Actor {
        if principal == nil {
                return auditlog.Actor{}
        }
        employeeID, _ := auth.ActorIDs(principal)
        return auditlog.Actor{
                ID:          employeeID,
                Email:       principal.Email,
                DisplayName: principal.DisplayName,
                Role:        principal.Role,
        }
}

// PlanState is the subscription gate resolution the protocol edge performs
// once per request (companyID lookup + CheckLimit) and hands to the
// service. Limit = -1 means unlimited, 0 means not configured (also
// treated unlimited) — the arithmetic below is the business rule and lives
// here, not in the handler.
type PlanState struct {
        Used  int
        Limit int
}

func (p PlanState) unlimited() bool { return p.Limit == -1 || p.Limit == 0 }

func (p PlanState) remaining() int {
        if p.unlimited() {
                return 0
        }
        r := p.Limit - p.Used
        if r < 0 {
                return 0
        }
        return r
}

// derefStr is the local nil-safe string deref (the handlers copy moves out
// with its file).
func derefStr(s *string) string {
        if s == nil {
                return ""
        }
        return *s
}

// ---------------------------------------------------------------------------
// الرؤية: مكتبات منشورة تناسب بلد الصيدلية (أو عامة)
// ---------------------------------------------------------------------------

// libraryVisibilityWhere is the shared visibility predicate. $1 = pharmacy
// id. Match rule: library.country_code IS NULL OR equals the pharmacy's
// country (case/space-insensitive). Pharmacies with no country only see
// universal libraries — the UI explains the rest.
const libraryVisibilityWhere = `
        l.is_published = true
        AND (l.country_code IS NULL
             OR (ph.country IS NOT NULL AND btrim(ph.country) <> ''
                 AND UPPER(btrim(l.country_code)) = UPPER(btrim(ph.country))))
`

// syncStateSelect computes the pharmacy's per-library sync status:
// 0 = never imported, 1 = up to date, >0 = update available.
const syncStateSelect = `
        COALESCE(s.last_synced_version, 0) AS last_synced_version,
        s.synced_at AS synced_at,
        l.version - COALESCE(s.last_synced_version, 0) AS versions_behind,
        CASE
                WHEN s.id IS NULL THEN 'not_imported'
                WHEN l.version = s.last_synced_version THEN 'up_to_date'
                ELSE 'update_available'
        END AS sync_status,
        (SELECT COUNT(*) FROM library_changes dc
           WHERE dc.library_id = l.id
             AND dc.version > COALESCE(s.last_synced_version, 0)
             AND dc.version <= l.version) AS pending_changes,
        (SELECT COUNT(*) FROM library_products lp WHERE lp.library_id = l.id) AS product_count
`

// PharmacyLibraryRow mirrors one row of GET /pharmacy/libraries (and the
// single-library GET) — json tags verbatim.
type PharmacyLibraryRow struct {
        ID                string     `json:"id"`
        Name              string     `json:"name"`
        Description       string     `json:"description"`
        CountryCode       string     `json:"country_code"`
        Currency          string     `json:"currency"`
        Version           int        `json:"version"`
        PublishedAt       *time.Time `json:"published_at"`
        LastSyncedVersion int        `json:"last_synced_version"`
        SyncedAt          *time.Time `json:"synced_at"`
        VersionsBehind    int        `json:"versions_behind"`
        SyncStatus        string     `json:"sync_status"`
        PendingChanges    int        `json:"pending_changes"`
        ProductCount      int        `json:"product_count"`
}

const pharmacyLibrarySelectBody = `
        SELECT l.id::text, l.name, COALESCE(l.description, ''), COALESCE(l.country_code, ''),
               l.currency::text, l.version, l.published_at,
        ` + syncStateSelect + `
        FROM product_libraries l
        JOIN pharmacies ph ON ph.id = $1::uuid
`

// ListPharmacyLibraries serves GET /pharmacy/libraries.
func ListPharmacyLibraries(ctx context.Context, db LibraryDB, pharmacyID string) ([]PharmacyLibraryRow, error) {
        rows, err := db.Query(ctx, pharmacyLibrarySelectBody+`
                LEFT JOIN pharmacy_library_syncs s ON s.library_id = l.id AND s.pharmacy_id = $1::uuid
                WHERE `+libraryVisibilityWhere+`
                ORDER BY (s.id IS NULL), l.name
        `, pharmacyID)
        if err != nil {
                return nil, &PharmacyLibrariesFailure{Stage: StagePharmacyQuery, Err: err}
        }
        defer rows.Close()

        items := []PharmacyLibraryRow{}
        for rows.Next() {
                var r PharmacyLibraryRow
                if err := rows.Scan(&r.ID, &r.Name, &r.Description, &r.CountryCode, &r.Currency,
                        &r.Version, &r.PublishedAt, &r.LastSyncedVersion, &r.SyncedAt,
                        &r.VersionsBehind, &r.SyncStatus, &r.PendingChanges, &r.ProductCount); err != nil {
                        return nil, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
                }
                items = append(items, r)
        }
        if err := rows.Err(); err != nil {
                return nil, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
        }
        return items, nil
}

// GetPharmacyLibrary serves GET /pharmacy/libraries/:id.
func GetPharmacyLibrary(ctx context.Context, db LibraryDB, pharmacyID, libraryID string) (PharmacyLibraryRow, error) {
        var row PharmacyLibraryRow
        err := db.QueryRow(ctx, pharmacyLibrarySelectBody+`
                LEFT JOIN pharmacy_library_syncs s ON s.library_id = l.id AND s.pharmacy_id = $1::uuid
                WHERE l.id = $2::uuid AND `+libraryVisibilityWhere+`
        `, pharmacyID, libraryID).Scan(
                &row.ID, &row.Name, &row.Description, &row.CountryCode, &row.Currency,
                &row.Version, &row.PublishedAt, &row.LastSyncedVersion, &row.SyncedAt,
                &row.VersionsBehind, &row.SyncStatus, &row.PendingChanges, &row.ProductCount)
        if errors.Is(err, pgx.ErrNoRows) {
                return row, ErrLibraryNotVisible
        }
        if err != nil {
                return row, &PharmacyLibrariesFailure{Stage: StagePharmacyQuery, Err: err}
        }
        return row, nil
}

// ---------------------------------------------------------------------------
// What the pharmacy would import — with the per-row «already have» flag
// ---------------------------------------------------------------------------

// PharmacyLibraryProductRow mirrors one row of GET
// /pharmacy/libraries/:id/products — json tags verbatim.
type PharmacyLibraryProductRow struct {
        GlobalProductID   string  `json:"global_product_id"`
        Name              string  `json:"name"`
        GenericName       string  `json:"generic_name"`
        BrandName         string  `json:"brand_name"`
        Barcode           string  `json:"barcode"`
        Strength          string  `json:"strength"`
        DosageForm        string  `json:"dosage_form"`
        IsVerified        bool    `json:"is_verified"`
        OfficialPrice     int64   `json:"official_price_piastres"`
        PharmacyProductID *string `json:"pharmacy_product_id"`
        MyPrice           *int64  `json:"my_price_piastres"`
        AlreadyHave       bool    `json:"already_have"`
}

// ListPharmacyLibraryProducts serves GET
// /pharmacy/libraries/:id/products?search=.
func ListPharmacyLibraryProducts(ctx context.Context, db LibraryDB, pharmacyID, libraryID, search string, page, pageSize int) ([]PharmacyLibraryProductRow, int64, error) {
        where := ` AND ($2 = '' OR gp.name ILIKE '%' || $2 || '%' OR gp.generic_name ILIKE '%' || $2 || '%' OR gp.barcode ILIKE $2 || '%')`

        var total int64
        if err := db.QueryRow(ctx, `
                SELECT COUNT(*)
                FROM product_libraries l
                JOIN pharmacies ph ON ph.id = $1::uuid
                JOIN library_products lp ON lp.library_id = l.id
                JOIN global_products gp ON gp.id = lp.global_product_id
                LEFT JOIN pharmacy_products pp ON pp.pharmacy_id = $1::uuid AND pp.global_product_id = lp.global_product_id
                WHERE l.id = $3::uuid AND `+libraryVisibilityWhere+where,
                pharmacyID, search, libraryID,
        ).Scan(&total); err != nil {
                return nil, 0, &PharmacyLibrariesFailure{Stage: StagePharmacyCount, Err: err}
        }

        rows, err := db.Query(ctx, `
                SELECT gp.id::text, gp.name, COALESCE(gp.generic_name, ''), COALESCE(gp.brand_name, ''),
                       COALESCE(gp.barcode, ''), COALESCE(gp.strength, ''), gp.dosage_form::text,
                       gp.is_verified, lp.official_price_piastres,
                       pp.id::text, pp.selling_price,
                       (pp.id IS NOT NULL) AS already_have
                FROM product_libraries l
                JOIN pharmacies ph ON ph.id = $1::uuid
                JOIN library_products lp ON lp.library_id = l.id
                JOIN global_products gp ON gp.id = lp.global_product_id
                LEFT JOIN pharmacy_products pp ON pp.pharmacy_id = $1::uuid AND pp.global_product_id = lp.global_product_id
                WHERE l.id = $3::uuid AND `+libraryVisibilityWhere+where+`
                ORDER BY gp.name
                LIMIT $4 OFFSET $5
        `, pharmacyID, search, libraryID, pageSize, (page-1)*pageSize)
        if err != nil {
                return nil, 0, &PharmacyLibrariesFailure{Stage: StagePharmacyQuery, Err: err}
        }
        defer rows.Close()

        items := []PharmacyLibraryProductRow{}
        for rows.Next() {
                var r PharmacyLibraryProductRow
                if err := rows.Scan(&r.GlobalProductID, &r.Name, &r.GenericName, &r.BrandName,
                        &r.Barcode, &r.Strength, &r.DosageForm, &r.IsVerified, &r.OfficialPrice,
                        &r.PharmacyProductID, &r.MyPrice, &r.AlreadyHave); err != nil {
                        return nil, 0, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
                }
                items = append(items, r)
        }
        if err := rows.Err(); err != nil {
                return nil, 0, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
        }
        return items, total, nil
}

// ---------------------------------------------------------------------------
// «ما الجديد منذ آخر مزامنة لي؟» — the exact rows the pull will read
// ---------------------------------------------------------------------------

// LibraryChangeRow mirrors one diff row — json tags verbatim.
type LibraryChangeRow struct {
        GlobalProductID string    `json:"global_product_id"`
        ProductName     string    `json:"product_name"`
        Barcode         string    `json:"barcode"`
        Version         int       `json:"version"`
        ChangeType      string    `json:"change_type"`
        OldPrice        *int64    `json:"old_price_piastres"`
        NewPrice        *int64    `json:"new_price_piastres"`
        Summary         string    `json:"summary"`
        CreatedAt       time.Time `json:"created_at"`
}

// LibraryRef identifies the diffed library — the handler nests it under
// "library" with the same keys.
type LibraryRef struct {
        ID          string
        Name        string
        Version     int
        LastSynced  int
        IsPublished bool
}

// LibraryDiff is the GET /pharmacy/libraries/:id/diff payload. Never
// imported → sync_version 0 and empty list (the UI offers the import
// wizard instead).
type LibraryDiff struct {
        Library LibraryRef
        Changes []LibraryChangeRow
        Counts  map[string]int
}

// GetLibraryDiff serves GET /pharmacy/libraries/:id/diff.
func GetLibraryDiff(ctx context.Context, db LibraryDB, pharmacyID, libraryID string) (LibraryDiff, error) {
        var ref LibraryRef
        err := db.QueryRow(ctx, `
                SELECT l.id::text, l.name, l.version, l.is_published, COALESCE(s.last_synced_version, 0)
                FROM product_libraries l
                JOIN pharmacies ph ON ph.id = $1::uuid
                LEFT JOIN pharmacy_library_syncs s ON s.library_id = l.id AND s.pharmacy_id = $1::uuid
                WHERE l.id = $2::uuid AND `+libraryVisibilityWhere+`
        `, pharmacyID, libraryID).Scan(&ref.ID, &ref.Name, &ref.Version, &ref.IsPublished, &ref.LastSynced)
        if errors.Is(err, pgx.ErrNoRows) {
                return LibraryDiff{}, ErrLibraryNotVisible
        }
        if err != nil {
                return LibraryDiff{}, &PharmacyLibrariesFailure{Stage: StagePharmacyQuery, Err: err}
        }

        rows, err := db.Query(ctx, `
                SELECT DISTINCT ON (c.global_product_id, c.change_type)
                       c.global_product_id::text, gp.name, COALESCE(gp.barcode, ''),
                       c.version, c.change_type, c.old_price_piastres, c.new_price_piastres,
                       COALESCE(c.summary, ''), c.created_at
                FROM library_changes c
                JOIN global_products gp ON gp.id = c.global_product_id
                WHERE c.library_id = $1::uuid
                  AND c.version > $2 AND c.version <= $3
                ORDER BY c.global_product_id, c.change_type, c.version DESC, c.created_at DESC
        `, ref.ID, ref.LastSynced, ref.Version)
        if err != nil {
                return LibraryDiff{}, &PharmacyLibrariesFailure{Stage: StagePharmacyQuery, Err: err}
        }
        defer rows.Close()

        changes := []LibraryChangeRow{}
        counts := map[string]int{}
        for rows.Next() {
                var r LibraryChangeRow
                if err := rows.Scan(&r.GlobalProductID, &r.ProductName, &r.Barcode, &r.Version,
                        &r.ChangeType, &r.OldPrice, &r.NewPrice, &r.Summary, &r.CreatedAt); err != nil {
                        return LibraryDiff{}, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
                }
                counts[r.ChangeType]++
                changes = append(changes, r)
        }
        if err := rows.Err(); err != nil {
                return LibraryDiff{}, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
        }
        return LibraryDiff{Library: ref, Changes: changes, Counts: counts}, nil
}

// ---------------------------------------------------------------------------
// البوابة 2 — كشف التكرار مقابل منتجات الصيدلية (دفعة واحدة)
// ---------------------------------------------------------------------------

// LibraryCandidate — منتج من المكتبة مرشّح للاستيراد. Json tags are the
// frozen wire vocabulary.
type LibraryCandidate struct {
        GlobalProductID   string           `json:"global_product_id"`
        Name              string           `json:"name"`
        Barcode           string           `json:"barcode"`
        OfficialPrice     int64            `json:"official_price_piastres"`
        AlreadyHave       bool             `json:"already_have"`
        PharmacyProductID string           `json:"pharmacy_product_id,omitempty"`
        MyPrice           *int64           `json:"my_price_piastres,omitempty"`
        SuggestedAction   string           `json:"suggested_action"` // create_new | link_existing | already_have
        Matches           []MatchCandidate `json:"matches,omitempty"`
        PlanLimited       bool             `json:"plan_limited,omitempty"`
}

// MatchCandidate — منتج قائم بالصيدلية يشتبه بأنه نفس المنتج.
type MatchCandidate struct {
        PharmacyProductID string  `json:"pharmacy_product_id"`
        Name              string  `json:"name"`
        Barcode           string  `json:"barcode"`
        Price             int64   `json:"selling_price_piastres"`
        Score             float64 `json:"score"`
        MatchType         string  `json:"match_type"` // global_match | barcode | name
}

// LoadLibraryCandidates returns the library's current published products as
// import candidates (optionally restricted to selected global product ids:
// non-empty filter = explicit selection). Task 168: an explicit selection
// filters IN SQL (`= ANY`) — loading all 27,190 wholesale rows to skip all
// but a handful in Go was a full library scan per preview.
func LoadLibraryCandidates(ctx context.Context, db LibraryDB, pharmacyID, libraryID string, filter map[string]bool) ([]LibraryCandidate, error) {
        query := `
                SELECT lp.global_product_id::text, gp.name, COALESCE(gp.barcode, ''),
                       lp.official_price_piastres,
                       pp.id::text, pp.selling_price
                FROM product_libraries l
                JOIN pharmacies ph ON ph.id = $1::uuid
                JOIN library_products lp ON lp.library_id = l.id
                JOIN global_products gp ON gp.id = lp.global_product_id
                LEFT JOIN pharmacy_products pp ON pp.pharmacy_id = $1::uuid AND pp.global_product_id = lp.global_product_id
                WHERE l.id = $2::uuid AND ` + libraryVisibilityWhere
        args := []any{pharmacyID, libraryID}
        if filter != nil {
                ids := make([]string, 0, len(filter))
                for id := range filter {
                        ids = append(ids, id)
                }
                query += `
                AND lp.global_product_id::text = ANY($3::text[])`
                args = append(args, ids)
        }
        query += `
                ORDER BY gp.name
        `
        rows, err := db.Query(ctx, query, args...)
        if err != nil {
                return nil, err
        }
        defer rows.Close()

        candidates := []LibraryCandidate{}
        for rows.Next() {
                var r LibraryCandidate
                var ppID *string
                var myPrice *int64
                if err := rows.Scan(&r.GlobalProductID, &r.Name, &r.Barcode, &r.OfficialPrice, &ppID, &myPrice); err != nil {
                        return nil, err
                }
                r.AlreadyHave = ppID != nil
                r.PharmacyProductID = derefStr(ppID)
                r.MyPrice = myPrice
                if r.AlreadyHave {
                        r.SuggestedAction = "already_have"
                } else {
                        r.SuggestedAction = "create_new"
                }
                candidates = append(candidates, r)
        }
        return candidates, rows.Err()
}

// MatchCardinalityCap — the wire carries at most this many suspect matches
// per candidate, best-first (barcode precedence, then score). The UI shows
// the top three and the auto-link suggestion keys on the strongest match;
// the cap keeps a generic name («Panadol») against a large pharmacy catalog
// from flooding the wire with hundreds of rows per candidate while
// preserving every suggestion that matters — barcode and containment
// matches outrank fuzzy ones, and the strongest fuzzy match is exactly what
// a link suggestion keys on. Task 167's shape returned matches in arbitrary
// scan order, so matches[0] could be a weak look-alike while a decisive
// barcode twin sat deeper in the list — the rank below fixes that.
const MatchCardinalityCap = 5

// AttachDedupMatches fills each not-yet-had candidate with the pharmacy's
// possible existing twins — one batched flow for the whole selection.
//
// Task 168 (the wholesale-library lesson): the old shape joined candidates
// against pharmacy products under ONE OR of barcode / containment /
// similarity — a predicate the planner cannot back with any index, so a
// 27,190-product wholesale preview computed similarity() over the whole
// pair space (~61s measured at 27K×500, and a quadratic 27K×27K after the
// pharmacy imported the library). The rewrite separates the arms by what
// each can actually be indexed with:
//
//   - ONE SQL read of the pharmacy's OWN rows — active rows NOT already
//     linked to ANY candidate of this library (the exclusion set is the
//     whole candidate list, not just the not-have subset: already-have
//     rows are outside the link game, a twin suggestion for them would
//     silently re-point a committed identity — Task 166's guard
//     generalized to the preview — and excluding them collapses the
//     wholesale-after-import pair space from 27K×27K to own×27K).
//   - barcode arm: exact equality on the pharmacy's barcodes — a Go map
//     probe per candidate, the same decisive signal the SQL arm carried.
//   - containment arms (either name contains the other, ILIKE semantics):
//     computed in Go over the in-memory candidates × own rows, each pair
//     gated by exact necessary conditions (length, then first sliding
//     trigram membership, with a trigram inverted index over the candidate
//     names for the reverse direction) before the substring check — SQL
//     cannot index «needle is a column value on the other side» without a
//     per-candidate probe storm (measured 21s for one arm); the Go pass
//     holds the whole space sub-second at every realistic scale.
//   - fuzzy arm: ONE trigram probe per OWN row (`%` prefilter at the
//     pg_trgm default threshold 0.3 purely as an index-accelerated
//     superset, exact `similarity >= minScore` recheck decides) — the
//     trigram GIN indexes have existed on global_products.name since
//     migration 17.
//
// One semantic edge, deliberate: a fuzzy pair of names too short to carry
// trigrams (< 3 chars) is no longer found — drug names never are, and exact
// short names still meet through the containment arms.
func AttachDedupMatches(ctx context.Context, db LibraryDB, pharmacyID string, candidates []LibraryCandidate, minScore float64) error {
        // مجموعة الاستبعاد = كل مرشحي المكتبة (المرتبطون بهم already_have وخارج
        // لعبة الربط)، ومجموعة العمل = المرشحون بلا صف عند الصيدلية فقط.
        excl := make([]string, 0, len(candidates))
        notHave := make([]int, 0, len(candidates))
        for i := range candidates {
                excl = append(excl, candidates[i].GlobalProductID)
                if !candidates[i].AlreadyHave {
                        notHave = append(notHave, i)
                }
        }
        if len(notHave) == 0 {
                return nil
        }
        notHaveSet := make(map[string]struct{}, len(notHave))
        for _, i := range notHave {
                notHaveSet[candidates[i].GlobalProductID] = struct{}{}
        }

        // صفوف الصيدلية الخاصة — خارج الروابط الملتزمة بأي مرشح من هذه المكتبة.
        type ownRow struct {
                id      string
                price   int64
                name    string
                barcode string
        }
        own := []ownRow{}
        // الهوية الفعالة (ميجريشن 48): التطابق يُقارن بما تعرضه الصيدلية فعلًا
        // — التعديلات المحلية أولًا والرسمي احتياطًا — فلا يُكرر منتج أعاد
        // الصيدلي تسميته محليًا.
        rows, err := db.Query(ctx, `
                SELECT pp.id::text, pp.selling_price, COALESCE(pp.name_override, gp2.name), COALESCE(pp.barcode_override, gp2.barcode, '')
                FROM pharmacy_products pp
                JOIN global_products gp2 ON gp2.id = pp.global_product_id
                WHERE pp.pharmacy_id = $1::uuid AND pp.is_active
                  AND NOT (gp2.id = ANY($2::uuid[]))
        `, pharmacyID, excl)
        if err != nil {
                return err
        }
        for rows.Next() {
                var r ownRow
                if err := rows.Scan(&r.id, &r.price, &r.name, &r.barcode); err != nil {
                        rows.Close()
                        return err
                }
                own = append(own, r)
        }
        rows.Close()
        if err := rows.Err(); err != nil {
                return err
        }

        type pair struct {
                ppID     string
                branch   int // 0 barcode, 1 containment, 2 fuzzy
                score    float64
                matchTyp string
        }
        pairsByCandidate := make(map[string][]pair)
        addPair := func(gpid, ppID string, branch int, score float64, typ string) {
                pairsByCandidate[gpid] = append(pairsByCandidate[gpid],
                        pair{ppID: ppID, branch: branch, score: score, matchTyp: typ})
        }

        // ذراعا الاحتواء في الذاكرة — نفس دلالة ILIKE (غير حساسة لحالة الأحرف)
        // مع شروط لازمة حرفية قبل مقارنة السلسلة الفرعية.
        lowered := make([]string, len(candidates))
        firstTri := make([]string, len(candidates))
        for _, i := range notHave {
                lowered[i] = strings.ToLower(candidates[i].Name)
                firstTri[i] = firstSlidingTrigram(lowered[i])
        }
        ownIndex := make(map[string]int, len(own))
        for j := range own {
                ownIndex[own[j].id] = j
        }
        if len(own) > 0 {
                // فهرس مقلوب ثلاثي فوق أسماء المرشحين — للاتجاه الذي يكون فيه اسم
                // الصيدلية هو الإبرة داخل اسم المرشح.
                inverted := map[string][]int32{}
                for _, i := range notHave {
                        for tri := range slidingTrigrams(lowered[i]) {
                                inverted[tri] = append(inverted[tri], int32(i))
                        }
                }
                for j := range own {
                        ownLower := strings.ToLower(own[j].name)
                        ownTris := slidingTrigrams(ownLower)
                        ownFirst := firstSlidingTrigram(ownLower)
                        // الاتجاه أ: اسم المرشح إبرة داخل اسم الصيدلية — أول ثلاثي للمرشح
                        // يجب أن يظهر في اسم الصيدلية (شرط لازم حرفيًا)، ومقارنة الدقيق
                        // بعد الشروط اللازمة فقط.
                        for _, i := range notHave {
                                if lowered[i] == "" || len(lowered[i]) > len(ownLower) {
                                        continue
                                }
                                if firstTri[i] != "" {
                                        if _, ok := ownTris[firstTri[i]]; !ok {
                                                continue
                                        }
                                }
                                if strings.Contains(ownLower, lowered[i]) {
                                        addPair(candidates[i].GlobalProductID, own[j].id, 1,
                                                trigramSimilarity(candidates[i].Name, own[j].name), "name_contains")
                                }
                        }
                        // الاتجاه ب: اسم الصيدلية إبرة داخل اسم المرشح — جسة الفهرس
                        // المقلوب بأول ثلاثي لاسم الصيدلية ثم مقارنة الدقيق.
                        if ownFirst != "" {
                                visited := map[int32]struct{}{}
                                for _, ci := range inverted[ownFirst] {
                                        if _, dup := visited[ci]; dup {
                                                continue
                                        }
                                        visited[ci] = struct{}{}
                                        i := int(ci)
                                        if len(ownLower) > len(lowered[i]) {
                                                continue
                                        }
                                        if strings.Contains(lowered[i], ownLower) {
                                                addPair(candidates[i].GlobalProductID, own[j].id, 1,
                                                        trigramSimilarity(candidates[i].Name, own[j].name), "name_contains")
                                        }
                                }
                        }
                }
        }

        // ذراع الباركود — خريطة باركودات الصيدلية.
        byBarcode := make(map[string]int, len(own))
        for j := range own {
                if own[j].barcode != "" {
                        if _, dup := byBarcode[own[j].barcode]; !dup {
                                byBarcode[own[j].barcode] = j
                        }
                }
        }
        for _, i := range notHave {
                if candidates[i].Barcode == "" {
                        continue
                }
                if j, ok := byBarcode[candidates[i].Barcode]; ok {
                        addPair(candidates[i].GlobalProductID, own[j].id, 0, 1.0, "barcode")
                }
        }

        // ذراع التشابه — جسة trgm واحدة لكل صف صيدلية خاص، والنتائج تُقبَل
        // للمرشحين بلا صف فقط.
        if len(own) > 0 {
                ownIDs := make([]string, 0, len(own))
                ownNames := make([]string, 0, len(own))
                for j := range own {
                        ownIDs = append(ownIDs, own[j].id)
                        ownNames = append(ownNames, own[j].name)
                }
                rows, err = db.Query(ctx, `
                        SELECT g.id::text, m.pp_id::text, similarity(g.name, m.pp_name) AS score
                        FROM unnest($2::uuid[], $3::text[]) AS m(pp_id, pp_name)
                        JOIN LATERAL (
                                SELECT g.id, g.name FROM global_products g
                                WHERE g.name % m.pp_name AND similarity(g.name, m.pp_name) >= $1
                                LIMIT 1000
                        ) g ON true
                `, minScore, ownIDs, ownNames)
                if err != nil {
                        return err
                }
                for rows.Next() {
                        var gpid, ppID string
                        var score float64
                        if err := rows.Scan(&gpid, &ppID, &score); err != nil {
                                rows.Close()
                                return err
                        }
                        if _, ok := notHaveSet[gpid]; ok {
                                addPair(gpid, ppID, 2, score, "name")
                        }
                }
                rows.Close()
                if err := rows.Err(); err != nil {
                        return err
                }
        }

        // الدمج بنفس أسبقية CASE القديم (باركود > احتواء > تشابه) والترتيب
        // الأفضل أولًا داخل كل مرشح مع سقف السلك — والمرفق على المرشحين بلا
        // صف حصرًا كما كان شكل السewire قبل Task 168.
        for _, i := range notHave {
                pairs := pairsByCandidate[candidates[i].GlobalProductID]
                if len(pairs) == 0 {
                        continue
                }
                seen := map[string]struct{}{}
                type merged struct {
                        ppID     string
                        branch   int
                        score    float64
                        matchTyp string
                }
                flat := make([]merged, 0, len(pairs))
                for _, p := range pairs {
                        if _, dup := seen[p.ppID]; dup {
                                continue
                        }
                        seen[p.ppID] = struct{}{}
                        flat = append(flat, merged{ppID: p.ppID, branch: p.branch, score: p.score, matchTyp: p.matchTyp})
                }
                sort.Slice(flat, func(a, b int) bool {
                        if flat[a].branch != flat[b].branch {
                                return flat[a].branch < flat[b].branch
                        }
                        if flat[a].score != flat[b].score {
                                return flat[a].score > flat[b].score
                        }
                        return flat[a].ppID < flat[b].ppID
                })
                if len(flat) > MatchCardinalityCap {
                        flat = flat[:MatchCardinalityCap]
                }
                ms := make([]MatchCandidate, 0, len(flat))
                suggest := false
                for _, m := range flat {
                        j := ownIndex[m.ppID]
                        ms = append(ms, MatchCandidate{
                                PharmacyProductID: own[j].id, Name: own[j].name, Barcode: own[j].barcode,
                                Price: own[j].price, Score: m.score, MatchType: m.matchTyp,
                        })
                        if m.matchTyp == "barcode" || m.matchTyp == "name_contains" || m.score >= NameAutoLinkThreshold {
                                suggest = true
                        }
                }
                candidates[i].Matches = ms
                // اقتراح الربط عند تطابق باركود قاطع أو احتواء اسم واضح أو تشابه قوي.
                if suggest {
                        candidates[i].SuggestedAction = "link_existing"
                }
        }
        return nil
}

// slidingTrigrams — the unpadded sliding trigram set of a lowercased string;
// the NECESSARY-condition vocabulary for the containment prefilters (a
// substring's sliding trigrams all appear in the superstring's stream).
func slidingTrigrams(s string) map[string]struct{} {
        out := map[string]struct{}{}
        rs := []rune(s)
        for i := 0; i+3 <= len(rs); i++ {
                out[string(rs[i:i+3])] = struct{}{}
        }
        return out
}

// firstSlidingTrigram — the first unpadded sliding trigram, empty below
// three runes.
func firstSlidingTrigram(s string) string {
        rs := []rune(s)
        if len(rs) < 3 {
                return ""
        }
        return string(rs[:3])
}

// trigramSimilarity — a Go approximation of pg_trgm's similarity() used
// only for ranking and display on the pairs the Go arms found; every
// DECISION threshold still keys on the SQL-computed score of the fuzzy arm
// or on the decisive barcode/containment match types.
func trigramSimilarity(a, b string) float64 {
        sa, sb := slidingTrigrams(strings.ToLower(a)), slidingTrigrams(strings.ToLower(b))
        if len(sa) == 0 || len(sb) == 0 {
                return 0
        }
        shared := 0
        for tri := range sa {
                if _, ok := sb[tri]; ok {
                        shared++
                }
        }
        return float64(shared) / float64(len(sa)+len(sb)-shared)
}

// ApplyPlanLimitToCandidates — البوابة 3 (المعاينة): سقف خطة المنتجات.
// العناصر التي ستنشئ صفوفًا جديدة تُحتسب بالترتيب؛ ما تجاوز السقف يُوسم
// plan_limited. Updates and LINKS are free (linking re-points an existing
// pharmacy row — no new pharmacy_products row is born), so they are never
// flagged: telling the user «upgrade to import this» about an action the
// execute gate will happily apply is a lie that blocks free work.
// Returns (used, limit) for the wire's plan block.
func ApplyPlanLimitToCandidates(candidates []LibraryCandidate, plan PlanState) (used, limit int) {
        used, limit = plan.Used, plan.Limit
        remaining := plan.remaining()
        unlimited := plan.unlimited()
        for i := range candidates {
                if candidates[i].AlreadyHave {
                        continue // تحديث سعر منتج قائم لا يستهلك السقف
                }
                if unlimited {
                        continue
                }
                if remaining <= 0 {
                        // الربط لا يولّد صفًا جديدًا فلا يستهلك السقف — وسمه plan_limited
                        // كان كذبًا يمنع عملًا مجانيًا سيطبّقه باب التنفيذ بلا مانع.
                        if candidates[i].SuggestedAction == "create_new" {
                                candidates[i].PlanLimited = true
                                candidates[i].SuggestedAction = "plan_limited"
                        }
                        continue
                }
                if candidates[i].SuggestedAction == "create_new" {
                        remaining--
                }
        }
        return used, limit
}

// ---------------------------------------------------------------------------
// التنفيذ (البوابة 3 التنفيذية) — import + one-tap pull
// ---------------------------------------------------------------------------

// PharmacyImportItem is one frozen wire item of POST
// /pharmacy/libraries/:id/import/execute.
type PharmacyImportItem struct {
        GlobalProductID   string  `json:"global_product_id"`
        Action            string  `json:"action"`
        PharmacyProductID *string `json:"pharmacy_product_id"`
}

// ImportSkip is one skipped row — ProductName is omitted from the wire when
// empty, exactly matching the old variable-key gin.H rows.
type ImportSkip struct {
        GlobalProductID string `json:"global_product_id"`
        Reason          string `json:"reason"`
        ProductName     string `json:"product_name,omitempty"`
}

// PlanRef mirrors the wire's "plan" block.
type PlanRef struct {
        ProductsUsed  int `json:"products_used"`
        ProductsLimit int `json:"products_limit"`
}

// WireSkipCap — the skip list on the wire carries at most this many rows;
// the exact total stays in the audit row and the counts, and the response
// flags the bound with skipped_truncated. A wholesale import against a
// plan-capped pharmacy can produce 26K+ skips — an unbounded body was a
// multi-megabyte response no client renders.
const WireSkipCap = 1000

// PharmacyImportOutcome mirrors the 200 body's "data" of the import.
type PharmacyImportOutcome struct {
        LibraryVersion  int          `json:"library_version"`
        Applied         int          `json:"applied"`
        ProductsCreated int          `json:"products_created"`
        ProductsLinked  int          `json:"products_linked"`
        PricesUpdated   int          `json:"prices_updated"`
        Skipped         []ImportSkip `json:"skipped"`
        Plan            PlanRef      `json:"plan"`
        // Task 168 — additive: true only when the skip list hit WireSkipCap.
        SkippedTruncated bool `json:"skipped_truncated,omitempty"`
}

// ExecutePharmacyImport — everything is re-validated server-side: link
// targets must belong to this pharmacy, «create» converts to a price update
// when the product already exists, and the plan limit is enforced for real
// inserts (skips, not failures). The sync pointer moves to the library's
// current version. Owns the transaction end to end.
//
// Task 168 (the wholesale-library lesson): the old shape issued two
// statements PER ITEM — an exists read plus the write — so importing
// 26,790 items measured 9.0s on a co-located PostgreSQL and extrapolated to
// ~27 minutes at cross-region production RTT. The rewrite keeps the loop's
// exact decision semantics (same item order, same skip reasons, same plan
// accounting, same red lines: cost and stock untouched, zero official price
// never nulls a pharmacy price) as an in-memory walk over ONE batched state
// read, then lands the decisions in three constant-count statements no
// matter how many items:
//  1. one UPDATE..FROM unnest for the already-have price refreshes,
//  2. one UPDATE..FROM unnest for the explicit links (with a collision
//     guard) plus one batched verify read that turns per-row outcomes into
//     the same link_conflict / link_target_invalid skips the per-item
//     unique-violation path produced,
//  3. one INSERT..SELECT..ON CONFLICT DO NOTHING for the creates.
//
// Interleaving is preserved: the walk mutates a virtual live view as items
// are processed, so a link that re-points a row makes a later item for that
// product take the create path exactly as the loop's re-read did.
func ExecutePharmacyImport(ctx context.Context, pool *pgxpool.Pool, principal *auth.Principal, pharmacyID, libraryID string, items []PharmacyImportItem, plan PlanState) (PharmacyImportOutcome, error) {
        out := PharmacyImportOutcome{Skipped: []ImportSkip{}, Plan: PlanRef{ProductsUsed: plan.Used, ProductsLimit: plan.Limit}}

        tx, err := pool.Begin(ctx)
        if err != nil {
                return out, &PharmacyLibrariesFailure{Stage: StagePharmacyBegin, Err: err}
        }
        defer func() { _ = tx.Rollback(ctx) }()

        // قفل المكتبة والتحقق من الرؤية داخل المعاملة.
        var libraryVersion int
        err = tx.QueryRow(ctx, `
                SELECT l.version
                FROM product_libraries l
                JOIN pharmacies ph ON ph.id = $1::uuid
                WHERE l.id = $2::uuid AND `+libraryVisibilityWhere+`
                FOR UPDATE OF l
        `, pharmacyID, libraryID).Scan(&libraryVersion)
        if errors.Is(err, pgx.ErrNoRows) {
                return out, ErrLibraryNotVisible
        }
        if err != nil {
                return out, &PharmacyLibrariesFailure{Stage: StagePharmacyQuery, Err: err}
        }

        // الأسعار الرسمية للعناصر المطلوبة (من الإصدار المنشور فقط).
        want := make(map[string]PharmacyImportItem, len(items))
        order := make([]string, 0, len(items))
        for _, it := range items {
                id := strings.TrimSpace(it.GlobalProductID)
                if id == "" {
                        continue
                }
                if _, seen := want[id]; !seen {
                        order = append(order, id)
                }
                action := it.Action
                if action != "create" && action != "link" {
                        action = "create"
                }
                it.Action = action
                want[id] = it
        }
        if len(want) == 0 {
                return out, ErrImportNoSelection
        }

        type official struct {
                price int64
                name  string
        }
        officials := make(map[string]official, len(order))
        rows, err := tx.Query(ctx, `
                SELECT lp.global_product_id::text, lp.official_price_piastres, gp.name
                FROM library_products lp
                JOIN global_products gp ON gp.id = lp.global_product_id
                WHERE lp.library_id = $1::uuid AND lp.global_product_id::text = ANY($2::text[])
        `, libraryID, order)
        if err != nil {
                return out, &PharmacyLibrariesFailure{Stage: StagePharmacyQuery, Err: err}
        }
        for rows.Next() {
                var id string
                var o official
                if err := rows.Scan(&id, &o.price, &o.name); err != nil {
                        rows.Close()
                        return out, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
                }
                officials[id] = o
        }
        rows.Close()
        if err := rows.Err(); err != nil {
                return out, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
        }

        // حالة الصيدلية على كل المعرفات المطلوبة دفعة واحدة — كانت قراءة لكل صنف.
        type liveRow struct {
                id    string
                price int64
        }
        live := make(map[string]liveRow, len(order))
        ppOldGp := make(map[string]string, len(order)) // row id → product it points at
        rows, err = tx.Query(ctx, `
                SELECT global_product_id::text, id::text, selling_price
                FROM pharmacy_products
                WHERE pharmacy_id = $1::uuid AND global_product_id = ANY($2::uuid[])
        `, pharmacyID, order)
        if err != nil {
                return out, &PharmacyLibrariesFailure{Stage: StagePharmacyQuery, Err: err}
        }
        for rows.Next() {
                var gpid, id string
                var price int64
                if err := rows.Scan(&gpid, &id, &price); err != nil {
                        rows.Close()
                        return out, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
                }
                live[gpid] = liveRow{id: id, price: price}
                ppOldGp[id] = gpid
        }
        rows.Close()
        if err := rows.Err(); err != nil {
                return out, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
        }

        // البوابة 3 (تنفيذية): سقف الخطة على الصفوف الجديدة فعليًا.
        remaining := plan.remaining()
        unlimited := plan.unlimited()

        // المسار الحتمي في الذاكرة — نفس قرارات الحلقة القديمة حرفيًا بلا أي
        // جملة SQL لكل صنف.
        skipped := []ImportSkip{}
        appliedRefresh := 0
        type linkOp struct {
                ppID, gpID string
                price      int64
        }
        linkOps := make(map[string]linkOp)
        var linkOrder []string
        var refreshGPs []string
        var refreshPrices []int64
        var createGPs []string
        var createPrices []int64

        for _, gpID := range order {
                o, inLibrary := officials[gpID]
                if !inLibrary {
                        skipped = append(skipped, ImportSkip{GlobalProductID: gpID, Reason: "not_in_library"})
                        continue
                }
                decision := want[gpID]

                if row, exists := live[gpID]; exists {
                        // موجود: حدّث سعر البيع إلى الرسمي (القاعدة الحمراء: لا لمس للتكلفة أو المخزون).
                        // سعر رسمي صفر = بيانات ناقصة في المكتبة — لا نصفر سعر الصيدلية أبدًا.
                        if row.price != o.price && o.price > 0 {
                                refreshGPs = append(refreshGPs, gpID)
                                refreshPrices = append(refreshPrices, o.price)
                        }
                        appliedRefresh++
                        continue
                }

                if decision.Action == "link" && decision.PharmacyProductID != nil {
                        // ربط صريح من المراجعة: الصف يجب أن يخص هذه الصيدلية فعلًا.
                        if o.price <= 0 {
                                skipped = append(skipped, ImportSkip{GlobalProductID: gpID, Reason: "no_official_price", ProductName: o.name})
                                continue
                        }
                        ppID := *decision.PharmacyProductID
                        // الصف نفسه قد يشابهه توأمان في مرشحين اثنين — آخر قرار يفوز
                        // كما كانت الكتابات المتتالية للحلقة تفعل بالضبط.
                        if _, seen := linkOps[ppID]; !seen {
                                linkOrder = append(linkOrder, ppID)
                        }
                        linkOps[ppID] = linkOp{ppID: ppID, gpID: gpID, price: o.price}
                        // العرض الحي: الصف يترك منتجَه القديم وينتقل لهذا المنتج.
                        if oldGp, ok := ppOldGp[ppID]; ok && oldGp != gpID {
                                delete(live, oldGp)
                        }
                        live[gpID] = liveRow{id: ppID, price: o.price}
                        ppOldGp[ppID] = gpID
                        continue
                }

                // إنشاء جديد — يخضع لسقف الخطة، ولا يُقبل بلا سعر رسمي
                // (استيراد منتجات مجهولة السعر يضر الصيدلية).
                if o.price <= 0 {
                        skipped = append(skipped, ImportSkip{GlobalProductID: gpID, Reason: "no_official_price", ProductName: o.name})
                        continue
                }
                if !unlimited {
                        if remaining <= 0 {
                                skipped = append(skipped, ImportSkip{GlobalProductID: gpID, Reason: "plan_limit", ProductName: o.name})
                                continue
                        }
                        remaining--
                }
                createGPs = append(createGPs, gpID)
                createPrices = append(createPrices, o.price)
                live[gpID] = liveRow{price: o.price} // يُولد داخل هذه الدفعة
        }

        // الكتابات المجمعة — عدد جمل ثابت مهما بلغ عدد الأصناف.
        priceUpdated := 0
        if len(refreshGPs) > 0 {
                // D3 (api_level 122): the batched refresh writes one history
                // row per changed price inside the same statement (CTE), so
                // the ledger and the change are committed atomically.
                tag, err := tx.Exec(ctx, `
                        WITH targets AS (
                                SELECT pp.id, pp.selling_price AS old_price, v.price AS new_price
                                FROM pharmacy_products pp
                                JOIN unnest($2::uuid[], $3::bigint[]) AS v(gpid, price)
                                  ON pp.pharmacy_id = $1::uuid AND pp.global_product_id = v.gpid
                                WHERE pp.selling_price <> v.price
                        ),
                        upd AS (
                                UPDATE pharmacy_products pp
                                SET selling_price = v.price, updated_at = NOW()
                                FROM unnest($2::uuid[], $3::bigint[]) AS v(gpid, price)
                                WHERE pp.pharmacy_id = $1::uuid
                                  AND pp.global_product_id = v.gpid
                                  AND pp.selling_price <> v.price
                        )
                        INSERT INTO pharmacy_product_price_history (
                                pharmacy_id, pharmacy_product_id, old_selling_price, new_selling_price, change_source
                        )
                        SELECT $1::uuid, t.id, t.old_price, t.new_price, 'library_sync' FROM targets t
                `, pharmacyID, refreshGPs, refreshPrices)
                if err != nil {
                        return out, &PharmacyLibrariesFailure{Stage: StagePharmacyWrite, Err: err}
                }
                priceUpdated = int(tag.RowsAffected())
        }

        linked := 0
        if len(linkOrder) > 0 {
                ppIDs := make([]string, 0, len(linkOrder))
                gpIDs := make([]string, 0, len(linkOrder))
                prices := make([]int64, 0, len(linkOrder))
                for _, ppID := range linkOrder {
                        op := linkOps[ppID]
                        ppIDs = append(ppIDs, ppID)
                        gpIDs = append(gpIDs, op.gpID)
                        prices = append(prices, op.price)
                }
                if _, err := tx.Exec(ctx, `
                        UPDATE pharmacy_products pp
                        SET global_product_id = v.gpid, selling_price = v.price, updated_at = NOW()
                        FROM unnest($2::uuid[], $3::uuid[], $4::bigint[]) AS v(pp_id, gpid, price)
                        WHERE pp.id = v.pp_id
                          AND pp.pharmacy_id = $1::uuid
                          AND pp.origin_type = 'central'
                          AND NOT EXISTS (
                                SELECT 1 FROM pharmacy_products q
                                WHERE q.pharmacy_id = $1::uuid
                                  AND q.global_product_id = v.gpid
                                  AND q.id <> v.pp_id
                          )
                `, pharmacyID, ppIDs, gpIDs, prices); err != nil {
                        return out, &PharmacyLibrariesFailure{Stage: StagePharmacyWrite, Err: err}
                }
                // تحقق مجمّع واحد يعيد إنتاج محاسبة الحلقة لكل صف: أُعيد توجيهه
                // (نجاح)، أو غاب عن هذه الصيدلية (link_target_invalid)، أو ظل
                // متمسكًا بمنتج آخر بعد حارس التصادم (link_conflict).
                now := make(map[string]string, len(ppIDs))
                rows, err = tx.Query(ctx, `
                        SELECT id::text, global_product_id::text
                        FROM pharmacy_products
                        WHERE id = ANY($1::uuid[])
                `, ppIDs)
                if err != nil {
                        return out, &PharmacyLibrariesFailure{Stage: StagePharmacyQuery, Err: err}
                }
                for rows.Next() {
                        var id, gpid string
                        if err := rows.Scan(&id, &gpid); err != nil {
                                rows.Close()
                                return out, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
                        }
                        now[id] = gpid
                }
                rows.Close()
                if err := rows.Err(); err != nil {
                        return out, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
                }
                for _, ppID := range linkOrder {
                        op := linkOps[ppID]
                        switch now[ppID] {
                        case op.gpID:
                                linked++
                        case "":
                                skipped = append(skipped, ImportSkip{GlobalProductID: op.gpID, Reason: "link_target_invalid"})
                        default:
                                skipped = append(skipped, ImportSkip{GlobalProductID: op.gpID, Reason: "link_conflict"})
                        }
                }
        }

        created := 0
        if len(createGPs) > 0 {
                tag, err := tx.Exec(ctx, `
                        INSERT INTO pharmacy_products (pharmacy_id, global_product_id, selling_price, cost_price, min_stock_level, source_library_id)
                        SELECT $1::uuid, v.gpid, v.price, 0, 0, $4::uuid
                        FROM unnest($2::uuid[], $3::bigint[]) AS v(gpid, price)
                        WHERE NOT EXISTS (
                                -- D4: an unlinked local twin carrying the same
                                -- scan code is the pharmacy's deliberate
                                -- ownership decision — never duplicate it under
                                -- the catalog row again.
                                SELECT 1
                                FROM pharmacy_products t
                                JOIN global_products tg ON tg.id = v.gpid
                                WHERE t.pharmacy_id = $1::uuid
                                  AND t.origin_type = 'local'
                                  AND t.barcode_override = tg.barcode
                        )
                        ON CONFLICT (pharmacy_id, global_product_id) DO NOTHING
                `, pharmacyID, createGPs, createPrices, libraryID)
                if err != nil {
                        return out, &PharmacyLibrariesFailure{Stage: StagePharmacyWrite, Err: err}
                }
                created = int(tag.RowsAffected())
        }

        skipTotal := len(skipped)
        truncated := false
        if skipTotal > WireSkipCap {
                skipped = skipped[:WireSkipCap]
                truncated = true
        }

        // تحديث مؤشر المزامنة إلى الإصدار الحالي (إن كان أقدم).
        if _, err := tx.Exec(ctx, `
                INSERT INTO pharmacy_library_syncs (pharmacy_id, library_id, last_synced_version, synced_at)
                VALUES ($1::uuid, $2::uuid, $3, NOW())
                ON CONFLICT (pharmacy_id, library_id) DO UPDATE SET
                        last_synced_version = GREATEST(pharmacy_library_syncs.last_synced_version, EXCLUDED.last_synced_version),
                        synced_at = NOW()
        `, pharmacyID, libraryID, libraryVersion); err != nil {
                return out, &PharmacyLibrariesFailure{Stage: StagePharmacyPointer, Err: err}
        }

        if err := auditlog.Tenant(ctx, tx, tenantActor(principal), pharmacyID,
                "libraries.import", "import",
                "product_library", libraryID,
                map[string]any{"library_version": libraryVersion, "created": created,
                        "linked": linked, "price_updated": priceUpdated, "skipped": skipTotal},
                "استيراد من مكتبة منتجات"); err != nil {
                return out, &PharmacyLibrariesFailure{Stage: StagePharmacyAudit, Err: err}
        }
        if err := tx.Commit(ctx); err != nil {
                return out, &PharmacyLibrariesFailure{Stage: StagePharmacyCommit, Err: err}
        }

        out.LibraryVersion = libraryVersion
        out.Applied = appliedRefresh + linked + created
        out.ProductsCreated = created
        out.ProductsLinked = linked
        out.PricesUpdated = priceUpdated
        out.Skipped = skipped
        out.SkippedTruncated = truncated
        return out, nil
}

// ExecutePharmacyImportAll — the wholesale path behind the wizard's
// «import the whole library» button: every published product of the library
// (in gp.name order — the same order the preview lists candidates) becomes
// an implicit create-or-refresh item, so the client never uploads a 27K-item
// body for a bulk import. Same transaction, same red lines, same plan gate
// as the explicit path; ExecutePharmacyImport re-validates visibility.
func ExecutePharmacyImportAll(ctx context.Context, pool *pgxpool.Pool, principal *auth.Principal, pharmacyID, libraryID string, plan PlanState) (PharmacyImportOutcome, error) {
        rows, err := pool.Query(ctx, `
                SELECT lp.global_product_id::text
                FROM product_libraries l
                JOIN pharmacies ph ON ph.id = $1::uuid
                JOIN library_products lp ON lp.library_id = l.id
                JOIN global_products gp ON gp.id = lp.global_product_id
                WHERE l.id = $2::uuid AND `+libraryVisibilityWhere+`
                ORDER BY gp.name
        `, pharmacyID, libraryID)
        if err != nil {
                return PharmacyImportOutcome{}, &PharmacyLibrariesFailure{Stage: StagePharmacyQuery, Err: err}
        }
        items := []PharmacyImportItem{}
        for rows.Next() {
                var id string
                if err := rows.Scan(&id); err != nil {
                        rows.Close()
                        return PharmacyImportOutcome{}, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
                }
                items = append(items, PharmacyImportItem{GlobalProductID: id, Action: "create"})
        }
        rows.Close()
        if err := rows.Err(); err != nil {
                return PharmacyImportOutcome{}, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
        }
        return ExecutePharmacyImport(ctx, pool, principal, pharmacyID, libraryID, items, plan)
}

// RemovedProduct mirrors the wire's "removed" rows (informational only).
type RemovedProduct struct {
        GlobalProductID string
        Name            string
}

// SyncOutcome is the service result of POST /pharmacy/libraries/:id/sync.
// NothingToDo=true means the caller is already current — the handler emits
// the shorter 200 body WITHOUT the removed/plan keys, byte-identical to the
// pre-extraction early return.
type SyncOutcome struct {
        Synced            bool
        LibraryVersion    int
        LastSyncedVersion int
        NothingToDo       bool
        PricesUpdated     int
        ProductsAdded     int
        ProductsLinked    int
        Skipped           []ImportSkip
        Removed           []RemovedProduct
        Plan              PlanRef
}

// SyncPharmacyLibrary — «اسحب التحديثات» — applies everything since the
// pharmacy's last synced version: official prices onto products the
// pharmacy already owns (cost & stock untouched), barcode/strong-name
// linking for new products (never a duplicate when a confident twin
// exists), plan-limited creations skip with a flag, and «removed» rows are
// informational only. Pointer moves to the library's current version.
//
// Task 168 (the wholesale-library lesson): the per-change loop issued three
// statements per change — a future library version refreshing thousands of
// prices would have cost a statement storm at cross-region RTT. The rewrite
// keeps the same walk semantics (same change order, same skip reasons, same
// plan accounting, same red lines) over batched statements. The twin search
// becomes ONE claim-guarded query: each pharmacy row is claimed by exactly
// its best-fitting added product (barcode first, then similarity, ties by
// product id) — the loop allowed two added products to link the SAME row
// sequentially, the second link silently overwriting the first and both
// being counted.
func SyncPharmacyLibrary(ctx context.Context, pool *pgxpool.Pool, principal *auth.Principal, pharmacyID, libraryID string, plan PlanState) (SyncOutcome, error) {
        out := SyncOutcome{Skipped: []ImportSkip{}, Plan: PlanRef{ProductsUsed: plan.Used, ProductsLimit: plan.Limit}}

        tx, err := pool.Begin(ctx)
        if err != nil {
                return out, &PharmacyLibrariesFailure{Stage: StagePharmacyBegin, Err: err}
        }
        defer func() { _ = tx.Rollback(ctx) }()

        var version, lastSynced int
        err = tx.QueryRow(ctx, `
                SELECT l.version, COALESCE(s.last_synced_version, 0)
                FROM product_libraries l
                JOIN pharmacies ph ON ph.id = $1::uuid
                LEFT JOIN pharmacy_library_syncs s ON s.library_id = l.id AND s.pharmacy_id = $1::uuid
                WHERE l.id = $2::uuid AND `+libraryVisibilityWhere+`
                FOR UPDATE OF l
        `, pharmacyID, libraryID).Scan(&version, &lastSynced)
        if errors.Is(err, pgx.ErrNoRows) {
                return out, ErrLibraryNotVisible
        }
        if err != nil {
                return out, &PharmacyLibrariesFailure{Stage: StagePharmacyQuery, Err: err}
        }
        if lastSynced == 0 {
                return out, ErrLibraryNotImported
        }
        if lastSynced >= version {
                out.Synced = true
                out.LibraryVersion = version
                out.LastSyncedVersion = lastSynced
                out.NothingToDo = true
                return out, nil
        }

        // التغييرات منذ آخر مزامنة + السعر الرسمي الحالي لكل منتج.
        type libChange struct {
                gpID       string
                changeType string
                price      int64
                name       string
                barcode    string
        }
        var changes []libChange
        rows, err := tx.Query(ctx, `
                SELECT DISTINCT ON (c.global_product_id)
                       c.global_product_id::text, c.change_type,
                       COALESCE(lp.official_price_piastres, 0), gp.name, COALESCE(gp.barcode, '')
                FROM library_changes c
                JOIN global_products gp ON gp.id = c.global_product_id
                LEFT JOIN library_products lp ON lp.library_id = c.library_id AND lp.global_product_id = c.global_product_id
                WHERE c.library_id = $1::uuid AND c.version > $2 AND c.version <= $3
                ORDER BY c.global_product_id, c.version DESC, c.created_at DESC
        `, libraryID, lastSynced, version)
        if err != nil {
                return out, &PharmacyLibrariesFailure{Stage: StagePharmacyQuery, Err: err}
        }
        for rows.Next() {
                var ch libChange
                if err := rows.Scan(&ch.gpID, &ch.changeType, &ch.price, &ch.name, &ch.barcode); err != nil {
                        rows.Close()
                        return out, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
                }
                changes = append(changes, ch)
        }
        rows.Close()
        if err := rows.Err(); err != nil {
                return out, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
        }

        // الحالة الحالية عند الصيدلية على كل المنتجات الملموسة — قراءة واحدة.
        state := make(map[string]struct {
                id    string
                price int64
        }, len(changes))
        gpIDs := make([]string, 0, len(changes))
        for _, ch := range changes {
                gpIDs = append(gpIDs, ch.gpID)
        }
        if len(gpIDs) > 0 {
                rows, err = tx.Query(ctx, `
                        SELECT global_product_id::text, id::text, selling_price
                        FROM pharmacy_products
                        WHERE pharmacy_id = $1::uuid AND global_product_id = ANY($2::uuid[])
                `, pharmacyID, gpIDs)
                if err != nil {
                        return out, &PharmacyLibrariesFailure{Stage: StagePharmacyQuery, Err: err}
                }
                for rows.Next() {
                        var gpid, id string
                        var price int64
                        if err := rows.Scan(&gpid, &id, &price); err != nil {
                                rows.Close()
                                return out, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
                        }
                        state[gpid] = struct {
                                id    string
                                price int64
                        }{id: id, price: price}
                }
                rows.Close()
                if err := rows.Err(); err != nil {
                        return out, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
                }
        }

        remaining := plan.remaining()
        unlimited := plan.unlimited()

        pricesUpdated, productsAdded, productsLinked := 0, 0, 0
        skipped := []ImportSkip{}
        removedInfo := []RemovedProduct{}

        // المسار الحتمي في الذاكرة على ترتيب التغييرات نفسه.
        changeByName := make(map[string]libChange, len(changes))
        for _, ch := range changes {
                changeByName[ch.gpID] = ch
        }
        var refreshGPs []string
        var refreshPrices []int64
        var addedName []string
        var addedBarcode []string
        var addedGP []string // بترتيب التغييرات — محاسبة السقف تتبعه

        for _, ch := range changes {
                if ch.changeType == "removed" {
                        // القاعدة الحمراء: إعلام فقط — الصيدلية تحتفظ بمنتجها.
                        removedInfo = append(removedInfo, RemovedProduct{GlobalProductID: ch.gpID, Name: ch.name})
                        continue
                }
                if row, exists := state[ch.gpID]; exists {
                        // سعر رسمي صفر = بيانات ناقصة — لا نصفر سعر الصيدلية أبدًا.
                        if row.price != ch.price && ch.price > 0 {
                                refreshGPs = append(refreshGPs, ch.gpID)
                                refreshPrices = append(refreshPrices, ch.price)
                        }
                        continue
                }
                if ch.changeType == "price_changed" || ch.changeType == "metadata_changed" {
                        // السعر تغيّر لكن الصيدلية لا تملك المنتج — لا شيء يُطبق الآن
                        // (يظهر كمنتج متاح للاستيراد من صفحة المكتبة).
                        continue
                }
                // added: مطلوب عند الصيدلية — توأم واثق أولًا وإلا إنشاء تحت السقف.
                addedGP = append(addedGP, ch.gpID)
                addedName = append(addedName, ch.name)
                addedBarcode = append(addedBarcode, ch.barcode)
        }

        // تحديث الأسعار المملوكة دفعة واحدة.
        if len(refreshGPs) > 0 {
                // D3 (api_level 122): the batched refresh writes one history
                // row per changed price inside the same statement (CTE), so
                // the ledger and the change are committed atomically.
                tag, err := tx.Exec(ctx, `
                        WITH targets AS (
                                SELECT pp.id, pp.selling_price AS old_price, v.price AS new_price
                                FROM pharmacy_products pp
                                JOIN unnest($2::uuid[], $3::bigint[]) AS v(gpid, price)
                                  ON pp.pharmacy_id = $1::uuid AND pp.global_product_id = v.gpid
                                WHERE pp.selling_price <> v.price
                        ),
                        upd AS (
                                UPDATE pharmacy_products pp
                                SET selling_price = v.price, updated_at = NOW()
                                FROM unnest($2::uuid[], $3::bigint[]) AS v(gpid, price)
                                WHERE pp.pharmacy_id = $1::uuid
                                  AND pp.global_product_id = v.gpid
                                  AND pp.selling_price <> v.price
                        )
                        INSERT INTO pharmacy_product_price_history (
                                pharmacy_id, pharmacy_product_id, old_selling_price, new_selling_price, change_source
                        )
                        SELECT $1::uuid, t.id, t.old_price, t.new_price, 'library_sync' FROM targets t
                `, pharmacyID, refreshGPs, refreshPrices)
                if err != nil {
                        return out, &PharmacyLibrariesFailure{Stage: StagePharmacyWrite, Err: err}
                }
                pricesUpdated = int(tag.RowsAffected())
        }

        // التوائم الواثقة للمنتجات المضافة — استعلام واحد بحارس المطالبة: كل صف
        // صيدلية يُطالب بأفضل منتج يناسبه فقط (الباركود أولًا ثم التشابه ثم
        // كسر التعادل بالمعرف)، فلا يُعاد توجيه الصف الواحد مرتين كما كانت
        // الحلقة تفعل صامتًا. مسار الباركود التام بلا شرط إضافي، ومسار التشابه
        // يشترط صفًا بلا باركود (درس Task 166)، و% فهارس trgm وتدقيق التشابه
        // الدقيق هو الحكم.
        type twinOp struct {
                ppID, gpID string
        }
        twins := make(map[string]twinOp) // gpID → توأمه المُطالَب به
        if len(addedGP) > 0 {
                rows, err = tx.Query(ctx, `
                        WITH want AS (
                                SELECT v.gpid, v.name, v.barcode
                                FROM unnest($2::uuid[], $3::text[], $4::text[]) AS v(gpid, name, barcode)
                        ),
                        scored AS (
                                SELECT w.gpid, pp.id AS pp_id,
                                       CASE WHEN w.barcode <> '' AND COALESCE(pp.barcode_override, gp2.barcode) = w.barcode THEN 0 ELSE 1 END AS branch,
                                       similarity(COALESCE(pp.name_override, gp2.name), w.name) AS score,
                                       row_number() OVER (
                                           PARTITION BY pp.id
                                           ORDER BY CASE WHEN w.barcode <> '' AND COALESCE(pp.barcode_override, gp2.barcode) = w.barcode THEN 0 ELSE 1 END,
                                                    similarity(COALESCE(pp.name_override, gp2.name), w.name) DESC, w.gpid
                                       ) AS claim_rank
                                FROM want w
                                JOIN pharmacy_products pp ON pp.pharmacy_id = $1::uuid AND pp.is_active
                                JOIN global_products gp2 ON gp2.id = pp.global_product_id
                                WHERE (w.barcode <> '' AND COALESCE(pp.barcode_override, gp2.barcode) = w.barcode)
                                   OR (COALESCE(pp.barcode_override, gp2.barcode, '') = ''
                                       AND (COALESCE(pp.name_override, gp2.name) % w.name) AND similarity(COALESCE(pp.name_override, gp2.name), w.name) >= $5)
                        ),
                        claimed AS (
                                SELECT gpid, pp_id, branch, score FROM scored WHERE claim_rank = 1
                        ),
                        winner AS (
                                SELECT gpid, pp_id, branch, score,
                                       row_number() OVER (PARTITION BY gpid ORDER BY branch, score DESC) AS rk
                                FROM claimed
                        )
                        SELECT gpid::text, pp_id::text, branch, score
                        FROM winner
                        WHERE rk = 1
                `, pharmacyID, addedGP, addedName, addedBarcode, NameAutoLinkThreshold)
                if err != nil {
                        return out, &PharmacyLibrariesFailure{Stage: StagePharmacyQuery, Err: err}
                }
                for rows.Next() {
                        var gpid, ppID string
                        var branch int
                        var score float64
                        if err := rows.Scan(&gpid, &ppID, &branch, &score); err != nil {
                                rows.Close()
                                return out, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
                        }
                        twins[gpid] = twinOp{ppID: ppID, gpID: gpid}
                }
                rows.Close()
                if err := rows.Err(); err != nil {
                        return out, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
                }
        }

        // الربط والإنشاء بترتيب التغييرات نفسه — الربط لا يستهلك السقف.
        var linkGPs, linkPPIDs []string
        var linkPrices []int64
        var createGPs []string
        var createPrices []int64
        for _, gpID := range addedGP {
                ch := changeByName[gpID] // السعر الرسمي للمنتج المضاف
                // الربط الواثق يسدد السعر الرسمي ولا يجري إلا بسعر رسمي موجب —
                // كما كانت الحلقة القديمة (hasTwin && price > 0) بالضبط.
                if twin, ok := twins[gpID]; ok && ch.price > 0 {
                        linkPPIDs = append(linkPPIDs, twin.ppID)
                        linkGPs = append(linkGPs, gpID)
                        linkPrices = append(linkPrices, ch.price)
                        continue
                }
                if ch.price <= 0 {
                        skipped = append(skipped, ImportSkip{GlobalProductID: gpID, Reason: "no_official_price", ProductName: ch.name})
                        continue
                }
                if !unlimited {
                        if remaining <= 0 {
                                skipped = append(skipped, ImportSkip{GlobalProductID: gpID, Reason: "plan_limit", ProductName: ch.name})
                                continue
                        }
                        remaining--
                }
                createGPs = append(createGPs, gpID)
                createPrices = append(createPrices, ch.price)
        }

        if len(linkGPs) > 0 {
                if _, err := tx.Exec(ctx, `
                        UPDATE pharmacy_products pp
                        SET global_product_id = v.gpid, selling_price = v.price, updated_at = NOW()
                        FROM unnest($2::uuid[], $3::uuid[], $4::bigint[]) AS v(pp_id, gpid, price)
                        WHERE pp.id = v.pp_id
                          AND pp.pharmacy_id = $1::uuid
                          AND pp.origin_type = 'central'
                          AND NOT EXISTS (
                                SELECT 1 FROM pharmacy_products q
                                WHERE q.pharmacy_id = $1::uuid
                                  AND q.global_product_id = v.gpid
                                  AND q.id <> v.pp_id
                          )
                `, pharmacyID, linkPPIDs, linkGPs, linkPrices); err != nil {
                        return out, &PharmacyLibrariesFailure{Stage: StagePharmacyWrite, Err: err}
                }
                now := make(map[string]string, len(linkPPIDs))
                rows, err = tx.Query(ctx, `
                        SELECT id::text, global_product_id::text
                        FROM pharmacy_products
                        WHERE id = ANY($1::uuid[])
                `, linkPPIDs)
                if err != nil {
                        return out, &PharmacyLibrariesFailure{Stage: StagePharmacyQuery, Err: err}
                }
                for rows.Next() {
                        var id, gpid string
                        if err := rows.Scan(&id, &gpid); err != nil {
                                rows.Close()
                                return out, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
                        }
                        now[id] = gpid
                }
                rows.Close()
                if err := rows.Err(); err != nil {
                        return out, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
                }
                for i, ppID := range linkPPIDs {
                        if now[ppID] == linkGPs[i] {
                                productsLinked++
                        }
                }
        }

        if len(createGPs) > 0 {
                tag, err := tx.Exec(ctx, `
                        INSERT INTO pharmacy_products (pharmacy_id, global_product_id, selling_price, cost_price, min_stock_level, source_library_id)
                        SELECT $1::uuid, v.gpid, v.price, 0, 0, $4::uuid
                        FROM unnest($2::uuid[], $3::bigint[]) AS v(gpid, price)
                        WHERE NOT EXISTS (
                                -- D4: an unlinked local twin carrying the same
                                -- scan code is the pharmacy's deliberate
                                -- ownership decision — never duplicate it under
                                -- the catalog row again.
                                SELECT 1
                                FROM pharmacy_products t
                                JOIN global_products tg ON tg.id = v.gpid
                                WHERE t.pharmacy_id = $1::uuid
                                  AND t.origin_type = 'local'
                                  AND t.barcode_override = tg.barcode
                        )
                        ON CONFLICT (pharmacy_id, global_product_id) DO NOTHING
                `, pharmacyID, createGPs, createPrices, libraryID)
                if err != nil {
                        return out, &PharmacyLibrariesFailure{Stage: StagePharmacyWrite, Err: err}
                }
                productsAdded = int(tag.RowsAffected())
        }

        if _, err := tx.Exec(ctx, `
                UPDATE pharmacy_library_syncs SET last_synced_version = $3, synced_at = NOW()
                WHERE pharmacy_id = $1::uuid AND library_id = $2::uuid
        `, pharmacyID, libraryID, version); err != nil {
                return out, &PharmacyLibrariesFailure{Stage: StagePharmacyPointer, Err: err}
        }

        if err := auditlog.Tenant(ctx, tx, tenantActor(principal), pharmacyID,
                "libraries.sync", "update",
                "product_library", libraryID,
                map[string]any{"from_version": lastSynced, "to_version": version,
                        "prices_updated": pricesUpdated, "products_added": productsAdded,
                        "products_linked": productsLinked, "skipped": len(skipped), "removed": len(removedInfo)},
                "سحب تحديثات مكتبة منتجات"); err != nil {
                return out, &PharmacyLibrariesFailure{Stage: StagePharmacyAudit, Err: err}
        }
        if err := tx.Commit(ctx); err != nil {
                return out, &PharmacyLibrariesFailure{Stage: StagePharmacyCommit, Err: err}
        }

        out.Synced = true
        out.LibraryVersion = version
        out.LastSyncedVersion = lastSynced
        out.NothingToDo = false
        out.PricesUpdated = pricesUpdated
        out.ProductsAdded = productsAdded
        out.ProductsLinked = productsLinked
        out.Skipped = skipped
        out.Removed = removedInfo
        return out, nil
}

// trimSpace is strings.TrimSpace under a local alias (kept for parity with
// the moved code's call sites).
func trimSpace(s string) string {
        b := []byte(s)
        for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\n' || b[0] == '\r') {
                b = b[1:]
        }
        for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t' || b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
                b = b[:len(b)-1]
        }
        return string(b)
}

// ---------------------------------------------------------------------------
// إزالة المكتبة من الصيدلية — «المكتبة مرجع، قاعدة المنتجات ملك الصيدلية»
// (api_level 117 / migration 48). ADR-0005: the transactions live here, the
// handlers stay thin.
// ---------------------------------------------------------------------------

// LibraryRemovalProtectedWhere is the ownership-protection predicate. A
// product the library CREATED for this pharmacy is kept when ANY operational
// trace exists on it — sales, customer returns, or stock batches (which
// subsume every stock movement, since movements hang on batches). The
// database enforces the hard core of this contract already:
// sale_items/sale_return_items reference pharmacy_products with ON DELETE
// RESTRICT, so a referenced row physically cannot be deleted — this
// predicate surfaces the survivors BEFORE anything is touched, so the
// pharmacist sees honest numbers instead of an opaque failure.
const LibraryRemovalProtectedWhere = `
                EXISTS (SELECT 1 FROM sale_items si WHERE si.pharmacy_product_id = pp.id)
                OR EXISTS (SELECT 1 FROM sale_return_items ri WHERE ri.pharmacy_product_id = pp.id)
                OR EXISTS (SELECT 1 FROM inventory_batches ib WHERE ib.pharmacy_product_id = pp.id)`

// LibraryRemovalItem is one product row of a removal preview.
type LibraryRemovalItem struct {
        PharmacyProductID string `json:"pharmacy_product_id"`
        Name              string `json:"name"`
        Barcode           string `json:"barcode"`
        SellingPrice      int64  `json:"selling_price_piastres"`
}

// LibraryRemovalSide is one bucket of the honest preview.
type LibraryRemovalSide struct {
        Count int64                `json:"count"`
        Items []LibraryRemovalItem `json:"items"`
}

// LibraryRemovalPreview is what removal would do, BEFORE anything moves.
type LibraryRemovalPreview struct {
        LibraryID   string             `json:"-"`
        LibraryName string             `json:"-"`
        WillDelete  LibraryRemovalSide `json:"will_delete"`
        WillKeep    LibraryRemovalSide `json:"will_keep"`
}

// libraryRemovalSide reads one bucket, capped list + honest total (the list
// preview caps at 200 rows; the count is always the real number).
func libraryRemovalSide(ctx context.Context, db LibraryDB, pharmacyID, libraryID, where string) (LibraryRemovalSide, error) {
        rows, err := db.Query(ctx, `
                SELECT pp.id::text, COALESCE(pp.name_override, gp.name::text, ''),
                       COALESCE(pp.barcode_override, gp.barcode::text, ''),
                       pp.selling_price::int8
                FROM pharmacy_products pp
                JOIN global_products gp ON gp.id = pp.global_product_id
                WHERE pp.pharmacy_id = $1::uuid
                  AND pp.source_library_id = $2::uuid
                  AND (`+where+`)
                ORDER BY 2
                LIMIT 200
        `, pharmacyID, libraryID)
        if err != nil {
                return LibraryRemovalSide{}, &PharmacyLibrariesFailure{Stage: StagePharmacyQuery, Err: err}
        }
        defer rows.Close()
        side := LibraryRemovalSide{Items: []LibraryRemovalItem{}}
        for rows.Next() {
                var it LibraryRemovalItem
                if err := rows.Scan(&it.PharmacyProductID, &it.Name, &it.Barcode, &it.SellingPrice); err != nil {
                        rows.Close()
                        return LibraryRemovalSide{}, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
                }
                side.Items = append(side.Items, it)
        }
        if err := rows.Err(); err != nil {
                return LibraryRemovalSide{}, &PharmacyLibrariesFailure{Stage: StagePharmacyScan, Err: err}
        }
        rows.Close()
        side.Count = int64(len(side.Items))
        if side.Count == 200 {
                // The preview list is capped; the count below is the honest total.
                if err := db.QueryRow(ctx, `
                        SELECT COUNT(*)
                        FROM pharmacy_products pp
                        WHERE pp.pharmacy_id = $1::uuid
                          AND pp.source_library_id = $2::uuid
                          AND (`+where+`)
                `, pharmacyID, libraryID).Scan(&side.Count); err != nil {
                        return LibraryRemovalSide{}, &PharmacyLibrariesFailure{Stage: StagePharmacyCount, Err: err}
                }
        }
        return side, nil
}

// PreviewPharmacyLibraryRemoval classifies the products the library CREATED
// for this pharmacy (pharmacy_products.source_library_id): what a removal
// would DELETE (zero operational traces) and what it would KEEP (sales /
// returns / stock — detached from the library, fully owned). Nothing is
// mutated.
func PreviewPharmacyLibraryRemoval(ctx context.Context, pool *pgxpool.Pool, pharmacyID, libraryID string) (LibraryRemovalPreview, error) {
        var name string
        err := pool.QueryRow(ctx, `SELECT l.name FROM product_libraries l WHERE l.id = $1::uuid`, libraryID).Scan(&name)
        if errors.Is(err, pgx.ErrNoRows) {
                return LibraryRemovalPreview{}, ErrLibraryNotFound
        }
        if err != nil {
                return LibraryRemovalPreview{}, &PharmacyLibrariesFailure{Stage: StagePharmacyQuery, Err: err}
        }
        out := LibraryRemovalPreview{LibraryID: libraryID, LibraryName: name}
        out.WillKeep, err = libraryRemovalSide(ctx, pool, pharmacyID, libraryID, LibraryRemovalProtectedWhere)
        if err != nil {
                return LibraryRemovalPreview{}, err
        }
        out.WillDelete, err = libraryRemovalSide(ctx, pool, pharmacyID, libraryID, `NOT (`+LibraryRemovalProtectedWhere+`)`)
        if err != nil {
                return LibraryRemovalPreview{}, err
        }
        return out, nil
}

// LibraryRemovalResult is the honest execution summary.
type LibraryRemovalResult struct {
        LibraryName     string `json:"library_name"`
        ProductsDeleted int64  `json:"products_deleted"`
        ProductsKept    int64  `json:"products_kept"`
}

// ExecutePharmacyLibraryRemoval runs one transaction that re-classifies
// INSIDE it (the preview's numbers may have aged): delete the trace-free
// products the library created, detach the protected survivors
// (source_library_id = NULL — they remain ordinary, fully-owned pharmacy
// products), and drop the sync pointer so the library reads as «not
// imported» again. The shared catalog (global_products) is untouched —
// other pharmacies and future imports are unaffected. Clean rows have no
// batches and no sale references, so nothing cascades and no sync
// tombstones are needed (the mobile delta-sync tracks batches only).
func ExecutePharmacyLibraryRemoval(ctx context.Context, pool *pgxpool.Pool, principal *auth.Principal, pharmacyID, libraryID string) (LibraryRemovalResult, error) {
        tx, err := pool.Begin(ctx)
        if err != nil {
                return LibraryRemovalResult{}, &PharmacyLibrariesFailure{Stage: StagePharmacyBegin, Err: err}
        }
        defer func() { _ = tx.Rollback(ctx) }()

        var name string
        err = tx.QueryRow(ctx, `
                SELECT l.name FROM product_libraries l WHERE l.id = $1::uuid FOR UPDATE OF l
        `, libraryID).Scan(&name)
        if errors.Is(err, pgx.ErrNoRows) {
                return LibraryRemovalResult{}, ErrLibraryNotFound
        }
        if err != nil {
                return LibraryRemovalResult{}, &PharmacyLibrariesFailure{Stage: StagePharmacyQuery, Err: err}
        }

        // Protected survivors first: detach from the library, keep the rows.
        // Their provenance disappears — a later re-import of the same library
        // treats them as «already have» price refreshes, never duplicates.
        tag, err := tx.Exec(ctx, `
                UPDATE pharmacy_products pp
                SET source_library_id = NULL, updated_at = NOW()
                WHERE pp.pharmacy_id = $1::uuid
                  AND pp.source_library_id = $2::uuid
                  AND (`+LibraryRemovalProtectedWhere+`)
        `, pharmacyID, libraryID)
        if err != nil {
                return LibraryRemovalResult{}, &PharmacyLibrariesFailure{Stage: StagePharmacyWrite, Err: err}
        }
        kept := tag.RowsAffected()

        // Trace-free rows: delete outright.
        tag, err = tx.Exec(ctx, `
                DELETE FROM pharmacy_products pp
                WHERE pp.pharmacy_id = $1::uuid
                  AND pp.source_library_id = $2::uuid
                  AND NOT (`+LibraryRemovalProtectedWhere+`)
        `, pharmacyID, libraryID)
        if err != nil {
                return LibraryRemovalResult{}, &PharmacyLibrariesFailure{Stage: StagePharmacyWrite, Err: err}
        }
        deleted := tag.RowsAffected()

        // The library itself leaves the pharmacy's library list.
        if _, err := tx.Exec(ctx, `
                DELETE FROM pharmacy_library_syncs
                WHERE pharmacy_id = $1::uuid AND library_id = $2::uuid
        `, pharmacyID, libraryID); err != nil {
                return LibraryRemovalResult{}, &PharmacyLibrariesFailure{Stage: StagePharmacyWrite, Err: err}
        }

        if err := auditlog.Tenant(ctx, tx, tenantActor(principal), pharmacyID,
                "libraries.remove", "delete",
                "product_library", libraryID,
                map[string]any{"library_name": name, "products_deleted": deleted, "products_kept": kept},
                "إزالة مكتبة منتجات من الصيدلية: "+name); err != nil {
                return LibraryRemovalResult{}, &PharmacyLibrariesFailure{Stage: StagePharmacyAudit, Err: err}
        }
        if err := tx.Commit(ctx); err != nil {
                return LibraryRemovalResult{}, &PharmacyLibrariesFailure{Stage: StagePharmacyCommit, Err: err}
        }
        return LibraryRemovalResult{LibraryName: name, ProductsDeleted: deleted, ProductsKept: kept}, nil
}

// ---------------------------------------------------------------------------
// الاستيراد الخلفي (api_level 118) — eligibility precheck for the async
// wholesale import and the run-status reader the wizard polls. The run rows
// themselves are written by the jobs worker (internal/jobs); this file owns
// the reads the wire serves.
// ---------------------------------------------------------------------------

// PharmacyImportEligible — the fast synchronous answer to «can this pharmacy
// run the wholesale import of this library at all?»: the library must be
// visible to the pharmacy (published + country), exactly the gate
// ExecutePharmacyImportAll re-validates inside its own transaction when the
// job actually runs. A 404 now beats a 404 four seconds later from a worker.
func PharmacyImportEligible(ctx context.Context, pool *pgxpool.Pool, pharmacyID, libraryID string) error {
        var one int
        err := pool.QueryRow(ctx, `
                SELECT 1
                FROM product_libraries l
                JOIN pharmacies ph ON ph.id = $1::uuid
                WHERE l.id = $2::uuid AND `+libraryVisibilityWhere, pharmacyID, libraryID).Scan(&one)
        if errors.Is(err, pgx.ErrNoRows) {
                return ErrLibraryNotVisible
        }
        if err != nil {
                return &PharmacyLibrariesFailure{Stage: StagePharmacyQuery, Err: err}
        }
        return nil
}

// LibraryImportRunStatus is the wire of GET /pharmacy/libraries/:id/import/
// status — the latest background run for this pharmacy+library, whatever its
// state. State 'queued' means the job exists but has not been picked up;
// 'running' an attempt is executing (including waiting on a retry);
// 'succeeded'/'failed' are terminal.
type LibraryImportRunStatus struct {
        RunID           int64      `json:"run_id"`
        State           string     `json:"state"`
        LibraryVersion  int        `json:"library_version"`
        ProductsCreated int        `json:"products_created"`
        ProductsLinked  int        `json:"products_linked"`
        PricesUpdated   int        `json:"prices_updated"`
        SkippedCount    int        `json:"skipped_count"`
        ErrorMessage    string     `json:"error_message,omitempty"`
        QueuedAt        time.Time  `json:"queued_at"`
        StartedAt       *time.Time `json:"started_at,omitempty"`
        FinishedAt      *time.Time `json:"finished_at,omitempty"`
}

// LatestLibraryImportRun returns the newest run row for the pair, or nil
// when background processing never queued one (the wizard then knows the
// run it is looking at is not there — e.g. after a deploy that lost the
// river row).
func LatestLibraryImportRun(ctx context.Context, pool *pgxpool.Pool, pharmacyID, libraryID string) (*LibraryImportRunStatus, error) {
        var s LibraryImportRunStatus
        var errMsg *string
        err := pool.QueryRow(ctx, `
                SELECT id, state, library_version, products_created, products_linked, prices_updated,
                       skipped_count, error_message, queued_at, started_at, finished_at
                FROM library_import_runs
                WHERE pharmacy_id = $1::uuid AND library_id = $2::uuid
                ORDER BY id DESC
                LIMIT 1
        `, pharmacyID, libraryID).Scan(
                &s.RunID, &s.State, &s.LibraryVersion, &s.ProductsCreated, &s.ProductsLinked, &s.PricesUpdated,
                &s.SkippedCount, &errMsg, &s.QueuedAt, &s.StartedAt, &s.FinishedAt)
        if errors.Is(err, pgx.ErrNoRows) {
                return nil, nil
        }
        if err != nil {
                return nil, &PharmacyLibrariesFailure{Stage: StagePharmacyQuery, Err: err}
        }
        if errMsg != nil {
                s.ErrorMessage = *errMsg
        }
        return &s, nil
}
