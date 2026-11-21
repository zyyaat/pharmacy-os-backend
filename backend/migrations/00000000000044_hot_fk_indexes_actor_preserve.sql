-- ===========================================================================
-- Migration 44: hot-FK indexes (R-10) + actor preservation (R-7)
-- ===========================================================================
-- BASELINE->AFTER (2026-11-09, live harness, api_level 101): the P1 wave
-- collapsed the fixed per-request cost; this wave removes the remaining
-- structural scan/erase hazards the audit left open.
--
-- PART I — R-10: every hot FK the audit listed unindexed. PostgreSQL never
-- auto-indexes FK columns; every join on them and every parent-side
-- RESTRICT delete (and every parent-side table rewrite) pays a sequential
-- scan. Plain CREATE INDEX IF NOT EXISTS (the migrator runs each file in a
-- transaction, so CONCURRENTLY is unavailable — same trade migration 43
-- made on the live database, additive + replay-safe):
--
--   sale_items.batch_id                     (11:47 RESTRICT) hottest join
--   sale_items.pharmacy_product_id          (11:46 RESTRICT)
--   sale_return_items.batch_id              (15:66 RESTRICT) returns path
--   sale_return_items.pharmacy_product_id   (15:65 RESTRICT)
--   sales.branch_id                         (11:37 RESTRICT)
--   sales.employee_id                       (11:38 RESTRICT)
--   sale_returns.branch_id                  (15:46 RESTRICT)
--   sale_returns.employee_id                (15:52 SET NULL, unindexed)
--   sale_returns.company_user_id            (15:53 SET NULL, unindexed)
--   stock_movements.created_by_company_user_id (13 CASCADE; the only index
--           touching it is the partial idempotency one — useless for joins
--           and for the parent-side action this migration re-aims below)
--   customer_payments.pharmacy_id           (19:30 CASCADE)
--   subscriptions.plan_id                   (26:251 RESTRICT)
--   payments.plan_id                        (26:294 RESTRICT)
--
-- PART II — R-7: the two ACTOR columns migration 13 deliberately pinned on
-- CASCADE. Migration 36 detached the six ORGANIZATIONAL ledger FKs
-- (pharmacy/company/batch/account) but left the actor columns, and its
-- self-check list never covered them. Hole: actually deleting a company
-- (soft delete exists, hard delete is possible) cascades company_users,
-- and the CASCADE then silently ERASES historical sales rows and stock
-- movement rows — the ledger loss 36 exists to prevent, one hop over.
-- Same methodology as 36: SET NULL + nullable column + named constraint
-- + self-verification. The ledger row survives, detached from its actor.
--
-- THE WRINKLE 36 DID NOT HAVE: migration 12 added write-time CHECKs
--   stock_movements_actor_required: created_by OR created_by_company_user_id
--   sales_actor_required:           employee_id OR company_user_id
-- A CHECK fires on EVERY row update — including the UPDATE that a
-- referential action performs. With the FKs on SET NULL, deleting a
-- company user who was the ONLY actor on a sale/movement would fire
-- SET NULL, NULL both actor columns, trip the CHECK (23514) and abort
-- the entire company deletion: the hole would not open, but deletion
-- would break loudly for almost every pharmacy (POS is run by the
-- owner's company user). A CHECK cannot tell a legitimate write from a
-- referential action, so the invariant moves to BEFORE triggers that
-- enforce it only for application-initiated writes:
--
--   pg_trigger_depth() < 2  — normal writes run at depth 1; the row-level
--   trigger fired by an RI action (SET NULL / CASCADE / SET DEFAULT) runs
--   at depth >= 2 (the RI trigger itself occupies depth 1). This is the
--   documented PostgreSQL technique for exempting referential actions.
--
-- The trigger fires BEFORE INSERT OR UPDATE OF <actor cols> only, so
-- historical detached rows (both actors NULL) stay freely updatable on
-- every other column. ERRCODE check_violation keeps client-visible
-- behavior identical to the old CHECK (23514).
-- ===========================================================================

-- ---------------------------------------------------------------------------
-- Part II first (constraints before indexes — the actor FK re-aim changes
-- what stock_movements.created_by_company_user_idx serves).
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    plan record;
    attn smallint;
    fk record;
BEGIN
    FOR plan IN
        SELECT * FROM (VALUES
            ('sales',           'company_user_id',            'company_users'),
            ('stock_movements', 'created_by_company_user_id', 'company_users')
        ) AS t(tbl, col, ref)
    LOOP
        -- SET NULL must be able to detach: allow NULL in the actor column
        -- (both were added nullable by migration 12; DROP NOT NULL is the
        -- 36 house style and a no-op here — kept as the permanent guard).
        EXECUTE format('ALTER TABLE %I ALTER COLUMN %I DROP NOT NULL', plan.tbl, plan.col);

        SELECT a.attnum INTO attn
        FROM pg_attribute a
        WHERE a.attrelid = format('public.%I', plan.tbl)::regclass
          AND a.attname  = plan.col
          AND NOT a.attisdropped;
        IF attn IS NULL THEN
            RAISE EXCEPTION 'migration 44: column %.% not found', plan.tbl, plan.col;
        END IF;

        -- Drop every FK living on this column (13's default names or any
        -- future rename), then re-add one with an explicit, greppable name.
        FOR fk IN
            SELECT con.conname
            FROM pg_constraint con
            WHERE con.conrelid = format('public.%I', plan.tbl)::regclass
              AND con.contype  = 'f'
              AND con.conkey   = ARRAY[attn]
        LOOP
            EXECUTE format('ALTER TABLE %I DROP CONSTRAINT IF EXISTS %I', plan.tbl, fk.conname);
        END LOOP;

        EXECUTE format(
            'ALTER TABLE %I ADD CONSTRAINT %I FOREIGN KEY (%I) REFERENCES %I(id) ON DELETE SET NULL',
            plan.tbl,
            plan.tbl || '_' || plan.col || '_preserve_fkey',
            plan.col, plan.ref);
    END LOOP;
END $$;

-- The write-time invariant moves from CHECK to depth-aware triggers.
ALTER TABLE sales DROP CONSTRAINT IF EXISTS sales_actor_required;
ALTER TABLE stock_movements DROP CONSTRAINT IF EXISTS stock_movements_actor_required;

CREATE OR REPLACE FUNCTION enforce_sales_actor_required() RETURNS trigger AS $$
BEGIN
    IF NEW.employee_id IS NULL AND NEW.company_user_id IS NULL
       AND pg_trigger_depth() < 2 THEN
        RAISE EXCEPTION
            'sales: one actor required (employee_id or company_user_id)'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$ LANGUAGE plpgsql;

CREATE TRIGGER sales_actor_required_trigger
    BEFORE INSERT OR UPDATE OF employee_id, company_user_id ON sales
    FOR EACH ROW EXECUTE FUNCTION enforce_sales_actor_required();

CREATE OR REPLACE FUNCTION enforce_stock_movements_actor_required()
RETURNS trigger AS $$
BEGIN
    IF NEW.created_by IS NULL AND NEW.created_by_company_user_id IS NULL
       AND pg_trigger_depth() < 2 THEN
        RAISE EXCEPTION
            'stock_movements: one actor required (created_by or created_by_company_user_id)'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$ LANGUAGE plpgsql;

CREATE TRIGGER stock_movements_actor_required_trigger
    BEFORE INSERT OR UPDATE OF created_by, created_by_company_user_id
    ON stock_movements
    FOR EACH ROW EXECUTE FUNCTION enforce_stock_movements_actor_required();

COMMENT ON CONSTRAINT sales_company_user_id_preserve_fkey ON sales IS
    'AUD-007/R-7: deleting a company user detaches the sales they rang up (SET NULL) — it must never erase them.';
COMMENT ON CONSTRAINT stock_movements_created_by_company_user_id_preserve_fkey ON stock_movements IS
    'AUD-007/R-7: deleting a company user detaches the movements they made (SET NULL) — the ledger is append-only.';

-- ---------------------------------------------------------------------------
-- Part I — R-10: the thirteen hot-FK indexes.
-- ---------------------------------------------------------------------------
CREATE INDEX IF NOT EXISTS idx_sale_items_batch
    ON sale_items(batch_id);
CREATE INDEX IF NOT EXISTS idx_sale_items_product
    ON sale_items(pharmacy_product_id);
CREATE INDEX IF NOT EXISTS idx_sale_return_items_batch
    ON sale_return_items(batch_id);
CREATE INDEX IF NOT EXISTS idx_sale_return_items_product
    ON sale_return_items(pharmacy_product_id);
CREATE INDEX IF NOT EXISTS idx_sales_branch
    ON sales(branch_id);
CREATE INDEX IF NOT EXISTS idx_sales_employee
    ON sales(employee_id);
CREATE INDEX IF NOT EXISTS idx_sale_returns_branch
    ON sale_returns(branch_id);
CREATE INDEX IF NOT EXISTS idx_sale_returns_employee
    ON sale_returns(employee_id);
CREATE INDEX IF NOT EXISTS idx_sale_returns_company_user
    ON sale_returns(company_user_id);
CREATE INDEX IF NOT EXISTS idx_stock_movements_company_user
    ON stock_movements(created_by_company_user_id);
CREATE INDEX IF NOT EXISTS idx_customer_payments_pharmacy
    ON customer_payments(pharmacy_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_plan
    ON subscriptions(plan_id);
CREATE INDEX IF NOT EXISTS idx_payments_plan
    ON payments(plan_id);

-- ---------------------------------------------------------------------------
-- Self-verification — migration 36's checklist EXTENDED with the two actor
-- columns (the audit's explicit ask): refuse to commit if ANY ledger FK on
-- the guarded tables still deletes its parent's history, and refuse if the
-- depth-aware triggers are missing (a dropped trigger would reopen the
-- write-time hole silently).
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    bad int;
    trig int;
BEGIN
    -- 1) No CASCADE anywhere on the guarded ledger columns (36's six +
    --    R-7's two actor columns — one query, wider net).
    SELECT count(*) INTO bad
    FROM pg_constraint con
    JOIN pg_attribute a
      ON a.attrelid = con.conrelid AND a.attnum = ANY (con.conkey)
    WHERE con.contype = 'f'
      AND con.confdeltype = 'c'
      AND con.conrelid IN (
            'sales'::regclass,
            'sale_returns'::regclass,
            'stock_movements'::regclass,
            'payments'::regclass,
            'audit_logs'::regclass)
      AND a.attname IN ('pharmacy_id', 'batch_id', 'company_id', 'account_id',
                        'company_user_id', 'created_by_company_user_id');
    IF bad > 0 THEN
        RAISE EXCEPTION
            'migration 44 verification failed: % ledger FK(s) still ON DELETE CASCADE', bad;
    END IF;

    -- 2) The two actor FKs specifically must be SET NULL ('n'), not merely
    --    non-CASCADE (e.g. NO ACTION would still block company deletion).
    SELECT count(*) INTO bad
    FROM pg_constraint
    WHERE conname IN ('sales_company_user_id_preserve_fkey',
                      'stock_movements_created_by_company_user_id_preserve_fkey')
      AND confdeltype <> 'n';
    IF bad > 0 THEN
        RAISE EXCEPTION
            'migration 44 verification failed: actor FK(s) not ON DELETE SET NULL';
    END IF;

    -- 3) The depth-aware triggers must exist and be enabled.
    SELECT count(*) INTO trig
    FROM pg_trigger
    WHERE tgname IN ('sales_actor_required_trigger',
                     'stock_movements_actor_required_trigger')
      AND NOT tgisinternal;
    IF trig <> 2 THEN
        RAISE EXCEPTION
            'migration 44 verification failed: expected 2 actor triggers, found %', trig;
    END IF;
END $$;
