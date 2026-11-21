-- Migration 37 — payment webhook dedup at the database level (AUD-010/032).
--
-- The audit P1 row «Paymob pending-flag + currency comparison; XPay currency;
-- constant-time XPay token compare; unique index on provider txn» requires
-- that a duplicate provider webhook fails AT THE DATABASE, not merely at a
-- SELECT-then-INSERT pre-check. Until now dedup was app-side only
-- (insertWebhookTxn's EXISTS probe scoped to one payment) — the same provider
-- transaction id replayed against a DIFFERENT payment row (or a race between
-- two webhook deliveries) could write two webhook rows.
--
-- The partial unique index makes the provider transaction id a database
-- guarantee across every payment:
--   * scoped to txn_type='webhook' — intent/sync/refund rows never collide
--     with it (sync rows reuse session ids deliberately, Task 90 Phase G);
--   * scoped to non-NULL ids — legacy rows and provider events without a
--     transaction id stay insertable;
--   * IF NOT EXISTS — replaying the migration is a no-op, house style.
--
-- insertWebhookTxn pairs this with ON CONFLICT DO NOTHING so a duplicate
-- delivery is absorbed silently (the webhook answers 200 already-processed
-- semantics) while a genuinely conflicting cross-payment replay can no
-- longer produce two rows.

-- Deploy-proofing: a partial unique index fails to build if the existing
-- rows already violate it, and a failed migration blocks the boot. Cross-
-- payment duplicates should not exist (the signed anchor resolves every
-- event to exactly one payment row), but if they ever do, the migration
-- refuses LOUDLY with the repair step named — the destructive-wall
-- philosophy: block the deployment with a human-readable error, never
-- silently rewrite ledger rows here.
DO $$
DECLARE
    duplicate_ids INT;
BEGIN
    SELECT COUNT(*) INTO duplicate_ids FROM (
        SELECT provider_transaction_id
        FROM payment_transactions
        WHERE txn_type = 'webhook' AND provider_transaction_id IS NOT NULL
        GROUP BY provider_transaction_id
        HAVING COUNT(*) > 1
    ) offenders;
    IF duplicate_ids > 0 THEN
        RAISE EXCEPTION
            'migration 37: % duplicate provider transaction id(s) among webhook rows — reconcile payment_transactions first (keep one row per provider event), then redeploy',
            duplicate_ids;
    END IF;
END
$$;

CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_txn_provider_webhook
    ON payment_transactions (provider_transaction_id)
    WHERE txn_type = 'webhook' AND provider_transaction_id IS NOT NULL;

-- Self-verification: the index must exist after this migration, fail loudly
-- otherwise (house style since migration 36).
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'payment_transactions'
          AND indexname  = 'uq_payment_txn_provider_webhook'
    ) THEN
        RAISE EXCEPTION 'migration 37: uq_payment_txn_provider_webhook missing after creation';
    END IF;
END
$$;
