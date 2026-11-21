// Package jobs — River Queue background processing on PostgreSQL.
//
// This is the engine behind «الاستيراد في الخلفية» (api_level 118): the
// pharmacy-side wholesale library import no longer blocks an HTTP request
// for seconds; it becomes a durable job row that survives deploys and
// restarts, with a success/failure email to the client when it finalizes.
// The same engine carries the library-update notice: when a platform admin
// publishes a new version, every pharmacy that imported the library gets
// an email telling it to pull.
//
// Two dedicated queues (created by migrations 51+52 alongside River's schema):
//
//	imports — MaxWorkers 1: a wholesale import is a heavy transaction;
//	          serializing them protects the database from pile-ups.
//	mail    — MaxWorkers 4: short external HTTP/SMTP calls.
//
// Jobs use River's unique enforcement keyed on their business identity
// (pharmacy+library for imports, library+version for notices) restricted to
// in-flight states, so a double-click cannot queue two imports, while a
// COMPLETED import never blocks the next one.
//
// The package stays orchestration-only: business transactions live in
// internal/services (ADR-0005) and are called directly by the workers; the
// only transaction authored here is the enqueue itself (run-ledger row +
// river insert must commit atomically or the status endpoint would show a
// phantom).
package jobs

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/pharmacy-os/backend/internal/services"
)

// Client is the River client handle the handlers keep for enqueueing.
// nil means background processing is unavailable (kill switch, or a
// construction failure logged at boot) — enqueue paths answer 503.
type Client = river.Client[pgx.Tx]

// MailDispatch sends one transactional email. Injectable so tests capture
// instead of dialing; the production closure resolves the configured
// transport (Brevo / SMTP) via mailx.Dispatch and skips silently when the
// operator has no mail configured at all.
type MailDispatch func(ctx context.Context, to, subject, html, text string) error

// PlanResolver resolves the tenant's product plan at WORK time — plan state
// can drift between enqueue and run, so the limit is re-read when the job
// executes, exactly as the synchronous path re-read it per request.
type PlanResolver func(ctx context.Context, companyID string) services.PlanState

// Deps wires the workers to the process they run inside.
type Deps struct {
	Pool *pgxpool.Pool
	Plan PlanResolver // nil → unlimited (same degrade the HTTP path uses on gate errors)
	Mail MailDispatch // nil → no email side effect
}

// uniqueInFlight limits River's unique enforcement to the states that mean
// "still going": a completed or discarded job must never block the next
// legitimate run of the same business identity. retryable stays INCLUDED on
// purpose — a job waiting on its retry is still the one live attempt, and
// letting a duplicate in would double-work it.
//
// ByArgs:true is what activates the `river:"unique"` struct tags on the args
// (pharmacy+library for imports, library+version for notices). WITHOUT it
// the key degrades to the kind alone — every pharmacy's import would queue
// behind every other pharmacy's, a cross-tenant deadlock the import gate
// test caught exactly this way.
func uniqueInFlight() *river.InsertOpts {
	return &river.InsertOpts{UniqueOpts: river.UniqueOpts{
		ByArgs: true,
		ByState: []rivertype.JobState{
			rivertype.JobStateAvailable,
			rivertype.JobStatePending,
			rivertype.JobStateRunning,
			rivertype.JobStateRetryable,
			rivertype.JobStateScheduled,
		},
	}}
}

// Start builds the River client with both workers, starts the daemon on ctx
// and returns the handle. The caller owns ctx: cancel it (after the HTTP
// drain, ideally with Stop) to wind workers down.
func Start(ctx context.Context, deps Deps) (*Client, error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, &LibraryImportWorker{deps: deps})
	river.AddWorker(workers, &LibraryUpdateNoticeWorker{deps: deps})

	client, err := river.NewClient(riverpgxv5.New(deps.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			"imports": {MaxWorkers: 1},
			"mail":    {MaxWorkers: 4},
		},
		Workers: workers,
	})
	if err != nil {
		return nil, err
	}
	if err := client.Start(ctx); err != nil {
		return nil, err
	}
	return client, nil
}

// Enabled reports the operator kill switch (JOBS_ENABLED=0 turns the engine
// off for an emergency; the HTTP paths then answer 503 instead of queueing
// into a client no daemon will ever serve).
func Enabled() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("JOBS_ENABLED")), "0")
}

// Stop winds the daemon down inside budget, re-queueing whatever was still
// running (River returns in-flight jobs to `available` on stop).
func Stop(client *Client, budget time.Duration) {
	if client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if err := client.Stop(ctx); err != nil {
		log.Printf("[jobs] graceful stop incomplete (%v) — in-flight jobs return to the queue on next boot", err)
	}
}
