package auth

// M1/G3 — janitor sweep tests against the real migration-39/42 schema.
//
// Pins the reap policy exactly as the janitor implements it:
//   - challenges whose expiry passed the 1h grace are reaped (consumed or
//     abandoned alike);
//   - live challenges (TTL in the future) survive;
//   - grants expired past the grace are reaped;
//   - consumed grants survive for the 24h forensic day, then are reaped
//     regardless of their nominal expiry;
//   - fresh consumed and live grants survive.
//
// Run through passkeyTestPool (TEST_DATABASE_URL + full migration chain,
// same harness as the Task 151 ledger gates).

import (
        "context"
        "testing"
        "time"

        "github.com/stretchr/testify/require"
)

func TestJanitorReapsExpiredArtifacts(t *testing.T) {
        pool := passkeyTestPool(t)
        service := &Service{db: pool}
        ctx := context.Background()
        owner := seedCompanyPrincipal(t, pool)

        // ---- challenges ----
        // Dead: expired 2h ago (past the 1h grace).
        deadKey := "janitor-dead-challenge-" + time.Now().Format(time.RFC3339Nano)
        require.NoError(t, service.storeChallenge(ctx, deadKey, ChallengePurposeLogin, "", "", "", nil, -2*time.Hour))
        // Live: expires in 2 minutes — untouchable.
        liveKey := "janitor-live-challenge-" + time.Now().Format(time.RFC3339Nano)
        require.NoError(t, service.storeChallenge(ctx, liveKey, ChallengePurposeLogin, "", "", "", nil, 2*time.Minute))
        // Consumed minutes ago: inside both graces — survives (forensics).
        consumedKey := "janitor-consumed-challenge-" + time.Now().Format(time.RFC3339Nano)
        require.NoError(t, service.storeChallenge(ctx, consumedKey, ChallengePurposeLogin, "", "", "", nil, time.Minute))
        _, err := service.consumeChallenge(ctx, consumedKey, ChallengePurposeLogin, "", "", "")
        require.NoError(t, err)

        // ---- grants ----
        mint := func(ttl time.Duration) string {
                token, err := service.issueStepUpGrant(ctx, owner, StepUpPurposeManagePasskeys, "password", nil, ttl)
                require.NoError(t, err)
                return token
        }
        // Dead: expired 2h ago, never spent.
        deadToken := mint(-2 * time.Hour)
        // Forensic survivor: consumed now — must survive this sweep...
        freshToken := mint(5 * time.Minute)
        granted, err := service.ConsumeStepUpGrant(ctx, owner, StepUpPurposeManagePasskeys, freshToken)
        require.NoError(t, err)
        require.True(t, granted)
        // ...but a grant consumed 25h ago must be reaped EVEN THOUGH its
        // nominal expiry is still in the future (the forensic day, not the
        // TTL, governs spent grants).
        oldToken := mint(5 * time.Minute)
        granted, err = service.ConsumeStepUpGrant(ctx, owner, StepUpPurposeManagePasskeys, oldToken)
        require.NoError(t, err)
        require.True(t, granted)
        _, err = pool.Exec(ctx, `UPDATE auth_stepup_grants SET consumed_at = NOW() - interval '25 hours' WHERE token_hash = $1`, tokenHash(oldToken))
        require.NoError(t, err)
        // Live: unspent, expires in 5 minutes — untouchable.
        liveToken := mint(5 * time.Minute)

        challenges, grants, err := service.CleanupExpiredAuthArtifacts(ctx)
        require.NoError(t, err)
        t.Logf("sweep reaped challenges=%d grants=%d", challenges, grants)

        // The three dead rows are gone (exact-key checks — other tests may
        // legitimately contribute to the swept counts, so counts use >=).
        var n int
        require.NoError(t, pool.QueryRow(ctx,
                `SELECT COUNT(*) FROM auth_challenges WHERE challenge_hash = $1`, challengeDigest(deadKey)).Scan(&n))
        require.Equal(t, 0, n, "expired challenge must be reaped")
        require.NoError(t, pool.QueryRow(ctx,
                `SELECT COUNT(*) FROM auth_stepup_grants WHERE token_hash = $1`, tokenHash(deadToken)).Scan(&n))
        require.Equal(t, 0, n, "expired-unspent grant must be reaped")
        require.NoError(t, pool.QueryRow(ctx,
                `SELECT COUNT(*) FROM auth_stepup_grants WHERE token_hash = $1`, tokenHash(oldToken)).Scan(&n))
        require.Equal(t, 0, n, "grant consumed >24h ago must be reaped despite live expiry")

        // Everything live or inside a grace survives.
        require.NoError(t, pool.QueryRow(ctx,
                `SELECT COUNT(*) FROM auth_challenges WHERE challenge_hash = $1`, challengeDigest(liveKey)).Scan(&n))
        require.Equal(t, 1, n, "live challenge must survive")
        require.NoError(t, pool.QueryRow(ctx,
                `SELECT COUNT(*) FROM auth_challenges WHERE challenge_hash = $1`, challengeDigest(consumedKey)).Scan(&n))
        require.Equal(t, 1, n, "recently consumed challenge must survive (forensics)")
        require.NoError(t, pool.QueryRow(ctx,
                `SELECT COUNT(*) FROM auth_stepup_grants WHERE token_hash = $1`, tokenHash(freshToken)).Scan(&n))
        require.Equal(t, 1, n, "freshly consumed grant must survive (forensic day)")
        require.NoError(t, pool.QueryRow(ctx,
                `SELECT COUNT(*) FROM auth_stepup_grants WHERE token_hash = $1`, tokenHash(liveToken)).Scan(&n))
        require.Equal(t, 1, n, "live grant must survive")

        require.GreaterOrEqual(t, challenges, int64(1))
        require.GreaterOrEqual(t, grants, int64(2))
}

// TestJanitorLoopStopsOnCancel — the loop honors context cancellation
// instead of leaking a goroutine past shutdown (DB-free: the first sweep
// fails harmlessly on a nil pool and the loop still ticks and still exits).
func TestJanitorLoopStopsOnCancel(t *testing.T) {
        service := &Service{db: nil}
        ctx, cancel := context.WithCancel(context.Background())
        done := make(chan struct{})
        go func() {
                service.StartAuthJanitor(ctx, 5*time.Millisecond)
                close(done)
        }()
        time.Sleep(50 * time.Millisecond)
        cancel()
        select {
        case <-done:
                // loop exited cleanly
        case <-time.After(2 * time.Second):
                t.Fatalf("janitor loop ignored context cancellation")
        }
}
