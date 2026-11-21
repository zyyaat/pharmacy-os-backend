-- Migration 48: product ownership («المنتج ملك الصيدلية»)
--
-- Flips the ownership model the libraries system was built on: a library is
-- a REFERENCE catalog; the pharmacy's product database is the pharmacy's
-- OWN property. Three moves, all additive and idempotent:
--
-- 1) Local identity overrides on pharmacy_products. Every pharmacy-facing
--    read now prefers the override columns and falls back to the shared
--    global_products row («المحلي لو موجود، وإلا الرسمي»). A pharmacy edit
--    of name/generic/barcode/strength/dosage form — including on rows the
--    platform minted or verified — writes ONLY here. global_products keeps
--    the official identity for everyone else and is never written from
--    pharmacy context again. This retires the T-B1 403
--    (catalog_fields_immutable): the shared-catalog write boundary moves
--    from «refuse the write» to «the write has nowhere near the catalog».
--    NULL override = showing the official value; clearing an override in
--    the edit form (empty string) reverts to official.
--
-- 2) Provenance: source_library_id records WHICH library a row was CREATED
--    by (import execute / sync «added»). Rows the pharmacy created itself
--    or linked to an existing product stay NULL — a library removal must
--    never delete a product that predates the library. ON DELETE SET NULL:
--    a platform-side library deletion detaches provenance and the rows
--    live on as plain pharmacy products (migration 31's red line).
--
-- 3) current_inventory rebuilt with effective identity. Same columns,
--    names, order and types as migrations 16/43 (the shape every consumer
--    scans — the documented contract) — only the IDENTITY SOURCE changes
--    to COALESCE(override, official). Every inventory list, delta-sync
--    row, report and dashboard widget shows the pharmacy's own naming
--    with zero consumer changes.
--
-- Barcode hygiene: a per-pharmacy unique index on barcode_override keeps
-- one pharmacy from mapping two products to one scannable code. Across
-- pharmacies overrides MAY coincide (a local label is local data); the
-- official global_products unique index is untouched.

ALTER TABLE pharmacy_products
    ADD COLUMN IF NOT EXISTS name_override VARCHAR(255),
    ADD COLUMN IF NOT EXISTS generic_name_override VARCHAR(255),
    ADD COLUMN IF NOT EXISTS barcode_override VARCHAR(100),
    ADD COLUMN IF NOT EXISTS barcode_type_override VARCHAR(20),
    ADD COLUMN IF NOT EXISTS strength_override VARCHAR(100),
    ADD COLUMN IF NOT EXISTS dosage_form_override dosage_form,
    ADD COLUMN IF NOT EXISTS source_library_id UUID REFERENCES product_libraries(id) ON DELETE SET NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_pharmacy_products_barcode_override
    ON pharmacy_products (pharmacy_id, barcode_override)
    WHERE barcode_override IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_pharmacy_products_source_library
    ON pharmacy_products (pharmacy_id, source_library_id)
    WHERE source_library_id IS NOT NULL;

COMMENT ON COLUMN pharmacy_products.name_override IS
    'Pharmacy-local display name; NULL = official global_products.name shows.';
COMMENT ON COLUMN pharmacy_products.barcode_override IS
    'Pharmacy-local scan code; NULL = official barcode. Unique per pharmacy when set.';
COMMENT ON COLUMN pharmacy_products.source_library_id IS
    'Library that CREATED this row (import/sync add). NULL = pharmacy-owned '
    'before any library (manual create or explicit link).';

-- ---------------------------------------------------------------------------
-- current_inventory: effective identity (override first, official fallback).
-- Column contract of migrations 16/43 preserved verbatim: same columns,
-- names, order, types; the quantity source (migration 43) is untouched.
-- ---------------------------------------------------------------------------
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
        COALESCE(pp.name_override, gp.name) AS product_name,
        COALESCE(pp.generic_name_override, gp.generic_name) AS generic_name,
        gp.brand_name,
        COALESCE(pp.barcode_override, gp.barcode) AS barcode,
        COALESCE(pp.dosage_form_override, gp.dosage_form)::text AS dosage_form,
        COALESCE(pp.strength_override, gp.strength) AS strength,
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
'inventory_batches.quantity once per row (migration 43) with effective '
'product identity: pharmacy-local overrides first, official catalog '
'fallback (migration 48). Quantity source of truth remains '
'SUM(stock_movements); all writers keep the cache in the same transaction.';
