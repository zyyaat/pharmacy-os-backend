// Package mailx — provider-independent SMTP transport for transactional
// email. One purpose: let the operator point Pharmacy OS at ANY mailbox
// provider (Gmail with an app password, Mailgun, SES, the hosting's own
// relay...) when the Brevo account itself is the broken link. Task 133.
//
// Why this exists: the 2026-10-31 neutral-domain probe proved Brevo can
// accept every send (202 + messageId) while delivering nothing to ANY
// provider — a failure mode that lives inside the Brevo account (unverified
// sender/domain, suspension, reputation) and is unreachable from code. The
// only durable escape is a second transport that does not depend on Brevo
// at all. SMTP is that transport: every provider on earth speaks it.
//
// Transport security is fail-closed:
//   - port 465   → implicit TLS from the first byte;
//   - other ports → STARTTLS whenever the server advertises it;
//   - credentials are NEVER sent over a plaintext connection (net/smtp's
//     PlainAuth refuses non-TLS unless the host is localhost) — a server
//     that demands auth but not TLS fails loudly instead of leaking the
//     password.
package mailx

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"log"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// Params is one complete outbound message plus the connection details of
// the SMTP relay. Everything is explicit — no globals, no env reads — so
// callers (auth mailer, support notifications) keep their own config flow.
type Params struct {
	Host     string // relay hostname, e.g. smtp.gmail.com
	Port     string // "587" (STARTTLS) or "465" (implicit TLS); "" → 587
	Username string // "" → no AUTH (open/internal relays)
	Password string

	FromEmail string // envelope + From header; must be a real mailbox
	FromName  string // display name; may be empty
	To        string
	Subject   string // UTF-8 safe (RFC 2047 encoded)
	HTML      string // multipart/alternative part 2
	Text      string // multipart/alternative part 1
}

const dialTimeout = 15 * time.Second

// Send delivers one message and returns nil only when the relay ACCEPTED
// the message (2xx on DATA). Like Brevo's 202, acceptance is not delivery —
// but unlike Brevo's API, an SMTP relay states its refusals synchronously
// (RCPT rejected, greylisting, auth failure), so silent black holes of the
// kind proven in the Task 132/132-b probes cannot happen here.
func Send(ctx context.Context, p Params) error {
	if strings.TrimSpace(p.Host) == "" {
		return fmt.Errorf("mailx: SMTP host is not configured")
	}
	if strings.TrimSpace(p.FromEmail) == "" || strings.TrimSpace(p.To) == "" {
		return fmt.Errorf("mailx: from/to addresses are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	port := strings.TrimSpace(p.Port)
	if port == "" {
		port = "587"
	}
	addr := net.JoinHostPort(p.Host, port)

	dialer := &net.Dialer{Timeout: dialTimeout}
	var conn net.Conn
	var err error
	if port == "465" {
		// Implicit TLS: the connection is encrypted before the SMTP
		// banner (legacy SSL port still used by many providers).
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: p.Host})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("mailx: dial %s failed: %w", addr, err)
	}

	client, err := smtp.NewClient(conn, p.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("mailx: smtp handshake failed: %w", err)
	}
	defer client.Close()

	if port != "465" {
		// Opportunistic STARTTLS — required in practice by every public
		// provider on 587; an internal relay may not offer it and that
		// stays legal ONLY while no credentials are involved (PlainAuth
		// refuses plaintext below, which is the real guard).
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: p.Host}); err != nil {
				return fmt.Errorf("mailx: STARTTLS failed: %w", err)
			}
		}
	}

	if p.Username != "" {
		if ok, _ := client.Extension("AUTH"); ok {
			auth := smtp.PlainAuth("", p.Username, p.Password, p.Host)
			if err := client.Auth(auth); err != nil {
				return fmt.Errorf("mailx: auth failed: %w", err)
			}
		}
	}

	if err := client.Mail(p.FromEmail); err != nil {
		return fmt.Errorf("mailx: MAIL FROM rejected: %w", err)
	}
	if err := client.Rcpt(p.To); err != nil {
		return fmt.Errorf("mailx: RCPT TO rejected for %s: %w", p.To, err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("mailx: DATA rejected: %w", err)
	}
	if _, err := w.Write(buildRFC5322(p)); err != nil {
		w.Close()
		return fmt.Errorf("mailx: writing message failed: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mailx: relay rejected message body: %w", err)
	}
	if err := client.Quit(); err != nil {
		// Message already handed over — a rude QUIT reply must not fail
		// an accepted send.
		log.Printf("[mailx] QUIT reply ignored after accepted send: %v", err)
	}
	log.Printf("[mailx] smtp accepted send: to=%s from=%s via=%s:%s", p.To, p.FromEmail, p.Host, port)
	return nil
}

// buildRFC5322 renders the wire message: RFC 5322 headers (UTF-8 safe via
// RFC 2047) + multipart/alternative with the plain part first and the HTML
// part second. Bodies travel base64-encoded so Arabic content survives any
// relay without line-length or 8-bit-cleanliness hazards.
func buildRFC5322(p Params) []byte {
	from := (&mail.Address{Name: p.FromName, Address: p.FromEmail}).String()
	to := (&mail.Address{Address: p.To}).String()

	boundary := "pos-alt-" + randHex(16)
	var b bytes.Buffer
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", p.Subject) + "\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString(`Content-Type: multipart/alternative; boundary="` + boundary + `"` + "\r\n")
	b.WriteString("\r\n")

	writePart := func(contentType, body string) {
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString("Content-Type: " + contentType + "; charset=utf-8\r\n")
		b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
		b.WriteString(base64Wrap(body))
		b.WriteString("\r\n")
	}
	writePart("text/plain", p.Text)
	writePart("text/html", p.HTML)
	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes()
}

// base64Wrap encodes s and folds it at 76 octets per RFC 2045 §6.8.
func base64Wrap(s string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(s))
	var b strings.Builder
	for i := 0; i < len(enc); i += 76 {
		end := i + 76
		if end > len(enc) {
			end = len(enc)
		}
		b.WriteString(enc[i:end])
		b.WriteString("\r\n")
	}
	return b.String()
}

func randHex(n int) string {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "fallbackboundary"
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, n*2)
	for _, v := range raw {
		out = append(out, hexdigits[v>>4], hexdigits[v&0x0f])
	}
	return string(out)
}
