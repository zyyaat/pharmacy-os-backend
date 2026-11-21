package auth

// اختبار تشخيصي لإنتاج فشل «Could not complete the Google sign-in» الحي:
// يمشي كل مسارات جوجل على قاعدة حقيقية مهاجرة كاملة (1..54) بترتيب الإنتاج
// نفسه: إنشاء حساب جديد → جلسة → عودة بالـsub → ربط بريد قائم → GoogleLogin
// كاملة عبر مُحقق على JWKS محلي. TEST_DATABASE_URL لقاعدة قابلة للرمي فقط.

import (
        "context"
        "os"
        "testing"
        "time"

        "github.com/jackc/pgx/v5/pgxpool"
)

func newReproService(t *testing.T) (*Service, context.Context) {
        t.Helper()
        dsn := os.Getenv("TEST_DATABASE_URL")
        if dsn == "" {
                t.Skip("TEST_DATABASE_URL not set — google repro needs a disposable PostgreSQL")
        }
        pool, err := pgxpool.New(context.Background(), dsn)
        if err != nil {
                t.Fatalf("connect: %v", err)
        }
        t.Cleanup(pool.Close)
        cfg := Config{
                GoogleClientID: "test-aud",
                GoogleJWKSURL:  "https://unused.invalid/certs",
                AccessTTL:      15 * time.Minute,
                RefreshTTL:     30 * 24 * time.Hour,
                PublicAppURL:   "http://localhost:3000",
                MailFromEmail:  "test@example.com",
                MailFromName:   "Repro",
        }
        return NewService(pool, cfg), context.Background()
}

func TestGoogleReproNewAccountSessionAndReloginDB(t *testing.T) {
        s, ctx := newReproService(t)
        meta := RequestMeta{IPAddress: "127.0.0.1", UserAgent: "repro"}

        claims := &googleClaims{Sub: "repro-sub-1", Email: "newowner@example.com", Name: "أحمد تجربة"}
        p, created, err := s.resolveGooglePrincipal(ctx, claims)
        if err != nil {
                t.Fatalf("resolve new account: %v", err)
        }
        if !created {
                t.Fatalf("expected creation for unknown email")
        }
        tokens, err := s.createSession(ctx, p, PharmacyRealm, meta)
        if err != nil {
                t.Fatalf("createSession after creation: %v", err)
        }
        if tokens.AccessToken == "" || tokens.RefreshToken == "" {
                t.Fatalf("empty session tokens")
        }

        p2, created2, err := s.resolveGooglePrincipal(ctx, claims)
        if err != nil {
                t.Fatalf("resolve by sub: %v", err)
        }
        if created2 || p2.ID != p.ID {
                t.Fatalf("sub relogin mismatch: created=%v id=%s≠%s", created2, p2.ID, p.ID)
        }
}

func TestGoogleReproLinkExistingOwnerDB(t *testing.T) {
        s, ctx := newReproService(t)

        owner, err := s.RegisterCompany(ctx, "صيدلية قائمة", "linkowner@example.com",
                "مالك", "قائم", "linkowner@example.com", "Str0ng!Passw0rd#")
        if err != nil {
                t.Fatalf("register company: %v", err)
        }
        claims := &googleClaims{Sub: "repro-sub-2", Email: "linkowner@example.com", Name: "مالك قائم"}
        p, created, err := s.resolveGooglePrincipal(ctx, claims)
        if err != nil {
                t.Fatalf("resolve existing email: %v", err)
        }
        if created {
                t.Fatalf("existing email must not create a second account")
        }
        if p.ID != owner.ID {
                t.Fatalf("linked principal %s != registered owner %s", p.ID, owner.ID)
        }
}

func TestGoogleReproFullLoginWithFakeJWKSDB(t *testing.T) {
        s, ctx := newReproService(t)
        keys := newGoogleTestKeys(t)
        s.google = newGoogleVerifier("test-aud", keys.server.URL)
        meta := RequestMeta{IPAddress: "127.0.0.1", UserAgent: "repro"}

        token := signGoogleToken(t, keys, func() map[string]interface{} {
                c := baseGoogleClaims()
                c["sub"] = "repro-sub-3"
                c["email"] = "fulllogin@example.com"
                c["aud"] = "test-aud"
                return c
        }())
        principal, tokens, err := s.GoogleLogin(ctx, token, PharmacyRealm, meta)
        if err != nil {
                t.Fatalf("GoogleLogin full path: %v", err)
        }
        if tokens.AccessToken == "" {
                t.Fatalf("no access token")
        }
        // عودة كاملة عبر GoogleLogin: يجب أن يُحسم بالـsub لا بالإنشاء
        claims := &googleClaims{Sub: "repro-sub-3", Email: "fulllogin@example.com", Name: "أحمد"}
        p, created, err := s.resolveGooglePrincipal(ctx, claims)
        if err != nil || created || p.ID != principal.ID {
                t.Fatalf("full relogin mismatch: err=%v created=%v", err, created)
        }
}
