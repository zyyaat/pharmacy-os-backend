// ADR-0005 (api_level 108) — ledger item 1: the POS money path moves out of
// product_pos_handler.go under the layering policy. The code is moved, not
// rewritten: same statements, same scan order, same allocation rules, same
// error identities — the handler maps each typed error 1:1 onto the exact
// wire body it emitted before (status, error code, Arabic message, keys).
// The wire contract is frozen during extraction (ADR-0005 rule 3): mobile
// APKs are not exported from this repo and a stale install must keep
// checking out — any "small" wire drift here is a pharmacy session outage
// tomorrow.
//
// Ownership split (ADR-0005 rule 2): THIS package owns the sale transaction
// (BEGIN/lease set_config/COMMIT), the pricing rules (packaging units,
// explicit-strip fallback with the sanctioned half-up division), the
// largest-remainder discount allocation, the AUD-015 idempotent replay, the
// AUD-009 cross-tenant customer check and the R-5 compressed fulfillment
// (FEFO + per-batch movement chains). The handler keeps protocol only:
// binding, request-shape validation, branch resolution, HTTP statuses and
// response bodies.
package services

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pharmacy-os/backend/internal/auth"
	"github.com/pharmacy-os/backend/internal/money"
)

// Packaging vocabulary shared by the pricing service and the product
// protocol handlers. One definition, no drift.
const (
	PackagingWholeOnly = "WHOLE_ONLY"
	PackagingBoxStrip  = "BOX_STRIP"
)

// maxBatchedTakes caps the multi-row VALUES path of PersistPOSItems:
// 12 params per sale_items row at 3000 takes stays far below the
// PostgreSQL 65535-parameter ceiling with headroom for both inserts.
const maxBatchedTakes = 3000

// All money in this file is money.Piastres: an integer count of piastres
// (1 EGP = 100 piastres). No floating point value ever touches an amount.

type POSSaleDiscountRequest struct {
	Kind  string  `json:"kind"` // "percent" | "amount"
	Value float64 `json:"value"`
}

type POSSaleItemRequest struct {
	PharmacyProductID string `json:"pharmacy_product_id"`
	SaleUnit          string `json:"sale_unit"`
	Quantity          int64  `json:"quantity"`
	// Client intent captured while the cashier was scanning. Both fields
	// are optional; when present the backend verifies them against the
	// authoritative prices and rejects the sale with 409 price_changed
	// instead of silently charging a stale price.
	ExpectedUnitPricePiastres *int64 `json:"expected_unit_price_piastres"`
	ExpectedLineTotalPiastres *int64 `json:"expected_line_total_piastres"`
}

type POSSaleRequest struct {
	Items          []POSSaleItemRequest `json:"items"`
	IdempotencyKey string               `json:"idempotency_key"`
	// خصم على مستوى الفاتورة: نسبة مئوية (0-100) أو مبلغ ثابت بالقروش
	Discount *POSSaleDiscountRequest `json:"discount"`
	// نقدي (افتراضي) أو آجل لعميل مسجل في نفس الصيدلية
	PaymentType string `json:"payment_type"`
	CustomerID  string `json:"customer_id"`
}

// POSPricingSnapshot is the authoritative per-product pricing read inside
// the sale transaction. Field semantics are part of the wire through the
// price_changed mismatch items — treat reordering as a wire change.
type POSPricingSnapshot struct {
	PackagingType string
	UnitsPerBox   int64
	BoxPrice      money.Piastres
	StripPrice    money.Piastres
	// Identity snapshot (migration 49): the EFFECTIVE product identity at
	// sale time — local override if the pharmacy set one, official catalog
	// otherwise. Frozen into every sale_items row so a later edit of the
	// product never rewrites yesterday's invoice.
	Name     string
	Barcode  string
	Strength string
}

// EffectiveUnitPrice returns the authoritative price for one sale unit.
func (s POSPricingSnapshot) EffectiveUnitPrice(saleUnit string) money.Piastres {
	if saleUnit == "box" {
		return s.BoxPrice
	}
	return s.StripPrice
}

