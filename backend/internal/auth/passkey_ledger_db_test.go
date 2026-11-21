package auth

// Task 151 — DB-backed regression gates for the challenge ledger and the
// step-up grant store (owner rules #2 and #3). These are the invariants a
// stolen session or a replayed ceremony must NEVER be able to break:
//
//   - a challenge consumes exactly ONCE (atomic UPDATE wins, loser rejects);
//   - a challenge bound to an account/session cannot be consumed by another;
//   - a challenge for one purpose cannot satisfy another;
//   - an expired challenge is dead on arrival;
//   - a grant is bound to ONE principal + ONE purpose and dies after the
//     first use.
//
// Needs a real PostgreSQL (it runs the full migration chain). Point
// TEST_DATABASE_URL at a DISPOSABLE database:
//
//      TEST_DATABASE_URL=postgres://.../pharmacy_scratch go test ./internal/auth/ -run TestChallengeLedger -v

import (
        "context"
        "encoding/json"
        "fmt"
        "os"
        "testing"
        "time"

        "github.com/jackc/pgx/v5/pgxpool"
)

func passkeyTestPool(t *testing.T) *pgxpool.Pool {
        t.Helper()
        dsn := os.Getenv("TEST_DATABASE_URL")
        if dsn == "" {
                t.Skip("TEST_DATABASE_URL not set — challenge ledger gates need a disposable PostgreSQL")
        }
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
        defer cancel()
        pool, err := pgxpool.New(ctx, dsn)
        if err != nil {
                t.Fatalf("connect: %v", err)
        }
        t.Cleanup(pool.Close)
        if err := runPasskeySchema(ctx, pool); err != nil {
                t.Fatalf("schema: %v", err)
        }
        return pool
}

// runPasskeySchema creates exactly the three migration-39 tables (self-
// contained so the auth package stays import-cycle free). Idempotent.
func runPasskeySchema(ctx context.Context, pool *pgxpool.Pool) error {
        _, err := pool.Exec(ctx, `
                CREATE TABLE IF NOT EXISTS passkey_credentials (
                    id                UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
                    principal_type    VARCHAR(32) NOT NULL CHECK (principal_type IN ('company_user', 'employee')),
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
                CREATE TABLE IF NOT EXISTS auth_challenges (
                    id             UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
                    challenge_hash BYTEA NOT NULL UNIQUE,
                    purpose        VARCHAR(32) NOT NULL CHECK (purpose IN ('passkey_register', 'passkey_login', 'passkey_recovery', 'stepup_verify')),
                    principal_type VARCHAR(32),
                    principal_id   UUID,
                    session_id     UUID,
                    session_data   JSONB,
                    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                    expires_at     TIMESTAMPTZ NOT NULL,
                    consumed_at    TIMESTAMPTZ
                );
                CREATE TABLE IF NOT EXISTS auth_stepup_grants (
                    id             UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
                    principal_type VARCHAR(32) NOT NULL CHECK (principal_type IN ('company_user', 'employee')),
                    principal_id   UUID NOT NULL,
                    purpose        VARCHAR(64) NOT NULL,
                    token_hash     BYTEA NOT NULL UNIQUE,
                    method         VARCHAR(16) NOT NULL DEFAULT 'passkey' CHECK (method IN ('passkey', 'password')),
                    credential_id  BYTEA,
                    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                    expires_at     TIMESTAMPTZ NOT NULL,
                    consumed_at    TIMESTAMPTZ
                );
        `)
        return err
}

