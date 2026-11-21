package database

// AUD T-C6 (MEDIUM, pool tuning) — the pool was built straight from the
// parsed DSN: pgx defaults (MaxConns = max(4, NumCPU)) or whatever the DSN
// happened to carry, invisible to the operator and untunable without
// editing the connection string. On the 1 vCPU / 1 GB deploy target the
// default is usually fine, but POS bursts + delta sync can saturate 4
// connections, and nobody could see or change that. The DB_POOL_* knobs
// apply explicit overrides over the parsed DSN and the effective values
// are logged at boot, so the pool stops being an invisible constant.

import (
        "log"
        "strconv"
        "strings"
        "time"

        "github.com/jackc/pgx/v5/pgxpool"
)

// PoolTuning is the effective pool configuration after the DB_POOL_*
// environment knobs were applied over the parsed DSN values.
type PoolTuning struct {
        MaxConns           int32
        MinConns           int32
        MaxConnLifetime    time.Duration
        MaxConnIdleTime    time.Duration
        MaxConnsOverridden bool // DB_POOL_MAX_CONNS won over the DSN/default

        // AUD-013: effective session budgets sent as startup-packet
        // RuntimeParams on every pooled connection (empty = not set).
        StmtTimeoutMs   string // statement_timeout
        LockTimeoutMs   string // lock_timeout
        IdleTxTimeoutMs string // idle_in_transaction_session_timeout
}

// ApplyPoolTuning mutates cfg in place with the optional DB_POOL_*
// overrides and returns the effective values for boot-time logging.
//
// Precedence: an explicitly set environment variable wins; when absent the
// DSN's own values (or pgx defaults) stand — the function never invents
// limits the operator did not ask for. Invalid values are logged and
// ignored, never fatal: a typo must not take the API down at boot.
//
//      DB_POOL_MAX_CONNS           — positive integer
//      DB_POOL_MIN_CONNS           — non-negative integer (warm connections)
//      DB_POOL_MAX_CONN_LIFETIME   — Go duration ("30m", "1h")
//      DB_POOL_MAX_CONN_IDLE_TIME  — Go duration ("5m")
func ApplyPoolTuning(cfg *pgxpool.Config, lookup func(string) string) PoolTuning {
        tuning := PoolTuning{
                MaxConns:        cfg.MaxConns,
                MinConns:        cfg.MinConns,
                MaxConnLifetime: cfg.MaxConnLifetime,
                MaxConnIdleTime: cfg.MaxConnIdleTime,
        }

        if v := strings.TrimSpace(lookup("DB_POOL_MAX_CONNS")); v != "" {
                if n, err := strconv.Atoi(v); err == nil && n > 0 {
                        cfg.MaxConns = int32(n)
                        tuning.MaxConns = cfg.MaxConns
                        tuning.MaxConnsOverridden = true
                } else {
                        log.Printf("[DB] ignoring invalid DB_POOL_MAX_CONNS=%q (want a positive integer)", v)
                }
        }

        if v := strings.TrimSpace(lookup("DB_POOL_MIN_CONNS")); v != "" {
                if n, err := strconv.Atoi(v); err == nil && n >= 0 {
                        cfg.MinConns = int32(n)
                        tuning.MinConns = cfg.MinConns
                } else {
                        log.Printf("[DB] ignoring invalid DB_POOL_MIN_CONNS=%q (want a non-negative integer)", v)
                }
        }

        if v := strings.TrimSpace(lookup("DB_POOL_MAX_CONN_LIFETIME")); v != "" {
                if d, err := time.ParseDuration(v); err == nil && d > 0 {
                        cfg.MaxConnLifetime = d
                        tuning.MaxConnLifetime = d
                } else {
                        log.Printf("[DB] ignoring invalid DB_POOL_MAX_CONN_LIFETIME=%q (want a Go duration like 30m)", v)
                }
        }

        if v := strings.TrimSpace(lookup("DB_POOL_MAX_CONN_IDLE_TIME")); v != "" {
                if d, err := time.ParseDuration(v); err == nil && d > 0 {
                        cfg.MaxConnIdleTime = d
                        tuning.MaxConnIdleTime = d
                } else {
                        log.Printf("[DB] ignoring invalid DB_POOL_MAX_CONN_IDLE_TIME=%q (want a Go duration like 5m)", v)
                }
        }

        // AUD-013 — server-side runaway budgets. pgx exposes no fields for
        // these, so they ride the startup packet as session RuntimeParams:
        // every connection in the pool (present and future) inherits them and
        // PostgreSQL enforces them server-side — a staged runaway query is
        // cancelled instead of pinning a pool slot on the 1 vCPU host. Absent
        // env vars leave the server defaults untouched (never invent limits
        // the operator did not ask for).
        for _, knob := range [...]struct{ env, param string }{
                {"DB_STMT_TIMEOUT_MS", "statement_timeout"},
                {"DB_LOCK_TIMEOUT_MS", "lock_timeout"},
                {"DB_IDLE_TX_TIMEOUT_MS", "idle_in_transaction_session_timeout"},
        } {
                if v := strings.TrimSpace(lookup(knob.env)); v != "" {
                        if n, err := strconv.Atoi(v); err == nil && n > 0 {
                                cfg.ConnConfig.RuntimeParams[knob.param] = v
                                switch knob.param {
                                case "statement_timeout":
                                        tuning.StmtTimeoutMs = v
                                case "lock_timeout":
                                        tuning.LockTimeoutMs = v
                                default:
                                        tuning.IdleTxTimeoutMs = v
                                }
                        } else {
                                log.Printf("[DB] ignoring invalid %s=%q (want a positive integer of milliseconds)", knob.env, v)
                        }
                }
        }

        return tuning
}

// LogPoolTuning prints the effective pool configuration at boot so the
// operator sees what the process actually runs with.
func LogPoolTuning(t PoolTuning) {
        suffix := ""
        if t.MaxConnsOverridden {
                suffix = " (DB_POOL_MAX_CONNS override)"
        }
        log.Printf("[DB] pool: max_conns=%d min_conns=%d max_conn_lifetime=%s max_conn_idle_time=%s%s",
                t.MaxConns, t.MinConns, t.MaxConnLifetime, t.MaxConnIdleTime, suffix)
        if t.StmtTimeoutMs != "" || t.LockTimeoutMs != "" || t.IdleTxTimeoutMs != "" {
                log.Printf("[DB] session budgets: statement_timeout=%s lock_timeout=%s idle_in_tx=%s (ms)",
                        orUnset(t.StmtTimeoutMs), orUnset(t.LockTimeoutMs), orUnset(t.IdleTxTimeoutMs))
        }
}

func orUnset(v string) string {
        if v == "" {
                return "unset"
        }
        return v
}
