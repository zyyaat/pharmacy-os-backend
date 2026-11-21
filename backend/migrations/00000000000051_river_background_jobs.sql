-- Migration 51 — River Queue background jobs, part 1 of 2 (api_level 118)
-- «الاستيراد في الخلفية»: the pharmacy-side wholesale library import stops
-- blocking an HTTP request and becomes a durable job, with a success/failure
-- email to the client and an update-notification email whenever a library
-- publishes a new version.
--
-- This is the FIRST half of the River v0.51.2 embedding (main migrations
-- 001..004): the river_job state enum and tables, the job table's NOT NULL
-- defaults, the 'pending' state value, river_queue and river_leader. The
-- SECOND half (005..008 — the unique-jobs function/index that reference
-- 'pending', the notification outbox, the final defaults) lives in
-- migration 52, and the split is NOT cosmetic:
--
-- this chain runs one file inside ONE transaction, and PostgreSQL ≤ 16 —
-- the production server — refuses to USE a value added by ALTER TYPE
-- ADD VALUE inside the transaction that added it (SQLSTATE 55P04
-- unsafe_new_enum_value; values born WITH the CREATE TYPE are exempt, which
-- is why the 002 CHECK constraints parse fine here). River 006's function
-- and unique index reference 'pending' by name, so they can only run after
-- this transaction commits. PostgreSQL 17 relaxed the restriction, which is
-- why the originally-combined single file passed every local gate and CI
-- (PG 17) while failing the production boot (55P04) — a lesson in testing
-- against the production engine version, not just the latest one. A future
-- rivermigrate run still sees 1..8 stamped in river_migration (migration 52
-- stamps the full ledger), so the two halves remain river-compatible.
--
-- Edits vs the official river files, all net-schema-identical:
-- (a) river_migration is created directly in its final 005 shape (skips
--     005's rebuild DO-block — see migration 52),
-- (b) the transient river_client/river_client_queue tables (created in 005,
--     dropped in 007 — a feature river never shipped) are omitted entirely,
-- (c) metadata-only drops carry IF EXISTS — this chain's replay-safe idiom.
--
-- No user data exists in these tables on any database: the schema is born
-- and consumed inside this one file.
--
-- Additive-only; no existing column or view is touched.

-- ===== River main migration 001_create_river_migration (verbatim) =====
-- Created directly in the FINAL river-005 shape ((line, version) PK) so
-- 005's rename-copy-drop rebuild below is unnecessary on this embedded path.
CREATE TABLE /* TEMPLATE: schema */river_migration(
    line TEXT NOT NULL,
    version bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT NOW(),
    CONSTRAINT line_length CHECK (char_length(line) > 0 AND char_length(line) < 128),
    CONSTRAINT version_gte_1 CHECK (version >= 1),
    PRIMARY KEY (line, version)
);
-- ===== River main migration 002_initial_schema (verbatim) =====
CREATE TYPE /* TEMPLATE: schema */river_job_state AS ENUM(
  'available',
  'cancelled',
  'completed',
  'discarded',
  'retryable',
  'running',
  'scheduled'
);

CREATE TABLE /* TEMPLATE: schema */river_job(
  -- 8 bytes
  id bigserial PRIMARY KEY,

  -- 8 bytes (4 bytes + 2 bytes + 2 bytes)
  --
  -- `state` is kept near the top of the table for operator convenience -- when
  -- looking at jobs with `SELECT *` it'll appear first after ID. The other two
  -- fields aren't as important but are kept adjacent to `state` for alignment
  -- to get an 8-byte block.
  state /* TEMPLATE: schema */river_job_state NOT NULL DEFAULT 'available',
  attempt smallint NOT NULL DEFAULT 0,
  max_attempts smallint NOT NULL,

  -- 8 bytes each (no alignment needed)
  attempted_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT NOW(),
  finalized_at timestamptz,
  scheduled_at timestamptz NOT NULL DEFAULT NOW(),

  -- 2 bytes (some wasted padding probably)
  priority smallint NOT NULL DEFAULT 1,

  -- types stored out-of-band
  args jsonb,
  attempted_by text[],
  errors jsonb[],
  kind text NOT NULL,
  metadata jsonb NOT NULL DEFAULT '{}',
  queue text NOT NULL DEFAULT 'default',
  tags varchar(255)[],

  CONSTRAINT finalized_or_finalized_at_null CHECK ((state IN ('cancelled', 'completed', 'discarded') AND finalized_at IS NOT NULL) OR finalized_at IS NULL),
  CONSTRAINT max_attempts_is_positive CHECK (max_attempts > 0),
  CONSTRAINT priority_in_range CHECK (priority >= 1 AND priority <= 4),
  CONSTRAINT queue_length CHECK (char_length(queue) > 0 AND char_length(queue) < 128),
  CONSTRAINT kind_length CHECK (char_length(kind) > 0 AND char_length(kind) < 128)
);

-- We may want to consider adding another property here after `kind` if it seems
-- like it'd be useful for something.
CREATE INDEX river_job_kind ON /* TEMPLATE: schema */river_job USING btree(kind);

CREATE INDEX river_job_state_and_finalized_at_index ON /* TEMPLATE: schema */river_job USING btree(state, finalized_at) WHERE finalized_at IS NOT NULL;

CREATE INDEX river_job_prioritized_fetching_index ON /* TEMPLATE: schema */river_job USING btree(state, queue, priority, scheduled_at, id);

CREATE INDEX river_job_args_index ON /* TEMPLATE: schema */river_job USING GIN(args);

CREATE INDEX river_job_metadata_index ON /* TEMPLATE: schema */river_job USING GIN(metadata);

CREATE OR REPLACE FUNCTION /* TEMPLATE: schema */river_job_notify()
  RETURNS TRIGGER
  AS $$
DECLARE
  payload json;
BEGIN
  IF NEW.state = 'available' THEN
    -- Notify will coalesce duplicate notifications within a transaction, so
    -- keep these payloads generalized:
    payload = json_build_object('queue', NEW.queue);
    PERFORM
      pg_notify('river_insert', payload::text);
  END IF;
  RETURN NULL;
END;
$$
LANGUAGE plpgsql;

CREATE TRIGGER river_notify
  AFTER INSERT ON /* TEMPLATE: schema */river_job
  FOR EACH ROW
  EXECUTE PROCEDURE /* TEMPLATE: schema */river_job_notify();

CREATE UNLOGGED TABLE /* TEMPLATE: schema */river_leader(
    -- 8 bytes each (no alignment needed)
    elected_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,

    -- types stored out-of-band
    leader_id text NOT NULL,
    name text PRIMARY KEY,

    CONSTRAINT name_length CHECK (char_length(name) > 0 AND char_length(name) < 128),
    CONSTRAINT leader_id_length CHECK (char_length(leader_id) > 0 AND char_length(leader_id) < 128)
);

-- ===== River main migration 003_river_job_tags_non_null (verbatim) =====
ALTER TABLE /* TEMPLATE: schema */river_job ALTER COLUMN tags SET DEFAULT '{}';
UPDATE /* TEMPLATE: schema */river_job SET tags = '{}' WHERE tags IS NULL;
ALTER TABLE /* TEMPLATE: schema */river_job ALTER COLUMN tags SET NOT NULL;

-- ===== River main migration 004_pending_and_more (verbatim) =====
-- The args column never had a NOT NULL constraint or default value at the
-- database level, though we tried to ensure one at the application level.
ALTER TABLE /* TEMPLATE: schema */river_job ALTER COLUMN args SET DEFAULT '{}';
UPDATE /* TEMPLATE: schema */river_job SET args = '{}' WHERE args IS NULL;
ALTER TABLE /* TEMPLATE: schema */river_job ALTER COLUMN args SET NOT NULL;
ALTER TABLE /* TEMPLATE: schema */river_job ALTER COLUMN args DROP DEFAULT;

-- The metadata column never had a NOT NULL constraint or default value at the
-- database level, though we tried to ensure one at the application level.
ALTER TABLE /* TEMPLATE: schema */river_job ALTER COLUMN metadata SET DEFAULT '{}';
UPDATE /* TEMPLATE: schema */river_job SET metadata = '{}' WHERE metadata IS NULL;
ALTER TABLE /* TEMPLATE: schema */river_job ALTER COLUMN metadata SET NOT NULL;

-- The 'pending' job state will be used for upcoming functionality. This is
-- the statement that anchors the 51/52 split: the value MUST commit before
-- anything references it (see the file header).
ALTER TYPE /* TEMPLATE: schema */river_job_state ADD VALUE IF NOT EXISTS 'pending' AFTER 'discarded';

ALTER TABLE /* TEMPLATE: schema */river_job DROP CONSTRAINT IF EXISTS finalized_or_finalized_at_null;
ALTER TABLE /* TEMPLATE: schema */river_job ADD CONSTRAINT finalized_or_finalized_at_null CHECK (
    (finalized_at IS NULL AND state NOT IN ('cancelled', 'completed', 'discarded')) OR
    (finalized_at IS NOT NULL AND state IN ('cancelled', 'completed', 'discarded'))
);

DROP TRIGGER IF EXISTS river_notify ON /* TEMPLATE: schema */river_job;
DROP FUNCTION IF EXISTS /* TEMPLATE: schema */river_job_notify;

--
-- Create table `river_queue`.
--

CREATE TABLE /* TEMPLATE: schema */river_queue (
    name text PRIMARY KEY NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    metadata jsonb NOT NULL DEFAULT '{}' ::jsonb,
    paused_at timestamptz,
    updated_at timestamptz NOT NULL
);

--
-- Alter `river_leader` to add a default value of 'default` to `name`.
--

ALTER TABLE /* TEMPLATE: schema */river_leader
    ALTER COLUMN name SET DEFAULT 'default',
    DROP CONSTRAINT IF EXISTS name_length,
    ADD CONSTRAINT name_length CHECK (name = 'default');
