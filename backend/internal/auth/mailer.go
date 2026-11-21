package auth

import (
        "bytes"
        "context"
        "encoding/json"
        "fmt"
        "html"
        "io"
        "log"
        "net/http"
        "net/url"
        "strings"
        "time"

        "github.com/pharmacy-os/backend/internal/mailx"
)

const (
        transportBrevo = "brevo"
        transportSMTP  = "smtp"
)

type mailer struct {
        apiKey    string
        fromEmail string
        fromName  string
        appURL    string
        client    *http.Client

        // Task 133 — outbound transport. Resolved ONCE from
        // Config.ResolvedMailTransport at construction: "brevo" (default,
        // HTTPS API) or "smtp" (any relay the operator configures — the
        // provider-independent escape hatch after the 2026-10-31 probe
        // proved a Brevo account can accept every send and deliver none).
        transportName string

        // SMTP relay connection settings (used when transportName == smtp).
        smtpHost     string
        smtpPort     string
        smtpUsername string
        smtpPassword string
}

func newMailer(cfg Config) *mailer {
        return &mailer{
                apiKey: cfg.BrevoAPIKey, fromEmail: cfg.MailFromEmail,
                fromName: cfg.MailFromName, appURL: strings.TrimRight(cfg.PublicAppURL, "/"),
                client: &http.Client{Timeout: 15 * time.Second},
                transportName: cfg.TransportResolved,
                smtpHost:      cfg.MailSMTPHost, smtpPort: cfg.MailSMTPPort,
                smtpUsername: cfg.MailSMTPUsername, smtpPassword: cfg.MailSMTPPassword,
        }
}

// transport reports the outbound path this mailer instance uses.
func (m *mailer) transport() string { return m.transportName }

func (m *mailer) configured() bool {
        if m.transport() == transportSMTP {
                return m.smtpHost != "" && m.fromEmail != ""
        }
        return m.apiKey != "" && m.fromEmail != ""
}

// deliveryEvents queries Brevo's transactional event log for one recipient
// address and returns the raw event objects (newest first). This is the only
// source of truth for what actually happened AFTER Brevo accepted a message:
// delivered / deferred / blocked / softBounce / hardBounce / spam, plus the
// provider's SMTP reason string (e.g. Microsoft's 550 5.7.1 block-list
// response). Brevo retains events for a limited window, so an empty result
// means "no record in the retention window", not "never sent".
func (m *mailer) deliveryEvents(ctx context.Context, email string) ([]map[string]interface{}, error) {
        if !m.configured() {
                return nil, fmt.Errorf("transactional email is not configured")
        }
        endpoint := "https://api.brevo.com/v3/smtp/statistics/events?limit=10&sort=desc&email=" + url.QueryEscape(email)
        req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
        if err != nil {
                return nil, err
        }
        req.Header.Set("accept", "application/json")
        req.Header.Set("api-key", m.apiKey)
        response, err := m.client.Do(req)
        if err != nil {
                return nil, err
        }
        defer response.Body.Close()
        body, _ := io.ReadAll(io.LimitReader(response.Body, 65536))
        if response.StatusCode < 200 || response.StatusCode >= 300 {
                detail := strings.TrimSpace(string(body))
                if detail == "" {
                        detail = fmt.Sprintf("status %d", response.StatusCode)
                }
                return nil, fmt.Errorf("brevo events returned %d: %s", response.StatusCode, detail)
        }
        var parsed struct {
                Events []map[string]interface{} `json:"events"`
        }
        if err := json.Unmarshal(body, &parsed); err != nil {
                return nil, fmt.Errorf("brevo events unparsable: %w", err)
        }
        return parsed.Events, nil
}

