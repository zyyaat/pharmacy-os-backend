package auth

// Per-email attempt wall for email verification (AUD-004, HIGH): the
// 6-digit code spans 10^6 candidates with a 24h TTL and ConsumeEmailVerificationCode
// used to accept unlimited guesses (the code itself is single-use, but
// nothing limited HOW OFTEN it could be probed).
//
// The counter is keyed by EMAIL, not by token hash: every wrong guess
// hashes to a different token that matches no row, so per-token accounting
// would count nothing. It is deliberately NOT reset by resend-verification —
// an attacker who can trigger resends must not mint fresh budgets; the
// window self-heals after quiet time.
//
// Two stores implement one atomic contract:
//   - dbVerifyAttempts: production. One SQL upsert both counts and answers,
//     so concurrent requests from any instance see the same truth.
//   - memoryVerifyAttempts: hermetic test double (no DB in unit tests).

import (
        "context"
        "errors"
        "log"
        "sync"
        "time"

        "github.com/jackc/pgx/v5/pgxpool"
)

// ErrVerificationLocked is returned once the per-email budget is exhausted;
// the verify handler maps it to 429 too_many_attempts.
var ErrVerificationLocked = errors.New("email verification attempts exhausted for this window")

const (
        // VerifyAttemptLimit — a human mistyping their own code a handful of
        // times stays far below this; an online guesser hits it immediately
        // (10 of ~900k candidates per window).
        VerifyAttemptLimit = 10
        // verifyAttemptWindow — fixed window length; matches the login lockout
        // horizon (15 minutes) so both walls share one mental model.
        verifyAttemptWindow = 15 * time.Minute
)

// verifyAttemptStore registers ONE verification attempt for an email inside
// a fixed window and returns the window's 1-based attempt count.
type verifyAttemptStore interface {
        Register(ctx context.Context, email string) (int, error)
}

// dbVerifyAttempts is the production store: a single atomic upsert that
// starts a fresh window when the previous one expired, else increments.
// The window decision and the increment share one statement so two
// concurrent guesses can never read stale counts.
type dbVerifyAttempts struct {
        db *pgxpool.Pool
}

// newDBVerifyAttemptStore binds the store to the shared pool. A nil pool
// (unit-test construction) yields nil so the service falls back to memory.
func newDBVerifyAttemptStore(db *pgxpool.Pool) verifyAttemptStore {
        if db == nil {
                return nil
        }
        return &dbVerifyAttempts{db: db}
}

func (s *dbVerifyAttempts) Register(ctx context.Context, email string) (int, error) {
        var count int
        err := s.db.QueryRow(ctx, `
INSERT INTO auth_verify_attempts (email, window_started_at, attempt_count)
VALUES (LOWER($1), NOW(), 1)
ON CONFLICT (email) DO UPDATE SET
  window_started_at = CASE
    WHEN auth_verify_attempts.window_started_at < NOW() - make_interval(secs => $2)
    THEN NOW()
    ELSE auth_verify_attempts.window_started_at END,
  attempt_count = CASE
    WHEN auth_verify_attempts.window_started_at < NOW() - make_interval(secs => $2)
    THEN 1
    ELSE auth_verify_attempts.attempt_count + 1 END
RETURNING attempt_count
`, email, verifyAttemptWindow.Seconds()).Scan(&count)
        return count, err
}

// memoryVerifyAttempts mirrors dbVerifyAttempts' semantics for tests.
type memoryVerifyAttempts struct {
        mu      sync.Mutex
        entries map[string]*memoryAttempt
        now     func() time.Time
}

type memoryAttempt struct {
        windowStarted time.Time
        count         int
}

func newMemoryVerifyAttempts() *memoryVerifyAttempts {
        return &memoryVerifyAttempts{
                entries: make(map[string]*memoryAttempt),
                now:     time.Now,
        }
}

func (m *memoryVerifyAttempts) Register(_ context.Context, email string) (int, error) {
        m.mu.Lock()
        defer m.mu.Unlock()
        now := m.now()
        entry, ok := m.entries[email]
        if !ok || now.Sub(entry.windowStarted) >= verifyAttemptWindow {
                entry = &memoryAttempt{windowStarted: now, count: 1}
                m.entries[email] = entry
                return entry.count, nil
        }
        entry.count++
        return entry.count, nil
}

// registerVerifyAttempt lazily binds the store (NewService wires the DB
// store; direct &Service{} construction in tests falls back to memory) and
// records one attempt.
func (s *Service) registerVerifyAttempt(ctx context.Context, email string) (int, error) {
        if s.verifyAttempts == nil {
                if s.db != nil {
                        s.verifyAttempts = &dbVerifyAttempts{db: s.db}
                } else {
                        s.verifyAttempts = newMemoryVerifyAttempts()
                }
        }
        count, err := s.verifyAttempts.Register(ctx, email)
        if err != nil {
                // Task 135 — the wall must never take verification hostage. On
                // production a broken attempt store turned EVERY verify into
                // 400 invalid_code before the submitted code was even read
                // (fresh correct codes rejected, wall never tripping 429 —
                // proven by probe). Fail open with a loud, greppable marker:
                // the per-IP auth throttle still bounds online guessing, and
                // the [verify-wall] line is the first place to look when
                // counting silently stops working again.
                log.Printf("[verify-wall] attempt accounting failed (fail-open): %v", err)
                return 0, nil
        }
        return count, nil
}
