package database

import (
        "context"
        "errors"
        "os"
        "path/filepath"
        "reflect"
        "strings"
        "testing"
        "time"
)

// resetDumpRunner restores the exec seam after a test swapped it.
func resetDumpRunner(t *testing.T) {
        t.Helper()
        orig := dumpRunner
        t.Cleanup(func() { dumpRunner = orig })
        return
}

// existingClientPath returns a real (empty) file path so the PGDUMP_PATH
// existence check passes while the stubbed runner is what actually runs.
func existingClientPath(t *testing.T) string {
        t.Helper()
        p := filepath.Join(t.TempDir(), "pgdump-client")
        if err := os.WriteFile(p, []byte{}, 0o755); err != nil {
                t.Fatalf("create fake client file: %v", err)
        }
        return p
}

func TestSnapshotBeforeDestructive_StubRunner(t *testing.T) {
        resetDumpRunner(t)
        dir := t.TempDir()
        t.Setenv("MIGRATION_SNAPSHOT_DIR", dir)

        var gotBin string
        var gotArgs []string
        dumpRunner = func(ctx context.Context, bin string, args []string) error {
                gotBin = bin
                gotArgs = args
                return nil
        }
        t.Setenv("PGDUMP_PATH", existingClientPath(t))

        path, attempted, err := snapshotBeforeDestructive(context.Background(), "postgresql://u:p@h:5432/db", "00000000000035_example.sql")
        if err != nil {
                t.Fatalf("unexpected error: %v", err)
        }
        if !attempted {
                t.Fatal("attempted = false, want true when a client is configured")
        }
        if gotBin == "" || gotBin != os.Getenv("PGDUMP_PATH") {
                t.Fatalf("binary = %q, want PGDUMP_PATH value", gotBin)
        }
        wantArgs := []string{"--format=custom", "--file=" + path, "postgresql://u:p@h:5432/db"}
        if !reflect.DeepEqual(gotArgs, wantArgs) {
                t.Fatalf("args = %v, want %v", gotArgs, wantArgs)
        }
        if base := filepath.Base(path); !strings.HasPrefix(base, "pre_") || !strings.Contains(base, "00000000000035_example.sql") || !strings.HasSuffix(base, ".dump") {
                t.Fatalf("file name = %q, want pre_<ts>_<migration>.dump shape", base)
        }
}

func TestSnapshotBeforeDestructive_FailurePropagates(t *testing.T) {
        resetDumpRunner(t)
        t.Setenv("MIGRATION_SNAPSHOT_DIR", t.TempDir())
        t.Setenv("PGDUMP_PATH", existingClientPath(t))
        dumpRunner = func(ctx context.Context, bin string, args []string) error {
                return errors.New("pg_dump: error: connection to server failed")
        }

        _, attempted, err := snapshotBeforeDestructive(context.Background(), "postgresql://u:p@h:5432/db", "00000000000036_demo.sql")
        if !attempted {
                t.Fatal("attempted = false, want true when a dump was attempted")
        }
        if err == nil || !strings.Contains(err.Error(), "pg_dump") {
                t.Fatalf("err = %v, want pg_dump failure wrapped", err)
        }
}

func TestSnapshotBeforeDestructive_NoClientIsNotAttempted(t *testing.T) {
        resetDumpRunner(t)
        t.Setenv("MIGRATION_SNAPSHOT_DIR", t.TempDir())
        // PGDUMP_PATH pointing at a missing file makes the lookup deterministic
        // regardless of the host PATH.
        t.Setenv("PGDUMP_PATH", "/nonexistent/pg_dump_path_for_test")
        called := false
        dumpRunner = func(ctx context.Context, bin string, args []string) error {
                called = true
                return nil
        }

        _, attempted, err := snapshotBeforeDestructive(context.Background(), "postgresql://u:p@h:5432/db", "00000000000036_demo.sql")
        if err != nil {
                t.Fatalf("no client must not error: %v", err)
        }
        if attempted || called {
                t.Fatalf("attempted = %v, called = %v; want nothing attempted without a client", attempted, called)
        }
}

