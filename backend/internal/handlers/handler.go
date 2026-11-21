// Package handlers contains HTTP handlers for the API
package handlers

import (
        "context"
        "log"
        "net/http"
        "strings"
        "time"

        "github.com/gin-gonic/gin"
        "github.com/gorilla/websocket"
        "github.com/jackc/pgx/v5/pgxpool"
        "github.com/pharmacy-os/backend/internal/auth"
        "github.com/pharmacy-os/backend/internal/config"
        "github.com/pharmacy-os/backend/internal/jobs"
        "github.com/pharmacy-os/backend/internal/models"
        appmiddleware "github.com/pharmacy-os/backend/internal/middleware"
        "github.com/pharmacy-os/backend/internal/ratelimit"
        "github.com/pharmacy-os/backend/internal/repository"
        "github.com/pharmacy-os/backend/internal/subscription"
)

// SubscriptionGate is the narrow surface of *subscription.Service the
// handlers consume (Task 90 commercial gates + cache invalidation). An
// interface so tests can fake the gates without a database — *subscription.
// Service satisfies it implicitly.
type SubscriptionGate interface {
        EnforcePermission(c *gin.Context, companyID, permissionKey string) bool
        EnforceStatus(c *gin.Context, companyID string) bool
        EnforceLimit(c *gin.Context, companyID, limitKey string) bool
        GetEffective(ctx context.Context, companyID string) (*models.EffectivePlan, error)
        Invalidate(companyID string)
        InvalidateAll()
        CompanyIDForPharmacy(ctx context.Context, pharmacyID string) (string, error)
        CheckLimit(ctx context.Context, companyID, limitKey string) (int, int, bool, error)
        UsageAll(ctx context.Context, companyID string) (map[string]int, error)
}

// Handler holds all dependencies for HTTP handlers
type Handler struct {
        config  *config.Config
        db      *pgxpool.Pool
        auth    *auth.Handler
        company *CompanyHandler
        subs    SubscriptionGate
        // Support live chat (Phase T1): the WebSocket hub + its upgrader.
        // REST stays the source of truth; the hub only accelerates delivery.
        supportHub      *supportHub
        supportUpgrader websocket.Upgrader
        // Background jobs (api_level 118): the River client powering the
        // wholesale library import and the library update notices. nil =
        // disabled (kill switch, construction failure, or a bare test
        // handler) — enqueue paths answer 503 and nothing else changes.
        jobsClient *jobs.Client
        jobsDeps   jobs.Deps
}

// StartAuthJanitor launches the auth artifact cleanup loop (M1/G3): expired
// WebAuthn challenges and dead step-up grants are reaped every interval.
// Passthrough so the server entry point never reaches into auth internals;
// no-op when no database is wired (bare test handlers).
func (h *Handler) StartAuthJanitor(ctx context.Context, interval time.Duration) {
        if h.auth == nil {
                return
        }
        h.auth.StartAuthJanitor(ctx, interval)
}

// New creates a new Handler instance
func New(cfg *config.Config, db ...*pgxpool.Pool) *Handler {
        h := &Handler{config: cfg}
        if len(db) > 0 && db[0] != nil {
                h.db = db[0]
                h.supportHub = newSupportHub(db[0])
                h.supportUpgrader = h.newSupportUpgrader()
                // SaaS plan enforcement (Task 90): one process-wide service
                // backs both the pharmacy and company permission gates.
                subs := subscription.NewService(db[0])
                h.subs = subs
                subscription.SetDefault(subs)
                h.auth = auth.NewHandler(db[0], auth.Config{
                        AccessTTL:     cfg.AuthAccessTTL,
                        RefreshTTL:    cfg.AuthRefreshTTL,
                        CookieSecure:  cfg.CookieSecure,
                        CookieDomain:  cfg.CookieDomain,
                        BrevoAPIKey:   cfg.BrevoAPIKey,
                        MailFromEmail: cfg.MailFromEmail,
                        MailFromName:  cfg.MailFromName,
                        PublicAppURL:  cfg.PublicAppURL,
                        // Task 133 — SMTP escape hatch mapping; the transport
                        // verdict comes from the single resolution rule on
                        // the global config.
                        MailSMTPHost:      cfg.MailSMTPHost,
                        MailSMTPPort:      cfg.MailSMTPPort,
                        MailSMTPUsername:  cfg.MailSMTPUsername,
                        MailSMTPPassword:  cfg.MailSMTPPassword,
                        TransportResolved: cfg.ResolvedMailTransport(),
                        RateLimitRPM:   cfg.AuthRateLimitRPM,
                        RateLimitBurst: cfg.AuthRateLimitBurst,
                        // Task 148 — fair-use budget mapping (per-recipient
                        // hour/day caps + platform-wide daily ceiling).
                        MailSendHourlyLimit:  cfg.MailSendHourlyLimit,
                        MailSendDailyLimit:   cfg.MailSendDailyLimit,
                        MailGlobalDailyLimit: cfg.MailGlobalDailyLimit,
                        // Task 151 — WebAuthn passkeys: Relying Party identity
                        // derived from PUBLIC_APP_URL (empty → family disabled).
                        WebAuthnRPID:      cfg.PasskeyRPID,
                        WebAuthnRPName:    cfg.PasskeyRPName,
                        WebAuthnRPOrigins: nonEmptyStrings(cfg.PasskeyRPOrigin),
                        // Task 160 (M3) — native-client opaque origins: the env
                        // carries a comma list ("android:apk-key-hash:<b64url>"),
                        // split here because PASSKEY_RP_ORIGIN stays single-valued.
                        WebAuthnRPOpaqueOrigins: csvNonEmpty(cfg.PasskeyRPOpaqueOrigins),
                        // تسجيل الدخول بجوجل: معرف العميل + نقطة المفاتيح العامة.
                        GoogleClientID: cfg.GoogleClientID,
                        GoogleJWKSURL:  cfg.GoogleJWKSURL,
                })
                h.company = NewCompanyHandler(
                        repository.NewCompanyRepository(db[0]),
                        repository.NewCompanyUserRepository(db[0]),
                        repository.NewCompanyUserPermissionRepository(db[0]),
                        nil,
                )
        }
        return h
}

// nonEmptyStrings filters out blank entries so an unset PASSKEY_RP_ORIGIN
// yields an empty slice (→ passkeys disabled) instead of a slice holding "".
func nonEmptyStrings(values ...string) []string {
        kept := make([]string, 0, len(values))
        for _, value := range values {
                if strings.TrimSpace(value) != "" {
                        kept = append(kept, value)
                }
        }
        return kept
}

// csvNonEmpty splits a comma-separated environment value into trimmed,
// non-blank entries (Task 160 — PASSKEY_RP_OPAQUE_ORIGINS). An unset or blank
// value yields a nil slice so the pre-M3 web-only behavior is byte-identical.
func csvNonEmpty(value string) []string {
        if strings.TrimSpace(value) == "" {
                return nil
        }
        parts := strings.Split(value, ",")
        kept := make([]string, 0, len(parts))
        for _, part := range parts {
                if trimmed := strings.TrimSpace(part); trimmed != "" {
                        kept = append(kept, trimmed)
                }
        }
        if len(kept) == 0 {
                return nil
        }
        return kept
}

