package auth

// تسجيل الدخول بجوجل — المسار الكامل:
//
//        العميل (ويب GIS / موبايل google_sign_in) يحصل على ID token من جوجل
//        POST /auth/google {id_token}
//          → تحقق التوقيع مقابل مفاتيح جوجل العامة (JWKS) + aud + email_verified
//          → إيجاد الحساب: google_sub أولًا (ربط دائم) ثم البريد (التوكن إثبات
//            ملكيته)، وأول بريد غير موجود ينشئ حساب مالك كاملًا بمسار التسجيل نفسه
//          → createSession + كوكيز + نفس استجابة /auth/pharmacy/login حرفيًا
//
// لا كلمات مرور هنا: قفل المحاولات (locked_until) يخص قناة كلمة المرور فقط؛
// حاجز هذه النقطة هو توقيع جوجل نفسه + rate limit العائلة المشترك.

import (
        "context"
        "crypto/rand"
        "crypto/rsa"
        "encoding/base64"
        "encoding/json"
        "errors"
        "fmt"
        "math"
        "math/big"
        "net/http"
        "strings"
        "sync"
        "time"
        "unicode/utf8"

        "github.com/golang-jwt/jwt/v5"
        "github.com/jackc/pgx/v5"
        "golang.org/x/crypto/bcrypt"
)

var (
        ErrGoogleDisabled     = errors.New("google login is not configured")
        ErrGoogleTokenInvalid = errors.New("invalid google identity token")
)

// googleClaims is the subset of the Google ID token payload this flow needs.
type googleClaims struct {
        Sub        string
        Email      string
        Name       string
        GivenName  string
        FamilyName string
}

// ---------------------------------------------------------------- المُحقق

// googleJWKSTTL — مفاتيح جوجل تدور على أيام؛ 6 ساعات كفاية، وkid غير
// المعروف يفرض تحديثًا فوريًا على أي حال (فشل مفتاح واحد لا يبقى عالقًا
// حتى نهاية صلاحية الكاش).
const googleJWKSTTL = 6 * time.Hour

type googleJWK struct {
        Kty string `json:"kty"`
        Kid string `json:"kid"`
        Alg string `json:"alg"`
        Use string `json:"use"`
        N   string `json:"n"`
        E   string `json:"e"`
}

type googleJWKS struct {
        Keys []googleJWK `json:"keys"`
}

type googleVerifier struct {
        clientID string
        jwksURL  string
        http     *http.Client

        mu      sync.Mutex
        keys    map[string]*rsa.PublicKey
        fetched time.Time
}

func newGoogleVerifier(clientID, jwksURL string) *googleVerifier {
        return &googleVerifier{
                clientID: clientID,
                jwksURL:  jwksURL,
                http:     &http.Client{Timeout: 10 * time.Second},
        }
}

// verify يتحقق من التوقيع وكل الادعاءات الحرجة ويعيد المجموعة الفرعية
// المطلوبة للربط. أي شك = خطأ (fail-closed).
func (v *googleVerifier) verify(ctx context.Context, raw string) (*googleClaims, error) {
        if strings.TrimSpace(raw) == "" {
                return nil, errors.New("empty id token")
        }
        head, _, err := jwt.NewParser().ParseUnverified(raw, jwt.MapClaims{})
        if err != nil {
                return nil, err
        }
        kid, _ := head.Header["kid"].(string)
        if kid == "" {
                return nil, errors.New("token header has no kid")
        }
        key, err := v.publicKeyFor(ctx, kid)
        if err != nil {
                return nil, err
        }

        claims := jwt.MapClaims{}
        if _, err := jwt.NewParser(
                jwt.WithValidMethods([]string{"RS256"}),
                jwt.WithAudience(v.clientID),
                jwt.WithExpirationRequired(),
        ).ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) { return key, nil }); err != nil {
                return nil, err
        }

        iss, _ := claims["iss"].(string)
        if iss != "accounts.google.com" && iss != "https://accounts.google.com" {
                return nil, fmt.Errorf("unexpected issuer %q", iss)
        }
        sub, _ := claims["sub"].(string)
        if sub == "" {
                return nil, errors.New("token has no sub")
        }
        email, _ := claims["email"].(string)
        if email == "" {
                return nil, errors.New("token has no email")
        }
        if verified, ok := claims["email_verified"].(bool); !ok || !verified {
                return nil, errors.New("google email is not verified")
        }
        return &googleClaims{
                Sub:        sub,
                Email:      email,
                Name:       strClaim(claims["name"]),
                GivenName:  strClaim(claims["given_name"]),
                FamilyName: strClaim(claims["family_name"]),
        }, nil
}

