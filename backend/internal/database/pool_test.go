package database

// Unit locks for AUD T-C6 pool tuning: explicit DB_POOL_* overrides win
// over the parsed DSN, absent knobs leave the DSN untouched, and invalid
// values are logged-and-ignored (a typo must not take the API down at
// boot). The lookup function is injected so tests never touch the
// process environment.

import (
        "testing"
        "time"

        "github.com/jackc/pgx/v5/pgxpool"
)

func testPoolConfig() *pgxpool.Config {
        cfg, err := pgxpool.ParseConfig("postgresql://app:secret@db.internal:5432/pharmacy")
        if err != nil {
                panic(err) // fixed DSN in a test cannot fail to parse
        }
        cfg.MaxConns = 10 // as a pool_max_conns=10 DSN would carry
        cfg.MinConns = 0
        cfg.MaxConnLifetime = time.Hour
        cfg.MaxConnIdleTime = 30 * time.Minute
        return cfg
}

func TestPoolKnobsOverrideParsedDSN(t *testing.T) {
        cfg := testPoolConfig()
        env := map[string]string{
                "DB_POOL_MAX_CONNS":          "16",
                "DB_POOL_MIN_CONNS":          "2",
                "DB_POOL_MAX_CONN_LIFETIME":  "45m",
                "DB_POOL_MAX_CONN_IDLE_TIME": "5m",
        }
        tuning := ApplyPoolTuning(cfg, func(key string) string { return env[key] })

        if cfg.MaxConns != 16 || tuning.MaxConns != 16 || !tuning.MaxConnsOverridden {
                t.Fatalf("DB_POOL_MAX_CONNS override not applied: cfg=%d tuning=%d overridden=%v", cfg.MaxConns, tuning.MaxConns, tuning.MaxConnsOverridden)
        }
        if cfg.MinConns != 2 || tuning.MinConns != 2 {
                t.Fatalf("DB_POOL_MIN_CONNS override not applied: cfg=%d", cfg.MinConns)
        }
        if cfg.MaxConnLifetime != 45*time.Minute || tuning.MaxConnLifetime != 45*time.Minute {
                t.Fatalf("lifetime override not applied: cfg=%v", cfg.MaxConnLifetime)
        }
        if cfg.MaxConnIdleTime != 5*time.Minute || tuning.MaxConnIdleTime != 5*time.Minute {
                t.Fatalf("idle-time override not applied: cfg=%v", cfg.MaxConnIdleTime)
        }
}

func TestPoolAbsentKnobsKeepDSNValues(t *testing.T) {
        cfg := testPoolConfig()
        tuning := ApplyPoolTuning(cfg, func(string) string { return "" })

        if cfg.MaxConns != 10 || tuning.MaxConnsOverridden {
                t.Fatalf("absent DB_POOL_MAX_CONNS must leave the DSN value: cfg=%d overridden=%v", cfg.MaxConns, tuning.MaxConnsOverridden)
        }
        if cfg.MinConns != 0 || cfg.MaxConnLifetime != time.Hour || cfg.MaxConnIdleTime != 30*time.Minute {
                t.Fatalf("absent knobs must not mutate the parsed config: %+v", cfg)
        }
        if tuning != (PoolTuning{MaxConns: 10, MinConns: 0, MaxConnLifetime: time.Hour, MaxConnIdleTime: 30 * time.Minute}) {
                t.Fatalf("tuning snapshot must mirror the untouched config: %+v", tuning)
        }
}

func TestPoolInvalidValuesIgnoredNotFatal(t *testing.T) {
        cfg := testPoolConfig()
        env := map[string]string{
                "DB_POOL_MAX_CONNS":          "not-a-number",
                "DB_POOL_MIN_CONNS":          "-3",
                "DB_POOL_MAX_CONN_LIFETIME":  "yesterday",
                "DB_POOL_MAX_CONN_IDLE_TIME": "0s",
        }
        ApplyPoolTuning(cfg, func(key string) string { return env[key] })

        if cfg.MaxConns != 10 || cfg.MinConns != 0 || cfg.MaxConnLifetime != time.Hour || cfg.MaxConnIdleTime != 30*time.Minute {
                t.Fatalf("invalid knob values must be ignored, config must stand: max=%d min=%d life=%v idle=%v",
                        cfg.MaxConns, cfg.MinConns, cfg.MaxConnLifetime, cfg.MaxConnIdleTime)
        }
}

// AUD-013: the session budgets must ride the startup packet so PostgreSQL
// itself cancels a runaway statement/lock/idle-in-tx — a cancelled query
// frees the pool slot instead of pinning it.
func TestPoolSessionBudgetsRideStartupPacket(t *testing.T) {
        cfg := testPoolConfig()
        env := map[string]string{
                "DB_STMT_TIMEOUT_MS":    "30000",
                "DB_LOCK_TIMEOUT_MS":    "5000",
                "DB_IDLE_TX_TIMEOUT_MS": "60000",
        }
        tuning := ApplyPoolTuning(cfg, func(key string) string { return env[key] })

        for param, want := range map[string]string{
                "statement_timeout":                    "30000",
                "lock_timeout":                         "5000",
                "idle_in_transaction_session_timeout":  "60000",
        } {
                if got := cfg.ConnConfig.RuntimeParams[param]; got != want {
                        t.Fatalf("RuntimeParams[%s]=%q, want %q", param, got, want)
                }
        }
        if tuning.StmtTimeoutMs != "30000" || tuning.LockTimeoutMs != "5000" || tuning.IdleTxTimeoutMs != "60000" {
                t.Fatalf("tuning must mirror the effective budgets: %+v", tuning)
        }
}

func TestPoolSessionBudgetsAbsentOrInvalidStayUnset(t *testing.T) {
        cfg := testPoolConfig()
        env := map[string]string{
                "DB_STMT_TIMEOUT_MS":    "abc",
                "DB_LOCK_TIMEOUT_MS":    "-5",
                "DB_IDLE_TX_TIMEOUT_MS": "0",
        }
        tuning := ApplyPoolTuning(cfg, func(key string) string { return env[key] })

        for _, param := range []string{"statement_timeout", "lock_timeout", "idle_in_transaction_session_timeout"} {
                if v, ok := cfg.ConnConfig.RuntimeParams[param]; ok {
                        t.Fatalf("invalid budget leaked into RuntimeParams[%s]=%q", param, v)
                }
        }
        if tuning.StmtTimeoutMs != "" || tuning.LockTimeoutMs != "" || tuning.IdleTxTimeoutMs != "" {
                t.Fatalf("invalid budgets must not appear in tuning: %+v", tuning)
        }
}
