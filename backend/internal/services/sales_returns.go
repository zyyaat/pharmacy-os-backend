// ADR-0005 (api_level 109) — ledger item 2: the POS returns (credit notes)
// path moves out of sales_history_handler.go under the layering policy. The
// code is moved, not rewritten: same statements, same lock ordering, same
// largest-remainder refund allocation, same error identities — the handler
// maps each typed error 1:1 onto the exact wire body it emitted before
// (status, error code, Arabic message, keys).
// The wire contract is frozen during extraction (ADR-0005 rule 3): mobile
// APKs are not exported from this repo and a stale install must keep
// checking out — any "small" wire drift here is a pharmacy session outage
// tomorrow.
//
// Ownership split (ADR-0005 rule 2): THIS package owns the return
// transaction (BEGIN/lease set_config/COMMIT), the serialized concurrency
// (FOR UPDATE on the invoice BEFORE reading returned aggregates), the
// idempotent replay of a retried credit note, the largest-remainder refund
// computation on the remaining amount, the exact-batch restock and its
// stock_movements chain, and the partially_returned/returned status machine.
// The handler keeps protocol only: binding, request-shape validation
// (item bounds, reason length, key shape, duplicates), HTTP statuses and
// response bodies.
//
// All money in this file is money.Piastres: an integer count of piastres
// (1 EGP = 100 piastres). No floating point value ever touches an amount.
//
// Return rules (unchanged, audit-safe):
//   - The original invoice is NEVER mutated; every return is its own
//     credit-note document (sale_returns) shown as a reverse invoice.
//   - Returned strips go back to the EXACT batches they were sold from
//     (sale_items.batch_id), so batch traceability is preserved.
//   - Refund amounts are computed from the sale row's remaining amount
//     with the same largest-remainder allocation used at sale time, so
//     chained partial returns always converge to the exact line amount.
//   - A client-supplied idempotency key makes retries safe; concurrent
//     returns are serialized by locking the sale row BEFORE reading the
//     already-returned aggregates (fresh snapshot after the wait).
package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pharmacy-os/backend/internal/auth"
	"github.com/pharmacy-os/backend/internal/money"
)

// POSReturnItemRequest is one credit-note line: the sold row and the base
// quantity (strips) to take back.
type POSReturnItemRequest struct {
	SaleItemID string `json:"sale_item_id"`
	Quantity   int64  `json:"quantity"`
}

// POSReturnRequest is the wire request of POST /pharmacy/pos/sales/:sale_id/returns.
// Field tags are the frozen wire vocabulary — the protocol edge binds this
// struct directly.
type POSReturnRequest struct {
	Items          []POSReturnItemRequest `json:"items"`
	Reason         string                 `json:"reason"`
	IdempotencyKey string                 `json:"idempotency_key"`
}

// POSReturnItemResult mirrors one accepted return line in the 201 body
// ("data.items") — the handler maps the fields onto the same gin.H keys.
type POSReturnItemResult struct {
	SaleItemID string
	Quantity   int64
	Amount     money.Piastres
}

// POSReturnOutcome is the service-level result of one accepted credit note.
// Replayed=true means the idempotent path served the ORIGINAL credit note
// (no second refund); the handler emits a 200 body WITHOUT the items key,
// exactly as the pre-extraction replay branch did. SaleStatus on the replay
// path is best-effort (empty string when the status read fails), matching
// the old saleStatusByID helper.
type POSReturnOutcome struct {
	ReturnID     string
	ReturnNumber int64
	Total        money.Piastres
	SaleStatus   string
	Replayed     bool
	Items        []POSReturnItemResult
}

// Business rejects — the handler maps each onto the exact pre-extraction
// body. Do not merge sentinels that carry different messages; do not split
// sentinels that share one.
var (
	// ErrPOSReturnSaleNotFound: the invoice does not exist in the caller's
	// pharmacy (or the id is foreign) — 404 sale_not_found. Shared by the
	// detail read, which serves the same body.
	ErrPOSSaleNotFound = errors.New("sale not found in this pharmacy")
	// ErrPOSReturnNoItems guards direct service callers against an empty
	// credit note (the protocol edge rejects it earlier with the same
	// invalid_return body).
	ErrPOSReturnNoItems = errors.New("POS return requires at least one item")
	// ErrPOSReturnItemNotFound: a requested sale_item does not belong to
	// this invoice — 404 return_item_not_found.
	ErrPOSReturnItemNotFound = errors.New("sale item does not belong to this invoice")
)

