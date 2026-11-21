package auth

// Task 161 — the recovery-finish binding regression gate.
//
// The field bug of 2026-11-09 (orarsm@…): fingerprint succeeded, then the
// finish always failed. Root cause: BeginPasskeyRecovery issues a BOUND
// ceremony (relay.BeginLogin → session.UserID + AllowedCredentialIDs set)
// while FinishPasskeyRecovery called relay.FinishDiscoverableLogin, whose
// vendor guard (login.go ValidatePasskeyLogin) rejects ANY session with a
// non-empty UserID — a begin/finish mode mismatch that made every real
// recovery impossible while the decoy path (unbound begin) was never meant
// to reach the relay at all. The fix verifies against the account the
// challenge was issued FOR (bound finish).
//
// These gates run the REAL ceremonies end-to-end against the real schema
// (TEST_DATABASE_URL, full migration chain via cmd/migrate — same harness
// as the ledger gates) with a virtual authenticator: a genuine ECDSA P-256
// keypair whose public half is persisted exactly like registration does.
//
// They pin four truths at once:
//  1. a legitimate bound round trip SUCCEEDS (begin-bound → finish-bound)
//     and yields a working single-use recovery grant + session wipe,
//  2. a cross-account assertion (attacker's key, victim's challenge) is
//     REJECTED — the binding is proven, not assumed,
//  3. the decoy ceremony (unknown email) still can never finish,
//  4. under the SERVER's exec mode (QueryExecModeExec — Task 158 lesson)
//     the same round trip holds byte-for-byte.
//
//      TEST_DATABASE_URL=postgres://…/pharmacy_scratch go test ./internal/auth/ -run TestRecovery -v

import (
	"bytes"
	"context"
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
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/jackc/pgx/v5/pgxpool"
)

// virtualPasskey is a test-only authenticator: the private key never leaves
// the test, and the public half travels into passkey_credentials in exactly
// the format FinishPasskeyRegistration persists (raw COSE EC2 CBOR).
type virtualPasskey struct {
	key    *ecdsa.PrivateKey
	credID []byte
}

func newVirtualPasskey(t *testing.T) *virtualPasskey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa key: %v", err)
	}
	credID := make([]byte, 32)
	if _, err := rand.Read(credID); err != nil {
		t.Fatalf("credential id: %v", err)
	}
	return &virtualPasskey{key: key, credID: credID}
}

// cosePublicKey marshals the EC2/P-256 COSE key (kty=2, alg=ES256, crv=1).
func (v *virtualPasskey) cosePublicKey(t *testing.T) []byte {
	t.Helper()
	x := v.key.PublicKey.X.FillBytes(make([]byte, 32))
	y := v.key.PublicKey.Y.FillBytes(make([]byte, 32))
	cose, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: x, -3: y})
	if err != nil {
		t.Fatalf("cose key: %v", err)
	}
	return cose
}

// assertionRequest builds the webauthn.get ceremony body: clientDataJSON bound
// to the issued challenge, authenticatorData (rpIdHash + UP|UV + counter),
// an ECDSA-SHA256 (ASN.1) signature over authData||clientDataHash, and the
// account's user handle — serialized as requestCredentialJSON like every
// browser sends it.
func (v *virtualPasskey) assertionRequest(t *testing.T, rpID, challenge, origin string, userHandle []byte) *http.Request {
	t.Helper()

	clientData, err := json.Marshal(map[string]any{
		"type":        "webauthn.get",
		"challenge":   challenge,
		"origin":      origin,
		"crossOrigin": false,
	})
	if err != nil {
		t.Fatalf("client data: %v", err)
	}

	rpHash := sha256.Sum256([]byte(rpID))
	authData := bytes.NewBuffer(nil)
	authData.Write(rpHash[:])          // rpIdHash
	authData.Write([]byte{0x05})       // flags: UP | UV (BE=0 matches the stored row)
	authData.Write([]byte{0, 0, 0, 1}) // signCount = 1 (stored 0 — counter advances)

	clientHash := sha256.Sum256(clientData)
	// ES256 authenticators hash the message themselves: the server side
	// (webauthncose EC2PublicKeyData.Verify) computes sha256(authData ||
	// clientDataHash) and verifies the DER signature against that digest —
	// so SignASN1 must receive the digest, not the raw concatenation.
	message := append(authData.Bytes(), clientHash[:]...)
	digest := sha256.Sum256(message)
	signature, err := ecdsa.SignASN1(rand.Reader, v.key, digest[:])
	if err != nil {
		t.Fatalf("assertion signature: %v", err)
	}

	b64 := base64.RawURLEncoding.EncodeToString
	body, err := json.Marshal(map[string]any{
		"id":    b64(v.credID),
		"rawId": b64(v.credID),
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64(clientData),
			"authenticatorData": b64(authData.Bytes()),
			"signature":         b64(signature),
			"userHandle":        b64(userHandle),
		},
		"clientExtensionResults": map[string]any{},
	})
	if err != nil {
		t.Fatalf("ceremony body: %v", err)
	}
	return httptest.NewRequest("POST", "/finish", bytes.NewReader(body))
}

