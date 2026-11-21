// P7 regression gate for the first live service extraction (ADR-0005,
// api_level 107). services.PlatformStats moved verbatim out of
// platform_admin_handler.go; this suite pins, against the real migrated
// schema:
//   - the service agrees with independent SQL on the live predicate
//     (companies WHERE deleted_at IS NULL AND NOT is_platform — Task 164
//     narrowed the contract: the bootstrap shell hosting the super admin is
//     infrastructure, not a customer, and is excluded from every counter;
//     marked companies cannot be deleted, so the scratch DB accumulates
//     frozen fixtures that BOTH sides of this comparison must ignore) —
//     the exact counting the wire
//     promises, and the Scan order is the documented one (a scrambled Scan
//     trips the partition invariants below),
//   - the aggregate RESPONDS to data: inserting an active company moves
//     totalCompanies/activeCompanies by exactly one and leaves the
//     suspended counter untouched, deleting the fixture restores the exact
//     baseline (aggregates track rows, no hidden caching).
//
// Concurrency note: the other DB gates (handlers/auth) run in PARALLEL
// against the same scratch database in CI and seed companies of their own,
// so global counters can move underneath this test. Each observe→mutate→
// observe sequence is therefore retried until it lands on a quiescent
// window (the sequence spans a few milliseconds; a foreign fixture landing
// inside it is rare). A REAL regression — wrong Scan order, broken
// predicate, hidden caching — fails EVERY attempt, so retrying cannot mask
// it; hard SQL errors abort immediately instead of retrying.
//
// Same harness as the other DB gates:
//
//      TEST_DATABASE_URL=postgres://.../pharmacy_scratch go test ./internal/services/ -run TestPlatformStatsServiceDB
package services

import (
        "context"
        "fmt"
        "os"
        "testing"
        "time"

        "github.com/jackc/pgx/v5"
        "github.com/jackc/pgx/v5/pgxpool"
        "github.com/stretchr/testify/require"
)

func platformStatsPool(t *testing.T) *pgxpool.Pool {
        t.Helper()
        dsn := os.Getenv("TEST_DATABASE_URL")
        if dsn == "" {
                t.Skip("TEST_DATABASE_URL not set — platform stats gate needs a disposable PostgreSQL")
        }
        cfg, err := pgxpool.ParseConfig(dsn)
        if err != nil {
                t.Fatalf("parse config: %v", err)
        }
        // Same mode as every production pool (ADR-0003 / Task 158 lesson).
        cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
        pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
        if err != nil {
                t.Fatalf("connect: %v", err)
        }
        t.Cleanup(pool.Close)
        return pool
}

// statsSnapshot reads the service aggregate AND the same predicates through
// independent SQL, back to back. The pair agrees unless a concurrent write
// lands between the two reads (retryable).
type statsSnapshot struct {
        svc             PlatformStatsRow
        directTotal     int
        directActive    int
        directSuspended int
}

func readStatsSnapshot(ctx context.Context, pool *pgxpool.Pool) (statsSnapshot, error) {
        var s statsSnapshot
        svc, err := PlatformStats(ctx, pool)
        if err != nil {
                return s, fmt.Errorf("PlatformStats: %w", err)
        }
        s.svc = svc
        err = pool.QueryRow(ctx, `
                SELECT COUNT(*)::int,
                       COUNT(*) FILTER (WHERE status = 'active')::int,
                       COUNT(*) FILTER (WHERE status = 'suspended')::int
                FROM companies
                WHERE deleted_at IS NULL AND NOT is_platform
        `).Scan(&s.directTotal, &s.directActive, &s.directSuspended)
        if err != nil {
                return s, fmt.Errorf("direct counts: %w", err)
        }
        return s, nil
}

func (s statsSnapshot) consistent() bool {
        // The service must count exactly what the independent predicate counts,
        // and active+suspended must be a partition of the live total — a
        // scrambled Scan order breaks both.
        return s.svc.TotalCompanies == s.directTotal &&
                s.svc.TotalCompanies >= s.svc.ActiveCompanies+s.svc.SuspendedCompanies &&
                s.svc.TotalUsers >= s.svc.ActiveUsers
}

func (s statsSnapshot) equal(other statsSnapshot) bool {
        return s.svc == other.svc && s.directTotal == other.directTotal &&
                s.directActive == other.directActive && s.directSuspended == other.directSuspended
}

func TestPlatformStatsServiceDB(t *testing.T) {
        ctx := context.Background()
        pool := platformStatsPool(t)

        const attempts = 6
        for attempt := 0; attempt < attempts; attempt++ {
                landed, err := runStatsSequence(ctx, pool, attempt)
                require.NoError(t, err, "attempt %d: hard SQL failure (never retryable)", attempt)
                if landed {
                        return
                }
        }
        t.Fatalf("the observe→mutate→observe sequence did not land on a quiescent window in %d attempts — "+
                "if this repeats on an isolated database it is a REAL regression in PlatformStats", attempts)
}

// runStatsSequence is one full observe→mutate→observe attempt. It returns
// true when every assertion held; false means a foreign concurrent write
// moved the shared tables underneath this attempt (retry). Errors returned
// are hard failures (SQL/service breakage).
func runStatsSequence(ctx context.Context, pool *pgxpool.Pool, attempt int) (bool, error) {
        suffix := fmt.Sprintf("p7-stats-gate-%d-%d", time.Now().UnixNano(), attempt)
        email := suffix + "@gate.local"
        tidy := func() {
                _, _ = pool.Exec(ctx, `DELETE FROM companies WHERE email = $1`, email)
        }
        tidy()

        s0, err := readStatsSnapshot(ctx, pool)
        if err != nil {
                return false, err
        }
        if !s0.consistent() {
                return false, nil // foreign write inside the snapshot window
        }

        inserted, err := pool.Exec(ctx, `
                INSERT INTO companies (name, email, status)
                VALUES ($1, $2, 'active')
        `, suffix, email)
        if err != nil {
                return false, fmt.Errorf("fixture insert: %w", err)
        }
        if inserted.RowsAffected() != 1 {
                return false, fmt.Errorf("fixture insert touched %d rows, want 1", inserted.RowsAffected())
        }
        defer tidy()

        s1, err := readStatsSnapshot(ctx, pool)
        if err != nil {
                return false, err
        }
        switch {
        case s1.svc.TotalCompanies != s0.svc.TotalCompanies+1,
                s1.svc.ActiveCompanies != s0.svc.ActiveCompanies+1,
                s1.svc.SuspendedCompanies != s0.svc.SuspendedCompanies,
                s1.directTotal != s0.directTotal+1,
                s1.directActive != s0.directActive+1,
                s1.directSuspended != s0.directSuspended,
                !s1.consistent():
                return false, nil // the +1 deltas (service AND direct) were disturbed — retry
        }

        deleted, err := pool.Exec(ctx, `DELETE FROM companies WHERE email = $1`, email)
        if err != nil {
                return false, fmt.Errorf("fixture delete: %w", err)
        }
        if deleted.RowsAffected() != 1 {
                return false, fmt.Errorf("fixture delete touched %d rows, want 1", deleted.RowsAffected())
        }

        s2, err := readStatsSnapshot(ctx, pool)
        if err != nil {
                return false, err
        }
        if !s2.equal(s0) {
                return false, nil // the exact-baseline restoration was disturbed — retry
        }
        return true, nil // the aggregate tracks rows with no hidden caching
}
