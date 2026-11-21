// Package config handles application configuration
// Supports: PostgreSQL, Go-managed authentication, River Queue, Application Settings
package config

import (
        "fmt"
        "log"
        urlpkg "net/url"
        "os"
        "strings"
        "time"
)

// Config holds all application configuration
type Config struct {
        // Server
        Port        string
        Environment string // "development", "staging", "production"

        // Database (PostgreSQL via Supavisor)
        DatabaseURL string // Format: postgresql://user:pass@host:port/dbname

        // Go-managed Authentication
        AuthAccessTTL  time.Duration
        AuthRefreshTTL time.Duration
        CookieSecure   bool
        CookieDomain   string

        // Transactional email — Brevo HTTP API by default; classic SMTP
        // (any provider) as the provider-independent escape hatch (Task 133).
        // Resolution (single source of truth: ResolvedMailTransport):
        // MAIL_TRANSPORT="brevo"|"smtp" wins, else SMTP as soon as
        // MAIL_SMTP_HOST is set, else Brevo.
        BrevoAPIKey   string
        MailFromEmail string
        MailFromName  string
        PublicAppURL  string
        // تسجيل الدخول بجوجل — معرف عميل الويب من Google Cloud Console.
        // فارغ = الميزة معطلة (/auth/google/config يجيب enabled=false
        // والعملاء يخفون الزر). الافتراضي المبني هو معرف النشر المعتمد.
        GoogleClientID string
        GoogleJWKSURL  string
        MailSMTPHost     string // relay host, e.g. smtp.gmail.com (empty → Brevo)
        MailSMTPPort     string // "587" STARTTLS or "465" implicit TLS; "" → 587
        MailSMTPUsername string // "" → no AUTH (internal/open relays)
        MailSMTPPassword string
        MailTransport    string // optional override: "brevo" or "smtp"

        // Task 148 — outbound email fair-use budget: per-recipient hourly /
        // daily caps plus a platform-wide daily ceiling protecting the
        // provider quota. Non-positive values fall back to code defaults
        // (internal/auth mailbudget.go) — a bad env var cannot disable the wall.
        MailSendHourlyLimit  int
        MailSendDailyLimit   int
        MailGlobalDailyLimit int

        // River Queue (Postgres-based job queue)
        RiverDSN string

        // Application Settings
        LogLevel       string // "debug", "info", "warn", "error"
        CorsOrigins    string // Comma-separated list of allowed origins
        MaxRequestSize int64  // Max request body size in bytes (default: 10MB)

        // AUD-004 — per-IP rate limiting for the unauthenticated auth family
        AuthRateLimitRPM   int
        AuthRateLimitBurst int
        // R-2 (api_level 106) — per-principal budgets on the authenticated
        // API groups (dashboard/platform-admin/pharmacy/companies) and
        // per-IP loose budgets on the signed payment webhooks. Same
        // malformed-value rule as AUTH_RATE_LIMIT_*: a typo falls back to
        // the default — it can never disable the wall.
        APIRateLimitRPM       int
        APIRateLimitBurst     int
        WebhookRateLimitRPM   int
        WebhookRateLimitBurst int
        // AUD-002/AUD-017 — CIDR list of reverse proxies allowed to set
        // X-Forwarded-For (ClientIP and rate-limit keys derive from it)
        TrustedProxies string

        // R-9 (api_level 103) — bearer token guarding /metrics. Empty keeps
        // the endpoint 404 (fail-closed, feature off); a value makes the
        // exposition scrapeable with `Authorization: Bearer <value>`.
        MetricsToken string

        // Task 151 — WebAuthn passkeys: the Relying Party identity. Defaults
        // derive from PUBLIC_APP_URL (RPID = its host, origin = the URL
        // itself); empty RPID keeps the whole family disabled (fail-closed).
        PasskeyRPID     string
        PasskeyRPName   string
        PasskeyRPOrigin string
        // Task 160 (M3) — comma-separated OPAQUE origins for native clients
        // (Android Credential Manager conveys "android:apk-key-hash:<b64url
        // sha256 of the signing cert>" instead of an https origin). Matched
        // by exact string comparison per go-webauthn; empty keeps web-only
        // (the pre-M3 behavior, fail-closed).
        PasskeyRPOpaqueOrigins string

        // One-time production bootstrap for the initial platform administrator.
        BootstrapSuperAdminEmail     string
        BootstrapSuperAdminPassword  string
        BootstrapSuperAdminFirstName string
        BootstrapSuperAdminLastName  string
        BootstrapSuperAdminCompany   string

        // Paymob payments (Phase G). All optional: when the required subset is
        // absent the online checkout endpoint answers paymob_not_configured and
        // the super-admin manual payment remains the billing path. Secrets live
        // only in the hosting environment — never in Vercel or NEXT_PUBLIC_*.
        PaymobSecretKey            string // dashboard "Secret Key" — server-only (Authorization: Token …)
        PaymobPublicKey           string // pk_... embedded in the Unified Checkout iframe URL
        PaymobCardIntegrationID   string // numeric Online Card integration ID
        PaymobWalletIntegrationID string // numeric Mobile Wallets integration ID (optional)
        PaymobHMACSecret          string // HMAC-SHA512 webhook verification secret
        PaymobWebhookToken        string // random token appended to the webhook URL (?token=…)
        PaymobBaseURL             string // default https://accept.paymob.com

        // XPay payments (Phase X) — the replacement gateway. Same philosophy:
        // optional, webhook-activated, secrets server-only. When the required
        // subset is present XPay takes checkout priority over Paymob (unset
        // XPAY_* to fall back — instant rollback). The webhook secret is the
        // endpoint's whsec_* shown once when the endpoint is created.
        XPAYSecretKey      string // sk_test_* / sk_live_* — server-only (Authorization: Bearer …)
        XPAYPublishableKey string // pk_test_* / pk_live_* — loaded by the browser SDK
        XPAYWebhookSecret  string // whsec_* — HMAC-SHA256 webhook verification secret
        XPAYWebhookToken   string // random token appended to the webhook URL (?token=…)
        XPAYBaseURL        string // default https://api.xpay.app
}