// persistVirtualPasskey writes the credential row exactly like
// persistPasskeyCredential — same columns, same defaults a "none"-attestation
// registration produces.
func persistVirtualPasskey(ctx context.Context, t *testing.T, pool *pgxpool.Pool, principal *Principal, v *virtualPasskey) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
                INSERT INTO passkey_credentials (
                        principal_type, principal_id, credential_id, public_key,
                        attestation_type, transports, aaguid, sign_count,
                        backup_eligible, backup_state, uv_initialized, last_used_at
                ) VALUES ($1, $2::uuid, $3, $4, 'none', '{}', '', 0, false, false, true, NOW())
        `, principal.Type, principal.ID, v.credID, v.cosePublicKey(t)); err != nil {
		t.Fatalf("persist credential: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM passkey_credentials WHERE credential_id = $1`, v.credID)
	})
}

// runRecoveryRoundTrip is the shared ceremony: seed account + virtual
// authenticator, bound begin, signed assertion, bound finish. Returns the
// finish results for per-mode assertions.
func runRecoveryRoundTrip(t *testing.T, service *Service, pool *pgxpool.Pool, origin string) (token, email string, owner *Principal) {
	t.Helper()
	ctx := context.Background()

	owner = seedCompanyPrincipal(t, pool)
	pk := newVirtualPasskey(t)
	persistVirtualPasskey(ctx, t, pool, owner, pk)

	// A live session to prove the announced policy (all sessions die).
	seedAuthSession(t, pool, owner)

	options, err := service.BeginPasskeyRecovery(ctx, owner.Email)
	if err != nil {
		t.Fatalf("bound begin: %v", err)
	}
	challenge := options.Response.Challenge.String()
	if challenge == "" {
		t.Fatalf("assertion options carry no challenge")
	}
	if len(options.Response.AllowedCredentials) == 0 {
		t.Fatalf("bound begin must ship a non-empty allowCredentials list")
	}
	if options.Response.UserVerification != "required" {
		t.Fatalf("user verification must be required, got %q", options.Response.UserVerification)
	}

	request := pk.assertionRequest(t, service.cfg.WebAuthnRPID, challenge, origin, webAuthnIDForPrincipal(owner.ID))
	meta := RequestMeta{IPAddress: "203.0.113.9", UserAgent: "t161-gate"}
	token, email, err = service.FinishPasskeyRecovery(ctx, challenge, request, meta)
	if err != nil {
		t.Fatalf("bound recovery round trip must succeed (Task 161 bug): %v", err)
	}
	if email != owner.Email {
		t.Fatalf("recovery email mismatch: got %q want %q", email, owner.Email)
	}
	if token == "" {
		t.Fatalf("recovery must mint a reset grant")
	}

	// The grant must be a working single-use recovery grant bound to owner.
	consumed, err := service.consumeGrantRow(ctx, token, RecoveryGrantPurpose)
	if err != nil {
		t.Fatalf("recovery grant must spend: %v", err)
	}
	if consumed.ID != owner.ID || consumed.Type != owner.Type {
		t.Fatalf("grant bound to the wrong principal: %+v", consumed)
	}
	if _, err := service.consumeGrantRow(ctx, token, RecoveryGrantPurpose); err != ErrChallengeInvalid {
		t.Fatalf("recovery grant replay must fail, got %v", err)
	}

	// Announced policy: zero live sessions survive a recovery verification.
	var live int
	if err := pool.QueryRow(ctx, `
                SELECT COUNT(*) FROM auth_sessions
                WHERE principal_type = $1 AND principal_id = $2::uuid AND revoked_at IS NULL
        `, owner.Type, owner.ID).Scan(&live); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if live != 0 {
		t.Fatalf("all sessions must be revoked after recovery, %d live", live)
	}
	return token, email, owner
}

