package database

// AUD-005 — pre-destruction logical snapshot.
//
// DockHosting's automatic backups cover the platform side, but the one
// moment data is most likely to die is a destructive migration executing
// at startup. When such a migration passes the AUD-007 gate, the runner
// takes a best-effort pg_dump (custom format) first:
//
//   - pg_dump client present  → dump runs; a FAILED dump blocks the
//     migration in production (fail-safe) and only warns in development.
//   - pg_dump client absent   → startup proceeds with a warning; the
//     AUD-007 gate (operator acknowledgment) remains the primary wall.
//
// The production image ships a postgresql client (Dockerfile) so the
// snapshot path is real, not theoretical. Snapshots land in
// MIGRATION_SNAPSHOT_DIR (default /tmp/pharmacy-migration-backups); the
// value "off" disables snapshots entirely as an operator escape hatch —
// the gate still applies.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// snapshotDumpTimeout caps a single pg_dump run so a wedged dump cannot
// eat the whole 5-minute migration budget that main.go grants.
const snapshotDumpTimeout = 2 * time.Minute

// snapshotOff is the sentinel value of MIGRATION_SNAPSHOT_DIR that
// disables snapshots (operator escape hatch; the AUD-007 gate still runs).
const snapshotOff = "off"

// defaultSnapshotDir is where pre-migration dumps land when
// MIGRATION_SNAPSHOT_DIR is unset. /tmp is writable on every PaaS
// container (ephemeral — copy the file out if you need to keep it).
const defaultSnapshotDir = "/tmp/pharmacy-migration-backups"

// dumpRunner is the exec seam for tests.
var dumpRunner = func(ctx context.Context, bin string, args []string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// findPgDump locates the pg_dump client: PGDUMP_PATH (explicit, used only
// if it exists) then PATH. The second return is false when unavailable.
func findPgDump() (string, bool) {
	if p := strings.TrimSpace(os.Getenv("PGDUMP_PATH")); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
		return "", false
	}
	p, err := exec.LookPath("pg_dump")
	return p, err == nil
}

// snapshotDisabled reports the operator escape hatch: when
// MIGRATION_SNAPSHOT_DIR is exactly "off" (case-insensitive), snapshots
// are skipped.
func snapshotDisabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("MIGRATION_SNAPSHOT_DIR")), snapshotOff)
}

// snapshotBeforeDestructive runs pg_dump --format=custom against dsn into
// the snapshot directory. Returns (path, attempted, error):
//
//   - attempted=false → no client available (or disabled): caller logs and
//     proceeds; only the AUD-007 gate protects data this boot.
//   - attempted=true, err=nil → dump file written at path.
//   - attempted=true, err≠nil → dump attempted but failed; production
//     must refuse the destructive migration.
//
// The DSN is passed as a pg_dump argument (pg_dump accepts connection
// URIs); the container is single-tenant so argv exposure is acceptable
// and documented in the deployment guide.
func snapshotBeforeDestructive(ctx context.Context, dsn, migration string) (string, bool, error) {
	if snapshotDisabled() {
		return "", false, nil
	}
	bin, ok := findPgDump()
	if !ok {
		return "", false, nil
	}
	dir := strings.TrimSpace(os.Getenv("MIGRATION_SNAPSHOT_DIR"))
	if dir == "" {
		dir = defaultSnapshotDir
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", true, fmt.Errorf("create snapshot dir %s: %w", dir, err)
	}
	fileName := fmt.Sprintf("pre_%s_%s.dump", time.Now().UTC().Format("20060102T150405Z"), migration)
	out := filepath.Join(dir, fileName)
	dctx, cancel := context.WithTimeout(ctx, snapshotDumpTimeout)
	defer cancel()
	args := []string{"--format=custom", "--file=" + out, dsn}
	if err := dumpRunner(dctx, bin, args); err != nil {
		return out, true, fmt.Errorf("pg_dump: %w", err)
	}
	return out, true, nil
}

// snapshotProductionStrict is the decision wrapper around
// snapshotBeforeDestructive used by RunMigrations: in production a failed
// attempt aborts the migration; everywhere else it only warns.
func snapshotProductionStrict(appEnv string) bool {
	return appEnv == "production" || appEnv == "staging"
}