func strClaim(v any) string {
        s, _ := v.(string)
        return strings.TrimSpace(s)
}

func (v *googleVerifier) publicKeyFor(ctx context.Context, kid string) (*rsa.PublicKey, error) {
        v.mu.Lock()
        defer v.mu.Unlock()
        if v.keys == nil || time.Since(v.fetched) > googleJWKSTTL {
                if err := v.refresh(ctx); err != nil {
                        return nil, err
                }
        }
        if k, ok := v.keys[kid]; ok {
                return k, nil
        }
        // kid غير معروف: تدوير مفاتيح من جوجل — تحديث إجباري واحد ثم حكم.
        if err := v.refresh(ctx); err != nil {
                return nil, err
        }
        if k, ok := v.keys[kid]; ok {
                return k, nil
        }
        return nil, fmt.Errorf("unknown google signing kid %q", kid)
}

func (v *googleVerifier) refresh(ctx context.Context) error {
        req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
        if err != nil {
                return err
        }
        resp, err := v.http.Do(req)
        if err != nil {
                return fmt.Errorf("fetch google jwks: %w", err)
        }
        defer resp.Body.Close()
        if resp.StatusCode != http.StatusOK {
                return fmt.Errorf("fetch google jwks: status %d", resp.StatusCode)
        }
        var set googleJWKS
        if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
                return fmt.Errorf("decode google jwks: %w", err)
        }
        keys := make(map[string]*rsa.PublicKey, len(set.Keys))
        for _, k := range set.Keys {
                if k.Kty != "RSA" || k.Kid == "" || k.N == "" || k.E == "" {
                        continue
                }
                if (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
                        continue
                }
                nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
                if err != nil {
                        continue
                }
                eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
                if err != nil {
                        continue
                }
                e := new(big.Int).SetBytes(eBytes).Int64()
                if e <= 0 || e > math.MaxInt32 {
                        continue
                }
                keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: int(e)}
        }
        if len(keys) == 0 {
                return errors.New("google jwks carries no usable RSA keys")
        }
        v.keys = keys
        v.fetched = time.Now()
        return nil
}

// ------------------------------------------------------------ الحساب

// findCompanyUserByGoogleSub — نفس شكل findCompanyUser بمفتاح الربط الدائم.
func (s *Service) findCompanyUserByGoogleSub(ctx context.Context, sub string) (*Principal, error) {
        var p Principal
        err := s.db.QueryRow(ctx, `
                SELECT cu.id::text, cu.email, cu.first_name, cu.last_name, COALESCE(cu.display_name, ''),
                       cu.role::text, cu.company_id::text, COALESCE(p.id::text, ''),
                       COALESCE(b.id::text, ''), COALESCE(cu.password_hash, ''),
                       cu.is_active, cu.email_verified_at IS NOT NULL, cu.login_attempts, cu.locked_until,
                       cu.permission_version, COALESCE(cu.locale, 'ar'),
                       COALESCE((p.settings->'onboarding'->>'completed') = 'true', false)
                FROM company_users cu
                LEFT JOIN accounts a ON a.company_id = cu.company_id AND a.deleted_at IS NULL
                LEFT JOIN pharmacies p ON p.account_id = a.id AND p.is_active = true AND p.is_main_branch = true
                LEFT JOIN branches b ON b.id = p.default_branch_id AND b.is_active = true
                WHERE cu.google_sub = $1
                  AND cu.deleted_at IS NULL
                LIMIT 1
        `, sub).Scan(
                &p.ID, &p.Email, &p.FirstName, &p.LastName, &p.DisplayName, &p.Role,
                &p.CompanyID, &p.PharmacyID, &p.BranchID, &p.PasswordHash, &p.IsActive, &p.EmailVerified,
                &p.LoginAttempts, &p.LockedUntil, &p.PermissionVersion, &p.Locale, &p.Onboarded,
        )
        if err != nil {
                return nil, err
        }
        p.Type = CompanyUserPrincipal
        return &p, nil
}

