package auth

// اختبار التدفق الكامل للطريقة الثالثة للتحقق الثانوي (رمز بريد 6 أرقام):
// حساب بلا كلمة مرور (مسار جوجل) — طلب الرمز → فشل خاطئ → نجاح صحيح →
// منح يُستهلك مرة واحدة → تعيين كلمة مرور بالمنح → كلمة المرور الجديدة
// تعمل في PasswordStepUp (أرضية السلم أصبحت صلبة). ثم التهدئة وجدار
// المحاولات. TEST_DATABASE_URL لقاعدة قابلة للرمي فقط.

import (
        "context"
        "errors"
        "os"
        "testing"
        "time"

        "github.com/jackc/pgx/v5/pgxpool"
)

func newStepUpEmailService(t *testing.T) (*Service, context.Context) {
        t.Helper()
        dsn := os.Getenv("TEST_DATABASE_URL")
        if dsn == "" {
                t.Skip("TEST_DATABASE_URL not set — stepup email repro needs a disposable PostgreSQL")
        }
        pool, err := pgxpool.New(context.Background(), dsn)
        if err != nil {
                t.Fatalf("connect: %v", err)
        }
        t.Cleanup(pool.Close)
        cfg := Config{
                GoogleClientID: "test-aud",
                AccessTTL:      15 * time.Minute,
                RefreshTTL:     30 * 24 * time.Hour,
                PublicAppURL:   "http://localhost:3000",
        }
        return NewService(pool, cfg), context.Background()
}

func TestStepUpEmailFullLadderDB(t *testing.T) {
        s, ctx := newStepUpEmailService(t)

        // حساب بلا كلمة مرور — نفس ما ينتجه مسار جوجل (passwordHash فارغ).
        p, err := s.createCompanyOwner(ctx, "صيدلية التحقق", "stepup@repro.test",
                "مالك", "بلا مرور", "stepup@repro.test", "", true, "")
        if err != nil {
                t.Fatalf("create owner without password: %v", err)
        }
        if p.PasswordHash != "" {
                t.Fatalf("expected empty password hash")
        }

        // 1) طلب الرمز — يصل (raw يُعاد للمرسل البريدي فقط).
        code, sent, _, err := s.RequestStepUpEmailCode(ctx, p, StepUpPurposeChangePassword)
        if err != nil || !sent {
                t.Fatalf("request code: err=%v sent=%v", err, sent)
        }
        if len(code) != 6 {
                t.Fatalf("expected 6-digit code, got %q", code)
        }

        // 2) رمز خاطئ يُرفض ولا يحرق الصحيح.
        if _, err := s.VerifyStepUpEmailCode(ctx, p, StepUpPurposeChangePassword, "000000"); !errors.Is(err, ErrInvalidToken) {
                t.Fatalf("wrong code should be ErrInvalidToken, got %v", err)
        }

        // 3) الرمز الصحيح يسكّ منحًا يُستهلك مرة واحدة بالغرض المحدد.
        grant, err := s.VerifyStepUpEmailCode(ctx, p, StepUpPurposeChangePassword, code)
        if err != nil {
                t.Fatalf("verify correct code: %v", err)
        }
        ok, err := s.ConsumeStepUpGrant(ctx, p, StepUpPurposeChangePassword, grant)
        if err != nil || !ok {
                t.Fatalf("consume grant: ok=%v err=%v", ok, err)
        }
        if ok2, _ := s.ConsumeStepUpGrant(ctx, p, StepUpPurposeChangePassword, grant); ok2 {
                t.Fatalf("grant must be single-use")
        }

        // 4) المنح يعيّن كلمة المرور، وكلمة المرور الجديدة تفتح السلم كله.
        if err := s.UpdatePasswordWithGrant(ctx, p, "Str0ng!NewPass"); err != nil {
                t.Fatalf("update password with grant: %v", err)
        }
        fresh, err := s.FindPrincipal(ctx, p.Email, p.Type, "")
        if err != nil {
                t.Fatalf("reload principal: %v", err)
        }
        if _, err := s.PasswordStepUp(ctx, fresh, StepUpPurposeManagePasskeys, "Str0ng!NewPass"); err != nil {
                t.Fatalf("password step-up with new password: %v", err)
        }
}

func TestStepUpEmailCooldownAndWallDB(t *testing.T) {
        s, ctx := newStepUpEmailService(t)
        p, err := s.createCompanyOwner(ctx, "صيدلية الجدار", "wall@repro.test",
                "مالك", "جدار", "wall@repro.test", "", true, "")
        if err != nil {
                t.Fatalf("create owner: %v", err)
        }
        code, sent, _, err := s.RequestStepUpEmailCode(ctx, p, StepUpPurposeManagePasskeys)
        if err != nil || !sent {
                t.Fatalf("request code: err=%v sent=%v", err, sent)
        }
        // التهدئة: طلب فوري ثانٍ لا يُرسل شيئًا ويحمل مهلة متبقية موجبة.
        _, sent2, remaining2, err := s.RequestStepUpEmailCode(ctx, p, StepUpPurposeManagePasskeys)
        if err != nil {
                t.Fatalf("second request: %v", err)
        }
        if sent2 || remaining2 <= 0 {
                t.Fatalf("expected cooldown: sent=%v remaining=%v", sent2, remaining2)
        }
        // جدار المحاولات: التخمين يُقفل قبل تجاوز 11 محاولة.
        locked := false
        for i := 0; i < 12; i++ {
                if _, err := s.VerifyStepUpEmailCode(ctx, p, StepUpPurposeManagePasskeys, "111111"); errors.Is(err, ErrVerificationLocked) {
                        locked = true
                        break
                }
        }
        if !locked {
                t.Fatalf("attempt wall never locked")
        }
        // الرمز الصحيح بعد القفل يُرفض أيضًا — الجدار يسبق أي مقارنة.
        if _, err := s.VerifyStepUpEmailCode(ctx, p, StepUpPurposeManagePasskeys, code); !errors.Is(err, ErrVerificationLocked) {
                t.Fatalf("correct code must stay locked after the wall: %v", err)
        }
}

func TestUserPayloadHasPasswordFlag(t *testing.T) {
        if userPayload(&Principal{PasswordHash: "x"})["has_password"] != true {
                t.Fatalf("has_password must be true with a hash")
        }
        if userPayload(&Principal{})["has_password"] != false {
                t.Fatalf("has_password must be false without a hash")
        }
}