func (m *mailer) send(ctx context.Context, recipient, subject, html, text string) (string, error) {
        if !m.configured() {
                return "", fmt.Errorf("transactional email is not configured")
        }
        if m.transport() == transportSMTP {
                return m.sendSMTP(ctx, recipient, subject, html, text)
        }
        body, err := json.Marshal(map[string]interface{}{
                "sender":      map[string]string{"email": m.fromEmail, "name": m.fromName},
                "to":          []map[string]string{{"email": recipient}},
                "subject":     subject,
                "htmlContent": html,
                "textContent": text,
        })
        if err != nil {
                return "", err
        }
        req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.brevo.com/v3/smtp/email", bytes.NewReader(body))
        if err != nil {
                return "", err
        }
        req.Header.Set("accept", "application/json")
        req.Header.Set("content-type", "application/json")
        req.Header.Set("api-key", m.apiKey)
        response, err := m.client.Do(req)
        if err != nil {
                return "", err
        }
        defer response.Body.Close()
        body, _ = io.ReadAll(io.LimitReader(response.Body, 4096))
        if response.StatusCode < 200 || response.StatusCode >= 300 {
                detail := strings.TrimSpace(string(body))
                if detail == "" {
                        return "", fmt.Errorf("brevo returned status %d", response.StatusCode)
                }
                return "", fmt.Errorf("brevo returned status %d: %s", response.StatusCode, detail)
        }
        // A 2xx means Brevo ACCEPTED the message for delivery — it does NOT
        // mean the recipient's provider (Microsoft is notorious for this)
        // actually accepted it. Keep the messageId so operators can correlate
        // with Brevo's async event log.
        var accepted struct {
                MessageID string `json:"messageId"`
        }
        if err := json.Unmarshal(body, &accepted); err != nil || accepted.MessageID == "" {
                log.Printf("[mailer] brevo accepted send to %s but messageId was unparsable: %v", recipient, err)
                return "", nil
        }
        log.Printf("[mailer] brevo accepted send: message_id=%s to=%s from=%s", accepted.MessageID, recipient, m.fromEmail)
        return accepted.MessageID, nil
}

// sendSMTP hands the message to any SMTP relay through mailx (Task 133).
// Unlike Brevo's async 202, an SMTP relay states its refusals synchronously
// (bad recipient, greylisting, auth failure) so a silent black hole of the
// kind the delivery probes proved cannot happen. There is no provider-side
// message id to correlate, so the returned reference is empty.
func (m *mailer) sendSMTP(ctx context.Context, recipient, subject, html, text string) (string, error) {
        err := mailx.Send(ctx, mailx.Params{
                Host: m.smtpHost, Port: m.smtpPort,
                Username: m.smtpUsername, Password: m.smtpPassword,
                FromEmail: m.fromEmail, FromName: m.fromName,
                To: recipient, Subject: subject, HTML: html, Text: text,
        })
        if err != nil {
                return "", err
        }
        return "", nil
}

