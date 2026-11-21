-- ===========================================================================
-- Migration 53 (api_level 122) — REAL local products (D1) + selling-price
--                    history (D3), on top of migration 48's effective-identity
--                    model and migration 49's invoice snapshots.
-- ===========================================================================
-- The catalog audit (the ChatGPT architecture plan vs. the live system)
-- approved eight decisions. Migration 48 (effective identity) and 49
-- (invoice snapshots) already landed D2 and D5; this migration completes the
-- audit's remaining schema halves:
--
-- D1 — REAL local products. Every product still mints a global_products row
--      (source='pharmacy') and every pharmacy_products row still carries a
--      catalog link, so a pharmacy-created product remains a shared-catalog
--      row any other pharmacy can silently link by scanning the same code.
--      This migration adds the ownership tier the override model cannot
--      express: origin_type='local' means the row is FULLY tenant-owned —
--      no catalog link at all (global_product_id NULL), its identity lives
--      entirely in migration 48's override columns (name_override REQUIRED,
--      barcode_override/barcode_type_override as its scan identity) and no
--      other pharmacy can ever resolve it. Existing rows backfill 'central'
--      (the DEFAULT) — zero behavior change for anything created before
--      this release. The FK and the (pharmacy_id, global_product_id) unique
--      constraint stay; PostgreSQL treats NULLs as distinct, so a pharmacy
--      can hold any number of local products. A CHECK pins the invariant
--      both ways so neither shape can drift.
--
-- D3 — PRICE HISTORY (pharmacy_product_price_history). Selling prices were
--      write-only: an edit erased the previous price forever, so "what did
--      we sell this at last month" was unanswerable and margin analysis over
--      time impossible. One append-only row per price event (create
--      baseline, manual edit, import update, library sync), written by the
--      application inside the same transaction as the change. Cost history
--      deliberately NOT duplicated here — cost_per_unit already carries
--      per-batch history through inventory_batches. RLS policy from day one
--      (the ADR-0002 posture).
--
-- current_inventory is rebuilt once more: migration 48's effective-identity
-- shape preserved VERBATIM (same columns, names, order, types) — the only
-- change is INNER JOIN global_products -> LEFT JOIN, so local products (no
-- catalog row) appear in every inventory list, delta-sync row, report and
-- dashboard widget. With the local identity living entirely in the override
-- columns, every COALESCE already resolves to the tenant-owned values.
--
-- NOT in this migration (documented, deliberate):
--   D4 (unlink) — pure application flow on these columns.
--   D6 (barcode fast path) — pure application flow.
--   D7 FORCE RLS / D8 partitioning — deferred by ADR-0002 / ADR-0006.
--
-- REPLAY SAFETY: every statement is IF NOT EXISTS / IF EXISTS / guarded
-- DO / DROP+CREATE-identical, so re-running the file on an upgraded
-- database is a no-op. The self-verification at the bottom refuses to boot
-- on drift.
-- ===========================================================================

-- ---------------------------------------------------------------------------
-- 1) D1 — origin_type + the nullable catalog link
-- ---------------------------------------------------------------------------
ALTER TABLE pharmacy_products
    ADD COLUMN IF NOT EXISTS origin_type VARCHAR(10) NOT NULL DEFAULT 'central';

-- The catalog link becomes optional exactly when the row is local.
ALTER TABLE pharmacy_products ALTER COLUMN global_product_id DROP NOT NULL;

ALTER TABLE pharmacy_products
    DROP CONSTRAINT IF EXISTS pharmacy_products_origin_type_check;
ALTER TABLE pharmacy_products
    ADD CONSTRAINT pharmacy_products_origin_type_check
    CHECK (origin_type IN ('central', 'local'));

-- The one invariant that makes LOCAL mean LOCAL: a local row carries no
-- catalog link and owns its name through migration 48's override column; a
-- central row always carries the link. Validated against existing rows at
-- ADD time (all 'central' with a link).
ALTER TABLE pharmacy_products
    DROP CONSTRAINT IF EXISTS pharmacy_products_origin_identity_check;
ALTER TABLE pharmacy_products
    ADD CONSTRAINT pharmacy_products_origin_identity_check
    CHECK (
        (origin_type = 'central' AND global_product_id IS NOT NULL)
        OR (origin_type = 'local' AND global_product_id IS NULL
            AND name_override IS NOT NULL AND btrim(name_override) <> '')
    );

-- Display-name search support (POS cascade + tier-2 / list search): the same
-- trigram machinery migration 17 gave the catalog name, now on the local
-- name (migration 48 added the unique barcode_override index; the fuzzy side
-- was left to the catalog).
CREATE INDEX IF NOT EXISTS idx_pharmacy_products_name_override_trgm
    ON pharmacy_products USING gin (name_override gin_trgm_ops)
    WHERE name_override IS NOT NULL;

COMMENT ON COLUMN pharmacy_products.origin_type IS
    'D1: central = linked to the shared catalog (global_product_id NOT NULL); local = fully tenant-owned (no catalog link; identity lives in the migration-48 override columns, name_override required). Backfilled central for all pre-existing rows.';

