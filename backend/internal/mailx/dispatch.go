// Dispatch — the transport-agnostic send entry for background jobs.
//
// The jobs workers (internal/jobs) send mail from INSIDE a River worker —
// no HTTP request context, no gin handler, no per-call config. They cannot
// reach for handler-local helpers, and a third copy of the Brevo call would
// drift (handlers' sendSupportEmail and auth's mailer already carry two).
// So the shared resolution lives here: same endpoint and payload contract
// as the existing Brevo callers, same SMTP escape hatch as mailx.Send.
//
// Transport selection mirrors config.Config.ResolvedMailTransport: an
// explicit MAIL_TRANSPORT wins, else SMTP as soon as a relay host is set,
// else Brevo. Everything is explicit fields — mailx keeps its no-globals
// rule; callers map their config into DispatchConfig once at construction.
package mailx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const brevoEndpoint = "https://api.brevo.com/v3/smtp/email"

// DispatchConfig is the operator's resolved transport choice + sender
// identity, mapped once from config.Config by the process owner.
type DispatchConfig struct {
	Transport string // "smtp" | "brevo" ("" → resolve: SMTP if host set, else Brevo)

	BrevoAPIKey string

	FromEmail string
	FromName  string

	SMTPHost     string
	SMTPPort     string
	SMTPUsername string
	SMTPPassword string
}

// Dispatch delivers one transactional message over whichever transport the
// operator configured. An unconfigured transport (no key, no host) fails
// loudly — callers that may legitimately run without mail check readiness
// BEFORE calling (a worker that swallowed a send would look like success).
func Dispatch(ctx context.Context, c DispatchConfig, to, subject, html, text string) error {
	transport := strings.ToLower(strings.TrimSpace(c.Transport))
	if transport == "" {
		if strings.TrimSpace(c.SMTPHost) != "" {
			transport = "smtp"
		} else {
			transport = "brevo"
		}
	}
	if transport == "smtp" {
		return Send(ctx, Params{
			Host: c.SMTPHost, Port: c.SMTPPort,
			Username: c.SMTPUsername, Password: c.SMTPPassword,
			FromEmail: c.FromEmail, FromName: c.FromName,
			To: to, Subject: subject, HTML: html, Text: text,
		})
	}
	return SendBrevo(ctx, c.BrevoAPIKey, c.FromEmail, c.FromName, to, subject, html, text)
}

// SendBrevo is the minimal Brevo transactional call — same endpoint, auth
// header and payload shape the support mailer and auth mailer use. Returns
// an error on any non-2xx so River's retry policy governs a dead provider
// instead of the send vanishing.
func SendBrevo(ctx context.Context, apiKey, fromEmail, fromName, to, subject, html, text string) error {
	if strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("mailx: brevo api key is not configured")
	}
	if strings.TrimSpace(fromEmail) == "" || strings.TrimSpace(to) == "" {
		return fmt.Errorf("mailx: from/to addresses are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{
		"sender":      map[string]string{"email": fromEmail, "name": fromName},
		"to":          []map[string]string{{"email": to}},
		"subject":     subject,
		"htmlContent": html,
		"textContent": text,
	})
	if err != nil {
		return fmt.Errorf("mailx: brevo payload: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, brevoEndpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("mailx: brevo request: %w", err)
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("api-key", apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("mailx: brevo send failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("mailx: brevo status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
