package database

// Behavioral companion to TestMigrationRecordCommittedAtomicallyWithDDL —
// this one needs a real PostgreSQL (DDL is transactional in PostgreSQL, the
// property the atomic record relies on) and is skipped in unit CI.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func integrationDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — migration atomicity needs a real PostgreSQL")
	}
	return dsn
}

func TestRecordMigrationComposesWithTransaction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, integrationDSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.schema_migrations (version text PRIMARY KEY)`); err != nil {
		t.Fatalf("ensure ledger: %v", err)
	}
	const probeVersion = "99999999999999_atomicity_probe"
	const probeTable = "public.atomicity_probe_tbl"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+probeTable)
		_, _ = pool.Exec(context.Background(), `DELETE FROM public.schema_migrations WHERE version = $1`, probeVersion)
	})

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE `+probeTable+` (id int)`); err != nil {
		t.Fatalf("create probe table: %v", err)
	}
	if err := recordMigration(ctx, tx, probeVersion); err != nil {
		t.Fatalf("recordMigration through tx: %v", err)
	}

	// Simulate the crash window: the transaction never commits.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var tableExists bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass($1) IS NOT NULL`, probeTable).Scan(&tableExists); err != nil {
		t.Fatalf("probe table check: %v", err)
	}
	var recorded bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM public.schema_migrations WHERE version = $1)`, probeVersion).Scan(&recorded); err != nil {
		t.Fatalf("ledger check: %v", err)
	}
	if tableExists || recorded {
		t.Fatalf("rollback was not atomic: table_exists=%v recorded=%v — the DDL and the version row must share one fate", tableExists, recorded)
	}
}