// devDefaultDatabaseURL is the local-development fallback for DATABASE_URL.
// It is deliberately refused in production (AUD T-A4): a misnamed or missing
// variable used to fall back to it silently and crash-loop against localhost
// instead of failing with an actionable error at boot.
const devDefaultDatabaseURL = "postgresql://postgres:postgres@localhost:6543/postgres"

// Load reads configuration from environment variables with sensible defaults
func Load() *Config {
        databaseURL := getEnv("DATABASE_URL", devDefaultDatabaseURL)
        cfg := &Config{
                // Server
                // Priority: PORT (DockHosting/standard) > BACKEND_PORT (custom) > 8080 (default)
                Port:        getEnv("PORT", getEnv("BACKEND_PORT", "8080")),
                Environment: getEnv("APP_ENV", "development"),

                // Database - Default to Supavisor port 6543
                DatabaseURL: databaseURL,

                // Go-managed Authentication
                AuthAccessTTL:  15 * time.Minute,
                AuthRefreshTTL: 30 * 24 * time.Hour,
                CookieSecure:   getEnv("APP_ENV", "development") == "production" || getEnvBool("AUTH_COOKIE_SECURE", false),
                CookieDomain:   getEnv("AUTH_COOKIE_DOMAIN", ""),

                // Transactional email
                BrevoAPIKey:   getEnv("BREVO_API_KEY", ""),
                MailFromEmail: getEnv("MAIL_FROM_EMAIL", ""),
                MailFromName:  getEnv("MAIL_FROM_NAME", "Pharmacy OS"),
                PublicAppURL:  getEnv("PUBLIC_APP_URL", ""),
                // تسجيل الدخول بجوجل: GOOGLE_CLIENT_ID بلا قيمة يعطل الميزة،
                // والافتراضي المبني هو معرف عميل الويب المعتمد للنشر. نقطة
                // JWKS افتراضية لجوجل وتُتجاوز في اختبارات الوحدة فقط.
                GoogleClientID: getEnv("GOOGLE_CLIENT_ID",
                        "605456710239-5a2guaql8tj9se2ni1gb2oa75ervfmnp.apps.googleusercontent.com"),
                GoogleJWKSURL: getEnv("GOOGLE_JWKS_URL", "https://www.googleapis.com/oauth2/v3/certs"),
                // Task 133 — SMTP escape hatch: filling MAIL_SMTP_HOST moves
                // the whole transactional pipeline off Brevo onto any SMTP
                // provider without a code change. MAIL_FROM_EMAIL stays the
                // sender identity under both transports.
                MailSMTPHost:     strings.TrimSpace(getEnv("MAIL_SMTP_HOST", "")),
                MailSMTPPort:     strings.TrimSpace(getEnv("MAIL_SMTP_PORT", "")),
                MailSMTPUsername: strings.TrimSpace(getEnv("MAIL_SMTP_USERNAME", "")),
                MailSMTPPassword: getEnv("MAIL_SMTP_PASSWORD", ""),
                MailTransport:    strings.ToLower(strings.TrimSpace(getEnv("MAIL_TRANSPORT", ""))),

                // Task 148 — email fair-use budget knobs (internal/auth/mailbudget.go).
                MailSendHourlyLimit:  getEnvAsInt("MAIL_SEND_HOURLY_LIMIT", 4),
                MailSendDailyLimit:   getEnvAsInt("MAIL_SEND_DAILY_LIMIT", 8),
                MailGlobalDailyLimit: getEnvAsInt("MAIL_GLOBAL_DAILY_LIMIT", 280),

                // River Queue
                RiverDSN: getEnv("RIVER_DSN", databaseURL),

                // Application Settings
                LogLevel:       getEnv("LOG_LEVEL", "info"),
                CorsOrigins:    getEnv("CORS_ORIGINS", "http://localhost:3000,http://localhost:3001"),
                MaxRequestSize: getEnvAsInt64("MAX_REQUEST_SIZE", 10*1024*1024), // 10MB default

                // AUD-004 — per-IP rate limiting for the unauthenticated auth
                // family (register/login/refresh/forgot/reset/verify/resend).
                // Generous by default (flood shield) because all web traffic
                // shares Vercel's proxy egress IPs — see internal/ratelimit.
                AuthRateLimitRPM:   getEnvAsInt("AUTH_RATE_LIMIT_RPM", 120),
                AuthRateLimitBurst: getEnvAsInt("AUTH_RATE_LIMIT_BURST", 30),

                // R-2 (api_level 106) — authenticated-surface shields. The
                // per-principal default (300/min, burst 100) is ~10x any real
                // user's sustained cadence (React Query dedupes and caches
                // since P4; the notification bell polls twice a minute) while
                // still hopeless for a scripted abuser hammering the heavy
                // lists. The webhook default (600/min per IP, burst 120)
                // rides above any genuine gateway's retry cadence — and a 429
                // there is safe by contract: both providers retry with
                // backoff and every webhook handler is idempotent (dedup by
                // idempotency keys, AUD-010/032), so no payment event is lost.
                APIRateLimitRPM:       getEnvAsInt("API_RATE_LIMIT_RPM", 300),
                APIRateLimitBurst:     getEnvAsInt("API_RATE_LIMIT_BURST", 100),
                WebhookRateLimitRPM:   getEnvAsInt("WEBHOOK_RATE_LIMIT_RPM", 600),
                WebhookRateLimitBurst: getEnvAsInt("WEBHOOK_RATE_LIMIT_BURST", 120),
                // AUD-002/AUD-017 — reverse proxies allowed to set
                // X-Forwarded-For (ClientIP + rate-limit keys depend on this).
                // Defaults cover private networks; override when the proxy
                // lives elsewhere. "*" is the documented trust-all escape
                // hatch for PaaS edges with unknowable shared egress (loud
                // warning logged at boot); an empty value disables header
                // trust entirely (fail-closed). Anything else must parse as
                // IP/CIDR or boot fails loudly.
                TrustedProxies: getEnv("TRUSTED_PROXIES", "127.0.0.1/8,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"),

                // R-9 (api_level 103) — /metrics bearer token. Empty = the
                // endpoint stays 404 (same fail-closed posture as
                // /debug/schema); set METRICS_TOKEN to expose the exposition
                // to a scraper carrying the token.
                MetricsToken: getEnv("METRICS_TOKEN", ""),

                // Task 151 — WebAuthn passkeys. RPID = the PUBLIC_APP_URL host
                // (scheme/port stripped — WebAuthn RPID is a registrable
                // domain); origin = PUBLIC_APP_URL verbatim. Explicit
                // PASSKEY_RP_* env vars win for non-standard setups.
                PasskeyRPID:     derivePasskeyRPID(getEnv("PUBLIC_APP_URL", "")),
                PasskeyRPName:   getEnv("PASSKEY_RP_NAME", "Pharmacy OS"),
                PasskeyRPOrigin: getEnv("PASSKEY_RP_ORIGIN", getEnv("PUBLIC_APP_URL", "")),
                // Task 160 (M3) — native-client opaque origins (comma list);
                // see the struct comment. Unset ⇒ web origins only.
                PasskeyRPOpaqueOrigins: getEnv("PASSKEY_RP_OPAQUE_ORIGINS", ""),

                BootstrapSuperAdminEmail:     getEnv("BOOTSTRAP_SUPER_ADMIN_EMAIL", ""),
                BootstrapSuperAdminPassword:  getEnv("BOOTSTRAP_SUPER_ADMIN_PASSWORD", ""),
                BootstrapSuperAdminFirstName: getEnv("BOOTSTRAP_SUPER_ADMIN_FIRST_NAME", "Mohamed"),
                BootstrapSuperAdminLastName:  getEnv("BOOTSTRAP_SUPER_ADMIN_LAST_NAME", "Admin"),
                BootstrapSuperAdminCompany:   getEnv("BOOTSTRAP_SUPER_ADMIN_COMPANY", "Pharmacy OS"),

                // Paymob payments (Phase G)
                // Dashboard field literally named "Secret Key". Falls back to the
                // legacy PAYMOB_API_KEY name so pre-rename deployments keep working.
                PaymobSecretKey: strings.TrimSpace(getEnv("PAYMOB_SECRET_KEY", getEnv("PAYMOB_API_KEY", ""))),
                PaymobPublicKey:           strings.TrimSpace(getEnv("PAYMOB_PUBLIC_KEY", "")),
                PaymobCardIntegrationID:   strings.TrimSpace(getEnv("PAYMOB_CARD_INTEGRATION_ID", "")),
                PaymobWalletIntegrationID: strings.TrimSpace(getEnv("PAYMOB_WALLET_INTEGRATION_ID", "")),
                PaymobHMACSecret:          strings.TrimSpace(getEnv("PAYMOB_HMAC_SECRET", "")),
                PaymobWebhookToken:        strings.TrimSpace(getEnv("PAYMOB_WEBHOOK_TOKEN", "")),
                PaymobBaseURL:             strings.TrimRight(getEnv("PAYMOB_BASE_URL", "https://accept.paymob.com"), "/"),

                // XPay payments (Phase X) — the replacement gateway
                XPAYSecretKey:      strings.TrimSpace(getEnv("XPAY_SECRET_KEY", "")),
                XPAYPublishableKey: strings.TrimSpace(getEnv("XPAY_PUBLISHABLE_KEY", "")),
                XPAYWebhookSecret:  strings.TrimSpace(getEnv("XPAY_WEBHOOK_SECRET", "")),
                XPAYWebhookToken:   strings.TrimSpace(getEnv("XPAY_WEBHOOK_TOKEN", "")),
                XPAYBaseURL:        strings.TrimRight(getEnv("XPAY_BASE_URL", "https://api.xpay.app"), "/"),
        }

        // Log important configuration for debugging
        log.Printf("[CONFIG] Port: %s, Environment: %s, DatabaseURL: %s",
                cfg.Port, cfg.Environment, maskDatabaseURL(cfg.DatabaseURL))

        return cfg
}

