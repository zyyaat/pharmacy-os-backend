package auth

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// Task 163 — معالجات إدارة الجلسات على نمط جوجل. كلها خلف Middleware
// (المستدعى مصادق بالضرورة) وعلى نفس الكوكيز، فلا توكن جديد ولا نمط
// إثبات جديد. الإبطال لا يطلب step-up: إنهاء جهاز من صفحتك لا يمنح
// مهاجمًا شيئًا جديدًا (أصلًا هو داخل حسابه)، بينما إضافة مفتاح أو
// تغيير كلمة مرور يظل خلف البوابة الأشد كما هو.

// sessionsListHandler → GET /auth/{realm}/sessions
func (h *Handler) sessionsListHandler(c *gin.Context) {
	principal, ok := PrincipalFromContext(c)
	if !ok {
		writeError(c, http.StatusUnauthorized, "authentication_required", "Authentication required")
		return
	}
	realm := AuthRealm(c.GetString("auth_realm"))
	token := accessTokenFromRequest(realm, c)
	if token == "" {
		writeError(c, http.StatusUnauthorized, "authentication_required", "Authentication required")
		return
	}
	sessions, err := h.service.ListSessions(c.Request.Context(), principal, token)
	if err != nil {
		log.Printf("sessions list failed for principal type %s: %v", principal.Type, err)
		writeError(c, http.StatusInternalServerError, "sessions_list_failed", "Could not load your devices")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": sessions})
}

// sessionRevokeHandler → POST /auth/{realm}/sessions/revoke  {session_id}
// (جسم الطلب بدل مسار wildcard: gin يمنع تعارض :id مع static على نفس الفرع).
func (h *Handler) sessionRevokeHandler(c *gin.Context) {
	principal, ok := PrincipalFromContext(c)
	if !ok {
		writeError(c, http.StatusUnauthorized, "authentication_required", "Authentication required")
		return
	}
	var req struct {
		SessionID string `json:"session_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "validation_error", "A session_id is required")
		return
	}
	if _, err := h.service.RevokeSessionByID(c.Request.Context(), principal, req.SessionID); err != nil {
		log.Printf("session revoke failed for principal type %s: %v", principal.Type, err)
		writeError(c, http.StatusInternalServerError, "session_revoke_failed", "Could not end that session")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Session ended"})
}

// sessionsRevokeOthersHandler → POST /auth/{realm}/sessions/revoke-others
func (h *Handler) sessionsRevokeOthersHandler(c *gin.Context) {
	principal, ok := PrincipalFromContext(c)
	if !ok {
		writeError(c, http.StatusUnauthorized, "authentication_required", "Authentication required")
		return
	}
	realm := AuthRealm(c.GetString("auth_realm"))
	token := accessTokenFromRequest(realm, c)
	if token == "" {
		writeError(c, http.StatusUnauthorized, "authentication_required", "Authentication required")
		return
	}
	ended, err := h.service.RevokeOtherSessions(c.Request.Context(), principal, token)
	if err != nil {
		log.Printf("revoke-others failed for principal type %s: %v", principal.Type, err)
		writeError(c, http.StatusInternalServerError, "session_revoke_failed", "Could not end other sessions")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Other sessions ended", "ended": ended})
}

// ─── التُنبيهات البريدية غير المتزامنة (نمط last_used_at: خارج مسار الرد) ───

// notifyNewDeviceAsync يُستدعى بعد نجاح دخول بكلمة المرور. يفحص في خلفية
// منفصلة إن كان UA غير مسبوق لهذا الحساب فيرسل بريد «دخول جديد» — بريد
// جوجل الأشهر: لا يوقف الدخول أبدًا، وفشله يهبط لوحده في اللوج فقط.
func (h *Handler) notifyNewDeviceAsync(principal *Principal, userAgent, ip string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[SECURITY-MAIL] new-device panic recovered: %v", r)
		}
	}()
	if !h.mailer.configured() || userAgent == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	known, err := h.service.IsKnownDevice(ctx, principal, userAgent)
	if err != nil {
		// فشل الفحص = لا بريد (نتحفظ على إزعاج وهمي ولا نكشف حارسًا صامتًا).
		log.Printf("[SECURITY-MAIL] known-device probe failed for principal type %s: %v", principal.Type, err)
		return
	}
	if known {
		return
	}
	browser, os, kind := parseUserAgent(userAgent)
	device := describeUserAgent(browser, os, kind)
	// الهوك: نفس حدث البريد يُسجل داخل التطبيق (جرس الأمان) — بلا أي ارتباط
	// بمزود البريد؛ المرآة تعمل حتى لو كان البريد غير مكوّن.
	h.service.RecordSecurityEventAsync(principal.Email, notifKindNewDevice,
		"تسجيل دخول من جهاز جديد: "+device+". إن لم تكن أنت فأنهِ الجلسة فورًا من إعدادات الأمان.",
		map[string]any{"device": device, "ip": ip})
	if _, err := h.mailer.newDeviceEmail(ctx, principal.Email, device, ip); err != nil {
		log.Printf("[SECURITY-MAIL] new-device email failed for %s: %v", principal.Email, err)
	}
}

// securityNoticeAsync يُستدعى بعد نجاح تغيير/إعادة تعيين كلمة المرور —
// الحدث الأمني الأول الذي يجب أن يصل صاحب البريد حتى لو لم يكن هو الفاعل
// (owner rule #4 نفسها التي حكمت تنبيهات المفاتيح).
func (h *Handler) securityNoticeAsync(principal *Principal, summary string) {
	// الهوك: مرآة تغيير/إعادة تعيين كلمة المرور داخل التطبيق — قبل بوابة
	// تكوين البريد عمدًا، فالإشعار الداخلي لا يعتمد على مزود البريد إطلاقًا.
	h.service.RecordSecurityEventAsync(principal.Email, notifKindPasswordChanged, summary, map[string]any{"type": principal.Type})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[SECURITY-MAIL] security-notice panic recovered: %v", r)
			}
		}()
		if !h.mailer.configured() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := h.mailer.securityAlertEmail(ctx, principal.Email, summary); err != nil {
			log.Printf("[SECURITY-MAIL] security notice failed for %s: %v", principal.Email, err)
		}
	}()
}

// welcomeEmailAsync يُستدعى بعد نجاح التحقق من البريد لأول مرة — لحظة
// «أهلًا بك» التي تصنع انطباع التسجيل الجديد (نمط جوجل: تحقق ثم ترحيب).
func (h *Handler) welcomeEmailAsync(principal *Principal) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[SECURITY-MAIL] welcome panic recovered: %v", r)
			}
		}()
		if !h.mailer.configured() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := h.mailer.welcomeEmail(ctx, principal.Email, principal.DisplayName); err != nil {
			log.Printf("[SECURITY-MAIL] welcome email failed for %s: %v", principal.Email, err)
		}
	}()
}

// securityNotificationsListHandler → GET /auth/{realm}/security-notifications
// مرآة بريد الأمان داخل التطبيق: آخر 20 حدثًا لصاحب الحساب + عدد غير المقروء
// لشارة الجرس. شخصية بالكامل — مقيّدة بprincipal.ID في طبقة الخدمة.
func (h *Handler) securityNotificationsListHandler(c *gin.Context) {
	principal, ok := PrincipalFromContext(c)
	if !ok {
		writeError(c, http.StatusUnauthorized, "authentication_required", "Authentication required")
		return
	}
	items, unread, err := h.service.ListSecurityNotifications(c.Request.Context(), principal)
	if err != nil {
		log.Printf("security notifications list failed for principal type %s: %v", principal.Type, err)
		writeError(c, http.StatusInternalServerError, "security_notifications_failed", "Could not load your security notifications")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"items": items, "unread": unread}})
}

// securityNotificationsReadHandler → POST /auth/{realm}/security-notifications/read
// «تعليم الكل كمقروء» — تحديث واحد بلا أجسام معقدة، والعدّاد يعود صفرًا.
func (h *Handler) securityNotificationsReadHandler(c *gin.Context) {
	principal, ok := PrincipalFromContext(c)
	if !ok {
		writeError(c, http.StatusUnauthorized, "authentication_required", "Authentication required")
		return
	}
	if _, err := h.service.MarkSecurityNotificationsRead(c.Request.Context(), principal); err != nil {
		log.Printf("security notifications read failed for principal type %s: %v", principal.Type, err)
		writeError(c, http.StatusInternalServerError, "security_notifications_failed", "Could not update your notifications")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "marked", "unread": 0})
}
