// ADR-0005 (api_level 110) — ledger item 3 (platform half): the central
// library ENTRY surface (official price per product), the Excel/CSV price
// bulletin import, and the catalog browser/editor move out of
// platform_libraries_products_handler.go under the layering policy. The
// code is moved, not rewritten: same statements, same lock ordering
// (per-row FOR UPDATE with the unique constraint as the race backstop),
// same barcode-first/name-second resolution, same change-log semantics,
// same audit events — the handler maps each typed error 1:1 onto the exact
// wire body it emitted before (status, error code, Arabic message, keys).
// The wire contract is frozen during extraction (ADR-0005 rule 3): mobile
// APKs are not exported from this repo and a stale install must keep
// working — any "small" wire drift here is a pharmacy session outage
// tomorrow.
//
// Ownership split (ADR-0005 rule 2): THIS package owns the transactions
// (BEGIN/COMMIT/ROLLBACK), the per-row locks, the catalog dedup (barcode
// index, normalized-name index, in-file dedupe), the draft-version tagging,
// the metadata_changed propagation, and every audit row. The handler keeps
// protocol only: multipart parsing, column-mapping suggestion, row-shape
// validation (the pure Arabic-reason validator), page parsing, HTTP
// statuses and response bodies.
//
// House rules honored here (unchanged):
//   - the OFFICIAL price lives on library_products (per library/currency),
//     never on global_products,
//   - every catalog/price mutation appends library_changes tagged with the
//     draft version (semantics documented in platform_libraries_handler.go),
//   - every write is audited to platform_audit_logs (auditlog.Platform —
//     the single canonical writer),
//   - import price values are major units ×100 (piastres); currencies with
//     3 decimals need their own divisor before those libraries go live —
//     Phase 1 activates EGP only.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pharmacy-os/backend/internal/auditlog"
	"github.com/pharmacy-os/backend/internal/auth"
	"github.com/pharmacy-os/backend/internal/barcode"
)

// LibraryDB is the smallest surface both *pgxpool.Pool and pgx.Tx already
// satisfy, so the shared tx-scoped helpers (LibraryDraftVersion,
// LogLibraryChange, TouchLibrary) and the pool-scoped reads take one type.
type LibraryDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// libraryUniqueViolation mirrors handlers.isUniqueViolation (23505): the
// catalog dedup (barcode collision → link instead of fail) and the sync
// link conflict both key on it.
func libraryUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// strFieldChanged reports whether a nullable-string payload field mutates
// the stored value under the UPDATE's COALESCE semantics: nil means «leave
// unchanged», a non-nil value differs when the stored column is NULL or
// holds a different string.
func strFieldChanged(new, old *string) bool {
	return new != nil && (old == nil || *new != *old)
}

// ---------------------------------------------------------------------------
// Shared tx-scoped helpers — moved verbatim from
// platform_libraries_handler.go where they were defined for the products
// file. The libraries CRUD handler file (its only other resident) no longer
// calls them; nothing else in handlers does.
// ---------------------------------------------------------------------------

// LibraryDraftVersion returns the version number new edits should be tagged
// with, locking the library row FOR UPDATE so concurrent publishers
// serialize.
func LibraryDraftVersion(ctx context.Context, db LibraryDB, libraryID string) (int, bool, error) {
	var version int
	var published bool
	err := db.QueryRow(ctx,
		`SELECT version, is_published FROM product_libraries WHERE id = $1::uuid FOR UPDATE`,
		libraryID,
	).Scan(&version, &published)
	if err != nil {
		return 0, false, err
	}
	if !published {
		return version, false, nil
	}
	return version + 1, true, nil
}

// LogLibraryChange appends one change-log row tagged with the version the
// change belongs to. oldP/newP are nil for non-price changes.
func LogLibraryChange(ctx context.Context, db LibraryDB, libraryID, globalProductID string, version int, changeType string, oldP, newP *int64, summary string) error {
	_, err := db.Exec(ctx, `
                INSERT INTO library_changes
                        (library_id, global_product_id, version, change_type, old_price_piastres, new_price_piastres, summary)
                VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, NULLIF($7, ''))
        `, libraryID, globalProductID, version, changeType, oldP, newP, summary)
	return err
}

// TouchLibrary keeps updated_at honest for UI sorting.
func TouchLibrary(ctx context.Context, db LibraryDB, libraryID string) error {
	_, err := db.Exec(ctx, `UPDATE product_libraries SET updated_at = NOW() WHERE id = $1::uuid`, libraryID)
	return err
}

// DrawInternalBarcode reads the next platform sequence value and builds the
// complete RCN EAN-13 for it (moved verbatim from handlers/barcode_handler.go
// — its four remaining handler callers are repointed mechanically). nextval
// is atomic; a rolled-back transaction leaves a gap in the sequence, which
// is fine — gaps are invisible because every value is used at most once.
func DrawInternalBarcode(ctx context.Context, tx pgx.Tx) (string, error) {
	var seq int64
	if err := tx.QueryRow(ctx, `SELECT nextval('product_internal_barcode_seq')`).Scan(&seq); err != nil {
		return "", err
	}
	return barcode.BuildRCNEAN13(seq)
}

// ---------------------------------------------------------------------------
// Typed rejections and failure stages — the handler maps each 1:1 onto the
// pre-extraction wire bodies. Do not merge sentinels that carry different
// bodies; do not split sentinels that share one.
// ---------------------------------------------------------------------------

var (
	// ErrLibraryNotFound: the library id does not exist — 404
	// library_not_found.
	ErrLibraryNotFound = errors.New("library not found")
	// ErrCatalogProductNotFound: the catalog id does not exist — 404
	// catalog_product_not_found.
	ErrCatalogProductNotFound = errors.New("catalog product not found")
	// ErrLibraryProductNotFound: the library entry (id, library) pair does
	// not exist — 404 library_product_not_found.
	ErrLibraryProductNotFound = errors.New("library product not found")
	// ErrBarcodeTaken: a catalog update collided with an existing barcode —
	// 409 barcode_taken.
	ErrBarcodeTaken = errors.New("barcode already registered to another product")
)

