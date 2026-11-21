// Package repository provides database access for Stock Movements
// This handles the SOURCE OF TRUTH for all inventory quantity changes
package repository

import (
        "context"
        "errors"
        "fmt"
        "math"
        "strings"
        "time"

        "github.com/jackc/pgx/v5"
        "github.com/jackc/pgx/v5/pgxpool"

        "github.com/pharmacy-os/backend/internal/money"
        "github.com/pharmacy-os/backend/internal/models"
)

// StockMovementRepository handles database operations for stock movements
type StockMovementRepository struct {
        pool *pgxpool.Pool
}

var (
        ErrInventoryBatchNotFound   = errors.New("inventory batch not found")
        ErrInventoryHistoryRequired = errors.New("inventory movement history is required")
        ErrInsufficientStock        = errors.New("insufficient stock")
        ErrIdempotencyConflict      = errors.New("idempotency key was already used for another mutation")
        ErrInvalidStockAdjustment   = errors.New("invalid stock adjustment")
)

// StockAdjustmentInput describes one atomic inventory adjustment.
// PharmacyID, BranchID, and actor IDs are always derived from the
// authenticated principal by the handler; they are never accepted from JSON.
type StockAdjustmentInput struct {
        BatchID        string
        PharmacyID     string
        BranchID       string
        EmployeeID     string
        CompanyUserID  string
        Delta          float64
        IdempotencyKey string
        Reason         string
        IPAddress      string
        UserAgent      string
}

type StockAdjustmentResult struct {
        MovementID       string
        BatchID          string
        Unit             string
        PreviousQuantity float64
        NewQuantity      float64
        CreatedAt        time.Time
        Replayed         bool
}

// NewStockMovementRepository creates a new stock movement repository
func NewStockMovementRepository(pool *pgxpool.Pool) *StockMovementRepository {
        return &StockMovementRepository{pool: pool}
}

