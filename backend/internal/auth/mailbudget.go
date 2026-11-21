package auth

// Fair-use budget for outbound transactional email (Task 148). The 60s
// resend cooldown (Task 131) only spaces consecutive sends; it never bounds
// VOLUME — one email per minute is 60/hour, exactly the free provider tier —
// and forgot-password had no send-side bound at all. Global platforms bound
// outbound email the way they bound traffic: a per-RECIPIENT budget (the
// inbox that feels the spam is the unit of fairness) plus a PLATFORM-WIDE
// daily ceiling protecting the provider quota shared by every feature
// (verification, reset, support).
//
// Two stores implement one contract, mirroring verify_attempts.go:
//   - dbMailBudget: production. Fixed windows (rolling hour + calendar day
//     per recipient, one row per day platform-wide) decided in single
//     atomic statements so any number of concurrent requests agree.
//   - memoryMailBudget: hermetic test double with an injectable clock.
//
// Counters count ATTEMPTS and are consumed only when a send is genuinely
// about to dispatch (the handler calls Take* after the 60s cooldown admits
// the request) — a double-click must not burn budget, but a send that later
// fails upstream still counts, so retry storms against a broken provider
// cannot amplify.
//
// Fail-open doctrine (Task 135): a broken budget store must never take
// account verification hostage. The Service.Take* wrappers log under
// [mail-budget] and allow on store errors; the per-IP flood shield
// (internal/ratelimit) still bounds floods.

import (
        "context"
        "errors"
        "log"
        "strings"
        "sync"
        "time"

        "github.com/jackc/pgx/v5"
        "github.com/jackc/pgx/v5/pgxpool"
)

// ErrMailQuotaExceeded marks the PLATFORM-WIDE daily ceiling. It never
// reaches clients as an error value — handlers translate a global denial
// into 429 email_quota_exceeded — but the sentinel documents intent and
// keeps the two walls distinguishable in logs.
var ErrMailQuotaExceeded = errors.New("platform daily email quota exhausted")

// MailBudgetKind classifies which wall denied a send. Empty means allowed.
type MailBudgetKind string

const (
        MailBudgetNone   MailBudgetKind = ""
        MailBudgetHourly MailBudgetKind = "hourly"
        MailBudgetDaily  MailBudgetKind = "daily"
        MailBudgetGlobal MailBudgetKind = "global"
)

// Defaults sized against the real flows: register (1) + a few honest
// resends + maybe a password reset — 4/hour and 8/day are far above any
// human path yet cap an abuser at 8/day per address. The global ceiling
// (280) sits under the free provider tier (300/day) so chat/support mail
// keeps headroom. Env-tunable via MAIL_SEND_HOURLY_LIMIT /
// MAIL_SEND_DAILY_LIMIT / MAIL_GLOBAL_DAILY_LIMIT; non-positive values fall
// back to the defaults so a bad env var cannot disable the wall (same
// doctrine as internal/ratelimit).
const (
        DefaultMailSendHourlyLimit  = 4
        DefaultMailSendDailyLimit   = 8
        DefaultMailGlobalDailyLimit = 280
)

// MailBudgetDecision is the verdict for one would-be send.
type MailBudgetDecision struct {
        Allowed    bool
        Kind       MailBudgetKind
        RetryAfter time.Duration
}

type mailBudgetStore interface {
        // TakeRecipient registers one send attempt for a recipient address and
        // rules on the per-recipient walls (hour + day).
        TakeRecipient(ctx context.Context, email string, hourLimit, dayLimit int) (MailBudgetDecision, error)
        // TakeGlobal registers one send attempt against the platform-wide
        // daily ceiling.
        TakeGlobal(ctx context.Context, limit int) (MailBudgetDecision, error)
}

// dbMailBudget is the production store.
type dbMailBudget struct {
        db *pgxpool.Pool
}

// newDBMailBudget binds the store to the shared pool. A nil pool
// (unit-test construction) yields nil so the Service falls back to memory.
func newDBMailBudget(db *pgxpool.Pool) mailBudgetStore {
        if db == nil {
                return nil
        }
        return &dbMailBudget{db: db}
}