// LibraryStage identifies where inside a library flow a failure happened,
// so the handler can emit the same stage-specific error code the wire
// served before the extraction (e.g. library_products_count_failed vs
// library_products_query_failed vs library_products_scan_failed).
type LibraryStage string

// Stage vocabulary — every value maps onto exactly one pre-extraction wire
// code in the handler's respondLibraryFailure.
const (
	StageTxBegin       LibraryStage = "tx_begin"
	StageCount         LibraryStage = "count"
	StageQuery         LibraryStage = "query"
	StageScan          LibraryStage = "scan"
	StageCatalogCheck  LibraryStage = "catalog_check"
	StageCatalogCreate LibraryStage = "catalog_create"
	StageUpsert        LibraryStage = "upsert"
	StageUpdate        LibraryStage = "update"
	StageDelete        LibraryStage = "delete"
	StageChangeLog     LibraryStage = "change_log"
	StageTouch         LibraryStage = "touch"
	StageAudit         LibraryStage = "audit"
	StageTxCommit      LibraryStage = "tx_commit"
	StageBarcodeIndex  LibraryStage = "barcode_index"
	StageNameIndex     LibraryStage = "name_index"
	StageMembership    LibraryStage = "membership"
	StagePropagation   LibraryStage = "propagation"
	StageDraftVersion  LibraryStage = "draft_version"
)

// LibrariesFailure wraps a stage with its underlying error. The handler's
// vocabulary maps Stage → wire code; the wrapped error is logged, never
// leaked to the wire (R-14).
type LibrariesFailure struct {
	Stage LibraryStage
	Err   error
}

func (e *LibrariesFailure) Error() string {
	return fmt.Sprintf("libraries: stage %s: %v", e.Stage, e.Err)
}

func (e *LibrariesFailure) Unwrap() error { return e.Err }

// platformActor unwraps a platform principal into the canonical auditlog
// identity (same fields writePlatformAuditLog used).
func platformActor(principal *auth.Principal) auditlog.Actor {
	if principal == nil {
		return auditlog.Actor{}
	}
	return auditlog.Actor{
		ID:          principal.ID,
		Email:       principal.Email,
		DisplayName: principal.DisplayName,
		Role:        principal.Role,
	}
}

// ---------------------------------------------------------------------------
// Library products (the entry surface)
// ---------------------------------------------------------------------------

// LibraryProductRow mirrors one row of the GET
// /platform-admin/libraries/:id/products page — json tags are the frozen
// wire vocabulary; the handler embeds these verbatim.
type LibraryProductRow struct {
	ID                    string    `json:"id"`
	GlobalProductID       string    `json:"global_product_id"`
	OfficialPricePiastres int64     `json:"official_price_piastres"`
	Notes                 *string   `json:"notes"`
	Name                  string    `json:"name"`
	GenericName           *string   `json:"generic_name"`
	Strength              *string   `json:"strength"`
	DosageForm            string    `json:"dosage_form"`
	ProductCategory       string    `json:"product_category"`
	Barcode               *string   `json:"barcode"`
	ManufacturerName      *string   `json:"manufacturer_name"`
	ActiveIngredient      *string   `json:"active_ingredient"`
	RequiresPrescription  string    `json:"requires_prescription"`
	IsVerified            bool      `json:"is_verified"`
	UpdatedAt             time.Time `json:"updated_at"`
}

const libraryProductSelectBody = `
        SELECT lp.id::text, lp.global_product_id::text, lp.official_price_piastres, lp.notes,
               gp.name, gp.generic_name, gp.strength, gp.dosage_form::text, gp.product_category::text,
               gp.barcode, gp.manufacturer_name, gp.active_ingredient, gp.requires_prescription::text,
               gp.is_verified, lp.updated_at
        FROM library_products lp
        JOIN global_products gp ON gp.id = lp.global_product_id
        WHERE lp.library_id = $1::uuid
`