// AdjustBatchStock performs a retry-safe adjustment in one transaction.
// The batch is locked before checking the idempotency key, so concurrent
// requests using the same key serialize even when the first request has not
// committed yet.
func (r *StockMovementRepository) AdjustBatchStock(ctx context.Context, input StockAdjustmentInput) (*StockAdjustmentResult, error) {
        if err := validateStockAdjustmentInput(input); err != nil {
                return nil, err
        }

        tx, err := r.pool.Begin(ctx)
        if err != nil {
                return nil, fmt.Errorf("begin inventory adjustment transaction: %w", err)
        }
        defer func() { _ = tx.Rollback(ctx) }()

        actorID := input.EmployeeID
        if actorID == "" {
                actorID = input.CompanyUserID
        }

        // Serialize reuse of the same key by the same actor, even when
        // two requests target different batches.
        if _, err := tx.Exec(ctx,
                `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
                actorID+":"+input.IdempotencyKey,
        ); err != nil {
                return nil, fmt.Errorf("lock idempotency key: %w", err)
        }

        // Keep RLS policies correct for this transaction and never rely on a
        // connection-level setting in the pool.
        if _, err := tx.Exec(ctx, `
                SELECT set_config('app.current_pharmacy_id', $1, true),
                       set_config('app.current_user_id', $2, true)
        `, input.PharmacyID, actorID); err != nil {
                return nil, fmt.Errorf("set inventory tenant context: %w", err)
        }

        var (
                batchID, unit   string
                currentQuantity float64
        )
        err = tx.QueryRow(ctx, `
                SELECT ib.id::text, ib.unit::text, ib.quantity::float8
                FROM inventory_batches ib
                JOIN pharmacy_products pp ON pp.id = ib.pharmacy_product_id
                WHERE ib.id = $1
                  AND pp.pharmacy_id = $2
                  AND ($3 = '' OR ib.branch_id::text = $3)
                FOR UPDATE
        `, input.BatchID, input.PharmacyID, input.BranchID).
                Scan(&batchID, &unit, &currentQuantity)
        if errors.Is(err, pgx.ErrNoRows) {
                return nil, ErrInventoryBatchNotFound
        }
        if err != nil {
                return nil, fmt.Errorf("lock inventory batch: %w", err)
        }

        var movementCount int
        var movementStock float64
        if err := tx.QueryRow(ctx, `
                SELECT COUNT(*)::int, COALESCE(SUM(quantity), 0)::float8
                FROM stock_movements
                WHERE batch_id = $1
        `, input.BatchID).Scan(&movementCount, &movementStock); err != nil {
                return nil, fmt.Errorf("read inventory movement history: %w", err)
        }
        if movementCount == 0 || math.Abs(movementStock-currentQuantity) > 0.0001 {
                // Do not silently create a ledger that disagrees with the
                // existing cached batch quantity. A receiving/opening-balance
                // flow must establish the ledger baseline first.
                return nil, ErrInventoryHistoryRequired
        }
        currentQuantity = movementStock

        var existing StockAdjustmentResult
        var existingDelta, existingQuantityBefore, existingQuantityAfter float64
        err = tx.QueryRow(ctx, `
                SELECT id::text, batch_id::text, unit::text,
                       quantity::float8, quantity_before::float8,
                       quantity_after::float8, created_at
                FROM stock_movements
                WHERE idempotency_key = $1
                  AND (
                    ($2 <> '' AND created_by = NULLIF($2, '')::uuid)
                    OR ($3 <> '' AND created_by_company_user_id = NULLIF($3, '')::uuid)
                  )
                FOR UPDATE
        `, input.IdempotencyKey, input.EmployeeID, input.CompanyUserID).Scan(
                &existing.MovementID, &existing.BatchID, &existing.Unit,
                &existingDelta, &existingQuantityBefore,
                &existingQuantityAfter, &existing.CreatedAt,
        )
        if err == nil {
                if existing.BatchID != input.BatchID ||
                        math.Abs(existingDelta-input.Delta) > 0.0001 {
                        return nil, ErrIdempotencyConflict
                }
                existing.PreviousQuantity = existingQuantityBefore
                existing.NewQuantity = existingQuantityAfter
                existing.Replayed = true
                return &existing, nil
        }
        if !errors.Is(err, pgx.ErrNoRows) {
                return nil, fmt.Errorf("check idempotency key: %w", err)
        }

        newQuantity := currentQuantity + input.Delta
        if newQuantity < 0 {
                return nil, ErrInsufficientStock
        }

        var movementID string
        var createdAt time.Time
        err = tx.QueryRow(ctx, `
                INSERT INTO stock_movements (
                        batch_id, movement_type, quantity, unit,
                        quantity_before, quantity_after, created_by, created_by_company_user_id,
                        reason, ip_address, user_agent, idempotency_key
                ) VALUES (
                        $1, 'adjustment', $2, $3,
                        $4, $5, NULLIF($6, '')::uuid, NULLIF($7, '')::uuid,
                        NULLIF($8, ''), NULLIF($9, '')::inet, NULLIF($10, ''), $11
                )
                RETURNING id::text, created_at
        `, input.BatchID, input.Delta, unit, currentQuantity, newQuantity,
                input.EmployeeID, input.CompanyUserID, input.Reason, input.IPAddress,
                input.UserAgent, input.IdempotencyKey).Scan(&movementID, &createdAt)
        if err != nil {
                return nil, fmt.Errorf("insert stock movement: %w", err)
        }

        if _, err := tx.Exec(ctx, `
                UPDATE inventory_batches
                SET quantity = $2, updated_at = NOW()
                WHERE id = $1
        `, input.BatchID, newQuantity); err != nil {
                return nil, fmt.Errorf("update inventory batch quantity: %w", err)
        }

        if err := tx.Commit(ctx); err != nil {
                return nil, fmt.Errorf("commit inventory adjustment: %w", err)
        }
        return &StockAdjustmentResult{
                MovementID: movementID, BatchID: batchID, Unit: unit,
                PreviousQuantity: currentQuantity, NewQuantity: newQuantity,
                CreatedAt: createdAt,
        }, nil
}

// ---------------------------------------------------------------------------
// Opening stock (api_level 119)
// ---------------------------------------------------------------------------

var (
        // ErrOpeningStockProductNotFound — the product id does not belong to
        // the caller's pharmacy.
        ErrOpeningStockProductNotFound = errors.New("opening stock product not found")
        // ErrOpeningStockExists — the product already owns at least one batch;
        // receiving more stock is the adjust path, not a second opening.
        ErrOpeningStockExists = errors.New("opening stock already exists")
        // ErrInvalidOpeningStock — quantities inconsistent with the product's
        // packaging type (strips on a WHOLE_ONLY product, zero total, a
        // BOX_STRIP product with units_per_box < 2, or a runaway quantity).
        ErrInvalidOpeningStock = errors.New("invalid opening stock")
)

// OpeningStockInput describes one atomic first-batch creation.
// PharmacyID and actor IDs are always derived from the authenticated
// principal by the handler; they are never accepted from JSON.
type OpeningStockInput struct {
        ProductID         string
        PharmacyID        string
        BranchID          string
        EmployeeID        string
        CompanyUserID     string
        InitialBoxes      int64
        InitialStrips     int64
        CostPricePiastres int64 // per box (per pack for WHOLE_ONLY); half-up divided for strips
        BatchNumber       string
        ExpiryDate        string // '' or YYYY-MM-DD (validated by the handler)
        Reason            string
        IdempotencyKey    string
        IPAddress         string
        UserAgent         string
}

type OpeningStockResult struct {
        MovementID string
        BatchID    string
        Unit       string
        Quantity   int64
        CreatedAt  time.Time
        Replayed   bool
}

// OpenProductStock creates the FIRST inventory batch of a product that has
// none, in one transaction that mirrors the create-product opening path
// (product_pos_handler.go) column for column: an 'opening_balance' batch and
// a 'purchase' movement whose sum IS the cached quantity, so AdjustBatchStock's
// history gate accepts the batch immediately. Concurrency is anchored twice:
// pg_advisory_xact_lock serializes reuse of the same idempotency key, and a
// FOR UPDATE lock on the pharmacy_products row serializes two concurrent
// openings of the same product under DIFFERENT keys — the loser sees the
// winner's batch and answers ErrOpeningStockExists instead of double-opening.
func (r *StockMovementRepository) OpenProductStock(ctx context.Context, input OpeningStockInput) (*OpeningStockResult, error) {
        if err := validateOpeningStockInput(input); err != nil {
                return nil, err
        }

        tx, err := r.pool.Begin(ctx)
        if err != nil {
                return nil, fmt.Errorf("begin opening stock transaction: %w", err)
        }
        defer func() { _ = tx.Rollback(ctx) }()

        actorID := input.EmployeeID
        if actorID == "" {
                actorID = input.CompanyUserID
        }

        // Serialize reuse of the same key by the same actor (same pattern as
        // AdjustBatchStock, which also keys on actor+key across batches).
        if _, err := tx.Exec(ctx,
                `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
                actorID+":"+input.IdempotencyKey,
        ); err != nil {
                return nil, fmt.Errorf("lock idempotency key: %w", err)
        }

        // Keep RLS policies correct for this transaction and never rely on a
        // connection-level setting in the pool.
        if _, err := tx.Exec(ctx, `
                SELECT set_config('app.current_pharmacy_id', $1, true),
                       set_config('app.current_user_id', $2, true)
        `, input.PharmacyID, actorID); err != nil {
                return nil, fmt.Errorf("set opening stock tenant context: %w", err)
        }

        // Lock the product row: the NOT EXISTS race between two different keys
        // resolves on this lock, not on hope.
        var packagingType string
        var unitsPerBox int64
        err = tx.QueryRow(ctx, `
                SELECT pp.packaging_type::text, pp.units_per_box::int8
                FROM pharmacy_products pp
                WHERE pp.id = $1 AND pp.pharmacy_id = $2
                FOR UPDATE
        `, input.ProductID, input.PharmacyID).Scan(&packagingType, &unitsPerBox)
        if errors.Is(err, pgx.ErrNoRows) {
                return nil, ErrOpeningStockProductNotFound
        }
        if err != nil {
                return nil, fmt.Errorf("lock pharmacy product: %w", err)
        }

        // Packaging rules mirror CreatePharmacyProduct exactly.
        isBoxStrip := packagingType == "BOX_STRIP"
        if !isBoxStrip && input.InitialStrips != 0 {
                return nil, ErrInvalidOpeningStock
        }
        if isBoxStrip && unitsPerBox < 2 {
                return nil, ErrInvalidOpeningStock
        }
        baseQuantity := input.InitialBoxes*unitsPerBox + input.InitialStrips
        if baseQuantity <= 0 || baseQuantity > 1000000000 {
                return nil, ErrInvalidOpeningStock
        }
        unit := "box"
        if isBoxStrip {
                unit = "strip"
        }
        unitCost := money.Piastres(input.CostPricePiastres)
        if isBoxStrip {
                unitCost = unitCost.DivRoundHalfUp(unitsPerBox)
        }

        // Idempotent replay FIRST: the same actor+key that already opened THIS
        // product replays its result — the one-batch rule below must never
        // shadow the replay, or a client retry after a timeout answers 409.
        // The same key on a different product/quantity is a conflict (the
        // adjust contract, transplanted to openings).
        var existing OpeningStockResult
        var existingQuantity int64
        err = tx.QueryRow(ctx, `
                SELECT m.id::text, m.batch_id::text, m.quantity::int8, m.unit::text, m.created_at
                FROM stock_movements m
                JOIN inventory_batches ib ON ib.id = m.batch_id
                WHERE m.idempotency_key = $1
                  AND ib.pharmacy_product_id = $2
                  AND (
                    ($3 <> '' AND m.created_by = NULLIF($3, '')::uuid)
                    OR ($4 <> '' AND m.created_by_company_user_id = NULLIF($4, '')::uuid)
                  )
                FOR UPDATE OF m
        `, input.IdempotencyKey, input.ProductID, input.EmployeeID, input.CompanyUserID).
                Scan(&existing.MovementID, &existing.BatchID, &existingQuantity, &existing.Unit, &existing.CreatedAt)
        if err == nil {
                if existingQuantity != baseQuantity {
                        return nil, ErrIdempotencyConflict
                }
                existing.Quantity = existingQuantity
                existing.Replayed = true
                return &existing, nil
        }
        if !errors.Is(err, pgx.ErrNoRows) {
                return nil, fmt.Errorf("check idempotency key: %w", err)
        }

        // The one-batch rule: any existing batch (any branch) means the opening
        // already happened — adjust is the path now.
        var hasBatch bool
        if err := tx.QueryRow(ctx, `
                SELECT EXISTS (SELECT 1 FROM inventory_batches WHERE pharmacy_product_id = $1)
        `, input.ProductID).Scan(&hasBatch); err != nil {
                return nil, fmt.Errorf("check existing batches: %w", err)
        }
        if hasBatch {
                return nil, ErrOpeningStockExists
        }

        batchNumber := input.BatchNumber
        if batchNumber == "" {
                batchNumber = "OPENING-" + time.Now().UTC().Format("20060102150405")
        }
        reason := input.Reason
        if reason == "" {
                reason = "opening_balance"
        }

        var batchID string
        err = tx.QueryRow(ctx, `
                INSERT INTO inventory_batches (
                        pharmacy_product_id, branch_id, batch_number, quantity, unit,
                        cost_per_unit, expiry_date, received_by, reference_type
                ) VALUES ($1, $2, $3, $4, $5::unit_type, $6, NULLIF($7, '')::date, NULLIF($8, '')::uuid, 'opening_balance')
                RETURNING id::text
        `, input.ProductID, input.BranchID, batchNumber, baseQuantity, unit,
                unitCost, input.ExpiryDate, input.EmployeeID).Scan(&batchID)
        if err != nil {
                return nil, fmt.Errorf("insert opening batch: %w", err)
        }

        var movementID string
        var createdAt time.Time
        err = tx.QueryRow(ctx, `
                INSERT INTO stock_movements (
                        batch_id, movement_type, quantity, unit,
                        quantity_before, quantity_after, unit_cost, total_cost,
                        created_by, created_by_company_user_id,
                        reason, ip_address, user_agent, idempotency_key
                ) VALUES (
                        $1, 'purchase'::movement_type, $2, $3::unit_type,
                        0, $2, $4, $5,
                        NULLIF($6, '')::uuid, NULLIF($7, '')::uuid,
                        NULLIF($8, ''), NULLIF($9, '')::inet, NULLIF($10, ''), $11
                )
                RETURNING id::text, created_at
        `, batchID, baseQuantity, unit, unitCost, unitCost.MulQty(baseQuantity),
                input.EmployeeID, input.CompanyUserID, reason, input.IPAddress,
                input.UserAgent, input.IdempotencyKey).Scan(&movementID, &createdAt)
        if err != nil {
                return nil, fmt.Errorf("insert opening movement: %w", err)
        }

        if err := tx.Commit(ctx); err != nil {
                return nil, fmt.Errorf("commit opening stock: %w", err)
        }
        return &OpeningStockResult{
                MovementID: movementID, BatchID: batchID, Unit: unit,
                Quantity: baseQuantity, CreatedAt: createdAt,
        }, nil
}

