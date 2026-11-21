// Package apierr is the centralized error envelope (R-14): one envelope
// shape, one wire-code registry, and one choke point that keeps real error
// details on the server (structured slog with P3's request_id) and never on
// the wire.
//
// Before this package there were two competing shapes: auth's writeError
// emitted {"error": code, "code": UPPER, "message": safe} while several
// platform/billing handlers emitted {"error": code, "detail": err.Error()}
// — leaking driver/SQL details (constraint names, statement fragments,
// driver versions) to any authenticated caller. The registry below makes
// the status and the safe message of every unified code a decision made
// exactly once, and LogAndRespond makes "log the real error, answer a
// static message" the only way a handler may answer an internal failure.
package apierr

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// requestID reads the correlation id the P3 RequestID middleware already
// stamped on the OUTGOING response header (set before c.Next(), so every
// handler sees it). Reading the header instead of middleware's context key
// keeps this package importable from anywhere: middleware imports auth
// (realm wiring), so reaching for middleware.RequestIDFrom here would close
// an auth → apierr → middleware → auth cycle.
func requestID(c *gin.Context) string {
	if c == nil || c.Writer == nil {
		return ""
	}
	return c.Writer.Header().Get("X-Request-ID")
}

const fallbackMessage = "حدث خطأ غير متوقع — أعد المحاولة"

// Definition is the registry entry for one wire code: the HTTP status and
// the safe user-facing message are decided exactly once, here.
type Definition struct {
	Status  int
	Message string
}

func def(status int, message string) Definition { return Definition{Status: status, Message: message} }

// registry — the central error-code registry (R-14). A code emitted through
// Respond/LogAndRespond must live here; anything unregistered falls back to
// internal_error (logged loudly) so a typo can never mint a malformed or
// leaking response. Codes mirror the handlers that now use them; the
// message tone matches the surface (Arabic, matching the API's user-facing
// localization).
var registry = map[string]Definition{
	"internal_error":                 def(http.StatusInternalServerError, fallbackMessage),
	"validation_error":               def(http.StatusBadRequest, "الطلب غير صالح — تحقق من الحقول المرسلة"),
	"subscription_transition_failed": def(http.StatusInternalServerError, "تعذر تحديث الاشتراك الآن — أعد المحاولة"),
	"subscription_insert_failed":     def(http.StatusInternalServerError, "تعذر إنشاء الاشتراك الآن"),
	"company_sync_failed":            def(http.StatusInternalServerError, "تعذر تحديث حالة الشركة"),
	"payment_insert_failed":          def(http.StatusInternalServerError, "تعذر تسجيل الدفعة الآن"),
	"payment_apply_failed":           def(http.StatusInternalServerError, "تعذر تطبيق الدفعة الآن"),
	"settlement_update_failed":       def(http.StatusInternalServerError, "تعذر تحديث التسوية الآن"),
	"settlement_txn_failed":          def(http.StatusInternalServerError, "تعذر إتمام معاملة التسوية"),
	"intent_txn_insert_failed":       def(http.StatusInternalServerError, "تعذر تسجيل عملية الدفع"),
	"entitlement_upsert_failed":      def(http.StatusInternalServerError, "تعذر تحديث صلاحيات الشركة الآن"),
	"entitlement_bundle_failed":      def(http.StatusInternalServerError, "تعذر تحديث حزمة الصلاحيات الآن"),
	"payments_scan_failed":           def(http.StatusInternalServerError, "تعذر قراءة سجل المدفوعات الآن"),
}

// Write emits the unified envelope — the exact shape auth's writeError has
// used since launch: {"error": code, "code": UPPER(code), "message": message}.
// It is the single envelope implementation in the codebase (auth's writeError
// delegates here, so one shape can never drift from the other again).
func Write(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": code, "code": strings.ToUpper(code), "message": message})
}

// Respond emits a registry code with its registered status and safe message.
// An unregistered code falls back to internal_error — never a raw error, and
// never a silent drift from the registry.
func Respond(c *gin.Context, code string) {
	d, ok := registry[code]
	if !ok {
		slog.Error("apierr: unregistered code — falling back to internal_error",
			"code", code, "request_id", requestID(c))
		Write(c, http.StatusInternalServerError, "internal_error", fallbackMessage)
		return
	}
	Write(c, d.Status, code, d.Message)
}

// LogAndRespond logs the real error server-side (slog ERROR, with the route
// and request_id so P3's pipeline can correlate it to the exact request line
// in the container log) and emits the registry envelope. err.Error() never
// reaches the response body — that is the whole point of the wave.
func LogAndRespond(c *gin.Context, code string, err error) {
	d, ok := registry[code]
	if !ok {
		slog.Error("apierr: unregistered code — falling back to internal_error",
			"code", code, "request_id", requestID(c), "err", err)
		Write(c, http.StatusInternalServerError, "internal_error", fallbackMessage)
		return
	}
	slog.Error("api error",
		"code", code,
		"status", d.Status,
		"route", c.FullPath(),
		"request_id", requestID(c),
		"err", err)
	Write(c, d.Status, code, d.Message)
}
