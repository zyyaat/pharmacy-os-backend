// Library import — the background engine behind «استيراد المكتبة كاملة».
//
// The wholesale import (mode:'all') used to run inside the HTTP request:
// fine at 4s, hostile at 30s+. It now becomes ONE durable job whose single
// transaction is the exact same services.ExecutePharmacyImportAll the
// synchronous path ran — same red lines, same plan gate, same audit row —
// plus two side effects the request could never guarantee: a run-ledger row
// the status endpoint polls (library_import_runs, migrations 51+52) and a
// success/failure email to the client when the job finalizes.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/riverqueue/river"

	"github.com/pharmacy-os/backend/internal/auth"
	"github.com/pharmacy-os/backend/internal/services"
)

// ErrImportAlreadyInFlight — a wholesale import for this pharmacy+library is
// already queued or running (River unique enforcement). The handler maps it
// to 409; the wizard keeps polling the existing run instead.
var ErrImportAlreadyInFlight = errors.New("library import already in flight")

// LibraryImportArgs carries the actor identity captured at enqueue time so
// the audit row written at run time names the person who clicked, and the
// company identity so the plan gate and the email resolve without an HTTP
// principal. PharmacyID+LibraryID are the uniqueness identity
// (`river:"unique"` — one in-flight import per pharmacy+library).
type LibraryImportArgs struct {
	PharmacyID string `json:"pharmacy_id" river:"unique"`
	LibraryID  string `json:"library_id" river:"unique"`

	CompanyID string `json:"company_id"`
	RunID     int64  `json:"run_id"`

	ActorID    string `json:"actor_id"`
	ActorType  string `json:"actor_type"`
	ActorEmail string `json:"actor_email"`
	ActorName  string `json:"actor_name"`
	ActorRole  string `json:"actor_role"`
}

// Kind identifies the job on the wire and in river_job.
func (LibraryImportArgs) Kind() string { return "pharmacy_library_import" }

// EnqueueLibraryImport atomically creates the run-ledger row and the River
// job (one transaction: a crash can leave neither, never an orphan status
// the wizard would poll forever). Returns the run id and river job id.
func EnqueueLibraryImport(ctx context.Context, client *Client, deps Deps, args LibraryImportArgs) (int64, int64, error) {
	if client == nil {
		return 0, 0, errors.New("background jobs unavailable")
	}
	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var runID int64
	if err := tx.QueryRow(ctx, `
                INSERT INTO library_import_runs (pharmacy_id, library_id, actor_user_id)
                VALUES ($1::uuid, $2::uuid, NULLIF($3, '')::uuid)
                RETURNING id
        `, args.PharmacyID, args.LibraryID, args.ActorID).Scan(&runID); err != nil {
		return 0, 0, err
	}
	args.RunID = runID

	opts := uniqueInFlight()
	opts.Queue = "imports"
	opts.MaxAttempts = 3
	insert, err := client.InsertTx(ctx, tx, args, opts)
	if err != nil {
		return 0, 0, err
	}
	if insert.UniqueSkippedAsDuplicate {
		// Roll the whole tx back — the caller polls the run that is
		// already in flight instead of a phantom second one.
		return 0, 0, ErrImportAlreadyInFlight
	}
	if _, err := tx.Exec(ctx,
		`UPDATE library_import_runs SET river_job_id = $2 WHERE id = $1`,
		runID, insert.Job.ID); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return runID, insert.Job.ID, nil
}

// LibraryImportWorker runs one wholesale import to completion.
type LibraryImportWorker struct {
	river.WorkerDefaults[LibraryImportArgs]
	deps Deps
}