// bindGoogleIdentity يثبّت الربط ويثبت البريد عند الحاجة. فشل الربط بسبب
// التعارض (صف محذوف يحمل نفس sub) ليس فشل دخول — الدخول بالبريد صحيح أصلًا.
func (s *Service) bindGoogleIdentity(ctx context.Context, principalType, principalID, sub string, alreadyVerified bool) error {
        if principalType == CompanyUserPrincipal {
                if _, err := s.db.Exec(ctx, `
                        UPDATE company_users SET
                            google_sub = $2,
                            email_verified_at = COALESCE(email_verified_at, CASE WHEN $3 THEN NOW() END)
                        WHERE id = $1::uuid AND google_sub IS NULL
                `, principalID, sub, !alreadyVerified); err != nil {
                        if strings.Contains(err.Error(), "ux_company_users_google_sub") ||
                                strings.Contains(strings.ToLower(err.Error()), "duplicate") {
                                return nil
                        }
                        return fmt.Errorf("bind google identity: %w", err)
                }
                return nil
        }
        if alreadyVerified {
                return nil
        }
        if _, err := s.db.Exec(ctx, `
                UPDATE employees SET email_verified_at = COALESCE(email_verified_at, NOW())
                WHERE id = $1::uuid
        `, principalID); err != nil {
                return fmt.Errorf("verify employee email: %w", err)
        }
        return nil
}

// resolveGooglePrincipal — ترتيب الحسم: sub ثم البريد (مالك ثم موظف) ثم
// إنشاء حساب مالك جديد. يعيد هل الحساب أُنشئ الآن (لأغراض السجل فقط).
func (s *Service) resolveGooglePrincipal(ctx context.Context, claims *googleClaims) (*Principal, bool, error) {
        email := normalizeEmail(claims.Email)

        if p, err := s.findCompanyUserByGoogleSub(ctx, claims.Sub); err == nil {
                return p, false, nil
        } else if !errors.Is(err, pgx.ErrNoRows) {
                return nil, false, fmt.Errorf("lookup google identity: %w", err)
        }

        p, cerr := s.findCompanyUser(ctx, email, "")
        if cerr == nil {
                if err := s.bindGoogleIdentity(ctx, CompanyUserPrincipal, p.ID, claims.Sub, p.EmailVerified); err != nil {
                        return nil, false, err
                }
                if !p.EmailVerified {
                        p.EmailVerified = true
                }
                return p, false, nil
        }
        if !errors.Is(cerr, pgx.ErrNoRows) {
                return nil, false, cerr
        }

        e, eerr := s.findEmployee(ctx, email, "")
        if eerr == nil {
                if err := s.bindGoogleIdentity(ctx, EmployeePrincipal, e.ID, claims.Sub, e.EmailVerified); err != nil {
                        return nil, false, err
                }
                if !e.EmailVerified {
                        e.EmailVerified = true
                }
                return e, false, nil
        }
        if !errors.Is(eerr, pgx.ErrNoRows) {
                return nil, false, eerr
        }

        created, err := s.RegisterCompanyGoogle(ctx, googleDisplayName(claims), email, claims.Sub)
        if err != nil {
                return nil, false, err
        }
        return created, true, nil
}

func googleDisplayName(claims *googleClaims) string {
        if claims.Name != "" {
                return claims.Name
        }
        return claims.GivenName
}