func TestChallengeLedgerSingleUseAndBinding(t *testing.T) {
        pool := passkeyTestPool(t)
        service := &Service{db: pool}
        ctx := context.Background()
        // Unique per run — challenge_hash is UNIQUE and CI reuses nothing,
        // but a developer rerunning against a scratch DB must not trip.
        stamp := time.Now().UnixNano()
        key := func(tag string) string { return fmt.Sprintf("challenge-%s-%d", tag, stamp) }

        // --- single use: the second consume of the same challenge dies ---
        if err := service.storeChallenge(ctx, key("A"), ChallengePurposeLogin, "", "", "", nil, time.Minute); err != nil {
            t.Fatalf("store: %v", err)
        }
        if _, err := service.consumeChallenge(ctx, key("A"), ChallengePurposeLogin, "", "", ""); err != nil {
                t.Fatalf("first consume must win: %v", err)
        }
        if _, err := service.consumeChallenge(ctx, key("A"), ChallengePurposeLogin, "", "", ""); err != ErrChallengeInvalid {
                t.Fatalf("second consume must fail with ErrChallengeInvalid, got %v", err)
        }

        // --- purpose binding: a login challenge never opens a register ---
        if err := service.storeChallenge(ctx, key("B"), ChallengePurposeLogin, "", "", "", nil, time.Minute); err != nil {
                t.Fatalf("store: %v", err)
        }
        if _, err := service.consumeChallenge(ctx, key("B"), ChallengePurposeRegister, "", "", ""); err != ErrChallengeInvalid {
                t.Fatalf("cross-purpose consume must fail, got %v", err)
        }
        // The row is untouched by the failed attempt — the right purpose
        // still consumes it exactly once.
        if _, err := service.consumeChallenge(ctx, key("B"), ChallengePurposeLogin, "", "", ""); err != nil {
                t.Fatalf("correct purpose must consume: %v", err)
        }

        // Real principals + a real session: the ledger carries real FKs
        // (session_id → auth_sessions), and step-up bindings resolve
        // principals at consume time — fabricated UUIDs would be noise.
        owner := seedCompanyPrincipal(t, pool)
        other := seedCompanyPrincipal(t, pool)
        sessionID := seedAuthSession(t, pool, owner)

        // --- principal binding: another account cannot spend my challenge ---
        if err := service.storeChallenge(ctx, key("C"), ChallengePurposeRegister,
                owner.Type, owner.ID, "", nil, time.Minute); err != nil {
                t.Fatalf("store: %v", err)
        }
        if _, err := service.consumeChallenge(ctx, key("C"), ChallengePurposeRegister,
                other.Type, other.ID, ""); err != ErrChallengeInvalid {
                t.Fatalf("cross-principal consume must fail, got %v", err)
        }
        if _, err := service.consumeChallenge(ctx, key("C"), ChallengePurposeRegister,
                owner.Type, owner.ID, ""); err != nil {
                t.Fatalf("own-principal consume must win: %v", err)
        }

        // --- session binding: a stolen challenge without the session dies ---
        if err := service.storeChallenge(ctx, key("D"), ChallengePurposeStepUp,
                owner.Type, owner.ID, sessionID, nil, time.Minute); err != nil {
                t.Fatalf("store: %v", err)
        }
        otherSession := seedAuthSession(t, pool, owner)
        if _, err := service.consumeChallenge(ctx, key("D"), ChallengePurposeStepUp,
                owner.Type, owner.ID, otherSession); err != ErrChallengeInvalid {
                t.Fatalf("cross-session consume must fail, got %v", err)
        }
        if _, err := service.consumeChallenge(ctx, key("D"), ChallengePurposeStepUp,
                owner.Type, owner.ID, sessionID); err != nil {
                t.Fatalf("own-session consume must win: %v", err)
        }

        // --- expiry: a 3-minute TTL is enforced by the ledger itself ---
        if err := service.storeChallenge(ctx, key("E"), ChallengePurposeLogin, "", "", "", nil, -time.Second); err != nil {
                t.Fatalf("store expired: %v", err)
        }
        if _, err := service.consumeChallenge(ctx, key("E"), ChallengePurposeLogin, "", "", ""); err != ErrChallengeInvalid {
                t.Fatalf("expired challenge must fail, got %v", err)
        }

        // --- unknown digest never matches anything ---
        if _, err := service.consumeChallenge(ctx, "never-issued", ChallengePurposeLogin, "", "", ""); err != ErrChallengeInvalid {
                t.Fatalf("unknown challenge must fail, got %v", err)
        }
}

