// Library update notice — «لما يظهر تحديث للمكتبة».
//
// When a platform admin publishes a NEW version of a library (re-publish
// with version+1), every pharmacy that already imported it and sits on an
// older synced version gets one email: the in-app badge already shows
// update_available; this is the push that reaches the owner who isn't
// looking. First publishes enqueue nothing (nobody could have imported a
// library that was never published).
package jobs

import (
	"context"
	"fmt"
	"log"

	"github.com/riverqueue/river"
)

// LibraryUpdateNoticeArgs — LibraryID+Version are the uniqueness identity:
// one notice per library version, ever (a completed notice also blocks a
// duplicate by River's default ByState which includes completed — exactly
// right for a monotonic version: the same version never re-notifies).
type LibraryUpdateNoticeArgs struct {
	LibraryID string `json:"library_id" river:"unique"`
	Version   int    `json:"version" river:"unique"`
}

// Kind identifies the job on the wire and in river_job.
func (LibraryUpdateNoticeArgs) Kind() string { return "library_update_notice" }

// EnqueueLibraryUpdateNotice queues the notice for one published version.
// Safe to call with a nil client (background processing disabled) — the
// publish flow must never fail because notifications are off.
func EnqueueLibraryUpdateNotice(ctx context.Context, client *Client, deps Deps, args LibraryUpdateNoticeArgs) error {
	if client == nil {
		return nil
	}
	opts := uniqueInFlight()
	opts.Queue = "mail"
	opts.MaxAttempts = 5
	insert, err := client.Insert(ctx, args, opts)
	if err != nil {
		return err
	}
	if insert.UniqueSkippedAsDuplicate {
		return nil
	}
	return nil
}

// LibraryUpdateNoticeWorker walks the importing pharmacies of one library
// version and emails their owners. One job per version; the walk batches
// nothing fancy — the mail transport itself is the bottleneck and the job
// queue serializes by design.
type LibraryUpdateNoticeWorker struct {
	river.WorkerDefaults[LibraryUpdateNoticeArgs]
	deps Deps
}

// Work sends the notice to every pharmacy whose sync pointer trails the
// published version. A pharmacy with no company email is skipped silently
// (the badge in-app is its notification). Send failures are logged per
// recipient and retried by River at the JOB level only when the whole walk
// fails (query error) — a single dead mailbox must not re-email everyone.
func (w *LibraryUpdateNoticeWorker) Work(ctx context.Context, job *river.Job[LibraryUpdateNoticeArgs]) error {
	if w.deps.Mail == nil {
		return nil
	}
	args := job.Args

	rows, err := w.deps.Pool.Query(ctx, `
                SELECT ph.name, c.email
                FROM pharmacy_library_syncs s
                JOIN pharmacies ph ON ph.id = s.pharmacy_id
                JOIN accounts a ON a.id = ph.account_id
                JOIN companies c ON c.id = a.company_id
                WHERE s.library_id = $1::uuid
                  AND s.last_synced_version < $2
                  AND COALESCE(c.email, '') <> ''
                ORDER BY ph.name
        `, args.LibraryID, args.Version)
	if err != nil {
		return fmt.Errorf("list stale importers: %w", err)
	}
	defer rows.Close()

	var libraryName string
	_ = w.deps.Pool.QueryRow(ctx,
		`SELECT name FROM product_libraries WHERE id = $1::uuid`,
		args.LibraryID).Scan(&libraryName)

	sent, failed := 0, 0
	for rows.Next() {
		var pharmacyName, email string
		if err := rows.Scan(&pharmacyName, &email); err != nil {
			return fmt.Errorf("scan importer: %w", err)
		}
		subject, html, text := libraryUpdateEmail(libraryName, args.Version, pharmacyName)
		if err := w.deps.Mail(ctx, email, subject, html, text); err != nil {
			failed++
			log.Printf("[jobs] update notice email failed library=%s v=%d to=%s: %v",
				args.LibraryID, args.Version, email, err)
			continue
		}
		sent++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("walk importers: %w", err)
	}
	log.Printf("[jobs] update notice library=%s v=%d: %d sent, %d failed",
		args.LibraryID, args.Version, sent, failed)
	return nil
}

func libraryUpdateEmail(libraryName string, version int, pharmacyName string) (string, string, string) {
	subject := fmt.Sprintf("تحديث جديد لمكتبة «%s» (الإصدار %d) — Pharmacy OS", libraryName, version)
	text := fmt.Sprintf(
		"صيدلية %s\n\nالمكتبة «%s» نشرت إصدارًا جديدًا (%d): أسعار وبيانات محدّثة متاحة الآن.\n"+
			"السحب لا يمس مخزونك ولا أسعارك المعدّلة — يزوّدك بالرسمي الجديد فقط.\n\n"+
			"افتح التطبيق: المكتبات ← «%s» ← اسحب التحديثات.",
		pharmacyName, libraryName, version, libraryName)
	html := fmt.Sprintf(`<!DOCTYPE html><html><body style="font-family:Arial,sans-serif;background:#f6f7f9;padding:24px" dir="rtl">`+
		`<div style="max-width:520px;margin:auto;background:#fff;border-radius:12px;padding:24px">`+
		`<h2 style="margin:0 0 12px;color:#0f766e;font-size:18px">Pharmacy OS — تحديث مكتبة متاح</h2>`+
		`<p style="margin:0 0 8px;color:#475569;font-size:14px">صيدلية %s — مكتبة «%s» نشرت الإصدار %d</p>`+
		`<p style="margin:8px 0;color:#0f172a;font-size:14px;line-height:1.6">أسعار وبيانات محدّثة متاحة الآن. السحب لا يمس مخزونك ولا أسعارك المعدّلة — يزوّدك بالرسمي الجديد فقط.</p>`+
		`<p style="margin:16px 0 0;color:#94a3b8;font-size:12px">افتح التطبيق: المكتبات ← «%s» ← اسحب التحديثات.</p>`+
		`</div></body></html>`,
		pharmacyName, libraryName, version, libraryName)
	return subject, html, text
}
