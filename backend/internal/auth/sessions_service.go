package auth

import (
	"context"
	"strings"
	"time"
)

// Task 163 — إدارة الجلسات على نمط جوجل: المستخدم يرى كل جهاز يحمل جلسة
// حية على حسابه، وينهي أي جهاز منها أو كل ما عدا جهازه الحالي. القراءة
// من auth_sessions وحدها (لا ميجريشن ولا عمود جديد): الجلسة تحمل أصلًا
// user_agent وip_address وlast_used_at، والإبطال نفس الميكانيكية التي
// يستعملها logout/logout-all — UPDATE revoked_at، فلا مساس بالعقد السلكي
// ولا بتوكنات حية غير المطلوب إبطالها.

// SessionInfo — صف واحد في قائمة «أجهزتك». الجلسة الملغاة لا تظهر أبدًا:
// القائمة هي الأجهزة الحية فقط، تاريخ الإبطال ليس بيانات هذه الشاشة.
type SessionInfo struct {
	ID         string    `json:"id"`
	Realm      string    `json:"auth_realm"`
	Browser    string    `json:"browser"`
	OS         string    `json:"os"`
	Kind       string    `json:"kind"` // desktop | mobile | tablet | unknown
	Device     string    `json:"device"`
	IPAddress  string    `json:"ip_address"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	IsCurrent  bool      `json:"is_current"`
}

// ListSessions يقرأ الجلسات الحية لصاحب الحساب (كل العوالم: جوجل تعرض كل
// أجهزتك بغض النظر عن التطبيق). الحيّة = غير ملغاة ورفريشها لم ينتهِ —
// جلسة رفريشها منتهٍ ميتة عمليًا فإظهارها يربك المالك بلا فائدة أمنية.
// is_current يُحسب بالهاش مباشرة في الاستعلام: التوكن الحقيقي لا يغادر
// الذاكرة، وهاشه وحده هو ما يعرف القاعدة.
func (s *Service) ListSessions(ctx context.Context, principal *Principal, currentAccessToken string) ([]SessionInfo, error) {
	currentHash := tokenHash(currentAccessToken)
	rows, err := s.db.Query(ctx, `
		SELECT id::text, auth_realm, COALESCE(user_agent, ''), COALESCE(ip_address::text, ''),
		       created_at, last_used_at, (access_token_hash = $3)
		FROM auth_sessions
		WHERE principal_type = $1 AND principal_id = $2
		  AND revoked_at IS NULL AND refresh_expires_at > NOW()
		ORDER BY last_used_at DESC
	`, principal.Type, principal.ID, currentHash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sessions := make([]SessionInfo, 0, 8)
	for rows.Next() {
		var info SessionInfo
		var ua string
		if err := rows.Scan(&info.ID, &info.Realm, &ua, &info.IPAddress,
			&info.CreatedAt, &info.LastUsedAt, &info.IsCurrent); err != nil {
			return nil, err
		}
		info.Browser, info.OS, info.Kind = parseUserAgent(ua)
		info.Device = describeUserAgent(info.Browser, info.OS, info.Kind)
		sessions = append(sessions, info)
	}
	return sessions, rows.Err()
}

// RevokeSessionByID ينهي جلسة واحدة بشرط الملكية: session_id يجب أن يعود
// لنفس الـprincipal مهما كان عالمه. الجلسة غير الموجودة أو الملغاة سابقًا
// تُرد idempotent بنجاح — الإبطال الثانوي بلا أثر ولا معنى.
// تنبيه المستخدم لا يُرسل هنا: إنهاء جهاز من صفحته لا يستحق بريدًا —
// بريد التغيير محفوظ لأحداث الحساب (كلمة المرور/الجهاز الجديد).
func (s *Service) RevokeSessionByID(ctx context.Context, principal *Principal, sessionID string) (bool, error) {
	tag, err := s.db.Exec(ctx, `
		UPDATE auth_sessions SET revoked_at = NOW()
		WHERE id = $1 AND principal_type = $2 AND principal_id = $3 AND revoked_at IS NULL
	`, sessionID, principal.Type, principal.ID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// RevokeOtherSessions ينهي كل جلسات المالك عدا الجلسة الحالية (زر جوجل
// «تسجيل الخروج من الأجهزة الأخرى»). التوكن الحالي لا يُمس فلا يفاجأ
// المستخدم بخروج من شاشته هو نفسه. يرجع عدد ما أُنهي.
func (s *Service) RevokeOtherSessions(ctx context.Context, principal *Principal, currentAccessToken string) (int64, error) {
	tag, err := s.db.Exec(ctx, `
		UPDATE auth_sessions SET revoked_at = NOW()
		WHERE principal_type = $1 AND principal_id = $2
		  AND revoked_at IS NULL
		  AND access_token_hash <> $3
		  AND refresh_token_hash <> $4
	`, principal.Type, principal.ID, tokenHash(currentAccessToken), tokenHash(currentAccessToken))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// IsKnownDevice يجيب: هل هذا user_agent رآه الحساب من قبل في أي جلسة؟
// يُسأل بعد ولادة الجلسة الجديدة، فالمرجع يستثني أحدث جلسة (غالبًا هذه
// نفسها) — دوران الرفريش يولّد صفوفًا متتالية بنفس UA فلا يرسل بريدًا
// مكررًا، بينما UA غير مسبوق يعني جهازًا جديدًا فعلًا.
func (s *Service) IsKnownDevice(ctx context.Context, principal *Principal, userAgent string) (bool, error) {
	var known bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM auth_sessions
			WHERE principal_type = $1 AND principal_id = $2
			  AND user_agent IS NOT DISTINCT FROM $3
			  AND id <> (
			    SELECT id FROM auth_sessions
			    WHERE principal_type = $1 AND principal_id = $2
			    ORDER BY created_at DESC LIMIT 1
			  )
		)
	`, principal.Type, principal.ID, userAgent).Scan(&known)
	return known, err
}

