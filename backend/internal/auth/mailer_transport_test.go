package auth

import (
        "bufio"
        "context"
        "io"
        "net"
        "net/http"
        "strings"
        "testing"
        "time"
)

// TestMailerUsesResolvedTransport locks the Task 133 wiring: the mailer
// rides the transport verdict the composition root resolves once from the
// global config (Config.ResolvedMailTransport — see internal/config tests
// for the resolution rule itself).
func TestMailerUsesResolvedTransport(t *testing.T) {
        cases := []struct {
                name string
                cfg  Config
                want string
        }{
                {
                        name: "brevo verdict keeps brevo",
                        cfg:  Config{BrevoAPIKey: "k", MailFromEmail: "f@x", TransportResolved: "brevo"},
                        want: transportBrevo,
                },
                {
                        name: "smtp verdict with host switches the pipeline",
                        cfg:  Config{BrevoAPIKey: "k", MailFromEmail: "f@x", MailSMTPHost: "smtp.x.y", TransportResolved: "smtp"},
                        want: transportSMTP,
                },
        }
        for _, tc := range cases {
                t.Run(tc.name, func(t *testing.T) {
                        m := newMailer(tc.cfg)
                        if m.transport() != tc.want {
                                t.Fatalf("mailer.transport() = %q, want %q", m.transport(), tc.want)
                        }
                })
        }
}

func TestMailerConfiguredMatrix(t *testing.T) {
        cases := []struct {
                name string
                cfg  Config
                want bool
        }{
                {name: "brevo ready", cfg: Config{BrevoAPIKey: "k", MailFromEmail: "f@x", TransportResolved: "brevo"}, want: true},
                {name: "brevo missing key", cfg: Config{MailFromEmail: "f@x", TransportResolved: "brevo"}, want: false},
                {name: "smtp ready", cfg: Config{MailFromEmail: "f@x", MailSMTPHost: "smtp.x.y", TransportResolved: "smtp"}, want: true},
                {name: "smtp missing host", cfg: Config{BrevoAPIKey: "k", MailFromEmail: "f@x", TransportResolved: "smtp"}, want: false},
                {name: "smtp missing from", cfg: Config{MailSMTPHost: "smtp.x.y", TransportResolved: "smtp"}, want: false},
        }
        for _, tc := range cases {
                t.Run(tc.name, func(t *testing.T) {
                        if got := newMailer(tc.cfg).configured(); got != tc.want {
                                t.Fatalf("configured() = %v, want %v", got, tc.want)
                        }
                })
        }
}

// leanRelay is a compact SMTP fake for routing tests (mailx has the full
// wire-level suite; this one only needs to accept one message).
type leanRelay struct {
        ln   net.Listener
        data string
}

func startLeanRelay(t *testing.T) *leanRelay {
        t.Helper()
        ln, err := net.Listen("tcp", "127.0.0.1:0")
        if err != nil {
                t.Fatalf("listen: %v", err)
        }
        r := &leanRelay{ln: ln}
        go func() {
                conn, err := ln.Accept()
                if err != nil {
                        return
                }
                defer conn.Close()
                br := bufio.NewReader(conn)
                write := func(s string) { conn.Write([]byte(s)) }
                write("220 lean ESMTP\r\n")
                inData := false
                deadline := time.Now().Add(5 * time.Second)
                for time.Now().Before(deadline) {
                        conn.SetDeadline(deadline)
                        line, err := br.ReadString('\n')
                        if err != nil {
                                return
                        }
                        trimmed := strings.TrimRight(line, "\r\n")
                        if inData {
                                if trimmed == "." {
                                        inData = false
                                        write("250 queued\r\n")
                                } else {
                                        r.data += trimmed + "\n"
                                }
                                continue
                        }
                        cmd := strings.ToUpper(trimmed)
                        switch {
                        case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
                                write("250-lean\r\n250 8BITMIME\r\n")
                        case strings.HasPrefix(cmd, "MAIL FROM:"), strings.HasPrefix(cmd, "RCPT TO:"):
                                write("250 OK\r\n")
                        case strings.HasPrefix(cmd, "DATA"):
                                inData = true
                                write("354 go\r\n")
                        case strings.HasPrefix(cmd, "QUIT"):
                                write("221 bye\r\n")
                                return
                        default:
                                write("250 OK\r\n")
                        }
                }
        }()
        return r
}

func TestMailerSendRoutesToSMTP(t *testing.T) {
        relay := startLeanRelay(t)
        host, port, _ := net.SplitHostPort(relay.ln.Addr().String())

        m := newMailer(Config{
                MailFromEmail: "no-reply@pharmacy.example", MailFromName: "Pharmacy OS",
                MailSMTPHost: host, MailSMTPPort: port, TransportResolved: "smtp",
        })
        ref, err := m.send(context.Background(), "owner@dest.example",
                "تأكيد التسجيل", "<p>رمزك 918273</p>", "رمزك 918273")
        if err != nil {
                t.Fatalf("smtp-routed send failed: %v", err)
        }
        if ref != "" {
                t.Fatalf("smtp path returns no provider message id, got %q", ref)
        }
        if !strings.Contains(relay.data, "Subject:") || !strings.Contains(relay.data, "MIME-Version: 1.0") {
                t.Fatalf("relay did not receive a MIME message:\n%s", relay.data)
        }
}

// stubTripper records the request a brevo-routed send makes and answers a
// canned acceptance — proving the default transport is untouched by Task 133.
type stubTripper struct {
        got  *http.Request
        body string
}

func (s *stubTripper) RoundTrip(r *http.Request) (*http.Response, error) {
        s.got = r
        raw, _ := io.ReadAll(r.Body)
        s.body = string(raw)
        return &http.Response{
                StatusCode: 200,
                Body:       io.NopCloser(strings.NewReader(`{"messageId":"<mid-1@brevo>"}`)),
                Header:     http.Header{},
        }, nil
}

func TestMailerSendRoutesToBrevo(t *testing.T) {
        m := newMailer(Config{BrevoAPIKey: "key-1", MailFromEmail: "f@x"})
        tripper := &stubTripper{}
        m.client = &http.Client{Transport: tripper}

        ref, err := m.send(context.Background(), "to@x", "subj", "<p/>", "txt")
        if err != nil {
                t.Fatalf("brevo-routed send failed: %v", err)
        }
        if ref != "<mid-1@brevo>" {
                t.Fatalf("message id = %q", ref)
        }
        if tripper.got.URL.String() != "https://api.brevo.com/v3/smtp/email" {
                t.Fatalf("brevo endpoint changed: %s", tripper.got.URL)
        }
        if tripper.got.Header.Get("api-key") != "key-1" {
                t.Fatalf("api-key header missing")
        }
        if !strings.Contains(tripper.body, `"htmlContent"`) {
                t.Fatalf("brevo payload shape changed: %s", tripper.body)
        }
}
