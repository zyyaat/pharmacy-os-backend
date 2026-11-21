// Package main is the entry point for Pharmacy OS backend
package main

import (
        "context"
        "errors"
        "log"
        "net/http"
        "os"
        "os/signal"
        "syscall"
        "time"

        "github.com/jackc/pgx/v5"
        "github.com/jackc/pgx/v5/pgxpool"
        "github.com/pharmacy-os/backend/internal/auth"
        "github.com/pharmacy-os/backend/internal/config"
        "github.com/pharmacy-os/backend/internal/database"
        "github.com/pharmacy-os/backend/internal/handlers"
        "github.com/pharmacy-os/backend/internal/jobs"
)

func main() {
        // Load configuration
        cfg := config.Load()
        if err := cfg.Validate(); err != nil {
                log.Fatalf("Invalid configuration: %v", err)
        }

        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        poolConfig, err := pgxpool.ParseConfig(cfg.DatabaseURL)
        if err != nil {
                log.Fatalf("Failed to parse database configuration: %v", err)
        }
        // Supabase's transaction pooler does not keep prepared statements between
        // connections. Simple query execution works with both pooled and direct
        // PostgreSQL URLs, so use it consistently across hosting providers.
        poolConfig.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
        // AUD T-C6 — explicit, visible pool tuning: DB_POOL_* knobs win over
        // the DSN, everything else stands, and the effective values print at
        // boot instead of hiding in pgx defaults.
        tuning := database.ApplyPoolTuning(poolConfig, os.Getenv)
        database.LogPoolTuning(tuning)
        db, err := pgxpool.NewWithConfig(ctx, poolConfig)
        if err != nil {
                log.Fatalf("Failed to initialize database pool: %v", err)
        }
        defer db.Close()
        if err := db.Ping(ctx); err != nil {
                log.Fatalf("Failed to connect to database: %v", err)
        }

        // Apply database migrations before anything else touches the schema.
        // The SQL files are embedded in the binary (backend/migrations), so a
        // single self-contained deployment can bootstrap a fresh database on
        // any hosting provider. Safe to run on every startup.
        migrationCtx, cancelMigrations := context.WithTimeout(context.Background(), 5*time.Minute)
        defer cancelMigrations()
        if err := database.RunMigrations(migrationCtx, db); err != nil {
                log.Fatalf("Failed to run database migrations: %v", err)
        }

        // Task 53: self-heal the pharmacy email from its registration source
        // (accounts.contact_email) on every startup. Old edit flows wiped
        // pharmacies.email, hiding the email the owner entered at registration
        // from the branch card and the edit form. Idempotent; see
        // database.HealPharmacyRegistrationEmail for the full story.
        if err := database.HealPharmacyRegistrationEmail(migrationCtx, db); err != nil {
                log.Printf("Warning: pharmacy email self-heal skipped: %v", err)
        }

        // R-9 (api_level 103) — best-effort pg_stat_statements setup. The
        // HTTP metrics surface is code-side and always on (gated by
        // METRICS_TOKEN); the DB-side statement timing can only exist if
        // the host preloads the library — this logs the actionable warning
        // once per boot when it does not. Never blocks or fails boot.
        database.TryEnablePgStatStatements(migrationCtx, db)

        // Bootstrap whenever super-admin credentials are configured: mandatory
        // in production, honored in any environment so test harnesses can
        // exercise the platform realm without faking APP_ENV=production
        // (which would force Secure cookies over plain HTTP).
        if cfg.IsProduction() || cfg.BootstrapSuperAdminEmail != "" {
                // The shared startup context may already be spent after a long cold
                // migration run against a fresh database, so the super admin bootstrap
                // gets its own time budget instead of reusing the ping context.
                bootstrapCtx, cancelBootstrap := context.WithTimeout(context.Background(), 2*time.Minute)
                defer cancelBootstrap()
                bootstrap := auth.NewService(db, auth.Config{})
                if err := bootstrap.BootstrapSuperAdmin(
                        bootstrapCtx,
                        cfg.BootstrapSuperAdminEmail,
                        cfg.BootstrapSuperAdminPassword,
                        cfg.BootstrapSuperAdminFirstName,
                        cfg.BootstrapSuperAdminLastName,
                        cfg.BootstrapSuperAdminCompany,
                ); err != nil {
                        log.Fatalf("Failed to bootstrap super admin: %v", err)
                }
                log.Printf("Super admin bootstrap completed or was already satisfied")
        }

        // Initialize handlers
        h := handlers.New(cfg, db)

        // api_level 118 — «الاستيراد في الخلفية»: the River daemon runs inside
        // this process, pulling the imports/mail queues against the same
        // pool. Nil-safe at every layer: the kill switch (JOBS_ENABLED=0) or
        // a construction failure leaves jobs off and the API fully alive —
        // the async import endpoint answers 503 instead of queueing into a
        // client nothing would serve. Stopped with its own budget AFTER the
        // HTTP drain so a mid-flight import re-queues instead of dying.
        jobsCtx, stopJobs := context.WithCancel(context.Background())
        defer stopJobs()
        if err := h.AttachBackgroundJobs(jobsCtx, db); err != nil {
                log.Printf("[JOBS] background processing disabled: %v", err)
        }

        // M1/G3 (api_level 98) — auth artifact janitor: expired WebAuthn
        // challenges (each carries a SessionData JSON blob) and dead step-up
        // grants are reaped every 10 minutes, first sweep at boot so a fresh
        // deploy immediately reaps the accumulated debt. Hygiene only — a
        // failed sweep logs and the loop keeps ticking. Cancelled with the
        // process (defers do not run on log.Fatalf paths; process death
        // reclaims the goroutine either way).
        janitorCtx, stopJanitor := context.WithCancel(context.Background())
        defer stopJanitor()
        go h.StartAuthJanitor(janitorCtx, auth.DefaultJanitorInterval)

        // Start server — AUD-002: a hardened http.Server (timeouts, header
        // bound, body cap, trusted proxies) built by NewServer, watched by a
        // signal handler so SIGTERM/SIGINT (platform stops, deploys, Ctrl-C)
        // DRAIN in-flight requests instead of guillotining them. The drain
        // budget (15s) covers slow responses; the WebSocket hub's hijacked
        // connections are released by Shutdown as clients disconnect.
        srv := h.NewServer(":" + cfg.Port)
        serveErr := make(chan error, 1)
        go func() {
                log.Printf("Starting server on port %s", cfg.Port)
                if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
                        serveErr <- err
                }
                close(serveErr)
        }()

        quit := make(chan os.Signal, 1)
        signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
        select {
        case err := <-serveErr:
                if err != nil {
                        log.Fatalf("Failed to start server: %v", err)
                }
        case sig := <-quit:
                log.Printf("%s received — draining in-flight requests (15s budget)", sig)
                drainCtx, cancelDrain := context.WithTimeout(context.Background(), 15*time.Second)
                defer cancelDrain()
                if err := srv.Shutdown(drainCtx); err != nil {
                        log.Printf("Graceful shutdown incomplete (forcing exit): %v", err)
                } else {
                        log.Printf("Graceful shutdown complete")
                }
        }

        // api_level 118 — wind the job daemon down with its own budget after
        // the HTTP drain: an in-flight import returns to the queue and the
        // next boot picks it up exactly where it stopped.
        jobs.Stop(h.JobsClient(), 20*time.Second)
}
