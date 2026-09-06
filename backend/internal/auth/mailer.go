package auth

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

type mailer struct {
	apiKey    string
	fromEmail string
	fromName  string
	appURL    string
	client    *http.Client
}

func newMailer(cfg Config) *mailer {
	return &mailer{
		apiKey: cfg.BrevoAPIKey, fromEmail: cfg.MailFromEmail,
		fromName: cfg.MailFromName, appURL: strings.TrimRight(cfg.PublicAppURL, "/"),
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

func (m *mailer) configured() bool {
	return m.apiKey != "" && m.fromEmail != ""
}

func (m *mailer) send(ctx context.Context, recipient, subject, html, text string) error {
	if !m.configured() {
		return fmt.Errorf("transactional email is not configured")
	}
	body, err := json.Marshal(map[string]interface{}{
		"sender":      map[string]string{"email": m.fromEmail, "name": m.fromName},
		"to":          []map[string]string{{"email": recipient}},
		"subject":     subject,
		"htmlContent": html,
		"textContent": text,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.brevo.com/v3/smtp/email", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("api-key", m.apiKey)
	response, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		details, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		detail := strings.TrimSpace(string(details))
		if detail == "" {
			return fmt.Errorf("brevo returned status %d", response.StatusCode)
		}
		return fmt.Errorf("brevo returned status %d: %s", response.StatusCode, detail)
	}
	return nil
}

func (m *mailer) verificationEmail(ctx context.Context, email, code string) error {
	html := fmt.Sprintf(`<!doctype html>
<html lang="ar" dir="rtl">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1.0"><title>تأكيد البريد الإلكتروني</title></head>
<body style="margin:0;padding:0;background:#07100d;color:#f2f5f2;font-family:Arial,Helvetica,sans-serif;">
  <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" style="background:#07100d;">
    <tr><td align="center" style="padding:32px 14px;">
      <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" style="max-width:600px;">
        <tr><td style="padding:6px 8px 28px;">
          <table role="presentation" cellspacing="0" cellpadding="0" border="0" align="right" dir="ltr">
            <tr>
              <td width="42" height="42" align="center" valign="middle" style="background:#00d084;border-radius:13px;color:#06100d;font-size:17px;font-weight:800;letter-spacing:2px;">▮▮</td>
              <td style="padding-left:11px;color:#f2f5f2;font-size:16px;font-weight:700;letter-spacing:-.5px;">Pharmacy <span style="color:#00d084;">OS</span></td>
            </tr>
          </table>
        </td></tr>
        <tr><td style="background:#0b1914;border:1px solid #20352b;border-radius:24px;padding:42px 34px 36px;text-align:right;">
          <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" dir="rtl">
            <tr><td style="color:#00d084;font-size:12px;font-weight:700;letter-spacing:1.5px;text-transform:uppercase;padding-bottom:16px;">PHARMACY OPERATING SYSTEM</td></tr>
            <tr><td style="color:#f2f5f2;font-size:28px;line-height:1.45;font-weight:800;padding-bottom:14px;">أهلًا بك في<br><span style="color:#00d084;">Pharmacy OS</span></td></tr>
            <tr><td style="color:#aab8b0;font-size:15px;line-height:1.9;padding-bottom:28px;">باقي خطوة واحدة لتفعيل حسابك والبدء في إدارة صيدليتك بوضوح أكبر.</td></tr>
            <tr><td align="center" style="background:#10251b;border:1px solid #24563f;border-radius:18px;padding:24px 18px;">
              <div style="color:#9da7a2;font-size:13px;line-height:1.6;">رمز التحقق الخاص بك</div>
              <div dir="ltr" style="color:#00d084;font-size:42px;line-height:1.25;font-weight:800;letter-spacing:11px;padding:10px 0 8px 11px;">%s</div>
              <div style="color:#7f9589;font-size:12px;line-height:1.6;">6 أرقام&nbsp; • &nbsp;صالح لمدة 24 ساعة</div>
            </td></tr>
            <tr><td style="color:#aab8b0;font-size:13px;line-height:1.8;padding-top:25px;">أدخل الرمز في شاشة تأكيد البريد الإلكتروني داخل التطبيق. إذا لم تطلب إنشاء هذا الحساب، يمكنك تجاهل هذه الرسالة.</td></tr>
          </table>
        </td></tr>
        <tr><td align="center" style="color:#6f8077;font-size:12px;line-height:1.7;padding:24px 18px 6px;">بُني للصيدليات التي تريد أن تنمو<br><span style="color:#00d084;">Pharmacy OS</span> &nbsp;•&nbsp; © 2026</td></tr>
      </table>
    </td></tr>
  </table>
</body>
</html>`, code)
	text := fmt.Sprintf("أهلًا بك في Pharmacy OS.\n\nرمز تأكيد البريد الإلكتروني: %s\n\nأدخل الرمز في شاشة التحقق داخل التطبيق. الرمز صالح لمدة 24 ساعة.\n\nإذا لم تطلب إنشاء هذا الحساب، يمكنك تجاهل هذه الرسالة.", code)
	return m.send(ctx, email, "تأكيد بريدك الإلكتروني — Pharmacy OS", html, text)
}

func (m *mailer) resetEmail(ctx context.Context, email, token string) error {
	if m.appURL == "" {
		return fmt.Errorf("public app URL is not configured")
	}
	link := fmt.Sprintf("%s/reset-password?token=%s", m.appURL, token)
	html := fmt.Sprintf(
		`<p>We received a request to reset your Pharmacy OS password.</p><p>Reset it by clicking <a href="%s">this link</a>.</p><p>This link expires in one hour.</p>`,
		link,
	)
	text := fmt.Sprintf("Reset your Pharmacy OS password using this link: %s\n\nThis link expires in one hour.", link)
	return m.send(ctx, email, "Reset your Pharmacy OS password", html, text)
}