func validateOpeningStockInput(input OpeningStockInput) error {
        switch {
        case strings.TrimSpace(input.ProductID) == "":
                return fmt.Errorf("%w: product id is required", ErrInvalidOpeningStock)
        case strings.TrimSpace(input.PharmacyID) == "":
                return fmt.Errorf("%w: pharmacy id is required", ErrInvalidOpeningStock)
        case input.EmployeeID == "" && input.CompanyUserID == "":
                return fmt.Errorf("%w: an actor id is required", ErrInvalidOpeningStock)
        case input.InitialBoxes < 0 || input.InitialStrips < 0:
                return fmt.Errorf("%w: quantities must be non-negative", ErrInvalidOpeningStock)
        case input.CostPricePiastres < 0:
                return fmt.Errorf("%w: cost must be non-negative", ErrInvalidOpeningStock)
        case len(strings.TrimSpace(input.IdempotencyKey)) < 8 ||
                len(strings.TrimSpace(input.IdempotencyKey)) > 128:
                return fmt.Errorf("%w: idempotency key must be between 8 and 128 characters", ErrInvalidOpeningStock)
        default:
                return nil
        }
}

func validateStockAdjustmentInput(input StockAdjustmentInput) error {
        switch {
        case strings.TrimSpace(input.BatchID) == "":
                return fmt.Errorf("%w: batch id is required", ErrInvalidStockAdjustment)
        case strings.TrimSpace(input.PharmacyID) == "":
                return fmt.Errorf("%w: pharmacy id is required", ErrInvalidStockAdjustment)
        case strings.TrimSpace(input.EmployeeID) == "" && strings.TrimSpace(input.CompanyUserID) == "":
                return fmt.Errorf("%w: an actor id is required", ErrInvalidStockAdjustment)
        case input.Delta == 0 || math.IsNaN(input.Delta) || math.IsInf(input.Delta, 0):
                return fmt.Errorf("%w: delta must be a finite non-zero number", ErrInvalidStockAdjustment)
        case math.Abs(input.Delta) > 1000000000:
                return fmt.Errorf("%w: delta exceeds the allowed range", ErrInvalidStockAdjustment)
        case len(strings.TrimSpace(input.IdempotencyKey)) < 8 ||
                len(strings.TrimSpace(input.IdempotencyKey)) > 128:
                return fmt.Errorf("%w: idempotency key must be between 8 and 128 characters", ErrInvalidStockAdjustment)
        default:
                return nil
        }
}

