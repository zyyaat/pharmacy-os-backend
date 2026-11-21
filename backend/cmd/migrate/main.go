// Command migrate applies the embedded Pharmacy OS migration chain to a
// PostgreSQL database and exits. It is the boot-time half of cmd/server
// (database.RunMigrations) without the server: CI applies the schema before
// running the integration suites, and operators get a documented way to
// migrate ahead of a deploy instead of discovering the chain at boot.
//
// Usage:
//
//	DATABASE_URL=postgres://... go run ./cmd/migrate
//	go run ./cmd/migrate -url postgres://...
//
// The same destructive-migration gate as the server applies (AUD-007):
// a fresh database (no tables besides the ledger) runs the whole chain
// unattended, while an existing install refuses destructive migrations
// unless ALLOW_DESTRUCTIVE_MIGRATIONS allows them explicitly. A zero exit
// code means the schema is fully up to date.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pharmacy-os/backend/internal/database"
)

func main() {
	dsn := flag.String("url", "", "PostgreSQL connection string (defaults to $DATABASE_URL, then $TEST_DATABASE_URL)")
	flag.Parse()

	url := *dsn
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		url = os.Getenv("TEST_DATABASE_URL")
	}
	if url == "" {
		log.Fatal("migrate: no database URL — pass -url or set DATABASE_URL (or TEST_DATABASE_URL)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	poolConfig, err := pgxpool.ParseConfig(url)
	if err != nil {
		log.Fatalf("migrate: failed to parse database configuration: %v", err)
	}
	// Same surface as cmd/server: simple query execution works with pooled
	// providers (Supabase transaction pooler) and direct connections alike.
	poolConfig.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec

	db, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		log.Fatalf("migrate: failed to initialize database pool: %v", err)
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		log.Fatalf("migrate: failed to connect to database: %v", err)
	}

	if err := database.RunMigrations(ctx, db); err != nil {
		log.Fatalf("migrate: failed to run migrations: %v", err)
	}

	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM public.schema_migrations`).Scan(&count); err != nil {
		fmt.Println("migrate: schema up to date")
		return
	}
	fmt.Printf("migrate: schema up to date (%d migrations in ledger)\n", count)
}