func (m *mailer) verificationEmail(ctx context.Context, email, code string) (string, error) {
        logoURL := html.EscapeString(m.assetURL("brand/pharmacy-os-icon.png"))
        logo := `<span style="display:inline-block;width:46px;height:46px;border-radius:14px;background:#00d084;color:#06100d;font-size:20px;line-height:46px;text-align:center;font-weight:800;">P</span>`
        if logoURL != "" {
                logo = fmt.Sprintf(`<img src="%s" alt="Pharmacy OS" width="46" height="46" style="display:block;width:46px;height:46px;border:0;border-radius:14px;" />`, logoURL)
        }
        emailHTML := fmt.Sprintf(`<!doctype html>
<html lang="ar" dir="rtl">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1.0"><title>تأكيد البريد الإلكتروني — Pharmacy OS</title></head>
<body style="margin:0;padding:0;background:#050a09;color:#f4fff8;font-family:Arial,Helvetica,sans-serif;">
  <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" style="width:100%%;background:#050a09;">
    <tr><td align="center" style="padding:32px 14px 44px;">
      <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" style="max-width:620px;">
        <tr><td style="padding:0 8px 22px;">
          <table role="presentation" cellspacing="0" cellpadding="0" border="0" align="right" dir="ltr">
            <tr>
              <td valign="middle">%s</td>
              <td style="padding-left:12px;color:#f4fff8;font-size:17px;font-weight:800;letter-spacing:-.4px;">Pharmacy <span style="color:#00d084;">OS</span></td>
            </tr>
          </table>
        </td></tr>
        <tr><td style="background:#0a1511;border:1px solid #2b493a;border-radius:24px;padding:42px 34px 38px;text-align:right;">
          <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" dir="rtl">
            <tr><td style="color:#00d084;font-size:12px;font-weight:800;letter-spacing:1.6px;text-transform:uppercase;padding-bottom:18px;">PHARMACY OPERATING SYSTEM</td></tr>
            <tr><td style="color:#ffffff;font-size:30px;line-height:1.45;font-weight:800;padding-bottom:14px;">أهلًا بك في<br><span style="color:#00d084;">Pharmacy OS</span></td></tr>
            <tr><td style="color:#d7e8dc;font-size:16px;line-height:1.9;padding-bottom:28px;">باقي خطوة واحدة لتفعيل حسابك والبدء في إدارة صيدليتك بوضوح أكبر.</td></tr>
            <tr><td align="center" style="background:#10251b;border:1px solid #347b59;border-radius:18px;padding:25px 18px 23px;">
              <div style="color:#d7e8dc;font-size:14px;line-height:1.6;">رمز التحقق الخاص بك</div>
              <div dir="ltr" style="color:#00d084;font-size:44px;line-height:1.25;font-weight:800;letter-spacing:12px;padding:10px 0 8px 12px;">%s</div>
              <div style="color:#b5cbbd;font-size:12px;line-height:1.6;">6 أرقام&nbsp; • &nbsp;صالح لمدة 24 ساعة</div>
            </td></tr>
            <tr><td style="color:#d7e8dc;font-size:14px;line-height:1.9;padding-top:26px;">أدخل الرمز في شاشة تأكيد البريد الإلكتروني داخل التطبيق. إذا لم تطلب إنشاء هذا الحساب، يمكنك تجاهل هذه الرسالة.</td></tr>
          </table>
        </td></tr>
        <tr><td align="center" style="color:#a9bdb0;font-size:12px;line-height:1.8;padding:24px 18px 0;">بُني للصيدليات التي تريد أن تنمو<br><span style="color:#00d084;font-weight:700;">Pharmacy OS</span> &nbsp;•&nbsp; © 2026</td></tr>
      </table>
    </td></tr>
  </table>
</body>
</html>`, logo, html.EscapeString(code))
        text := fmt.Sprintf("أهلًا بك في Pharmacy OS.\n\nرمز تأكيد البريد الإلكتروني: %s\n\nأدخل الرمز في شاشة التحقق داخل التطبيق. الرمز صالح لمدة 24 ساعة.\n\nإذا لم تطلب إنشاء هذا الحساب، يمكنك تجاهل هذه الرسالة.", code)
        return m.send(ctx, email, "تأكيد بريدك الإلكتروني — Pharmacy OS", emailHTML, text)
}