// Create creates a new stock movement record
// IMPORTANT: This should be called within a transaction that also updates the batch quantity
func (r *StockMovementRepository) Create(ctx context.Context, movement *models.StockMovement) error {
        query := `
                INSERT INTO stock_movements (
                        batch_id,
                        movement_type, quantity, unit,
                        reference_type, reference_id,
                        quantity_before, quantity_after,
                        unit_cost, total_cost,
                        created_by,
                        reason, notes,
                        ip_address, user_agent
                ) VALUES (
                        $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
                )
                RETURNING id, created_at
        `

        err := r.pool.QueryRow(ctx, query,
                movement.BatchID,
                movement.MovementType,
                movement.Quantity,
                movement.Unit,
                movement.ReferenceType,
                movement.ReferenceID,
                movement.QuantityBefore,
                movement.QuantityAfter,
                movement.UnitCost,
                movement.TotalCost,
                movement.CreatedBy,
                movement.Reason,
                movement.Notes,
                movement.IPAddress,
                movement.UserAgent,
        ).Scan(&movement.ID, &movement.CreatedAt)

        if err != nil {
                return fmt.Errorf("failed to create stock movement: %w", err)
        }

        return nil
}

// GetByID retrieves a stock movement by ID with joined data
func (r *StockMovementRepository) GetByID(ctx context.Context, id string) (*models.StockMovement, error) {
        query := `
                SELECT 
                        sm.id, sm.batch_id,
                        sm.movement_type, sm.quantity, sm.unit,
                        sm.reference_type, sm.reference_id,
                        sm.quantity_before, sm.quantity_after,
                        sm.unit_cost, sm.total_cost,
                        sm.created_by,
                        sm.reason, sm.notes,
                        sm.ip_address, sm.user_agent,
                        sm.created_at,
                        gp.name as product_name, gp.generic_name,
                        ib.batch_number,
                        CONCAT(e.first_name, ' ', e.last_name) as created_by_name
                FROM stock_movements sm
                JOIN inventory_batches ib ON sm.batch_id = ib.id
                JOIN pharmacy_products pp ON ib.pharmacy_product_id = pp.id
                LEFT JOIN global_products gp ON pp.global_product_id = gp.id
                LEFT JOIN employees e ON sm.created_by = e.id
                WHERE sm.id = $1
        `

        movement := &models.StockMovement{
                Batch:             &models.InventoryBatch{},
                CreatedByEmployee: &models.Employee{},
        }

        err := r.pool.QueryRow(ctx, query, id).Scan(
                &movement.ID,
                &movement.BatchID,
                &movement.MovementType,
                &movement.Quantity,
                &movement.Unit,
                &movement.ReferenceType,
                &movement.ReferenceID,
                &movement.QuantityBefore,
                &movement.QuantityAfter,
                &movement.UnitCost,
                &movement.TotalCost,
                &movement.CreatedBy,
                &movement.Reason,
                &movement.Notes,
                &movement.IPAddress,
                &movement.UserAgent,
                &movement.CreatedAt,
                &movement.Batch.BatchNumber,
                &movement.CreatedByEmployee.FirstName, // Will contain full name due to CONCAT
        )

        if err != nil {
                return nil, fmt.Errorf("failed to get stock movement by ID %s: %w", id, err)
        }

        return movement, nil
}

