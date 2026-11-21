-- Migration 34 — email verification attempt accounting (AUD-004, HIGH).
-- The 6-digit verification code lives in a 10^6 space with a 24h TTL and
-- used to accept unlimited guesses. This table is the per-email fixed-window
-- attempt wall enforced atomically by ConsumeEmailVerificationCode BEFORE
-- any code comparison happens. Keyed by EMAIL (not token hash) because each
-- wrong guess hashes to a different nonexistent token — the counter must
-- survive guesses that match no row.
-- NOT reset by resend-verification: an attacker who can trigger resends must
-- not inherit a fresh budget; the window self-heals after quiet time.
CREATE TABLE IF NOT EXISTS auth_verify_attempts (
    email TEXT PRIMARY KEY,
    window_started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    attempt_count INT NOT NULL DEFAULT 0
);
