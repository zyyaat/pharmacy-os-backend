// Package ratelimit provides a self-contained token-bucket limiter: born as
// the per-IP guard for the public auth surface (AUD-004, HIGH — before this
// package existed the repository had no x/time/rate usage and no per-IP
// budget on any route, only per-account lockout, which does not stop
// password spraying across many accounts from one address), and extended in
// R-2 (api_level 106) with MiddlewareForKey so the SAME bucket store also
// shields the authenticated API groups per-principal and the signed payment
// webhooks per-IP — same mechanics, same 429 shape, no new dependency.
//
// It lives in its own package (NOT internal/middleware) because
// internal/middleware already imports internal/auth, and the auth package
// needs to attach this limiter to its routes — importing middleware back
// would create a cycle.
//
// Scope note: buckets live in process memory. The production deployment is
// a single instance (the support WebSocket hub is process-local by design),
// so one process is the correct sharing boundary today. A future
// multi-instance VPS deployment must move the bucket store behind Redis or
// sticky-route the auth family — the written decision is docs/adr/
// ADR-0001-single-instance-process-state.md (R-3), operational note in
// docs/deployment.md §10.3.
package ratelimit

import (
        "net/http"
        "sync"
        "time"

        "github.com/gin-gonic/gin"
        "golang.org/x/time/rate"
)

// Defaults sized for the PROXY REALITY, not the ideal one: all web-app
// traffic reaches the backend through Vercel's proxy, whose egress IPs are
// public, few per region and rotating — so they CANNOT be trusted for
// X-Forwarded-For (spoofing hole), which means web users collapse into a
// handful of shared buckets. A tight per-IP budget would exhaust on a normal
// morning and lock every web user out — an outage caused by the guard
// itself. Hence a generous flood-shield budget (120/min, burst 30): enough
// for a whole region of real users, still hopeless for scripted abuse. The
// TIGHT anti-guess walls are IP-independent: the per-email verify window
// (10/15min in the DB) and the per-account login lockout (5/15min).
// bcrypt cost (~1.2s/attempt on production hardware) further taxes floods.
// Both knobs are env-tunable via AUTH_RATE_LIMIT_RPM / AUTH_RATE_LIMIT_BURST.
const (
        DefaultRequestsPerMinute = 120
        DefaultBurst             = 30
        defaultMaxIdle           = 10 * time.Minute
        defaultJanitorEvery      = time.Minute
)

// IPRateLimiter is a token-bucket set keyed by client IP. Buckets are
// created lazily on first use and evicted by a janitor after maxIdle —
// without eviction an internet-scale attacker would grow the map forever
// (a memory-DoS in a limiter meant to stop a DoS).
type IPRateLimiter struct {
        mu       sync.Mutex
        buckets  map[string]*ipBucket
        limit    rate.Limit
        burst    int
        maxIdle  time.Duration
        janitor  time.Duration
        stop     chan struct{}
        stopOnce sync.Once
}

type ipBucket struct {
        limiter  *rate.Limiter
        lastSeen time.Time
}

// NewIPRateLimiter builds a limiter admitting requestsPerMinute per key
// (refilled continuously) with burst instantaneous allowance. Non-positive
// values fall back to the defaults so a bad env var cannot disable the wall.
func NewIPRateLimiter(requestsPerMinute, burst int) *IPRateLimiter {
        if requestsPerMinute <= 0 {
                requestsPerMinute = DefaultRequestsPerMinute
        }
        if burst <= 0 {
                burst = DefaultBurst
        }
        l := &IPRateLimiter{
                buckets: make(map[string]*ipBucket),
                limit:   rate.Limit(float64(requestsPerMinute) / 60.0),
                burst:   burst,
                maxIdle: defaultMaxIdle,
                janitor: defaultJanitorEvery,
                stop:    make(chan struct{}),
        }
        go l.evictLoop()
        return l
}

// Allow consumes one token for key and reports whether it was admitted.
func (l *IPRateLimiter) Allow(key string) bool {
        l.mu.Lock()
        defer l.mu.Unlock()
        b, ok := l.buckets[key]
        now := time.Now()
        if !ok {
                b = &ipBucket{limiter: rate.NewLimiter(l.limit, l.burst)}
                l.buckets[key] = b
        }
        b.lastSeen = now
        return b.limiter.Allow()
}

// Middleware returns a gin guard keyed by c.ClientIP(): over-budget
// requests are aborted with 429 + Retry-After BEFORE any handler work
// (rate limiting must precede CSRF, body parsing and bcrypt).
//
// Key correctness depends on trusted-proxy configuration: ClientIP() only
// reflects the real caller when the engine's SetTrustedProxies covers the
// reverse proxy that appends X-Forwarded-For (see handlers.NewServer).
// Otherwise a spoofed header would mint fresh buckets per request.
func (l *IPRateLimiter) Middleware() gin.HandlerFunc {
        return func(c *gin.Context) {
                if !l.Allow(c.ClientIP()) {
                        rejectOverBudget(c, "Too many requests from this address. Try again shortly.")
                        return
                }
                c.Next()
        }
}

// MiddlewareForKey (R-2, api_level 106) returns a gin guard keyed by an
// arbitrary request-derived key instead of the client IP — the extension
// the audit asked for ("توسيع الـpkg الحالي لا اختراعه"): the authenticated
// API groups key by PRINCIPAL (an identity taken from the signed session
// cookie — unspoofable and collision-free, unlike ClientIP under
// TRUSTED_PROXIES="*", the exact spoofing hole the audit flagged in §4.5.4),
// while the bucket store, the janitor, the non-positive fallbacks and the
// 429 wire shape stay identical to the AUD-004 guard.
//
// A keyFn returning "" (identity absent) falls back to the caller IP so the
// shield can never pass silently: fail-closed, AUD-004 semantics for any
// request that reaches the guard without an identity.
func (l *IPRateLimiter) MiddlewareForKey(keyFn func(*gin.Context) string) gin.HandlerFunc {
        return func(c *gin.Context) {
                key := keyFn(c)
                if key == "" {
                        key = c.ClientIP()
                }
                if !l.Allow(key) {
                        rejectOverBudget(c, "Too many requests. Try again shortly.")
                        return
                }
                c.Next()
        }
}

// rejectOverBudget is the single 429 body for both guards: the same
// {error, code, message} envelope family as auth's writeError / apierr
// (R-14), with Retry-After so well-behaved clients back off instead of
// spinning into the same wall.
func rejectOverBudget(c *gin.Context, message string) {
        c.Header("Retry-After", "60")
        c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
                "error":   "rate_limited",
                "message": message,
                "code":    "RATE_LIMITED",
        })
}

// Stop terminates the janitor (tests, graceful shutdown).
func (l *IPRateLimiter) Stop() {
        l.stopOnce.Do(func() { close(l.stop) })
}

func (l *IPRateLimiter) evictLoop() {
        ticker := time.NewTicker(l.janitor)
        defer ticker.Stop()
        for {
                select {
                case <-l.stop:
                        return
                case <-ticker.C:
                        l.mu.Lock()
                        cutoff := time.Now().Add(-l.maxIdle)
                        for key, bucket := range l.buckets {
                                if bucket.lastSeen.Before(cutoff) {
                                        delete(l.buckets, key)
                                }
                        }
                        l.mu.Unlock()
                }
        }
}
