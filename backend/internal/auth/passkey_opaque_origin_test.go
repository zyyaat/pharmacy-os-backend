package auth

import (
        "bytes"
        "crypto/ecdsa"
        "crypto/elliptic"
        "crypto/rand"
        "crypto/sha256"
        "encoding/base64"
        "encoding/json"
        "net/http"
        "net/http/httptest"
        "testing"

        "github.com/fxamacker/cbor/v2"
        "github.com/go-webauthn/webauthn/webauthn"
)

// Task 160 (M3, passkeys on mobile) — the native-client origin gate, proven
// with a REAL registration ceremony instead of a config-table echo.
//
// Android Credential Manager (the native passkey API the Flutter app uses)
// conveys "android:apk-key-hash:<base64url sha256 of the APK signing cert>"
// as clientDataJSON.origin — NOT the https origin a browser conveys. Since
// Task 151 every ceremony was bound to the single web origin, so a native
// assertion would be rejected with "Error validating origin" no matter how
// correct its signature is. The fix: PASSKEY_RP_OPAQUE_ORIGINS →
// Config.RPOpaqueOrigins, which go-webauthn matches by exact string
// comparison (opaque origins have no host to normalize).
//
// This gate builds genuine attestation objects (ECDSA P-256 COSE key, "none"
// attestation — the format PreferNoAttestation deployments receive) and pins
// three truths at once:
//  1. a configured opaque origin REGISTERS successfully (the M3 unlock),
//  2. the identical ceremony WITHOUT the config is REJECTED (the gate is
//     the configuration, not luck — fail-closed default preserved),
//  3. the web origin keeps registering unchanged (no regression for M2).

// testOpaqueOrigin — base64url("test-cert-hash") + 2 chars of tail, shaped
// exactly like Credential Manager conveys it (no padding, url-safe alphabet).
const testOpaqueOrigin = "android:apk-key-hash:dGVzdC1jZXJ0LWhhc2gtZmluZ2VycHJpbnQ"

// relayTestUser satisfies webauthn.User with the 32-byte handle the spec
// demands (validateUserHandle rejects anything under 16 bytes).
type relayTestUser struct{ id []byte }

func (u relayTestUser) WebAuthnID() []byte                 { return u.id }
func (u relayTestUser) WebAuthnName() string               { return "ci@example.com" }
func (u relayTestUser) WebAuthnDisplayName() string        { return "CI User" }
func (u relayTestUser) WebAuthnCredentials() []webauthn.Credential { return nil }

// buildRegisterRequest fabricates a complete authenticator response:
// COSE EC2/P-256 public key embedded in authData (flags UP|UV|AT), "none"
// attestation statement, clientDataJSON bound to the issued challenge and the
// given origin — then serializes it as the requestCredentialJSON body the
// finish endpoint parses (transports INSIDE response, like every browser).
func buildRegisterRequest(t *testing.T, rpID, challenge, origin string) *http.Request {
        t.Helper()

        key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
        if err != nil {
                t.Fatalf("ecdsa key: %v", err)
        }
        x := key.PublicKey.X.FillBytes(make([]byte, 32))
        y := key.PublicKey.Y.FillBytes(make([]byte, 32))
        cose, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: x, -3: y})
        if err != nil {
                t.Fatalf("cose key: %v", err)
        }

        credID := make([]byte, 32)
        if _, err := rand.Read(credID); err != nil {
                t.Fatalf("credential id: %v", err)
        }

        rpHash := sha256.Sum256([]byte(rpID))
        authData := bytes.NewBuffer(nil)
        authData.Write(rpHash[:])                      // rpIdHash
        authData.Write([]byte{0x45})                   // flags: UP | UV | AT
        authData.Write([]byte{0, 0, 0, 1})             // signCount
        authData.Write(make([]byte, 16))               // AAGUID (zero)
        authData.Write([]byte{0x00, 0x20})             // credentialIdLength = 32
        authData.Write(credID)                         // credentialId
        authData.Write(cose)                           // credentialPublicKey

        attObj, err := cbor.Marshal(map[string]any{
                "fmt":      "none",
                "attStmt":  map[string]any{},
                "authData": authData.Bytes(),
        })
        if err != nil {
                t.Fatalf("attestation object: %v", err)
        }

        clientData, err := json.Marshal(map[string]any{
                "type":        "webauthn.create",
                "challenge":   challenge,
                "origin":      origin,
                "crossOrigin": false,
        })
        if err != nil {
                t.Fatalf("client data: %v", err)
        }

        b64 := base64.RawURLEncoding.EncodeToString
        body, err := json.Marshal(map[string]any{
                "id":    b64(credID),
                "rawId": b64(credID),
                "type":  "public-key",
                "response": map[string]any{
                        "clientDataJSON":    b64(clientData),
                        "attestationObject": b64(attObj),
                        "transports":        []string{"internal"},
                },
                "clientExtensionResults": map[string]any{},
        })
        if err != nil {
                t.Fatalf("ceremony body: %v", err)
        }
        return httptest.NewRequest("POST", "/finish", bytes.NewReader(body))
}

func TestOpaqueOriginRegistrationGate(t *testing.T) {
        webOrigin := "https://pharmacy-ci.example.com"
        base := Config{
                WebAuthnRPID:      "pharmacy-ci.example.com",
                WebAuthnRPName:    "Pharmacy OS CI",
                WebAuthnRPOrigins: []string{webOrigin},
        }
        user := relayTestUser{id: bytes.Repeat([]byte{7}, 32)}

        // 1) Configured opaque origin — the M3 unlock: registration SUCCEEDS.
        withNative := base
        withNative.WebAuthnRPOpaqueOrigins = []string{testOpaqueOrigin}
        nativeService := &Service{cfg: withNative}
        nativeRelay, err := nativeService.relay()
        if err != nil {
                t.Fatalf("relay init with opaque origins: %v", err)
        }
        creation, session, err := nativeRelay.BeginRegistration(user)
        if err != nil {
                t.Fatalf("begin registration: %v", err)
        }
        _ = creation
        if _, err := nativeRelay.FinishRegistration(user, *session,
                buildRegisterRequest(t, base.WebAuthnRPID, session.Challenge, testOpaqueOrigin)); err != nil {
                t.Fatalf("native-origin registration must succeed when configured: %v", err)
        }

        // 2) The IDENTICAL ceremony without the config — REJECTED (fail-closed
        // default preserved; the gate is the configuration, not luck).
        webOnlyService := &Service{cfg: base}
        webOnlyRelay, err := webOnlyService.relay()
        if err != nil {
                t.Fatalf("relay init web-only: %v", err)
        }
        creation2, session2, err := webOnlyRelay.BeginRegistration(user)
        if err != nil {
                t.Fatalf("begin registration (web-only service): %v", err)
        }
        _ = creation2
        if _, err := webOnlyRelay.FinishRegistration(user, *session2,
                buildRegisterRequest(t, base.WebAuthnRPID, session2.Challenge, testOpaqueOrigin)); err == nil {
                t.Fatalf("native-origin registration must be REJECTED when not configured")
        }

        // 3) Control — the web origin keeps registering unchanged (no M2 regression).
        creation3, session3, err := webOnlyRelay.BeginRegistration(user)
        if err != nil {
                t.Fatalf("begin registration (control): %v", err)
        }
        _ = creation3
        if _, err := webOnlyRelay.FinishRegistration(user, *session3,
                buildRegisterRequest(t, base.WebAuthnRPID, session3.Challenge, webOrigin)); err != nil {
                t.Fatalf("web-origin registration must keep working: %v", err)
        }
}
