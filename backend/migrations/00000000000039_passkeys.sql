-- Migration 39 — WebAuthn passkeys (Task 151): hardware-backed biometric
-- sign-in that REPLACES the password at login, plus scoped step-up
-- verification for sensitive operations (change password, recovery, passkey
-- management). Server-side verification only — a local "success" is never
-- proof; the Go API owns every decision (owner rule #1).
--
-- passkey_credentials — one row per registered authenticator. Only PUBLIC
-- material is stored (credential id + COSE public key); the private key never
-- leaves the device's secure enclave / Keystore. sign_count enables clone
-- detection (unreliable for multi-device synced passkeys — the verifier knows
-- and relies on backup flags instead). credential_id is UNIQUE: two accounts
-- cannot share one authenticator registration.
--
-- auth_challenges — the single-use challenge ledger (owner rule #2): the
-- SHA-256 of every issued challenge with its PURPOSE, the principal it is
-- bound to (NULL for discoverable login, where the credential identifies the
-- account), the auth_session it belongs to (registration/step-up always run
-- inside an authenticated session), the full go-webauthn SessionData needed
-- to verify the finish call, and consumed_at for atomic single-use
-- consumption (UPDATE ... WHERE consumed_at IS NULL). TTL 3 minutes,
-- enforced in code and backstopped by the index-driven cleanup.
--
-- auth_stepup_grants — scoped step-up proof (owner rule #3): NO blanket
-- "verified for 5 minutes" authority. Each grant is bound to exactly ONE
-- purpose (an allowlisted operation class), ONE principal, expires in minutes
-- and is consumed atomically on first use (single-use). Only the SHA-256 of
-- the grant token is stored; the token itself is shown to the client once.
CREATE TABLE IF NOT EXISTS passkey_credentials (
    id                UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    principal_type    VARCHAR(32) NOT NULL
        CHECK (principal_type IN ('company_user', 'employee')),
    principal_id      UUID NOT NULL,
    credential_id     BYTEA NOT NULL UNIQUE,
    public_key        BYTEA NOT NULL,
    attestation_type  VARCHAR(64) NOT NULL DEFAULT 'none',
    transports        TEXT[] NOT NULL DEFAULT '{}',
    aaguid            VARCHAR(64) NOT NULL DEFAULT '',
    sign_count        BIGINT NOT NULL DEFAULT 0,
    backup_eligible   BOOLEAN NOT NULL DEFAULT false,
    backup_state      BOOLEAN NOT NULL DEFAULT false,
    device_name       VARCHAR(120) NOT NULL DEFAULT '',
    uv_initialized    BOOLEAN NOT NULL DEFAULT true,
    last_used_at      TIMESTAMPTZ,
    revoked_at        TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_passkey_credentials_principal
    ON passkey_credentials (principal_type, principal_id)
    WHERE revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS auth_challenges (
    id             UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    challenge_hash BYTEA NOT NULL UNIQUE,
    purpose        VARCHAR(32) NOT NULL
        CHECK (purpose IN ('passkey_register', 'passkey_login', 'passkey_recovery', 'stepup_verify')),
    principal_type VARCHAR(32),
    principal_id   UUID,
    session_id     UUID REFERENCES auth_sessions(id),
    session_data   JSONB,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at     TIMESTAMPTZ NOT NULL,
    consumed_at    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_auth_challenges_expiry
    ON auth_challenges (expires_at);

CREATE TABLE IF NOT EXISTS auth_stepup_grants (
    id             UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    principal_type VARCHAR(32) NOT NULL
        CHECK (principal_type IN ('company_user', 'employee')),
    principal_id   UUID NOT NULL,
    purpose        VARCHAR(64) NOT NULL,
    token_hash     BYTEA NOT NULL UNIQUE,
    method         VARCHAR(16) NOT NULL DEFAULT 'passkey'
        CHECK (method IN ('passkey', 'password')),
    credential_id  BYTEA,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at     TIMESTAMPTZ NOT NULL,
    consumed_at    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_auth_stepup_grants_principal
    ON auth_stepup_grants (principal_type, principal_id, purpose)
    WHERE consumed_at IS NULL;