// SetupRoutes configures all API routes
func (h *Handler) SetupRoutes(r *gin.Engine) {
        // Allow the configured frontend origins to call the API.
        r.Use(appmiddleware.CORS(h.config.GetCorsOrigins()...))

        // Root health endpoint for deployment startup probes.
        r.GET("/", h.HealthCheck)

        // API v1 group
        v1 := r.Group("/api/v1")

        // R-2 (api_level 106) — one per-principal token bucket shared by the
        // four authenticated API groups (keys are namespaced "p:<type>:<id>"
        // and a principal lives in exactly one realm group, so one store is
        // safe and carries one janitor). Mounted AFTER each group's auth
        // middleware so the key is the session identity, not the spoofable
        // ClientIP — the audit's threat is the authenticated abuser (or a
        // stolen cookie) hammering the heavy lists, and an identity-derived
        // budget cannot be shaken off by rotating X-Forwarded-For headers.
        principalShield := ratelimit.NewIPRateLimiter(
                h.config.APIRateLimitRPM, h.config.APIRateLimitBurst)

        // Health check (both /health and /api/v1/health for compatibility)
        r.GET("/health", h.HealthCheck)
        v1.GET("/health", h.HealthCheck)
        // AUD T-A6 — /readyz is the load-bearing probe: it answers only when
        // the database dependency answers. /health stays a static liveness
        // signal (process up); uptime monitors and orchestrators poll /readyz
        // so a database outage is visible instead of masked by a green /health.
        r.GET("/readyz", h.ReadyCheck)
        v1.GET("/readyz", h.ReadyCheck)

        // R-9 (api_level 103) — the Prometheus exposition, fail-closed:
        // METRICS_TOKEN unset keeps /metrics at 404 (feature off, the
        // /debug/schema posture); set, it answers only to
        // `Authorization: Bearer <token>`. Mounted on the bare engine (not
        // v1) so scrapers hit a stable top-level path; CORS above applies
        // harmlessly (plain GET).
        r.GET("/metrics", appmiddleware.MetricsHandler(h.config.MetricsToken))

        if h.auth != nil {
                h.auth.RegisterRoutes(v1)

                // Company dashboard data is always scoped by the authenticated
                // company session. The handlers derive the company id from the
                // principal instead of accepting it from the client.
                dashboard := v1.Group("/dashboard")
                dashboard.Use(h.auth.Middleware(auth.PlatformRealm))
                dashboard.Use(principalShield.MiddlewareForKey(principalShieldKey))
                dashboard.Use(appmiddleware.CompanySessionContext())
                dashboard.Use(appmiddleware.CompanyDBPoolContext(h.db))
                dashboard.Use(appmiddleware.RequireCompanyPermission("companies.view"))
                dashboard.GET("/stats", h.GetDashboardStats)
                dashboard.GET("/activity", h.GetRecentActivity)

                // Platform admin routes are global. They have their own explicit
                // super-admin guard and never reuse company-scoped dashboard routes.
                platformAdmin := v1.Group("/platform-admin")
                platformAdmin.Use(h.auth.Middleware(auth.PlatformRealm))
                platformAdmin.Use(requirePlatformSuperAdmin())
                platformAdmin.Use(principalShield.MiddlewareForKey(principalShieldKey))
                platformAdmin.GET("/stats", h.GetPlatformAdminStats)
                platformAdmin.GET("/settings", h.GetPlatformSettings)
                platformAdmin.PATCH("/settings", auth.CSRF(auth.PlatformRealm), h.UpdatePlatformSettings)
                platformAdmin.GET("/companies", h.ListPlatformCompanies)
                platformAdmin.GET("/users", h.ListPlatformUsers)
                // Task 132 — manual email verification. The live delivery probe
                // proved Brevo can accept every send while nothing lands in any
                // inbox (shared-IP reputation, invisible to the app); the
                // operator needs a same-minute unblock that does not depend on
                // email. CSRF-guarded like every platform mutation.
                platformAdmin.POST("/users/verify-email", auth.CSRF(auth.PlatformRealm), h.VerifyPlatformUserEmail)
                platformAdmin.GET("/accounts", h.ListPlatformAccounts)
                platformAdmin.GET("/permissions", h.ListPlatformPermissions)
                // SaaS plans management (Task 90): fully dynamic plans +
                // subscription operations + manual payments. All writes are
                // CSRF-guarded like the rest of the platform mutations.
                platformAdmin.GET("/plans", h.ListPlatformPlans)
                platformAdmin.POST("/plans", auth.CSRF(auth.PlatformRealm), h.CreatePlatformPlan)
                platformAdmin.GET("/plans/:id", h.GetPlatformPlan)
                platformAdmin.PUT("/plans/:id", auth.CSRF(auth.PlatformRealm), h.UpdatePlatformPlan)
                platformAdmin.PATCH("/plans/:id/status", auth.CSRF(auth.PlatformRealm), h.UpdatePlatformPlanStatus)
                platformAdmin.DELETE("/plans/:id", auth.CSRF(auth.PlatformRealm), h.DeletePlatformPlan)
                platformAdmin.GET("/features", h.ListPlatformFeatures)
                platformAdmin.GET("/subscriptions", h.ListPlatformSubscriptions)
                platformAdmin.GET("/subscriptions/overview", h.SubscriptionsOverview)
                platformAdmin.POST("/subscriptions", auth.CSRF(auth.PlatformRealm), h.CreatePlatformSubscription)
                platformAdmin.PATCH("/subscriptions/:id", auth.CSRF(auth.PlatformRealm), h.UpdatePlatformSubscription)
                platformAdmin.GET("/payments", h.ListPlatformPayments)
                platformAdmin.POST("/payments/manual", auth.CSRF(auth.PlatformRealm), h.CreateManualPayment)
                // Task 165 — بيت أداة دفتر الترحيلات على سطح تطوير المنتج:
                // GET /platform-admin/system/migrations (حارس المجموعة PlatformRealm
                // هو التصريح) — والمسار المستأجري القديم يظل حيًا لعقد Flutter
                // المجمّد لكنه مقفول لأعضاء شركة المنصة وحدهم (requesterIsPlatformCompany).
                platformAdmin.GET("/system/migrations", h.GetPlatformSystemMigrations)
                platformAdmin.POST("/payments/:id/refund", auth.CSRF(auth.PlatformRealm), h.RefundPlatformPayment)
                // Phase S1 — settlement & reconciliation: the operator's
                // matching worklist (read-only) plus the two audited actions
                // a payment row supports: recording the provider payout batch
                // (settle) and pulling the session truth from XPay (resync).
                platformAdmin.GET("/payments/reconciliation", h.PaymentsReconciliation)
                platformAdmin.GET("/payments/:id", h.GetPlatformPaymentDetail)
                platformAdmin.POST("/payments/:id/settlement", auth.CSRF(auth.PlatformRealm), h.SettlePlatformPayment)
                platformAdmin.POST("/payments/:id/resync", auth.CSRF(auth.PlatformRealm), h.ResyncPlatformPayment)
                // Phase T1 — support live chat + tickets: the platform inbox
                // (conversation list with unread/unanswered filters), the
                // ticket work table and the audited lifecycle PATCH. Support
                // is never plan-gated — a locked-out company must still reach
                // the operator. The platform socket pushes committed events;
                // REST stays the source of truth.
                platformAdmin.GET("/support/overview", h.GetPlatformSupportOverview)
                platformAdmin.GET("/support/conversations", h.ListPlatformSupportConversations)
                platformAdmin.GET("/support/conversations/:id", h.GetPlatformSupportConversation)
                platformAdmin.GET("/support/conversations/:id/messages", h.ListPlatformSupportMessages)
                platformAdmin.POST("/support/conversations/:id/messages", auth.CSRF(auth.PlatformRealm), h.SendPlatformSupportMessage)
                platformAdmin.POST("/support/conversations/:id/read", auth.CSRF(auth.PlatformRealm), h.MarkPlatformSupportRead)
                platformAdmin.POST("/support/conversations/:id/close", auth.CSRF(auth.PlatformRealm), h.ClosePlatformSupportConversation)
                platformAdmin.POST("/support/attachments", auth.CSRF(auth.PlatformRealm), h.UploadPlatformSupportAttachment)
                platformAdmin.GET("/support/attachments/:id", h.GetPlatformSupportAttachment)
                platformAdmin.GET("/support/tickets", h.ListPlatformSupportTickets)
                platformAdmin.PATCH("/support/tickets/:id", auth.CSRF(auth.PlatformRealm), h.UpdatePlatformSupportTicket)
                platformAdmin.GET("/support/ws", h.SupportPlatformWS)
                // Task 15 — per-company account page: profile + per-company
                // entitlement overrides (plan baseline + per-account merge)
                // + the account's own audit log. Company-scoped routes are
                // NESTED under /companies/:id/* so the wildcard-free path
                // keeps /payments/paymob-diagnostics resolvable.
                platformAdmin.GET("/companies/:id", h.GetPlatformCompany)
                platformAdmin.GET("/companies/:id/entitlements", h.ListCompanyEntitlements)
                platformAdmin.POST("/companies/:id/entitlements", auth.CSRF(auth.PlatformRealm), h.UpsertCompanyEntitlement)
                platformAdmin.DELETE("/companies/:id/entitlements/:eid", auth.CSRF(auth.PlatformRealm), h.DeleteCompanyEntitlement)
                platformAdmin.GET("/companies/:id/logs", h.ListCompanyLogs)
                // Task 90 prod diagnostics — super-admin self-service for the
                // intention 404 investigation: echoes the exact Paymob config
                // and (with PAYMOB_DIAG_API_KEY) lists the integration IDs
                // that really exist on the account. Read-only GET.
                platformAdmin.GET("/payments/paymob-diagnostics", h.PaymobDiagnostics)
                // Central product libraries («مكتبات المنتجات») — the
                // platform control room for the shared catalog: named,
                // country-targeted, versioned libraries with official prices
                // per entry + Excel bulk import (price bulletins). Every
                // write is transactional + audited to platform_audit_logs.
                // Pharmacy-side import/sync ships in the next api level.
                platformAdmin.GET("/libraries", h.ListPlatformLibraries)
                platformAdmin.POST("/libraries", auth.CSRF(auth.PlatformRealm), h.CreatePlatformLibrary)
                platformAdmin.GET("/libraries/:id", h.GetPlatformLibrary)
                platformAdmin.PUT("/libraries/:id", auth.CSRF(auth.PlatformRealm), h.UpdatePlatformLibrary)
                platformAdmin.DELETE("/libraries/:id", auth.CSRF(auth.PlatformRealm), h.DeletePlatformLibrary)
                platformAdmin.POST("/libraries/:id/publish", auth.CSRF(auth.PlatformRealm), h.PublishPlatformLibrary)
                platformAdmin.GET("/libraries/:id/changes", h.ListPlatformLibraryChanges)
                platformAdmin.GET("/libraries/:id/products", h.ListPlatformLibraryProducts)
                platformAdmin.POST("/libraries/:id/products", auth.CSRF(auth.PlatformRealm), h.AddPlatformLibraryProduct)
                platformAdmin.PUT("/libraries/:id/products/:pid", auth.CSRF(auth.PlatformRealm), h.UpdatePlatformLibraryProduct)
                platformAdmin.DELETE("/libraries/:id/products/:pid", auth.CSRF(auth.PlatformRealm), h.RemovePlatformLibraryProduct)
                platformAdmin.POST("/libraries/:id/import/preview", auth.CSRF(auth.PlatformRealm), h.PreviewPlatformLibraryImport)
                platformAdmin.POST("/libraries/:id/import/execute", auth.CSRF(auth.PlatformRealm), h.ExecutePlatformLibraryImport)
                platformAdmin.GET("/catalog/products", h.ListPlatformCatalogProducts)
                platformAdmin.PUT("/catalog/products/:id", auth.CSRF(auth.PlatformRealm), h.UpdatePlatformCatalogProduct)

                // Pharmacy data is scoped from the authenticated employee/company
                // principal. These endpoints intentionally do not accept a pharmacy
                // id in the URL or query string.
                pharmacy := v1.Group("/pharmacy")
                pharmacy.Use(h.auth.Middleware(auth.PharmacyRealm))
                pharmacy.Use(auth.RequirePharmacyPrincipal())
                pharmacy.Use(principalShield.MiddlewareForKey(principalShieldKey))

                // Flexible permission guards (Task 42). Company owners
                // (admin/manager) and legacy employees without explicit
                // permission rows pass through untouched — existing workflows
                // keep working. Only employees with an explicit permission set
                // are restricted to what the owner granted them.
                perm := h.requirePharmacyPermission

                pharmacy.GET("/context", h.GetPharmacyContext)
                // Public Paymob webhook (Phase G): no session auth — the proof
                // is the HMAC-SHA512 signature + optional URL token. Registered
                // in the Paymob dashboard as
                // https://<backend-host>/api/v1/payments/webhook/paymob?token=…
                // R-2 (api_level 106) — a LOOSE per-IP flood shield rides in
                // front of each signed webhook (no session to key by, so the
                // key is the gateway's IP). Mounted before the body read and
                // HMAC work so a flood costs nothing; a 429 is safe by
                // contract — both gateways retry with backoff and every
                // webhook handler is idempotent (dedup, AUD-010/032), so no
                // payment event is ever lost to the shield.
                webhookShield := ratelimit.NewIPRateLimiter(
                        h.config.WebhookRateLimitRPM, h.config.WebhookRateLimitBurst)
                v1.POST("/payments/webhook/paymob", webhookShield.Middleware(), h.PaymobWebhook)
                // Public XPay webhook (Phase X — the replacement gateway): no
                // session auth — the proof is the HMAC-SHA256 XPay-Signature
                // over the raw body + optional URL token. Registered in the
                // XPay dashboard as
                // https://<backend-host>/api/v1/payments/webhook/xpay?token=…
                // Paymob's webhook stays mounted: pending paymob payments keep
                // resolving through their own provider even after the switch.
                v1.POST("/payments/webhook/xpay", webhookShield.Middleware(), h.XPayWebhook)
                // SaaS subscription surface (Task 90): these two are part of
                // the lockout allow-list — an expired/suspended company must
                // always be able to read its own status and the public plans
                // so it can recover. No status gate here, by design.
                pharmacy.GET("/subscription", h.GetPharmacySubscription)
                pharmacy.GET("/plans", h.ListPublicPlans)
                // Phase G — online checkout (EMBEDDED Unified Checkout):
                // deliberately NOT behind the plan permission/status gates —
                // an expired or suspended company must be able to pay to
                // recover, which is the entire point of the recovery flow.
                // Mutation principal + CSRF + server-side company scope still
                // apply. The card form renders INSIDE the app modal; the
                // webhook is the only activation path.
                pharmacy.POST("/subscription/checkout", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), h.CreatePharmacyCheckout)
                pharmacy.GET("/subscription/payments/:id", h.GetPharmacyPaymentStatus)
                // Self-service billing (global best practice): cancel at period
                // end + resume + the company's own payment history. Behind the
                // billing permission EVERY plan grants (a company must always
                // control its own billing) + mutation principal + CSRF on
                // writes. Cancelling is a FLAG, not a kill switch: paid access
                // continues to the end of the period, then normal expiry.
                pharmacy.POST("/subscription/cancel", perm("settings.billing"), auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), h.CancelPharmacySubscription)
                pharmacy.POST("/subscription/resume", perm("settings.billing"), auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), h.ResumePharmacySubscription)
                pharmacy.GET("/subscription/payments", perm("settings.billing"), h.ListPharmacyPayments)
                // Delta sync (offline-first smart synchronization): one cheap
                // round-trip returns only what changed since the caller's
                // cursor + tombstoned deletions, in row shapes identical to
                // the list endpoints (sync_rows.go). Sections self-filter by
                // the same permission keys as their list endpoints, so no
                // route-level permission guard is needed — exactly like
                // /context and /settings.
                pharmacy.GET("/sync", h.GetPharmacySync)
                pharmacy.GET("/dashboard/stats", perm("dashboard.view"), h.GetPharmacyDashboardStats)
                pharmacy.GET("/dashboard/activity", perm("dashboard.view"), h.GetPharmacyDashboardActivity)
                pharmacy.GET("/inventory", perm("inventory.view"), h.GetPharmacyInventory)
                pharmacy.GET("/inventory/movements", perm("inventory.movements.view"), h.ListPharmacyStockMovements)
                pharmacy.GET("/inventory/low-stock", perm("inventory.view"), h.GetPharmacyLowStock)
                pharmacy.GET("/system/migrations", h.requirePharmacyOwner(), h.GetSystemMigrations)
                pharmacy.GET("/products", perm("inventory.view"), h.ListPharmacyProducts)
                pharmacy.POST("/products", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("inventory.manage_products"), h.CreatePharmacyProduct)
                pharmacy.GET("/products/:id", perm("inventory.view"), h.GetPharmacyProduct)
                pharmacy.PUT("/products/:id", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("inventory.manage_products"), h.UpdatePharmacyProduct)
                // D4 (api_level 122): deliberate catalog detachment — turns a
                // central product into a real LOCAL one (confirm gate inside,
                // historical invoice snapshots frozen in the same transaction,
                // fully audited).
                pharmacy.POST("/products/:id/unlink", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("inventory.manage_products"), h.UnlinkPharmacyProduct)
                // Opening stock (api_level 119): the first batch of a product
                // that has none — library imports create products with zero
                // stock by red line, and without this receiving path those
                // products were stranded (invisible to the inventory page,
                // unsellable, unable to ever receive stock). Same ledger
                // contract as adjust: one batch + one movement, idempotent.
                pharmacy.POST("/products/:id/opening-stock", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("inventory.adjust"), h.OpenProductStock)
                // Internal barcode generation (barcode system v1): a single
                // product gets a barcode from its form, the settings batch
                // panel fills many at once. Both write under the same product
                // management permission and generate server-side only.
                pharmacy.POST("/products/:id/barcode/generate", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("inventory.manage_products"), h.GenerateProductBarcode)
                pharmacy.POST("/barcodes/bulk-generate", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("inventory.manage_products"), h.BulkGenerateProductBarcodes)
                // استيراد المنتجات من ملف جداول (ترحيل البرامج القديمة)
                pharmacy.GET("/imports/products/template", perm("inventory.import"), h.ProductImportTemplate)
                pharmacy.POST("/imports/products/preview", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("inventory.import"), h.PreviewProductImport)
                pharmacy.POST("/imports/products/execute", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("inventory.import"), h.ExecuteProductImport)
                // مكتبات المنتجات المركزية (المرحلة 2): المكتبات المرئية
                // للصيدلية حسب البلد + الاستيراد الذكي بالبوابات الثلاث
                // (اختيار ← كشف تكرار ← سقف الخطة) + سحب التحديثات بعد النشر.
                // القاعدة الحمراء: المخزون والتكلفة لا يُمسّان أبدًا.
                pharmacy.GET("/libraries", perm("inventory.view"), h.ListPharmacyLibraries)
                pharmacy.GET("/libraries/:id", perm("inventory.view"), h.GetPharmacyLibrary)
                pharmacy.GET("/libraries/:id/products", perm("inventory.view"), h.ListPharmacyLibraryProducts)
                pharmacy.GET("/libraries/:id/diff", perm("inventory.view"), h.GetPharmacyLibraryDiff)
                pharmacy.POST("/libraries/:id/import/preview", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("inventory.import"), h.PreviewPharmacyLibraryImport)
                pharmacy.POST("/libraries/:id/import/execute", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("inventory.import"), h.ExecutePharmacyLibraryImport)
                pharmacy.GET("/libraries/:id/import/status", perm("inventory.view"), h.GetPharmacyLibraryImportStatus)
                pharmacy.POST("/libraries/:id/sync", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("inventory.import"), h.SyncPharmacyLibrary)
                pharmacy.POST("/libraries/:id/remove/preview", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("inventory.import"), h.PreviewPharmacyLibraryRemoval)
                pharmacy.POST("/libraries/:id/remove/execute", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("inventory.import"), h.ExecutePharmacyLibraryRemoval)
                pharmacy.GET("/pos/products", perm("pos.access"), h.LookupPOSProduct)
                pharmacy.GET("/pos/search", perm("pos.access"), h.SearchPOSProducts)
                // Task 120 — خريطة الباركود لمسح فوري: قراءة واحدة بلا أي كتابة،
                // نفس بوابة صلاحية الكاشير كما نظيرتها lookup/search.
                pharmacy.GET("/pos/barcode-map", perm("pos.access"), h.GetPOSBarcodeMap)
                pharmacy.POST("/pos/sales", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("pos.access"), h.CreatePOSSale)
                pharmacy.GET("/pos/sales", perm("sales.view"), h.ListPOSSales)
                pharmacy.GET("/customers", perm("customers.view"), h.ListPharmacyCustomers)
                pharmacy.POST("/customers", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("customers.create"), h.CreatePharmacyCustomer)
                pharmacy.GET("/customers/:id/statement", perm("customers.view"), h.GetPharmacyCustomerStatement)
                pharmacy.POST("/customers/:id/payments", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("customers.payments"), h.CreatePharmacyCustomerPayment)
                pharmacy.PUT("/customers/:id", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("customers.update"), h.UpdatePharmacyCustomer)
                pharmacy.GET("/pos/sales/:sale_id", perm("sales.view"), h.GetPOSSale)
                pharmacy.POST("/pos/sales/:sale_id/returns", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("sales.returns"), h.CreatePOSSaleReturn)
                pharmacy.POST("/inventory/:batch_id/adjust", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("inventory.adjust"), h.AdjustPharmacyInventory)
                // Reports are read-only aggregates scoped by the session
                // principal — no mutation guard needed.
                pharmacy.GET("/reports/sales", perm("reports.sales"), h.GetPharmacySalesReport)
                pharmacy.GET("/reports/inventory", perm("reports.inventory"), h.GetPharmacyInventoryReport)
                pharmacy.GET("/reports/movements", perm("reports.movements"), h.GetPharmacyMovementsReport)

                // Flexible permission APIs (catalog/templates are open to every
                // pharmacy principal; management APIs require the matching
                // employees.* permissions like every other mutating endpoint).
                pharmacy.GET("/permissions/catalog", h.GetPermissionCatalog)
                pharmacy.GET("/permissions/templates", h.GetPermissionTemplates)
                pharmacy.GET("/permissions/me", h.GetMyPermissions)
                pharmacy.GET("/employees", perm("employees.view"), h.ListPharmacyEmployees)
                pharmacy.POST("/employees", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("employees.create"), h.CreatePharmacyEmployee)
                pharmacy.GET("/employees/:id/permissions", perm("employees.manage_permissions"), h.GetEmployeePermissions)
                pharmacy.PUT("/employees/:id/permissions", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("employees.manage_permissions"), h.UpdateEmployeePermissions)
                pharmacy.PATCH("/employees/:id/status", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("employees.update"), h.SetPharmacyEmployeeStatus)

                // Branches (Task 50): the branches tab is the single place to
                // manage pharmacy locations — read for every principal, write
                // behind the matching mutation guard + CSRF + permission.
                pharmacy.GET("/branches", perm("branches.view"), h.ListPharmacyBranches)
                pharmacy.POST("/branches", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("branches.create"), h.CreatePharmacyBranch)
                pharmacy.PUT("/branches/:id", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("branches.update"), h.UpdatePharmacyBranch)
                pharmacy.DELETE("/branches/:id", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("branches.delete"), h.DeletePharmacyBranch)
                pharmacy.GET("/attendance", perm("attendance.view"), h.ListPharmacyAttendance)
                // Pharmacy settings: receipt/print configuration. Reading is
                // open to every pharmacy principal; writing follows the same
                // mutation guard + CSRF as every other mutating endpoint.
                pharmacy.GET("/settings", h.GetPharmacySettings)
                pharmacy.PUT("/settings", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("settings.general"), h.UpdatePharmacySettings)
                // Label templates (barcode system v1): reading is open to every
                // pharmacy principal (the print action needs it); template
                // management is gated by the dedicated settings.labels
                // permission (migration 23) like every mutating endpoint.
                pharmacy.GET("/settings/labels", h.GetLabelSettings)
                pharmacy.PUT("/settings/labels", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), perm("settings.labels"), h.UpdateLabelSettings)

                // Task 57 — إعداد الصيدلية الذي يفتح مباشرة بعد التحقق من البريد:
                // القراءة لأي جلسة صيدلية صالحة، والكتابة بنفس حرس الطفرات + CSRF
                // كباقي نقاط النهاية المكتوبة. لا صلاحية مفصّلة هنا لأن المالك
                // الجديد يجب أن يمرّ من هذه الصفحة قبل أي إعداد آخر.
                pharmacy.GET("/onboarding", h.GetPharmacyOnboarding)
                // The onboarding write has no single permission key, but it
                // must still respect the subscription lockout (expired/
                // suspended companies are gated by status only).
                pharmacy.PUT("/onboarding", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), h.guardPlanStatus(h.UpdatePharmacyOnboarding))
                // Phase T1 — support live chat + tickets (company side):
                // deliberately NOT behind any permission key and NOT behind
                // guardPlanStatus — every principal of the company can reach
                // support, and above all an expired/suspended company must
                // still be able to ask for help (same reasoning as the
                // subscription recovery routes). Mutation principal + CSRF
                // still guard the writes.
                pharmacy.GET("/support/overview", h.GetPharmacySupportOverview)
                pharmacy.GET("/support/conversations", h.ListPharmacySupportConversations)
                pharmacy.POST("/support/conversations", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), h.CreatePharmacySupportConversation)
                pharmacy.GET("/support/conversations/:id", h.GetPharmacySupportConversation)
                pharmacy.GET("/support/conversations/:id/messages", h.ListPharmacySupportMessages)
                pharmacy.POST("/support/conversations/:id/messages", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), h.SendPharmacySupportMessage)
                pharmacy.POST("/support/conversations/:id/read", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), h.MarkPharmacySupportRead)
                pharmacy.POST("/support/conversations/:id/close", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), h.ClosePharmacySupportConversation)
                pharmacy.POST("/support/attachments", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), h.UploadPharmacySupportAttachment)
                pharmacy.GET("/support/attachments/:id", h.GetPharmacySupportAttachment)
                pharmacy.GET("/support/tickets", h.ListPharmacySupportTickets)
                pharmacy.POST("/support/tickets", auth.RequirePharmacyMutationPrincipal(), auth.CSRF(auth.PharmacyRealm), h.CreatePharmacySupportTicket)
                pharmacy.GET("/support/ws", h.SupportPharmacyWS)
        }
        // Diagnostics for legacy-schema forensics — AUD-016 closure. Three
        // independent gates must ALL pass: the env flag (off by default in
        // production), an authenticated platform-realm session, and the
        // super-admin role. An unauthenticated caller gets 401 from the auth
        // middleware and any non-super-admin gets 403 — the audit's acceptance
        // criterion — so even a misconfigured environment that leaves
        // APP_DEBUG=true no longer exposes the schema to strangers.
        h.registerDebugSchema(v1)

        // Domain routes use the central opaque session created by /auth/login.
        // The legacy company JWT middleware is intentionally not registered.
        if h.company != nil {
                company := v1.Group("/companies")
                company.Use(h.auth.Middleware(auth.PlatformRealm))
                company.Use(appmiddleware.CompanySessionContext())
                company.Use(appmiddleware.CompanyDBPoolContext(h.db))
                company.Use(appmiddleware.RequireCompanyPermission("companies.view"))
                company.Use(principalShield.MiddlewareForKey(principalShieldKey))

                company.GET("", h.company.ListCompanies)
                company.GET("/:id", h.company.GetCompany)
                company.GET("/:id/summary", h.company.GetCompanySummary)
                registerCompanyMutationRoutes(company,
                        h.company.UpdateCompany, h.company.UpdateCompanyStatus, h.company.DeleteCompany)
        }
}