// POSReturnExceedsSold is the 409 return_exceeds_sold payload: the requested
// quantity is larger than what is still returnable on that row. The handler
// maps the fields onto the same "data" keys verbatim.
type POSReturnExceedsSold struct {
	SaleItemID             string
	ReturnableQuantityBase int64
}

func (e *POSReturnExceedsSold) Error() string {
	return fmt.Sprintf("return exceeds sold: sale_item %s returnable %d", e.SaleItemID, e.ReturnableQuantityBase)
}

// POSReturnStage identifies where inside the return transaction a failure
// happened, so the handler can emit the same stage-specific message the
// pre-extraction branch emitted, while the real cause lands in the log —
// never on the wire.
type POSReturnStage int

const (
	POSReturnStageBegin        POSReturnStage = iota // pool.Begin
	POSReturnStageLease                              // set_config('app.current_pharmacy_id'...)
	POSReturnStageReplayCheck                        // idempotency pre-read
	POSReturnStageSaleLock                           // SELECT ... FOR UPDATE on the invoice
	POSReturnStageItemsRead                          // sale_items + returned aggregates
	POSReturnStageInsert                             // INSERT INTO sale_returns
	POSReturnStageItemInsert                         // INSERT INTO sale_return_items
	POSReturnStageRestock                            // UPDATE inventory_batches
	POSReturnStageMovement                           // INSERT INTO stock_movements
	POSReturnStageStatusUpdate                       // UPDATE sales SET status
	POSReturnStageCommit                             // tx.Commit
)

type POSReturnFailure struct {
	Stage POSReturnStage
	Err   error
}

func (e *POSReturnFailure) Error() string {
	return fmt.Sprintf("pos return stage %d: %v", e.Stage, e.Err)
}

func (e *POSReturnFailure) Unwrap() error { return e.Err }

