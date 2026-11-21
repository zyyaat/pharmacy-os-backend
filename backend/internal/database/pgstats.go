package database

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TryEnablePgStatStatements is R-9's database half (api_level 103): the
// audit requires pg_stat_statements for any informed capacity decision,
// but the extension can only load if the DB host already lists it in
// shared_preload_libraries — a server-level setting the application can
// never set itself. So the boot attempt is honest self-setup: create the
// extension when the host allows it (the common managed-PG case: the
// library is preloaded but the extension not yet created), and log a
// single actionable warning when the host forbids it. Either way boot
// continues — observability must never be load-bearing for liveness.
func TryEnablePgStatStatements(ctx context.Context, pool *pgxpool.Pool) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`); err != nil {
		log.Printf("[OBSERVABILITY] pg_stat_statements NOT enabled: %v — ask the DB host to add it to shared_preload_libraries (top-statement timing stays blind until then)", err)
		return
	}
	log.Printf("[OBSERVABILITY] pg_stat_statements enabled — pg_stat_statements view is available for top-query analysis")
}
