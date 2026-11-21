package auth

// Unit locks for the verify-email attempt wall (AUD-004): the memory store's
// fixed-window semantics, the limit constant's reachability, and the
// service-level guarantee that a locked email is refused BEFORE any token
// lookup (the DB-touching consume must never run for a locked email — that
// is what makes the wall cheap for the attacker to hit and expensive to
// fight through).

import (
        "context"
        "errors"
        "testing"
        "time"

        "github.com/stretchr/testify/require"
)

func TestMemoryStoreCountsWithinWindow(t *testing.T) {
        store := newMemoryVerifyAttempts()
        ctx := context.Background()
        for i := 1; i <= VerifyAttemptLimit; i++ {
                count, err := store.Register(ctx, "user@example.com")
                require.NoError(t, err)
                require.Equal(t, i, count)
        }
}

func TestMemoryStoreLocksBeyondLimit(t *testing.T) {
        store := newMemoryVerifyAttempts()
        ctx := context.Background()
        for i := 0; i < VerifyAttemptLimit; i++ {
                store.Register(ctx, "user@example.com")
        }
        count, err := store.Register(ctx, "user@example.com")
        require.NoError(t, err)
        require.Equal(t, VerifyAttemptLimit+1, count)
        // The service refuses when count > limit — exactly this value.
        require.True(t, count > VerifyAttemptLimit)
}

func TestMemoryStoreOpensFreshWindowAfterQuiet(t *testing.T) {
        store := newMemoryVerifyAttempts()
        ctx := context.Background()
        now := time.Now()
        store.now = func() time.Time { return now }
        for i := 0; i < VerifyAttemptLimit; i++ {
                store.Register(ctx, "user@example.com")
        }
        // Advance past the window: the next attempt starts a fresh count of 1.
        now = now.Add(verifyAttemptWindow + time.Minute)
        count, err := store.Register(ctx, "user@example.com")
        require.NoError(t, err)
        require.Equal(t, 1, count, "window did not reset after quiet period")
}

func TestMemoryStoreCountsArePerEmail(t *testing.T) {
        store := newMemoryVerifyAttempts()
        ctx := context.Background()
        for i := 0; i < VerifyAttemptLimit; i++ {
                store.Register(ctx, "victim@example.com")
        }
        count, err := store.Register(ctx, "other@example.com")
        require.NoError(t, err)
        require.Equal(t, 1, count, "budget leaked across emails")
}

// The service-level lock: with an exhausted budget (memory store, no DB),
// ConsumeEmailVerificationCode must fail with ErrVerificationLocked and —
// critically — never reach the SQL consume (s.db is nil here; reaching it
// would panic, which is exactly the failure mode this test catches).
func TestLockedEmailRefusedBeforeTokenLookup(t *testing.T) {
        service := &Service{} // no db: any token-lookup attempt panics the test
        for i := 0; i < VerifyAttemptLimit; i++ {
                _, err := service.registerVerifyAttempt(context.Background(), "victim@example.com")
                require.NoError(t, err)
        }

        _, err := service.ConsumeEmailVerificationCode(context.Background(), "victim@example.com", "123456")
        require.True(t, errors.Is(err, ErrVerificationLocked),
                "want ErrVerificationLocked, got %v — the wall did not fire before the token lookup", err)
}

// Below the limit the flow proceeds to the token lookup. With no DB that
// lookup panics on the nil pool — this test catches that panic as the
// POSITIVE proof of two facts at once: (1) the wall did not fire on a fresh
// email (otherwise the flow would have returned ErrVerificationLocked long
// before touching the DB), and (2) the flow really reached the token lookup
// (the ordering is wall-first, lookup-second as designed).
func TestFreshEmailReachesTokenLookup(t *testing.T) {
        service := &Service{}
        reachedLookup := false
        func() {
                defer func() {
                        if r := recover(); r != nil {
                                // nil-pool panic inside the token UPDATE — the flow got
                                // past the attempt wall.
                                reachedLookup = true
                        }
                }()
                _, _ = service.ConsumeEmailVerificationCode(context.Background(), "fresh@example.com", "123456")
        }()
        require.True(t, reachedLookup,
                "fresh email never reached the token lookup — the wall is over-blocking or the flow reordered")
}
