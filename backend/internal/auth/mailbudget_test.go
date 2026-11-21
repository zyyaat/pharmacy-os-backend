package auth

// Task 148 — hermetic tests for the fair-use mail budget. The memory double
// mirrors the SQL semantics (fixed windows decided inside one atomic
// statement, counters count ATTEMPTS), so these tests pin the CONTRACT:
// caps trip at exactly limit+1, the day window outranks the hour window in
// the verdict, the global ceiling resets at the UTC day boundary, and a
// broken store fails OPEN (Task 135 doctrine — the wall must never take
// account verification hostage).

import (
        "context"
        "errors"
        "testing"
        "time"
)

type fakeClock struct {
        current time.Time
}

func (f *fakeClock) Now() time.Time { return f.current }
func (f *fakeClock) Advance(d time.Duration) {
        f.current = f.current.Add(d)
}

func newTestMailBudget(start time.Time) (*memoryMailBudget, *fakeClock) {
        clock := &fakeClock{current: start}
        m := newMemoryMailBudget()
        m.now = clock.Now
        return m, clock
}

// Hourly cap: a human's handful of requests passes, the abuser's 5th within
// the same rolling hour is denied with an honest retry window.
func TestMailBudgetRecipientHourlyCap(t *testing.T) {
        m, _ := newTestMailBudget(time.Date(2026, 11, 2, 10, 5, 0, 0, time.UTC))
        ctx := context.Background()
        for i := 0; i < 4; i++ {
                d, err := m.TakeRecipient(ctx, "user@example.com", 4, 8)
                if err != nil {
                        t.Fatalf("take %d: unexpected error: %v", i+1, err)
                }
                if !d.Allowed {
                        t.Fatalf("take %d: denied before the cap", i+1)
                }
        }
        d, err := m.TakeRecipient(ctx, "user@example.com", 4, 8)
        if err != nil {
                t.Fatalf("take 5: unexpected error: %v", err)
        }
        if d.Allowed || d.Kind != MailBudgetHourly {
                t.Fatalf("take 5: want hourly denial, got allowed=%v kind=%q", d.Allowed, d.Kind)
        }
        if d.RetryAfter <= 0 || d.RetryAfter > time.Hour {
                t.Fatalf("take 5: retry window out of band: %v", d.RetryAfter)
        }
}

// Daily cap outranks the hourly one: four sends per hour across two hours
// never trips the hour wall (day total 8 stays inclusive), but the FIRST
// send of the third hour — day total 9, hour count only 1 — must report
// DAILY with the longer retry, not a small hourly remainder inviting a
// doomed retry.
func TestMailBudgetRecipientDailyCapBeatsHourly(t *testing.T) {
        m, clock := newTestMailBudget(time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC))
        ctx := context.Background()
        for hour := 0; hour < 2; hour++ {
                for i := 0; i < 4; i++ {
                        d, err := m.TakeRecipient(ctx, "user@example.com", 4, 8)
                        if err != nil || !d.Allowed {
                                t.Fatalf("hour %d take %d: unexpected verdict allowed=%v err=%v", hour, i+1, d.Allowed, err)
                        }
                }
                clock.Advance(time.Hour)
        }
        // Third hour, first take: hour count is fresh (1 ≤ 4) but the day
        // count crosses the ceiling (9 > 8) — the DAILY wall must win.
        d, _ := m.TakeRecipient(ctx, "user@example.com", 4, 8)
        if d.Allowed || d.Kind != MailBudgetDaily {
                t.Fatalf("9th send of the day: want daily denial, got allowed=%v kind=%q", d.Allowed, d.Kind)
        }
        if d.RetryAfter < time.Hour {
                t.Fatalf("daily retry must point at the next day boundary, got %v", d.RetryAfter)
        }
}

// The budget is keyed by the RECIPIENT address: the same inbox reached
// through two principals (company user + employee) shares one budget.
func TestMailBudgetKeyedByEmailCaseInsensitively(t *testing.T) {
        m, _ := newTestMailBudget(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC))
        ctx := context.Background()
        variants := []string{"Owner@Example.com", "owner@example.com", "OWNER@example.com", "Owner@example.COM"}
        for i, v := range variants { // 4 takes across case variants = one bucket
                if d, _ := m.TakeRecipient(ctx, v, 4, 8); !d.Allowed {
                        t.Fatalf("take %d (%q) denied before the cap — buckets must key on the lowercased address", i+1, v)
                }
        }
        d, _ := m.TakeRecipient(ctx, "owner@example.com", 4, 8)
        if d.Allowed || d.Kind != MailBudgetHourly {
                t.Fatalf("5th take across case variants must trip the hourly wall, got allowed=%v kind=%q", d.Allowed, d.Kind)
        }
}