// principalShieldKey (R-2, api_level 106) derives the per-principal bucket
// key from what auth's setPrincipal plants on every authenticated request
// (the string context keys are that middleware's public contract): the
// realm type + the identity id. Empty when no identity is present so the
// guard falls back to per-IP (fail-closed) instead of collapsing strangers
// into one shared budget.
func principalShieldKey(c *gin.Context) string {
        userID := c.GetString("user_id")
        if userID == "" {
                return ""
        }
        return "p:" + c.GetString("principal_type") + ":" + userID
}

// registerCompanyMutationRoutes wires the company record mutations behind
// CSRF + permission gates. AUD-003 (HIGH, launch blocker): this group was
// the single authenticated mutation family in the router left without
// auth.CSRF while production cookies are SameSite=None + Secure — a victim
// visiting an attacker page could ride the attached session cookie into
// silent PUT/PATCH/DELETE on /companies with admin privileges. Wrapped
// exactly like the sibling platform mutations; the regression test locking
// this chain (403 csrf_failed without the header, pass-through with the
// pair) lives in company_routes_csrf_test.go.
func registerCompanyMutationRoutes(company *gin.RouterGroup, update, updateStatus, deleteCompany gin.HandlerFunc) {
        company.PUT("/:id", auth.CSRF(auth.PlatformRealm), appmiddleware.RequireCompanyPermission("companies.update"), update)
        company.PATCH("/:id/status", auth.CSRF(auth.PlatformRealm), appmiddleware.RequireCompanyPermission("companies.update"), updateStatus)
        company.DELETE("/:id", auth.CSRF(auth.PlatformRealm), appmiddleware.RequireCompanyPermission("companies.delete"), deleteCompany)
}