func (s *dbMailBudget) TakeRecipient(ctx context.Context, email string, hourLimit, dayLimit int) (MailBudgetDecision, error) {
        var hourCount, dayCount int
        var hourStart, dayStart time.Time
        // One atomic upsert decides BOTH windows: the fixed-window resets and
        // the increments share a single statement, so concurrent requests can
        // never read a stale count (same shape as dbVerifyAttempts.Register).
        err := s.db.QueryRow(ctx, `
INSERT INTO auth_email_send_budget AS b
    (email, hour_window_started_at, hour_count, day_window_started_at, day_count)
VALUES (LOWER($1), date_trunc('hour', NOW()), 1, date_trunc('day', NOW()), 1)
ON CONFLICT (email) DO UPDATE SET
    hour_window_started_at = date_trunc('hour', NOW()),
    hour_count = CASE WHEN b.hour_window_started_at = date_trunc('hour', NOW())
                      THEN b.hour_count + 1 ELSE 1 END,
    day_window_started_at = date_trunc('day', NOW()),
    day_count = CASE WHEN b.day_window_started_at = date_trunc('day', NOW())
                     THEN b.day_count + 1 ELSE 1 END
RETURNING hour_count, day_count, hour_window_started_at, day_window_started_at
`, email).Scan(&hourCount, &dayCount, &hourStart, &dayStart)
        if err != nil {
                return MailBudgetDecision{}, err
        }
        now := time.Now()
        // Daily is ruled BEFORE hourly: a day-denial must report the longer
        // retry, not a small hourly remainder that would invite a doomed retry.
        if dayCount > dayLimit {
                return MailBudgetDecision{Allowed: false, Kind: MailBudgetDaily,
                        RetryAfter: clampMailRetry(dayStart.Add(24 * time.Hour).Sub(now))}, nil
        }
        if hourCount > hourLimit {
                return MailBudgetDecision{Allowed: false, Kind: MailBudgetHourly,
                        RetryAfter: clampMailRetry(hourStart.Add(time.Hour).Sub(now))}, nil
        }
        return MailBudgetDecision{Allowed: true}, nil
}

func (s *dbMailBudget) TakeGlobal(ctx context.Context, limit int) (MailBudgetDecision, error) {
        var count int
        // Conditional upsert: the first send of the day inserts; later sends
        // increment only while under the ceiling. When the WHERE fails no row
        // comes back (pgx.ErrNoRows) — that IS the denial signal, and nothing
        // was incremented, so the counter never overshoots the ceiling.
        // CURRENT_DATE follows the database session timezone; production runs
        // a single instance with a UTC database, matching the reset boundary
        // the memory double and the retry math assume.
        err := s.db.QueryRow(ctx, `
INSERT INTO platform_email_sends (day, send_count)
VALUES (CURRENT_DATE, 1)
ON CONFLICT (day) DO UPDATE SET
    send_count = platform_email_sends.send_count + 1
WHERE platform_email_sends.send_count < $1
RETURNING send_count
`, limit).Scan(&count)
        if errors.Is(err, pgx.ErrNoRows) {
                return MailBudgetDecision{Allowed: false, Kind: MailBudgetGlobal,
                        RetryAfter: retryUntilNextUTCMidnight(time.Now())}, nil
        }
        if err != nil {
                return MailBudgetDecision{}, err
        }
        return MailBudgetDecision{Allowed: true}, nil
}

// memoryMailBudget mirrors dbMailBudget's semantics for hermetic tests.
// Window truncation follows UTC (time.Truncate anchors at the UTC zero),
// matching the production database's UTC session timezone.
type memoryMailBudget struct {
        mu         sync.Mutex
        recipients map[string]*memoryRecipientBudget
        globalDay  time.Time
        globalN    int
        now        func() time.Time
}

type memoryRecipientBudget struct {
        hourStart time.Time
        hourCount int
        dayStart  time.Time
        dayCount  int
}

func newMemoryMailBudget() *memoryMailBudget {
        return &memoryMailBudget{
                recipients: make(map[string]*memoryRecipientBudget),
                now:        time.Now,
        }
}