// stepUpEmail — رمز التحقق الثانوي (الطريقة الثالثة): لحساب بلا كلمة مرور
// وبلا مفاتيح حيوية (تسجيل بجوجل) يفتح الإجراءات الحساسة بكود 6 أرقام على
// بريد الحساب. نفس الهوية البصرية لرمز التفعيل بنص أدق: «رمز تأكيد الهوية»
// وعمر 10 دقائق بدل 24 ساعة. بلا روابط (الرمز وحده هو الحرفة — أي رابط
// هنا طُعم تصيّد).
func (m *mailer) stepUpEmail(ctx context.Context, email, code string) (string, error) {
        logoURL := html.EscapeString(m.assetURL("brand/pharmacy-os-icon.png"))
        logo := `<span style="display:inline-block;width:46px;height:46px;border-radius:14px;background:#00d084;color:#06100d;font-size:20px;line-height:46px;text-align:center;font-weight:800;">P</span>`
        if logoURL != "" {
                logo = fmt.Sprintf(`<img src="%s" alt="Pharmacy OS" width="46" height="46" style="display:block;width:46px;height:46px;border:0;border-radius:14px;" />`, logoURL)
        }
        emailHTML := fmt.Sprintf(`<!doctype html>
<html lang="ar" dir="rtl">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1.0"><title>رمز تأكيد الهوية — Pharmacy OS</title></head>
<body style="margin:0;padding:0;background:#050a09;color:#f4fff8;font-family:Arial,Helvetica,sans-serif;">
  <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" style="width:100%%;background:#050a09;">
    <tr><td align="center" style="padding:32px 14px 44px;">
      <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" style="max-width:620px;">
        <tr><td style="padding:0 8px 22px;">
          <table role="presentation" cellspacing="0" cellpadding="0" border="0" align="right" dir="ltr">
            <tr>
              <td valign="middle">%s</td>
              <td style="padding-left:12px;color:#f4fff8;font-size:17px;font-weight:800;letter-spacing:-.4px;">Pharmacy <span style="color:#00d084;">OS</span></td>
            </tr>
          </table>
        </td></tr>
        <tr><td style="background:#0a1511;border:1px solid #2b493a;border-radius:24px;padding:42px 34px 38px;text-align:right;">
          <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" dir="rtl">
            <tr><td style="color:#00d084;font-size:12px;font-weight:800;letter-spacing:1.6px;text-transform:uppercase;padding-bottom:18px;">تأكيد أنه أنت</td></tr>
            <tr><td style="color:#ffffff;font-size:28px;line-height:1.5;font-weight:800;padding-bottom:14px;">رمز تأكيد الهوية</td></tr>
            <tr><td style="color:#d7e8dc;font-size:16px;line-height:1.9;padding-bottom:28px;">أُرسل لك هذا الرمز لأن أحد الإجراءات الحساسة في حسابك يحتاج تحققًا إضافيًا.</td></tr>
            <tr><td align="center" style="background:#10251b;border:1px solid #347b59;border-radius:18px;padding:25px 18px 23px;">
              <div style="color:#d7e8dc;font-size:14px;line-height:1.6;">رمز التأكيد الخاص بك</div>
              <div dir="ltr" style="color:#00d084;font-size:44px;line-height:1.25;font-weight:800;letter-spacing:12px;padding:10px 0 8px 12px;">%s</div>
              <div style="color:#b5cbbd;font-size:12px;line-height:1.6;">6 أرقام&nbsp; • &nbsp;صالح لمدة 10 دقائق</div>
            </td></tr>
            <tr><td style="color:#d7e8dc;font-size:14px;line-height:1.9;padding-top:26px;">أدخل الرمز في شاشة «تأكيد أنه أنت» داخل التطبيق. إذا لم تطلب هذا الإجراء، تجاهل هذه الرسالة — لن ينجح أحد بهذا الرمز غيرك.</td></tr>
          </table>
        </td></tr>
        <tr><td align="center" style="color:#a9bdb0;font-size:12px;line-height:1.8;padding:24px 18px 0;">بُني للصيدليات التي تريد أن تنمو<br><span style="color:#00d084;font-weight:700;">Pharmacy OS</span> &nbsp;•&nbsp; © 2026</td></tr>
      </table>
    </td></tr>
  </table>
</body>
</html>`, logo, html.EscapeString(code))
        text := fmt.Sprintf("تأكيد أنه أنت — Pharmacy OS.\n\nرمز تأكيد الهوية: %s\n\nأدخل الرمز في شاشة «تأكيد أنه أنت» داخل التطبيق. الرمز صالح لمدة 10 دقائق.\n\nإذا لم تطلب هذا الإجراء، تجاهل هذه الرسالة.", code)
        return m.send(ctx, email, "رمز تأكيد الهوية — Pharmacy OS", emailHTML, text)
}

// securityAlertEmail — the passkey-recovery security notification (owner
// rule #4): account-affecting events must reach the owner's inbox even when
// the event itself came from that inbox's account. Bilingual, minimal, no
// links (phishing bait otherwise).
func (m *mailer) securityAlertEmail(ctx context.Context, email, summary string) (string, error) {
        emailHTML := fmt.Sprintf(`<!doctype html>
<html lang="ar" dir="rtl">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1.0"><title>تنبيه أمني — Pharmacy OS</title></head>
<body style="margin:0;padding:0;background:#050a09;color:#f4fff8;font-family:Arial,Helvetica,sans-serif;">
  <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" style="width:100%%;background:#050a09;">
    <tr><td align="center" style="padding:32px 14px 44px;">
      <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" style="max-width:620px;">
        <tr><td style="background:#0a1511;border:1px solid #7a3b2e;border-radius:24px;padding:40px 34px 36px;text-align:right;">
          <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" dir="rtl">
            <tr><td style="color:#ff7a59;font-size:12px;font-weight:800;letter-spacing:1.6px;text-transform:uppercase;padding-bottom:16px;">تنبيه أمني • SECURITY ALERT</td></tr>
            <tr><td style="color:#ffffff;font-size:26px;line-height:1.5;font-weight:800;padding-bottom:16px;">نشاط أمني على حسابك</td></tr>
            <tr><td style="color:#d7e8dc;font-size:16px;line-height:2;padding-bottom:18px;">%s</td></tr>
            <tr><td style="color:#b5cbbd;font-size:13px;line-height:1.9;">إن لم تكن أنت من نفّذ هذا الإجراء، أعد تعيين كلمة المرور فورًا من شاشة الدخول وتواصل مع الدعم.</td></tr>
          </table>
        </td></tr>
        <tr><td align="center" style="color:#a9bdb0;font-size:12px;line-height:1.8;padding:24px 18px 0;"><span style="color:#00d084;font-weight:700;">Pharmacy OS</span> &nbsp;•&nbsp; © 2026</td></tr>
      </table>
    </td></tr>
  </table>
</body>
</html>`, html.EscapeString(summary))
        text := fmt.Sprintf("تنبيه أمني — Pharmacy OS\n\n%s\n\nإن لم تكن أنت من نفّذ هذا الإجراء، أعد تعيين كلمة المرور فورًا وتواصل مع الدعم.", summary)
        return m.send(ctx, email, "تنبيه أمني — Pharmacy OS", emailHTML, text)
}

