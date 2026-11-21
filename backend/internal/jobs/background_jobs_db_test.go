// TestLibraryBackgroundImportDB / TestLibraryUpdateNoticeDB — api_level 118
// gates (migrations 51+52 + internal/jobs): the wholesale library import runs as
// a durable River job with a run-ledger row and a client email, and a
// re-published library notifies its importers. Pinned from the outside:
//
//   - enqueue is ONE atomic transaction: run row + river job + link, so a
//     duplicate enqueue (River unique on pharmacy+library, in-flight states
//     only) rolls the phantom run row back too — the wizard polls the real
//     run, never a ghost,
//   - the worker reuses services.ExecutePharmacyImportAll verbatim: same
//     red lines (zero inventory rows, cost never written), same plan gate,
//   - the run row ends 'succeeded' with exact counts + library_version, and
//     the success email reaches the COMPANY email via
//     pharmacies → accounts → companies (the tenant's real address chain),
//   - the update notice emails exactly the pharmacies whose sync pointer
//     trails the new version — up-to-date pharmacies and companies without
//     an address get nothing, and a duplicate notice for the same
//     library+version is absorbed by River's unique enforcement.
//
// TEST_DATABASE_URL=postgres://.../pharmacy_scratch go test ./internal/jobs/ -run 'TestLibraryBackgroundImportDB|TestLibraryUpdateNoticeDB' -v
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pharmacy-os/backend/internal/services"
)

func jobsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — background jobs gate needs a disposable PostgreSQL")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// mailRecorder captures the emails the workers dispatch.
type mailRecorder struct {
	mu   sync.Mutex
	sent []mailRecord
}

type mailRecord struct{ To, Subject, HTML, Text string }

func (r *mailRecorder) dispatch(_ context.Context, to, subject, html, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, mailRecord{To: to, Subject: subject, HTML: html, Text: text})
	return nil
}

func (r *mailRecorder) all() []mailRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]mailRecord(nil), r.sent...)
}

// importTestClient builds a REAL River client against the pool (insert +
// unique enforcement live, daemon never started — workers are invoked
// directly below for determinism).
func importTestClient(t *testing.T, pool *pgxpool.Pool, deps Deps) *Client {
	t.Helper()
	workers := river.NewWorkers()
	river.AddWorker(workers, &LibraryImportWorker{deps: deps})
	river.AddWorker(workers, &LibraryUpdateNoticeWorker{deps: deps})
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			"imports": {MaxWorkers: 1},
			"mail":    {MaxWorkers: 1},
		},
		Workers: workers,
	})
	require.NoError(t, err)
	return client
}

type importFixture struct {
	companyID, accountID, pharmacyID, employeeID string
	libraryID                                    string
	companyEmail                                 string
}

func seedImportFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, suffix string) importFixture {
	t.Helper()
	var fx importFixture
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO companies (name, email) VALUES ($1, $2) RETURNING id::text, email
        `, "p118-co "+suffix, "p118-owner-"+suffix+"@test.local").Scan(&fx.companyID, &fx.companyEmail))
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO accounts (company_name, contact_email, status, company_id)
                VALUES ($1, $2, 'active', $3::uuid) RETURNING id::text
        `, "p118 account "+suffix, "p118-acc-"+suffix+"@test.local", fx.companyID).Scan(&fx.accountID))
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacies (account_id, name, license_number, is_active, country)
                VALUES ($1, $2, $3, true, 'EG') RETURNING id::text
        `, fx.accountID, "p118-ph "+suffix, "P118-"+suffix).Scan(&fx.pharmacyID))
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO branches (pharmacy_id, name, code, is_active)
                VALUES ($1, 'main', $2, true) RETURNING id::text
        `, fx.pharmacyID, "P118B-"+suffix).Scan(new(string)))
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO employees (account_id, pharmacy_id, branch_id, email, first_name, last_name, status, permissions_initialized)
                VALUES ($1, $2, $3, $4, 'p118', 'gate', 'active', TRUE) RETURNING id::text
        `, fx.accountID, fx.pharmacyID, branchIDOf(t, ctx, pool, fx.pharmacyID), "p118-emp-"+suffix+"@test.local").Scan(&fx.employeeID))
	return fx
}

func branchIDOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pharmacyID string) string {
	t.Helper()
	var id string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id::text FROM branches WHERE pharmacy_id = $1::uuid LIMIT 1`, pharmacyID).Scan(&id))
	return id
}