// ListByBatch lists all movements for a specific batch
func (r *StockMovementRepository) ListByBatch(
        ctx context.Context,
        batchID string,
        limit int,
) ([]*models.StockMovement, error) {
        if limit == 0 || limit > 1000 {
                limit = 100 // Default limit to prevent excessive data loading
        }

        query := `
                SELECT 
                        sm.id, sm.batch_id,
                        sm.movement_type, sm.quantity, sm.unit,
                        sm.reference_type, reference_id,
                        sm.quantity_before, sm.quantity_after,
                        sm.created_by, sm.reason, sm.notes,
                        sm.created_at,
                        CONCAT(e.first_name, ' ', e.last_name) as created_by_name
                FROM stock_movements sm
                LEFT JOIN employees e ON sm.created_by = e.id
                WHERE sm.batch_id = $1
                ORDER BY sm.created_at DESC
                LIMIT $2
        `

        rows, err := r.pool.Query(ctx, query, batchID, limit)
        if err != nil {
                return nil, fmt.Errorf("failed to list movements for batch %s: %w", batchID, err)
        }
        defer rows.Close()

        movements := make([]*models.StockMovement, 0)
        for rows.Next() {
                movement := &models.StockMovement{
                        CreatedByEmployee: &models.Employee{},
                }

                var createdByName *string

                err := rows.Scan(
                        &movement.ID,
                        &movement.BatchID,
                        &movement.MovementType,
                        &movement.Quantity,
                        &movement.Unit,
                        &movement.ReferenceType,
                        &movement.ReferenceID,
                        &movement.QuantityBefore,
                        &movement.QuantityAfter,
                        &movement.CreatedBy,
                        &movement.Reason,
                        &movement.Notes,
                        &movement.CreatedAt,
                        &createdByName,
                )
                if err != nil {
                        return nil, fmt.Errorf("failed to scan stock movement: %w", err)
                }

                if createdByName != nil {
                        movement.CreatedByEmployee.FirstName = *createdByName
                }

                movements = append(movements, movement)
        }

        if err := rows.Err(); err != nil {
                return nil, fmt.Errorf("error iterating stock movements: %w", err)
        }

        return movements, nil
}