// Work executes the import through the exact service the synchronous path
// used, then finalizes the run row and (best-effort) the client email. A
// mail failure never fails the job — the import itself succeeded and the
// status endpoint shows the truth regardless.
func (w *LibraryImportWorker) Work(ctx context.Context, job *river.Job[LibraryImportArgs]) error {
	args := job.Args

	plan := services.PlanState{Used: 0, Limit: -1}
	if w.deps.Plan != nil && args.CompanyID != "" {
		plan = w.deps.Plan(ctx, args.CompanyID)
	}
	principal := &auth.Principal{
		ID:          args.ActorID,
		Type:        args.ActorType,
		Email:       args.ActorEmail,
		DisplayName: args.ActorName,
		Role:        args.ActorRole,
	}

	if _, err := w.deps.Pool.Exec(ctx,
		`UPDATE library_import_runs SET state = 'running', started_at = COALESCE(started_at, NOW()) WHERE id = $1`,
		args.RunID); err != nil {
		return fmt.Errorf("mark run running: %w", err)
	}

	out, err := services.ExecutePharmacyImportAll(ctx, w.deps.Pool, principal, args.PharmacyID, args.LibraryID, plan)
	if err != nil {
		// Final attempt only: intermediate failures stay 'running'
		// (the retry picks the same run row up) and River's own
		// retry policy governs. The email tells the client it needs
		// to look at the wizard again.
		if job.Attempt >= job.MaxAttempts {
			msg := importFailureMessage(err)
			if _, uerr := w.deps.Pool.Exec(ctx,
				`UPDATE library_import_runs SET state = 'failed', error_message = $2, finished_at = NOW() WHERE id = $1`,
				args.RunID, msg); uerr != nil {
				log.Printf("[jobs] import run %d: mark failed failed: %v", args.RunID, uerr)
			}
			w.notifyFinalFailure(ctx, args, msg)
		}
		return err
	}

	skipped := len(out.Skipped)
	if _, err := w.deps.Pool.Exec(ctx, `
                UPDATE library_import_runs
                SET state = 'succeeded', library_version = $2, products_created = $3, products_linked = $4,
                    prices_updated = $5, skipped_count = $6, finished_at = NOW()
                WHERE id = $1
        `, args.RunID, out.LibraryVersion, out.ProductsCreated, out.ProductsLinked, out.PricesUpdated, skipped); err != nil {
		return fmt.Errorf("mark run succeeded: %w", err)
	}

	w.notifySuccess(ctx, args, importSuccessSummary{
		LibraryVersion: out.LibraryVersion,
		Created:        out.ProductsCreated,
		Linked:         out.ProductsLinked,
		PricesUpdated:  out.PricesUpdated,
		Skipped:        skipped,
		Truncated:      out.SkippedTruncated,
	})
	return nil
}

// importFailureMessage keeps the operator-facing error short and honest —
// the typed service failures carry their stage; anything else is verbatim.
func importFailureMessage(err error) string {
	var fail *services.PharmacyLibrariesFailure
	if errors.As(err, &fail) {
		return fmt.Sprintf("فشل عند المرحلة: %s", fail.Stage)
	}
	const max = 280
	msg := err.Error()
	if len(msg) > max {
		msg = msg[:max]
	}
	return msg
}

// importContext is the shared row behind both import emails.
type importContext struct {
	PharmacyName string
	LibraryName  string
	ClientEmail  string
}

func (w *LibraryImportWorker) loadContext(ctx context.Context, args LibraryImportArgs) (importContext, bool) {
	if w.deps.Mail == nil {
		return importContext{}, false
	}
	var ic importContext
	err := w.deps.Pool.QueryRow(ctx, `
                SELECT ph.name, l.name, COALESCE(c.email, '')
                FROM pharmacies ph
                JOIN product_libraries l ON l.id = $2::uuid
                LEFT JOIN accounts a ON a.id = ph.account_id
                LEFT JOIN companies c ON c.id = a.company_id
                WHERE ph.id = $1::uuid
        `, args.PharmacyID, args.LibraryID).Scan(&ic.PharmacyName, &ic.LibraryName, &ic.ClientEmail)
	if err != nil || ic.ClientEmail == "" {
		// No address → nothing to notify, and not an error worth
		// noise: the status endpoint is the source of truth.
		return importContext{}, false
	}
	return ic, true
}

func (w *LibraryImportWorker) notifySuccess(ctx context.Context, args LibraryImportArgs, s importSuccessSummary) {
	ic, ok := w.loadContext(ctx, args)
	if !ok {
		return
	}
	subject, html, text := importSuccessEmail(ic, s)
	if err := w.deps.Mail(ctx, ic.ClientEmail, subject, html, text); err != nil {
		log.Printf("[jobs] import success email failed run=%d to=%s: %v", args.RunID, ic.ClientEmail, err)
	}
}

func (w *LibraryImportWorker) notifyFinalFailure(ctx context.Context, args LibraryImportArgs, msg string) {
	ic, ok := w.loadContext(ctx, args)
	if !ok {
		return
	}
	subject, html, text := importFailureEmail(ic, msg)
	if err := w.deps.Mail(ctx, ic.ClientEmail, subject, html, text); err != nil {
		log.Printf("[jobs] import failure email failed run=%d to=%s: %v", args.RunID, ic.ClientEmail, err)
	}
}

// importSuccessSummary is the wire of the success email.
type importSuccessSummary struct {
	LibraryVersion int
	Created        int
	Linked         int
	PricesUpdated  int
	Skipped        int
	Truncated      bool
}