func seedPublishedLibrary(t *testing.T, ctx context.Context, pool *pgxpool.Pool, suffix string) (string, []string) {
	t.Helper()
	var libraryID string
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO product_libraries (name, description, country_code, currency, is_published, version, published_at)
                VALUES ($1, 'p118 gate', 'EG', 'EGP', true, 1, NOW()) RETURNING id::text
        `, "P118 Lib "+suffix).Scan(&libraryID))
	gpIDs := make([]string, 0, 2)
	for _, spec := range []struct {
		name, barcode string
		price         int64
	}{
		{"P118 Alpha", "7720000000010", 1000},
		{"P118 Beta", "7720000000021", 2000},
	} {
		var gpID string
		require.NoError(t, pool.QueryRow(ctx, `
                        INSERT INTO global_products (name, dosage_form, product_category, requires_prescription, barcode, is_verified, source)
                        VALUES ($1, 'tablet', 'medication', 'no', $2, true, 'platform_admin')
                        RETURNING id::text
                `, spec.name, spec.barcode).Scan(&gpID))
		_, err := pool.Exec(ctx, `
                        INSERT INTO library_products (library_id, global_product_id, official_price_piastres)
                        VALUES ($1::uuid, $2::uuid, $3)
                `, libraryID, gpID, spec.price)
		require.NoError(t, err)
		gpIDs = append(gpIDs, gpID)
	}
	return libraryID, gpIDs
}

func setSyncPointer(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pharmacyID, libraryID string, version int) {
	t.Helper()
	_, err := pool.Exec(ctx, `
                INSERT INTO pharmacy_library_syncs (pharmacy_id, library_id, last_synced_version, synced_at)
                VALUES ($1::uuid, $2::uuid, $3, NOW())
                ON CONFLICT (pharmacy_id, library_id)
                DO UPDATE SET last_synced_version = $3, synced_at = NOW()
        `, pharmacyID, libraryID, version)
	require.NoError(t, err)
}

func TestLibraryBackgroundImportDB(t *testing.T) {
	ctx := context.Background()
	pool := jobsPool(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	fx := seedImportFixture(t, ctx, pool, suffix)
	libraryID, gpIDs := seedPublishedLibrary(t, ctx, pool, suffix)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM accounts WHERE id = $1`, fx.accountID)
		_, _ = pool.Exec(bg, `DELETE FROM companies WHERE id = $1`, fx.companyID)
		_, _ = pool.Exec(bg, `DELETE FROM product_libraries WHERE description = 'p118 gate'`)
		_, _ = pool.Exec(bg, `DELETE FROM global_products WHERE name LIKE 'P118 %'`)
	})

	mail := &mailRecorder{}
	deps := Deps{Pool: pool, Plan: nil, Mail: mail.dispatch}
	client := importTestClient(t, pool, deps)

	args := LibraryImportArgs{
		PharmacyID: fx.pharmacyID,
		LibraryID:  libraryID,
		CompanyID:  fx.companyID,
		ActorID:    fx.employeeID,
		ActorType:  "employee",
		ActorEmail: "p118-emp-" + suffix + "@test.local",
		ActorName:  "p118 gate",
		ActorRole:  "pharmacist",
	}

	// 1) Enqueue — one atomic row pair.
	runID, jobID, err := EnqueueLibraryImport(ctx, client, deps, args)
	require.NoError(t, err)
	require.NotZero(t, runID)
	require.NotZero(t, jobID)

	var runState string
	var runJobID int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT state, river_job_id FROM library_import_runs WHERE id = $1`, runID).Scan(&runState, &runJobID))
	assert.Equal(t, "queued", runState)
	assert.Equal(t, jobID, runJobID)
	var kind string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT kind FROM river_job WHERE id = $1`, jobID).Scan(&kind))
	assert.Equal(t, "pharmacy_library_import", kind)

	// 2) Duplicate enqueue — the in-flight unique guard rejects it AND
	//    the phantom run row is rolled back with it.
	_, _, dupErr := EnqueueLibraryImport(ctx, client, deps, args)
	require.ErrorIs(t, dupErr, ErrImportAlreadyInFlight)
	var runCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM library_import_runs WHERE pharmacy_id = $1::uuid AND library_id = $2::uuid`,
		fx.pharmacyID, libraryID).Scan(&runCount))
	assert.Equal(t, 1, runCount, "duplicate enqueue must not leave a second run row")

	// 3) The worker runs the EXACT service the synchronous path used.
	//    Its args come from the river_job row itself — the same decode
	//    the daemon performs — which is where the run id lives.
	var encodedArgs []byte
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT args FROM river_job WHERE id = $1`, jobID).Scan(&encodedArgs))
	var jobArgs LibraryImportArgs
	require.NoError(t, json.Unmarshal(encodedArgs, &jobArgs))
	assert.Equal(t, runID, jobArgs.RunID, "the encoded args carry the run row they belong to")
	w := &LibraryImportWorker{deps: deps}
	job := &river.Job[LibraryImportArgs]{
		JobRow: &rivertype.JobRow{ID: jobID, Attempt: 1, MaxAttempts: 3},
		Args:   jobArgs,
	}
	require.NoError(t, w.Work(ctx, job))

	// 4) Products landed as owned pharmacy products — prices only,
	//    zero inventory, cost untouched (the red lines).
	var created, withCost, withStock int
	require.NoError(t, pool.QueryRow(ctx, `
                SELECT COUNT(*),
                       COUNT(*) FILTER (WHERE cost_price <> 0),
                       COUNT(*) FILTER (WHERE EXISTS (SELECT 1 FROM inventory_batches b WHERE b.pharmacy_product_id = pp.id))
                FROM pharmacy_products pp
                WHERE pp.pharmacy_id = $1::uuid
        `, fx.pharmacyID).Scan(&created, &withCost, &withStock))
	assert.Equal(t, len(gpIDs), created)
	assert.Zero(t, withCost, "cost price is the pharmacy's — never written by import")
	assert.Zero(t, withStock, "import never touches inventory")

	// 5) Run row finalized with exact counts and the library version.
	var state, errMsg *string
	var libVersion, productsCreated, pricesUpdated int
	require.NoError(t, pool.QueryRow(ctx, `
                SELECT state, error_message, library_version, products_created, prices_updated
                FROM library_import_runs WHERE id = $1
        `, runID).Scan(&state, &errMsg, &libVersion, &productsCreated, &pricesUpdated))
	require.NotNil(t, state)
	assert.Equal(t, "succeeded", *state)
	assert.Nil(t, errMsg)
	assert.Equal(t, 1, libVersion)
	// A FIRST import CREATES every missing product — the price-refresh
	// counter stays at zero (it counts refreshes of OWNED rows only).
	assert.Equal(t, len(gpIDs), productsCreated)
	assert.Zero(t, pricesUpdated)

	// 6) Status endpoint service sees the same truth.
	status, err := services.LatestLibraryImportRun(ctx, pool, fx.pharmacyID, libraryID)
	require.NoError(t, err)
	require.NotNil(t, status)
	assert.Equal(t, "succeeded", status.State)
	assert.Equal(t, runID, status.RunID)

	// 7) Exactly one success email to the COMPANY address (the tenant
	//    chain: pharmacies → accounts → companies).
	sent := mail.all()
	require.Len(t, sent, 1)
	assert.Equal(t, fx.companyEmail, sent[0].To)
	assert.Contains(t, sent[0].Subject, "اكتمل استيراد مكتبة")
	assert.Contains(t, sent[0].HTML, "P118 Lib "+suffix)

	// 8) A second enqueue AFTER completion is legal (completed jobs do
	//    not block) — re-import is a price refresh, not an error. The
	//    daemon never ran, so the first job is still 'available'; it is
	//    finalized by hand to make the exact claim under test true.
	_, err = pool.Exec(ctx, `
                UPDATE river_job SET state = 'completed', finalized_at = NOW() WHERE id = $1
        `, jobID)
	require.NoError(t, err)
	_, _, err = EnqueueLibraryImport(ctx, client, deps, args)
	require.NoError(t, err)
}

