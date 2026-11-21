package auth

// Task 158 — the production wire-format regression gate.
//
// Production runs pgx in QueryExecModeExec (cmd/server/main.go — required by
// Vercel/Supabase transaction pooling). In that mode pgx never Describe()s a
// statement, so it cannot see the server-side parameter types and encodes
// every value in the TEXT format chosen from the GO type alone. A []byte
// therefore rides as bytea-hex text ("\x7b…"), and the server's text→jsonb
// cast explodes with SQLSTATE 22P02 — which is how EVERY passkey ceremony
// (register/login/step-up/recovery: they all store a challenge first) answered
// 500 on production while every local test stayed green: the test harnesses
// build their pools with the DEFAULT exec mode (CacheStatement → binary
// encoding), a gap this file pins shut.
//
// storeChallenge now passes session_data as a string (the codebase-wide
// jsonb convention — every other jsonb site already did). These tests run
// the real ceremonies under the SERVER's exec mode against the real
// migration chain (TEST_DATABASE_URL, same harness as the ledger gates):
//
//      TEST_DATABASE_URL=postgres://.../pharmacy_scratch go test ./internal/auth/ -run TestExecMode

import (
        "context"
        "encoding/json"
        "os"
        "testing"
        "time"

        "github.com/jackc/pgx/v5"
        "github.com/jackc/pgx/v5/pgxpool"
)

// serverModePool opens a pool in the EXACT mode cmd/server uses.
func serverModePool(t *testing.T) *pgxpool.Pool {
        t.Helper()
        dsn := os.Getenv("TEST_DATABASE_URL")
        if dsn == "" {
                t.Skip("TEST_DATABASE_URL not set — exec-mode gates need a disposable PostgreSQL")
        }
        cfg, err := pgxpool.ParseConfig(dsn)
        if err != nil {
                t.Fatalf("parse config: %v", err)
        }
        cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
        pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
        if err != nil {
                t.Fatalf("connect: %v", err)
        }
        t.Cleanup(pool.Close)
        return pool
}

func execModeService(pool *pgxpool.Pool) *Service {
        return &Service{db: pool, cfg: Config{
                WebAuthnRPID:      "pharmacy-ci.example.com",
                WebAuthnRPName:    "Pharmacy OS CI",
                WebAuthnRPOrigins: []string{"https://pharmacy-ci.example.com"},
        }}
}

// TestExecModeDecoyRecovery — the exact production failure of 2026-11-08:
// unknown email → decoy ceremony → storeChallenge("" ,"", "") with a
// serialized SessionData under QueryExecModeExec. Before Task 158 this
// answered SQLSTATE 22P02 "invalid input syntax for type json".
func TestExecModeDecoyRecovery(t *testing.T) {
        pool := serverModePool(t)
        service := execModeService(pool)
        ctx := context.Background()

        options, err := service.BeginPasskeyRecovery(ctx, "nonexistent-probe-t158@example.com")
        if err != nil {
                t.Fatalf("decoy recovery under server exec mode: %v", err)
        }
        if options.Response.Challenge.String() == "" {
                t.Fatalf("decoy options carry no challenge")
        }
        // The decoy row must exist, unconsumed, with DECODABLE session data —
        // stored as a string but read back as real JSON.
        digest := challengeDigest(options.Response.Challenge.String())
        var storedSession []byte
        if err := pool.QueryRow(ctx, `
                SELECT session_data FROM auth_challenges WHERE challenge_hash = $1
        `, digest).Scan(&storedSession); err != nil {
                t.Fatalf("decoy challenge row missing: %v", err)
        }
        var decoded map[string]any
        if err := json.Unmarshal(storedSession, &decoded); err != nil {
                t.Fatalf("session_data must decode as JSON after string-transport fix: %v", err)
        }
}

// TestExecModeLoginBeginAndConsume — the discoverable-login begin (the same
// all-unbound INSERT shape) plus a full consume round trip under the server
// exec mode: the session data written through the fixed path must survive
// the read path byte-for-JSON.
func TestExecModeLoginBeginAndConsume(t *testing.T) {
        pool := serverModePool(t)
        service := execModeService(pool)
        ctx := context.Background()

        options, err := service.BeginPasskeyLogin(ctx)
        if err != nil {
                t.Fatalf("login begin under server exec mode: %v", err)
        }
        challenge := options.Response.Challenge.String()

        record, err := service.consumeChallenge(ctx, challenge, ChallengePurposeLogin, "", "", "")
        if err != nil {
                t.Fatalf("consume under server exec mode: %v", err)
        }
        if record.Session.Challenge != challenge {
                t.Fatalf("round-tripped session challenge mismatch: got %q want %q", record.Session.Challenge, challenge)
        }
        if record.Session.Expires.Before(time.Now()) {
                t.Fatalf("round-tripped session expiry lost")
        }
        // Single-use still holds under this mode.
        if _, err := service.consumeChallenge(ctx, challenge, ChallengePurposeLogin, "", "", ""); err == nil {
                t.Fatalf("second consume must fail — single-use invariant broken under exec mode")
        }
}

// TestExecModeBoundChallengeInsert — the bound INSERT shape (real principal
// binding, the register/known-recovery path) under the server exec mode.
func TestExecModeBoundChallengeInsert(t *testing.T) {
        pool := serverModePool(t)
        service := execModeService(pool)
        ctx := context.Background()

        key := "t158-bound-challenge-" + time.Now().Format(time.RFC3339Nano)
        if err := service.storeChallenge(ctx, key, ChallengePurposeRecovery,
                "company_user", "00000000-0000-0000-0000-000000000000", "", nil, challengeTTL); err != nil {
                t.Fatalf("bound storeChallenge under server exec mode: %v", err)
        }
}
