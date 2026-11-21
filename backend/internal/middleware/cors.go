package middleware

import (
        "log"
        "net/http"
        "strconv"
        "strings"

        "github.com/gin-gonic/gin"
)

// CORS returns a middleware that handles CORS.
//
// Allowed origins are configured via CORS_ORIGINS (comma separated). In
// addition to exact matches, entries support a single leading wildcard for
// subdomains, e.g. "*.vercel.app" — required because every Vercel preview
// deployment gets its own randomly generated subdomain.
//
// Rejected origins are logged with the reason and the current allow-list so
// misconfiguration shows up directly in the hosting provider logs:
//
//      [CORS] origin not allowed: https://foo.vercel.app (allowed: [...])
func CORS(allowedOrigins ...string) gin.HandlerFunc {
        for _, o := range allowedOrigins {
                if strings.TrimSpace(o) == "*" {
                        // AUD-018 — this middleware reflects the request origin together
                        // with Allow-Credentials, so "*" means "any site can call the API
                        // with the caller's cookies". config.Validate refuses it in
                        // production; the warning keeps development use honest.
                        log.Printf("[CORS] WARNING: allow-list contains \"*\" — any origin will be reflected WITH credentials; development only")
                }
        }
        return func(c *gin.Context) {
                origin := c.GetHeader("Origin")
                allowed := originAllowed(origin, allowedOrigins)

                // No Origin header means the call is same-origin or a server/curl
                // client: CORS is irrelevant, always allow through.
                if origin == "" {
                        allowed = true
                }

                if allowed {
                        if origin != "" {
                                c.Header("Access-Control-Allow-Origin", origin)
                                c.Header("Access-Control-Allow-Credentials", "true")
                                c.Header("Vary", "Origin")
                        }
                } else {
                        log.Printf("[CORS] origin not allowed: %s (allowed: %s) - configure CORS_ORIGINS on the backend",
                                origin, strings.Join(allowedOrigins, ", "))
                }

                c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
                c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Accept, Authorization, X-CSRF-Token, X-Request-ID")
                c.Header("Access-Control-Max-Age", "86400")

                if c.Request.Method == "OPTIONS" {
                        c.AbortWithStatus(http.StatusNoContent)
                        return
                }

                c.Next()
        }
}

// originAllowed matches an origin against the allow-list. AUD-018: matching
// is now exact scheme+host — the "http://" twin of an https entry is a
// DIFFERENT origin and no longer slips through, and a port only matches its
// scheme's default (:443/:80) or an explicitly listed one. An entry may be:
//   - an exact origin:      https://pharmacy-app.vercel.app — scheme, host
//     and port must all match (scheme required)
//   - a bare host (legacy): pharmacy-app.vercel.app or localhost:3000 —
//     matches that host+port on either scheme (dev convenience)
//   - a subdomain wildcard: *.vercel.app — HTTPS ONLY, matches any subdomain
//     at any depth, never the bare domain itself, never a lookalike TLD
//   - "*"                   development convenience only — config.Validate
//     refuses it in production (credentials make it a reflection hole)
func originAllowed(origin string, allowedOrigins []string) bool {
        if origin == "" {
                return false
        }
        scheme, host, port, ok := parseOrigin(origin)
        if !ok {
                return false
        }
        for _, candidate := range allowedOrigins {
                candidate = strings.TrimSpace(candidate)
                if candidate == "" {
                        continue
                }
                if candidate == "*" {
                        return true
                }
                if strings.HasPrefix(candidate, "*.") {
                        _, wHost, wPort, wOK := parseOrigin(strings.TrimPrefix(candidate, "*."))
                        if !wOK || wHost == "" {
                                continue
                        }
                        // Wildcard entries are HTTPS-only: an http page must never ride
                        // a *.vercel.app preview entry.
                        if scheme != "https" {
                                continue
                        }
                        if host == wHost || !strings.HasSuffix(host, "."+wHost) {
                                continue
                        }
                        if wPort != "" && effectivePort(scheme, port) != effectivePort("https", wPort) {
                                continue
                        }
                        return true
                }
                cs, ch, cp, cOK := parseOrigin(candidate)
                if !cOK || ch == "" {
                        continue
                }
                if cs != "" && cs != scheme {
                        continue // AUD-018: the http twin of an https entry is a different origin
                }
                if ch != host {
                        continue
                }
                if effectivePort(cs, cp) != effectivePort(scheme, port) {
                        continue
                }
                return true
        }
        return false
}

// parseOrigin reduces an origin (or bare host[:port]) to its lowercase
// scheme / host / port triple. Without a scheme the result is a legacy
// bare-host match (scheme-agnostic). Ports are validated; paths are cut.
func parseOrigin(value string) (scheme, host, port string, ok bool) {
        value = strings.ToLower(strings.TrimSpace(value))
        if i := strings.Index(value, "://"); i >= 0 {
                scheme = value[:i]
                value = value[i+3:]
                if scheme != "http" && scheme != "https" {
                        return "", "", "", false
                }
        }
        if i := strings.IndexAny(value, "/?#"); i >= 0 {
                value = value[:i]
        }
        if value == "" {
                return "", "", "", false
        }
        host, port = splitHostPort(value)
        if host == "" {
                return "", "", "", false
        }
        if port != "" {
                if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
                        return "", "", "", false
                }
        }
        return scheme, host, port, true
}

// splitHostPort splits host[:port], careful not to break IPv6 literals.
func splitHostPort(value string) (host, port string) {
        if i := strings.LastIndex(value, ":"); i >= 0 && !strings.Contains(value, "]") {
                return value[:i], value[i+1:]
        }
        return value, ""
}

// effectivePort resolves the port a request effectively used: the explicit
// one, else the scheme's default, else "" (scheme-agnostic comparison).
func effectivePort(scheme, port string) string {
        if port != "" {
                return port
        }
        switch scheme {
        case "https":
                return "443"
        case "http":
                return "80"
        }
        return ""
}