func TestLibraryUpdateNoticeDB(t *testing.T) {
	ctx := context.Background()
	pool := jobsPool(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	fx := seedImportFixture(t, ctx, pool, suffix)
	libraryID, _ := seedPublishedLibrary(t, ctx, pool, suffix)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM accounts WHERE id = $1`, fx.accountID)
		_, _ = pool.Exec(bg, `DELETE FROM companies WHERE id = $1`, fx.companyID)
		_, _ = pool.Exec(bg, `DELETE FROM product_libraries WHERE description = 'p118 gate'`)
		_, _ = pool.Exec(bg, `DELETE FROM global_products WHERE name LIKE 'P118 %'`)
	})

	// A stale importer (v1) + an up-to-date importer (v2). Both share the
	// library; only the stale one should hear about the new version.
	setSyncPointer(t, ctx, pool, fx.pharmacyID, libraryID, 1)

	var accountID2, pharmacyID2, companyID2 string
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO companies (name, email) VALUES ($1, $2) RETURNING id::text
        `, "p118-co2 "+suffix, "p118-owner2-"+suffix+"@test.local").Scan(&companyID2))
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO accounts (company_name, contact_email, status, company_id)
                VALUES ($1, $2, 'active', $3::uuid) RETURNING id::text
        `, "p118 account2 "+suffix, "p118-acc2-"+suffix+"@test.local", companyID2).Scan(&accountID2))
	require.NoError(t, pool.QueryRow(ctx, `
                INSERT INTO pharmacies (account_id, name, license_number, is_active, country)
                VALUES ($1, $2, $3, true, 'EG') RETURNING id::text
        `, accountID2, "p118-ph2 "+suffix, "P118X-"+suffix).Scan(&pharmacyID2))
	setSyncPointer(t, ctx, pool, pharmacyID2, libraryID, 2)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM accounts WHERE id IN ($1, $2)`, fx.accountID, accountID2)
		_, _ = pool.Exec(bg, `DELETE FROM companies WHERE id IN ($1, $2)`, fx.companyID, companyID2)
	})

	// Library re-published: version 2.
	_, err := pool.Exec(ctx, `UPDATE product_libraries SET version = 2 WHERE id = $1::uuid`, libraryID)
	require.NoError(t, err)

	mail := &mailRecorder{}
	deps := Deps{Pool: pool, Plan: nil, Mail: mail.dispatch}
	client := importTestClient(t, pool, deps)

	args := LibraryUpdateNoticeArgs{LibraryID: libraryID, Version: 2}
	require.NoError(t, EnqueueLibraryUpdateNotice(ctx, client, deps, args))

	// Duplicate notice for the SAME library+version is absorbed (unique).
	require.NoError(t, EnqueueLibraryUpdateNotice(ctx, client, deps, args))
	var noticeJobs int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM river_job WHERE kind = 'library_update_notice' AND (args->>'library_id') = $1`,
		libraryID).Scan(&noticeJobs))
	assert.Equal(t, 1, noticeJobs, "one notice job per library+version")

	w := &LibraryUpdateNoticeWorker{deps: deps}
	job := &river.Job[LibraryUpdateNoticeArgs]{
		JobRow: &rivertype.JobRow{ID: 1, Attempt: 1, MaxAttempts: 5},
		Args:   args,
	}
	require.NoError(t, w.Work(ctx, job))

	sent := mail.all()
	require.Len(t, sent, 1, "exactly the stale importer is notified")
	assert.Equal(t, fx.companyEmail, sent[0].To)
	assert.Contains(t, sent[0].Subject, "تحديث جديد لمكتبة")
	assert.True(t, strings.Contains(sent[0].HTML, "v2") || strings.Contains(sent[0].HTML, "الإصدار 2"),
		"the notice names the new version")
	assert.Contains(t, sent[0].HTML, "P118 Lib "+suffix)
}