// ListLibraryProducts serves GET /platform-admin/libraries/:id/products —
// stable placeholder positions ($1 library, $2 search empty = match all,
// $3 page size, $4 offset; count and page get their own slices because
// Postgres rejects parameters a statement never references).
func ListLibraryProducts(ctx context.Context, db LibraryDB, libraryID, search string, page, pageSize int) ([]LibraryProductRow, int64, error) {
	where := ` AND ($2 = '' OR gp.name ILIKE '%' || $2 || '%' OR gp.generic_name ILIKE '%' || $2 || '%' OR gp.barcode ILIKE $2 || '%')`
	searchArg := escapeLikeValue(search)

	var total int64
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM library_products lp JOIN global_products gp ON gp.id = lp.global_product_id WHERE lp.library_id = $1::uuid`+where,
		libraryID, searchArg,
	).Scan(&total); err != nil {
		return nil, 0, &LibrariesFailure{Stage: StageCount, Err: err}
	}

	rows, err := db.Query(ctx, libraryProductSelectBody+where+`
                ORDER BY gp.name LIMIT $3 OFFSET $4`,
		libraryID, searchArg, pageSize, (page-1)*pageSize,
	)
	if err != nil {
		return nil, 0, &LibrariesFailure{Stage: StageQuery, Err: err}
	}
	defer rows.Close()

	items := []LibraryProductRow{}
	for rows.Next() {
		var r LibraryProductRow
		if err := rows.Scan(&r.ID, &r.GlobalProductID, &r.OfficialPricePiastres, &r.Notes,
			&r.Name, &r.GenericName, &r.Strength, &r.DosageForm, &r.ProductCategory,
			&r.Barcode, &r.ManufacturerName, &r.ActiveIngredient, &r.RequiresPrescription,
			&r.IsVerified, &r.UpdatedAt); err != nil {
			return nil, 0, &LibrariesFailure{Stage: StageScan, Err: err}
		}
		items = append(items, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, &LibrariesFailure{Stage: StageScan, Err: err}
	}
	return items, total, nil
}

// escapeLikeValue escapes LIKE metacharacters — the services-side twin of
// handlers.escapeLike (byte-identical escaping, kept local so services
// never imports handlers).
func escapeLikeValue(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}

// UpsertLibraryEntry adds the product to the library or updates its
// official price, appending the right change-log rows. Returns what
// happened. Takes a per-row FOR UPDATE lock; the unique constraint
// backstops any insert race.
func UpsertLibraryEntry(ctx context.Context, db LibraryDB, libraryID, gpID string, pricePiastres int64, hasPrice bool, notes *string, draftVersion int) (action string, err error) {
	var (
		entryID  string
		currentP int64
	)
	err = db.QueryRow(ctx, `
                SELECT id::text, official_price_piastres FROM library_products
                WHERE library_id = $1::uuid AND global_product_id = $2::uuid FOR UPDATE
        `, libraryID, gpID).Scan(&entryID, &currentP)
	if errors.Is(err, pgx.ErrNoRows) {
		p := int64(0)
		if hasPrice {
			p = pricePiastres
		}
		if _, err := db.Exec(ctx, `
                        INSERT INTO library_products (library_id, global_product_id, official_price_piastres, notes)
                        VALUES ($1::uuid, $2::uuid, $3, $4)
                `, libraryID, gpID, p, notes); err != nil {
			return "", err
		}
		if err := LogLibraryChange(ctx, db, libraryID, gpID, draftVersion, "added", nil, &p, "إضافة منتج إلى المكتبة"); err != nil {
			return "", err
		}
		return "added", nil
	}
	if err != nil {
		return "", err
	}
	if hasPrice && pricePiastres != currentP {
		if _, err := db.Exec(ctx,
			`UPDATE library_products SET official_price_piastres = $3, notes = COALESCE($4, notes), updated_at = NOW() WHERE id = $1::uuid AND library_id = $2::uuid`,
			entryID, libraryID, pricePiastres, notes); err != nil {
			return "", err
		}
		if err := LogLibraryChange(ctx, db, libraryID, gpID, draftVersion, "price_changed",
			&currentP, &pricePiastres, "تحديث السعر الرسمي"); err != nil {
			return "", err
		}
		return "price_updated", nil
	}
	if notes != nil {
		if _, err := db.Exec(ctx,
			`UPDATE library_products SET notes = $3, updated_at = NOW() WHERE id = $1::uuid AND library_id = $2::uuid`,
			entryID, libraryID, notes); err != nil {
			return "", err
		}
	}
	return "unchanged", nil
}

// ---------------------------------------------------------------------------
// Catalog product creation (the verified platform-admin path)
// ---------------------------------------------------------------------------

// NewCatalogProduct — إنشاء دواء جديد في الكتالوج المركزي من محرر المسؤول.
// Admin-created products are born verified: the platform is the source of
// truth for curated library content. The json tags are the frozen wire
// vocabulary — the protocol edge binds this struct directly.
type NewCatalogProduct struct {
	Name                 string `json:"name"`
	GenericName          string `json:"generic_name"`
	BrandName            string `json:"brand_name"`
	DosageForm           string `json:"dosage_form"`
	Strength             string `json:"strength"`
	ProductCategory      string `json:"product_category"`
	RequiresPrescription string `json:"requires_prescription"`
	Barcode              string `json:"barcode"`
	GenerateBarcode      bool   `json:"generate_barcode"`
	ManufacturerName     string `json:"manufacturer_name"`
	ManufacturerCountry  string `json:"manufacturer_country"`
	ActiveIngredient     string `json:"active_ingredient"`
	AtcCode              string `json:"atc_code"`
	TherapeuticClass     string `json:"therapeutic_class"`
	StorageInstructions  string `json:"storage_instructions"`
	Description          string `json:"description"`
}

// ValidDosageForms / ValidProductCategories — the enum whitelist the
// protocol edge validates against; exported so the catalog edit handler and
// the import validator share ONE definition (moved verbatim).
var ValidDosageForms = map[string]bool{
	"tablet": true, "capsule": true, "syrup": true, "drop": true, "injection": true,
	"ointment": true, "cream": true, "gel": true, "powder": true, "solution": true,
	"suspension": true, "inhaler": true, "patch": true, "suppository": true,
	"eye_drops": true, "ear_drops": true, "nasal_spray": true, "other": true,
}

var ValidProductCategories = map[string]bool{
	"medication": true, "supplement": true, "medical_device": true, "personal_care": true,
	"cosmetic": true, "food_supplement": true, "herbal": true, "vaccine": true,
	"consumable": true, "other": true,
}

// Normalize trims every free-form field (moved verbatim).
func (n *NewCatalogProduct) Normalize() {
	n.Name = strings.TrimSpace(n.Name)
	n.GenericName = strings.TrimSpace(n.GenericName)
	n.BrandName = strings.TrimSpace(n.BrandName)
	n.DosageForm = strings.TrimSpace(n.DosageForm)
	n.Strength = strings.TrimSpace(n.Strength)
	n.ProductCategory = strings.TrimSpace(n.ProductCategory)
	n.RequiresPrescription = strings.TrimSpace(n.RequiresPrescription)
	n.Barcode = strings.TrimSpace(n.Barcode)
	n.ManufacturerName = strings.TrimSpace(n.ManufacturerName)
	n.ManufacturerCountry = strings.TrimSpace(n.ManufacturerCountry)
	n.ActiveIngredient = strings.TrimSpace(n.ActiveIngredient)
	n.AtcCode = strings.TrimSpace(n.AtcCode)
	n.TherapeuticClass = strings.TrimSpace(n.TherapeuticClass)
}

// Validate returns an Arabic rejection reason or "" (moved verbatim).
func (n *NewCatalogProduct) Validate() string {
	if n.Name == "" {
		return "اسم المنتج مطلوب"
	}
	if n.DosageForm == "" {
		n.DosageForm = "tablet"
	}
	if !ValidDosageForms[n.DosageForm] {
		return "الشكل الصيدلي غير معروف"
	}
	if n.ProductCategory == "" {
		n.ProductCategory = "medication"
	}
	if !ValidProductCategories[n.ProductCategory] {
		return "تصنيف المنتج غير معروف"
	}
	if n.RequiresPrescription == "" {
		n.RequiresPrescription = "no"
	}
	if n.RequiresPrescription != "yes" && n.RequiresPrescription != "no" && n.RequiresPrescription != "otc_only" {
		return "قيمة وصفة الطبيب يجب أن تكون: yes أو no أو otc_only"
	}
	return ""
}

// CreateCatalogProduct inserts a global_products row inside the caller's
// transaction (source=platform_admin, verified) with optional RCN barcode
// generation. Barcode collision → link to the existing product instead of
// failing — the catalog stays deduplicated by barcode.
func CreateCatalogProduct(ctx context.Context, tx pgx.Tx, p *NewCatalogProduct) (string, error) {
	barcodeValue := p.Barcode
	if barcodeValue == "" && p.GenerateBarcode {
		code, err := DrawInternalBarcode(ctx, tx)
		if err != nil {
			return "", err
		}
		barcodeValue = code
	}
	var id string
	err := tx.QueryRow(ctx, `
                INSERT INTO global_products (
                        name, generic_name, brand_name, dosage_form, strength, product_category,
                        requires_prescription, barcode, manufacturer_name, manufacturer_country,
                        active_ingredient, atc_code, therapeutic_class, storage_instructions,
                        description, is_verified, source
                ) VALUES (
                        $1, NULLIF($2,''), NULLIF($3,''), $4, NULLIF($5,''), $6,
                        $7, NULLIF($8,''), NULLIF($9,''), NULLIF($10,''),
                        NULLIF($11,''), NULLIF($12,''), NULLIF($13,''), NULLIF($14,''),
                        NULLIF($15,''), true, 'platform_admin'
                )
                RETURNING id::text
        `, p.Name, p.GenericName, p.BrandName, p.DosageForm, p.Strength, p.ProductCategory,
		p.RequiresPrescription, barcodeValue, p.ManufacturerName, p.ManufacturerCountry,
		p.ActiveIngredient, p.AtcCode, p.TherapeuticClass, p.StorageInstructions,
		p.Description).Scan(&id)
	if err != nil {
		// Barcode collision: another product already owns this code. Link to
		// it instead of failing — the catalog stays deduplicated by barcode.
		if libraryUniqueViolation(err) && barcodeValue != "" {
			var existing string
			qerr := tx.QueryRow(ctx,
				`SELECT id::text FROM global_products WHERE barcode = $1 LIMIT 1`, barcodeValue).Scan(&existing)
			if qerr == nil {
				return existing, nil
			}
		}
		return "", err
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// Add / Update / Remove a library entry
// ---------------------------------------------------------------------------

// AddLibraryProductInput is the frozen wire request of POST
// /platform-admin/libraries/:id/products (the handler binds the payload
// struct and copies the fields across).
type AddLibraryProductInput struct {
	GlobalProductID       string
	OfficialPricePiastres *int64
	Notes                 *string
	NewProduct            *NewCatalogProduct
}

// AddLibraryProductOutcome mirrors the 201 body's "data" — the handler
// maps the fields onto the same gin.H keys.
type AddLibraryProductOutcome struct {
	LibraryID             string
	GlobalProductID       string
	CreatedProduct        bool
	Action                string
	OfficialPricePiastres int64
}

// AddLibraryProduct — upsert semantics: adding an existing entry with a new
// price UPDATES it and logs price_changed — bulk price-list imports rely on
// this behavior. Owns the transaction end to end.
func AddLibraryProduct(ctx context.Context, pool *pgxpool.Pool, principal *auth.Principal, libraryID string, in AddLibraryProductInput) (AddLibraryProductOutcome, error) {
	out := AddLibraryProductOutcome{LibraryID: libraryID}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return out, &LibrariesFailure{Stage: StageTxBegin, Err: err}
	}
	defer func() { _ = tx.Rollback(ctx) }()

	createdProduct := false
	gpID := in.GlobalProductID
	if in.NewProduct != nil {
		gpID, err = CreateCatalogProduct(ctx, tx, in.NewProduct)
		if err != nil {
			return out, &LibrariesFailure{Stage: StageCatalogCreate, Err: err}
		}
		createdProduct = true
	} else {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM global_products WHERE id = $1::uuid)`, gpID).Scan(&exists); err != nil {
			return out, &LibrariesFailure{Stage: StageCatalogCheck, Err: err}
		}
		if !exists {
			return out, ErrCatalogProductNotFound
		}
	}

	draftVersion, _, err := LibraryDraftVersion(ctx, tx, libraryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrLibraryNotFound
	}
	if err != nil {
		return out, &LibrariesFailure{Stage: StageDraftVersion, Err: err}
	}

	hasPrice := in.OfficialPricePiastres != nil
	price := int64(0)
	if hasPrice {
		price = *in.OfficialPricePiastres
	}
	action, err := UpsertLibraryEntry(ctx, tx, libraryID, gpID, price, hasPrice, in.Notes, draftVersion)
	if err != nil {
		return out, &LibrariesFailure{Stage: StageUpsert, Err: err}
	}
	if err := TouchLibrary(ctx, tx, libraryID); err != nil {
		return out, &LibrariesFailure{Stage: StageTouch, Err: err}
	}
	if err := auditlog.Platform(ctx, tx, platformActor(principal),
		"library.product_add", "create", "library_product", libraryID+"/"+gpID, "", "",
		map[string]any{"library_id": libraryID, "global_product_id": gpID,
			"official_price_piastres": price, "created_product": createdProduct, "action": action},
		"إضافة/تحديث منتج في مكتبة"); err != nil {
		return out, &LibrariesFailure{Stage: StageAudit, Err: err}
	}
	if err := tx.Commit(ctx); err != nil {
		return out, &LibrariesFailure{Stage: StageTxCommit, Err: err}
	}
	out.GlobalProductID = gpID
	out.CreatedProduct = createdProduct
	out.Action = action
	out.OfficialPricePiastres = price
	return out, nil
}