// TestRecoveryBoundRoundTripDB — the exact production failure of 2026-11-09,
// proven dead: begin-bound → finish-bound with a real signature succeeds.
func TestRecoveryBoundRoundTripDB(t *testing.T) {
	pool := passkeyTestPool(t)
	service := &Service{db: pool, cfg: Config{
		WebAuthnRPID:      "pharmacy-ci.example.com",
		WebAuthnRPName:    "Pharmacy OS CI",
		WebAuthnRPOrigins: []string{"https://pharmacy-ci.example.com"},
	}}
	runRecoveryRoundTrip(t, service, pool, "https://pharmacy-ci.example.com")
}

// TestRecoveryBoundRoundTripExecModeDB — Task 158 lesson: production answers
// through QueryExecModeExec, so the fixed finish must hold there too.
func TestRecoveryBoundRoundTripExecModeDB(t *testing.T) {
	pool := serverModePool(t)
	service := execModeService(pool)
	runRecoveryRoundTrip(t, service, pool, "https://pharmacy-ci.example.com")
}

// TestRecoveryCrossAccountRejectionDB — the binding bite: an attacker's
// credential (registered on THEIR account) can never spend the victim's
// challenge, because the session's allow-list and the signature check both
// bind to the account the challenge was issued FOR.
func TestRecoveryCrossAccountRejectionDB(t *testing.T) {
	pool := passkeyTestPool(t)
	service := &Service{db: pool, cfg: Config{
		WebAuthnRPID:      "pharmacy-ci.example.com",
		WebAuthnRPName:    "Pharmacy OS CI",
		WebAuthnRPOrigins: []string{"https://pharmacy-ci.example.com"},
	}}
	ctx := context.Background()

	victim := seedCompanyPrincipal(t, pool)
	attacker := seedCompanyPrincipal(t, pool)
	attackerKey := newVirtualPasskey(t)
	persistVirtualPasskey(ctx, t, pool, attacker, attackerKey)

	options, err := service.BeginPasskeyRecovery(ctx, victim.Email)
	if err != nil {
		t.Fatalf("victim begin: %v", err)
	}
	challenge := options.Response.Challenge.String()

	// Attacker signs with their OWN credential and their OWN user handle —
	// the most honest attempt an account thief can produce.
	request := attackerKey.assertionRequest(t, service.cfg.WebAuthnRPID, challenge,
		"https://pharmacy-ci.example.com", webAuthnIDForPrincipal(attacker.ID))
	meta := RequestMeta{IPAddress: "198.51.100.7", UserAgent: "t161-attacker"}
	token, email, err := service.FinishPasskeyRecovery(ctx, challenge, request, meta)
	if err == nil {
		t.Fatalf("cross-account assertion must be REJECTED")
	}
	if err != ErrPasskeyRejected {
		t.Fatalf("cross-account rejection must be ErrPasskeyRejected, got %v", err)
	}
	if token != "" || email != "" {
		t.Fatalf("no grant, no email may leak on rejection")
	}

	// The victim's challenge was CONSUMED by the failed attempt (single-use
	// ledger) — a replay of any kind is dead.
	if _, err := service.consumeChallenge(ctx, challenge, ChallengePurposeRecovery, "", "", ""); err == nil {
		t.Fatalf("failed finish must have consumed the challenge (single-use)")
	}
}

