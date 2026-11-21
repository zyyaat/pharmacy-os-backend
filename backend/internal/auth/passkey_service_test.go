package auth

// Task 151 — security contracts of the passkey family that need no database:
// handle stability, purpose allowlist, grant-consumption input hygiene, and
// the fail-closed behavior when no Relying Party is configured. The DB-backed
// challenge/grant lifecycle lives in passkey_ledger_db_test.go.

import (
        "bytes"
        "encoding/hex"
        "testing"

        "github.com/go-webauthn/webauthn/protocol"
        "github.com/go-webauthn/webauthn/webauthn"
)

func TestWebAuthnIDForPrincipalIsStableAndSixteenBytes(t *testing.T) {
        id := "7e6a3f2c-9b41-4c8e-a1d2-3f4e5b6a7c8d"
        first := webAuthnIDForPrincipal(id)
        second := webAuthnIDForPrincipal(id)
        if len(first) != 16 {
                t.Fatalf("handle length = %d, want 16", len(first))
        }
        if !bytes.Equal(first, second) {
                t.Fatalf("handle is not deterministic for the same principal")
        }
        // The handle must be the raw UUID bytes — an account keeps the same
        // handle for its whole life, discoverable login depends on it.
        raw, err := hex.DecodeString("7e6a3f2c9b414c8ea1d23f4e5b6a7c8d")
        if err != nil {
                t.Fatalf("decode fixture: %v", err)
        }
        if !bytes.Equal(first, raw) {
                t.Fatalf("handle = %x, want the raw UUID bytes %x", first, raw)
        }
        // Uppercase/dashed variants must normalize to the same handle.
        if !bytes.Equal(webAuthnIDForPrincipal("7E6A3F2C-9B41-4C8E-A1D2-3F4E5B6A7C8D"), first) {
                t.Fatalf("uuid casing must not change the handle")
        }
        // A garbage id still yields 16 deterministic bytes (never a panic).
        junk := webAuthnIDForPrincipal("not-a-uuid")
        if len(junk) != 16 {
                t.Fatalf("junk handle length = %d, want 16", len(junk))
        }
        if !bytes.Equal(junk, webAuthnIDForPrincipal("not-a-uuid")) {
                t.Fatalf("junk handle not deterministic")
        }
        if bytes.Equal(junk, webAuthnIDForPrincipal("not-a-uuid-2")) {
                t.Fatalf("distinct ids must not collide on the fallback path")
        }
}

func TestCredentialsToDescriptors(t *testing.T) {
        if got := credentialsToDescriptors(nil); len(got) != 0 {
                t.Fatalf("nil credentials must yield no descriptors, got %d", len(got))
        }
        credentials := []webauthn.Credential{
                {ID: []byte{1, 2, 3}},
                {ID: []byte{4, 5}},
        }
        descriptors := credentialsToDescriptors(credentials)
        if len(descriptors) != 2 {
                t.Fatalf("descriptor count = %d, want 2", len(descriptors))
        }
        if descriptors[0].Type != protocol.PublicKeyCredentialType {
                t.Fatalf("descriptor type = %q, want %q", descriptors[0].Type, protocol.PublicKeyCredentialType)
        }
        if !bytes.Equal(descriptors[1].CredentialID, []byte{4, 5}) {
                t.Fatalf("descriptor id mismatch")
        }
}

func TestValidateStepUpPurposeAllowlist(t *testing.T) {
        // Only the allowlisted purposes exist — a grant for one purpose can
        // never be minted for another class of operation (rule #3).
        for _, purpose := range []string{StepUpPurposeChangePassword, StepUpPurposeManagePasskeys} {
                if err := validateStepUpPurpose(purpose); err != nil {
                        t.Fatalf("purpose %q must be allowed, got %v", purpose, err)
                }
        }
        for _, purpose := range []string{"", "anything", "login", "delete_company", "change_password ", "CHANGE_PASSWORD"} {
                if err := validateStepUpPurpose(purpose); err == nil {
                        t.Fatalf("purpose %q must be refused", purpose)
                }
        }
}