// ListByReference lists all movements for a specific reference (e.g., purchase order, sale)
func (r *StockMovementRepository) ListByReference(
        ctx context.Context,
        referenceType string,
        referenceID string,
) ([]*models.StockMovement, error) {
        query := `
                SELECT 
                        sm.id, sm.batch_id,
                        sm.movement_type, sm.quantity, sm.unit,
                        sm.quantity_before, sm.quantity_after,
                        sm.created_by, sm.created_at,
                        gp.name as product_name, ib.batch_number
                FROM stock_movements sm
                JOIN inventory_batches ib ON sm.batch_id = ib.id
                JOIN pharmacy_products pp ON ib.pharmacy_product_id = pp.id
                LEFT JOIN global_products gp ON pp.global_product_id = gp.id
                WHERE sm.reference_type = $1 AND sm.reference_id = $2
                ORDER BY sm.created_at ASC
        `

        rows, err := r.pool.Query(ctx, query, referenceType, referenceID)
        if err != nil {
                return nil, fmt.Errorf("failed to list movements for reference: %w", err)
        }
        defer rows.Close()

        movements := make([]*models.StockMovement, 0)
        for rows.Next() {
                movement := &models.StockMovement{
                        Batch: &models.InventoryBatch{},
                }

                err := rows.Scan(
                        &movement.ID,
                        &movement.BatchID,
                        &movement.MovementType,
                        &movement.Quantity,
                        &movement.Unit,
                        &movement.QuantityBefore,
                        &movement.QuantityAfter,
                        &movement.CreatedBy,
                        &movement.CreatedAt,
                        &movement.Batch.BatchNumber,
                )
                if err != nil {
                        return nil, fmt.Errorf("failed to scan stock movement by reference: %w", err)
                }
                movements = append(movements, movement)
        }

        if err := rows.Err(); err != nil {
                return nil, fmt.Errorf("error iterating stock movements by reference: %w", err)
        }

        return movements, nil
}

