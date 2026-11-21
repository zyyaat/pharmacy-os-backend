package auth

// Task 151 — HTTP surface for the passkey family. Every ceremony runs
// through the shared per-IP throttle; every authenticated mutation carries
// CSRF; the WebAuthn library owns the request body (no ShouldBindJSON on
// finish endpoints), so ceremony metadata (device name, challenge echo)
// travels as query parameters.
//
// M1/G1 (api_level 98) — REGISTRATION IS A SENSITIVE OPERATION. Beginning a
// passkey registration now demands a fresh step-up grant for purpose
// manage_passkeys (X-Step-Up-Token): the passkey path via /stepup/begin+
// /finish, or the current password via /stepup/password. This mirrors
// Google's "Confirm it's you" gate before adding a new sign-in method —
// a hijacked session alone must never be able to mint a new credential for
// itself. The grant is consumed at BEGIN (single-use, owner rule #3); the
// finish call stays challenge-bound, and a challenge can only exist if begin
// already passed the gate. Adds a factor, removes none: the password step-up
// keeps first-passkey registration available to every account.

import (
        "context"
        "errors"
        "log"
        "net/http"
        "strings"

        "github.com/gin-gonic/gin"
        "github.com/jackc/pgx/v5"
        "github.com/jackc/pgx/v5/pgconn"
)

// passkeysEnabledGate answers 503 until a Relying Party identity exists —
// a misconfigured deployment degrades loudly, never half-works.
func (h *Handler) passkeysEnabledGate(c *gin.Context) bool {
        if !h.service.PasskeysEnabled() {
                writeError(c, http.StatusServiceUnavailable, "passkeys_disabled",
                        "Passkey authentication is not configured on this deployment")
                return false
        }
        return true
}

func passkeyChallengeKey(c *gin.Context) string {
        return c.Query("challenge")
}

