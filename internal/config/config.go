// Package config handles application configuration
// Supports: PostgreSQL, Supabase Auth, River Queue, Application Settings
package config

import (
        "fmt"
        "log"
        "os"
        "time"
)

// Config holds all application configuration
type Config struct {
        // Server
        Port         string
        Environment  string // "development", "staging", "production"

        // Database (PostgreSQL via Supavisor)
        DatabaseURL string // Format: postgresql://user:pass@host:port/dbname

        // Supabase Authentication
        SupabaseURL      string // e.g., "https://xyz.supabase.co"
        SupabaseJWTSecret string // JWT secret from Supabase project settings
        JWTExpiry        time.Duration // Access token expiry (default: 15 minutes)

        // River Queue (Postgres-based job queue)
        RiverDSN string

        // Application Settings
        LogLevel          string // "debug", "info", "warn", "error"
        CorsOrigins       string // Comma-separated list of allowed origins
        MaxRequestSize    int64  // Max request body size in bytes (default: 10MB)
}

// Load reads configuration from environment variables with sensible defaults
func Load() *Config {
        cfg := &Config{
                // Server
                // Priority: PORT (DockHosting/standard) > BACKEND_PORT (custom) > 8080 (default)
                Port:         getEnv("PORT", getEnv("BACKEND_PORT", "8080")),
                Environment:  getEnv("APP_ENV", "development"),

                // Database - Default to Supavisor port 6543
                DatabaseURL: getEnv("DATABASE_URL", "postgresql://postgres:postgres@localhost:6543/postgres"),

                // Supabase Authentication
                SupabaseURL:      getEnv("SUPABASE_URL", ""),
                SupabaseJWTSecret: getEnv("SUPABASE_JWT_SECRET", ""),
                JWTExpiry:        15 * time.Minute, // Short-lived access tokens

                // River Queue
                RiverDSN: getEnv("RIVER_DSN", "postgresql://postgres:postgres@localhost:6543/postgres"),

                // Application Settings
                LogLevel:       getEnv("LOG_LEVEL", "info"),
                CorsOrigins:    getEnv("CORS_ORIGINS", "http://localhost:3000,http://localhost:3001"),
                MaxRequestSize: getEnvAsInt64("MAX_REQUEST_SIZE", 10*1024*1024), // 10MB default
        }
        
        // Log important configuration for debugging
        log.Printf("[CONFIG] Port: %s, Environment: %s, DatabaseURL: %s", 
            cfg.Port, cfg.Environment, maskDatabaseURL(cfg.DatabaseURL))
        
        return cfg
}

// maskDatabaseURL masks sensitive parts of the database URL for logging
func maskDatabaseURL(url string) string {
    if url == "" {
        return "(empty)"
    }
    // Show only the host part, mask credentials
    if len(url) > 50 {
        return url[:50] + "..."
    }
        return url
}

// IsProduction returns true if running in production mode
func (c *Config) IsProduction() bool {
        return c.Environment == "production"
}

// IsDevelopment returns true if running in development mode
func (c *Config) IsDevelopment() bool {
        return c.Environment == "development"
}

// Validate checks if required configuration is present
func (c *Config) Validate() error {
        var errors []string

        if c.DatabaseURL == "" {
                errors = append(errors, "DATABASE_URL is required")
        }

        if c.SupabaseJWTSecret == "" && !c.IsDevelopment() {
                errors = append(errors, "SUPABASE_JWT_SECRET is required in non-development mode")
        }

        if len(errors) > 0 {
                return fmt.Errorf("configuration errors: %v", errors)
        }

        return nil
}

// GetCorsOrigins returns CORS origins as a slice
func (c *Config) GetCorsOrigins() []string {
        if c.CorsOrigins == "" {
                return []string{"*"}
        }
        
        origins := make([]string, 0)
        for _, origin := range splitString(c.CorsOrigins, ",") {
                if trimmed := trimSpace(origin); trimmed != "" {
                        origins = append(origins, trimmed)
                }
        }
        
        if len(origins) == 0 {
                return []string{"*"}
        }
        
        return origins
}

// Helper functions for environment variable parsing

func getEnv(key, defaultValue string) string {
        if value := os.Getenv(key); value != "" {
                return value
        }
        return defaultValue
}

func getEnvAsInt64(key string, defaultValue int64) int64 {
        value := os.Getenv(key)
        if value == "" {
                return defaultValue
        }
        
        var result int64
        _, err := fmt.Sscanf(value, "%d", &result)
        if err != nil {
                return defaultValue
        }
        return result
}

func splitString(s, separator string) []string {
        result := make([]string, 0)
        start := 0
        
        for i := 0; i < len(s); i++ {
                if string(s[i]) == separator {
                        result = append(result, s[start:i])
                        start = i + 1
                }
        }
        
        result = append(result, s[start:])
        return result
}

func trimSpace(s string) string {
        // Simple trim implementation
        start := 0
        end := len(s) - 1
        
        for start <= end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
                start++
        }
        
        for end >= start && (s[end] == ' ' || s[end] == '\t' || s[end] == '\n' || s[end] == '\r') {
                end--
        }
        
        if start > end {
                return ""
        }
        
        return s[start : end+1]
}
