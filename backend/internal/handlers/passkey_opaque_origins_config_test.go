package handlers

import (
	"reflect"
	"testing"
)

// Task 160 (M3) — the comma split for PASSKEY_RP_OPAQUE_ORIGINS: unset/blank
// stays nil (byte-identical pre-M3 configuration — web origins only), blanks
// between commas vanish, values trim cleanly.
func TestCSVNonEmpty(t *testing.T) {
	if got := csvNonEmpty(""); got != nil {
		t.Fatalf("empty value must yield nil, got %#v", got)
	}
	if got := csvNonEmpty("   "); got != nil {
		t.Fatalf("blank value must yield nil, got %#v", got)
	}
	if got := csvNonEmpty(" , , "); got != nil {
		t.Fatalf("comma-only value must yield nil, got %#v", got)
	}
	got := csvNonEmpty(" android:apk-key-hash:abc , android:apk-key-hash:abc= ,  https://app.example  ")
	want := []string{"android:apk-key-hash:abc", "android:apk-key-hash:abc=", "https://app.example"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("csv split mismatch:\n got %#v\nwant %#v", got, want)
	}
}