func TestConsumeStepUpGrantRejectsEmptyInputWithoutDB(t *testing.T) {
        // The nil-pool service proves the guards fire BEFORE any database
        // touch — a missing DB must not turn into a silent grant.
        service := &Service{}
        granted, err := service.ConsumeStepUpGrant(nil, &Principal{}, StepUpPurposeChangePassword, "")
        if granted || err != nil {
                t.Fatalf("empty token must be (false, nil), got (%v, %v)", granted, err)
        }
        granted, err = service.ConsumeStepUpGrant(nil, &Principal{}, "", "some-token")
        if granted || err != nil {
                t.Fatalf("empty purpose must be (false, nil), got (%v, %v)", granted, err)
        }
}

func TestRelayFailsClosedWithoutRelyingParty(t *testing.T) {
        // No RPID configured → ErrPasskeysDisabled, never a half-working
        // ceremony (rule #1: the server owns verification or offers none).
        service := &Service{}
        if _, err := service.relay(); err != ErrPasskeysDisabled {
                t.Fatalf("relay error = %v, want ErrPasskeysDisabled", err)
        }
        if service.PasskeysEnabled() {
                t.Fatalf("PasskeysEnabled must be false without config")
        }
}

func TestSanitizeDeviceName(t *testing.T) {
        if got := sanitizeDeviceName("  iPhone 15 \t"); got != "iPhone 15" {
                t.Fatalf("sanitize = %q", got)
        }
        if got := sanitizeDeviceName("bad\x00name\x1b"); got != "badname" {
                t.Fatalf("control chars must be stripped, got %q", got)
        }
        long := make([]byte, 300)
        for index := range long {
                long[index] = 'a'
        }
        if got := len(sanitizeDeviceName(string(long))); got != 120 {
                t.Fatalf("long name length = %d, want 120", got)
        }
}

func TestChallengeDigestIsDeterministicAndDistinct(t *testing.T) {
        a := challengeDigest("challenge-one")
        b := challengeDigest("challenge-one")
        c := challengeDigest("challenge-two")
        if !bytes.Equal(a, b) || bytes.Equal(a, c) {
                t.Fatalf("digest must be deterministic per key and distinct across keys")
        }
        if len(a) != 32 {
                t.Fatalf("digest length = %d, want 32 (SHA-256)", len(a))
        }
}

func TestTransportsRoundTrip(t *testing.T) {
        if got := transportsToStrings(nil); len(got) != 0 {
                t.Fatalf("nil transports must map to empty slice, got %v", got)
        }
        values := []string{"internal", "hybrid"}
        protocols := transportsToProtocol(values)
        if len(protocols) != 2 || protocols[0] != "internal" || protocols[1] != "hybrid" {
                t.Fatalf("transportsToProtocol mismatch: %v", protocols)
        }
        back := transportsToStrings(protocols)
        if len(back) != 2 || back[0] != "internal" || back[1] != "hybrid" {
                t.Fatalf("transportsToStrings mismatch: %v", back)
        }
}

func TestPasskeyUserAdapter(t *testing.T) {
        principal := &Principal{
                ID:          "7e6a3f2c-9b41-4c8e-a1d2-3f4e5b6a7c8d",
                Email:       "owner@example.com",
                DisplayName: "صاحب الصيدلية",
        }
        user := &passkeyUser{principal: principal}
        if user.WebAuthnName() != "owner@example.com" {
                t.Fatalf("WebAuthnName = %q", user.WebAuthnName())
        }
        if user.WebAuthnDisplayName() != "صاحب الصيدلية" {
                t.Fatalf("WebAuthnDisplayName = %q", user.WebAuthnDisplayName())
        }
        bare := &passkeyUser{principal: &Principal{ID: principal.ID, Email: "owner@example.com"}}
        if bare.WebAuthnDisplayName() != "owner@example.com" {
                t.Fatalf("empty display name must fall back to email, got %q", bare.WebAuthnDisplayName())
        }
}