func TestStepUpGrantScopingAndSingleUse(t *testing.T) {
        pool := passkeyTestPool(t)
        service := &Service{db: pool}
        ctx := context.Background()

        // Real seeded principals — consumeGrantRow resolves the principal
        // through findPrincipalByID, exactly like production does.
        owner := seedCompanyPrincipal(t, pool)
        other := seedCompanyPrincipal(t, pool)

        token, err := service.issueStepUpGrant(ctx, owner, StepUpPurposeChangePassword, "passkey", []byte{9}, time.Minute)
        if err != nil {
                t.Fatalf("issue: %v", err)
        }

        // Wrong principal: rejected, grant stays alive.
        granted, err := service.ConsumeStepUpGrant(ctx, other, StepUpPurposeChangePassword, token)
        if err != nil || granted {
                t.Fatalf("cross-principal consume must be rejected, got (%v, %v)", granted, err)
        }
        // Wrong purpose: rejected, grant stays alive.
        granted, err = service.ConsumeStepUpGrant(ctx, owner, StepUpPurposeManagePasskeys, token)
        if err != nil || granted {
                t.Fatalf("cross-purpose consume must be rejected, got (%v, %v)", granted, err)
        }
        // Right principal + purpose: exactly one success.
        granted, err = service.ConsumeStepUpGrant(ctx, owner, StepUpPurposeChangePassword, token)
        if err != nil || !granted {
                t.Fatalf("legitimate consume must succeed, got (%v, %v)", granted, err)
        }
        // Replay: dead.
        granted, err = service.ConsumeStepUpGrant(ctx, owner, StepUpPurposeChangePassword, token)
        if err != nil || granted {
                t.Fatalf("replayed consume must fail, got (%v, %v)", granted, err)
        }

        // Expired grant: dead on arrival.
        expired, err := service.issueStepUpGrant(ctx, owner, StepUpPurposeManagePasskeys, "password", nil, -time.Second)
        if err != nil {
                t.Fatalf("issue expired: %v", err)
        }
        granted, err = service.ConsumeStepUpGrant(ctx, owner, StepUpPurposeManagePasskeys, expired)
        if err != nil || granted {
                t.Fatalf("expired grant must fail, got (%v, %v)", granted, err)
        }

        // The recovery grant path: token identifies the principal.
        recoveryToken, err := service.issueStepUpGrant(ctx, owner, RecoveryGrantPurpose, "passkey", []byte{7}, time.Minute)
        if err != nil {
                t.Fatalf("issue recovery: %v", err)
        }
        if _, err := service.consumeGrantRow(ctx, recoveryToken, StepUpPurposeChangePassword); err != ErrChallengeInvalid {
                t.Fatalf("recovery token must not satisfy another purpose, got %v", err)
        }
        principal, err := service.consumeGrantRow(ctx, recoveryToken, RecoveryGrantPurpose)
        if err != nil {
                t.Fatalf("recovery consume must succeed: %v", err)
        }
        if principal.ID != owner.ID || principal.Type != owner.Type {
                t.Fatalf("recovery principal mismatch: %+v", principal)
        }
        if _, err := service.consumeGrantRow(ctx, recoveryToken, RecoveryGrantPurpose); err != ErrChallengeInvalid {
                t.Fatalf("recovery replay must fail, got %v", err)
        }
}

// seedCompanyPrincipal creates a real companies + company_users pair and
// returns the matching Principal. Unique per test via nanosecond emails;
// cleanup cascades from the company row.
func seedCompanyPrincipal(t *testing.T, pool *pgxpool.Pool) *Principal {
        t.Helper()
        ctx := context.Background()
        stamp := time.Now().UnixNano()
        var companyID, userID string
        if err := pool.QueryRow(ctx, `
                INSERT INTO companies (name, email) VALUES ($1, $2) RETURNING id::text
        `, "passkey-test-co", "passkey-test-"+fmt.Sprint(stamp)+"@example.com").Scan(&companyID); err != nil {
                t.Fatalf("seed company: %v", err)
        }
        t.Cleanup(func() {
                _, _ = pool.Exec(ctx, `DELETE FROM companies WHERE id = $1::uuid`, companyID)
        })
        if err := pool.QueryRow(ctx, `
                INSERT INTO company_users (company_id, email, password_hash, first_name, last_name, role, email_verified_at)
                VALUES ($1::uuid, $2, 'x', 'Passkey', 'Test', 'company_admin', NOW())
                RETURNING id::text
        `, companyID, "passkey-user-"+fmt.Sprint(stamp)+"@example.com").Scan(&userID); err != nil {
                t.Fatalf("seed company_user: %v", err)
        }
        return &Principal{Type: CompanyUserPrincipal, ID: userID,
                Email: "passkey-user-" + fmt.Sprint(stamp) + "@example.com", Role: "company_admin"}
}