// UpdateLibraryProductInput is the frozen wire request of PUT
// /platform-admin/libraries/:id/products/:pid.
type UpdateLibraryProductInput struct {
	OfficialPricePiastres *int64
	Notes                 *string
}

// UpdateLibraryProduct — price change → update + change log; notes change →
// plain update. COALESCE keeps whichever field the payload did not touch.
// Returns whether the official price changed.
func UpdateLibraryProduct(ctx context.Context, pool *pgxpool.Pool, principal *auth.Principal, libraryID, productID string, in UpdateLibraryProductInput) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, &LibrariesFailure{Stage: StageTxBegin, Err: err}
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		gpID     string
		currentP int64
	)
	err = tx.QueryRow(ctx, `
                SELECT lp.global_product_id::text, lp.official_price_piastres
                FROM library_products lp
                WHERE lp.id = $1::uuid AND lp.library_id = $2::uuid FOR UPDATE
        `, productID, libraryID).Scan(&gpID, &currentP)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrLibraryProductNotFound
	}
	if err != nil {
		return false, &LibrariesFailure{Stage: StageQuery, Err: err}
	}

	// Price change → update + change log; notes change → plain update.
	// COALESCE keeps whichever field the payload did not touch.
	priceChanged := in.OfficialPricePiastres != nil && *in.OfficialPricePiastres != currentP
	if priceChanged || in.Notes != nil {
		if _, err := tx.Exec(ctx, `
                        UPDATE library_products
                        SET official_price_piastres = COALESCE($2, official_price_piastres),
                            notes = COALESCE($3, notes),
                            updated_at = NOW()
                        WHERE id = $1::uuid
                `, productID, in.OfficialPricePiastres, in.Notes); err != nil {
			return false, &LibrariesFailure{Stage: StageUpdate, Err: err}
		}
	}
	if priceChanged {
		draftVersion, _, err := LibraryDraftVersion(ctx, tx, libraryID)
		if err != nil {
			return false, &LibrariesFailure{Stage: StageDraftVersion, Err: err}
		}
		newP := *in.OfficialPricePiastres
		if err := LogLibraryChange(ctx, tx, libraryID, gpID, draftVersion, "price_changed",
			&currentP, &newP, "تعديل يدوي للسعر الرسمي"); err != nil {
			return false, &LibrariesFailure{Stage: StageChangeLog, Err: err}
		}
		if err := TouchLibrary(ctx, tx, libraryID); err != nil {
			return false, &LibrariesFailure{Stage: StageTouch, Err: err}
		}
	}
	if err := auditlog.Platform(ctx, tx, platformActor(principal),
		"library.product_update", "update", "library_product", productID, "", "",
		map[string]any{"library_id": libraryID, "global_product_id": gpID,
			"old_price_piastres": currentP, "new_price_piastres": in.OfficialPricePiastres},
		"تعديل منتج داخل مكتبة"); err != nil {
		return false, &LibrariesFailure{Stage: StageAudit, Err: err}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, &LibrariesFailure{Stage: StageTxCommit, Err: err}
	}
	return priceChanged, nil
}