// ResolvedMailTransport is the single source of truth for which outbound
// transport the transactional email uses (Task 133): an explicit
// MAIL_TRANSPORT ("smtp"|"brevo") wins, otherwise SMTP as soon as a relay
// host is configured (the operator's escape hatch from a broken Brevo
// account), otherwise Brevo. Every mail sender — auth verification/reset
// and support notifications — resolves through this one function so the
// two paths can never disagree.
func (c *Config) ResolvedMailTransport() string {
        override := strings.ToLower(strings.TrimSpace(c.MailTransport))
        switch override {
        case "smtp", "brevo":
                return override
        }
        if c.MailSMTPHost != "" {
                return "smtp"
        }
        return "brevo"
}

// maskDatabaseURL masks sensitive parts of the database URL for logging
func maskDatabaseURL(url string) string {
        if url == "" {
                return "(empty)"
        }

        parsed, err := urlpkg.Parse(url)
        if err != nil || parsed.User == nil {
                return "[redacted database URL]"
        }

        parsed.User = urlpkg.UserPassword(parsed.User.Username(), "***")
        return parsed.String()
}

// IsProduction returns true if running in production mode
func (c *Config) IsProduction() bool {
        return c.Environment == "production"
}

// PaymobEnabled reports whether the online checkout has its required
// credential subset: secret key (server auth), public key (iframe URL), one
// payment channel integration ID, and the webhook HMAC secret. Missing any
// of them keeps billing on the manual-payment path.
func (c *Config) PaymobEnabled() bool {
        return c.PaymobSecretKey != "" &&
                c.PaymobPublicKey != "" &&
                c.PaymobCardIntegrationID != "" &&
                c.PaymobHMACSecret != ""
}