// Global ceiling: exactly `limit` sends pass, the next is denied until the
// UTC day rolls over.
func TestMailBudgetGlobalCap(t *testing.T) {
        m, clock := newTestMailBudget(time.Date(2026, 11, 2, 23, 30, 0, 0, time.UTC))
        ctx := context.Background()
        for i := 0; i < 3; i++ {
                d, err := m.TakeGlobal(ctx, 3)
                if err != nil {
                        t.Fatalf("global take %d: %v", i+1, err)
                }
                if !d.Allowed {
                        t.Fatalf("global take %d denied before the ceiling", i+1)
                }
        }
        d, _ := m.TakeGlobal(ctx, 3)
        if d.Allowed || d.Kind != MailBudgetGlobal {
                t.Fatalf("global take 4: want global denial, got allowed=%v kind=%q", d.Allowed, d.Kind)
        }
        if d.RetryAfter <= 0 || d.RetryAfter > 24*time.Hour {
                t.Fatalf("global retry window out of band: %v", d.RetryAfter)
        }
        // Day rolls over (23:30 + 30min = next UTC day) → fresh ceiling.
        clock.Advance(30 * time.Minute)
        if d, _ := m.TakeGlobal(ctx, 3); !d.Allowed {
                t.Fatalf("global ceiling must reset at the UTC day boundary")
        }
}

// Fail-open doctrine (Task 135): a broken budget store must NEVER block
// verification. The Service wrappers log loudly and allow.
type brokenMailBudgetStore struct{}

func (brokenMailBudgetStore) TakeRecipient(context.Context, string, int, int) (MailBudgetDecision, error) {
        return MailBudgetDecision{}, errors.New("db down")
}

func (brokenMailBudgetStore) TakeGlobal(context.Context, int) (MailBudgetDecision, error) {
        return MailBudgetDecision{}, errors.New("db down")
}

func TestMailBudgetFailOpenOnStoreError(t *testing.T) {
        s := &Service{cfg: Config{MailSendHourlyLimit: 4, MailSendDailyLimit: 8, MailGlobalDailyLimit: 280}}
        s.mailBudget = brokenMailBudgetStore{}
        if d := s.TakeMailBudget(context.Background(), "user@example.com"); !d.Allowed {
                t.Fatalf("recipient wall must fail OPEN on store errors, got %+v", d)
        }
        if d := s.TakeGlobalMailBudget(context.Background()); !d.Allowed {
                t.Fatalf("global wall must fail OPEN on store errors, got %+v", d)
        }
}

// Non-positive config values fall back to the defaults so a bad env var
// cannot disable the wall (same doctrine as internal/ratelimit).
func TestMailBudgetZeroConfigFallsBackToDefaults(t *testing.T) {
        s := &Service{}
        if got := s.mailSendHourlyLimit(); got != DefaultMailSendHourlyLimit {
                t.Fatalf("hourly default: want %d, got %d", DefaultMailSendHourlyLimit, got)
        }
        if got := s.mailSendDailyLimit(); got != DefaultMailSendDailyLimit {
                t.Fatalf("daily default: want %d, got %d", DefaultMailSendDailyLimit, got)
        }
        if got := s.mailGlobalDailyLimit(); got != DefaultMailGlobalDailyLimit {
                t.Fatalf("global default: want %d, got %d", DefaultMailGlobalDailyLimit, got)
        }
}

// The advisory retry window is clamped: never zero/negative ("retry now"
// would be a lie while the window is still open), never past the day the
// walls operate on.
func TestClampMailRetry(t *testing.T) {
        cases := []struct {
                in, want time.Duration
        }{
                {-5 * time.Second, time.Minute},
                {0, time.Minute},
                {30 * time.Second, time.Minute},
                {45 * time.Minute, 45 * time.Minute},
                {30 * time.Hour, 24 * time.Hour},
        }
        for _, c := range cases {
                if got := clampMailRetry(c.in); got != c.want {
                        t.Fatalf("clampMailRetry(%v) = %v, want %v", c.in, got, c.want)
                }
        }
}
