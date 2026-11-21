package apierr

// R-14 gates: the envelope shape is frozen ({"error", "code", "message"} —
// the exact shape auth's writeError has carried since launch), every unified
// code resolves through the registry (unknown codes fall back to
// internal_error rather than inventing a shape), and LogAndRespond never
// lets err.Error() reach the wire — the real error belongs to the structured
// log (P3), the safe message belongs to the client.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func testContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	return c, w
}

func TestWriteEnvelopeShape(t *testing.T) {
	c, w := testContext()
	Write(c, http.StatusTeapot, "some_code", "safe message")
	require.Equal(t, http.StatusTeapot, w.Code)
	require.JSONEq(t, `{"error":"some_code","code":"SOME_CODE","message":"safe message"}`, w.Body.String())
}

func TestRespondResolvesRegistry(t *testing.T) {
	// Every code this wave unified must answer with its registered status.
	cases := map[string]int{
		"validation_error":               http.StatusBadRequest,
		"subscription_transition_failed": http.StatusInternalServerError,
		"subscription_insert_failed":     http.StatusInternalServerError,
		"company_sync_failed":            http.StatusInternalServerError,
		"payment_insert_failed":          http.StatusInternalServerError,
		"payment_apply_failed":           http.StatusInternalServerError,
		"settlement_update_failed":       http.StatusInternalServerError,
		"settlement_txn_failed":          http.StatusInternalServerError,
		"intent_txn_insert_failed":       http.StatusInternalServerError,
	}
	for code, wantStatus := range cases {
		c, w := testContext()
		Respond(c, code)
		require.Equal(t, wantStatus, w.Code, "code %s", code)
		body := w.Body.String()
		require.Contains(t, body, `"error":"`+code+`"`, "code %s", code)
		require.Contains(t, body, `"code":"`+strings.ToUpper(code)+`"`, "code %s", code)
		require.NotContains(t, body, "detail", "code %s — the leak shape is retired", code)
	}
}

func TestRespondUnknownCodeFallsBack(t *testing.T) {
	c, w := testContext()
	Respond(c, "typo_code_that_never_registered")
	require.Equal(t, http.StatusInternalServerError, w.Code)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(w.Body.String()), &parsed))
	require.Equal(t, "internal_error", parsed["error"])
	require.NotContains(t, parsed, "typo_code_that_never_registered")
}

func TestLogAndRespondNeverLeaksError(t *testing.T) {
	c, w := testContext()
	// Task 163 أمنيًا — كان هنا DSN حقيقي بكلمة مرور إنتاج فعلية (سُجّل في
	// تاريخ git)؛ استُبدل بعلامة وهمية بنفس الشكل: الاختبار يثبت الدلالة
	// نفسها (err.Error() لا يصل السلك أبدًا) بلا أي سر حقيقي في الريبو.
	marker := "dbconn: secret-password=EXAMPLE-ROTATED-SECRET-PLACEHOLDER host=db-internal"
	LogAndRespond(c, "payment_insert_failed", errors.New(marker))
	require.Equal(t, http.StatusInternalServerError, w.Code)
	body := w.Body.String()
	require.NotContains(t, body, "EXAMPLE-ROTATED-SECRET-PLACEHOLDER", "raw error text must never reach the wire")
	require.NotContains(t, body, "dbconn", "driver detail must never reach the wire")
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(w.Body.String()), &parsed))
	require.Equal(t, "payment_insert_failed", parsed["error"])
	require.NotEmpty(t, parsed["message"], "safe message present")
}

func TestLogAndRespondUnknownCodeFallsBack(t *testing.T) {
	c, w := testContext()
	LogAndRespond(c, "no_such_code", errors.New("internal marker"))
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.NotContains(t, w.Body.String(), "internal marker")
	require.Contains(t, w.Body.String(), `"error":"internal_error"`)
}