-- ---------------------------------------------------------------------------
-- 2) D3 — pharmacy_product_price_history (append-only, tenant-scoped)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS pharmacy_product_price_history (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),

    pharmacy_id UUID NOT NULL REFERENCES pharmacies(id) ON DELETE CASCADE,
    pharmacy_product_id UUID NOT NULL REFERENCES pharmacy_products(id) ON DELETE CASCADE,

    old_selling_price NUMERIC(12,4),        -- NULL = creation baseline
    new_selling_price NUMERIC(12,4) NOT NULL CHECK (new_selling_price >= 0),
    old_partial_selling_price NUMERIC(12,4),
    new_partial_selling_price NUMERIC(12,4),

    -- Which writer produced the event: product_create (baseline row),
    -- manual_edit (product edit form), import (Excel/library import),
    -- library_sync (central library price application).
    change_source VARCHAR(30) NOT NULL,
    CONSTRAINT pharmacy_product_price_history_source_check
        CHECK (change_source IN ('product_create', 'manual_edit', 'import', 'library_sync')),

    changed_by_employee_id UUID REFERENCES employees(id) ON DELETE SET NULL,
    changed_by_company_user_id UUID REFERENCES company_users(id) ON DELETE SET NULL,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ppph_product_recent
    ON pharmacy_product_price_history(pharmacy_product_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_ppph_pharmacy_recent
    ON pharmacy_product_price_history(pharmacy_id, created_at DESC);

-- RLS from day one (ADR-0002 posture): the policy exists now so the future
-- FORCE moment flips a switch instead of authoring policy under pressure.
-- CREATE POLICY has no IF NOT EXISTS, so the replay-safe form is a guarded
-- DO block.
ALTER TABLE pharmacy_product_price_history ENABLE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename = 'pharmacy_product_price_history'
          AND policyname = 'pharmacies_can_view_own_price_history'
    ) THEN
        CREATE POLICY "pharmacies_can_view_own_price_history" ON pharmacy_product_price_history
            FOR ALL USING (
                pharmacy_id = current_setting('app.current_pharmacy_id', true)::UUID
            );
    END IF;
END $$;

COMMENT ON TABLE pharmacy_product_price_history IS
    'D3: append-only selling-price history per pharmacy product. One row per price event, written by the application in the same transaction as the change. Cost history intentionally lives on inventory_batches.cost_per_unit.';
COMMENT ON COLUMN pharmacy_product_price_history.old_selling_price IS
    'NULL on the creation baseline row; otherwise the price this event replaced.';
COMMENT ON COLUMN pharmacy_product_price_history.change_source IS
    'Writer that produced the event: product_create | manual_edit | import | library_sync.';

-- ---------------------------------------------------------------------------
-- 3) current_inventory — migration 48's effective-identity shape, now
--    local-product aware. Column contract preserved VERBATIM (same columns,
--    names, order, types as migrations 16/43/48); the only change is
--    INNER JOIN global_products -> LEFT JOIN, so a local product (no
--    catalog row) shows up with its override identity everywhere.
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
        pp.global_product_id,
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
    LEFT JOIN global_products gp ON pp.global_product_id = gp.id
    LEFT JOIN branches b ON ib.branch_id = b.id
) q;

COMMENT ON VIEW current_inventory IS
    'Inventory snapshot reading the transactionally-maintained '
    'inventory_batches.quantity once per row (migration 43 contract) with '
    'the effective identity of migration 48 (override first, official '
    'fallback), now local-product aware (migration 53): LEFT JOIN catalog, '
    'so a local product shows up with its override identity everywhere.';

-- ---------------------------------------------------------------------------
-- 4) Self-verification — refuse to boot on drift (migrations 36/44/45's
--    pattern).
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    v_count INTEGER;
BEGIN
    SELECT COUNT(*) INTO v_count FROM pharmacy_products
    WHERE NOT (
        (origin_type = 'central' AND global_product_id IS NOT NULL)
        OR (origin_type = 'local' AND global_product_id IS NULL
            AND name_override IS NOT NULL AND btrim(name_override) <> '')
    );
    IF v_count <> 0 THEN
        RAISE EXCEPTION 'migration 53 self-check: % pharmacy_products rows violate the origin identity invariant', v_count;
    END IF;

    SELECT COUNT(*) INTO v_count FROM pg_constraint
    WHERE conrelid = 'pharmacy_products'::regclass
      AND conname IN ('pharmacy_products_origin_type_check',
                      'pharmacy_products_origin_identity_check')
      AND NOT convalidated;
    IF v_count <> 0 THEN
        RAISE EXCEPTION 'migration 53 self-check: % origin constraints are NOT VALID', v_count;
    END IF;

    SELECT COUNT(*) INTO v_count FROM pg_attribute
    WHERE attrelid = 'pharmacy_products'::regclass
      AND attname = 'global_product_id' AND attnotnull;
    IF v_count <> 0 THEN
        RAISE EXCEPTION 'migration 53 self-check: pharmacy_products.global_product_id is still NOT NULL';
    END IF;

    SELECT COUNT(*) INTO v_count FROM pg_tables
    WHERE tablename = 'pharmacy_product_price_history';
    IF v_count = 0 THEN
        RAISE EXCEPTION 'migration 53 self-check: pharmacy_product_price_history missing';
    END IF;

    SELECT COUNT(*) INTO v_count FROM pg_class c
    WHERE c.relname = 'pharmacy_product_price_history' AND c.relrowsecurity;
    IF v_count = 0 THEN
        RAISE EXCEPTION 'migration 53 self-check: RLS not enabled on pharmacy_product_price_history';
    END IF;

    SELECT COUNT(*) INTO v_count FROM pg_views
    WHERE viewname = 'current_inventory'
      AND definition LIKE '% LEFT JOIN global_products%';
    IF v_count = 0 THEN
        RAISE EXCEPTION 'migration 53 self-check: current_inventory was not rebuilt with the LEFT JOIN catalog';
    END IF;
END $$;
