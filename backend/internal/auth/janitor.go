package auth

// M1/G3 (api_level 98) — the WebAuthn artifact janitor.
//
// Migration 39's two ephemeral tables had NO reaper: every challenge row
// (each carrying a full serialized WebAuthn SessionData JSON blob) and every
// dead step-up grant row accumulated forever. Ceremonies themselves stay
// correct — consumption is an atomic conditional UPDATE and every read
// filters on expires_at — so the debt was pure hygiene, but it compounds:
// a long-lived deployment grows the tables without bound and the
// (unique) challenge_hash index and grant lookups slowly drown in dead rows.
//
// Sweep policy (both tables, one DELETE each, index-driven):
//   - auth_challenges: expires_at < NOW() - 1h. A consumed challenge always
//     expires within minutes of consumption (the TTL is enforced at consume
//     time), so the single expiry predicate reaps consumed and abandoned
//     rows alike; the 1h grace keeps in-flight ceremonies untouchable and
//     recent rows available for incident forensics.
//   - auth_stepup_grants: expires_at < NOW() - 1h (expired, never spent)
//     OR consumed_at < NOW() - 24h (spent — kept one forensic day so a
//     support question like "was this grant ever consumed?" stays answerable
//     from the table itself; only the SHA-256 of the token is stored, so the
//     retained rows disclose nothing re-playable).
//
// Availability: the janitor serves hygiene, never correctness — a failed
// sweep logs and the loop keeps ticking (fail-open on the sweep, fail-closed
// nowhere). Interval 10 minutes per the M1 plan; the first sweep runs at
// boot so a fresh deploy reaps the accumulated debt immediately.

import (
        "context"
        "log"
        "time"
)

const (
        // DefaultJanitorInterval is the sweep cadence (M1/G3: every 10 minutes).
        DefaultJanitorInterval = 10 * time.Minute

        janitorSweepBudget   = time.Minute      // one sweep never wedges the loop
        challengeReapGrace   = time.Hour        // past-expiry grace before a challenge row dies
        grantExpiryGrace     = time.Hour        // past-expiry grace before an unspent grant row dies
        grantConsumedGrace   = 24 * time.Hour   // forensic retention for consumed grants
)

// CleanupExpiredAuthArtifacts runs ONE sweep and returns how many challenge
// rows and grant rows it deleted. Exposed for tests and for operators who
// want an on-demand sweep.
func (s *Service) CleanupExpiredAuthArtifacts(ctx context.Context) (challenges int64, grants int64, err error) {
        if s.db == nil {
                return 0, 0, ErrPasskeysDisabled
        }
        challengeCutoff := time.Now().UTC().Add(-challengeReapGrace)
        cTag, err := s.db.Exec(ctx, `
                DELETE FROM auth_challenges
                WHERE expires_at < $1
        `, challengeCutoff)
        if err != nil {
                return 0, 0, err
        }
        grantExpiryCutoff := time.Now().UTC().Add(-grantExpiryGrace)
        grantConsumedCutoff := time.Now().UTC().Add(-grantConsumedGrace)
        gTag, err := s.db.Exec(ctx, `
                DELETE FROM auth_stepup_grants
                WHERE expires_at < $1
                   OR (consumed_at IS NOT NULL AND consumed_at < $2)
        `, grantExpiryCutoff, grantConsumedCutoff)
        if err != nil {
                return 0, 0, err
        }
        return cTag.RowsAffected(), gTag.RowsAffected(), nil
}

// StartAuthJanitor runs one sweep immediately, then one every interval until
// ctx is cancelled. Designed to be launched as a goroutine from the server
// entry point; ctx cancellation (SIGTERM drain path) stops it cleanly.
func (s *Service) StartAuthJanitor(ctx context.Context, interval time.Duration) {
        if interval <= 0 {
                interval = DefaultJanitorInterval
        }
        sweep := func() {
                sweepCtx, cancel := context.WithTimeout(ctx, janitorSweepBudget)
                defer cancel()
                challenges, grants, err := s.CleanupExpiredAuthArtifacts(sweepCtx)
                if err != nil {
                        // Hygiene, not correctness: log and try again next tick.
                        log.Printf("[AUTH-JANITOR] sweep failed: %v", err)
                        return
                }
                if challenges > 0 || grants > 0 {
                        log.Printf("[AUTH-JANITOR] reaped %d expired challenge(s) and %d dead grant(s)", challenges, grants)
                }
        }
        sweep()
        ticker := time.NewTicker(interval)
        defer ticker.Stop()
        for {
                select {
                case <-ctx.Done():
                        return
                case <-ticker.C:
                        sweep()
                }
        }
}

// StartAuthJanitor is the Handler-facing passthrough so the server entry
// point never reaches into auth internals.
func (h *Handler) StartAuthJanitor(ctx context.Context, interval time.Duration) {
        if h.service == nil {
                return
        }
        h.service.StartAuthJanitor(ctx, interval)
}
