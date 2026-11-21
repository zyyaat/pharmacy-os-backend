// Gzip — ضغط ردود JSON على الطبقة النقلية (P3 — موجة سرعة التنقل).
//
// القياس الحي (2026-11-18): أرضية كل طلب إلى الخادم ≈ 0.6–1.0 ثانية حتى على
// 401 — الشبكة هي المسيطرة لا المعالج. ردود الجداول (المخزون/الحركات/السياق)
// تخرج JSON خامًا 50–300KB، والضغط يقصّ ~80% من بايتات النقل على كل صفحة
// وموبايل بلا أي تغيير في العقد أو العميل.
//
// الاستثناءات الصريحة:
//   - ترقيات websocket (support_hub): الغلاف يكسر Hijacker والثنائي الجارٍ
//     لا معنى لضغطه — تمرّ كما هي.
//   - عميل لا يرسل Accept-Encoding: gzip — تمرّ كما هي.
//   - Content-Type غير نصي (تصدير xlsx/pdf/مقاييس prometheus) — تمرّ كما هي.
//   - 204/304 — لا جسم فلا ترويسة ضغط.
//
// القرار (compress أم لا) يُتخذ مرة واحدة عند أول WriteHeader/Write — قبل
// أن يرسل gin الترويسات فعليًا (gin يؤجل الإرسال لأول كتابة، وهذا هو
// الذي يجعل الإعداد المتأخر قبل Write صحيحًا).
//
// الترميم بعد c.Next() إلزامي: Logger/Metrics يقرآن Writer الأصلي بعد عودة
// السلسلة، فأي غلاف بلا ترميم يفسد مقاييس الحجم والسجل المنظم.
package middleware

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// gzipWriterPool يعيد استخدام كتّاب gzip — بناء gzip.Writer يحجز قاموسًا
// ونافذة 32KB، وعلى 1 vCPU التكديس لكل طلب يظهر في ذيل p99.
var gzipWriterPool = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) },
}

// gzipResponseWriter يلف gin.ResponseWriter ويضغط الجسم أول ما يُكتب،
// مع تمرير بقية الواجهة (Hijack/CloseNotify/WriteString/Flush) كما هي.
type gzipResponseWriter struct {
	gin.ResponseWriter
	gz            *gzip.Writer
	neverCompress bool // 204/304 — بلا جسم فلا ضغط ولا ترويسة
	setupDone     bool
	compressible  bool
	started       bool
}

// setup يقفل القرار مرة واحدة عند أول كتابة فعلية — حين يكون Content-Type
// قد ضبطه الهاندلر بالفعل. الدرس من الاختبار: gin's c.Status يستدعي
// WriteHeader مباشرة قبل ضبط النوع، فالقفل عند WriteHeader كان يقرأ نوعًا
// فارغًا فتُضغط الثنائيات (xlsx/pdf) خطأً.
func (g *gzipResponseWriter) setup() {
	if g.setupDone {
		return
	}
	g.setupDone = true
	ct := g.ResponseWriter.Header().Get("Content-Type")
	g.compressible = !g.neverCompress && isCompressibleContentType(ct)
	if g.compressible {
		h := g.ResponseWriter.Header()
		h.Set("Content-Encoding", "gzip")
		h.Add("Vary", "Accept-Encoding")
		// الطول المضغوط يختلف عن الخام — الحذف يفعّل التقطيع الصحيح.
		h.Del("Content-Length")
	}
}

// isCompressibleContentType: JSON هو لغة هذا الـAPI كله؛ النصوص تُضغط
// أيضًا. النوع الفارغ أو غير المعروف = مرور صافي (الافتراضي الأأمن:
// ما لا نعرفه لا نلمسه — metrics/ثنائي/تدفقات).
func isCompressibleContentType(ct string) bool {
	ct = strings.ToLower(ct)
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = strings.TrimRight(ct[:i], " ")
	}
	switch {
	case strings.HasPrefix(ct, "application/json"),
		strings.HasPrefix(ct, "text/"),
		strings.HasPrefix(ct, "application/xml"),
		strings.HasPrefix(ct, "application/javascript"):
		return true
	}
	return false
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	g.setup()
	if !g.compressible {
		return g.ResponseWriter.Write(b)
	}
	g.started = true
	return g.gz.Write(b)
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	if code == http.StatusNoContent || code == http.StatusNotModified {
		g.neverCompress = true
		g.setupDone = true
	}
	g.ResponseWriter.WriteHeader(code)
}

// WriteHeaderNow: التفريغ الضمني عند gin — الحالة 204/304 تُسجل على
// الكاتب الأصلي مباشرة عبر c.Status، فنقرأها من عنده لا من عندها.
func (g *gzipResponseWriter) WriteHeaderNow() {
	if s := g.ResponseWriter.Status(); s == http.StatusNoContent || s == http.StatusNotModified {
		g.neverCompress = true
		g.setupDone = true
	} else {
		g.setup()
	}
	g.ResponseWriter.WriteHeaderNow()
}

// Flush للتدفقات: يغسل gzip أولًا حتى لا تعلق البايتات في نافذة الضغط،
// ثم الطبقة التحتية.
func (g *gzipResponseWriter) Flush() {
	if g.started {
		_ = g.gz.Flush()
	}
	g.ResponseWriter.Flush()
}

// acceptsGzip تحقق تقريبي كافٍ: q-values نادرة عمليًا، ووجود الرمز يكفي —
// gzip مقبول دائمًا في العملاء الحقيقيين (المتصفح وDio).
func acceptsGzip(header string) bool {
	for _, part := range strings.Split(strings.ToLower(header), ",") {
		part = strings.TrimSpace(part)
		if part == "gzip" || strings.HasPrefix(part, "gzip;") || strings.HasPrefix(part, "gzip ") {
			return true
		}
	}
	return false
}

// Gzip يعيد وسيط gin الجاهز للتركيب في السلسلة العامة.
func Gzip() gin.HandlerFunc {
	return func(c *gin.Context) {
		// websocket: يحتاج Hijacker ولا يُضغط — تمرير صامت.
		conn := strings.ToLower(c.Request.Header.Get("Connection"))
		upgrade := strings.ToLower(c.Request.Header.Get("Upgrade"))
		if strings.Contains(conn, "upgrade") && strings.Contains(upgrade, "websocket") {
			c.Next()
			return
		}
		if !acceptsGzip(c.Request.Header.Get("Accept-Encoding")) {
			c.Next()
			return
		}

		gz := gzipWriterPool.Get().(*gzip.Writer)
		underlying := c.Writer
		wrapped := &gzipResponseWriter{
			ResponseWriter: underlying,
			gz:             gz,
		}
		gz.Reset(underlying)

		c.Writer = wrapped

		defer func() {
			if wrapped.started {
				// الإغلاق يكتب تذييل gzip حتى مع panic (Recovery يكمل بعده).
				_ = gz.Close()
			}
			gz.Reset(io.Discard)
			gzipWriterPool.Put(gz)
			c.Writer = underlying
		}()

		c.Next()
	}
}