// XPayEnabled reports whether the XPay gateway has its required credential
// subset: secret key (server session creation), publishable key (browser
// SDK), and the webhook endpoint signing secret (whsec_*) — the only path
// that activates a subscription.
func (c *Config) XPayEnabled() bool {
        return c.XPAYSecretKey != "" &&
                c.XPAYPublishableKey != "" &&
                c.XPAYWebhookSecret != ""
}

// ActiveGateway resolves which gateway NEW checkout sessions go to:
// "xpay" when configured (the replacement gateway), falling back to
// "paymob" (Phase G), then "" (manual payments only). Pending payments
// keep resolving through their OWN provider's webhook regardless of this
// choice, so switching gateways never strands in-flight payments.
func (c *Config) ActiveGateway() string {
        if c.XPayEnabled() {
                return "xpay"
        }
        if c.PaymobEnabled() {
                return "paymob"
        }
        return ""
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
        } else if c.IsProduction() && c.DatabaseURL == devDefaultDatabaseURL {
                // AUD T-A4 — the development default is never a legitimate
                // production value: refusing it loudly beats a confusing
                // crash-loop against localhost:6543 on the deploy target.
                errors = append(errors,
                        "DATABASE_URL is the built-in development default (postgres@localhost:6543) — refusing to boot production against it; set the real DATABASE_URL")
        }
        if c.IsProduction() {
                if strings.TrimSpace(os.Getenv("CORS_ORIGINS")) == "" {
                        errors = append(errors, "CORS_ORIGINS is required in production")
                } else {
                        // AUD-018 — the middleware reflects the request origin together
                        // with Allow-Credentials, so a wildcard entry in production means
                        // any site can call the API with the caller's cookies. Refuse the
                        // boot instead of trusting the operator to notice.
                        for _, entry := range splitString(os.Getenv("CORS_ORIGINS"), ",") {
                                if strings.TrimSpace(entry) == "*" {
                                        errors = append(errors,
                                                `CORS_ORIGINS must not contain "*" in production — list the exact frontend origins instead`)
                                        break
                                }
                        }
                }
                if strings.TrimSpace(c.PublicAppURL) == "" {
                        errors = append(errors, "PUBLIC_APP_URL is required in production")
                }
                if strings.TrimSpace(c.BootstrapSuperAdminEmail) == "" {
                        errors = append(errors, "BOOTSTRAP_SUPER_ADMIN_EMAIL is required in production")
                }
        }

        if len(errors) > 0 {
                return fmt.Errorf("configuration errors: %v", errors)
        }

        return nil
}

