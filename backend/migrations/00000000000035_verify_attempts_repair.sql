-- Migration 35 — repair auth_verify_attempts (Task 135, P0).
--
-- The email-verification attempt wall (AUD-004, migration 34) errors at
-- runtime on production: every call to registerVerifyAttempt fails, and
-- ConsumeEmailVerificationCode mapped that failure to 400 invalid_code
-- BEFORE any code comparison — so every fresh, correct 6-digit code was
-- rejected and nobody could complete email verification, while the token
-- row itself stayed pristine (the reset_password flow on the same table
-- family kept working, which is what isolated the fault to the wall).
--
-- Migration 34 cannot repair this: its CREATE TABLE IF NOT EXISTS skips a
-- table that already exists with a foreign shape, so the drifted state
-- persists forever. This migration rebuilds the table deterministically.
-- Cost: attempt counters (at most 15 minutes of budget per email) reset —
-- a trivial price against a total verification outage.
--
-- The PUBLIC DML grant is deliberate: the runtime role's name is not fixed
-- here, this table holds no sensitive data (an email key and a counter),
-- and the wall must never again be unreachable to the app that enforces
-- it. The code path additionally fails open with a loud [verify-wall] log
-- if this table ever breaks again — availability first, guess-wall second.
DROP TABLE IF EXISTS auth_verify_attempts;
CREATE TABLE auth_verify_attempts (
    email TEXT PRIMARY KEY,
    window_started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    attempt_count INT NOT NULL DEFAULT 0
);
GRANT SELECT, INSERT, UPDATE ON auth_verify_attempts TO PUBLIC;
