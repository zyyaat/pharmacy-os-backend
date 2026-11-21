package auth

// اختبارات مُحقق ID token جوجل — وحدة صرفة بلا قاعدة بيانات: خادم JWKS
// محلي يخدم مفتاح RSA مولّد، وتوكنات موقّعة معه لكل الحالات الحرجة
// (التوقيع/aud/exp/iss/email_verified/kid). العقد: أي شك = رفض fail-closed.

import (
        "context"
        "crypto/rand"
        "crypto/rsa"
        "encoding/base64"
        "encoding/json"
        "math/big"
        "net/http"
        "net/http/httptest"
        "strings"
        "testing"
        "time"

        "github.com/golang-jwt/jwt/v5"
)

func base64RawURL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

type googleTestKeys struct {
        key *rsa.PrivateKey
        jwks googleJWKS
        server *httptest.Server
}

func newGoogleTestKeys(t *testing.T) *googleTestKeys {
        t.Helper()
        key, err := rsa.GenerateKey(rand.Reader, 2048)
        if err != nil {
                t.Fatalf("generate rsa key: %v", err)
        }
        // n وe بترميز base64url غير المبطن — نفس صيغة JWKS في جوجل.
        n := key.PublicKey.N.Bytes()
        e := big.NewInt(int64(key.PublicKey.E)).Bytes()
        set := googleJWKS{Keys: []googleJWK{{
                Kty: "RSA", Kid: "test-key", Alg: "RS256", Use: "sig",
                N: base64RawURL(n), E: base64RawURL(e),
        }}}
        server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                w.Header().Set("Content-Type", "application/json")
                _ = json.NewEncoder(w).Encode(set)
        }))
        t.Cleanup(server.Close)
        return &googleTestKeys{key: key, jwks: set, server: server}
}

func signGoogleToken(t *testing.T, keys *googleTestKeys, claims jwt.MapClaims) string {
        t.Helper()
        tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
        tok.Header["kid"] = "test-key"
        signed, err := tok.SignedString(keys.key)
        if err != nil {
                t.Fatalf("sign token: %v", err)
        }
        return signed
}

func baseGoogleClaims() jwt.MapClaims {
        now := time.Now()
        return jwt.MapClaims{
                "iss":            "https://accounts.google.com",
                "sub":            "google-sub-123",
                "aud":            "test-aud",
                "email":          "owner@example.com",
                "email_verified": true,
                "name":           "أحمد محمد",
                "given_name":     "أحمد",
                "family_name":    "محمد",
                "iat":            now.Unix(),
                "exp":            now.Add(time.Hour).Unix(),
        }
}

func TestGoogleVerifierAcceptsValidToken(t *testing.T) {
        keys := newGoogleTestKeys(t)
        v := newGoogleVerifier("test-aud", keys.server.URL)
        claims, err := v.verify(context.Background(), signGoogleToken(t, keys, baseGoogleClaims()))
        if err != nil {
                t.Fatalf("valid token rejected: %v", err)
        }
        if claims.Sub != "google-sub-123" || claims.Email != "owner@example.com" {
                t.Fatalf("claims mismatch: %+v", claims)
        }
        if claims.Name != "أحمد محمد" {
                t.Fatalf("name claim lost: %q", claims.Name)
        }
}

func TestGoogleVerifierRejects(t *testing.T) {
        cases := map[string]func(jwt.MapClaims) jwt.MapClaims{
                "wrong audience":        func(c jwt.MapClaims) jwt.MapClaims { c["aud"] = "other-aud"; return c },
                "email not verified":    func(c jwt.MapClaims) jwt.MapClaims { c["email_verified"] = false; return c },
                "email_verified absent": func(c jwt.MapClaims) jwt.MapClaims { delete(c, "email_verified"); return c },
                "expired":               func(c jwt.MapClaims) jwt.MapClaims { c["exp"] = time.Now().Add(-time.Hour).Unix(); return c },
                "foreign issuer":        func(c jwt.MapClaims) jwt.MapClaims { c["iss"] = "https://evil.example"; return c },
                "no sub":                func(c jwt.MapClaims) jwt.MapClaims { delete(c, "sub"); return c },
                "no email":              func(c jwt.MapClaims) jwt.MapClaims { delete(c, "email"); return c },
        }
        for name, mutate := range cases {
                t.Run(name, func(t *testing.T) {
                        keys := newGoogleTestKeys(t)
                        v := newGoogleVerifier("test-aud", keys.server.URL)
                        if _, err := v.verify(context.Background(), signGoogleToken(t, keys, mutate(baseGoogleClaims()))); err == nil {
                                t.Fatalf("%s: token accepted, want rejection", name)
                        }
                })
        }
}

func TestGoogleVerifierIssuerWithoutScheme(t *testing.T) {
        keys := newGoogleTestKeys(t)
        v := newGoogleVerifier("test-aud", keys.server.URL)
        claims := baseGoogleClaims()
        claims["iss"] = "accounts.google.com"
        if _, err := v.verify(context.Background(), signGoogleToken(t, keys, claims)); err != nil {
                t.Fatalf("issuer accounts.google.com rejected: %v", err)
        }
}

func TestGoogleVerifierUnknownKidFailsClosed(t *testing.T) {
        keys := newGoogleTestKeys(t)
        v := newGoogleVerifier("test-aud", keys.server.URL)
        // توكن بتوقيع سليم لكن kid آخر غير موجود في JWKS — رفض لا قبول.
        forged := jwt.NewWithClaims(jwt.SigningMethodRS256, baseGoogleClaims())
        forged.Header["kid"] = "some-other-kid"
        signed, err := forged.SignedString(keys.key)
        if err != nil {
                t.Fatalf("sign: %v", err)
        }
        if _, err := v.verify(context.Background(), signed); err == nil {
                t.Fatalf("unknown kid accepted, want rejection")
        }
}

func TestGoogleVerifierEmptyToken(t *testing.T) {
        keys := newGoogleTestKeys(t)
        v := newGoogleVerifier("test-aud", keys.server.URL)
        if _, err := v.verify(context.Background(), "   "); err == nil {
                t.Fatalf("empty token accepted, want rejection")
        }
}

func TestGoogleNameSplitting(t *testing.T) {
        first, last := splitGoogleName("أحمد محمد علي", "x@y.z")
        if first != "أحمد" || last != "محمد علي" {
                t.Fatalf("split mismatch: %q / %q", first, last)
        }
        first, last = splitGoogleName("", "owner@example.com")
        if first != "owner" || last != "" {
                t.Fatalf("email fallback mismatch: %q / %q", first, last)
        }
}

func TestGoogleCompanyNameIdentity(t *testing.T) {
        if got := companyNameFromIdentity("صيدلية النور", "n@x.y"); got != "صيدلية النور" {
                t.Fatalf("display name lost: %q", got)
        }
        if got := companyNameFromIdentity("", "owner@example.com"); got != "owner" {
                t.Fatalf("email fallback lost: %q", got)
        }
        long := strings.Repeat("ا", 300)
        if got := companyNameFromIdentity(long, "n@x.y"); len([]rune(got)) != 255 {
                t.Fatalf("truncation broken: %d", len([]rune(got)))
        }
}