func (m *memoryMailBudget) TakeRecipient(_ context.Context, email string, hourLimit, dayLimit int) (MailBudgetDecision, error) {
        m.mu.Lock()
        defer m.mu.Unlock()
        now := m.now()
        b, ok := m.recipients[strings.ToLower(strings.TrimSpace(email))]
        if !ok {
                b = &memoryRecipientBudget{}
                m.recipients[strings.ToLower(strings.TrimSpace(email))] = b
        }
        hStart := now.Truncate(time.Hour)
        dStart := now.Truncate(24 * time.Hour)
        if !b.hourStart.Equal(hStart) {
                b.hourStart, b.hourCount = hStart, 0
        }
        if !b.dayStart.Equal(dStart) {
                b.dayStart, b.dayCount = dStart, 0
        }
        b.hourCount++
        b.dayCount++
        if b.dayCount > dayLimit {
                return MailBudgetDecision{Allowed: false, Kind: MailBudgetDaily,
                        RetryAfter: clampMailRetry(b.dayStart.Add(24 * time.Hour).Sub(now))}, nil
        }
        if b.hourCount > hourLimit {
                return MailBudgetDecision{Allowed: false, Kind: MailBudgetHourly,
                        RetryAfter: clampMailRetry(b.hourStart.Add(time.Hour).Sub(now))}, nil
        }
        return MailBudgetDecision{Allowed: true}, nil
}

func (m *memoryMailBudget) TakeGlobal(_ context.Context, limit int) (MailBudgetDecision, error) {
        m.mu.Lock()
        defer m.mu.Unlock()
        now := m.now()
        day := now.Truncate(24 * time.Hour)
        if !m.globalDay.Equal(day) {
                m.globalDay, m.globalN = day, 0
        }
        if m.globalN >= limit {
                return MailBudgetDecision{Allowed: false, Kind: MailBudgetGlobal,
                        RetryAfter: clampMailRetry(m.globalDay.Add(24 * time.Hour).Sub(now))}, nil
        }
        m.globalN++
        return MailBudgetDecision{Allowed: true}, nil
}

// retryUntilNextUTCMidnight is the honest retry window for the global
// ceiling: provider daily quotas reset at the UTC day boundary.
func retryUntilNextUTCMidnight(now time.Time) time.Duration {
        n := now.UTC()
        next := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
        return clampMailRetry(next.Sub(n))
}

// clampMailRetry keeps the advisory retry window in a sane band: never a
// zero/negative value (which would read as "retry now"), never past the
// day boundary the walls themselves operate on.
func clampMailRetry(d time.Duration) time.Duration {
        if d < time.Minute {
                return time.Minute
        }
        if d > 24*time.Hour {
                return 24 * time.Hour
        }
        return d
}

// TakeMailBudget applies the per-recipient walls with the fail-open
// doctrine. Lazily binds the store (NewService wires the DB store; direct
// &Service{} construction in tests falls back to memory), mirroring
// registerVerifyAttempt.
func (s *Service) TakeMailBudget(ctx context.Context, email string) MailBudgetDecision {
        if s.mailBudget == nil {
                if s.db != nil {
                        s.mailBudget = &dbMailBudget{db: s.db}
                } else {
                        s.mailBudget = newMemoryMailBudget()
                }
        }
        d, err := s.mailBudget.TakeRecipient(ctx, email, s.mailSendHourlyLimit(), s.mailSendDailyLimit())
        if err != nil {
                log.Printf("[mail-budget] recipient accounting failed (fail-open): %v", err)
                return MailBudgetDecision{Allowed: true}
        }
        return d
}

// TakeGlobalMailBudget applies the platform-wide daily ceiling with the
// same fail-open doctrine.
func (s *Service) TakeGlobalMailBudget(ctx context.Context) MailBudgetDecision {
        if s.mailBudget == nil {
                if s.db != nil {
                        s.mailBudget = &dbMailBudget{db: s.db}
                } else {
                        s.mailBudget = newMemoryMailBudget()
                }
        }
        d, err := s.mailBudget.TakeGlobal(ctx, s.mailGlobalDailyLimit())
        if err != nil {
                log.Printf("[mail-budget] global accounting failed (fail-open): %v", err)
                return MailBudgetDecision{Allowed: true}
        }
        return d
}

func (s *Service) mailSendHourlyLimit() int {
        if s.cfg.MailSendHourlyLimit > 0 {
                return s.cfg.MailSendHourlyLimit
        }
        return DefaultMailSendHourlyLimit
}

func (s *Service) mailSendDailyLimit() int {
        if s.cfg.MailSendDailyLimit > 0 {
                return s.cfg.MailSendDailyLimit
        }
        return DefaultMailSendDailyLimit
}

func (s *Service) mailGlobalDailyLimit() int {
        if s.cfg.MailGlobalDailyLimit > 0 {
                return s.cfg.MailGlobalDailyLimit
        }
        return DefaultMailGlobalDailyLimit
}
