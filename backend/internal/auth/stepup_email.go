package auth

// الطريقة الثالثة للتحقق الثانوي — رمز بريدي من 6 أرقام.
//
// حسابات جوجل بلا كلمة مرور وبلا مفاتيح حيوية كانت تُغلق خارج كل إجراء
// حساس: BeginStepUp يجيب ErrNoActivePasskey فتنقلهم الواجهة إلى كلمة
// المرور، وPasswordStepUp يرفض because PasswordHash == "" — طريق مسدود
// (بلاغ المالك). الرمز هنا يمر بعقود العائلة حرفيًا: غرض منح محدد مسبقًا
// من القائمة المسموحة، منح طازج أحادي الاستخدام method=email_code، ونفس
// جدار المحاولات العام (AUD-004) قبل أي مقارنة، وبريد الإرسال نفسه مع
// ميزانية البريد. ملف رمز التحدي داخل auth_email_tokens بغرض مخصص —
// بلا أي مهاجرة (الجدول موجود منذ التأسيس).

import (
        "context"
        "log"
        "time"
)

// StepUpEmailChallengePurpose — غرض رمز التحدي البريدي داخل auth_email_tokens
// (ليس غرض منح: المنح يبقى change_password/manage_passkeys كما هي — الرمز
// مجرد طريقة لإصدارها، مثل passkey/password).
const StepUpEmailChallengePurpose = "stepup_email"

// RequestStepUpEmailCode يبني رمز 6 أرقام مرتبطًا بالحساب إذا استحق الإرسال
// (تهدئة الإعادة المشتركة عبر CreateEmailTokenIfDue — إعادة الإرسال لا
// تُجدّد ميزانية التخمين). رمز التحدي قصير العمر (10 دقائق في
// tokenLifetime) بعكس رمز تفعيل البريد (24 ساعة) لأنه تحقق حي لا رابط تفعيل.
func (s *Service) RequestStepUpEmailCode(ctx context.Context, principal *Principal, purpose string) (string, bool, time.Duration, error) {
        if err := validateStepUpPurpose(purpose); err != nil {
                return "", false, 0, err
        }
        return s.CreateEmailTokenIfDue(ctx, principal, StepUpEmailChallengePurpose)
}

// VerifyStepUpEmailCode يحرق أحدث رمز غير مستهلك لهذا الحساب ويسكّ المنح
// الطازج. جدار المحاولات قبل أي مقارنة — الترتيب نفسه الذي فرضه AUD-004 في
// ConsumeEmailVerificationCode حتى لا يستفيد التخمين من فشله بجولة قاعدة
// بيانات أرخص. الرمز يُقارن بأحدث رمز صالح فقط (الرمز الأحدث يبطل ما قبله
// عمليًا: كله صالح حتى 10 دقائق لكن الاستهلاك واحداً بواحد والأحدث هو
// ما يصل صندوق البريد أخيرًا).
func (s *Service) VerifyStepUpEmailCode(ctx context.Context, principal *Principal, purpose, code string) (string, error) {
        if err := validateStepUpPurpose(purpose); err != nil {
                return "", err
        }
        if !isVerificationCode(code) {
                return "", ErrInvalidToken
        }
        count, err := s.registerVerifyAttempt(ctx, principal.Email)
        if err != nil {
                return "", err
        }
        if count > VerifyAttemptLimit {
                return "", ErrVerificationLocked
        }
        tag, err := s.db.Exec(ctx, `
                UPDATE auth_email_tokens SET used_at = NOW()
                WHERE id = (
                        SELECT id FROM auth_email_tokens
                        WHERE principal_type = $1 AND principal_id = $2::uuid
                          AND purpose = $3 AND used_at IS NULL AND expires_at > NOW()
                        ORDER BY created_at DESC
                        LIMIT 1
                )
                  AND token_hash = $4
        `, principal.Type, principal.ID, StepUpEmailChallengePurpose, tokenHash(code))
        if err != nil {
                // عطل بنية تحتية لا يتنكر في هيئة «رمز خاطئ» (درس Task 135).
                log.Printf("[stepup-email] token consume failed (infra): %v", err)
                return "", err
        }
        if tag.RowsAffected() != 1 {
                s.auditPasskeyEvent(ctx, principal, "stepup.failed", "warning",
                        "فشل التحقق الثانوي برمز البريد", map[string]any{"purpose": purpose})
                return "", ErrInvalidToken
        }
        token, err := s.issueStepUpGrant(ctx, principal, purpose, "email_code", nil, stepUpGrantTTL)
        if err != nil {
                return "", err
        }
        s.auditPasskeyEvent(ctx, principal, "stepup.granted", "info",
                "تحقق ثانوي ناجح — إذن محدود الغرض", map[string]any{
                        "purpose": purpose, "method": "email_code",
                })
        return token, nil
}