// Task 163 — بريد الترحيب (نمط جوجل: تحقق ثم أهلًا بك). يُرسل مرة واحدة
// بعد نجاح تأكيد البريد لأول مرة. بلا توكنات ولا روابط إجراء — رابط
// التطبيق فقط، فلا يصبح قالبًا صيدًا تصيّريًا لو تداوله أحد.
func (m *mailer) welcomeEmail(ctx context.Context, email, displayName string) (string, error) {
        logoURL := html.EscapeString(m.assetURL("brand/pharmacy-os-icon.png"))
        logo := `<span style="display:inline-block;width:46px;height:46px;border-radius:14px;background:#00d084;color:#06100d;font-size:20px;line-height:46px;text-align:center;font-weight:800;">P</span>`
        if logoURL != "" {
                logo = fmt.Sprintf(`<img src="%s" alt="Pharmacy OS" width="46" height="46" style="display:block;width:46px;height:46px;border:0;border-radius:14px;" />`, logoURL)
        }
        name := html.EscapeString(strings.TrimSpace(displayName))
        greeting := "أهلًا بك في Pharmacy OS."
        if name != "" {
                greeting = "أهلًا بك يا " + name + " في Pharmacy OS."
        }
        appLink := ""
        if m.appURL != "" {
                appLink = fmt.Sprintf(`<tr><td align="center" style="padding-top:30px;">
  <a href="%s" style="display:inline-block;background:#00d084;color:#06100d;font-size:16px;font-weight:800;text-decoration:none;padding:14px 42px;border-radius:14px;">البدء الآن</a>
</td></tr>`, html.EscapeString(m.appURL))
        }
        emailHTML := fmt.Sprintf(`<!doctype html>
<html lang="ar" dir="rtl">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1.0"><title>حسابك جاهز — Pharmacy OS</title></head>
<body style="margin:0;padding:0;background:#050a09;color:#f4fff8;font-family:Arial,Helvetica,sans-serif;">
  <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" style="width:100%%;background:#050a09;">
    <tr><td align="center" style="padding:32px 14px 44px;">
      <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" style="max-width:620px;">
        <tr><td style="padding:0 8px 22px;">
          <table role="presentation" cellspacing="0" cellpadding="0" border="0" align="right" dir="ltr">
            <tr>
              <td valign="middle">%s</td>
              <td style="padding-left:12px;color:#f4fff8;font-size:17px;font-weight:800;letter-spacing:-.4px;">Pharmacy <span style="color:#00d084;">OS</span></td>
            </tr>
          </table>
        </td></tr>
        <tr><td style="background:#0a1511;border:1px solid #2b493a;border-radius:24px;padding:42px 34px 38px;text-align:right;">
          <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" dir="rtl">
            <tr><td style="color:#00d084;font-size:12px;font-weight:800;letter-spacing:1.6px;text-transform:uppercase;padding-bottom:18px;">تم تأكيد بريدك الإلكتروني</td></tr>
            <tr><td style="color:#ffffff;font-size:30px;line-height:1.45;font-weight:800;padding-bottom:14px;">حسابك جاهز 🎉</td></tr>
            <tr><td style="color:#d7e8dc;font-size:16px;line-height:1.9;padding-bottom:22px;">%s<br>من الآن تدير صيدليتك كاملة — مخزون وفواتير ومبيعات — من مكان واحد، وبتأمين يليق ببياناتك.</td></tr>
            <tr><td style="color:#b5cbbd;font-size:14px;line-height:1.9;padding-bottom:6px;">نصيحة أمان سريعة: من صفحة الإعدادات ← الأمان يمكنك إضافة مفتاح دخول حيوي (بصمة/وجه) ومراجعة الأجهزة الداخلة على حسابك في أي وقت.</td></tr>
            %s
          </table>
        </td></tr>
        <tr><td align="center" style="color:#a9bdb0;font-size:12px;line-height:1.8;padding:24px 18px 0;">بُني للصيدليات التي تريد أن تنمو<br><span style="color:#00d084;font-weight:700;">Pharmacy OS</span> &nbsp;•&nbsp; © 2026</td></tr>
      </table>
    </td></tr>
  </table>
</body>
</html>`, logo, greeting, appLink)
        text := fmt.Sprintf("%s\n\nتم تأكيد بريدك الإلكتروني وحسابك جاهز الآن. من الإعدادات ← الأمان يمكنك إضافة مفتاح دخول حيوي ومراجعة أجهزتك في أي وقت.", greeting)
        return m.send(ctx, email, "حسابك جاهز — أهلًا بك في Pharmacy OS", emailHTML, text)
}

