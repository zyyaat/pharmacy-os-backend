-- Migration 36 — ledger FK preservation (AUD-007, P0).
--
-- The audit found that the append-only ledger/history tables hang on the
-- organizational tree with ON DELETE CASCADE: deleting a pharmacy, company,
-- or batch would SILENTLY ERASE sales, sale returns, stock movements,
-- payments, and audit logs — the records pharmacies are legally and
-- operationally required to keep (tax, pharmacy-record obligations).
-- Section I of the audit is explicit: no ledger row may move or be deleted
-- until the retention questions are answered by a qualified professional.
--
-- The fix detaches instead of erases: every FK from a ledger/history table
-- INTO an organizational/identity table becomes ON DELETE SET NULL, and the
-- referencing column becomes nullable. Deleting the parent leaves the
-- history row in place, detached (NULL) — nothing is lost, nothing dangles.
-- Detail rows that hang on a preserved parent (sale_items -> sales,
-- sale_return_items -> sale_returns, payment_transactions -> payments) keep
-- their internal CASCADE: the parent row now survives, so they survive.
--
-- Rows already written stay byte-identical: this migration changes
-- constraint metadata and column nullability only — zero data migration.
--
-- Self-verifying: the final block re-reads pg_constraint and refuses to
-- commit if any of the six ledger FKs is still ON DELETE CASCADE, so the
-- protection cannot silently regress in a future edit.

-- ---------------------------------------------------------------------------
-- 1) Swap the ledger FKs from CASCADE to SET NULL (+ drop NOT NULL where
--    SET NULL needs to detach).
--
--    sales.pharmacy_id            -> pharmacies(id)        (was CASCADE)
--    sale_returns.pharmacy_id     -> pharmacies(id)        (was CASCADE)
--    stock_movements.batch_id     -> inventory_batches(id) (was CASCADE)
--    payments.company_id          -> companies(id)         (was CASCADE)
--    audit_logs.pharmacy_id       -> pharmacies(id)        (was CASCADE)
--    audit_logs.account_id        -> accounts(id)          (was CASCADE)
--
--    Left untouched (already safe): sales.branch_id / sales.employee_id are
--    RESTRICT; sale_returns.employee_id / .company_user_id are already SET
--    NULL; payments.subscription_id is SET NULL.
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    plan record;
    attn smallint;
    fk record;
BEGIN
    FOR plan IN
        SELECT * FROM (VALUES
            ('sales',           'pharmacy_id', 'pharmacies',        'SET NULL'),
            ('sale_returns',    'pharmacy_id', 'pharmacies',        'SET NULL'),
            ('stock_movements', 'batch_id',    'inventory_batches', 'SET NULL'),
            ('payments',        'company_id',  'companies',         'SET NULL'),
            ('audit_logs',      'pharmacy_id', 'pharmacies',        'SET NULL'),
            ('audit_logs',      'account_id',  'accounts',          'SET NULL')
        ) AS t(tbl, col, ref, action)
    LOOP
        -- SET NULL must be able to detach: allow NULL in the history column.
        EXECUTE format('ALTER TABLE %I ALTER COLUMN %I DROP NOT NULL', plan.tbl, plan.col);

        SELECT a.attnum INTO attn
        FROM pg_attribute a
        WHERE a.attrelid = format('public.%I', plan.tbl)::regclass
          AND a.attname  = plan.col
          AND NOT a.attisdropped;
        IF attn IS NULL THEN
            RAISE EXCEPTION 'migration 36: column %.% not found', plan.tbl, plan.col;
        END IF;

        -- Drop every FK living on this column (default-named or not), then
        -- re-add one with an explicit, greppable name. IF EXISTS keeps the
        -- drop replay-safe house style (the AUD-007 scanner deliberately
        -- does not flag it) — the loop only ever targets FKs it just read
        -- from pg_constraint, so the guard is pure defense in depth.
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
            'ALTER TABLE %I ADD CONSTRAINT %I FOREIGN KEY (%I) REFERENCES %I(id) ON DELETE %s',
            plan.tbl,
            plan.tbl || '_' || plan.col || '_preserve_fkey',
            plan.col, plan.ref, plan.action);
    END LOOP;
END $$;

-- ---------------------------------------------------------------------------
-- 2) Self-verification: refuse the migration if any of the six ledger FKs
--    still deletes its parent's history (confdeltype 'c' = CASCADE).
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    bad int;
BEGIN
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
      AND a.attname IN ('pharmacy_id', 'batch_id', 'company_id', 'account_id');
    IF bad > 0 THEN
        RAISE EXCEPTION
            'migration 36 verification failed: % ledger FK(s) still ON DELETE CASCADE', bad;
    END IF;
END $$;

COMMENT ON CONSTRAINT sales_pharmacy_id_preserve_fkey ON sales IS
    'AUD-007: deleting a pharmacy detaches its sales history (SET NULL) — it must never erase it.';
COMMENT ON CONSTRAINT sale_returns_pharmacy_id_preserve_fkey ON sale_returns IS
    'AUD-007: deleting a pharmacy detaches its return credit notes (SET NULL) — never erases them.';
COMMENT ON CONSTRAINT stock_movements_batch_id_preserve_fkey ON stock_movements IS
    'AUD-007: deleting a batch detaches its movement history (SET NULL) — the ledger is append-only.';
COMMENT ON CONSTRAINT payments_company_id_preserve_fkey ON payments IS
    'AUD-007: deleting a company detaches its payment records (SET NULL) — financial history is retained.';
