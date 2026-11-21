package middleware

// Unit tests — Gzip (P3 — موجة سرعة التنقل).
//
// العقد: ضغط JSON المضغوط المقبول، مرور الصافي لمن لا يقبل/للثنائي/
// لترقيات websocket، ولا ترويسة ضغط على 204، والpool يعيد الاستخدام.

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func newGzipEngine(handler gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Gzip())
	r.GET("/x", handler)
	return r
}

func bigJSON() string {
	// ~50KB JSON واقعي الشكل (صفوف جداول المخزون/الحركات).
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 500; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"id":1234,"name":"عنصر صيدلية باسم طويل بما يكفي ليكون واقعيًا","qty":42,"price":1550}`)
	}
	b.WriteString("]")
	return b.String()
}

func do(r *gin.Engine, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestGzipCompressesJSONWhenAccepted(t *testing.T) {
	raw := bigJSON()
	r := newGzipEngine(func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json", []byte(raw))
	})
	rec := do(r, map[string]string{"Accept-Encoding": "gzip"})

	if code := rec.Code; code != http.StatusOK {
		t.Fatalf("الكود: %d", code)
	}
	if ce := rec.Header().Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("Content-Encoding = %q، المتوقع gzip", ce)
	}
	if vary := rec.Header().Get("Vary"); !strings.Contains(vary, "Accept-Encoding") {
		t.Fatalf("Vary مفقود: %q", vary)
	}
	if rec.Body.Len() >= len(raw) {
		t.Fatalf("الجسم المضغوط %d ليس أصغر من الخام %d", rec.Body.Len(), len(raw))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("فتح gzip: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("فك الضغط: %v", err)
	}
	if !bytes.Equal(got, []byte(raw)) {
		t.Fatal("المحتوى بعد الفك لا يطابق الأصل")
	}
}

func TestGzipPassesThroughWithoutAcceptEncoding(t *testing.T) {
	raw := bigJSON()
	r := newGzipEngine(func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json", []byte(raw))
	})
	rec := do(r, nil)

	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Fatalf("بلا قبول، Content-Encoding = %q", ce)
	}
	if rec.Body.String() != raw {
		t.Fatal("الجسم يجب أن يمر خامًا")
	}
}

func TestGzipSkips204(t *testing.T) {
	r := newGzipEngine(func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	rec := do(r, map[string]string{"Accept-Encoding": "gzip"})

	if code := rec.Code; code != http.StatusNoContent {
		t.Fatalf("الكود: %d", code)
	}
	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Fatalf("204 بلا جسم فلا ترويسة ضغط، وجدت %q", ce)
	}
}

func TestGzipSkipsBinaryContentType(t *testing.T) {
	payload := []byte("PK\x03\x04-fake-xlsx-bytes")
	r := newGzipEngine(func(c *gin.Context) {
		c.Data(http.StatusOK, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", payload)
	})
	rec := do(r, map[string]string{"Accept-Encoding": "gzip"})

	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Fatalf("الثنائي يمر كما هو، وجدت Content-Encoding=%q", ce)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatal("الجسم الثنائي لا يطابق الأصل")
	}
}

func TestGzipPassesWebsocketUpgrade(t *testing.T) {
	r := newGzipEngine(func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json", []byte(`{"ok":true}`))
	})
	rec := do(r, map[string]string{
		"Accept-Encoding": "gzip",
		"Connection":      "Upgrade",
		"Upgrade":         "websocket",
	})
	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Fatalf("ترقية websocket يجب أن تمر بلا ضغط، وجدت %q", ce)
	}
}

func TestGzipPoolReuseAcrossRequests(t *testing.T) {
	raw := bigJSON()
	r := newGzipEngine(func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json", []byte(raw))
	})
	for i := 0; i < 3; i++ {
		rec := do(r, map[string]string{"Accept-Encoding": "gzip"})
		if ce := rec.Header().Get("Content-Encoding"); ce != "gzip" {
			t.Fatalf("التشغيلة %d: Content-Encoding=%q", i+1, ce)
		}
		zr, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatalf("التشغيلة %d: فتح gzip: %v", i+1, err)
		}
		got, err := io.ReadAll(zr)
		if err != nil || !bytes.Equal(got, []byte(raw)) {
			t.Fatalf("التشغيلة %d: المحتوى غير سليم (err=%v)", i+1, err)
		}
	}
}
