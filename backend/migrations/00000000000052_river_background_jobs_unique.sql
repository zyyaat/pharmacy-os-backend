-- Migration 52 — River Queue background jobs, part 2 of 2 (api_level 118)
-- The SECOND half of the River v0.51.2 embedding (main migrations 005..008):
-- the unique-jobs columns and indexes, the state-bitmask function that
-- references the 'pending' state, the notification outbox, and the final
-- column defaults — plus this chain's wiring (river's ledger stamp, the
-- dedicated queue rows, and library_import_runs, the run ledger the status
-- endpoint reads so the wire never touches river_job internals).
--
-- WHY A SEPARATE MIGRATION FROM 51: one file runs inside ONE transaction,
-- and PostgreSQL ≤ 16 — the production server — refuses to USE a value
-- added by ALTER TYPE ADD VALUE inside the transaction that added it
-- (SQLSTATE 55P04 unsafe_new_enum_value). River 006's function
-- river_job_state_in_bitmask and its unique index reference 'pending',
-- which migration 51 adds — so 'pending' must be committed by 51 before
-- this file may mention it. PostgreSQL 17 relaxed the restriction, which is
-- why the originally-combined single file passed every local gate and CI
-- (PG 17) while failing the production boot (55P04). No version-specific
-- leniency is relied upon anymore.
--
-- Edits vs the official river files, all net-schema-identical:
-- (a) 005's river_migration rebuild DO-block is omitted — the table was
--     born in its final shape in migration 51,
-- (b) the transient river_client/river_client_queue tables (created in 005,
--     dropped in 007 — a feature river never shipped) are omitted entirely,
-- (c) metadata-only drops carry IF EXISTS — this chain's replay-safe idiom.
-- The river_migration ledger stamp below still covers 1..8, so a future
-- rivermigrate run agrees with reality exactly as the combined file would
-- have left it.
--
-- Additive-only; no existing column or view is touched.

-- ===== River main migration 005_migration_unique_client (verbatim) =====
--
-- (River 005's river_migration rebuild DO-block is omitted: the table above
-- is already born in its final shape — the rebuild would be a no-op copy.)

--
-- Add `river_job.unique_key` and bring up an index on it.
--

-- These statements use `IF NOT EXISTS` to allow users with a `river_job` table
-- of non-trivial size to build the index `CONCURRENTLY` out of band of this
-- migration, then follow by completing the migration.
ALTER TABLE /* TEMPLATE: schema */river_job
    ADD COLUMN IF NOT EXISTS unique_key bytea;

CREATE UNIQUE INDEX IF NOT EXISTS river_job_kind_unique_key_idx ON /* TEMPLATE: schema */river_job (kind, unique_key) WHERE unique_key IS NOT NULL;

--
-- (River 005's transient `river_client` / `river_client_queue` tables are
-- omitted: river 007 drops them again in the same release train — the net
-- schema is identical and the destructive scanner stays honest.)

-- ===== River main migration 006_bulk_unique (verbatim) =====
CREATE OR REPLACE FUNCTION /* TEMPLATE: schema */river_job_state_in_bitmask(bitmask BIT(8), state /* TEMPLATE: schema */river_job_state)
RETURNS boolean
LANGUAGE SQL
IMMUTABLE
AS $$
    SELECT CASE state
        WHEN 'available' THEN get_bit(bitmask, 7)
        WHEN 'cancelled' THEN get_bit(bitmask, 6)
        WHEN 'completed' THEN get_bit(bitmask, 5)
        WHEN 'discarded' THEN get_bit(bitmask, 4)
        WHEN 'pending'   THEN get_bit(bitmask, 3)
        WHEN 'retryable' THEN get_bit(bitmask, 2)
        WHEN 'running'   THEN get_bit(bitmask, 1)
        WHEN 'scheduled' THEN get_bit(bitmask, 0)
        ELSE 0
    END = 1;
$$;

--
-- Add `river_job.unique_states` and bring up an index on it.
--

-- This column may exist already if users manually created the column and index
-- as instructed in the changelog so the index could be created `CONCURRENTLY`.
ALTER TABLE /* TEMPLATE: schema */river_job ADD COLUMN IF NOT EXISTS unique_states BIT(8);

