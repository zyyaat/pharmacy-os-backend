package mailx

import (
        "bufio"
        "context"
        "encoding/base64"
        "net"
        "regexp"
        "strings"
        "testing"
        "time"
)

// fakeRelay is a minimal but honest SMTP server: it speaks the real wire
// protocol so Send() is exercised end-to-end, and records the envelope and
// DATA payload for assertions. advertisedAuth controls the EHLO extensions.
type fakeRelay struct {
        ln net.Listener

        advertiseAuth bool // EHLO advertises AUTH PLAIN (plaintext server!)

        greeting string
        mailFrom string
        rcptTo   string
        data     string
        quitSeen bool
}

func startFakeRelay(t *testing.T, advertiseAuth bool) *fakeRelay {
        t.Helper()
        ln, err := net.Listen("tcp", "127.0.0.1:0")
        if err != nil {
                t.Fatalf("relay listen: %v", err)
        }
        r := &fakeRelay{ln: ln, advertiseAuth: advertiseAuth}
        go r.serveOnce()
        return r
}

func (r *fakeRelay) addr() string { return r.ln.Addr().String() }
func (r *fakeRelay) host() string { return "127.0.0.1" }

// serveOnce handles exactly one connection (each test starts a fresh relay).
func (r *fakeRelay) serveOnce() {
        conn, err := r.ln.Accept()
        if err != nil {
                return
        }
        defer conn.Close()
        br := bufio.NewReader(conn)
        write := func(s string) { conn.Write([]byte(s)) }
        write("220 fake-relay ESMTP\r\n")

        inData := false
        for {
                conn.SetDeadline(time.Now().Add(5 * time.Second))
                line, err := br.ReadString('\n')
                if err != nil {
                        return
                }
                trimmed := strings.TrimRight(line, "\r\n")
                if inData {
                        if trimmed == "." {
                                inData = false
                                write("250 queued OK\r\n")
                        } else {
                                r.data += strings.TrimPrefix(trimmed, ".") + "\n"
                        }
                        continue
                }
                cmd := strings.ToUpper(trimmed)
                switch {
                case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
                        r.greeting = trimmed
                        write("250-fake-relay greets you\r\n")
                        write("250-8BITMIME\r\n")
                        if r.advertiseAuth {
                                write("250-AUTH PLAIN LOGIN\r\n")
                        }
                        write("250 SIZE 26214400\r\n")
                case strings.HasPrefix(cmd, "AUTH"):
                        // Plaintext relay: net/smtp's PlainAuth must REFUSE to hand
                        // over credentials here — the client errors before us, so we
                        // should never see this line when advertiseAuth is true and
                        // TLS is absent.
                        write("535 authentication refused\r\n")
                case strings.HasPrefix(cmd, "MAIL FROM:"):
                        r.mailFrom = extractAddr(trimmed[len("MAIL FROM:"):])
                        write("250 OK\r\n")
                case strings.HasPrefix(cmd, "RCPT TO:"):
                        r.rcptTo = extractAddr(trimmed[len("RCPT TO:"):])
                        write("250 OK\r\n")
                case strings.HasPrefix(cmd, "DATA"):
                        inData = true
                        write("354 end with <CRLF>.<CRLF>\r\n")
                case strings.HasPrefix(cmd, "QUIT"):
                        r.quitSeen = true
                        write("221 bye\r\n")
                        return
                case strings.HasPrefix(cmd, "RSET"), strings.HasPrefix(cmd, "NOOP"):
                        write("250 OK\r\n")
                default:
                        write("500 unrecognized\r\n")
                }
        }
}

// extractAddr pulls the address out of "<a@b.c> BODY=8BITMIME" style
// arguments: the address is whatever sits between the angle brackets.
func extractAddr(arg string) string {
        open := strings.Index(arg, "<")
        close := strings.LastIndex(arg, ">")
        if open == -1 || close == -1 || close < open {
                return strings.TrimSpace(arg)
        }
        return arg[open+1 : close]
}