func TestSnapshotBeforeDestructive_DisabledEscapeHatch(t *testing.T) {
        resetDumpRunner(t)
        t.Setenv("MIGRATION_SNAPSHOT_DIR", "off")
        t.Setenv("PGDUMP_PATH", "/opt/fake-pgdump")
        called := false
        dumpRunner = func(ctx context.Context, bin string, args []string) error {
                called = true
                return nil
        }

        _, attempted, err := snapshotBeforeDestructive(context.Background(), "postgresql://u:p@h:5432/db", "00000000000036_demo.sql")
        if err != nil || attempted || called {
                t.Fatalf("disabled: attempted=%v err=%v called=%v, want skipped silently", attempted, err, called)
        }
}

// TestSnapshotBeforeDestructive_FakeBinary is the end-to-end seam check:
// PGDUMP_PATH points at a shell script acting as pg_dump, proving the real
// runner executes the binary and the --file target materializes.
func TestSnapshotBeforeDestructive_FakeBinary(t *testing.T) {
        resetDumpRunner(t)
        // A NON-existent snapshot directory: MkdirAll must create it with
        // private 0700 permissions.
        snapDir := filepath.Join(t.TempDir(), "snapshots")
        t.Setenv("MIGRATION_SNAPSHOT_DIR", snapDir)

        fake := filepath.Join(t.TempDir(), "fake-pgdump")
        script := "#!/bin/sh\nout=\"\"\nfor a in \"$@\"; do case \"$a\" in --file=*) out=\"${a#--file=}\" ;; esac; done\nmkdir -p \"$(dirname \"$out\")\"; : > \"$out\"; echo dump-ok >> \"$out\"\n"
        if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
                t.Fatalf("write fake pg_dump: %v", err)
        }
        t.Setenv("PGDUMP_PATH", fake)

        path, attempted, err := snapshotBeforeDestructive(context.Background(), "postgresql://u:p@h:5432/db", "00000000000037_demo.sql")
        if err != nil {
                t.Fatalf("unexpected error: %v", err)
        }
        if !attempted {
                t.Fatal("attempted = false, want true")
        }
        b, err := os.ReadFile(path)
        if err != nil {
                t.Fatalf("snapshot file missing: %v", err)
        }
        if !strings.Contains(string(b), "dump-ok") {
                t.Fatalf("snapshot file content = %q, want fake dump output", b)
        }
        // The snapshot directory must have been created with private perms.
        info, err := os.Stat(snapDir)
        if err != nil {
                t.Fatalf("stat dir: %v", err)
        }
        if perm := info.Mode().Perm(); perm != 0o700 {
                t.Fatalf("dir perms = %v, want 0700", perm)
        }
}

func TestSnapshotProductionStrict(t *testing.T) {
        for env, want := range map[string]bool{
                "production":  true,
                "staging":     true,
                "development": false,
                "":            false,
        } {
                if got := snapshotProductionStrict(env); got != want {
                        t.Fatalf("snapshotProductionStrict(%q) = %v, want %v", env, got, want)
                }
        }
}

// TestSnapshotDumpDeadlinePropagates pins the safety budget: the dump
// inherits the caller's deadline, so a wedged pg_dump client can never
// outlive it. A short parent context proves propagation in milliseconds
// while the 2-minute const bound is asserted separately.
func TestSnapshotDumpDeadlinePropagates(t *testing.T) {
        if snapshotDumpTimeout != 2*time.Minute {
                t.Fatalf("snapshotDumpTimeout = %v, want 2m (the migration budget is only 5m total)", snapshotDumpTimeout)
        }
        resetDumpRunner(t)
        t.Setenv("MIGRATION_SNAPSHOT_DIR", t.TempDir())
        t.Setenv("PGDUMP_PATH", existingClientPath(t))
        dumpRunner = func(ctx context.Context, bin string, args []string) error {
                <-ctx.Done()
                return ctx.Err()
        }
        parent, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
        defer cancel()

        start := time.Now()
        _, attempted, err := snapshotBeforeDestructive(parent, "postgresql://u:p@h:5432/db", "00000000000038_demo.sql")
        elapsed := time.Since(start)
        if !attempted || err == nil {
                t.Fatalf("attempted=%v err=%v, want deadline-exceeded attempt with error", attempted, err)
        }
        if elapsed > 10*time.Second {
                t.Fatalf("dump ran %v — the deadline did not bound it", elapsed)
        }
}