// TestRecoveryDecoyShapeParityDB — Task 162: the decoy (unknown email) is
// wire-indistinguishable from a real single-passkey account at BEGIN time.
// Before this gate the decoy shipped an EMPTY allowCredentials list while a
// bound begin carried the account's credential ids — the response shape
// alone answered "does this email own a passkey", no ceremony required.
// The gate pins three truths:
//  1. the decoy carries EXACTLY ONE descriptor (parity with the dominant
//     real shape: one registered passkey),
//  2. the fake id is a fresh 32-byte draw that never equals a real
//     credential id and never repeats across requests (no stable fingerprint),
//  3. the decoy still can never finish (unbound rows reject before any
//     relay verification) — enumeration stays useless even if someone
//     signs for the fake id.
func TestRecoveryDecoyShapeParityDB(t *testing.T) {
	pool := passkeyTestPool(t)
	service := &Service{db: pool, cfg: Config{
		WebAuthnRPID:      "pharmacy-ci.example.com",
		WebAuthnRPName:    "Pharmacy OS CI",
		WebAuthnRPOrigins: []string{"https://pharmacy-ci.example.com"},
	}}
	ctx := context.Background()

	decoy, err := service.BeginPasskeyRecovery(ctx, "nobody-t161@example.com")
	if err != nil {
		t.Fatalf("decoy begin: %v", err)
	}
	if len(decoy.Response.AllowedCredentials) != 1 {
		t.Fatalf("decoy must ship exactly one allowCredentials descriptor (shape parity), got %d",
			len(decoy.Response.AllowedCredentials))
	}
	fake := decoy.Response.AllowedCredentials[0]
	if fake.Type != protocol.PublicKeyCredentialType {
		t.Fatalf("decoy descriptor type = %q, want public-key", fake.Type)
	}
	if len(fake.CredentialID) != 32 {
		t.Fatalf("decoy credential id must be a 32-byte draw, got %d bytes", len(fake.CredentialID))
	}

	// Shape parity against the real thing: a one-passkey account yields
	// exactly the same list length, and its id is never the decoy's.
	owner := seedCompanyPrincipal(t, pool)
	pk := newVirtualPasskey(t)
	persistVirtualPasskey(ctx, t, pool, owner, pk)
	bound, err := service.BeginPasskeyRecovery(ctx, owner.Email)
	if err != nil {
		t.Fatalf("bound begin: %v", err)
	}
	if len(bound.Response.AllowedCredentials) != 1 {
		t.Fatalf("single-passkey account must ship exactly one descriptor, got %d",
			len(bound.Response.AllowedCredentials))
	}
	if string(bound.Response.AllowedCredentials[0].CredentialID) == string(fake.CredentialID) {
		t.Fatalf("decoy id must never collide with a real credential id")
	}

	// A second decoy draw must differ — no stable per-attacker fingerprint.
	again, err := service.BeginPasskeyRecovery(ctx, "nobody-t161@example.com")
	if err != nil {
		t.Fatalf("second decoy begin: %v", err)
	}
	if string(again.Response.AllowedCredentials[0].CredentialID) == string(fake.CredentialID) {
		t.Fatalf("decoy id must be a fresh draw per request")
	}

	// And the decoy still can never finish.
	challenge := decoy.Response.Challenge.String()
	stranger := newVirtualPasskey(t)
	request := stranger.assertionRequest(t, service.cfg.WebAuthnRPID, challenge,
		"https://pharmacy-ci.example.com", bytes.Repeat([]byte{9}, 16))
	meta := RequestMeta{IPAddress: "192.0.2.15", UserAgent: "t162-decoy"}
	if _, _, err := service.FinishPasskeyRecovery(ctx, challenge, request, meta); err != ErrPasskeyRejected {
		t.Fatalf("decoy finish must fail with ErrPasskeyRejected, got %v", err)
	}
}