// CreatePOSSaleReturn owns the whole credit-note transaction: lease context
// (ADR-0002 — derived from the principal, never from the client), the
// idempotent replay, the invoice lock that serializes concurrent returns,
// the returnable-quantity validation, the largest-remainder refund
// computation, the credit-note insert, the exact-batch restock with its
// movement chain, the invoice status recompute and the commit. On any error
// the transaction is rolled back and nothing partial survives; the caller
// only translates errors to wire.
//
// Request-shape validation (item bounds, key length, duplicates) stays at
// the protocol edge; this function re-checks only what guards data
// integrity for direct service callers (empty credit note).
func CreatePOSSaleReturn(
	ctx context.Context,
	pool *pgxpool.Pool,
	principal *auth.Principal,
	saleID string,
	req POSReturnRequest,
) (POSReturnOutcome, error) {
	out := POSReturnOutcome{}
	if len(req.Items) == 0 {
		return out, ErrPOSReturnNoItems
	}

	employeeID, companyUserID := auth.ActorIDs(principal)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return out, &POSReturnFailure{Stage: POSReturnStageBegin, Err: err}
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
                SELECT set_config('app.current_pharmacy_id', $1, true),
                       set_config('app.current_user_id', $2, true)
        `, principal.PharmacyID, principal.ID); err != nil {
		return out, &POSReturnFailure{Stage: POSReturnStageLease, Err: err}
	}

	// Idempotent replay: retrying the same return returns the original
	// credit note instead of double-refunding.
	if req.IdempotencyKey != "" {
		var existingID string
		var existingNumber int64
		var existingTotal money.Piastres
		err := tx.QueryRow(ctx, `
                        SELECT r.id::text, r.return_number::int8, r.total_amount_piastres::int8
                        FROM sale_returns r
                        WHERE r.pharmacy_id = $1 AND r.idempotency_key = $2
                `, principal.PharmacyID, req.IdempotencyKey).Scan(&existingID, &existingNumber, &existingTotal)
		if err == nil {
			return POSReturnOutcome{
				ReturnID:     existingID,
				ReturnNumber: existingNumber,
				Total:        existingTotal,
				SaleStatus:   saleStatusByID(ctx, tx, existingID),
				Replayed:     true,
			}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return out, &POSReturnFailure{Stage: POSReturnStageReplayCheck, Err: err}
		}
	}

	// Lock the invoice FIRST. Concurrent returns of the same invoice then
	// serialize here, and the returned-aggregates query below runs on a
	// fresh snapshot AFTER the wait, so it always sees committed returns.
	var saleInternalID, branchID string
	err = tx.QueryRow(ctx, `
                SELECT id::text, branch_id::text
                FROM sales
                WHERE id = $1::uuid AND pharmacy_id = $2
                FOR UPDATE
        `, saleID, principal.PharmacyID).Scan(&saleInternalID, &branchID)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrPOSSaleNotFound
	}
	if err != nil {
		return out, &POSReturnFailure{Stage: POSReturnStageSaleLock, Err: err}
	}

	type saleRow struct {
		id             string
		productID      string
		batchID        string
		quantityBase   int64
		amount         money.Piastres
		unitCost       money.Piastres
		returnedQty    int64
		returnedAmount int64
	}
	rows, err := tx.Query(ctx, `
                SELECT si.id::text, si.pharmacy_product_id::text, si.batch_id::text,
                       ROUND(si.quantity)::int8, si.amount_piastres::int8,
                       COALESCE(si.unit_cost::int8, 0),
                       COALESCE(rb.returned_qty, 0)::int8,
                       COALESCE(rb.returned_amount, 0)::int8
                FROM sale_items si
                LEFT JOIN (
                    SELECT sale_item_id,
                           SUM(quantity) AS returned_qty,
                           SUM(amount_piastres) AS returned_amount
                    FROM sale_return_items
                    GROUP BY sale_item_id
                ) rb ON rb.sale_item_id = si.id
                WHERE si.sale_id = $1::uuid
        `, saleID)
	if err != nil {
		return out, &POSReturnFailure{Stage: POSReturnStageItemsRead, Err: err}
	}
	itemsBySaleItem := make(map[string]*saleRow)
	allRows := make([]*saleRow, 0)
	for rows.Next() {
		var r saleRow
		if err := rows.Scan(&r.id, &r.productID, &r.batchID, &r.quantityBase, &r.amount,
			&r.unitCost, &r.returnedQty, &r.returnedAmount); err != nil {
			rows.Close()
			return out, &POSReturnFailure{Stage: POSReturnStageItemsRead, Err: err}
		}
		itemsBySaleItem[r.id] = &r
		allRows = append(allRows, &r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, &POSReturnFailure{Stage: POSReturnStageItemsRead, Err: err}
	}

	// Validate every requested quantity against what is actually returnable
	// BEFORE mutating anything, and compute the exact refund per row.
	type plannedReturn struct {
		row      *saleRow
		quantity int64
		refund   money.Piastres
	}
	planned := make([]plannedReturn, 0, len(req.Items))
	var totalRefund money.Piastres
	for _, item := range req.Items {
		row, ok := itemsBySaleItem[item.SaleItemID]
		if !ok {
			return out, ErrPOSReturnItemNotFound
		}
		returnable := row.quantityBase - row.returnedQty
		if item.Quantity > returnable {
			return out, &POSReturnExceedsSold{SaleItemID: item.SaleItemID, ReturnableQuantityBase: returnable}
		}
		remainingQty := returnable
		remainingAmount := money.Piastres(row.amount) - money.Piastres(row.returnedAmount)
		if remainingAmount < 0 {
			remainingAmount = 0
		}
		var refund money.Piastres
		if item.Quantity == remainingQty {
			// The row is fully returned now: refund exactly what is left.
			refund = remainingAmount
		} else {
			// Partial return: split the remaining amount between the
			// returned and kept strips with the largest-remainder method;
			// the two parts always sum to the remaining amount exactly.
			parts := money.Allocate(remainingAmount, []int64{item.Quantity, remainingQty - item.Quantity})
			refund = parts[0]
		}
		planned = append(planned, plannedReturn{row: row, quantity: item.Quantity, refund: refund})
		totalRefund = totalRefund.Add(refund)
	}

	var returnID string
	var returnNumber int64
	insertErr := tx.QueryRow(ctx, `
                INSERT INTO sale_returns (pharmacy_id, branch_id, sale_id, total_amount_piastres, reason, employee_id, company_user_id, idempotency_key)
                VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')::uuid, NULLIF($7, '')::uuid, NULLIF($8, ''))
                RETURNING id::text, return_number::int8
        `, principal.PharmacyID, branchID, saleID, totalRefund, req.Reason, employeeID, companyUserID, req.IdempotencyKey).
		Scan(&returnID, &returnNumber)
	if insertErr != nil {
		return out, &POSReturnFailure{Stage: POSReturnStageInsert, Err: insertErr}
	}

	responseItems := make([]POSReturnItemResult, 0, len(planned))
	for _, p := range planned {
		if _, err := tx.Exec(ctx, `
                        INSERT INTO sale_return_items (return_id, sale_item_id, pharmacy_product_id, batch_id, quantity, amount_piastres)
                        VALUES ($1, $2, $3, $4, $5, $6)
                `, returnID, p.row.id, p.row.productID, p.row.batchID, p.quantity, p.refund); err != nil {
			return out, &POSReturnFailure{Stage: POSReturnStageItemInsert, Err: err}
		}

		// Restock into the EXACT batch the strips were sold from, and write
		// the matching movement at the original cost of that sale row so
		// inventory value is reversed consistently.
		var quantityBefore, quantityAfter int64
		var batchUnit string
		restockErr := tx.QueryRow(ctx, `
                        UPDATE inventory_batches
                        SET quantity = quantity + $2, updated_at = NOW()
                        WHERE id = $1::uuid
                        RETURNING (ROUND(quantity) - $2)::int8, ROUND(quantity)::int8, COALESCE(unit::text, 'other')
                `, p.row.batchID, p.quantity).Scan(&quantityBefore, &quantityAfter, &batchUnit)
		if restockErr != nil {
			return out, &POSReturnFailure{Stage: POSReturnStageRestock, Err: restockErr}
		}
		costAmount := p.row.unitCost.MulQty(p.quantity)
		if _, err := tx.Exec(ctx, `
                        INSERT INTO stock_movements (
                                batch_id, movement_type, quantity, unit, reference_type,
                                reference_id, quantity_before, quantity_after, unit_cost,
                                total_cost, created_by, created_by_company_user_id, reason
                        ) VALUES ($1, 'return_from_customer'::movement_type, $2, $3::unit_type, 'sale_return', $4,
                                  $5, $6, $7, $8, NULLIF($9, '')::uuid, NULLIF($10, '')::uuid, 'pos_return')
                `, p.row.batchID, p.quantity, batchUnit, returnID,
			quantityBefore, quantityAfter, p.row.unitCost, costAmount, employeeID, companyUserID); err != nil {
			return out, &POSReturnFailure{Stage: POSReturnStageMovement, Err: err}
		}

		p.row.returnedQty += p.quantity
		p.row.returnedAmount += int64(p.refund)
		responseItems = append(responseItems, POSReturnItemResult{
			SaleItemID: p.row.id,
			Quantity:   p.quantity,
			Amount:     p.refund,
		})
	}

	// Recompute the invoice status from the authoritative rows.
	newStatus := "partially_returned"
	fullyReturned := true
	for _, row := range allRows {
		if row.quantityBase-row.returnedQty > 0 {
			fullyReturned = false
			break
		}
	}
	if fullyReturned {
		newStatus = "returned"
	}
	if _, err := tx.Exec(ctx, `
                UPDATE sales SET status = $2 WHERE id = $1::uuid
        `, saleID, newStatus); err != nil {
		return out, &POSReturnFailure{Stage: POSReturnStageStatusUpdate, Err: err}
	}

	if err := tx.Commit(ctx); err != nil {
		return out, &POSReturnFailure{Stage: POSReturnStageCommit, Err: err}
	}
	return POSReturnOutcome{
		ReturnID:     returnID,
		ReturnNumber: returnNumber,
		Total:        totalRefund,
		SaleStatus:   newStatus,
		Replayed:     false,
		Items:        responseItems,
	}, nil
}

// saleStatusByID reads the current status of the sale behind a credit note
// on the open transaction (the idempotent replay path). Best-effort: the
// pre-extraction helper swallowed the error into "" and the wire body keeps
// that exact behavior.
func saleStatusByID(ctx context.Context, tx pgx.Tx, returnID string) string {
	var status string
	err := tx.QueryRow(ctx, `
                SELECT s.status::text FROM sales s
                JOIN sale_returns r ON r.sale_id = s.id
                WHERE r.id = $1::uuid
        `, returnID).Scan(&status)
	if err != nil {
		return ""
	}
	return status
}

// ---- Invoice detail read (GET /pharmacy/pos/sales/:sale_id) ---------------
//
// The detail read feeds the returns UI: one row per batch line with
// everything already returned, so the client can offer exact returnable
// quantities per row. Typed rows leave the service; the handler maps them
// onto the same gin.H keys (ADR-0005 rule 2 — protocol owns the shape).

// POSSaleHeader is the invoice-level row of the detail read.
type POSSaleHeader struct {
	ID             string
	InvoiceNumber  int64
	Status         string
	TotalAmount    money.Piastres
	DiscountAmount money.Piastres
	PaymentType    string
	CustomerName   string
	CreatedAt      time.Time
}

// POSSaleReturnSummary is one credit note attached to the invoice.
type POSSaleReturnSummary struct {
	ID           string
	ReturnNumber int64
	Total        money.Piastres
	Reason       string
	CreatedAt    time.Time
	QuantityBase int64
}

// POSSaleLine is one invoice row (per batch line) with its returned
// aggregates. QuantityBase is the base-unit quantity (ROUND(si.quantity)).
type POSSaleLine struct {
	SaleItemID          string
	ProductID           string
	ProductName         string
	GenericName         string
	Strength            string
	Barcode             string
	PackagingType       string
	UnitsPerBox         int64
	SaleUnit            string
	BatchNumber         string
	QuantityBase        int64
	UnitPrice           money.Piastres
	Amount              money.Piastres
	ReturnedQty         int64
	ReturnedAmount      int64
	SaleQuantity        *float64
	UnitsPerBoxSnapshot *int64
}

// POSSaleDetail is the full service-level result of the detail read.
// ReturnedTotal is the sum of the attached credit notes — the value the
// wire exposes as returned_amount_piastres on the sale object.
type POSSaleDetail struct {
	Sale          POSSaleHeader
	Returns       []POSSaleReturnSummary
	Items         []POSSaleLine
	ReturnedTotal int64
}

// POSSaleDetailStage identifies which read of the detail endpoint failed.
// All three map onto the same wire body (sale_query_failed) — the stage
// only sharpens the server log.
type POSSaleDetailStage int

const (
	POSSaleDetailHeaderRead POSSaleDetailStage = iota
	POSSaleDetailReturnsRead
	POSSaleDetailItemsRead
)

type POSSaleDetailFailure struct {
	Stage POSSaleDetailStage
	Err   error
}

func (e *POSSaleDetailFailure) Error() string {
	return fmt.Sprintf("pos sale detail stage %d: %v", e.Stage, e.Err)
}

func (e *POSSaleDetailFailure) Unwrap() error { return e.Err }

// POSSaleDetailByID reads one invoice with row-level detail (one row per
// batch line) plus everything already returned, so the UI can offer exact
// returnable quantities per row. Returns ErrPOSSaleNotFound for an invoice
// that does not exist in the pharmacy.
func POSSaleDetailByID(
	ctx context.Context,
	pool *pgxpool.Pool,
	pharmacyID string,
	saleID string,
) (POSSaleDetail, error) {
	out := POSSaleDetail{}

	var header POSSaleHeader
	err := pool.QueryRow(ctx, `
                SELECT s.id::text, s.invoice_number::int8, s.status::text, s.total_amount::int8, s.created_at,
                       COALESCE(s.discount_amount::int8, 0), COALESCE(s.payment_type::text, 'cash'),
                       COALESCE(c.name::text, '')
                FROM sales s
                LEFT JOIN customers c ON c.id = s.customer_id
                WHERE s.id = $1::uuid AND s.pharmacy_id = $2
        `, saleID, pharmacyID).Scan(&header.ID, &header.InvoiceNumber, &header.Status, &header.TotalAmount,
		&header.CreatedAt, &header.DiscountAmount, &header.PaymentType, &header.CustomerName)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrPOSSaleNotFound
	}
	if err != nil {
		return out, &POSSaleDetailFailure{Stage: POSSaleDetailHeaderRead, Err: err}
	}
	out.Sale = header

	returns := make([]POSSaleReturnSummary, 0)
	returnRows, err := pool.Query(ctx, `
                SELECT r.id::text, r.return_number::int8, r.total_amount_piastres::int8,
                       r.reason, r.created_at,
                       COALESCE(
                           (SELECT SUM(ri.quantity)::int8 FROM sale_return_items ri WHERE ri.return_id = r.id), 0
                       )::int8
                FROM sale_returns r
                WHERE r.sale_id = $1::uuid
                ORDER BY r.created_at ASC, r.return_number ASC
        `, saleID)
	if err != nil {
		return out, &POSSaleDetailFailure{Stage: POSSaleDetailReturnsRead, Err: err}
	}
	for returnRows.Next() {
		var r POSSaleReturnSummary
		if err := returnRows.Scan(&r.ID, &r.ReturnNumber, &r.Total, &r.Reason, &r.CreatedAt,
			&r.QuantityBase); err != nil {
			returnRows.Close()
			return out, &POSSaleDetailFailure{Stage: POSSaleDetailReturnsRead, Err: err}
		}
		returns = append(returns, r)
	}
	returnRows.Close()
	if err := returnRows.Err(); err != nil {
		return out, &POSSaleDetailFailure{Stage: POSSaleDetailReturnsRead, Err: err}
	}
	out.Returns = returns

	items := make([]POSSaleLine, 0)
	// Identity display (migration 49): the invoice shows what it recorded at
	// sale time — the frozen snapshot — and only rows written before the
	// snapshot existed fall through to the live chain (local override first,
	// official catalog last), like migration 24's legacy fallback.
	itemRows, err := pool.Query(ctx, `
                SELECT si.id::text,
                       si.pharmacy_product_id::text,
                       COALESCE(si.product_name_snapshot, pp.name_override, gp.name::text, ''),
                       COALESCE(gp.generic_name::text, ''),
                       COALESCE(si.product_strength_snapshot, pp.strength_override, gp.strength::text, ''),
                       COALESCE(si.product_barcode_snapshot, pp.barcode_override, gp.barcode::text, ''),
                       COALESCE(pp.packaging_type::text, ''),
                       COALESCE(pp.units_per_box::int8, 1),
                       si.sale_unit::text,
                       COALESCE(ib.batch_number::text, ''),
                       ROUND(si.quantity)::int8,
                       si.unit_price::int8,
                       si.amount_piastres::int8,
                       COALESCE(rb.returned_qty, 0)::int8,
                       COALESCE(rb.returned_amount, 0)::int8,
                       si.sale_quantity::float8,
                       si.units_per_box_snapshot::int8
                FROM sale_items si
                JOIN pharmacy_products pp ON pp.id = si.pharmacy_product_id
                LEFT JOIN global_products gp ON gp.id = pp.global_product_id
                LEFT JOIN inventory_batches ib ON ib.id = si.batch_id
                LEFT JOIN (
                    SELECT sale_item_id,
                           SUM(quantity) AS returned_qty,
                           SUM(amount_piastres) AS returned_amount
                    FROM sale_return_items
                    GROUP BY sale_item_id
                ) rb ON rb.sale_item_id = si.id
                WHERE si.sale_id = $1::uuid
                ORDER BY si.created_at, si.id
        `, saleID)
	if err != nil {
		return out, &POSSaleDetailFailure{Stage: POSSaleDetailItemsRead, Err: err}
	}
	for itemRows.Next() {
		var line POSSaleLine
		if err := itemRows.Scan(&line.SaleItemID, &line.ProductID, &line.ProductName, &line.GenericName,
			&line.Strength, &line.Barcode, &line.PackagingType, &line.UnitsPerBox, &line.SaleUnit,
			&line.BatchNumber, &line.QuantityBase, &line.UnitPrice, &line.Amount,
			&line.ReturnedQty, &line.ReturnedAmount, &line.SaleQuantity, &line.UnitsPerBoxSnapshot); err != nil {
			itemRows.Close()
			return out, &POSSaleDetailFailure{Stage: POSSaleDetailItemsRead, Err: err}
		}
		items = append(items, line)
	}
	itemRows.Close()
	if err := itemRows.Err(); err != nil {
		return out, &POSSaleDetailFailure{Stage: POSSaleDetailItemsRead, Err: err}
	}
	out.Items = items

	for _, r := range returns {
		out.ReturnedTotal += int64(r.Total)
	}
	return out, nil
}
