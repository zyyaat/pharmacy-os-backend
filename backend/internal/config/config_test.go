package config

// Regression lock for AUD T-A4 (MEDIUM, misconfiguration trap) —
// DATABASE_URL carries a local-development fallback so `go run` works with
// zero setup. That fallback used to swallow production misconfigurations:
// a typo'd or missing variable on the deploy target booted the binary
// against postgres@localhost:6543, producing an unreadable crash-loop
// instead of an actionable error — and the production "required" check in
// Validate was dead code because the default meant the field was never
// empty. The contract now:
//
//  1. development + built-in default  → boots (the fallback's whole purpose)
//  2. production + built-in default   → REFUSED with a message naming the fix
//  3. production + empty              → refused as before
//  4. production + any real DSN       → accepted
//  5. Load() still applies the fallback in development

import (
        "strings"
        "testing"
)

func productionConfig(databaseURL string) *Config {
        return &Config{
                Environment:              "production",
                DatabaseURL:              databaseURL,
                PublicAppURL:             "https://pharmacy-os.app",
                BootstrapSuperAdminEmail: "owner@example.com",
        }
}

func TestValidateRefusesDevDefaultDatabaseURLInProduction(t *testing.T) {
        t.Setenv("CORS_ORIGINS", "https://pharmacy-os.app")
        err := productionConfig(devDefaultDatabaseURL).Validate()
        if err == nil {
                t.Fatal("production boot with the built-in development DATABASE_URL was accepted — the misconfiguration trap (AUD T-A4) is back")
        }
        if !strings.Contains(err.Error(), "development default") || !strings.Contains(err.Error(), "DATABASE_URL") {
                t.Fatalf("refusal message does not name the problem and the fix: %v", err)
        }
}

func TestValidateAcceptsRealDatabaseURLInProduction(t *testing.T) {
        t.Setenv("CORS_ORIGINS", "https://pharmacy-os.app")
        err := productionConfig("postgresql://app:secret@db.internal:5432/pharmacy?pool_max_conns=10").Validate()
        if err != nil {
                t.Fatalf("production with a real DATABASE_URL must boot: %v", err)
        }
}

func TestValidateRefusesWildcardCorsInProduction(t *testing.T) {
        t.Setenv("CORS_ORIGINS", "*")
        err := productionConfig("postgresql://app:secret@db.internal:5432/pharmacy").Validate()
        if err == nil {
                t.Fatal("production with wildcard CORS_ORIGINS was accepted — the credential-reflection hole (AUD-018) is back")
        }
        if !strings.Contains(err.Error(), "CORS_ORIGINS") {
                t.Fatalf("refusal must name CORS_ORIGINS so the fix is obvious: %v", err)
        }
}

func TestValidateKeepsDevFallbackUsableInDevelopment(t *testing.T) {
        cfg := &Config{Environment: "development", DatabaseURL: devDefaultDatabaseURL}
        if err := cfg.Validate(); err != nil {
                t.Fatalf("development boot with the built-in default must stay frictionless: %v", err)
        }
}

func TestValidateStillRequiresDatabaseURL(t *testing.T) {
        t.Setenv("CORS_ORIGINS", "https://pharmacy-os.app")
        err := productionConfig("").Validate()
        if err == nil || !strings.Contains(err.Error(), "DATABASE_URL is required") {
                t.Fatalf("empty DATABASE_URL in production must be refused: %v", err)
        }
}

func TestLoadAppliesDevFallback(t *testing.T) {
        t.Setenv("DATABASE_URL", "")
        cfg := Load()
        if cfg.DatabaseURL != devDefaultDatabaseURL {
                t.Fatalf("Load must keep the development fallback: got %q", cfg.DatabaseURL)
        }
}
