package config

import "testing"

// TestResolvedMailTransport locks the single resolution rule (Task 133):
// explicit MAIL_TRANSPORT ("smtp"|"brevo") wins, otherwise SMTP as soon as
// a relay host is configured (the operator's escape hatch from a broken
// Brevo account), otherwise Brevo — the zero-env default stays untouched.
func TestResolvedMailTransport(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "empty env stays brevo",
			cfg:  Config{BrevoAPIKey: "k", MailFromEmail: "f@x"},
			want: "brevo",
		},
		{
			name: "relay host set means smtp",
			cfg:  Config{BrevoAPIKey: "k", MailFromEmail: "f@x", MailSMTPHost: "smtp.gmail.com"},
			want: "smtp",
		},
		{
			name: "explicit smtp override wins even without host",
			cfg:  Config{MailFromEmail: "f@x", MailTransport: "smtp"},
			want: "smtp",
		},
		{
			name: "explicit brevo override wins even with host",
			cfg:  Config{BrevoAPIKey: "k", MailFromEmail: "f@x", MailSMTPHost: "smtp.gmail.com", MailTransport: "BREVO"},
			want: "brevo",
		},
		{
			name: "unknown override value falls back to host rule",
			cfg:  Config{BrevoAPIKey: "k", MailFromEmail: "f@x", MailTransport: "sendgrid"},
			want: "brevo",
		},
		{
			name: "unknown override with host resolves smtp",
			cfg:  Config{MailFromEmail: "f@x", MailSMTPHost: "smtp.gmail.com", MailTransport: "sendgrid"},
			want: "smtp",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.ResolvedMailTransport(); got != tc.want {
				t.Fatalf("ResolvedMailTransport() = %q, want %q", got, tc.want)
			}
		})
	}
}