// 101 — P1 performance wave (R-1/R-4/R-5/R-6): current_inventory reads the
// maintained cache once per row (migration 43 + 2 statement indexes); auth
// session last_used_at is throttled+async; POS invoice fulfillment is a
// fixed handful of statements (FEFO/allocation/ledger semantics pinned by
// TestPOSPersistDB); customer statement is keyset-paginated (after cursor,
// bounded pages, opening balance, has_more/next_cursor).
// APILevel is bumped with every behavioral backend change so a deployment's
// code state is verifiable from outside without credentials (Task 57: smart
// registration — verifying the email OTP now opens the session immediately
// and the pharmacy onboarding wizard ships with api_level 57; if /health
// reports a lower value, the running backend predates the deploy).
// 59 — delta sync: migration 25 (sales/customers updated_at + triggers,
// sync_tombstones) + GET /pharmacy/sync for the mobile offline cache.
// 60 — SaaS plans & subscriptions: migration 26 (plans/features/limits/
// subscriptions/payments), plan gate inside both permission middlewares,
// limit enforcement at creation endpoints, platform-admin plan/subscription
// management, GET /pharmacy/subscription + /pharmacy/plans.
// 61 — Paymob embedded checkout (Phase G): POST /pharmacy/subscription/checkout
// (intention server-side, client_secret + embed URL for the in-app iframe),
// GET /pharmacy/subscription/payments/:id polling, HMAC-SHA512-verified
// POST /payments/webhook/paymob as the only online activation path, shared
// applySucceededPaymentTx transition for manual + webhook payments.
// 62 — checkout now sends notification_url (our webhook URL + token) on
// every intention so the callback is code-driven, not dashboard-dependent;
// PAYMOB_SECRET_KEY is the canonical secret env name (falls back to the
// legacy PAYMOB_API_KEY).
// 63 — Paymob intention wire contract fixed against the live API: endpoint
// is POST {base}/v1/intention/ (integration ID inside the body as
// payment_methods, never in the URL path) and "amount" is integer
// piastres/cents as-is (the previous major-unit division undercharged by
// 100x); billing country now 3-letter ISO (EGY); locked by
// TestCreateIntentionWireContract.
// 64 — intention resilience: Paymob rejects the WHOLE intention with 404
// "Integration ID does not exist" when ANY payment_methods entry is bad,
// so a stale optional wallet ID no longer blocks card payments — on 404
// with multiple channels the client retries once with the card channel
// alone; HMAC concatenation locked to Paymob's official worked example
// (TestHMACConcatenationMatchesPaymobDocsExample).
// 65 — super-admin diagnostics: GET /platform-admin/payments/paymob-
// diagnostics echoes the exact Paymob config in use (base URL, masked
// keys, literal integration IDs) and — when PAYMOB_DIAG_API_KEY is set —
// probes the legacy API (auth/tokens → integrations list) to enumerate the
// integration IDs that really exist on the account; intention failures now
// log the exact base_url + payment_methods sent, making a dashboard
// mismatch provable from the server log alone.
// 66 — webhook HMAC fix: Paymob delivers the HMAC as a QUERY PARAMETER on
// the callback URL (docs + every official sample); the handler read
// obj["hmac"] from the body (always absent) so EVERY genuine webhook was
// rejected. Now read from the query first (body kept as fallback) +
// forensic log (hmac source/length + exact concatenated string, no
// secrets) on any future failure + HMAC secret fingerprint in the
// paymob-diagnostics response so a Test/Live secret mismatch is visible.
// 67 — subscriptions best-practice pass: 7-day post-expiry GRACE (access
// continues with renewal urgency instead of instant lockout), tenant
// self-service cancel-at-period-end/resume + payment history, admin
// payments ledger + billing overview (MRR/expiring-7d) + refunds with
// optional period shortening, trial-aware extend (extend no longer
// silently converts trials to paid), reactivate always restores access
// (fresh 30-day window when the period end is past), Stripe-style
// idempotency keys on manual payments (migration 27).
// 68 — plan pricing gate: a public+active plan must carry at least one
// positive price (Stripe-model) — closes the "0 EGP" dead-end checkout
// (plan editor no longer accepts unpriced public plans; pharmacy UI renders
// them as contact-support instead of a doomed subscribe button).
// 73 — plan seed repair: migration 26 seeded free/starter plan_permissions
// by joining plan_features BEFORE plan_features was populated, so those
// plans held only the 12-key core set — a company on «البداية» showed an
// ACTIVE subscription while every module API answered plan_permission_denied.
// Migration 28 repairs the data (idempotent, strictly additive), migration
// 27 is wired into the chain retroactively (payments.idempotency_key was
// authored but never applied — manual payments would 42703), and a startup
// consistency guard logs any plan whose features lack their permissions.
// 74 — per-company account page + «التحكم الكامل» (professional SaaS
// pattern): migration 29 adds company_entitlements — per-account
// feature/permission/limit overrides merged ON TOP of the plan baseline
// (resolution: override → plan) inside loadSets, so the permission gate,
// the limit gate and the pharmacy sidebar all see ONE merged source of
// truth; overrides are self-reversing via expires_at and audited. New
// endpoints: GET /platform-admin/companies/:id (+ /entitlements CRUD +
// /logs). Migration 30 adds platform_audit_logs and fixes the silent loss
// of ALL platform billing audit rows: the tenant writeAuditLog requires a
// pharmacy scope a platform principal does not have, so every plan/-
// subscription/payment audit write used to fail under `_ =`.
// 75 — central product libraries («مكتبات المنتجات»): migration 31 adds
// product_libraries (named, country-targeted, versioned, publishable) +
// library_products (the OFFICIAL regulated price lives on the library
// entry, not the global product — one drug, different official prices per
// country/currency) + library_changes (append-only log powering the
// pharmacy «الفرق منذ آخر مزامنة» diff) + pharmacy_library_syncs
// (per-pharmacy last-synced pointer), and global_products gains
// is_verified + source provenance. Platform-admin endpoints under
// /platform-admin/libraries (+ /products sub-tree) and
// /platform-admin/catalog/products: full CRUD, version publishing, Excel
// bulk import (thousands of products, preview→execute like the pharmacy
// import), every write audited to platform_audit_logs. Pharmacy-side
// import/sync ships in the next api level.
// 76 — pharmacy-side library import & sync (product libraries Phase 2):
// GET /pharmacy/libraries (+ /:id, /:id/products, /:id/diff) list the
// libraries visible to this pharmacy (published + country match/universal)
// with per-library sync state («not_imported | up_to_date |
// update_available» + pending change counts). POST /:id/import/preview →
// /import/execute run the three approved gates: selection, dedup against
// the pharmacy's own products (exact barcode → suggest link; pg_trgm name
// similarity ≥ 0.55 surfaces suspects with side-by-side matches, ≥ 0.72
// auto-links during sync), and the plan product limit as a soft gate
// (skipped items flagged plan_limit — never a silent failure). POST
// /:id/sync pulls only the delta since the pharmacy's last synced version.
// Red lines enforced server-side: inventory batches and stock movements are
// never touched, cost_price is never written, selling_price follows the
// official library price, «removed» library entries are informational only
// — imported products stay owned by the pharmacy. Tenant audit rows
// (libraries.import / libraries.sync) via writeAuditLog.
// 77 — XPay replaces Paymob as the active online gateway (Phase X):
// config-driven provider priority (XPAY_SECRET_KEY + XPAY_PUBLISHABLE_KEY +
// XPAY_WEBHOOK_SECRET ⇒ xpay, else the Phase G paymob subset, else manual
// only). POST /checkout/sessions (api.xpay.app) with Idempotency-Key =
// payments.id and metadata.payment_id anchor; web gets the inline drop-in
// (client_secret + pk), mobile/legacy get the hosted session URL on
// embed_url via ui_mode. POST /payments/webhook/xpay: HMAC-SHA256
// XPay-Signature (t,v1 over the raw body, 300s replay window) + optional
// URL token; fulfil ONLY on paymentStatus=paid from checkout.session.completed
// or async_payment_succeeded; event.id dedup; amount tamper check; expired
// / async_payment_failed flip only pending rows. Paymob webhook stays
// mounted — in-flight paymob payments keep resolving. paymob-diagnostics
// now reports active_gateway + the xpay config subset.
// 79 — Support system (live chat + tickets, Phase T1): migration 33 adds
// support_conversations (per-company chat channel, per-side unread counters
// + read pointers + close semantics), append-only support_messages (system
// rows record ticket lifecycle inside the chat), support_attachments
// (png/jpeg/webp/pdf ≤2MiB inline) and support_tickets (TKT-00001 via
// sequence trigger; open → in_progress → waiting_customer → resolved →
// closed with reopen). WebSocket hub (gorilla/websocket, in-process) pushes
// committed events — message.new / typing / conversation.updated /
// ticket.updated — with REST as the source of truth and client-side polling
// fallback. Both realms get symmetric REST surfaces: company side under
// /pharmacy/support/* (NEVER plan-gated or permission-gated — a suspended
// company must reach support), platform side under /platform-admin/support/*
// with unread/unanswered inbox filters and the audited lifecycle PATCH.
// Quiet-hours email via Brevo when the receiving side has no live socket
// (one mail per 15-minute window per conversation+side, PLATFORM_SUPPORT_EMAIL
// overrides the platform inbox).
// 80 — AUD-003 (HIGH, launch blocker): the three /companies mutation routes
// (PUT /:id, PATCH /:id/status, DELETE /:id) now sit behind auth.CSRF like
// every other authenticated mutation — they were the only exception while
// production cookies are SameSite=None. The admin panel already sends
// X-CSRF-Token on every call, so legitimate clients are unaffected; the
// registration moved into registerCompanyMutationRoutes with a regression
// test asserting the chain.
// 81 — AUD-002 + AUD-004 (HIGH, launch blockers): (1) the HTTP server is
// now built by NewServer — ReadHeaderTimeout/ReadTimeout/WriteTimeout/
// IdleTimeout + MaxHeaderBytes bound every connection phase, MAX_REQUEST_SIZE
// is finally enforced (it existed with zero references) and TRUSTED_PROXIES
// governs X-Forwarded-For so ClientIP is real; main.go drains connections on
// SIGTERM/SIGINT via server.Shutdown. (2) the unauthenticated auth family
// (register/login×2/refresh×2/forgot/reset/verify/resend) sits behind a
// per-IP token bucket (internal/ratelimit) and verify-email gains a
// per-email attempt wall (migration 34 auth_verify_attempts, 10 per 15
// minutes, never reset by resend) — unlimited 6-digit-code probing is over.
// 82 (Task 121, AUD-005+007 HIGH launch-blockers): the destructive-migration
// guard — migrations carrying DROP TABLE/COLUMN, TRUNCATE, DELETE FROM or
// ALTER COLUMN … TYPE are flagged by a startup scanner and BLOCKED in
// production/staging unless ALLOW_DESTRUCTIVE_MIGRATIONS explicitly lists
// them (fresh installs bootstrap unattended; development warns and
// proceeds); every destructive migration that passes the gate first takes
// a best-effort pg_dump snapshot (postgresql client now ships in the image,
// MIGRATION_SNAPSHOT_DIR, failed snapshots abort in production) — a
// destructive schema change can no longer reach production data silently
// or without a snapshot.
// 83 (Task 122, MEDIUM quick-win batch A): (1) T-D2-1 — every provider
// failure-flip on payments carries `AND status = 'pending'` (Paymob
// definitive-failure + amount-tamper branches, XPay amount-tamper branch,
// intent-failure fail-marks) with a source-contract test: a late webhook
// can no longer regress a succeeded payment. (2) T-B6 — the keyless
// pharmacy reads (/sync, /context, /settings GET) sit behind the
// subscription-status lockout via a new SubscriptionGate seam. (3) T-C1 —
// migration versions commit in the same transaction as their DDL (crash
// window boot-loop closed). (4) T-A4 — production refuses the built-in
// development DATABASE_URL. (5) T-A6 — /readyz answers the database
// dependency (fail-closed); /health stays liveness. (6) T-C6 — pool
// tuning knobs (DB_POOL_MAX_CONNS/MIN_CONNS/MAX_CONN_LIFETIME/
// MAX_CONN_IDLE_TIME) applied over the parsed DSN with boot-time logging.
// 84 (Task 123, MEDIUM batch B — T-B1): the shared-catalog write boundary.
// PUT /pharmacy/products/:id was rewriting the SHARED global_products row
// (name, generic name, barcode, strength, dosage form) from tenant context
// with no gate — one pharmacy's "typo fix" rewrites the official identity
// every other tenant and the platform libraries read, and a barcode
// overwrite silently re-points other pharmacies' scans. Provenance
// (migration 31: source + is_verified) now decides: platform-governed rows
// (source='platform_admin' OR verified) are catalog-immutable from pharmacy
// context — the edit endpoint answers 403 catalog_fields_immutable when the
// request actually mutates an identity field (a pure price/packaging edit
// still passes — the change is diffed through the same COALESCE lens the
// GET reads), and both barcode-generation endpoints refuse to mint internal
// codes onto governed rows (single: 403; bulk: per-target skip reason
// catalog_immutable). Crowd-sourced rows ('pharmacy'/'import', never
// verified) stay pharmacy-editable until the per-tenant display-field split
// lands in pharmacy_products.
// 86 (Task 126, deploy blocker): TRUSTED_PROXIES accepts the "*" wildcard as
// the documented trust-all escape hatch for PaaS edges with unknowable shared
// egress — the strict AUD-002 parse treated the star as a malformed IP and
// panicked NewServer at boot, killing the otherwise-green DockHosting
// deployment (build + migrations + bootstrap fine, API dead before listen).
// The star translates to gin's own trust-all CIDRs (0.0.0.0/0 + ::/0) with a
// loud [SECURITY] warning; the default private list keeps ignoring XFF from
// public proxies, an empty value stays fail-closed, and garbage values still
// fail the boot loudly. Behavioral tests lock all four outcomes.
// Task 135 (P0, verify-outage) — the email-verification attempt wall's store
// was erroring on production, and ConsumeEmailVerificationCode mapped that
// error to 400 invalid_code BEFORE any code comparison: every fresh, correct
// code was rejected for every user while the token rows stayed pristine.
// Fix ships on two legs: migration 35 rebuilds the drifted table, and the
// wall now fails open with a loud [verify-wall] log instead of bricking
// verification. api_level 88→89.
//
// Task 138 (P0/P1 audit batch) — migration 36 detaches the ledger tables
// from the org tree (AUD-007: deleting a pharmacy/company/batch can no
// longer erase sales, returns, stock movements, payments, or audit logs),
// CORS exact scheme+host matching with the production wildcard boot-refusal
// (AUD-018), pool session budgets statement/lock/idle-in-tx (AUD-013), gin
// release mode (AUD-016 rest). api_level 89→90.
// Task 139 (P1 audit close-out) — the payments/POS integrity batch finishes
// the Section K P1 row: partial UNIQUE index on provider transaction ids
// (duplicate webhook dies at the DB, AUD-010/032), currency mismatch
// discipline on Paymob + XPay webhooks and the settlement resync (AUD-010),
// constant-time XPay URL-token compare (AUD-010), cross-tenant customer
// ownership inside the sale tx (AUD-009), idempotency key REQUIRED on POS
// sales (AUD-015), /debug/schema behind platform super-admin (AUD-016
// closure). api_level 90→91.
// Task 148 — fair-use budget for outbound email: per-recipient hour/day
// counters (auth_email_send_budget) + platform-wide daily ceiling
// (platform_email_sends) enforced at the dispatch choke points
// (verification + password reset), 429 email_quota_exceeded on the global
// wall, fail-open on store errors. api_level 91→92.
// Task 149 — migrations 34-38 registered in migrationChain: the .sql files
// shipped but the chain never listed them, so NO runner (CI's cmd/migrate or
// production boot) ever applied them — the CI DB-gated regression tests
// (ledger FKs, webhook dedup) failed on fresh databases, and production ran
// without migration 38's budget tables (fail-open masked it). 34/35 adopt
// via legacyMarkers (production tables exist from the manual repair era),
// 36/37/38 execute safely (IF EXISTS/IF NOT EXISTS house style; 36's dynamic
// constraint drop now carries IF EXISTS so the scanner treats it as
// replay-safe). api_level 92→93.
// Task 151 — WebAuthn passkeys (owner-approved architecture): passkey login
// as a password REPLACEMENT (discoverable credentials, UV required), scoped
// single-use step-up grants for sensitive operations (change password, passkey
// management), trusted-device account recovery with announced session
// revocation + security notification, per-IP throttle + audit rows on every
// event. Migration 39 adds passkey_credentials/auth_challenges/
// auth_stepup_grants (public key material only). Password paths untouched.
// api_level 93→94.
// Task 152 — Zero-Permission Fail-Closed: migration 40 adds permissions.tier
// and employees.permissions_initialized (backfill from full history, no DEFAULT
// by design), three-state fail-closed gates in requirePharmacyPermission and
// sync paths, grant guards closing the employees.create escalation chain.
// api_level 94→95.
// Task 153 — SECURITY DEFINER hardening: migration 41 pins search_path on the
// eight SECURITY DEFINER functions, revokes their EXECUTE from PUBLIC and the
// provider's JWT roles, revokes CREATE on schema public, enables deny-by-default
// RLS on every public table lacking it, and locks the four owner-privileged
// views from PUBLIC/JWT reads. The app (owner role) is unaffected; the direct
// PostgREST RPC escalation surface is closed at the database layer.
// api_level 95→96.
// Task 156 — truthful login verdict for deactivated accounts: the IsActive
// veto moves AFTER password proof in Login() (a deactivated employee typing
// CORRECT credentials now gets 403 account_inactive instead of the misleading
// 401 invalid_credentials; wrong-password guesses still get the generic 401
// with no account-state hint — no enumeration oracle). The realm gate keeps
// its veto for Authenticate/Refresh so live sessions of deactivated accounts
// stay dead, and the passkey path is unchanged.
// api_level 96→97.
// M1 (passkeys to Google's measure) — G1: beginning passkey registration now
// demands a fresh manage_passkeys step-up grant (X-Step-Up-Token) — the same
// "Confirm it's you" gate Google puts before adding a sign-in method; the
// password step-up keeps first-passkey registration available to every
// account (adds a factor, removes none). G3: an auth artifact janitor reaps
// expired challenges and dead step-up grants every 10 minutes (migration 42
// adds the supporting indexes). The passkey-as-identity-proof in the
// forgot-password flow remains the existing recovery family
// (/auth/passkeys/recovery/*, decoy-safe); the UI wiring lands in M2.
// api_level 97→98.
// Task 158 — storeChallenge shipped session_data as []byte, which under the
// server's QueryExecModeExec (production pooling mode) is wire-encoded as
// bytea-hex text and the server's text→jsonb cast answers SQLSTATE 22P02:
// EVERY passkey ceremony (register/login/step-up/recovery — all store a
// challenge first) 500'd on production since the family shipped, unseen
// because no UI calls the family yet. session_data now travels as a string
// (the codebase-wide jsonb convention) and the exec-mode regression gate
// pins the server's wire mode in tests. Found by the first real production
// call to the family (M1 field verification); fix proven on PostgreSQL 17.11
// under both exec modes.
// api_level 98→99.
// Task 160 (M3, passkeys on mobile) — the Android app joins the passkey
// family: Credential Manager conveys "android:apk-key-hash:<b64url sha256 of
// the APK signing cert>" as the ceremony origin instead of the https one, so
// PASSKEY_RP_OPAQUE_ORIGINS (comma list, exact-string match per go-webauthn's
// RPOpaqueOrigins) admits native clients next to the web origin. Empty env =
// pre-M3 web-only behavior (fail-closed); the counterpart trust anchor is
// assetlinks.json served from the RP ID domain listing the same cert. No
// migration, no route changes — registration/login/step-up/recovery accept
// the native origin only where configured.
// api_level 99→100.
// P2 (R-10 + R-7, migration 44) — thirteen hot-FK indexes (the audit's §5.3
// list, sale_items.batch_id first among them) + the two actor columns
// (sales.company_user_id, stock_movements.created_by_company_user_id)
// re-aimed from CASCADE to SET NULL with the write-time actor invariant
// moved from CHECK constraints to depth-aware triggers: a hard company
// delete can no longer erase ledger history one hop over, and can no longer
// be aborted by its own referential action. Actor checks pinned by
// TestActorPreserveDB.
// api_level 101→102.
// P3 (R-9, api_level 103) — real observability: the no-op logging
// middleware became a structured slog chain (one JSON line per request
// with method/route-template/status/latency/ip/bytes/request_id, 5xx at
// ERROR), a slog Recovery replaces gin.Recovery (same 500+abort contract,
// panics land in the structured log with stack), RequestID minting/honoring
// with response header echo, Prometheus metrics on the default registry
// (http_requests_total / http_request_duration_seconds / in-flight, labels
// bounded to route templates — never raw paths) served at /metrics behind
// a fail-closed METRICS_TOKEN bearer gate, and a best-effort boot-time
// CREATE EXTENSION pg_stat_statements with an actionable warning when the
// host has not preloaded it.
// api_level 102→103.
// P4 (R-8, api_level 104) — client cache layer + real list paging. The
// backend turns the inventory list's silent LIMIT 500 and the customers
// list's in-query CASE caps into real pages: limit/offset with hard clamps
// (parseListPaging, invalid input falls back to defaults, ceiling 1000),
// a server-side search mirroring the six text fields the inventory page
// used to filter client-side, and a true total over the same WHERE in every
// response (additive keys only — pre-R-8 clients ignore them). No-params
// requests are byte-for-byte the pre-R-8 query with the same defaults
// (inventory 500, customers 200, debts 20). Handler + DB gate share one
// inventoryPage function so the contract cannot drift from its gate.
// api_level 103→104.
// P5 (R-13 + R-14, api_level 105) — the two auth/error hardening rows the
// audit kept open. R-13: refresh-token reuse detection — presenting a
// refresh token that matches an already-REVOKED session (rotated, logged
// out, admin-killed) is the theft signature (an attacker racing the real
// user's rotation); the whole session family is now revoked inside the
// same transaction, the kill commits even though the request is rejected,
// a refresh.reuse_detected audit row lands with family id + kill count,
// and the wire still answers the exact same invalid-token contract. R-14:
// one error envelope (apierr) — a central code registry (status + safe
// message decided once), auth's writeError delegating to it, and the
// "detail": err.Error() leaks (subscriptions/billing/xpay/company binding)
// replaced by log-and-respond: real errors go to the structured log with
// the P3 request_id, static safe messages go to the wire. Pinned by
// TestRefreshReuseDB + apierr envelope gates.
// api_level 104→105.
// 106 — P6 wave (R-2/R-3, the last critical pair): rate limiting extended
// beyond the auth family — per-principal token buckets on
// dashboard/platform-admin/pharmacy/companies (keyed by the session
// identity via MiddlewareForKey, immune to X-Forwarded-For rotation under
// TRUSTED_PROXIES="*") and loose per-IP shields on the signed payment
// webhooks (429-safe by gateway retry + idempotent handlers). The process-
// state decision is now a written single-instance commitment with
// metric-based escalation triggers (docs/adr/ADR-0001) and the remaining
// big decisions are ADR-0002..0004 (closes R-28). api_level 105→106.
// 107 — P7 layer-clarity wave: the layering policy is now a written ADR
// (ADR-0005): business rules + transactional SQL live in internal/services,
// handlers are protocol-only, and NO new SQL may be added inside
// internal/handlers from this level on. First live extraction proves it:
// the platform dashboard aggregate moved verbatim to services.PlatformStats
// (wire-identical, pinned by TestPlatformStatsServiceDB), and
// docs/architecture.md now maps every layer explicitly — service, security
// chain, and the client boundary (the four Next.js apps + the Flutter
// mobile app, which is built only via GitHub Actions into a signed APK and
// is never exported by the server deployment; the frozen wire contract is
// what keeps those installed APKs working across server waves).
// No wire behavior changed in 108 either — the bump marks ledger items 1
// and 7 of ADR-0005 going live (the POS checkout extracted into
// internal/services/pos.go behind the frozen wire contract; the dead
// services/repos/handlers deleted, 18 files / 2196 lines). api_level
// 107→108.
// No wire behavior changed in 109 — the bump marks ledger item 2 of
// ADR-0005 going live: the POS credit-note transaction (idempotent replay,
// FOR UPDATE serialization, largest-remainder refunds, exact-batch restock,
// movement chain, status machine) and the invoice detail read extracted
// verbatim into internal/services/sales_returns.go behind the same frozen
// wire contract, pinned by TestPOSSaleReturnServiceDB and the
// TestPOSReturnContract source gate. api_level 108→109.
// 110 — ADR-0005 ledger item 3: the central libraries split across the
// layer boundary. The platform half (library entry upsert, Excel/CSV
// price-bulletin import, catalog browse/edit) moved to
// internal/services/libraries_platform.go; the pharmacy half (visibility,
// three-gated import, one-tap pull) moved to
// internal/services/libraries_pharmacy.go. The two handler files are
// protocol only (binding, file parsing, the Arabic-reason row validator,
// the frozen stage→code vocabularies); the services own the transactions,
// the catalog dedup, the plan-limit arithmetic, the sync pointer and the
// audit rows — the tenant audit INSERT moved to auditlog.Tenant (the
// canonical single writer, mirroring auditlog.Platform) and
// DrawInternalBarcode/NormalizeImportName moved with their owners.
// Wire contract frozen (Flutter rule): same routes, same bodies, same
// error codes — pinned by TestLibraryPlatformContract /
// TestLibraryPharmacyContract source gates and the
// TestLibraryPlatformServiceDB / TestLibraryPharmacyServiceDB DB gates.
// No migration. api_level 109→110.
//
// Task 161 — the passkey recovery finish was UNREACHABLE in production for
// every account that owns a passkey: the bound begin (relay.BeginLogin →
// session.UserID + AllowedCredentialIDs + a BOUND ledger row) was verified
// by a discoverable finish against two mismatched layers at once — the
// ledger consumer turned its all-empty expectations into typed NULLs
// (IS NOT DISTINCT FROM) which never match a bound row (the failure the
// field actually saw: ErrChallengeInvalid → "التحدي غير صالح…" behind the
// generic frontend message), and the vendor's FinishDiscoverableLogin
// guard rejects any session with a UserID set, which would have fired
// next. Fix: a recovery-only consumer (key + purpose + expiry + single-use,
// binding proven after consumption) plus a bound finish
// (relay.FinishLogin on the account the challenge was issued FOR; the
// vendor re-proves UserID, allow-list and userHandle before the
// signature). Decoys stay consumable-and-dead; cross-account assertions
// are rejected; the round trip is pinned in BOTH exec modes by
// TestRecoveryBoundRoundTripDB / TestRecoveryBoundRoundTripExecModeDB /
// TestRecoveryCrossAccountRejectionDB / TestRecoveryDecoyFinishFailsDB
// (scientific proof: gates go red with the exact production error when the
// fix is reverted). No migration. api_level 110→111.
//
// Task 163 — إدارة الجلسات على نمط جوجل + التُنبيهات البريدية الأمنية
// الكاملة: GET /auth/{realm}/sessions (قائمة الأجهزة الحية، is_current
// يُحسب بالهاش في الاستعلام)، POST /auth/{realm}/sessions/revoke
// (إنهاء جهاز واحد بشرط الملكية)، POST /auth/{realm}/sessions/revoke-others
// (إنهاء كل ما عدا الجلسة الحالية). قراءة من auth_sessions وحدها — لا
// ميجريشن ولا عمود جديد. بريد جديد: welcome (بعد أول تأكيد بريد)،
// new-device (دخول من UA غير مسبوق، fire-and-forget بعد الدخول)، تنبيه
// تغيير/إعادة تعيين كلمة المرور (owner rule #4)، وإعادة صياغة reset
// بالقالب الداكن الموحد. الإضافة endpoints جديدة على عقد السلك →
// api_level 111→112.
//
// Task 164 (migration 45) — عزل شركة المنصة: companies.is_platform يميّز
// قشرة البوتستراب التي تستضيف السوبر أدمين؛ مستثناة من كل قائمة عملاء
// وإحصاء، ومجمّدة أمام كل تعديل فوترة/حالة/حذف بحواري 403 نظيفة يسبقها
// تريجر BEFORE UPDATE/DELETE كخط دفاع أخير حتى بSQL خام.
// api_level 112→113.
// Task 165 — api_level 113→114: باب /platform-admin/system/migrations على
// عقد السلك، والمسار المستأجري /pharmacy/system/migrations صار لأعضاء
// شركة المنصة حصرًا (403 product_dev_tool لأي صيدلية) — أدوات تطوير
// المنتج خرجت من واجهة المستأجرين.
// Task 167 — api_level 114→115 (ميجريشن 46): الدليل الدوائي المصري — أول
// مكتبة منتجات حقيقية (~27 ألف دواء مصري بباركودات GTIN مدققة المجموع
// وأسعار الجملة/التجزئة المرجعية) تُزرع ككتالوج موثق + مكتبة EG/EGP منشورة،
// مع سقف المعاينة الشاملة (400 preview_too_large فوق 2000 مرشح) وحارس
// التوأم الاسمي متضارب الباركود في السحب (Task 166).
// Task 168 — api_level 115→116 (ميجريشن 47 تحليلات المُخطِّط): «استيراد»
// الدليل على مكتبة كاملة يشتغل فعلًا — المعاينة الجمعية تفحص التكرار بأذرع
// مفهرسة (باركود/احتواء في الذاكرة/جسة trgm لكل صف خاص) 60.7s→42ms مقاسًا،
// والتنفيذ الصفري للجمل لكل صنف (مجمّع بالأبراج) 9s محليًا و~27 دقيقة
// مقذوفة على RTT الإنتاج → 0.7s، والسحب مجمّع بحارس مطالبة يمنع إعادة
// توجيه الصف نفسه لمرشحين اثنين، والسلك يتوسع إضافيًا فقط: limit/truncated/
// candidates_total في المعاينة وmode:'all' مع skipped_truncated في التنفيذ —
// العميل القديم يظل يعمل بنفس الأجسام حرفيًا.
// Product ownership («المنتج ملك الصيدلية», migrations 48+49) — the library
// is a reference catalog and the pharmacy's product database is the
// pharmacy's property: local identity overrides on pharmacy_products
// (name/generic/barcode/type/strength/dosage form) replace the T-B1 403 so
// every pharmacy edit — governed or not — stays pharmacy-local and
// global_products is never written from pharmacy context again; effective
// identity flows through POS search/scan, current_inventory, movements,
// reports and dedup matching; sale_items freezes the effective identity at
// sale time (invoice snapshot, documented legacy fallback); and the
// pharmacy can REMOVE an imported library with honest preview criteria
// (sales/returns/stock = kept and detached, trace-free = deleted, shared
// catalog untouched). Pinned by TestProductOwnershipDB /
// TestSaleIdentitySnapshotDB / TestLibraryRemovalDB.
// api_level 116→117.
//
// api_level 118→119 — «الحقايق» على صفحة المخزون: منتجات استيراد المكتبة
// (المُنشأة عمدًا بلا مخزون) كانت محبوسة تمامًا — الغائبون عن view
// current_inventory المبني على التشغيلات، وبلا أي مسار لاستلام مخزون (التعديل
// يتطلب batch_id قائمًا، والإنشاء من جديد يكرر المنتج العام). الموجة: (1)
// GET /pharmacy/inventory?include=unstocked — صف اصطناعي لكل منتج بلا تشغيلة
// (batch_id null، status 'no_stock'، سعر المنتج وتعبئته) خلف باراميتر opt-in
// ليبقى الرد الافتراضي مطابقًا بايت-ببايت (كاش الموبايل يعمل upsert بمفتاح
// batch_id) — pinned بـTestInventoryUnstockedDB (العقد بالمسارين + عزل
// الصيدليات + البحث + الترقيم) وTestInventoryPagingDB (المسار الافتراضي)؛ (2)
// POST /pharmacy/products/:id/opening-stock — أول تشغيلة لمنتج بلا مخزون:
// نفس عقد الدفتر حرفيًا (opening_balance batch + purchase movement بمجموع =
// الكمية المخزنة فبوابة history للتعديل تقبل فورًا)، advisory lock على
// actor+key وFOR UPDATE على صف المنتج يحسمان سباق مفتاحين مختلفين، قواعد
// التعبئة مطابقة للإنشاء (نصف-للأعلى لتقسيم تكلفة الشريط)، replay idempotent
// بنفس نمط التعديل — pinned بـTestOpeningStockDB.
// api_level 120→121 — سرعة دفعات صفحة المخزون (بلاغ المالك: «كل 100 منتج
// بيتحملوا بشكل بطىء شوية») بدون أي تغيير شكل: (1) العدّاد الكامل يُحسم من
// الصفحة الأولى فقط (offset=0) — إعادة العد فوق الاتحاد الكامل مع كل دفعة
// تمرير كانت مضاعفة للعمل بلا مستفيد؛ صفحات الإزاحة الأعلى تعيد total=-1
// كإشارة عقد موثقة والواجهة تقرأه من allPages[0] — كل دفعة تمرير صارت
// استعلامًا واحدًا بدل اثنين (pinned بتحديث TestInventoryPagingDB
// وTestInventoryUnstockedDB)؛ (2) رد الصفحة يضغط gzip تلقائيًا للعميل
// القابله (respondJSON — نمط خريطة الباركود المعمم) فالجسم ينزل عُشر حجمه
// على خط الصيدلية.
// api_level 121→122 — D1+D3+D4+D6 of the catalog audit (migration 53), on
// top of migration 48/49's effective identity and invoice snapshots:
// REAL local products (origin_type; a local row carries NO catalog link and
// its identity lives in the migration-48 override columns, invisible to
// every other pharmacy), the append-only selling-price ledger
// (pharmacy_product_price_history wired into every price writer: create,
// manual edit, import, library sync), POST /pharmacy/products/:id/unlink
// with the confirm gate + historical snapshot freeze + full audit, and the
// exact-barcode fast path in POS search (one btree probe serves a scan; the
// trigram cascade remains the fallback). FORCE RLS / partitioning stay
// deferred by ADR-0002 / ADR-0006; the SaaS control plane foundation is
// ADR-0007. Pinned by TestLocalProductsDB.
// api_level 123→124 — إشعارات الأمان داخل التطبيق: مرآة بريد الأمني في جرس
// اللوحة — ميجريشن 56 (جدول security_notifications) + هوك
// auth.RecordSecurityEventByEmail عند كل موقع بريد أمني (جهاز جديد، إضافة
// مفتاح حيوي، استعادة الحساب، تغيير كلمة المرور) + GET/POST
// /auth/pharmacy/security-notifications(/read).
// api_level 124→125 — إصلاح 500 صفحة المخزون (بلاغ المالك: «مش بتحمل المنتجات
// وفي نفس الوقت بتطلب اعادة تحميل»): migration 53 جعلت global_product_id يقبل
// NULL (المنتجات المحلية origin_type='local') بينما قائمة أعمدة المخزون
// المشتركة كانت تمسح العمود الثالث في نص عادي — أول منتج محلي بتشغيلة (إنشاء
// POS برصيد افتتاحي أو فك ربط يُبقي التشغيلات) يمسح NULL في string بتوقيع pgx
// الموثق «cannot scan NULL into *string» فيموت الرد كله بـ500 بينما CI أخضر
// (لا اختبار يخلق منتجًا محليًا بتشغيلة ثم يستدعي القايمة). الإصلاح COALESCE
// في SQL إلى '' — شكل الرد نص كما كان ويوافق اصطلاح قائمة المنتجات — مع
// تحصين ROUND(COALESCE(min_stock_level,0)) وCOALESCE(gp.id::text,'') في الصف
// الاصطناعي no_stock. Pinned بـTestInventoryLocalProductScanDB
// وTestInventoryUnstockedLocalOpeningDB.
const APILevel = 125

