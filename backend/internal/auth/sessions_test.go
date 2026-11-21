package auth

import (
	"testing"
)

// Task 163 — بوابات وحدات لتحليل User-Agent وصياغة وصف الجهاز. القرار
// المطبّق هنا هو نفسه الذي يظهر في قائمة «أجهزتك» وفي بريد «دخول جديد»،
// فالتمييز الصحيح بين المتصفح/النظام/الفئة هو عقد الشاشتين معًا.

func TestParseUserAgentKnownDevices(t *testing.T) {
	cases := []struct {
		name, ua, browser, os, kind string
	}{
		{
			name:    "chrome windows desktop",
			ua:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
			browser: "Chrome", os: "Windows", kind: "desktop",
		},
		{
			name:    "android phone chrome",
			ua:      "Mozilla/5.0 (Linux; Android 14; SM-S918B) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36",
			browser: "Chrome", os: "Android", kind: "mobile",
		},
		{
			name:    "iphone safari",
			ua:      "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1",
			browser: "Safari", os: "iOS", kind: "mobile",
		},
		{
			name:    "ipad safari tablet",
			ua:      "Mozilla/5.0 (iPad; CPU OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1",
			browser: "Safari", os: "iOS", kind: "tablet",
		},
		{
			name:    "edge windows",
			ua:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 Edg/126.0.0.0",
			browser: "Microsoft Edge", os: "Windows", kind: "desktop",
		},
		{
			name:    "firefox linux",
			ua:      "Mozilla/5.0 (X11; Linux x86_64; rv:127.0) Gecko/20100101 Firefox/127.0",
			browser: "Firefox", os: "Linux", kind: "desktop",
		},
		{
			name:    "mac chrome (CriOS is Chrome on iOS only — mac uses Chrome)",
			ua:      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
			browser: "Chrome", os: "macOS", kind: "desktop",
		},
		{
			name:    "samsung internet android tablet",
			ua:      "Mozilla/5.0 (Linux; Android 13; SM-X710) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/24.0 Chrome/117.0.0.0 Safari/537.36",
			browser: "Samsung Internet", os: "Android", kind: "tablet",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			browser, os, kind := parseUserAgent(tc.ua)
			if browser != tc.browser || os != tc.os || kind != tc.kind {
				t.Fatalf("parseUserAgent(%q) = (%q, %q, %q), want (%q, %q, %q)",
					tc.ua, browser, os, kind, tc.browser, tc.os, tc.kind)
			}
		})
	}
}

func TestParseUserAgentDegenerateInputs(t *testing.T) {
	// UA محذوف أو فارغ: «غير معروف» لا خطأ — الشاشة تعرض صفًا مقروءًا دائمًا.
	for _, ua := range []string{"", "   ", "curl/8.0", "python-requests/2.31"} {
		browser, os, kind := parseUserAgent(ua)
		if browser == "" || os == "" || kind == "" {
			t.Fatalf("parseUserAgent(%q) returned an empty field: (%q, %q, %q)", ua, browser, os, kind)
		}
		if kind != "unknown" && kind != "desktop" {
			t.Fatalf("parseUserAgent(%q) kind = %q, want unknown|desktop", ua, kind)
		}
	}
}

func TestDescribeUserAgentStableText(t *testing.T) {
	got := describeUserAgent("Chrome", "Windows", "desktop")
	if got != "Chrome على Windows (جهاز كمبيوتر)" {
		t.Fatalf("describeUserAgent desktop = %q", got)
	}
	if got := describeUserAgent("Safari", "iOS", "mobile"); got != "Safari على iOS (هاتف)" {
		t.Fatalf("describeUserAgent mobile = %q", got)
	}
	if got := describeUserAgent("Safari", "iOS", "tablet"); got != "Safari على iOS (لوحي)" {
		t.Fatalf("describeUserAgent tablet = %q", got)
	}
}

// TestIsKnownDeviceExcludesLatestSession — قاعدة استبعاد أحدث جلسة تُثبت
// على PostgreSQL في بوابة قاعدة البيانات (sessions_db_test بنمط
// passkey_recovery_bound_db_test) وليس هنا: الوحدة بلا pool لا معنى لها
// ولا سلامةً تُستدعى (db غير مهيأ = لا استدعاء).
const _ = "sessions db gate lives in sessions_db_test (PG17, suite 1→44)"