// RemoveLibraryProduct — removing an entry logs «removed» so pharmacies
// (Phase 2) see it in the diff; products already imported into a pharmacy
// stay theirs — the log is informational, never a deletion order.
func RemoveLibraryProduct(ctx context.Context, pool *pgxpool.Pool, principal *auth.Principal, libraryID, productID string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return &LibrariesFailure{Stage: StageTxBegin, Err: err}
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		gpID     string
		name     string
		currentP int64
	)
	err = tx.QueryRow(ctx, `
                SELECT lp.global_product_id::text, gp.name, lp.official_price_piastres
                FROM library_products lp
                JOIN global_products gp ON gp.id = lp.global_product_id
                WHERE lp.id = $1::uuid AND lp.library_id = $2::uuid FOR UPDATE OF lp
        `, productID, libraryID).Scan(&gpID, &name, &currentP)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLibraryProductNotFound
	}
	if err != nil {
		return &LibrariesFailure{Stage: StageQuery, Err: err}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM library_products WHERE id = $1::uuid`, productID); err != nil {
		return &LibrariesFailure{Stage: StageDelete, Err: err}
	}
	draftVersion, _, err := LibraryDraftVersion(ctx, tx, libraryID)
	if err != nil {
		return &LibrariesFailure{Stage: StageDraftVersion, Err: err}
	}
	if err := LogLibraryChange(ctx, tx, libraryID, gpID, draftVersion, "removed",
		&currentP, nil, "سحب منتج من المكتبة"); err != nil {
		return &LibrariesFailure{Stage: StageChangeLog, Err: err}
	}
	if err := TouchLibrary(ctx, tx, libraryID); err != nil {
		return &LibrariesFailure{Stage: StageTouch, Err: err}
	}
	if err := auditlog.Platform(ctx, tx, platformActor(principal),
		"library.product_remove", "delete", "library_product", productID, "", "",
		map[string]any{"library_id": libraryID, "global_product_id": gpID, "product_name": name},
		"سحب منتج من مكتبة: "+name); err != nil {
		return &LibrariesFailure{Stage: StageAudit, Err: err}
	}
	if err := tx.Commit(ctx); err != nil {
		return &LibrariesFailure{Stage: StageTxCommit, Err: err}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Excel/CSV price-bulletin import (execute half — validation is protocol)
// ---------------------------------------------------------------------------

// LibraryImportRow is one validated import row: the protocol edge parses
// the file, suggests the column mapping and validates every row (pure
// functions with Arabic rejection reasons) BEFORE opening the transaction
// (house rule) — this package receives only rows that passed.
type LibraryImportRow struct {
	Name             string
	Barcode          string
	PricePiastres    int64
	HasPrice         bool
	GenericName      string
	Strength         string
	DosageForm       string
	ManufacturerName string
}

// LibraryImportErrorCap — the merged wire error list (row-validation
// failures seeded by the handler + per-product failures appended here) is
// capped at the same value the pharmacy product import has always used.
// handlers.importErrorCap aliases this constant so there is ONE number.
const LibraryImportErrorCap = 200

// LibraryImportReport mirrors the 200 body's "data" of POST
// /platform-admin/libraries/:id/import/execute — keys verbatim.
type LibraryImportReport struct {
	TotalRows       int      `json:"total_rows"`
	Added           int      `json:"added"`
	PriceUpdated    int      `json:"price_updated"`
	ProductsCreated int      `json:"products_created"`
	Unchanged       int      `json:"unchanged"`
	Failed          int      `json:"failed"`
	Errors          []string `json:"errors"`
}

// ExecuteLibraryImport feeds a library from an Excel/CSV price list (e.g.
// the regulator's official price bulletin): rows resolve to the central
// catalog by barcode first, then by normalized name, and only truly new
// drugs create global_products rows (source=platform_admin, verified).
// Every library entry becomes an upsert: new → «added», different price →
// «price_changed» (old→new lands in the change log and later in the
// pharmacy diff). All-or-nothing in a single transaction, like the pharmacy
// import.
//
// totalRows is the RAW file row count (pre-validation) — the report's
// total_rows. validationErrors are the row-shape rejections the protocol
// edge already collected (they lead the merged error list exactly as the
// pre-extraction wire served them).
func ExecuteLibraryImport(ctx context.Context, pool *pgxpool.Pool, principal *auth.Principal, libraryID string, parsed []LibraryImportRow, totalRows int, validationErrors []string) (LibraryImportReport, error) {
	report := LibraryImportReport{
		TotalRows: totalRows,
	}
	importErrors := validationErrors

	tx, err := pool.Begin(ctx)
	if err != nil {
		return report, &LibrariesFailure{Stage: StageTxBegin, Err: err}
	}
	defer func() { _ = tx.Rollback(ctx) }()

	draftVersion, _, err := LibraryDraftVersion(ctx, tx, libraryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return report, ErrLibraryNotFound
	}
	if err != nil {
		return report, &LibrariesFailure{Stage: StageDraftVersion, Err: err}
	}

	// Current library entries are NOT preloaded here: UpsertLibraryEntry
	// takes a per-row FOR UPDATE lock, and the unique constraint backstops
	// any insert race. Fewer moving parts than a full pre-materialized map.

	// Barcode index for the whole file in ONE query.
	barcodeIdx := map[string]string{}
	{
		codes := make([]string, 0, len(parsed))
		for _, pr := range parsed {
			if pr.Barcode != "" {
				codes = append(codes, pr.Barcode)
			}
		}
		if len(codes) > 0 {
			brows, err := tx.Query(ctx,
				`SELECT id::text, barcode FROM global_products WHERE barcode = ANY($1)`, codes)
			if err != nil {
				return report, &LibrariesFailure{Stage: StageBarcodeIndex, Err: err}
			}
			for brows.Next() {
				var id, code string
				if err := brows.Scan(&id, &code); err == nil {
					barcodeIdx[code] = id
				}
			}
			brows.Close()
		}
	}

	// Name index (normalized) for rows without a barcode.
	nameIdx := map[string]string{}
	{
		nrows, err := tx.Query(ctx, `SELECT id::text, name FROM global_products`)
		if err != nil {
			return report, &LibrariesFailure{Stage: StageNameIndex, Err: err}
		}
		for nrows.Next() {
			var id, name string
			if err := nrows.Scan(&id, &name); err == nil {
				key := NormalizeImportName(name)
				if _, taken := nameIdx[key]; !taken {
					nameIdx[key] = id
				}
			}
		}
		nrows.Close()
	}

	added, priceUpdated, created, unchanged, failed := 0, 0, 0, 0, 0

	for _, pr := range parsed {
		gpID := ""
		if pr.Barcode != "" {
			gpID = barcodeIdx[pr.Barcode]
		}
		if gpID == "" {
			gpID = nameIdx[NormalizeImportName(pr.Name)]
		}
		if gpID == "" {
			np := NewCatalogProduct{
				Name: pr.Name, GenericName: pr.GenericName, DosageForm: pr.DosageForm,
				Strength: pr.Strength, ProductCategory: "medication",
				RequiresPrescription: "no", Barcode: pr.Barcode,
				ManufacturerName: pr.ManufacturerName,
			}
			newID, err := CreateCatalogProduct(ctx, tx, &np)
			if err != nil {
				failed++
				if len(importErrors) < LibraryImportErrorCap {
					importErrors = append(importErrors, fmt.Sprintf("منتج %s: تعذر إنشاؤه في الكتالوج", pr.Name))
				}
				continue
			}
			gpID = newID
			created++
			// keep in-file dedupe consistent
			if pr.Barcode != "" {
				barcodeIdx[pr.Barcode] = newID
			}
			nameIdx[NormalizeImportName(pr.Name)] = newID
		}

		action, err := UpsertLibraryEntry(ctx, tx, libraryID, gpID, pr.PricePiastres, pr.HasPrice, nil, draftVersion)
		if err != nil {
			failed++
			if len(importErrors) < LibraryImportErrorCap {
				importErrors = append(importErrors, fmt.Sprintf("منتج %s: تعذر إضافته للمكتبة", pr.Name))
			}
			continue
		}
		switch action {
		case "added":
			added++
		case "price_updated":
			priceUpdated++
		default:
			unchanged++
		}
	}

	if err := TouchLibrary(ctx, tx, libraryID); err != nil {
		return report, &LibrariesFailure{Stage: StageTouch, Err: err}
	}
	// Audit metadata mirrors the original gin.H at this exact point: the
	// six counters WITHOUT the errors key (errors were appended to the
	// wire report only after commit).
	if err := auditlog.Platform(ctx, tx, platformActor(principal),
		"library.import", "create", "product_library", libraryID, "", "",
		map[string]any{"report": map[string]any{
			"total_rows": totalRows, "added": added, "price_updated": priceUpdated,
			"products_created": created, "unchanged": unchanged, "failed": failed,
		}},
		fmt.Sprintf("استيراد مكتبة: %d صفًا — %d إضافة، %d تحديث سعر، %d منتج جديد", totalRows, added, priceUpdated, created)); err != nil {
		return report, &LibrariesFailure{Stage: StageAudit, Err: err}
	}
	if err := tx.Commit(ctx); err != nil {
		return report, &LibrariesFailure{Stage: StageTxCommit, Err: err}
	}
	report.Added = added
	report.PriceUpdated = priceUpdated
	report.ProductsCreated = created
	report.Unchanged = unchanged
	report.Failed = failed
	// The wire list stays nil (marshals as "errors": null) when nothing
	// failed — the pre-extraction gin.H served null in that case too.
	report.Errors = importErrors
	return report, nil
}

// NormalizeImportName normalizes a product name for catalog dedup (moved
// verbatim from handlers/product_import_execute.go — its four remaining
// handler callers are repointed mechanically).
func NormalizeImportName(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), ""))
}

// ---------------------------------------------------------------------------
// Catalog browse + full metadata edit
// ---------------------------------------------------------------------------

// CatalogProductRow mirrors one row of GET /platform-admin/catalog/products
// — json tags verbatim (the "libraries" raw blob keeps its exact shape:
// [{"id","name"}…] or []).
type CatalogProductRow struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	GenericName     *string         `json:"generic_name"`
	Strength        *string         `json:"strength"`
	DosageForm      string          `json:"dosage_form"`
	ProductCategory string          `json:"product_category"`
	Barcode         *string         `json:"barcode"`
	Manufacturer    *string         `json:"manufacturer_name"`
	IsVerified      bool            `json:"is_verified"`
	Source          string          `json:"source"`
	IsActive        bool            `json:"is_active"`
	Libraries       json.RawMessage `json:"libraries"`
}

// ListCatalogProducts serves GET /platform-admin/catalog/products — the
// central catalog browser backing the «إضافة منتج من الكتالوج» picker:
// barcode-exact and prefix matches rank first, then name matches; each row
// carries the libraries it currently belongs to (single membership query,
// keyed in Go).
func ListCatalogProducts(ctx context.Context, db LibraryDB, search string, page, pageSize int) ([]CatalogProductRow, int64, error) {
	where := ""
	args := []any{}
	if search != "" {
		where = ` WHERE name ILIKE '%' || $1 || '%' OR generic_name ILIKE '%' || $1 || '%' OR barcode = $2 OR barcode ILIKE $2 || '%'`
		args = append(args, escapeLikeValue(search), escapeLikeValue(search))
	}

	var total int64
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM global_products`+where, args...,
	).Scan(&total); err != nil {
		return nil, 0, &LibrariesFailure{Stage: StageCount, Err: err}
	}

	qargs := append(args, pageSize, (page-1)*pageSize)
	order := ` ORDER BY name LIMIT $` + itoaLib(len(qargs)-1) + ` OFFSET $` + itoaLib(len(qargs))
	rows, err := db.Query(ctx, `
                SELECT id::text, name, generic_name, strength, dosage_form::text, product_category::text,
                       barcode, manufacturer_name, is_verified, source, is_active
                FROM global_products`+where+order, qargs...)
	if err != nil {
		return nil, 0, &LibrariesFailure{Stage: StageQuery, Err: err}
	}
	defer rows.Close()

	items := []CatalogProductRow{}
	ids := []string{}
	for rows.Next() {
		var r CatalogProductRow
		if err := rows.Scan(&r.ID, &r.Name, &r.GenericName, &r.Strength, &r.DosageForm,
			&r.ProductCategory, &r.Barcode, &r.Manufacturer, &r.IsVerified, &r.Source,
			&r.IsActive); err != nil {
			return nil, 0, &LibrariesFailure{Stage: StageScan, Err: err}
		}
		r.Libraries = json.RawMessage("[]")
		items = append(items, r)
		ids = append(ids, r.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, &LibrariesFailure{Stage: StageScan, Err: err}
	}

	// Library membership for the page rows (single query, keyed in Go).
	// The pre-extraction code deliberately swallowed membership failures
	// (rows keep their "libraries": [] fallback) — preserved verbatim.
	member := map[string][]map[string]any{}
	if len(ids) > 0 {
		mrows, err := db.Query(ctx, `
                        SELECT lp.global_product_id::text, l.id::text, l.name
                        FROM library_products lp
                        JOIN product_libraries l ON l.id = lp.library_id
                        WHERE lp.global_product_id = ANY($1::uuid[])
                        ORDER BY l.name
                `, ids)
		if err == nil {
			for mrows.Next() {
				var gpID, libID, libName string
				if err := mrows.Scan(&gpID, &libID, &libName); err == nil {
					member[gpID] = append(member[gpID], map[string]any{"id": libID, "name": libName})
				}
			}
			mrows.Close()
		}
	}
	for i := range items {
		if libs := member[items[i].ID]; len(libs) > 0 {
			blob, err := json.Marshal(libs)
			if err == nil {
				items[i].Libraries = blob
			}
		}
	}

	return items, total, nil
}