// newDeviceEmail — تنبيه «دخول جديد» على نمط جوجل: جهاز + IP + وقت، بلا
// روابط (سياسة securityAlertEmail: الرابط في بريد تنبيه صيدٌ تصيّري)،
// والتوصية النصية: إن لم تكن أنت فغيّر كلمة المرور وإنهِ الأجهزة من
// الإعدادات ← الأمان.
func (m *mailer) newDeviceEmail(ctx context.Context, email, device, ip string) (string, error) {
        device = html.EscapeString(strings.TrimSpace(device))
        if device == "" {
                device = "جهاز غير معروف"
        }
        ip = html.EscapeString(strings.TrimSpace(ip))
        now := html.EscapeString(time.Now().Format("2006-01-02 15:04") + " UTC")
        ipRow := ""
        if ip != "" {
                ipRow = fmt.Sprintf(`<tr><td style="color:#d7e8dc;font-size:15px;line-height:2;"><b style="color:#f4fff8;">عنوان الشبكة:</b> <span dir="ltr">%s</span></td></tr>`, ip)
        }
        emailHTML := fmt.Sprintf(`<!doctype html>
<html lang="ar" dir="rtl">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1.0"><title>دخول جديد إلى حسابك — Pharmacy OS</title></head>
<body style="margin:0;padding:0;background:#050a09;color:#f4fff8;font-family:Arial,Helvetica,sans-serif;">
  <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" style="width:100%%;background:#050a09;">
    <tr><td align="center" style="padding:32px 14px 44px;">
      <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" style="max-width:620px;">
        <tr><td style="background:#0a1511;border:1px solid #2b493a;border-radius:24px;padding:40px 34px 36px;text-align:right;">
          <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" dir="rtl">
            <tr><td style="color:#00d084;font-size:12px;font-weight:800;letter-spacing:1.6px;text-transform:uppercase;padding-bottom:16px;">تنبيه أمني • NEW SIGN-IN</td></tr>
            <tr><td style="color:#ffffff;font-size:26px;line-height:1.5;font-weight:800;padding-bottom:14px;">دخول جديد إلى حسابك</td></tr>
            <tr><td style="color:#d7e8dc;font-size:15px;line-height:1.9;padding-bottom:20px;">تم تسجيل دخول إلى حسابك من جهاز لم نره من قبل. إن كان هذا أنت فلا حاجة لأي إجراء.</td></tr>
            <tr><td style="background:#10251b;border:1px solid #347b59;border-radius:18px;padding:22px 24px;">
              <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" dir="rtl">
                <tr><td style="color:#d7e8dc;font-size:15px;line-height:2;"><b style="color:#f4fff8;">الجهاز:</b> %s</td></tr>
                %s
                <tr><td style="color:#d7e8dc;font-size:15px;line-height:2;"><b style="color:#f4fff8;">الوقت:</b> %s</td></tr>
              </table>
            </td></tr>
            <tr><td style="color:#b5cbbd;font-size:14px;line-height:1.9;padding-top:22px;">إن لم تكن أنت: غيّر كلمة المرور فورًا من شاشة الدخول، ثم من الإعدادات ← الأمان أنهِ كل الجلسات الأخرى وراجع مفاتيح الدخول الحيوية على حسابك.</td></tr>
          </table>
        </td></tr>
        <tr><td align="center" style="color:#a9bdb0;font-size:12px;line-height:1.8;padding:24px 18px 0;"><span style="color:#00d084;font-weight:700;">Pharmacy OS</span> &nbsp;•&nbsp; © 2026</td></tr>
      </table>
    </td></tr>
  </table>
</body>
</html>`, device, ipRow, now)
        text := fmt.Sprintf("تنبيه أمني — Pharmacy OS\n\nتم تسجيل دخول جديد إلى حسابك.\nالجهاز: %s\nعنوان الشبكة: %s\n\nإن لم تكن أنت: غيّر كلمة المرور فورًا ثم أنهِ كل الجلسات الأخرى من الإعدادات ← الأمان.", device, ip)
        return m.send(ctx, email, "دخول جديد إلى حسابك — Pharmacy OS", emailHTML, text)
}

