package auth

import (
        "context"
        "log"

        "github.com/pharmacy-os/backend/internal/auditlog"
)

// T-B5 — authentication audit events. Every terminal path of Service.Login
// writes exactly one row into platform_audit_logs through the shared
// auditlog writer: login.success, login.failed (with a machine reason) and
// account.locked (the 5-attempt lock transition). These events are
// best-effort by design — a broken audit feed must never block a login or
// change its response, so failures are logged loudly and swallowed.
//
// The events go to the platform table, not the tenant audit_logs table:
// a failed login may carry no pharmacy at all (unknown email), and the
// platform operator needs the cross-account security picture (brute force
// against one pharmacy email, lock transitions, realm probing).

const (
        authReasonInvalidCredentials = "invalid_credentials"
        authReasonRealmMismatch      = "realm_mismatch"
        authReasonAccountLocked      = "account_locked"
        authReasonAccountInactive    = "account_inactive"
        authReasonEmailNotVerified   = "email_not_verified"
        authReasonRateLimited        = "rate_limited"
        authReasonRefreshReuse       = "refresh_reuse"
)

// auditLoginSuccess records a completed login (credentials verified, session
// issued). company_id is set for company users so the per-company logs page
// shows the account's sign-ins; pharmacy users carry their pharmacy_id in
// metadata instead (platform_audit_logs has no pharmacy column).
func (s *Service) auditLoginSuccess(ctx context.Context, principal *Principal, realm AuthRealm, meta RequestMeta) {
        s.auditAuthEvent(ctx, principal, principal.Email, realm, meta,
                "login.success", "", "info", "تسجيل دخول ناجح", nil)
}

// auditLoginFailure records a rejected login attempt. principal may be nil
// (unknown email — only the attempted address is kept). A failure tied to a
// real account is a targeted-attack signal (severity warning); scanner noise
// against unknown emails stays info.
func (s *Service) auditLoginFailure(ctx context.Context, principal *Principal, attemptedEmail string, realm AuthRealm, meta RequestMeta, reason string) {
        summary := "محاولة دخول فاشلة"
        switch reason {
        case authReasonInvalidCredentials:
                summary = "محاولة دخول فاشلة — بيانات غير صحيحة"
        case authReasonRealmMismatch:
                summary = "محاولة دخول فاشلة — الحساب غير مسموح في هذا النطاق"
        case authReasonAccountLocked:
                summary = "محاولة دخول أثناء قفل الحساب المؤقت"
        case authReasonAccountInactive:
                summary = "محاولة دخول لحساب موقوف"
        case authReasonEmailNotVerified:
                summary = "محاولة دخول قبل توثيق البريد الإلكتروني"
        case authReasonRateLimited:
                summary = "محاولة دخول مصانة — تجاوز حد المحاولات المسموح"
        }
        severity := "info"
        if principal != nil {
                severity = "warning"
        }
        s.auditAuthEvent(ctx, principal, attemptedEmail, realm, meta,
                "login.failed", reason, severity, summary, nil)
}

// auditAccountLocked records the lock transition itself — fired exactly when
// recordFailedLogin flips locked_until (5th consecutive wrong password),
// never for attempts that merely hit an already-locked account (those are
// login.failed with reason=account_locked).
func (s *Service) auditAccountLocked(ctx context.Context, principal *Principal, realm AuthRealm, meta RequestMeta) {
        s.auditAuthEvent(ctx, principal, principal.Email, realm, meta,
                "account.locked", authReasonInvalidCredentials, "warning",
                "قفل مؤقت للحساب بعد 5 محاولات دخول فاشلة — يُرفع تلقائيًا بعد 15 دقيقة", nil)
}

// auditRefreshReuse records R-13's terminal signal: a revoked (rotated,
// logged-out, or admin-killed) refresh token was presented again — the theft
// signature — and the whole session family was revoked in response. The
// principal row is deliberately NOT re-loaded (the endpoint must not grow a
// lookup for a rejected token): its coordinates ride in metadata alongside
// the family id and the number of live sessions the kill took down.
func (s *Service) auditRefreshReuse(ctx context.Context, familyID, principalType, principalID string, killed int64, realm AuthRealm, meta RequestMeta) {
        principal := &Principal{ID: principalID, Type: principalType}
        s.auditAuthEvent(ctx, principal, "", realm, meta,
                "refresh.reuse_detected", authReasonRefreshReuse, "warning",
                "إعادة استخدام refresh token مُبطل — عائلة الجلسة أُبطلت بالكامل (R-13)",
                map[string]any{"family_id": familyID, "sessions_killed": killed})
}

// auditAuthEvent is the single choke point: builds the Actor, the metadata
// blob and routes the row through auditlog.Platform on the pool (auditDB
// replaces it in tests). extraMeta (usually nil) merges caller-specific
// fields into the metadata blob without widening every call site.
func (s *Service) auditAuthEvent(ctx context.Context, principal *Principal, attemptedEmail string, realm AuthRealm, meta RequestMeta, action, reason, severity, summary string, extraMeta map[string]any) {
        db := s.auditDB
        if db == nil {
                if s.db == nil {
                        // Defensive: unit tests construct Service{} without a pool; never
                        // let auditing panic there. (Assigning the nil *pgxpool.Pool to
                        // the Execer interface would produce a non-nil interface holding
                        // nil — the classic Go trap — hence the explicit check here.)
                        return
                }
                db = s.db
        }
        actor := auditlog.Actor{Email: attemptedEmail}
        entityID := ""
        companyID := ""
        metadata := map[string]any{
                "realm":      string(realm),
                "ip_address": meta.IPAddress,
                "user_agent": meta.UserAgent,
        }
        if reason != "" {
                metadata["reason"] = reason
        }
        for k, v := range extraMeta {
                metadata[k] = v
        }
        if principal != nil {
                actor = auditlog.Actor{
                        ID:          principal.ID,
                        Email:       principal.Email,
                        DisplayName: principal.DisplayName,
                        Role:        principal.Role,
                }
                entityID = principal.ID
                metadata["principal_type"] = principal.Type
                if principal.CompanyID != "" {
                        companyID = principal.CompanyID
                        metadata["company_id"] = principal.CompanyID
                }
                if principal.PharmacyID != "" {
                        metadata["pharmacy_id"] = principal.PharmacyID
                }
        }
        if err := auditlog.Platform(ctx, db, actor,
                action, "authentication", "account", entityID, companyID, severity,
                metadata, summary); err != nil {
                log.Printf("[AUTH AUDIT] %s write failed: %v", action, err)
        }
}