// seedAuthSession creates a live auth_sessions row for the principal (opaque
// token digests are arbitrary bytes) and returns its id. Digests are unique
// per session — the columns carry UNIQUE constraints.
func seedAuthSession(t *testing.T, pool *pgxpool.Pool, principal *Principal) string {
        t.Helper()
        ctx := context.Background()
        var sessionID string
        stamp := time.Now().UnixNano()
        access := challengeDigest(fmt.Sprintf("test-access-%d", stamp))
        refresh := challengeDigest(fmt.Sprintf("test-refresh-%d", stamp))
        if err := pool.QueryRow(ctx, `
                INSERT INTO auth_sessions (
                        auth_realm, principal_type, principal_id,
                        access_token_hash, refresh_token_hash,
                        access_expires_at, refresh_expires_at
                ) VALUES ('pharmacy', $1, $2::uuid, $3, $4, NOW() + interval '1 hour', NOW() + interval '30 days')
                RETURNING id::text
        `, principal.Type, principal.ID, access, refresh).Scan(&sessionID); err != nil {
                t.Fatalf("seed session: %v", err)
        }
        t.Cleanup(func() {
                _, _ = pool.Exec(ctx, `DELETE FROM auth_sessions WHERE id = $1::uuid`, sessionID)
        })
        return sessionID
}

// TestBeginPasskeyLoginE2E reproduces the production begin/finish entry
// path against the real migration-39 schema: real WebAuthn instance, real
// challenge INSERT (WITH serialized SessionData — the pgx []byte→jsonb
// encoding is exactly what CI must validate), real ledger consumption.
func TestBeginPasskeyLoginE2E(t *testing.T) {
        pool := passkeyTestPool(t)
        service := &Service{db: pool, cfg: Config{
                WebAuthnRPID:      "pharmacy-ci.example.com",
                WebAuthnRPName:    "Pharmacy OS CI",
                WebAuthnRPOrigins: []string{"https://pharmacy-ci.example.com"},
        }}
        ctx := context.Background()

        options, err := service.BeginPasskeyLogin(ctx)
        if err != nil {
                t.Fatalf("BeginPasskeyLogin failed: %v", err)
        }
        if options.Response.Challenge.String() == "" {
                t.Fatalf("assertion options carry no challenge")
        }
        if len(options.Response.AllowedCredentials) != 0 {
                t.Fatalf("discoverable login must ship empty allowCredentials")
        }
        if options.Response.UserVerification != "required" {
                t.Fatalf("user verification must be required, got %q", options.Response.UserVerification)
        }

        // The ledger row must exist, unconsumed, with decodable session data.
        var storedSession []byte
        var consumed *time.Time
        digest := challengeDigest(options.Response.Challenge.String())
        if err := pool.QueryRow(ctx, `
                SELECT session_data, consumed_at FROM auth_challenges WHERE challenge_hash = $1
        `, digest).Scan(&storedSession, &consumed); err != nil {
                t.Fatalf("challenge row missing after begin: %v", err)
        }
        if consumed != nil {
                t.Fatalf("fresh challenge must be unconsumed")
        }
        if len(storedSession) == 0 || string(storedSession) == "null" {
                t.Fatalf("session_data must be stored for finish verification")
        }
        var decoded map[string]any
        if err := json.Unmarshal(storedSession, &decoded); err != nil {
                t.Fatalf("session_data must be valid JSON: %v", err)
        }
        if decoded["challenge"] != options.Response.Challenge.String() {
                t.Fatalf("stored challenge mismatch")
        }

        // Consuming with the exact challenge echo must win exactly once.
        if _, err := service.consumeChallenge(ctx, options.Response.Challenge.String(), ChallengePurposeLogin, "", "", ""); err != nil {
                t.Fatalf("consume after begin failed: %v", err)
        }
        if _, err := service.consumeChallenge(ctx, options.Response.Challenge.String(), ChallengePurposeLogin, "", "", ""); err != ErrChallengeInvalid {
                t.Fatalf("replay must fail, got %v", err)
        }
}