// itoaLib is strconv.Itoa under a local alias so this file's helper set
// stays self-contained.
func itoaLib(v int) string { return fmt.Sprintf("%d", v) }

// CatalogProductUpdate is the frozen wire request of PUT
// /platform-admin/catalog/products/:id — full metadata edit + verification
// badge (json tags verbatim; the protocol edge binds this struct directly).
type CatalogProductUpdate struct {
	Name                 *string `json:"name"`
	GenericName          *string `json:"generic_name"`
	BrandName            *string `json:"brand_name"`
	DosageForm           *string `json:"dosage_form"`
	Strength             *string `json:"strength"`
	ProductCategory      *string `json:"product_category"`
	RequiresPrescription *string `json:"requires_prescription"`
	Barcode              *string `json:"barcode"`
	ManufacturerName     *string `json:"manufacturer_name"`
	ManufacturerCountry  *string `json:"manufacturer_country"`
	ActiveIngredient     *string `json:"active_ingredient"`
	AtcCode              *string `json:"atc_code"`
	TherapeuticClass     *string `json:"therapeutic_class"`
	StorageInstructions  *string `json:"storage_instructions"`
	Description          *string `json:"description"`
	IsVerified           *bool   `json:"is_verified"`
	IsActive             *bool   `json:"is_active"`
}