-- This statement uses `IF NOT EXISTS` to allow users with a `river_job` table
-- of non-trivial size to build the index `CONCURRENTLY` out of band of this
-- migration, then follow by completing the migration.
CREATE UNIQUE INDEX IF NOT EXISTS river_job_unique_idx ON /* TEMPLATE: schema */river_job (unique_key)
    WHERE unique_key IS NOT NULL
      AND unique_states IS NOT NULL
      AND /* TEMPLATE: schema */river_job_state_in_bitmask(unique_states, state);

-- Remove the old unique index. Users who are actively using the unique jobs
-- feature and who wish to avoid deploy downtime may want od drop this in a
-- subsequent migration once all jobs using the old unique system have been
-- completed (i.e. no more rows with non-null unique_key and null
-- unique_states).
DROP INDEX IF EXISTS /* TEMPLATE: schema */river_job_kind_unique_key_idx;

-- ===== River main migration 007_notification_outbox_sqlite_jsonb_and_sql_cleanup (verbatim) =====
--
-- Notification outbox.
--

CREATE TABLE /* TEMPLATE: schema */river_notification (
    id bigserial PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now(),
    payload text NOT NULL,
    topic text NOT NULL,
    CONSTRAINT topic_length CHECK (length(topic) > 0 AND length(topic) < 128)
);

CREATE INDEX river_notification_created_at_idx ON /* TEMPLATE: schema */river_notification (created_at);
CREATE INDEX river_notification_topic_id_idx ON /* TEMPLATE: schema */river_notification (topic, id);

--
-- SQLite JSONB conversion.
--
-- No-op. Postgres already stores River JSON columns as jsonb.

--
-- SQL cleanup.
--

-- (Drop of the omitted transient `river_client` tables — see 005 note.)
--
-- Adds `DEFAULT 25` to `river_job.max_attempts`.
--

ALTER TABLE /* TEMPLATE: schema */river_job
    ALTER COLUMN max_attempts SET DEFAULT 25;

--
-- Changes `river_queue.updated_at` to have a default of `CURRENT_TIMESTAMP`.
--

ALTER TABLE /* TEMPLATE: schema */river_queue
    ALTER COLUMN updated_at SET DEFAULT CURRENT_TIMESTAMP;

-- ===== River main migration 008_job_id_autoincrement (verbatim) =====
-- No-op. Postgres sequences already prevent automatically generated job IDs
-- from being reused.
SELECT 1;

-- Stamp river's own ledger so its migrate tool agrees with reality.
INSERT INTO river_migration (created_at, line, version)
SELECT NOW(), 'main', v FROM unnest(ARRAY[1,2,3,4,5,6,7,8]) AS v
ON CONFLICT (line, version) DO NOTHING;

-- Dedicated queues: one worker for imports (they are heavy), a small pool
-- for mail. The 'default' queue row already exists from River's schema.
INSERT INTO river_queue (name, metadata)
VALUES ('imports', '{}'), ('mail', '{}')
ON CONFLICT (name) DO NOTHING;

-- Run ledger the wire reads: one row per background import, updated by the
-- worker (queued → running → succeeded|failed). Status polling and the
-- wizard never touch river_job directly — river internals stay internal.
CREATE TABLE library_import_runs (
        id bigserial PRIMARY KEY,
        pharmacy_id uuid NOT NULL REFERENCES pharmacies(id) ON DELETE CASCADE,
        library_id uuid NOT NULL REFERENCES product_libraries(id) ON DELETE CASCADE,
        river_job_id bigint,
        actor_user_id uuid,
        library_version integer NOT NULL DEFAULT 0,
        state text NOT NULL DEFAULT 'queued'
                CHECK (state IN ('queued', 'running', 'succeeded', 'failed')),
        products_created integer NOT NULL DEFAULT 0,
        products_linked integer NOT NULL DEFAULT 0,
        prices_updated integer NOT NULL DEFAULT 0,
        skipped_count integer NOT NULL DEFAULT 0,
        error_message text,
        queued_at timestamptz NOT NULL DEFAULT NOW(),
        started_at timestamptz,
        finished_at timestamptz
);
CREATE INDEX idx_library_import_runs_recent
        ON library_import_runs (pharmacy_id, library_id, id DESC);