// CalculateCurrentStock calculates the current stock for a batch by summing all movements
// This is the authoritative source of truth for batch quantities
func (r *StockMovementRepository) CalculateCurrentStock(ctx context.Context, batchID string) (float64, error) {
        query := `SELECT COALESCE(SUM(quantity), 0) FROM stock_movements WHERE batch_id = $1`

        var currentStock float64
        err := r.pool.QueryRow(ctx, query, batchID).Scan(&currentStock)
        if err != nil {
                return 0, fmt.Errorf("failed to calculate current stock for batch %s: %w", batchID, err)
        }

        return currentStock, nil
}

// CalculateProductTotalStock calculates total stock across all batches for a pharmacy product
func (r *StockMovementRepository) CalculateProductTotalStock(
        ctx context.Context,
        pharmacyProductID string,
        branchID *string,
) (float64, error) {
        var query string
        args := []interface{}{pharmacyProductID}

        if branchID != nil && *branchID != "" {
                query = `
                        SELECT COALESCE(SUM(sm.quantity), 0)
                        FROM stock_movements sm
                        JOIN inventory_batches ib ON sm.batch_id = ib.id
                        WHERE ib.pharmacy_product_id = $1 AND ib.branch_id = $2
                `
                args = append(args, *branchID)
        } else {
                query = `
                        SELECT COALESCE(SUM(sm.quantity), 0)
                        FROM stock_movements sm
                        JOIN inventory_batches ib ON sm.batch_id = ib.id
                        WHERE ib.pharmacy_product_id = $1
                `
        }

        var totalStock float64
        err := r.pool.QueryRow(ctx, query, args...).Scan(&totalStock)
        if err != nil {
                return 0, fmt.Errorf("failed to calculate total stock for product %s: %w", pharmacyProductID, err)
        }

        return totalStock, nil
}