// UpdateCatalogProduct — full metadata edit of a central catalog product +
// the verification badge. Metadata fixes propagate as «metadata_changed»
// rows into EVERY library containing the product, tagged with each
// library's draft version — that is how a name/strength correction later
// reaches pharmacies through the same diff channel as prices.
func UpdateCatalogProduct(ctx context.Context, pool *pgxpool.Pool, principal *auth.Principal, productID string, in CatalogProductUpdate) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return &LibrariesFailure{Stage: StageTxBegin, Err: err}
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		oldName  string
		oldCode  *string
		oldGen   *string
		oldBrand *string
		oldForm  string
		oldStr   *string
		oldCat   string
		oldRx    string
		oldMfg   *string
		oldMfgC  *string
		oldAct   *string
		oldAtc   *string
		oldClass *string
		oldStore *string
		oldDesc  *string
		oldVer   bool
		oldLive  bool
	)
	err = tx.QueryRow(ctx,
		`SELECT name, barcode, generic_name, brand_name, dosage_form::text, strength,
		        product_category::text, requires_prescription::text,
		        manufacturer_name, manufacturer_country, active_ingredient,
		        atc_code, therapeutic_class, storage_instructions, description,
		        is_verified, is_active
		 FROM global_products WHERE id = $1::uuid FOR UPDATE`,
		productID).Scan(&oldName, &oldCode, &oldGen, &oldBrand, &oldForm, &oldStr,
		&oldCat, &oldRx, &oldMfg, &oldMfgC, &oldAct,
		&oldAtc, &oldClass, &oldStore, &oldDesc, &oldVer, &oldLive)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCatalogProductNotFound
	}
	if err != nil {
		return &LibrariesFailure{Stage: StageQuery, Err: err}
	}

	tag, err := tx.Exec(ctx, `
                UPDATE global_products SET
                        name                 = COALESCE($2, name),
                        generic_name         = COALESCE($3, generic_name),
                        brand_name           = COALESCE($4, brand_name),
                        dosage_form          = COALESCE($5::dosage_form, dosage_form),
                        strength             = COALESCE($6, strength),
                        product_category     = COALESCE($7::product_category, product_category),
                        requires_prescription = COALESCE($8::prescription_required, requires_prescription),
                        barcode              = COALESCE(NULLIF($9, ''), barcode),
                        manufacturer_name    = COALESCE($10, manufacturer_name),
                        manufacturer_country = COALESCE($11, manufacturer_country),
                        active_ingredient    = COALESCE($12, active_ingredient),
                        atc_code             = COALESCE($13, atc_code),
                        therapeutic_class    = COALESCE($14, therapeutic_class),
                        storage_instructions = COALESCE($15, storage_instructions),
                        description          = COALESCE($16, description),
                        is_verified          = COALESCE($17, is_verified),
                        is_active            = COALESCE($18, is_active),
                        updated_at           = NOW()
                WHERE id = $1::uuid
        `, productID,
		in.Name, in.GenericName, in.BrandName,
		in.DosageForm, in.Strength, in.ProductCategory,
		in.RequiresPrescription, in.Barcode,
		in.ManufacturerName, in.ManufacturerCountry,
		in.ActiveIngredient, in.AtcCode, in.TherapeuticClass,
		in.StorageInstructions, in.Description,
		in.IsVerified, in.IsActive)
	if err != nil {
		if libraryUniqueViolation(err) {
			return ErrBarcodeTaken
		}
		return &LibrariesFailure{Stage: StageUpdate, Err: err}
	}
	if tag.RowsAffected() == 0 {
		return ErrCatalogProductNotFound
	}

	// Propagate the metadata fix into every library containing the product —
	// ONLY when the edit actually changed something: a no-op save (the UI
	// round-tripping unchanged values) used to append metadata_changed rows
	// into EVERY containing library, inflating pending_changes and letting
	// a subsequent publish ship an «empty release» the pharmacies pull for
	// nothing. NULL/empty-string payloads mean «leave unchanged» (the
	// UPDATE's COALESCE semantics), and the barcode additionally cannot be
	// cleared (NULLIF('') keeps the old value) — all mirrored below.
	summary := "تصحيح بيانات منتج من قبل المنصة"
	metadataChanged := strFieldChanged(in.Name, &oldName) ||
		strFieldChanged(in.GenericName, oldGen) ||
		strFieldChanged(in.BrandName, oldBrand) ||
		(in.DosageForm != nil && *in.DosageForm != oldForm) ||
		strFieldChanged(in.Strength, oldStr) ||
		(in.ProductCategory != nil && *in.ProductCategory != oldCat) ||
		(in.RequiresPrescription != nil && *in.RequiresPrescription != oldRx) ||
		(in.Barcode != nil && *in.Barcode != "" && (oldCode == nil || *in.Barcode != *oldCode)) ||
		strFieldChanged(in.ManufacturerName, oldMfg) ||
		strFieldChanged(in.ManufacturerCountry, oldMfgC) ||
		strFieldChanged(in.ActiveIngredient, oldAct) ||
		strFieldChanged(in.AtcCode, oldAtc) ||
		strFieldChanged(in.TherapeuticClass, oldClass) ||
		strFieldChanged(in.StorageInstructions, oldStore) ||
		strFieldChanged(in.Description, oldDesc) ||
		(in.IsVerified != nil && *in.IsVerified != oldVer) ||
		(in.IsActive != nil && *in.IsActive != oldLive)
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" && *in.Name != oldName {
		summary = "تصحيح بيانات منتج: " + oldName + " ← " + strings.TrimSpace(*in.Name)
	}
	if metadataChanged {
		if _, err := tx.Exec(ctx, `
			INSERT INTO library_changes (library_id, global_product_id, version, change_type, summary)
			SELECT lp.library_id, $1::uuid,
			       CASE WHEN l.is_published THEN l.version + 1 ELSE l.version END,
			       'metadata_changed', $2
			FROM library_products lp
			JOIN product_libraries l ON l.id = lp.library_id
			WHERE lp.global_product_id = $1::uuid
		`, productID, summary); err != nil {
			return &LibrariesFailure{Stage: StagePropagation, Err: err}
		}
	}

	if err := auditlog.Platform(ctx, tx, platformActor(principal),
		"catalog.product_update", "update", "global_product", productID, "", "",
		map[string]any{"old_name": oldName, "old_barcode": oldCode, "barcode_changed": in.Barcode != nil},
		summary); err != nil {
		return &LibrariesFailure{Stage: StageAudit, Err: err}
	}
	if err := tx.Commit(ctx); err != nil {
		return &LibrariesFailure{Stage: StageTxCommit, Err: err}
	}
	return nil
}
