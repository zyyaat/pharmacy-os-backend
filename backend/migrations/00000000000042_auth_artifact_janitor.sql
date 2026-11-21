-- Migration 42 — auth artifact janitor support (M1/G3, api_level 98).
--
-- Migration 39's auth_challenges and auth_stepup_grants had no reaper: dead
-- rows accumulated forever. The application-side janitor (auth/janitor.go,
-- swept every 10 minutes from server boot) now reaps them, and these two
-- indexes keep every reap predicate index-driven instead of sequential:
--
--   - auth_stepup_grants (expires_at): serves the "expired, never spent"
--     predicate (expires_at < NOW() - 1h). The pre-existing partial index
--     on (principal_type, principal_id, purpose) WHERE consumed_at IS NULL
--     serves the live-grant lookups but cannot serve a full-table reap.
--
--   - auth_challenges (consumed_at) partial WHERE consumed_at IS NOT NULL:
--     serves observability queries over consumed ceremonies (how many
--     ceremonies completed recently); the expiry-side reap is served by the
--     pre-existing idx_auth_challenges_expiry.
--
-- Additive-only, replay-safe (IF NOT EXISTS), no legacy markers (nothing to
-- adopt — indexes carry no data), no destructive statements.
CREATE INDEX IF NOT EXISTS idx_auth_stepup_grants_expiry
    ON auth_stepup_grants (expires_at);

CREATE INDEX IF NOT EXISTS idx_auth_challenges_consumed
    ON auth_challenges (consumed_at)
    WHERE consumed_at IS NOT NULL;
