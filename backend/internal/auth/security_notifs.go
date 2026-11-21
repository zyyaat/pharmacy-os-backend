package auth

// إشعارات الأمان داخل التطبيق — المرآة الرسمية للبريد الأمني.
//
// الهوك الواضحة الوحيدة: RecordSecurityEventByEmail تُستدعى من كل موقع يرسل
// بريدًا أمنيًا (دخول جهاز جديد، إضافة مفتاح حيوي، استعادة الحساب، تغيير/إعادة
// تعيين كلمة المرور) فتسجل الحدث في جدول security_notifications (ميجريشن 56)
// ليظهر في جرس لوحة القيادة بجانب بريد الأمان نفسه — «نفس الإشعارات التي
// تُرسل للبريد» بالحرف.
//
// عقود صارمة:
//   - أفضل جهد: فشل التسجيل يهبط في اللوج فقط ولا يفشل المسار الأصلي أبدًا
//     (نفس ميزانية notifyNewDeviceAsync — الإشعار مرآة لا بوابة).
//   - الحل بالبريد: مواقع الاستدعاء تمتلك البريد دائمًا (بعضها بلا جلسة مثل
//     الاستعادة)، فالتحول إلى company_users يتم هنا بLIMIT 1 على LOWER(email).
//     حساب منصة بنفس بريد مستخدم شركة سيرى أحداثه (صندوق واحد لحساب واحد) —
//     نفس سلوك بريد الأمان القائم بالحرف.
//   - القراءة شخصية: ListSecurityNotifications/MarkSecurityNotificationsRead
//     تقيدان بprincipal.ID فلا يمكن لصاحب حساب رؤية غيره مهما حاول.

import (
	"context"
	"encoding/json"
	"log"
	"time"
)

// SecurityNotification — صف واحد في جرس الأمان (شكل الاستجابة للواجهة).
type SecurityNotification struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	Summary   string          `json:"summary"`
	Meta      json.RawMessage `json:"meta"`
	ReadAt    *time.Time      `json:"read_at"`
	CreatedAt time.Time       `json:"created_at"`
}

// أنواع الأحداث المعروفة — نفس عائلة بريد الأمان.
const (
	notifKindNewDevice       = "new_device"
	notifKindPasskeyAdded    = "passkey_added"
	notifKindPasskeyRecovery = "passkey_recovery"
	notifKindPasswordChanged = "password_changed"
)

// RecordSecurityEventByEmail — الهوك. يحول البريد إلى company_user (إن وجد)
// ويسجل الحدث. لا تُرجع خطأ: فشل المرآة لا يعني فشل الحدث الأمني.
func (s *Service) RecordSecurityEventByEmail(ctx context.Context, email, kind, summary string, meta map[string]any) {
	if email == "" || kind == "" || summary == "" {
		return
	}
	payload := []byte("{}")
	if meta != nil {
		if encoded, err := json.Marshal(meta); err == nil {
			payload = encoded
		}
	}
	const q = `INSERT INTO security_notifications (company_user_id, kind, summary, meta)
	           SELECT id, $2, $3, $4::jsonb FROM company_users WHERE LOWER(email) = LOWER($1) LIMIT 1`
	if _, err := s.db.Exec(ctx, q, email, kind, summary, string(payload)); err != nil {
		log.Printf("[SECURITY-NOTIF] record failed kind=%s: %v", kind, err)
	}
}

// RecordSecurityEventAsync — الهوك خارج مسار الطلب بنمط notifyNewDeviceAsync:
// مهلة مستقلة وpanic-safe، فلا يلمس إطلاقًا زمن استجابة المسار الذي استدعاها.
func (s *Service) RecordSecurityEventAsync(email, kind, summary string, meta map[string]any) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[SECURITY-NOTIF] panic recovered kind=%s: %v", kind, r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.RecordSecurityEventByEmail(ctx, email, kind, summary, meta)
	}()
}

// ListSecurityNotifications → GET /auth/pharmacy/security-notifications:
// آخر 20 حدثًا لصاحب الحساب + عدد غير المقروء للشارة.
func (s *Service) ListSecurityNotifications(ctx context.Context, principal *Principal) ([]SecurityNotification, int, error) {
	const listQ = `SELECT id, kind, summary, meta, read_at, created_at
	               FROM security_notifications
	               WHERE company_user_id = $1
	               ORDER BY created_at DESC, id DESC
	               LIMIT 20`
	rows, err := s.db.Query(ctx, listQ, principal.ID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := []SecurityNotification{}
	for rows.Next() {
		var n SecurityNotification
		var meta []byte
		if err := rows.Scan(&n.ID, &n.Kind, &n.Summary, &meta, &n.ReadAt, &n.CreatedAt); err != nil {
			return nil, 0, err
		}
		// meta يُعاد كما هو للواجهة — pgx يسلم JSONB كنص خام
		n.Meta = json.RawMessage(meta)
		items = append(items, n)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	var unread int
	const unreadQ = `SELECT COUNT(*) FROM security_notifications
	                 WHERE company_user_id = $1 AND read_at IS NULL`
	if err := s.db.QueryRow(ctx, unreadQ, principal.ID).Scan(&unread); err != nil {
		return nil, 0, err
	}
	return items, unread, nil
}

// MarkSecurityNotificationsRead → POST /auth/pharmacy/security-notifications/read:
// تعليم كل أحداث صاحب الحساب كمقروءة (NOW() على الخادم — لا يسافر زمن من العميل).

// MarkSecurityNotificationsRead يعلم كل غير المقروء لصاحب الحساب.
func (s *Service) MarkSecurityNotificationsRead(ctx context.Context, principal *Principal) (int64, error) {
	tag, err := s.db.Exec(ctx, `UPDATE security_notifications SET read_at = NOW()
	                            WHERE company_user_id = $1 AND read_at IS NULL`, principal.ID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