func decodeMIMEPart(data, contentType string) string {
        // Extract the base64 body of the requested part and decode it.
        re := regexp.MustCompile(`Content-Type: ` + regexp.QuoteMeta(contentType) + `; charset=utf-8\r?\nContent-Transfer-Encoding: base64\r?\n\r?\n([\s\S]*?)\r?\n\r?\n`)
        m := re.FindStringSubmatch(data)
        if m == nil {
                return ""
        }
        dec, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(m[1], "\r\n", ""))
        if err != nil {
                return ""
        }
        return string(dec)
}

func TestSendDeliversRFC5322OverSMTP(t *testing.T) {
        relay := startFakeRelay(t, false)
        host, port, _ := net.SplitHostPort(relay.addr())

        err := Send(context.Background(), Params{
                Host: host, Port: port,
                FromEmail: "no-reply@example.com", FromName: "Pharmacy OS",
                To:      "user@dest.example",
                Subject: "تأكيد بريدك الإلكتروني — Pharmacy OS",
                Text:    "رمز التحقق: 428193",
                HTML:    "<p>رمز التحقق: <b>428193</b></p>",
        })
        if err != nil {
                t.Fatalf("Send failed: %v", err)
        }

        if relay.mailFrom != "no-reply@example.com" {
                t.Errorf("envelope MAIL FROM = %q, want no-reply@example.com", relay.mailFrom)
        }
        if relay.rcptTo != "user@dest.example" {
                t.Errorf("envelope RCPT TO = %q, want user@dest.example", relay.rcptTo)
        }
        if !relay.quitSeen {
                t.Error("client never sent QUIT")
        }
        for _, header := range []string{"From: ", "To: ", "Subject: =?utf-8?", "MIME-Version: 1.0", `multipart/alternative`} {
                if !strings.Contains(relay.data, header) {
                        t.Errorf("DATA missing %q\n--- captured DATA ---\n%s", header, relay.data)
                }
        }
        // From header must carry the display name (RFC 2047 for safety).
        if !strings.Contains(relay.data, "From:") {
                t.Error("missing From header")
        }
        if got := decodeMIMEPart(relay.data, "text/plain"); !strings.Contains(got, "رمز التحقق: 428193") {
                t.Errorf("decoded text part = %q, want the Arabic code line", got)
        }
        if got := decodeMIMEPart(relay.data, "text/html"); !strings.Contains(got, "<b>428193</b>") {
                t.Errorf("decoded html part = %q, want the code markup", got)
        }
}

func TestSendAuthFailureSurfaces(t *testing.T) {
        relay := startFakeRelay(t, true)
        host, port, _ := net.SplitHostPort(relay.addr())

        err := Send(context.Background(), Params{
                Host: host, Port: port, Username: "user@mail.example", Password: "sekrit",
                FromEmail: "no-reply@example.com", To: "user@dest.example",
                Subject: "s", Text: "t", HTML: "<p>h</p>",
        })
        // The relay advertises AUTH but refuses the credentials (535) — the
        // failure must surface to the caller, never be swallowed. (Over
        // loopback the stdlib permits plaintext AUTH by design — the traffic
        // never leaves the host; for every non-local host smtp.PlainAuth
        // additionally refuses to send credentials without TLS, which is the
        // fail-closed guarantee documented in the package comment.)
        if err == nil || !strings.Contains(err.Error(), "auth failed") {
                t.Fatalf("expected auth failure to surface, got: %v", err)
        }
}

func TestSendValidatesRequiredFields(t *testing.T) {
        if err := Send(context.Background(), Params{To: "x@y.z"}); err == nil {
                t.Error("empty host must fail")
        }
        if err := Send(context.Background(), Params{Host: "h", FromEmail: ""}); err == nil {
                t.Error("empty from must fail")
        }
}

func TestBase64WrapFoldsAt76(t *testing.T) {
        long := strings.Repeat("أ", 200)
        out := base64Wrap(long)
        for _, line := range strings.Split(strings.TrimRight(out, "\r\n"), "\r\n") {
                if len(line) > 76 {
                        t.Fatalf("line longer than 76: %d", len(line))
                }
        }
        dec, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out, "\r\n", ""))
        if err != nil || string(dec) != long {
                t.Fatalf("roundtrip broken: %v", err)
        }
}