// POSSaleOutcome is the service-level result of one checkout. Replayed=true
// means the AUD-015 idempotent path served the ORIGINAL invoice (no second
// charge); the handler emits a 200 body without the discount key, exactly
// as the pre-extraction replay branch did.
type POSSaleOutcome struct {
	SaleID   string
	Total    money.Piastres
	Discount money.Piastres
	Replayed bool
}

// POSSalePriceMismatch mirrors one rejected cart line for the 409
// price_changed body ("data.items"). The handler maps these fields onto the
// same gin.H keys verbatim.
type POSSalePriceMismatch struct {
	Index             int
	PharmacyProductID string
	SaleUnit          string
	Quantity          int64
	UnitPricePiastres money.Piastres
	LineTotalPiastres money.Piastres
}

// Business rejects — the handler maps each onto the exact pre-extraction
// body. Do not merge sentinels that carry different messages; do not split
// sentinels that share one.
var (
	// ErrPOSSaleNoItems guards direct service callers against an empty
	// invoice (the protocol edge rejects it earlier with the same body).
	ErrPOSSaleNoItems = errors.New("POS sale requires at least one item")
	// ErrPOSSaleInsufficientStock is the R-5 contract: insufficient stock
	// inside the locked sweep, no partial writes (409 upstream).
	ErrPOSSaleInsufficientStock = errors.New("insufficient stock for POS sale")
	// ErrPOSCustomerForeign is AUD-009: the attached customer does not
	// belong to the selling pharmacy (checked inside the transaction).
	ErrPOSCustomerForeign = errors.New("customer does not belong to this pharmacy")
	// ErrPOSUnknownProduct: the cart references a product the pharmacy does
	// not sell (or is inactive) — same body the old ErrNoRows branch served.
	ErrPOSUnknownProduct = errors.New("unknown or inactive pharmacy product")
	// ErrPOSSaleUnitForbidden: the sale unit violates the product's
	// packaging rules (strip on WHOLE_ONLY, or strip on BOX_STRIP with
	// units_per_box < 2).
	ErrPOSSaleUnitForbidden = errors.New("sale unit not allowed for this product")
	// ErrPOSProductPricing: the product's prices are not sane
	// (BoxPrice/StripPrice invalid) — the cashier must fix the product.
	ErrPOSProductPricing = errors.New("product pricing is not set up")
	// Discount rejections — four distinct wire messages under one code.
	ErrPOSDiscountPercentRange = errors.New("discount percent must be between 0 and 100")
	ErrPOSDiscountNegative     = errors.New("discount amount cannot be negative")
	ErrPOSDiscountKind         = errors.New("unknown discount kind")
	ErrPOSDiscountExceedsTotal = errors.New("discount exceeds invoice total")
)

// POSFailure wraps an internal (non-business) failure with the stage it
// happened at, so the handler can emit the same stage-specific message the
// pre-extraction branch emitted, while the real cause lands in the log —
// never on the wire.
type POSStage int

const (
	POSStageBegin          POSStage = iota // pool.Begin
	POSStageLease                          // set_config('app.current_pharmacy_id'...)
	POSStageReplayCheck                    // AUD-015 idempotency pre-read
	POSStagePricingRead                    // LoadPOSPricingBatch
	POSStageInsert                         // INSERT INTO sales
	POSStageInsertRaceRead                 // concurrent-retry replay read after DO NOTHING
	POSStagePersistItems                   // PersistPOSItems
	POSStageTotal                          // UPDATE sales SET total_amount
	POSStageCommit                         // tx.Commit
)

type POSFailure struct {
	Stage POSStage
	Err   error
}

func (e *POSFailure) Error() string {
	return fmt.Sprintf("pos sale stage %d: %v", e.Stage, e.Err)
}

func (e *POSFailure) Unwrap() error { return e.Err }

// POSSalePriceChanged is the 409 price_changed payload: every mismatched
// line reported together so the cashier's cart refreshes in one round trip.
type POSSalePriceChanged struct {
	Items []POSSalePriceMismatch
}

func (e *POSSalePriceChanged) Error() string {
	return fmt.Sprintf("price changed on %d line(s)", len(e.Items))
}

