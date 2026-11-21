package database

// Regression lock for AUD T-C1 (MEDIUM, migration bookkeeping) — the
// version row must commit in the SAME transaction as the migration DDL.
//
// The old shape recorded the version AFTER tx.Commit: a crash in that
// window left an applied-but-unrecorded migration, and the next boot
// re-ran it, boot-looping on the first already-applied statement
// (duplicate column / duplicate table) until a human intervened.
//
// Two layers protect the fix:
//
//  1. TestMigrationRecordCommittedAtomicallyWithDDL — a source-contract
//     probe (CI-visible, no DB): inside the apply block, recordMigration
//     must be called with the TRANSACTION (not the pool connection) and
//     must appear BEFORE tx.Commit.
//  2. TestRecordMigrationComposesWithTransaction — a behavioral probe
//     (skipped without TEST_DATABASE_URL): the real recordMigration writes
//     through a pgx.Tx and a ROLLBACK discards BOTH the DDL and the ledger
//     row — the property the migrator now relies on.

import (
	"os"
	"strings"
	"testing"
)

func TestMigrationRecordCommittedAtomicallyWithDDL(t *testing.T) {
	src, err := os.ReadFile("migrator.go")
	if err != nil {
		t.Fatalf("read migrator.go: %v", err)
	}
	text := string(src)

	// Anchor the apply block precisely: from the migration file read to the
	// post-loop drift warning. The legacy pre-existing-schema branch (which
	// legitimately records through the pool connection) sits BEFORE the
	// first anchor and is excluded from the slice.
	start := strings.Index(text, "sqlBytes, err := migrations.FS.ReadFile")
	end := strings.Index(text, "warnPlanPermissionDrift(ctx, conn)")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("apply block anchors not found (start=%d end=%d) — the migrator shape changed; update this contract", start, end)
	}
	block := text[start:end]

	if strings.Contains(block, "recordMigration(ctx, conn, item.name)") {
		t.Fatal("apply block still records the version through the pool connection — the crash window between Commit and record is back (AUD T-C1)")
	}
	recordIdx := strings.Index(block, "recordMigration(ctx, tx, item.name)")
	if recordIdx < 0 {
		t.Fatal("apply block does not record the version through the transaction — the record is no longer atomic with the DDL (AUD T-C1)")
	}
	commitIdx := strings.LastIndex(block, "tx.Commit(ctx)")
	if commitIdx < 0 {
		t.Fatal("apply block lost its tx.Commit")
	}
	if recordIdx > commitIdx {
		t.Fatal("recordMigration called after tx.Commit — the crash window is back (AUD T-C1)")
	}
	// A failed record must abort the migration, not commit it half-done.
	if !strings.Contains(block, "record migration %s: %w") {
		t.Fatal("record failure is not wrapped/propagated — a failed version write would be silently ignored")
	}
}
