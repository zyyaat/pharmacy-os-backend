-- إشعارات الأمان داخل التطبيق — المرآة الرسمية للبريد الأمني (api_level 124).
-- كل حدث أمني يرسل بريدًا (دخول جهاز جديد، إضافة مفتاح حيوي، استعادة الحساب،
-- تغيير/إعادة تعيين كلمة المرور) يستدعي هوك auth.Service.RecordSecurityEventByEmail
-- فيسجل صفًا مطابقًا هنا، فيظهر في جرس لوحة القيادة نفسه. القراءة شخصية:
-- صاحب الحساب وحده يرى أحداثه، والفشل هوك لا يفشل المسار الأصلي أبدًا
-- (أفضل جهد — البريد يبقى المصدر الخارجي).
CREATE TABLE IF NOT EXISTS security_notifications (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    company_user_id UUID NOT NULL REFERENCES company_users(id) ON DELETE CASCADE,
    kind VARCHAR(32) NOT NULL CHECK (kind IN ('new_device', 'passkey_added', 'passkey_recovery', 'password_changed')),
    summary TEXT NOT NULL,
    meta JSONB NOT NULL DEFAULT '{}'::jsonb,
    read_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- قائمة الجرس: آخر الأحداث لصاحب الحساب
CREATE INDEX IF NOT EXISTS idx_security_notifications_recent
    ON security_notifications (company_user_id, created_at DESC);

-- عدّاد غير المقروء: فهرس جزئي صغير يخدم استعلام الشارة وحده
CREATE INDEX IF NOT EXISTS idx_security_notifications_unread
    ON security_notifications (company_user_id)
    WHERE read_at IS NULL;