// CreatePOSSale owns the whole checkout transaction: lease context
// (ADR-0002 — derived from the principal, never from the client), the
// AUD-009 customer ownership check, the AUD-015 idempotent replay, the
// two-phase price verification, the largest-remainder discount allocation,
// the sale insert (with its concurrent-retry race branch) and the R-5
// compressed fulfillment. On any error the transaction is rolled back and
// nothing partial survives; the caller only translates errors to wire.
//
// Request-shape validation (item bounds, unit vocabulary, key length) stays
// at the protocol edge; this function re-checks only what guards data
// integrity for direct service callers (empty cart).
func CreatePOSSale(
	ctx context.Context,
	pool *pgxpool.Pool,
	principal *auth.Principal,
	branchID string,
	req POSSaleRequest,
) (POSSaleOutcome, error) {
	out := POSSaleOutcome{}
	if len(req.Items) == 0 {
		return out, ErrPOSSaleNoItems
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return out, &POSFailure{Stage: POSStageBegin, Err: err}
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		SELECT set_config('app.current_pharmacy_id', $1, true),
		       set_config('app.current_user_id', $2, true)
	`, principal.PharmacyID, principal.ID); err != nil {
		return out, &POSFailure{Stage: POSStageLease, Err: err}
	}

	// AUD-009: cross-tenant customer ownership — the request only carries
	// a UUID, and UUIDs are unguessable but not secret (sync pages, logs,
	// shared devices). The customer attached to a cash/credit sale MUST
	// belong to the selling pharmacy; a foreign UUID (even a valid one)
	// is rejected inside the sale transaction, before any row is written.
	// Credit sales already require a customer — for them this is also the
	// guarantee that receivables land on the tenant's own ledger.
	if req.CustomerID != "" {
		var owned bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(
			    SELECT 1 FROM customers
			    WHERE id = $1::uuid AND pharmacy_id = $2::uuid
			)
		`, req.CustomerID, principal.PharmacyID).Scan(&owned); err != nil || !owned {
			return out, ErrPOSCustomerForeign
		}
	}

	// Idempotent replay: the same checkout retry returns the original
	// invoice instead of creating a second sale.
	if req.IdempotencyKey != "" {
		var existingID string
		var existingTotal money.Piastres
		err := tx.QueryRow(ctx, `
			SELECT id::text, total_amount::int8 FROM sales
			WHERE pharmacy_id = $1 AND idempotency_key = $2
		`, principal.PharmacyID, req.IdempotencyKey).Scan(&existingID, &existingTotal)
		if err == nil {
			return POSSaleOutcome{SaleID: existingID, Total: existingTotal, Replayed: true}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return out, &POSFailure{Stage: POSStageReplayCheck, Err: err}
		}
	}

	// Phase 1: verify the client's price intent against the authoritative
	// prices BEFORE any mutation. Mismatched items are reported together
	// so the cashier's cart can be refreshed in one round trip.
	// R-5: one round trip reads the pricing of every DISTINCT product in
	// the cart (was: one loadPOSPricing round trip per line). A product
	// absent from the result is the same contract the old ErrNoRows
	// branch served (unknown/inactive product -> invalid sale unit).
	distinctProducts := make([]string, 0, len(req.Items))
	seenProduct := make(map[string]bool, len(req.Items))
	for _, item := range req.Items {
		if !seenProduct[item.PharmacyProductID] {
			seenProduct[item.PharmacyProductID] = true
			distinctProducts = append(distinctProducts, item.PharmacyProductID)
		}
	}
	pricings, pricingErr := LoadPOSPricingBatch(ctx, tx, principal.PharmacyID, distinctProducts)
	if pricingErr != nil {
		return out, &POSFailure{Stage: POSStagePricingRead, Err: pricingErr}
	}
	snapshots := make([]POSPricingSnapshot, len(req.Items))
	lineTotals := make([]money.Piastres, len(req.Items))
	mismatches := make([]POSSalePriceMismatch, 0)
	for i, item := range req.Items {
		snapshot, found := pricings[item.PharmacyProductID]
		if !found {
			return out, ErrPOSUnknownProduct
		}
		if !snapshot.BoxPrice.Valid() || !snapshot.StripPrice.Valid() {
			return out, ErrPOSProductPricing
		}
		// Sale-unit rules.
		if snapshot.PackagingType == PackagingWholeOnly && item.SaleUnit != "box" {
			return out, ErrPOSSaleUnitForbidden
		}
		if snapshot.PackagingType == PackagingBoxStrip && item.SaleUnit == "strip" && snapshot.UnitsPerBox < 2 {
			return out, ErrPOSSaleUnitForbidden
		}
		snapshots[i] = snapshot
		unitPrice := snapshot.EffectiveUnitPrice(item.SaleUnit)
		lineTotals[i] = unitPrice.MulQty(item.Quantity)
		if (item.ExpectedUnitPricePiastres != nil && *item.ExpectedUnitPricePiastres != int64(unitPrice)) ||
			(item.ExpectedLineTotalPiastres != nil && *item.ExpectedLineTotalPiastres != int64(lineTotals[i])) {
			mismatches = append(mismatches, POSSalePriceMismatch{
				Index:             i,
				PharmacyProductID: item.PharmacyProductID,
				SaleUnit:          item.SaleUnit,
				Quantity:          item.Quantity,
				UnitPricePiastres: unitPrice,
				LineTotalPiastres: lineTotals[i],
			})
			continue
		}
	}
	if len(mismatches) > 0 {
		return out, &POSSalePriceChanged{Items: mismatches}
	}

	// الخصم: يُحسب من الإجمالي ثم يُوزّع على الأسطر بطريقة أكبر باقٍ
	// بحيث يكون مبلغ كل سطر صافياً بعد الخصم — فاسترجاع أي سطر يرد
	// ما حُمّل فعلاً بلا أي معالجة خاصة في تدفق الاسترجاع.
	var subtotal money.Piastres
	for _, lt := range lineTotals {
		subtotal = subtotal.Add(lt)
	}
	discount := money.Piastres(0)
	if req.Discount != nil && req.Discount.Value != 0 {
		switch req.Discount.Kind {
		case "percent":
			if req.Discount.Value < 0 || req.Discount.Value > 100 {
				return out, ErrPOSDiscountPercentRange
			}
			discount = money.Piastres(int64(math.Round(float64(subtotal) * req.Discount.Value / 100.0)))
		case "amount":
			if req.Discount.Value < 0 {
				return out, ErrPOSDiscountNegative
			}
			discount = money.Piastres(int64(math.Round(req.Discount.Value)))
		default:
			return out, ErrPOSDiscountKind
		}
		if discount > subtotal {
			return out, ErrPOSDiscountExceedsTotal
		}
	}
	weights := make([]int64, len(lineTotals))
	for i, lt := range lineTotals {
		weights[i] = int64(lt)
	}
	lineDiscounts := money.Allocate(discount, weights)
	itemAmounts := make([]money.Piastres, len(lineTotals))
	for i := range lineTotals {
		itemAmounts[i] = lineTotals[i] - lineDiscounts[i]
	}

	// Phase 2: persist the invoice.
	employeeID, companyUserID := auth.ActorIDs(principal)
	var saleID string
	insertErr := tx.QueryRow(ctx, `
		INSERT INTO sales (pharmacy_id, branch_id, employee_id, company_user_id, idempotency_key, payment_type, customer_id)
		VALUES ($1, $2, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid, NULLIF($5, ''), $6, NULLIF($7, '')::uuid)
		ON CONFLICT (pharmacy_id, idempotency_key) WHERE idempotency_key IS NOT NULL
		DO NOTHING
		RETURNING id::text
	`, principal.PharmacyID, branchID, employeeID, companyUserID, req.IdempotencyKey, req.PaymentType, req.CustomerID).Scan(&saleID)
	if errors.Is(insertErr, pgx.ErrNoRows) && req.IdempotencyKey != "" {
		// A concurrent retry of the same checkout won the race.
		var existingID string
		var existingTotal money.Piastres
		if err := tx.QueryRow(ctx, `
			SELECT id::text, total_amount::int8 FROM sales
			WHERE pharmacy_id = $1 AND idempotency_key = $2
		`, principal.PharmacyID, req.IdempotencyKey).Scan(&existingID, &existingTotal); err == nil {
			return POSSaleOutcome{SaleID: existingID, Total: existingTotal, Replayed: true}, nil
		}
		return out, &POSFailure{Stage: POSStageInsertRaceRead, Err: insertErr}
	}
	if insertErr != nil {
		return out, &POSFailure{Stage: POSStageInsert, Err: insertErr}
	}

	// R-5: the whole invoice fulfillment is one compressed batch of
	// statements (see PersistPOSItems) — FEFO order, largest-remainder
	// allocation, ledger semantics and the insufficient-stock 409 are
	// unchanged; only the round-trip count collapses.
	if err := PersistPOSItems(ctx, tx, principal, branchID, saleID, req.Items, snapshots, itemAmounts); err != nil {
		if errors.Is(err, ErrPOSSaleInsufficientStock) {
			return out, err
		}
		return out, &POSFailure{Stage: POSStagePersistItems, Err: err}
	}
	var total money.Piastres
	for _, amt := range itemAmounts {
		total = total.Add(amt)
	}
	// total هنا هو الصافي بعد الخصم (مجموع الأسطر الموزّعة) ويساوي subtotal - discount
	if _, err := tx.Exec(ctx, `UPDATE sales SET total_amount = $2, discount_amount = $3 WHERE id = $1`, saleID, total, discount); err != nil {
		return out, &POSFailure{Stage: POSStageTotal, Err: err}
	}
	if err := tx.Commit(ctx); err != nil {
		return out, &POSFailure{Stage: POSStageCommit, Err: err}
	}
	return POSSaleOutcome{SaleID: saleID, Total: total, Discount: discount, Replayed: false}, nil
}