// splitGoogleName يقسم الاسم المعروض إلى أولي/أخير، والفراغ يعود لبريد جوجل.
func splitGoogleName(display, email string) (string, string) {
        display = strings.TrimSpace(display)
        if display == "" {
                local := email
                if i := strings.Index(local, "@"); i > 0 {
                        local = local[:i]
                }
                return local, ""
        }
        parts := strings.Fields(display)
        first := parts[0]
        var last string
        if len(parts) > 1 {
                last = strings.Join(parts[1:], " ")
        }
        return first, last
}

// companyNameFromIdentity اسم الشركة/الصيدلية لأول حساب بجوجل: الاسم المعروض
// أو الجزء المحلي من البريد — محدود بـ 255 حرفًا (حد عمود name)، وقابل
// للتعديل من المالك لاحقًا.
func companyNameFromIdentity(display, email string) string {
        name := strings.TrimSpace(display)
        if name == "" {
                name = email
                if i := strings.Index(name, "@"); i > 0 {
                        name = name[:i]
                }
        }
        if utf8.RuneCountInString(name) > 255 {
                name = string([]rune(name)[:255])
        }
        return name
}

// RegisterCompanyGoogle — أول دخول بجوجل لبريد غير موجود: حساب مالك كامل
// (شركة + صيدلية + فرع + اشتراك تجريبي + صلاحيات المالك) بمسار التسجيل
// نفسه، لكن بلا كلمة مرور قابلة للاستخدام (hash عشوائي — الدخول بكلمة
// المرور معطل حتى يعيدها المالك من forgot-password/change-password)،
// وبريد موثق فورًا لأن توكن جوجل هو إثبات الملكية، وgoogle_sub مربوط
// من أول لحظة.
func (s *Service) RegisterCompanyGoogle(ctx context.Context, displayName, email, googleSub string) (*Principal, error) {
        seed := make([]byte, 32)
        if _, err := rand.Read(seed); err != nil {
                return nil, err
        }
        hash, err := bcrypt.GenerateFromPassword(seed, bcrypt.DefaultCost)
        if err != nil {
                return nil, err
        }
        first, last := splitGoogleName(displayName, email)
        return s.createCompanyOwner(ctx,
                companyNameFromIdentity(displayName, email), email,
                first, last, email,
                string(hash), true, googleSub)
}

// GoogleLogin — البديل الكامل لتسجيل الدخول بكلمة المرور: تحقق التوكن،
// حسم الحساب، ثم نفس ذيل Login حرفيًا (recordSuccessfulLogin → createSession
// → auditLoginSuccess). قناة جوجل لا تزيد عدادات فشل كلمة المرور ولا تقبل
// بها القفل: القفل يحمي قناة كلمة المرور فقط، وهنا الإثبات جاء من جوجل.
func (s *Service) GoogleLogin(ctx context.Context, idToken string, realm AuthRealm, meta RequestMeta) (*Principal, *SessionTokens, error) {
        if s.cfg.GoogleClientID == "" {
                return nil, nil, ErrGoogleDisabled
        }
        claims, err := s.google.verify(ctx, idToken)
        if err != nil {
                return nil, nil, fmt.Errorf("%w: %v", ErrGoogleTokenInvalid, err)
        }
        principal, _, err := s.resolveGooglePrincipal(ctx, claims)
        if err != nil {
                return nil, nil, err
        }
        if !principalRealmShapeAllowed(principal, realm) {
                return nil, nil, ErrInvalidCredentials
        }
        if !principal.IsActive {
                return nil, nil, ErrAccountInactive
        }
        if err := s.recordSuccessfulLogin(ctx, principal); err != nil {
                return nil, nil, fmt.Errorf("update login state: %w", err)
        }
        tokens, err := s.createSession(ctx, principal, realm, meta)
        if err != nil {
                return nil, nil, err
        }
        s.auditLoginSuccess(ctx, principal, realm, meta)
        return principal, tokens, nil
}