// parseUserAgent يفكّ المتصفح والنظام وفئة الجهاز من ترويسة UA — بلا
// مكتبة خارجية: دقة «اسم يفهمه المالك» تكفي هذه الشاشة، والـUA المبهم
// أو المحذوف يظهر «جهاز غير معروف» لا خطأ.
func parseUserAgent(ua string) (browser, os, kind string) {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return "غير معروف", "غير معروف", "unknown"
	}
	kind = "desktop"
	if strings.Contains(ua, "Mobile") || strings.Contains(ua, "iPhone") ||
		strings.Contains(ua, "Android") && strings.Contains(ua, "Mobile") {
		kind = "mobile"
	}
	if strings.Contains(ua, "iPad") || (strings.Contains(ua, "Android") && !strings.Contains(ua, "Mobile")) {
		kind = "tablet"
	}
	switch {
	case strings.Contains(ua, "Edg/"):
		browser = "Microsoft Edge"
	case strings.Contains(ua, "OPR/") || strings.Contains(ua, "Opera"):
		browser = "Opera"
	case strings.Contains(ua, "SamsungBrowser"):
		browser = "Samsung Internet"
	case strings.Contains(ua, "Firefox"):
		browser = "Firefox"
	case strings.Contains(ua, "Chrome") || strings.Contains(ua, "CriOS"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari") && strings.Contains(ua, "Version/"):
		browser = "Safari"
	default:
		browser = "متصفح آخر"
	}
	switch {
	case strings.Contains(ua, "Windows"):
		os = "Windows"
	case strings.Contains(ua, "Android"):
		os = "Android"
	case strings.Contains(ua, "iPhone") || strings.Contains(ua, "iPad"):
		os = "iOS"
	case strings.Contains(ua, "Mac OS X") || strings.Contains(ua, "Macintosh"):
		os = "macOS"
	case strings.Contains(ua, "Linux") || strings.Contains(ua, "X11"):
		os = "Linux"
	default:
		os = "نظام غير معروف"
	}
	return browser, os, kind
}

// describeUserAgent يجمع الصف الثلاثي في جملة عربية واحدة تظهر في القائمة
// وفي بريد «دخول جديد» — نفس النص في الشاشتين حتى لا يلتبس على المالك.
func describeUserAgent(browser, os, kind string) string {
	noun := "جهاز كمبيوتر"
	switch kind {
	case "mobile":
		noun = "هاتف"
	case "tablet":
		noun = "لوحي"
	}
	return browser + " على " + os + " (" + noun + ")"
}