// HealthCheck returns the health status of the API
func (h *Handler) HealthCheck(c *gin.Context) {
        c.JSON(200, gin.H{
                "status":    "healthy",
                "service":   "pharmacy-os-api",
                "api_level": APILevel,
        })
}

// ReadyCheck reports whether the process is serving AND its database
// dependency answers (AUD T-A6). The static /health used to be the only
// probe — the orchestrator (and any uptime monitor pointed at it) saw
// "healthy" while the database was down, which is exactly the silent
// failure the last outage exposed. Fail-closed: no pool wired, or a ping
// that does not answer within 2s, is 503 unready.
func (h *Handler) ReadyCheck(c *gin.Context) {
        if h.db == nil {
                c.JSON(http.StatusServiceUnavailable, gin.H{
                        "status":    "unready",
                        "database":  "unconfigured",
                        "service":   "pharmacy-os-api",
                        "api_level": APILevel,
                })
                return
        }
        ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
        defer cancel()
        if err := h.db.Ping(ctx); err != nil {
                c.JSON(http.StatusServiceUnavailable, gin.H{
                        "status":    "unready",
                        "database":  "down",
                        "service":   "pharmacy-os-api",
                        "api_level": APILevel,
                })
                return
        }
        c.JSON(http.StatusOK, gin.H{
                "status":    "ready",
                "database":  "up",
                "service":   "pharmacy-os-api",
                "api_level": APILevel,
        })
}

