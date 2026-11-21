package config

// Task 151 — the passkey Relying Party identity derives from PUBLIC_APP_URL
// and fails CLOSED: any missing/unparsable URL yields an empty RPID, which
// keeps the whole passkey family disabled instead of half-configured.

import "testing"

func TestDerivePasskeyRPID(t *testing.T) {
        cases := []struct {
                name string
                url  string
                want string
        }{
                {"production cloud host", "https://pharmacy-os.dockhosting.cloud", "pharmacy-os.dockhosting.cloud"},
                {"port is stripped", "https://localhost:8080", "localhost"},
                {"http dev origin", "http://127.0.0.1:3000", "127.0.0.1"},
                {"path ignored", "https://example.com/some/path", "example.com"},
                {"empty url", "", ""},
                {"whitespace only", "   ", ""},
                {"not a url", ":::not-a-url", ""},
                {"bare string", "example.com", "example.com"},
        }
        for _, testCase := range cases {
                t.Run(testCase.name, func(t *testing.T) {
                        if got := derivePasskeyRPID(testCase.url); got != testCase.want {
                                t.Fatalf("derivePasskeyRPID(%q) = %q, want %q", testCase.url, got, testCase.want)
                        }
                })
        }
}