// ListRecent lists recent stock movements with filtering options
func (r *StockMovementRepository) ListRecent(
        ctx context.Context,
        pharmacyID string,
        movementType *string,
        limit int,
) ([]*models.StockMovement, error) {
        if limit == 0 || limit > 500 {
                limit = 50
        }

        baseQuery := `
                SELECT 
                        sm.id, sm.batch_id,
                        sm.movement_type, sm.quantity, sm.unit,
                        sm.quantity_before, sm.quantity_after,
                        sm.created_by, sm.reason,
                        sm.created_at,
                        gp.name as product_name, gp.barcode,
                        ib.batch_number,
                        b.name as branch_name,
                        CONCAT(e.first_name, ' ', e.last_name) as created_by_name
                FROM stock_movements sm
                JOIN inventory_batches ib ON sm.batch_id = ib.id
                JOIN pharmacy_products pp ON ib.pharmacy_product_id = pp.id
                LEFT JOIN global_products gp ON pp.global_product_id = gp.id
                LEFT JOIN branches b ON ib.branch_id = b.id
                LEFT JOIN employees e ON sm.created_by = e.id
                WHERE pp.pharmacy_id = $1
        `

        args := []interface{}{pharmacyID}
        argNum := 2

        if movementType != nil && *movementType != "" {
                baseQuery += fmt.Sprintf(" AND sm.movement_type = $%d", argNum)
                args = append(args, *movementType)
                argNum++
        }

        baseQuery += " ORDER BY sm.created_at DESC LIMIT " + fmt.Sprintf("$%d", argNum)
        args = append(args, limit)

        rows, err := r.pool.Query(ctx, baseQuery, args...)
        if err != nil {
                return nil, fmt.Errorf("failed to list recent stock movements: %w", err)
        }
        defer rows.Close()

        movements := make([]*models.StockMovement, 0)
        for rows.Next() {
                movement := &models.StockMovement{
                        Batch:             &models.InventoryBatch{},
                        CreatedByEmployee: &models.Employee{},
                }

                var createdByName *string

                err := rows.Scan(
                        &movement.ID,
                        &movement.BatchID,
                        &movement.MovementType,
                        &movement.Quantity,
                        &movement.Unit,
                        &movement.QuantityBefore,
                        &movement.QuantityAfter,
                        &movement.CreatedBy,
                        &movement.Reason,
                        &movement.CreatedAt,
                        &movement.Batch.BatchNumber,
                        &createdByName,
                )
                if err != nil {
                        return nil, fmt.Errorf("failed to scan recent stock movement: %w", err)
                }

                if createdByName != nil {
                        movement.CreatedByEmployee.FirstName = *createdByName
                }

                movements = append(movements, movement)
        }

        if err := rows.Err(); err != nil {
                return nil, fmt.Errorf("error iterating recent stock movements: %w", err)
        }

        return movements, nil
}

// GetMovementSummary returns summary statistics for movements in a date range
func (r *StockMovementRepository) GetMovementSummary(
        ctx context.Context,
        pharmacyID string,
        startDate, endDate time.Time,
) (map[string]float64, error) {
        query := `
                SELECT 
                        sm.movement_type,
                        COUNT(*) as movement_count,
                        COALESCE(SUM(CASE WHEN sm.quantity > 0 THEN sm.quantity ELSE 0 END), 0) as total_in,
                        COALESCE(SUM(CASE WHEN sm.quantity < 0 THEN ABS(sm.quantity) ELSE 0 END), 0) as total_out
                FROM stock_movements sm
                JOIN inventory_batches ib ON sm.batch_id = ib.id
                JOIN pharmacy_products pp ON ib.pharmacy_product_id = pp.id
                WHERE pp.pharmacy_id = $1 
                  AND sm.created_at BETWEEN $2 AND $3
                GROUP BY sm.movement_type
        `

        rows, err := r.pool.Query(ctx, query, pharmacyID, startDate, endDate)
        if err != nil {
                return nil, fmt.Errorf("failed to get movement summary: %w", err)
        }
        defer rows.Close()

        summary := make(map[string]float64)
        for rows.Next() {
                var movementType string
                var count, totalIn, totalOut float64

                err := rows.Scan(&movementType, &count, &totalIn, &totalOut)
                if err != nil {
                        return nil, fmt.Errorf("failed to scan movement summary row: %w", err)
                }

                summary[movementType+".count"] = count
                summary[movementType+".in"] = totalIn
                summary[movementType+".out"] = totalOut
        }

        if err := rows.Err(); err != nil {
                return nil, fmt.Errorf("error iterating movement summary: %w", err)
        }

        return summary, nil
}
