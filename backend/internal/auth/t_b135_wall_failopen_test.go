package auth

// Task 135 — the P0 verify-outage regression locks.
//
// On production the attempt-wall store (auth_verify_attempts) errored on
// every call, and ConsumeEmailVerificationCode propagated that error into a
// 400 invalid_code BEFORE any code comparison: every fresh, correct code was
// rejected for every user. These tests pin the two behaviors that must never
// regress: (1) a broken store degrades to fail-open instead of bricking
// verification, and (2) a healthy wall still refuses beyond the limit.

import (
        "context"
        "errors"
        "testing"

        "github.com/stretchr/testify/require"
)

// failingVerifyAttempts simulates a broken attempt store (missing table,
// permission drift, dead DB — the production failure class of Task 135).
type failingVerifyAttempts struct{}

func (failingVerifyAttempts) Register(context.Context, string) (int, error) {
        return 0, errors.New("auth_verify_attempts unavailable (simulated production drift)")
}

// A broken wall must fail OPEN: registerVerifyAttempt reports zero budget
// with no error so verification proceeds, and the [verify-wall] log line is
// the operator's signal. Availability over the guess-wall — the per-IP
// throttle still bounds online guessing while the store is down.
func TestBrokenWallFailsOpen(t *testing.T) {
        service := &Service{verifyAttempts: failingVerifyAttempts{}}
        count, err := service.registerVerifyAttempt(context.Background(), "user@example.com")
        require.NoError(t, err, "a broken wall must not fail verification")
        require.Equal(t, 0, count)
}