// NewServer builds the hardened HTTP server (AUD-002, HIGH): the previous
// `gin.Default() + r.Run()` exposed a server with NO timeouts, NO header
// bound and NO shutdown path — on a 1 vCPU / 1 GB box, slowloris clients
// could pin sockets and goroutines forever, and deploys killed in-flight
// requests ungracefully.
//
//   - ReadHeaderTimeout 10s: shed slow-header (slowloris) clients fast —
//     the FIRST line of defense; a client that cannot send headers within
//     10s is not a real client.
//   - ReadTimeout 60s / WriteTimeout 120s / IdleTimeout 120s: bound every
//     lifecycle phase of a normal request (10MB uploads over slow links
//     still fit comfortably).
//   - MaxHeaderBytes 1MB: the default-bound header bomb.
//   - MaxRequestBody middleware finally ENFORCES config.MaxRequestSize
//     (the setting existed with zero references before this change).
//   - SetTrustedProxies: ClientIP (and therefore the AUD-004 rate-limit
//     keys and the login throttle) only reflect the real caller when
//     X-Forwarded-For is honored exclusively from the listed proxies —
//     without it, spoofed headers mint fresh rate-limit buckets per request.
//     Also closes AUD-017's practical exposure (proxy trust).
//
// The support WebSocket hub is unaffected: gorilla's Upgrader clears the
// server-set deadlines on the hijacked connection and manages its own
// ping/pong budgets.
func (h *Handler) NewServer(addr string) *http.Server {
        // AUD-016 — serve on gin's release mode unless an explicit mode was
        // already chosen (GIN_MODE env or tests' TestMode): debug mode dumps
        // route tables and warning noise on every boot and builds debug
        // trees production never needs.
        if gin.Mode() == gin.DebugMode {
                gin.SetMode(gin.ReleaseMode)
        }
        r := gin.New()
        // R-9 (api_level 103) — the observability chain replaces the
        // gin.Default() text pair: RequestID mints/honors the correlation
        // ID outermost (even panic 500s carry it), Logger emits the one
        // JSON line per request (replacing gin.Logger's text; Recovery is
        // slog-based and lands panics with request_id + stack), Metrics
        // counts and times everything by ROUTE TEMPLATE (bounded labels).
        // gin.Recovery's contract (500 + abort) is preserved by
        // middleware.Recovery — see its comment.
        r.Use(appmiddleware.RequestID(), appmiddleware.Logger(), appmiddleware.Metrics(), appmiddleware.Recovery())
        // Gzip (P3 — موجة سرعة التنقل): ردود الجداول تخرج JSON خامًا
        // 50–300KB على شبكة هي المسيطرة أصلاً (أرضية الطلب 0.6–1s بالقياس
        // الحي 2026-11-18) — الضغط يقصّ ~80% من بايتات النقل لكل صفحة
        // وموبايل بلا تغيير في العقد. استثناءات: websocket و204/304 والثنائي.
        r.Use(appmiddleware.Gzip())
        if strings.TrimSpace(h.config.TrustedProxies) == "*" {
                // TRUSTED_PROXIES="*" is an explicit operator decision to trust every
                // proxy — the escape hatch for PaaS hosts whose shared edge egress is
                // unknowable (the DockHosting boot panic on 2026-10-30: the strict
                // AUD-002 parse rejected the star and killed the API at boot). It
                // translates to gin's own trust-all vocabulary (isUnsafeTrustedProxies
                // checks exactly these CIDRs) and restores ClientIP/X-Forwarded-For
                // behavior for every host whose proxy is not in a private range.
                // Loud warning stands: with the star, a spoofed X-Forwarded-For mints
                // fresh rate-limit buckets until a real CIDR list replaces it.
                log.Println("[SECURITY] TRUSTED_PROXIES=\"*\": every proxy is trusted — X-Forwarded-For is taken at face value. Replace it with the edge proxy CIDR list when possible.")
                if err := r.SetTrustedProxies([]string{"0.0.0.0/0", "::/0"}); err != nil {
                        panic("invalid TRUSTED_PROXIES config: " + err.Error())
                }
        } else if err := r.SetTrustedProxies(splitCIDRs(h.config.TrustedProxies)); err != nil {
                // A malformed TRUSTED_PROXIES value must not take the API
                // down silently half-configured: fail loudly at boot.
                panic("invalid TRUSTED_PROXIES config: " + err.Error())
        }
        r.Use(appmiddleware.MaxRequestBody(h.config.MaxRequestSize))
        h.SetupRoutes(r)
        return &http.Server{
                Addr:              addr,
                Handler:           r,
                ReadHeaderTimeout: 10 * time.Second,
                ReadTimeout:       60 * time.Second,
                WriteTimeout:      120 * time.Second,
                IdleTimeout:       120 * time.Second,
                MaxHeaderBytes:    1 << 20,
        }
}

// splitCIDRs turns the comma-separated TRUSTED_PROXIES value into the list
// gin expects; empty entries are dropped. The "*" wildcard is NOT handled
// here — NewServer treats it as the documented trust-all escape hatch, and
// an empty list (empty env) disables header trust entirely (fail-closed).
func splitCIDRs(value string) []string {
        var out []string
        for _, part := range strings.Split(value, ",") {
                if trimmed := strings.TrimSpace(part); trimmed != "" {
                        out = append(out, trimmed)
                }
        }
        return out
}