func importSuccessEmail(ic importContext, s importSuccessSummary) (string, string, string) {
	subject := fmt.Sprintf("اكتمل استيراد مكتبة «%s» — Pharmacy OS", ic.LibraryName)
	text := fmt.Sprintf(
		"صيدلية %s\n\nاكتمل استيراد مكتبة «%s» (الإصدار %d) في الخلفية:\n"+
			"— منتجات أُنشئت: %d\n— منتجات مربوطة بمنتجاتك القائمة: %d\n— أسعار محدّثة: %d\n"+
			"— منتجات متخطاة: %d%s\n\nالمنتجات المستوردة ملكك الآن وتعدّل فيها بحرية.\n"+
			"افتح التطبيق: المنتجات.",
		ic.PharmacyName, ic.LibraryName, s.LibraryVersion, s.Created, s.Linked, s.PricesUpdated,
		s.Skipped, truncationNote(s.Truncated))
	html := fmt.Sprintf(`<!DOCTYPE html><html><body style="font-family:Arial,sans-serif;background:#f6f7f9;padding:24px" dir="rtl">`+
		`<div style="max-width:520px;margin:auto;background:#fff;border-radius:12px;padding:24px">`+
		`<h2 style="margin:0 0 12px;color:#0f766e;font-size:18px">Pharmacy OS — اكتمل الاستيراد</h2>`+
		`<p style="margin:0 0 8px;color:#475569;font-size:14px">صيدلية %s — مكتبة «%s» (الإصدار %d)</p>`+
		`<table style="border-collapse:collapse;font-size:14px;color:#0f172a;margin:12px 0">`+
		rowCell("منتجات أُنشئت", s.Created)+rowCell("منتجات مربوطة بقائمتك", s.Linked)+
		rowCell("أسعار محدّثة", s.PricesUpdated)+rowCell("منتجات متخطاة", s.Skipped)+
		`</table>`+
		`<p style="margin:8px 0 0;color:#475569;font-size:13px">المنتجات المستوردة ملكك الآن — عدّل فيها بحرية، والأسعار والبيانات الرسمية مرجع فقط.</p>`+
		`<p style="margin:16px 0 0;color:#94a3b8;font-size:12px">%s</p>`+
		`</div></body></html>`,
		ic.PharmacyName, ic.LibraryName, s.LibraryVersion,
		rowCell("منتجات أُنشئت", s.Created), rowCell("منتجات مربوطة بقائمتك", s.Linked),
		rowCell("أسعار محدّثة", s.PricesUpdated), rowCell("منتجات متخطاة", s.Skipped),
		truncationNote(s.Truncated))
	return subject, html, text
}

func importFailureEmail(ic importContext, msg string) (string, string, string) {
	subject := fmt.Sprintf("تعذّر استيراد مكتبة «%s» — Pharmacy OS", ic.LibraryName)
	text := fmt.Sprintf(
		"صيدلية %s\n\nتعذّر استيراد مكتبة «%s» بعد كل المحاولات.\nالسبب المسجل: %s\n\n"+
			"جرّب مرة أخرى من صفحة المكتبات، ولو تكرر تواصل مع الدعم من داخل التطبيق.",
		ic.PharmacyName, ic.LibraryName, msg)
	html := fmt.Sprintf(`<!DOCTYPE html><html><body style="font-family:Arial,sans-serif;background:#f6f7f9;padding:24px" dir="rtl">`+
		`<div style="max-width:520px;margin:auto;background:#fff;border-radius:12px;padding:24px">`+
		`<h2 style="margin:0 0 12px;color:#b91c1c;font-size:18px">Pharmacy OS — تعذّر الاستيراد</h2>`+
		`<p style="margin:0 0 8px;color:#475569;font-size:14px">صيدلية %s — مكتبة «%s»</p>`+
		`<p style="margin:8px 0;color:#0f172a;font-size:14px;line-height:1.6">تعذّر الاستيراد بعد كل المحاولات. السبب المسجل: %s</p>`+
		`<p style="margin:8px 0 0;color:#475569;font-size:13px">جرّب مرة أخرى من صفحة المكتبات، ولو تكرر تواصل مع الدعم من داخل التطبيق.</p>`+
		`</div></body></html>`,
		ic.PharmacyName, ic.LibraryName, msg)
	return subject, html, text
}

func rowCell(label string, n int) string {
	return fmt.Sprintf(`<tr><td style="padding:4px 12px 4px 0;color:#475569">%s</td><td style="padding:4px 0;font-weight:bold">%d</td></tr>`, label, n)
}

func truncationNote(truncated bool) string {
	if !truncated {
		return ""
	}
	return " (القائمة مقصوصة عند سقف الخطة — الباقي متاح بسحب التحديثات)"
}