func getEnvBool(key string, defaultValue bool) bool {
        value := os.Getenv(key)
        if value == "" {
                return defaultValue
        }
        return value == "1" || value == "true" || value == "TRUE" || value == "yes"
}

// GetCorsOrigins returns CORS origins as a slice
func (c *Config) GetCorsOrigins() []string {
        if c.CorsOrigins == "" {
                return []string{"*"}
        }

        origins := make([]string, 0)
        for _, origin := range splitString(c.CorsOrigins, ",") {
                if trimmed := strings.TrimRight(trimSpace(origin), "/"); trimmed != "" {
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

// getEnvAsInt parses an integer env var with a fallback default. Used by the
// rate-limit knobs (AUD-004); a malformed value silently keeps the default
// so a typo can never disable a security wall.
func getEnvAsInt(key string, defaultValue int) int {
        value := os.Getenv(key)
        if value == "" {
                return defaultValue
        }
        var result int
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

// derivePasskeyRPID extracts the WebAuthn Relying Party ID from the public
// app URL: the bare hostname, scheme and port stripped (RPID is a
// registrable domain). A scheme-less bare host is accepted as-is. Empty,
// whitespace, unparsable or host-less input returns "" which keeps the
// whole passkey family disabled — fail-closed by construction (Task 151).
func derivePasskeyRPID(appURL string) string {
        appURL = strings.TrimSpace(appURL)
        if appURL == "" {
                return ""
        }
        if parsed, err := urlpkg.Parse(appURL); err == nil {
                if host := parsed.Hostname(); host != "" {
                        return host
                }
        }
        // Bare registrable domain ("example.com", "localhost") — valid RPID
        // as-is. Must LOOK like a hostname: hostname characters only, so
        // garbage (":::not-a-url") stays rejected.
        if !strings.Contains(appURL, "://") && isHostnameLike(appURL) {
                return appURL
        }
        return ""
}

// isHostnameLike accepts only [A-Za-z0-9.-] with sane boundaries — enough
// discipline to reject URLs/garbage while keeping bare domains working.
func isHostnameLike(value string) bool {
        if value == "" || len(value) > 253 {
                return false
        }
        if value[0] == '-' || value[0] == '.' || value[len(value)-1] == '-' || value[len(value)-1] == '.' {
                return false
        }
        for _, char := range value {
                switch {
                case char >= 'a' && char <= 'z':
                case char >= 'A' && char <= 'Z':
                case char >= '0' && char <= '9':
                case char == '.' || char == '-':
                default:
                        return false
                }
        }
        return true
}