// LoadPOSPricingBatch is R-5's single-statement replacement for the
// per-item loadPOSPricing round trip: one SELECT ... = ANY over the cart's
// DISTINCT products inside the current transaction. The pricing rules are
// byte-for-byte identical (packaging/units/explicit-strip-price fallback
// with half-up division for legacy rows); only the round trips change:
// 1 instead of len(items). A product missing from the result means the
// same thing ErrNoRows meant before (unknown/inactive product).
func LoadPOSPricingBatch(ctx context.Context, tx pgx.Tx, pharmacyID string, productIDs []string) (map[string]POSPricingSnapshot, error) {
	out := make(map[string]POSPricingSnapshot, len(productIDs))
	if len(productIDs) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT pp.id::text, COALESCE(pp.packaging_type::text, ''), COALESCE(pp.units_per_box::int8, 1),
		       pp.selling_price::int8, pp.partial_selling_price::int8,
		       COALESCE(pp.name_override, gp.name::text, ''),
		       COALESCE(pp.barcode_override, gp.barcode::text, ''),
		       COALESCE(pp.strength_override, gp.strength::text, '')
		FROM pharmacy_products pp
		LEFT JOIN global_products gp ON gp.id = pp.global_product_id
		WHERE pp.pharmacy_id = $1 AND pp.is_active = true
		  AND pp.id = ANY($2::uuid[])
	`, pharmacyID, productIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var snapshot POSPricingSnapshot
		var boxPrice money.Piastres
		// partial_selling_price is nullable (legacy rows ship without a strip
		// price), so it MUST scan into a pointer; a NULL must never fail the
		// read, it triggers the documented fallback instead.
		var partialPrice *int64
		if err := rows.Scan(&id, &snapshot.PackagingType, &snapshot.UnitsPerBox, &boxPrice, &partialPrice,
					&snapshot.Name, &snapshot.Barcode, &snapshot.Strength); err != nil {
			return nil, err
		}
		snapshot.BoxPrice = boxPrice
		snapshot.StripPrice = 0
		if partialPrice != nil && *partialPrice >= 0 {
			snapshot.StripPrice = money.Piastres(*partialPrice)
		}
		if snapshot.PackagingType == PackagingBoxStrip && snapshot.StripPrice == 0 {
			// Legacy row without an explicit strip price: the only
			// sanctioned division in the system, half-up on piastres.
			snapshot.StripPrice = boxPrice.DivRoundHalfUp(snapshot.UnitsPerBox)
		}
		out[id] = snapshot
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// PersistPOSItems is R-5's compressed write path for the whole invoice.
// Instead of one pricing read per line plus per-take statements (up to ~41
// round trips for a 5-line invoice), the entire fulfillment issues a fixed
// handful of statements regardless of line count:
//  1. one FOR UPDATE sweep over every candidate batch of every line
//     (deterministic lock order by product id — a strictly better deadlock
//     profile than per-line cart-order locking),
//  2. one multi-row INSERT into sale_items,
//  3. one multi-row INSERT into stock_movements,
//  4. one UPDATE ... FROM (VALUES) for inventory_batches.quantity,
//  5. one UPDATE ... = ANY for pharmacy_products.last_sold_at.
//
// FEFO order, largest-remainder line allocation, quantity_before/after
// semantics, and the insufficient-stock contract are byte-for-byte the
// same as the per-line implementation. Duplicate products across lines
// share one locked batch state and drain it in cart order. Carts whose
// allocation spans more than maxBatchedTakes batch takes fall back to
// per-take statements so the PostgreSQL parameter ceiling can never be
// reached (the plan itself is identical).
func PersistPOSItems(
	ctx context.Context,
	tx pgx.Tx,
	principal *auth.Principal,
	branchID, saleID string,
	items []POSSaleItemRequest,
	snapshots []POSPricingSnapshot,
	lineAmounts []money.Piastres,
) error {
	// Distinct products, cart order (also the lock order).
	distinct := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		if !seen[it.PharmacyProductID] {
			seen[it.PharmacyProductID] = true
			distinct = append(distinct, it.PharmacyProductID)
		}
	}

	// 1. Lock every candidate batch once, FEFO order within each product.
	type batchStock struct {
		productID   string
		id          string
		available   int64
		baseUnit    string
		costPerUnit money.Piastres
	}
	perProduct := make(map[string][]*batchStock, len(distinct))
	rows, err := tx.Query(ctx, `
		SELECT ib.pharmacy_product_id::text, ib.id::text, ROUND(ib.quantity)::int8,
		       ib.unit::text, ib.cost_per_unit::int8
		FROM inventory_batches ib
		WHERE ib.branch_id = $1
		  AND ib.is_quarantined = false
		  AND ib.quantity > 0
		  AND ib.pharmacy_product_id = ANY($2::uuid[])
		ORDER BY ib.pharmacy_product_id, ib.expiry_date NULLS LAST, ib.received_date, ib.created_at
		FOR UPDATE
	`, branchID, distinct)
	if err != nil {
		return fmt.Errorf("batch query: %w", err)
	}
	for rows.Next() {
		var b batchStock
		if err := rows.Scan(&b.productID, &b.id, &b.available, &b.baseUnit, &b.costPerUnit); err != nil {
			rows.Close()
			return fmt.Errorf("batch scan: %w", err)
		}
		perProduct[b.productID] = append(perProduct[b.productID], &b)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("batch rows: %w", err)
	}
	rows.Close() // release the tx connection before the writes (conn-busy guard)

	// FEFO allocation across all lines against the shared locked state.
	type takePlan struct {
		lineIdx int
		ordinal int // take index within its line (largest-remainder slot)
		batch   *batchStock
		take    int64
		before  int64
		after   int64
	}
	plan := make([]takePlan, 0, len(items))
	lineOrdinals := make(map[int]int, len(items))
	for i, item := range items {
		snapshot := snapshots[i]
		unitFactor := int64(1)
		if item.SaleUnit == "box" {
			unitFactor = snapshot.UnitsPerBox
		}
		remaining := item.Quantity * unitFactor
		for _, b := range perProduct[item.PharmacyProductID] {
			if remaining <= 0 {
				break
			}
			if b.available <= 0 {
				continue
			}
			take := b.available
			if take > remaining {
				take = remaining
			}
			plan = append(plan, takePlan{
				lineIdx: i, ordinal: lineOrdinals[i], batch: b,
				take: take, before: b.available, after: b.available - take,
			})
			lineOrdinals[i]++
			b.available -= take
			remaining -= take
		}
		if remaining > 0 {
			return ErrPOSSaleInsufficientStock
		}
	}

	employeeID, companyUserID := auth.ActorIDs(principal)
	if len(plan) <= maxBatchedTakes {
		// Per-line largest-remainder amounts over the line's takes.
		amountsByLine := make(map[int][]money.Piastres, len(items))
		takesByLine := make(map[int][]int64, len(items))
		for _, t := range plan {
			takesByLine[t.lineIdx] = append(takesByLine[t.lineIdx], t.take)
		}
		for lineIdx, takes := range takesByLine {
			amountsByLine[lineIdx] = money.Allocate(lineAmounts[lineIdx], takes)
		}

		// 2. sale_items — one multi-row INSERT. The identity snapshot
		// columns (migration 49) freeze the effective product identity at
		// sale time on every row.
		var sb strings.Builder
		args := make([]interface{}, 0, len(plan)*15)
		sb.WriteString(`
			INSERT INTO sale_items (
				sale_id, pharmacy_product_id, batch_id, sale_unit,
				quantity, base_quantity, sale_quantity, units_per_box_snapshot,
				unit_price, unit_cost, amount_piastres, cost_amount_piastres,
				product_name_snapshot, product_barcode_snapshot, product_strength_snapshot
			) VALUES `)
		p := 1
		for k, t := range plan {
			item := items[t.lineIdx]
			snapshot := snapshots[t.lineIdx]
			unitPrice := snapshot.EffectiveUnitPrice(item.SaleUnit)
			amount := amountsByLine[t.lineIdx][t.ordinal]
			costAmount := t.batch.costPerUnit.MulQty(t.take)
			if k > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
				p, p+1, p+2, p+3, p+4, p+5, p+6, p+7, p+8, p+9, p+10, p+11, p+12, p+13, p+14)
			p += 15
			args = append(args, saleID, item.PharmacyProductID, t.batch.id, item.SaleUnit,
				t.take, t.take, item.Quantity, snapshot.UnitsPerBox,
				unitPrice, t.batch.costPerUnit, amount, costAmount,
				snapshot.Name, snapshot.Barcode, snapshot.Strength)
		}
		if _, err := tx.Exec(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("sale_items insert: %w", err)
		}

		// 3. stock_movements — one multi-row INSERT (append-only ledger).
		sb.Reset()
		args = make([]interface{}, 0, len(plan)*13)
		sb.WriteString(`
			INSERT INTO stock_movements (
				batch_id, movement_type, quantity, unit, reference_type,
				reference_id, quantity_before, quantity_after, unit_cost,
				total_cost, created_by, created_by_company_user_id, reason
			) VALUES `)
		p = 1
		for k, t := range plan {
			costAmount := t.batch.costPerUnit.MulQty(t.take)
			if k > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, "($%d,'sale'::movement_type,$%d,$%d::unit_type,'sale',$%d,$%d,$%d,$%d,$%d,NULLIF($%d,'')::uuid,NULLIF($%d,'')::uuid,'pos_sale')",
				p, p+1, p+2, p+3, p+4, p+5, p+6, p+7, p+8, p+9)
			p += 10
			args = append(args, t.batch.id, -t.take, t.batch.baseUnit, saleID,
				t.before, t.after, t.batch.costPerUnit, -int64(costAmount),
				employeeID, companyUserID)
		}
		if _, err := tx.Exec(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("stock_movements insert: %w", err)
		}

		// 4. inventory_batches.quantity — one UPDATE ... FROM (VALUES).
		// The LAST take per batch in plan order carries the final value.
		finalQty := make(map[string]int64, len(plan))
		order := make([]string, 0, len(plan))
		for _, t := range plan {
			if _, ok := finalQty[t.batch.id]; !ok {
				order = append(order, t.batch.id)
			}
			finalQty[t.batch.id] = t.after
		}
		sb.Reset()
		args = make([]interface{}, 0, len(order)*2)
		sb.WriteString(`UPDATE inventory_batches AS ib SET quantity = v.qty, updated_at = NOW() FROM (VALUES `)
		p = 1
		for k, id := range order {
			if k > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, "($%d::uuid,$%d::numeric(12,4))", p, p+1)
			p += 2
			args = append(args, id, finalQty[id])
		}
		sb.WriteString(") AS v(id, qty) WHERE ib.id = v.id")
		if _, err := tx.Exec(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("inventory_batches update: %w", err)
		}

		// 5. pharmacy_products.last_sold_at — one UPDATE for all lines.
		if _, err := tx.Exec(ctx, `
			UPDATE pharmacy_products SET last_sold_at = NOW(), updated_at = NOW()
			WHERE id = ANY($1::uuid[])
		`, distinct); err != nil {
			return fmt.Errorf("last_sold_at update: %w", err)
		}
		return nil
	}

	// Fallback (pathological carts): identical plan, per-take statements —
	// exactly the pre-R-5 write shape, so no parameter ceiling is hit.
	amountsByLine := make(map[int][]money.Piastres, len(items))
	takesByLine := make(map[int][]int64, len(items))
	for _, t := range plan {
		takesByLine[t.lineIdx] = append(takesByLine[t.lineIdx], t.take)
	}
	for lineIdx, takes := range takesByLine {
		amountsByLine[lineIdx] = money.Allocate(lineAmounts[lineIdx], takes)
	}
	ordinals := make(map[int]int, len(items)) // lineIdx -> takes consumed
	for _, t := range plan {
		item := items[t.lineIdx]
		snapshot := snapshots[t.lineIdx]
		unitPrice := snapshot.EffectiveUnitPrice(item.SaleUnit)
		ordinal := ordinals[t.lineIdx]
		ordinals[t.lineIdx] = ordinal + 1
		amount := amountsByLine[t.lineIdx][ordinal]
		costAmount := t.batch.costPerUnit.MulQty(t.take)
		if _, err := tx.Exec(ctx, `
			INSERT INTO sale_items (
				sale_id, pharmacy_product_id, batch_id, sale_unit,
				quantity, base_quantity, sale_quantity, units_per_box_snapshot,
				unit_price, unit_cost, amount_piastres, cost_amount_piastres,
				product_name_snapshot, product_barcode_snapshot, product_strength_snapshot
			) VALUES ($1, $2, $3, $4, $5, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		`, saleID, item.PharmacyProductID, t.batch.id, item.SaleUnit,
			t.take, item.Quantity, snapshot.UnitsPerBox,
			unitPrice, t.batch.costPerUnit, amount, costAmount,
			snapshot.Name, snapshot.Barcode, snapshot.Strength); err != nil {
			return fmt.Errorf("sale_items insert (per-take): %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO stock_movements (
				batch_id, movement_type, quantity, unit, reference_type,
				reference_id, quantity_before, quantity_after, unit_cost,
				total_cost, created_by, created_by_company_user_id, reason
			) VALUES ($1, 'sale'::movement_type, $2, $3::unit_type, 'sale', $4,
				  $5, $6, $7, $8, NULLIF($9, '')::uuid, NULLIF($10, '')::uuid, 'pos_sale')
		`, t.batch.id, -t.take, t.batch.baseUnit, saleID,
			t.before, t.after, t.batch.costPerUnit, -int64(costAmount),
			employeeID, companyUserID); err != nil {
			return fmt.Errorf("stock_movements insert (per-take): %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE inventory_batches SET quantity = $2, updated_at = NOW() WHERE id = $1`,
			t.batch.id, t.after); err != nil {
			return fmt.Errorf("inventory_batches update (per-take): %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE pharmacy_products SET last_sold_at = NOW(), updated_at = NOW() WHERE id = ANY($1::uuid[])
	`, distinct); err != nil {
		return fmt.Errorf("last_sold_at update: %w", err)
	}
	return nil
}