func (m *mailer) assetURL(path string) string {
        if m.appURL == "" {
                return ""
        }
        return strings.TrimRight(m.appURL, "/") + "/" + strings.TrimLeft(path, "/")
}

// Task 163 — إعادة تعيين كلمة المرور بالقالب الداكن الموحّد (كانت إنجليزية
// معزولة عن هوية النظام). الرابط هو التوكن الوحيد — تنتهي صلاحيته بعد ساعة
// وهو أحادي الاستخدام على الخادم كعادته.
func (m *mailer) resetEmail(ctx context.Context, email, token string) (string, error) {
        if m.appURL == "" {
                return "", fmt.Errorf("public app URL is not configured")
        }
        link := fmt.Sprintf("%s/reset-password?token=%s", m.appURL, html.EscapeString(token))
        emailHTML := fmt.Sprintf(`<!doctype html>
<html lang="ar" dir="rtl">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1.0"><title>إعادة تعيين كلمة المرور — Pharmacy OS</title></head>
<body style="margin:0;padding:0;background:#050a09;color:#f4fff8;font-family:Arial,Helvetica,sans-serif;">
  <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" style="width:100%%;background:#050a09;">
    <tr><td align="center" style="padding:32px 14px 44px;">
      <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" style="max-width:620px;">
        <tr><td style="background:#0a1511;border:1px solid #2b493a;border-radius:24px;padding:42px 34px 38px;text-align:right;">
          <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" dir="rtl">
            <tr><td style="color:#00d084;font-size:12px;font-weight:800;letter-spacing:1.6px;text-transform:uppercase;padding-bottom:18px;">طلب إعادة تعيين كلمة المرور</td></tr>
            <tr><td style="color:#ffffff;font-size:28px;line-height:1.5;font-weight:800;padding-bottom:14px;">نسيت كلمة المرور؟</td></tr>
            <tr><td style="color:#d7e8dc;font-size:16px;line-height:1.9;padding-bottom:26px;">وصلنا طلب لإعادة تعيين كلمة مرور حسابك. اضغط الزر لتعيين كلمة جديدة — الرابط صالح لمدة ساعة واحدة.</td></tr>
            <tr><td align="center" style="padding-bottom:24px;">
              <a href="%s" style="display:inline-block;background:#00d084;color:#06100d;font-size:16px;font-weight:800;text-decoration:none;padding:14px 42px;border-radius:14px;">تعيين كلمة مرور جديدة</a>
            </td></tr>
            <tr><td style="color:#b5cbbd;font-size:13px;line-height:1.9;">إن لم تطلب هذا الإجراء، تجاهل هذه الرسالة — كلمة مرورك الحالية لن تتغير، ولن يُرسل أي شيء آخر.</td></tr>
          </table>
        </td></tr>
        <tr><td align="center" style="color:#a9bdb0;font-size:12px;line-height:1.8;padding:24px 18px 0;"><span style="color:#00d084;font-weight:700;">Pharmacy OS</span> &nbsp;•&nbsp; © 2026</td></tr>
      </table>
    </td></tr>
  </table>
</body>
</html>`, link)
        text := fmt.Sprintf("إعادة تعيين كلمة مرور Pharmacy OS.\n\nاستخدم هذا الرابط لتعيين كلمة مرور جديدة (صالح لمدة ساعة):\n%s\n\nإن لم تطلب هذا الإجراء، تجاهل الرسالة — كلمة مرورك الحالية لن تتغير.", html.UnescapeString(link))
        return m.send(ctx, email, "إعادة تعيين كلمة المرور — Pharmacy OS", emailHTML, text)
}
