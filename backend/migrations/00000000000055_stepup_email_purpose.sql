-- مهاجرة 55 — الطريقة الثالثة للتحقق الثانوي: رمز بريد 6 أرقام.
--
-- جدول auth_email_tokens مقيّد بقيم purpose منذ مهاجرة 6
-- (verify_email، reset_password). الطريقة الجديدة تخزّن رمز التحدي
-- بغرض مخصص stepup_email (10 دقائق عمره) — قيمة رابعة في القيد نفسه.
-- ومنح الترقية auth_stepup_grants مقيّد بطريقتين منذ مهاجرة 39
-- (passkey، password) — القيمة الثالثة email_code تُضاف للقيد نفسه.
-- التوسيع additive بحت: إعادة إنشاء القيدين بقائمتين أوسع بلا مساس
-- بأي صف قائم. الصيغة IF EXISTS ملزمة هنا (AUD-007): إعادة إنشاء قيد
-- قائم الاسم صيانة قابلة لإعادة التشغيل — الحارس يسمح بها لا ياخذها
-- مع السلامة لا بجملة DROP CONSTRAINT المجردة التي تُحاصر في الإنتاج
-- (نفس نمط مهاجرات 19/36/44).

ALTER TABLE auth_email_tokens DROP CONSTRAINT IF EXISTS auth_email_tokens_purpose_check;
ALTER TABLE auth_email_tokens ADD CONSTRAINT auth_email_tokens_purpose_check
        CHECK (purpose IN ('verify_email', 'reset_password', 'stepup_email'));

ALTER TABLE auth_stepup_grants DROP CONSTRAINT IF EXISTS auth_stepup_grants_method_check;
ALTER TABLE auth_stepup_grants ADD CONSTRAINT auth_stepup_grants_method_check
        CHECK (method IN ('passkey', 'password', 'email_code'));
