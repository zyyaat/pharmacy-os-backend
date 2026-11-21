-- ===========================================================================
-- Migration 43: current_inventory reads the maintained cache (R-1) + two
--               statement-path indexes (R-6 support)
-- ===========================================================================
-- BASELINE (2026-11-09, live harness, pharmacy-os.dockhosting.cloud):
--   inventory  5 rows   -> 839/873/1772 ms   (was 618/645/1213 at 1 row)
--   statement  3 entries-> 863/1127/1290 ms
--
-- PROBLEM (audit R-1): the view called calculate_batch_current_stock(ib.id)
-- -- one SUM over stock_movements per call -- FOUR times per row
-- (quantity + out_of_stock branch + low_stock branch). At the endpoint's
-- LIMIT 500 that is up to 2000 aggregate subplans per inventory request.
--
-- SAFETY OF READING THE CACHE: inventory_batches.quantity is a
-- transactionally-maintained column; every production writer of
-- stock_movements updates it in the SAME transaction:
--   * product create opening batch  (product_pos_handler.go:369 area)
--   * POS sale FEFO takes           (product_pos_handler.go:853 area)
--   * sale returns                  (sales_history_handler.go:678 area)
--   * stock adjustments             (stock_movement_repo.go:197)
--   * product import opening batch  (product_import_execute.go:537 area)
-- The unused repo method StockMovementRepository.Create (whose contract
-- pushes cache upkeep to callers) has no production callers. Migration 2's
-- CHECK (quantity >= 0) still guards the column.
--
-- COLUMN CONTRACT: identical columns, names, order and types as migration
-- 16 (the shape every consumer scans); only the quantity SOURCE and the
-- status CASE input change. quantity = ib.quantity (NUMERIC(12,4), same
-- type the stock function returned). Status semantics unchanged:
-- out_of_stock -> low_stock -> expiring_soon -> quarantined -> normal,
-- and every batch row stays visible (migration 16's contract).
--
-- STATEMENT INDEXES: GET /pharmacy/customers/:id/statement scans
-- sales WHERE customer_id AND payment_type='credit' and
-- customer_payments WHERE customer_id; both previously seq-scanned.
-- ===========================================================================

DROP VIEW IF EXISTS current_inventory;

CREATE VIEW current_inventory AS
SELECT
    q.batch_id,
    q.pharmacy_product_id,
    q.pharmacy_id,
    q.branch_id,
    q.global_product_id,
    q.product_name,
    q.generic_name,
    q.brand_name,
    q.barcode,
    q.dosage_form,
    q.strength,
    q.batch_number,
    q.unit,
    q.quantity,
    q.cost_per_unit,
    q.total_cost,
    q.expiry_date,
    q.days_until_expiry,
    q.selling_price,
    q.partial_selling_price,
    q.packaging_type,
    q.units_per_box,
    q.min_stock_level,
    q.branch_name,
    CASE
        WHEN q.quantity <= 0 THEN 'out_of_stock'
        WHEN q.quantity <= q.min_stock_level THEN 'low_stock'
        WHEN q.expiry_date IS NOT NULL
            AND q.expiry_date <= CURRENT_DATE + INTERVAL '90 days' THEN 'expiring_soon'
        WHEN q.is_quarantined THEN 'quarantined'
        ELSE 'normal'
    END AS status
FROM (
    SELECT
        ib.id AS batch_id,
        pp.id AS pharmacy_product_id,
        pp.pharmacy_id,
        ib.branch_id,
        gp.id AS global_product_id,
        gp.name AS product_name,
        gp.generic_name,
        gp.brand_name,
        gp.barcode,
        gp.dosage_form::text AS dosage_form,
        gp.strength,
        ib.batch_number,
        ib.unit::text AS unit,
        ib.quantity AS quantity,
        ib.cost_per_unit,
        ib.total_cost,
        ib.expiry_date,
        ib.days_until_expiry,
        pp.selling_price,
        pp.partial_selling_price,
        pp.packaging_type,
        pp.units_per_box,
        pp.min_stock_level,
        b.name AS branch_name,
        ib.is_quarantined
    FROM inventory_batches ib
    JOIN pharmacy_products pp ON ib.pharmacy_product_id = pp.id
    JOIN global_products gp ON pp.global_product_id = gp.id
    LEFT JOIN branches b ON ib.branch_id = b.id
) q;

COMMENT ON VIEW current_inventory IS
'Inventory snapshot reading the transactionally-maintained '
'inventory_batches.quantity once per row (migration 43). Quantity source '
'of truth remains SUM(stock_movements); all writers keep the cache in the '
'same transaction (see migration header for the writer list).';

CREATE INDEX IF NOT EXISTS idx_sales_customer_credit_created
    ON sales (customer_id, created_at)
    WHERE payment_type = 'credit';

CREATE INDEX IF NOT EXISTS idx_customer_payments_customer_created
    ON customer_payments (customer_id, created_at);