// RegisterPasskeyRoutes attaches the whole family. `throttle` is the shared
// AUD-004 per-IP limiter already used by the password endpoints.
func (h *Handler) RegisterPasskeyRoutes(authGroup *gin.RouterGroup, throttle gin.HandlerFunc) {
        platform := authGroup.Group("/platform")
        // G1 — the fresh manage_passkeys grant is required (and consumed)
        // at begin; finish remains challenge-bound (no challenge without a
        // gated begin).
        platform.POST("/passkeys/register/begin", h.service.Middleware(PlatformRealm), CSRF(PlatformRealm),
                h.service.RequireStepUp(StepUpPurposeManagePasskeys), h.beginPasskeyRegistration)
        platform.POST("/passkeys/register/finish", h.service.Middleware(PlatformRealm), CSRF(PlatformRealm), h.finishPasskeyRegistration)
        platform.POST("/passkeys/login/begin", throttle, h.beginPasskeyLogin(PlatformRealm))
        platform.POST("/passkeys/login/finish", throttle, h.finishPasskeyLogin(PlatformRealm))
        platform.POST("/stepup/begin", h.service.Middleware(PlatformRealm), CSRF(PlatformRealm), h.beginStepUp)
        platform.POST("/stepup/finish", h.service.Middleware(PlatformRealm), CSRF(PlatformRealm), h.finishStepUp)
        platform.POST("/stepup/password", h.service.Middleware(PlatformRealm), CSRF(PlatformRealm), h.passwordStepUp)
        // الطريقة الثالثة — كود بريد 6 أرقام (حسابات بلا كلمة مرور/مفاتيح):
        // الإرسال والتحقق كلاهما مخنوق لكل IP فوق جلسة صحيحة وCSRF.
        platform.POST("/stepup/email/request", h.service.Middleware(PlatformRealm), CSRF(PlatformRealm), throttle, h.stepUpEmailRequest)
        platform.POST("/stepup/email/verify", h.service.Middleware(PlatformRealm), CSRF(PlatformRealm), throttle, h.stepUpEmailVerify)
        platform.GET("/passkeys", h.service.Middleware(PlatformRealm), h.listPasskeys)
        platform.DELETE("/passkeys/:id", h.service.Middleware(PlatformRealm), CSRF(PlatformRealm),
                h.service.RequireStepUp(StepUpPurposeManagePasskeys), h.deletePasskey)

        pharmacy := authGroup.Group("/pharmacy")
        // G1 — same gate as the platform realm (one policy, two realms).
        pharmacy.POST("/passkeys/register/begin", h.service.Middleware(PharmacyRealm), CSRF(PharmacyRealm),
                h.service.RequireStepUp(StepUpPurposeManagePasskeys), h.beginPasskeyRegistration)
        pharmacy.POST("/passkeys/register/finish", h.service.Middleware(PharmacyRealm), CSRF(PharmacyRealm), h.finishPasskeyRegistration)
        pharmacy.POST("/passkeys/login/begin", throttle, h.beginPasskeyLogin(PharmacyRealm))
        pharmacy.POST("/passkeys/login/finish", throttle, h.finishPasskeyLogin(PharmacyRealm))
        pharmacy.POST("/stepup/begin", h.service.Middleware(PharmacyRealm), CSRF(PharmacyRealm), h.beginStepUp)
        pharmacy.POST("/stepup/finish", h.service.Middleware(PharmacyRealm), CSRF(PharmacyRealm), h.finishStepUp)
        pharmacy.POST("/stepup/password", h.service.Middleware(PharmacyRealm), CSRF(PharmacyRealm), h.passwordStepUp)
        // G1 — same gate as the platform realm (one policy, two realms).
        pharmacy.POST("/stepup/email/request", h.service.Middleware(PharmacyRealm), CSRF(PharmacyRealm), throttle, h.stepUpEmailRequest)
        pharmacy.POST("/stepup/email/verify", h.service.Middleware(PharmacyRealm), CSRF(PharmacyRealm), throttle, h.stepUpEmailVerify)
        pharmacy.GET("/passkeys", h.service.Middleware(PharmacyRealm), h.listPasskeys)
        pharmacy.DELETE("/passkeys/:id", h.service.Middleware(PharmacyRealm), CSRF(PharmacyRealm),
                h.service.RequireStepUp(StepUpPurposeManagePasskeys), h.deletePasskey)

        // Unauthenticated recovery family (owner rule #4): begin/finish run a
        // ceremony; reset spends the grant. All throttled.
        authGroup.POST("/passkeys/recovery/begin", throttle, h.beginPasskeyRecovery)
        authGroup.POST("/passkeys/recovery/finish", throttle, h.finishPasskeyRecovery)
        authGroup.POST("/passkeys/recovery/reset", throttle, h.recoveryPasswordReset)
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

func (h *Handler) beginPasskeyRegistration(c *gin.Context) {
        if !h.passkeysEnabledGate(c) {
                return
        }
        principal, ok := PrincipalFromContext(c)
        if !ok {
                writeError(c, http.StatusUnauthorized, "authentication_required", "Authentication required")
                return
        }
        realm := AuthRealm(c.GetString("auth_realm"))
        accessToken := accessTokenFromRequest(realm, c)
        options, err := h.service.BeginPasskeyRegistration(c.Request.Context(), principal,
                h.service.sessionIDForToken(c.Request.Context(), accessToken))
        if err != nil {
                log.Printf("[PASSKEY] begin registration failed: %v", err)
                writeError(c, http.StatusInternalServerError, "passkey_begin_failed", "Could not start passkey registration")
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": options})
}

func (h *Handler) finishPasskeyRegistration(c *gin.Context) {
        if !h.passkeysEnabledGate(c) {
                return
        }
        principal, ok := PrincipalFromContext(c)
        if !ok {
                writeError(c, http.StatusUnauthorized, "authentication_required", "Authentication required")
                return
        }
        realm := AuthRealm(c.GetString("auth_realm"))
        accessToken := accessTokenFromRequest(realm, c)
        deviceName, err := h.service.FinishPasskeyRegistration(c.Request.Context(), principal,
                h.service.sessionIDForToken(c.Request.Context(), accessToken),
                passkeyChallengeKey(c), c.Request)
        if err != nil {
                h.writePasskeyError(c, err)
                return
        }
	// القاعدة الأمنية نفسها للاستعادة (قاعدة #4): إضافة وسيلة دخول جديدة حدث
	// أمني كامل — بريد الأمان + الهوك الداخلية في جرس الأمان معًا.
	h.sendSecurityAlert(c.Request.Context(), principal.Email,
		"تمت إضافة مفتاح جهاز موثوق جديد («"+deviceName+"») لحسابك. إن لم تكن أنت من أضافه فأنهِ كل الجلسات فورًا من إعدادات الأمان وأعد تعيين كلمة المرور.")
	h.service.RecordSecurityEventAsync(principal.Email, notifKindPasskeyAdded,
		"أُضيف مفتاح أمان جديد («"+deviceName+"»).",
		map[string]any{"device": deviceName})
	c.JSON(http.StatusOK, gin.H{
		"message":     "Passkey registered",
		"device_name": deviceName,
	})
}

// ---------------------------------------------------------------------------
// Discoverable login — same response shape as password login
// ---------------------------------------------------------------------------

func (h *Handler) beginPasskeyLogin(realm AuthRealm) gin.HandlerFunc {
        return func(c *gin.Context) {
                if !h.passkeysEnabledGate(c) {
                        return
                }
                options, err := h.service.BeginPasskeyLogin(c.Request.Context())
                if err != nil {
                        log.Printf("[PASSKEY] begin login failed: %v (cause=%s)", err, pgErrorClass(err))
                        writeErrorCause(c, http.StatusInternalServerError, "passkey_begin_failed", "Could not start passkey login", err)
                        return
                }
                c.JSON(http.StatusOK, gin.H{"data": options})
        }
}

func (h *Handler) finishPasskeyLogin(realm AuthRealm) gin.HandlerFunc {
        return func(c *gin.Context) {
                if !h.passkeysEnabledGate(c) {
                        return
                }
                meta := RequestMeta{IPAddress: c.ClientIP(), UserAgent: c.GetHeader("User-Agent")}
                principal, tokens, err := h.service.FinishPasskeyLogin(c.Request.Context(), realm,
                        passkeyChallengeKey(c), c.Request, meta)
                if err != nil {
                        h.writePasskeyError(c, err)
                        return
                }
                if err := h.setAuthCookies(c, tokens, realm); err != nil {
                        log.Printf("passkey login cookie setup failed: %v", err)
                        writeError(c, http.StatusInternalServerError, "session_error", "Could not create a secure session")
                        return
                }
                c.JSON(http.StatusOK, gin.H{
                        "data": map[string]interface{}{
                                "user":       userPayload(principal),
                                "expires_in": int64(h.service.cfg.AccessTTL.Seconds()),
                        },
                        "user":    userPayload(principal),
                        "message": "Login successful",
                })
        }
}

// ---------------------------------------------------------------------------
// Step-up
// ---------------------------------------------------------------------------

func (h *Handler) beginStepUp(c *gin.Context) {
        if !h.passkeysEnabledGate(c) {
                return
        }
        principal, ok := PrincipalFromContext(c)
        if !ok {
                writeError(c, http.StatusUnauthorized, "authentication_required", "Authentication required")
                return
        }
        var req struct {
                Purpose string `json:"purpose" binding:"required"`
        }
        if err := c.ShouldBindJSON(&req); err != nil {
                writeError(c, http.StatusBadRequest, "validation_error", "A step-up purpose is required")
                return
        }
        realm := AuthRealm(c.GetString("auth_realm"))
        options, err := h.service.BeginStepUp(c.Request.Context(), principal,
                h.service.sessionIDForToken(c.Request.Context(), accessTokenFromRequest(realm, c)), req.Purpose)
        if err != nil {
                h.writePasskeyError(c, err)
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": options})
}

func (h *Handler) finishStepUp(c *gin.Context) {
        if !h.passkeysEnabledGate(c) {
                return
        }
        principal, ok := PrincipalFromContext(c)
        if !ok {
                writeError(c, http.StatusUnauthorized, "authentication_required", "Authentication required")
                return
        }
        // The request body belongs to the WebAuthn library (assertion
        // response) — purpose arrives as a query parameter alongside the
        // challenge echo.
        purpose := c.Query("purpose")
        if purpose == "" {
                writeError(c, http.StatusBadRequest, "validation_error", "A step-up purpose is required")
                return
        }
        realm := AuthRealm(c.GetString("auth_realm"))
        token, err := h.service.FinishStepUp(c.Request.Context(), principal,
                h.service.sessionIDForToken(c.Request.Context(), accessTokenFromRequest(realm, c)),
                purpose, passkeyChallengeKey(c), c.Request)
        if err != nil {
                h.writePasskeyError(c, err)
                return
        }
        c.JSON(http.StatusOK, gin.H{
                "step_up_token": token,
                "expires_in":    300,
        })
}

func (h *Handler) passwordStepUp(c *gin.Context) {
        principal, ok := PrincipalFromContext(c)
        if !ok {
                writeError(c, http.StatusUnauthorized, "authentication_required", "Authentication required")
                return
        }
        var req struct {
                Purpose  string `json:"purpose" binding:"required"`
                Password string `json:"password" binding:"required"`
        }
        if err := c.ShouldBindJSON(&req); err != nil {
                writeError(c, http.StatusBadRequest, "validation_error", "Purpose and password are required")
                return
        }
        token, err := h.service.PasswordStepUp(c.Request.Context(), principal, req.Purpose, req.Password)
        if err != nil {
                if errors.Is(err, ErrInvalidCredentials) {
                        writeError(c, http.StatusUnauthorized, "invalid_password", "Current password is incorrect")
                        return
                }
                h.writePasskeyError(c, err)
                return
        }
        c.JSON(http.StatusOK, gin.H{
                "step_up_token": token,
                "expires_in":    300,
        })
}

// stepUpEmailRequest — الطريقة الثالثة، المرحلة الأولى: إرسال رمز 6 أرقام
// إلى بريد الحساب (لحسابات بلا كلمة مرور وبلا مفاتيح). الرد يخبر العميل
// بالتهدئة المتبقية حتى حين لا يُرسل شيء (نفس عقد Task 131 للتحقق البريدي).
func (h *Handler) stepUpEmailRequest(c *gin.Context) {
        principal, ok := PrincipalFromContext(c)
        if !ok {
                writeError(c, http.StatusUnauthorized, "authentication_required", "Authentication required")
                return
        }
        var req struct {
                Purpose string `json:"purpose" binding:"required"`
        }
        if err := c.ShouldBindJSON(&req); err != nil {
                writeError(c, http.StatusBadRequest, "validation_error", "Purpose is required")
                return
        }
        code, sent, retryAfter, err := h.service.RequestStepUpEmailCode(c.Request.Context(), principal, req.Purpose)
        if err != nil {
                if errors.Is(err, ErrStepUpUnsupported) {
                        writeError(c, http.StatusBadRequest, "stepup_unsupported", "Unsupported step-up purpose")
                        return
                }
                log.Printf("[stepup-email] request failed for principal %s: %v", principal.ID, err)
                writeError(c, http.StatusInternalServerError, "stepup_unavailable", "تعذر إرسال رمز التحقق الآن. حاول بعد لحظات")
                return
        }
        if !sent {
                c.JSON(http.StatusOK, gin.H{
                        "sent":        false,
                        "retry_after": int(retryAfter.Seconds()),
                })
                return
        }
        // نفس ميزانية البريد العامة قبل الإرسال (نفس ترتيب sendVerificationEmail:
        // فشل الإرسال يُبطل الرمز حتى لا يبقى رمزًا حيًا لم يصل).
        if d := h.service.TakeMailBudget(c.Request.Context(), principal.Email); !d.Allowed {
                if invErr := h.service.InvalidateEmailToken(c.Request.Context(), principal, StepUpEmailChallengePurpose); invErr != nil {
                        log.Printf("stepup email token cleanup failed for principal type %s: %v", principal.Type, invErr)
                }
                c.JSON(http.StatusOK, gin.H{"sent": false, "retry_after": int(d.RetryAfter.Seconds())})
                return
        }
        if d := h.service.TakeGlobalMailBudget(c.Request.Context()); !d.Allowed {
                if invErr := h.service.InvalidateEmailToken(c.Request.Context(), principal, StepUpEmailChallengePurpose); invErr != nil {
                        log.Printf("stepup email token cleanup failed for principal type %s: %v", principal.Type, invErr)
                }
                c.JSON(http.StatusOK, gin.H{"sent": false, "retry_after": int(d.RetryAfter.Seconds())})
                return
        }
        messageID, err := h.mailer.stepUpEmail(c.Request.Context(), principal.Email, code)
        if err != nil {
                if invErr := h.service.InvalidateEmailToken(c.Request.Context(), principal, StepUpEmailChallengePurpose); invErr != nil {
                        log.Printf("stepup email token cleanup failed for principal type %s: %v", principal.Type, invErr)
                }
                log.Printf("[stepup-email] send failed for principal %s: %v", principal.ID, err)
                writeError(c, http.StatusInternalServerError, "email_send_failed", "تعذر إرسال البريد الآن. حاول بعد لحظات")
                return
        }
        if messageID != "" {
                log.Printf("step-up code email accepted: to=%s message_id=%s", principal.Email, messageID)
        }
        c.JSON(http.StatusOK, gin.H{"sent": true, "retry_after": int(retryAfter.Seconds())})
}

// stepUpEmailVerify — المرحلة الثانية: الرمز يُقارن بعد ذكر ويُحرق ويسكّ
// منحًا طازجًا بنفس شكل استجابة stepup/password تمامًا.
func (h *Handler) stepUpEmailVerify(c *gin.Context) {
        principal, ok := PrincipalFromContext(c)
        if !ok {
                writeError(c, http.StatusUnauthorized, "authentication_required", "Authentication required")
                return
        }
        var req struct {
                Purpose string `json:"purpose" binding:"required"`
                Code    string `json:"code" binding:"required"`
        }
        if err := c.ShouldBindJSON(&req); err != nil || !isVerificationCode(req.Code) {
                writeError(c, http.StatusBadRequest, "validation_error", "A valid 6-digit code is required")
                return
        }
        token, err := h.service.VerifyStepUpEmailCode(c.Request.Context(), principal, req.Purpose, req.Code)
        if err != nil {
                if errors.Is(err, ErrVerificationLocked) {
                        writeError(c, http.StatusTooManyRequests, "too_many_attempts", "محاولات كثيرة جدًا. انتظر قليلًا ثم أعد المحاولة")
                        return
                }
                if errors.Is(err, ErrInvalidToken) || errors.Is(err, ErrStepUpUnsupported) {
                        writeError(c, http.StatusUnauthorized, "invalid_code", "الرمز غير صحيح أو انتهت صلاحيته")
                        return
                }
                log.Printf("[stepup-email] verify failed for principal %s: %v", principal.ID, err)
                writeError(c, http.StatusInternalServerError, "stepup_unavailable", "تعذر التحقق الآن. حاول بعد لحظات")
                return
        }
        c.JSON(http.StatusOK, gin.H{
                "step_up_token": token,
                "expires_in":    300,
        })
}

// ---------------------------------------------------------------------------
// Management
// ---------------------------------------------------------------------------

func (h *Handler) listPasskeys(c *gin.Context) {
        principal, ok := PrincipalFromContext(c)
        if !ok {
                writeError(c, http.StatusUnauthorized, "authentication_required", "Authentication required")
                return
        }
        rows, err := h.service.ListPasskeys(c.Request.Context(), principal)
        if err != nil {
                log.Printf("[PASSKEY] list failed: %v", err)
                writeError(c, http.StatusInternalServerError, "passkey_list_failed", "Could not list passkeys")
                return
        }
        if rows == nil {
                rows = []PasskeyRow{}
        }
        c.JSON(http.StatusOK, gin.H{"data": rows})
}

func (h *Handler) deletePasskey(c *gin.Context) {
        principal, ok := PrincipalFromContext(c)
        if !ok {
                writeError(c, http.StatusUnauthorized, "authentication_required", "Authentication required")
                return
        }
        deviceName, err := h.service.RevokePasskeyCredential(c.Request.Context(), principal, c.Param("id"))
        if err != nil {
                if errors.Is(err, pgx.ErrNoRows) {
                        writeError(c, http.StatusNotFound, "passkey_not_found", "Passkey not found")
                        return
                }
                if errors.Is(err, ErrLastCredential) {
                        writeError(c, http.StatusConflict, "last_sign_in_method",
                                "لا يمكن إبطال وسيلة الدخول الأخيرة — أضف كلمة مرور أو مفتاحًا آخر أولًا")
                        return
                }
                log.Printf("[PASSKEY] revoke failed: %v", err)
                writeError(c, http.StatusInternalServerError, "passkey_revoke_failed", "Could not revoke passkey")
                return
        }
        c.JSON(http.StatusOK, gin.H{"message": "Passkey revoked", "device_name": deviceName})
}

// ---------------------------------------------------------------------------
// Recovery
// ---------------------------------------------------------------------------

func (h *Handler) beginPasskeyRecovery(c *gin.Context) {
        if !h.passkeysEnabledGate(c) {
                return
        }
        var req struct {
                Email string `json:"email" binding:"required,email"`
        }
        if err := c.ShouldBindJSON(&req); err != nil {
                writeError(c, http.StatusBadRequest, "validation_error", "A valid email is required")
                return
        }
        options, err := h.service.BeginPasskeyRecovery(c.Request.Context(), normalizeEmail(req.Email))
        if err != nil {
                log.Printf("[PASSKEY] recovery begin failed: %v (cause=%s)", err, pgErrorClass(err))
                writeErrorCause(c, http.StatusInternalServerError, "recovery_begin_failed", "Could not start recovery", err)
                return
        }
        c.JSON(http.StatusOK, gin.H{"data": options})
}

func (h *Handler) finishPasskeyRecovery(c *gin.Context) {
        if !h.passkeysEnabledGate(c) {
                return
        }
        meta := RequestMeta{IPAddress: c.ClientIP(), UserAgent: c.GetHeader("User-Agent")}
        token, email, err := h.service.FinishPasskeyRecovery(c.Request.Context(),
                passkeyChallengeKey(c), c.Request, meta)
        if err != nil {
                h.writePasskeyError(c, err)
                return
        }
        // Security notification (rule #4) — best effort, AFTER the audit row,
        // through the same fair-use budget as every other outbound email.
	h.sendSecurityAlert(c.Request.Context(), email,
		"تم التحقق من هويتك عبر مفتاح جهاز موثوق لاستعادة الحساب. أُلغيت جميع الجلسات النشطة (سياسة معلنة). إن لم تكن أنت من طلب ذلك فأعد تعيين كلمة المرور فورًا وتواصل مع الدعم.")
	// الهوك: المرآة داخل التطبيق لنفس الحدث — تصل جرس اللوحة بعد الدخول الجديد.
	h.service.RecordSecurityEventAsync(email, notifKindPasskeyRecovery,
		"تم التحقق من هويتك عبر مفتاح جهاز موثوق لاستعادة الحساب، وأُلغيت جميع الجلسات النشطة.",
		map[string]any{})
	c.JSON(http.StatusOK, gin.H{
                "recovery_token": token,
                "expires_in":     600,
                "message":        "Identity verified. All sessions were revoked; set a new password now.",
        })
}

func (h *Handler) recoveryPasswordReset(c *gin.Context) {
        var req struct {
                Token       string `json:"token" binding:"required"`
                NewPassword string `json:"new_password" binding:"required"`
        }
        if err := c.ShouldBindJSON(&req); err != nil || !validPassword(req.NewPassword) {
                writeError(c, http.StatusBadRequest, "weak_password", passwordPolicyMessage)
                return
        }
        principal, err := h.service.RecoveryPasswordReset(c.Request.Context(), req.Token, req.NewPassword)
        if err != nil {
                if errors.Is(err, ErrChallengeInvalid) {
                        writeError(c, http.StatusBadRequest, "invalid_token", "Invalid or expired recovery token")
                        return
                }
                log.Printf("[PASSKEY] recovery reset failed: %v", err)
                writeError(c, http.StatusInternalServerError, "password_update_failed", "Could not update password")
                return
        }
        _ = principal
        c.JSON(http.StatusOK, gin.H{"message": "Password reset successfully. Please sign in."})
}

// sendSecurityAlert delivers the recovery security notification through the
// same fair-use budget as every other outbound email (Task 148 doctrine:
// a recipient's inbox is the unit of fairness). Best effort — a denied or
// failed send must never fail the security operation itself.
func (h *Handler) sendSecurityAlert(ctx context.Context, email, summary string) {
        if d := h.service.TakeMailBudget(ctx, email); !d.Allowed {
                log.Printf("[mail-budget] security alert denied for %s (retry after %v)", email, d.RetryAfter)
                return
        }
        if d := h.service.TakeGlobalMailBudget(ctx); !d.Allowed {
                log.Printf("[mail-budget] global ceiling reached — security alert skipped for %s", email)
                return
        }
        messageID, err := h.mailer.securityAlertEmail(ctx, email, summary)
        if err != nil {
                log.Printf("security alert email failed for %s: %v", email, err)
                return
        }
        if messageID != "" {
                log.Printf("security alert email accepted: to=%s message_id=%s", email, messageID)
        }
}

// pgErrorClass maps a driver error to a SHORT, secret-free triage class for
// ops surfaces (no messages, no constraint names — just the PostgreSQL SQLSTATE
// family). Production deploys without shell access still need to answer
// "which failure mode is this?" from the outside.
func pgErrorClass(err error) string {
        var pgErr *pgconn.PgError
        if errors.As(err, &pgErr) {
                switch pgErr.Code {
                case "42P01":
                        return "relation_missing"
                case "42703":
                        return "column_missing"
                case "23505":
                        return "unique_violation"
                case "23503":
                        return "fk_violation"
                case "23514":
                        return "check_violation"
                case "22P02":
                        return "invalid_input"
                case "42804", "42883":
                        return "type_mismatch"
                default:
                        return "pg_" + pgErr.Code
                }
        }
        return "internal"
}

// writeErrorCause is writeError plus the secret-free SQLSTATE triage class
// (see pgErrorClass) — enough for outside-in diagnosis, never a raw message.
func writeErrorCause(c *gin.Context, status int, code, message string, err error) {
        c.JSON(status, gin.H{
                "error": code, "code": strings.ToUpper(code), "message": message,
                "cause": pgErrorClass(err),
        })
}

// writePasskeyError maps service errors to stable HTTP contracts.
func (h *Handler) writePasskeyError(c *gin.Context, err error) {
        switch {
        case errors.Is(err, ErrPasskeysDisabled):
                writeError(c, http.StatusServiceUnavailable, "passkeys_disabled", "Passkey authentication is not configured")
        case errors.Is(err, ErrChallengeInvalid):
                writeError(c, http.StatusBadRequest, "challenge_invalid", "التحدي غير صالح أو منتهٍ أو مستهلَك — ابدأ العملية من جديد")
        case errors.Is(err, ErrPasskeyRejected):
                writeError(c, http.StatusUnauthorized, "passkey_rejected", "فشل التحقق من المفتاح الحيوي")
        case errors.Is(err, ErrPasskeyExists):
                writeError(c, http.StatusConflict, "passkey_exists", "هذا المفتاح مسجل مسبقًا")
        case errors.Is(err, ErrNoActivePasskey):
                writeError(c, http.StatusPreconditionFailed, "passkey_unavailable", "لا يوجد مفتاح حيوي نشط على هذا الحساب")
        case errors.Is(err, ErrStepUpUnsupported):
                writeError(c, http.StatusBadRequest, "stepup_purpose_invalid", "Unsupported step-up purpose")
        case errors.Is(err, ErrAccountInactive):
                writeError(c, http.StatusForbidden, "account_inactive", "Account is inactive")
        case errors.Is(err, ErrEmailNotVerified):
                writeError(c, http.StatusForbidden, "email_not_verified", "يرجى تأكيد بريدك الإلكتروني قبل تسجيل الدخول")
        case errors.Is(err, ErrLastCredential):
                writeError(c, http.StatusConflict, "last_sign_in_method", "لا يمكن إبطال وسيلة الدخول الأخيرة")
        default:
                log.Printf("[PASSKEY] internal error: %v (cause=%s)", err, pgErrorClass(err))
                writeErrorCause(c, http.StatusInternalServerError, "passkey_error", "تعذرت عملية المفتاح الحيوي. حاول بعد لحظات", err)
        }
}
