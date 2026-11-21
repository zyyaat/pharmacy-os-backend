package auth

// Task 151 — WebAuthn passkeys. Hardware-backed sign-in that REPLACES the
// password (owner-approved architecture): the private key never leaves the
// device; the Go API owns every verification decision (rule #1 — a local
// "success" is never proof). This file holds the ceremony core: the
// single-use challenge ledger, the credential store, registration and
// discoverable login. Step-up grants and account recovery live in
// passkey_stepup.go; the HTTP surface in passkey_handler.go.

import (
        "context"
        "crypto/rand"
        "crypto/sha256"
        "encoding/hex"
        "encoding/json"
        "errors"
        "fmt"
        "log"
        "net/http"
        "strings"
        "time"

        "github.com/go-webauthn/webauthn/protocol"
        "github.com/go-webauthn/webauthn/webauthn"
        "github.com/jackc/pgx/v5"

        "github.com/pharmacy-os/backend/internal/auditlog"
)

const (
        // Challenge purposes — the ledger binds every challenge to exactly
        // one ceremony class; a response can never be replayed against a
        // different purpose (owner rule #2).
        ChallengePurposeRegister = "passkey_register"
        ChallengePurposeLogin    = "passkey_login"
        ChallengePurposeRecovery = "passkey_recovery"
        ChallengePurposeStepUp   = "stepup_verify"

        // Every challenge dies in 3 minutes regardless of the client
        // (owner rule #2). SessionData.Expires is overwritten to match so
        // the library verifier and the ledger agree.
        challengeTTL = 3 * time.Minute
)

var (
        ErrPasskeysDisabled  = errors.New("passkey authentication is not configured")
        ErrNoActivePasskey   = errors.New("no active passkey registered for this account")
        ErrChallengeInvalid  = errors.New("challenge invalid, expired, or already consumed")
        ErrPasskeyRejected   = errors.New("passkey verification failed")
        ErrPasskeyExists     = errors.New("this authenticator is already registered")
        ErrLastCredential    = errors.New("cannot remove the only sign-in method of an account without a password")
)

// passkeyUser adapts a Principal + its stored credentials to the
// webauthn.User interface. WebAuthnID is the raw 16 bytes of the principal
// UUID — stable for the account's lifetime and unique across both principal
// tables (the UUID namespace is shared).
type passkeyUser struct {
        principal   *Principal
        credentials []webauthn.Credential
}

func (u *passkeyUser) WebAuthnID() []byte           { return webAuthnIDForPrincipal(u.principal.ID) }
func (u *passkeyUser) WebAuthnName() string         { return u.principal.Email }
func (u *passkeyUser) WebAuthnDisplayName() string {
        if u.principal.DisplayName != "" {
                return u.principal.DisplayName
        }
        return u.principal.Email
}
func (u *passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

// webAuthnIDForPrincipal converts a UUID string to the 16 raw bytes used as
// the WebAuthn user handle. It is deterministic: the same account always
// presents the same handle, which the library requires when matching a
// discoverable assertion to an account.
func webAuthnIDForPrincipal(id string) []byte {
        clean := strings.ReplaceAll(id, "-", "")
        if len(clean) == 32 {
                if raw, err := hex.DecodeString(clean); err == nil {
                        return raw
                }
        }
        // Non-UUID principal ids would break handle stability; hash so the
        // handle stays 16 bytes and deterministic rather than panicking.
        digest := sha256.Sum256([]byte(id))
        return digest[:16]
}

func credentialsToDescriptors(credentials []webauthn.Credential) []protocol.CredentialDescriptor {
        descriptors := make([]protocol.CredentialDescriptor, 0, len(credentials))
        for _, credential := range credentials {
                descriptors = append(descriptors, protocol.CredentialDescriptor{
                        Type:            protocol.PublicKeyCredentialType,
                        CredentialID:    credential.ID,
                })
        }
        return descriptors
}

// relay lazily builds the webauthn.WebAuthn instance from the configured
// Relying Party identity. An empty RPID disables the whole passkey family
// (fail-closed — a misconfigured deployment answers 503, never "verify
// anyway").
func (s *Service) relay() (*webauthn.WebAuthn, error) {
        s.passkeyMu.Lock()
        defer s.passkeyMu.Unlock()
        if s.passkeyRelay != nil {
                return s.passkeyRelay, nil
        }
        if strings.TrimSpace(s.cfg.WebAuthnRPID) == "" || len(s.cfg.WebAuthnRPOrigins) == 0 {
                return nil, ErrPasskeysDisabled
        }
        instance, err := webauthn.New(&webauthn.Config{
                RPID:                   s.cfg.WebAuthnRPID,
                RPDisplayName:          s.cfg.WebAuthnRPName,
                RPOrigins:              s.cfg.WebAuthnRPOrigins,
                // Task 160 (M3) — native clients (Android Credential
                // Manager) convey an opaque origin instead of the https one;
                // matched by exact string comparison. Empty slice keeps the
                // pre-M3 web-only behavior.
                RPOpaqueOrigins:        s.cfg.WebAuthnRPOpaqueOrigins,
                AttestationPreference:  protocol.PreferNoAttestation,
        })
        if err != nil {
                return nil, fmt.Errorf("init webauthn: %w", err)
        }
        s.passkeyRelay = instance
        return s.passkeyRelay, nil
}

// PasskeysEnabled reports whether the deployment configured a Relying Party
// identity. The handler surface answers 503 passkeys_disabled otherwise.
func (s *Service) PasskeysEnabled() bool {
        relay, err := s.relay()
        return err == nil && relay != nil
}

// ---------------------------------------------------------------------------
// Challenge ledger (owner rule #2)
// ---------------------------------------------------------------------------

// challengeRecord is what consumption hands back: the binding proven at
// issue time plus the serialized library session needed to verify the
// finish call.
type challengeRecord struct {
        Purpose       string
        PrincipalType string
        PrincipalID   string
        SessionID     string
        ExpiresAt     time.Time
        Session       webauthn.SessionData
}

// nullableParam converts an optional identifier to an explicit nil so pgx
// sends a TYPED NULL instead of an empty string: with NULLIF($n,'')::uuid the
// server may infer the parameter AS uuid (Supabase) or AS text (other
// builds) — the empty string then explodes as 22P02 on the uuid path.
// Deterministic across every PostgreSQL build (Task 151 prod fix).
func nullableParam(value string) any {
        if value == "" {
                return nil
        }
        return value
}

// storeChallenge persists a challenge under the SHA-256 of its key. The key
// is the base64url challenge string the client saw — hashing at rest means a
// database leak cannot mint valid ceremonies.
//
// session_data travels as a STRING (never []byte): production runs pgx in
// QueryExecModeExec (Vercel/Supabase pooling), where no Describe() happens
// and every value is encoded in the TEXT format chosen from the Go type
// alone. A []byte is sent as bytea-hex ("\x7b…") and the server's
// text→jsonb cast explodes with 22P02 — the exact failure the decoy-recovery
// probe hit on 2026-11-08 (Task 158). Every other jsonb site in the codebase
// already passes string(blob); this was the one outlier.
func (s *Service) storeChallenge(ctx context.Context, challengeKey, purpose, principalType, principalID, sessionID string, session *webauthn.SessionData, ttl time.Duration) error {
        var sessionData []byte
        if session != nil {
                session.Expires = time.Now().UTC().Add(ttl)
                if encoded, err := json.Marshal(session); err == nil {
                        sessionData = encoded
                } else {
                        return fmt.Errorf("marshal session data: %w", err)
                }
        }
        // string(nil) is "" — an empty string into jsonb is the same 22P02 under
        // the exec mode, so nil-ness must survive the conversion (NULL stays NULL).
        var sessionParam any
        if len(sessionData) > 0 {
                sessionParam = string(sessionData)
        }
        _, err := s.db.Exec(ctx, `
                INSERT INTO auth_challenges (
                        challenge_hash, purpose, principal_type, principal_id,
                        session_id, session_data, expires_at
                ) VALUES ($1, $2, $3, $4::uuid,
                          $5::uuid, $6::jsonb, $7)
        `, challengeDigest(challengeKey), purpose, nullableParam(principalType), nullableParam(principalID), nullableParam(sessionID),
                sessionParam, time.Now().UTC().Add(ttl))
        if err != nil {
                return fmt.Errorf("store challenge: %w", err)
        }
        return nil
}

// consumeChallenge atomically flips consumed_at on exactly one live row. The
// UPDATE is the single-use guarantee: two concurrent finishes race on the
// same row, one wins, the other gets ErrChallengeInvalid. When principal
// identifiers are passed, the row must carry the SAME binding (IS NOT
// DISTINCT FROM so unbound rows still match the all-empty caller of
// discoverable login).
func (s *Service) consumeChallenge(ctx context.Context, challengeKey, purpose, principalType, principalID, sessionID string) (*challengeRecord, error) {
        var record challengeRecord
        var sessionData []byte
        err := s.db.QueryRow(ctx, `
                UPDATE auth_challenges SET consumed_at = NOW()
                WHERE challenge_hash = $1 AND purpose = $2
                  AND consumed_at IS NULL AND expires_at > NOW()
                  AND (principal_type IS NOT DISTINCT FROM $3)
                  AND (principal_id   IS NOT DISTINCT FROM $4::uuid)
                  AND (session_id     IS NOT DISTINCT FROM $5::uuid)
                RETURNING purpose, COALESCE(principal_type, ''), COALESCE(principal_id::text, ''),
                          COALESCE(session_id::text, ''), expires_at, COALESCE(session_data, 'null'::jsonb)
        `, challengeDigest(challengeKey), purpose, nullableParam(principalType), nullableParam(principalID), nullableParam(sessionID)).Scan(
                &record.Purpose, &record.PrincipalType, &record.PrincipalID,
                &record.SessionID, &record.ExpiresAt, &sessionData)
        if errors.Is(err, pgx.ErrNoRows) {
                return nil, ErrChallengeInvalid
        }
        if err != nil {
                return nil, fmt.Errorf("consume challenge: %w", err)
        }
        if len(sessionData) > 0 && string(sessionData) != "null" {
                if err := json.Unmarshal(sessionData, &record.Session); err != nil {
                        return nil, fmt.Errorf("decode session data: %w", err)
                }
                // Defense in depth: the library checks Session.Expires too —
                // make it agree with the ledger row.
                record.Session.Expires = record.ExpiresAt
        }
        return &record, nil
}

// consumeRecoveryChallenge is the recovery-only consumer: the caller is
// UNAUTHENTICATED, so the only pre-conditions the ledger can enforce are
// the key, the purpose, expiry and single-use — the principal binding is
// PROVEN after consumption (recovery finish loads record.PrincipalID and
// the vendor re-checks session.UserID / allow-list / userHandle before the
// signature). Task 161: the generic consumeChallenge with all-empty
// expectations turns them into typed NULLs (IS NOT DISTINCT FROM), which
// can never match a BOUND row — every real recovery died with
// ErrChallengeInvalid before even reaching the relay. Decoy rows stay
// consumable (and dead) through the explicit unbinding check in the
// finish — single-use holds for failures exactly as for successes.
func (s *Service) consumeRecoveryChallenge(ctx context.Context, challengeKey string) (*challengeRecord, error) {
        var record challengeRecord
        var sessionData []byte
        err := s.db.QueryRow(ctx, `
                UPDATE auth_challenges SET consumed_at = NOW()
                WHERE challenge_hash = $1 AND purpose = $2
                  AND consumed_at IS NULL AND expires_at > NOW()
                RETURNING purpose, COALESCE(principal_type, ''), COALESCE(principal_id::text, ''),
                          COALESCE(session_id::text, ''), expires_at, COALESCE(session_data, 'null'::jsonb)
        `, challengeDigest(challengeKey), ChallengePurposeRecovery).Scan(
                &record.Purpose, &record.PrincipalType, &record.PrincipalID,
                &record.SessionID, &record.ExpiresAt, &sessionData)
        if errors.Is(err, pgx.ErrNoRows) {
                return nil, ErrChallengeInvalid
        }
        if err != nil {
                return nil, fmt.Errorf("consume recovery challenge: %w", err)
        }
        if len(sessionData) > 0 && string(sessionData) != "null" {
                if err := json.Unmarshal(sessionData, &record.Session); err != nil {
                        return nil, fmt.Errorf("decode session data: %w", err)
                }
                record.Session.Expires = record.ExpiresAt
        }
        return &record, nil
}

func challengeDigest(challengeKey string) []byte {
        digest := sha256.Sum256([]byte(challengeKey))
        return digest[:]
}

func randomChallenge() ([]byte, error) {
        raw := make([]byte, 32)
        if _, err := rand.Read(raw); err != nil {
                return nil, err
        }
        return raw, nil
}

// ---------------------------------------------------------------------------
// Credential store — public key material only
// ---------------------------------------------------------------------------

func (s *Service) loadPasskeyUser(ctx context.Context, principal *Principal) (*passkeyUser, error) {
        rows, err := s.db.Query(ctx, `
                SELECT credential_id, public_key, attestation_type, transports,
                       aaguid, sign_count, backup_eligible, backup_state, uv_initialized
                FROM passkey_credentials
                WHERE principal_type = $1 AND principal_id = $2::uuid AND revoked_at IS NULL
        `, principal.Type, principal.ID)
        if err != nil {
                return nil, fmt.Errorf("load passkeys: %w", err)
        }
        defer rows.Close()

        user := &passkeyUser{principal: principal}
        for rows.Next() {
                var credential webauthn.Credential
                var transports []string
                var aaguid string
                var signCount int64
                var backupEligible, backupState, uvInitialized bool
                if err := rows.Scan(&credential.ID, &credential.PublicKey, &credential.AttestationType,
                        &transports, &aaguid, &signCount, &backupEligible, &backupState, &uvInitialized); err != nil {
                        return nil, fmt.Errorf("scan passkey: %w", err)
                }
                credential.Flags = webauthn.CredentialFlags{
                        UserPresent:   true,
                        UserVerified:  uvInitialized,
                        BackupEligible: backupEligible,
                        BackupState:    backupState,
                }
                credential.Transport = transportsToProtocol(transports)
                if raw, err := hex.DecodeString(aaguid); err == nil {
                        credential.Authenticator.AAGUID = raw
                }
                credential.Authenticator.SignCount = uint32(signCount)
                user.credentials = append(user.credentials, credential)
        }
        if err := rows.Err(); err != nil {
                return nil, fmt.Errorf("iterate passkeys: %w", err)
        }
        return user, nil
}

func transportsToProtocol(values []string) []protocol.AuthenticatorTransport {
        if len(values) == 0 {
                return nil
        }
        transports := make([]protocol.AuthenticatorTransport, 0, len(values))
        for _, value := range values {
                transports = append(transports, protocol.AuthenticatorTransport(value))
        }
        return transports
}

func transportsToStrings(transports []protocol.AuthenticatorTransport) []string {
        if len(transports) == 0 {
                return []string{}
        }
        values := make([]string, 0, len(transports))
        for _, transport := range transports {
                values = append(values, string(transport))
        }
        return values
}

func (s *Service) persistPasskeyCredential(ctx context.Context, principal *Principal, credential *webauthn.Credential) error {
        _, err := s.db.Exec(ctx, `
                INSERT INTO passkey_credentials (
                        principal_type, principal_id, credential_id, public_key,
                        attestation_type, transports, aaguid, sign_count,
                        backup_eligible, backup_state, uv_initialized, last_used_at
                ) VALUES ($1, $2::uuid, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW())
        `, principal.Type, principal.ID, credential.ID, credential.PublicKey,
                credential.AttestationType, transportsToStrings(credential.Transport),
                hex.EncodeToString(credential.Authenticator.AAGUID), credential.Authenticator.SignCount,
                credential.Flags.BackupEligible, credential.Flags.BackupState, credential.Flags.UserVerified)
        if err != nil {
                if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
                        return ErrPasskeyExists
                }
                return fmt.Errorf("persist passkey: %w", err)
        }
        return nil
}

// touchPasskeyCredential records a successful assertion: the new signature
// counter (clone detection), the backup flag (multi-device sync state can
// change) and the latch of user verification.
func (s *Service) touchPasskeyCredential(ctx context.Context, credential *webauthn.Credential) {
        _, err := s.db.Exec(ctx, `
                UPDATE passkey_credentials
                SET sign_count = $2, backup_state = $3,
                    uv_initialized = uv_initialized OR $4, last_used_at = NOW()
                WHERE credential_id = $1
        `, credential.ID, credential.Authenticator.SignCount,
                credential.Flags.BackupState, credential.Flags.UserVerified)
        if err != nil {
                log.Printf("[PASSKEY] credential touch failed: %v", err)
        }
}

// resolvePasskeyUser is the DiscoverableUserHandler backend: it turns the
// asserted credential id into the owning account. The user handle the
// authenticator reports must equal the account's WebAuthnID — a mismatch
// means the response does not describe this account.
func (s *Service) resolvePasskeyUser(ctx context.Context, rawID, userHandle []byte) (*passkeyUser, *webauthn.Credential, error) {
        var principalType, principalID string
        err := s.db.QueryRow(ctx, `
                SELECT principal_type, principal_id::text
                FROM passkey_credentials
                WHERE credential_id = $1 AND revoked_at IS NULL
        `, rawID).Scan(&principalType, &principalID)
        if err != nil {
                return nil, nil, ErrPasskeyRejected
        }
        principal, err := s.findPrincipalByID(ctx, principalType, principalID)
        if err != nil {
                return nil, nil, ErrPasskeyRejected
        }
        if !bytesEqual(userHandle, webAuthnIDForPrincipal(principal.ID)) {
                return nil, nil, ErrPasskeyRejected
        }
        user, err := s.loadPasskeyUser(ctx, principal)
        if err != nil {
            return nil, nil, err
        }
        for index := range user.credentials {
                if bytesEqual(user.credentials[index].ID, rawID) {
                        return user, &user.credentials[index], nil
                }
        }
        return nil, nil, ErrPasskeyRejected
}

func bytesEqual(a, b []byte) bool {
        if len(a) != len(b) {
                return false
        }
        var diff byte
        for index := range a {
                diff |= a[index] ^ b[index]
        }
        return diff == 0
}

// ---------------------------------------------------------------------------
// Registration — add a passkey to an AUTHENTICATED account
// ---------------------------------------------------------------------------

// BeginPasskeyRegistration issues creation options for the signed-in
// account. Registration always requires an authenticated session and the
// challenge is bound to BOTH the account and that session (rule #2) — a
// stolen options blob is worthless without the session.
func (s *Service) BeginPasskeyRegistration(ctx context.Context, principal *Principal, sessionID string) (*protocol.CredentialCreation, error) {
        relay, err := s.relay()
        if err != nil {
                return nil, err
        }
        user, err := s.loadPasskeyUser(ctx, principal)
        if err != nil {
                return nil, err
        }
        options := []webauthn.RegistrationOption{
                // Discoverable credential + user verification REQUIRED — the
                // whole point is a biometric/PIN-gated passkey that can
                // replace the password at login.
                webauthn.WithAuthenticatorSelection(webauthn.SelectAuthenticator(
                        "", protocol.ResidentKeyRequired(), "required")),
        }
        if len(user.credentials) > 0 {
                options = append(options, webauthn.WithExclusions(credentialsToDescriptors(user.credentials)))
        }
        creation, session, err := relay.BeginRegistration(user, options...)
        if err != nil {
                return nil, fmt.Errorf("begin passkey registration: %w", err)
        }
        if err := s.storeChallenge(ctx, session.Challenge, ChallengePurposeRegister,
                principal.Type, principal.ID, sessionID, session, challengeTTL); err != nil {
                return nil, err
        }
        return creation, nil
}

// FinishPasskeyRegistration verifies the attestation response against the
// stored ceremony, persists the credential's PUBLIC material and audits the
// event. Device name arrives as a query parameter (the request body belongs
// to the WebAuthn library).
func (s *Service) FinishPasskeyRegistration(ctx context.Context, principal *Principal, sessionID, challengeKey string, r *http.Request) (string, error) {
        relay, err := s.relay()
        if err != nil {
                return "", err
        }
        record, err := s.consumeChallenge(ctx, challengeKey, ChallengePurposeRegister,
                principal.Type, principal.ID, sessionID)
        if err != nil {
                return "", err
        }
        user, err := s.loadPasskeyUser(ctx, principal)
        if err != nil {
                return "", err
        }
        credential, err := relay.FinishRegistration(user, record.Session, r)
        if err != nil {
                log.Printf("[PASSKEY] registration rejected for principal %s: %v", principal.ID, err)
                return "", ErrPasskeyRejected
        }
        if err := s.persistPasskeyCredential(ctx, principal, credential); err != nil {
                return "", err
        }
        deviceName := sanitizeDeviceName(r.URL.Query().Get("device_name"))
        s.auditPasskeyEvent(ctx, principal, "passkey.registered", "info",
                "تسجيل مفتاح دخول حيوي جديد", map[string]any{
                        "device_name": deviceName,
                        "backup_eligible": credential.Flags.BackupEligible,
                })
        return deviceName, nil
}

func sanitizeDeviceName(value string) string {
        value = strings.TrimSpace(value)
        value = strings.Map(func(char rune) rune {
                if char < 32 || char == 127 {
                        return -1
                }
                return char
        }, value)
        if len(value) > 120 {
                value = value[:120]
        }
        return value
}

// ---------------------------------------------------------------------------
// Discoverable login — the passkey REPLACES the password
// ---------------------------------------------------------------------------

// BeginPasskeyLogin issues assertion options with NO account binding: the
// authenticator itself reveals the account through its discoverable
// credential (usernameless flow). User verification is REQUIRED — the
// biometric/PIN gate is what stands in for the password.
func (s *Service) BeginPasskeyLogin(ctx context.Context) (*protocol.CredentialAssertion, error) {
        relay, err := s.relay()
        if err != nil {
                return nil, err
        }
        raw, err := randomChallenge()
        if err != nil {
                return nil, err
        }
        assertion, session, err := relay.BeginDiscoverableLogin(
                webauthn.WithChallenge(raw),
                webauthn.WithUserVerification("required"),
        )
        if err != nil {
                return nil, fmt.Errorf("begin passkey login: %w", err)
        }
        if err := s.storeChallenge(ctx, session.Challenge, ChallengePurposeLogin,
                "", "", "", session, challengeTTL); err != nil {
                return nil, err
        }
        return assertion, nil
}

// FinishPasskeyLogin resolves the account from the assertion, enforces the
// realm/inactive/unverified gates exactly like password login, then issues a
// regular session. Returns the same shape as Service.Login.
func (s *Service) FinishPasskeyLogin(ctx context.Context, realm AuthRealm, challengeKey string, r *http.Request, meta RequestMeta) (*Principal, *SessionTokens, error) {
        relay, err := s.relay()
        if err != nil {
                return nil, nil, err
        }
        record, err := s.consumeChallenge(ctx, challengeKey, ChallengePurposeLogin, "", "", "")
        if err != nil {
                return nil, nil, err
        }
        var resolved *passkeyUser
        handler := func(rawID, userHandle []byte) (webauthn.User, error) {
                user, _, err := s.resolvePasskeyUser(ctx, rawID, userHandle)
                if err != nil {
                        return nil, err
                }
                resolved = user
                return user, nil
        }
        credential, err := relay.FinishDiscoverableLogin(handler, record.Session, r)
        if err != nil || resolved == nil {
                if resolved != nil {
                        s.auditPasskeyEvent(ctx, resolved.principal, "passkey.login.failed", "warning",
                                "محاولة دخول حيوي فاشلة", map[string]any{"reason": "verification_failed"})
                }
                log.Printf("[PASSKEY] login rejected: %v", err)
                return nil, nil, ErrPasskeyRejected
        }
        principal := resolved.principal

        // Same gates as password login — the passkey replaces the password,
        // not the account rules.
        if !principalAllowedInRealm(principal, realm) {
                s.auditPasskeyEvent(ctx, principal, "passkey.login.failed", "warning",
                        "محاولة دخول حيوي فاشلة — الحساب غير مسموح في هذا النطاق",
                        map[string]any{"reason": "realm_mismatch"})
                return nil, nil, ErrPasskeyRejected
        }
        if !principal.IsActive {
                s.auditPasskeyEvent(ctx, principal, "passkey.login.failed", "warning",
                        "محاولة دخول حيوي لحساب موقوف", map[string]any{"reason": "account_inactive"})
                return nil, nil, ErrAccountInactive
        }
        if !principal.EmailVerified {
                s.auditPasskeyEvent(ctx, principal, "passkey.login.failed", "warning",
                        "محاولة دخول حيوي قبل توثيق البريد", map[string]any{"reason": "email_not_verified"})
                return nil, nil, ErrEmailNotVerified
        }

        s.touchPasskeyCredential(ctx, credential)
        if err := s.recordSuccessfulLogin(ctx, principal); err != nil {
                return nil, nil, fmt.Errorf("update login state: %w", err)
        }
        if principal.Role == "super_admin" {
                s.clearSuperAdminFailures(principal.Email, meta.IPAddress)
        }
        tokens, err := s.createSession(ctx, principal, realm, meta)
        if err != nil {
                return nil, nil, err
        }
        s.auditPasskeyEvent(ctx, principal, "passkey.login.success", "info",
                "تسجيل دخول ناجح بالمفتاح الحيوي", map[string]any{
                        "backup_state": credential.Flags.BackupState,
                        "clone_warning": credential.Authenticator.CloneWarning,
                })
        return principal, tokens, nil
}

// ---------------------------------------------------------------------------
// Management — listing, revocation, session binding
// ---------------------------------------------------------------------------

// sessionIDForToken resolves the auth_sessions.id of the caller's access
// token so challenges can be bound to the exact session (rule #2). Unknown
// tokens simply bind to NULL — the ceremony stays valid, just less tightly
// scoped (defense in depth, never a hard dependency).
func (s *Service) sessionIDForToken(ctx context.Context, accessToken string) string {
        if accessToken == "" {
                return ""
        }
        var sessionID string
        if err := s.db.QueryRow(ctx, `
                SELECT id::text FROM auth_sessions
                WHERE access_token_hash = $1 AND revoked_at IS NULL
                  AND refresh_expires_at > NOW()
        `, tokenHash(accessToken)).Scan(&sessionID); err != nil {
                return ""
        }
        return sessionID
}

// PasskeyRow is the client-facing view of a registered device.
type PasskeyRow struct {
        ID             string    `json:"id"`
        DeviceName     string    `json:"device_name"`
        BackupEligible bool      `json:"backup_eligible"`
        BackupState    bool      `json:"backup_state"`
        LastUsedAt     *time.Time `json:"last_used_at"`
        CreatedAt      time.Time `json:"created_at"`
}

func (s *Service) ListPasskeys(ctx context.Context, principal *Principal) ([]PasskeyRow, error) {
        rows, err := s.db.Query(ctx, `
                SELECT id::text, device_name, backup_eligible, backup_state,
                       last_used_at, created_at
                FROM passkey_credentials
                WHERE principal_type = $1 AND principal_id = $2::uuid AND revoked_at IS NULL
                ORDER BY created_at
        `, principal.Type, principal.ID)
        if err != nil {
                return nil, fmt.Errorf("list passkeys: %w", err)
        }
        defer rows.Close()
        var list []PasskeyRow
        for rows.Next() {
                var row PasskeyRow
                if err := rows.Scan(&row.ID, &row.DeviceName, &row.BackupEligible,
                        &row.BackupState, &row.LastUsedAt, &row.CreatedAt); err != nil {
                        return nil, fmt.Errorf("scan passkey row: %w", err)
                }
                list = append(list, row)
        }
        return list, rows.Err()
}

// RevokePasskeyCredential removes ONE device. Revoking the LAST credential
// of an account with no password would brick the account (no sign-in method
// left), so that combination is refused — the client is told to add a
// password or another passkey first.
func (s *Service) RevokePasskeyCredential(ctx context.Context, principal *Principal, credentialRowID string) (string, error) {
        var credentialID []byte
        var deviceName string
        err := s.db.QueryRow(ctx, `
                UPDATE passkey_credentials SET revoked_at = NOW()
                WHERE id = $1::uuid
                  AND principal_type = $2 AND principal_id = $3::uuid
                  AND revoked_at IS NULL
                RETURNING credential_id, device_name
        `, credentialRowID, principal.Type, principal.ID).Scan(&credentialID, &deviceName)
        if errors.Is(err, pgx.ErrNoRows) {
                return "", pgx.ErrNoRows
        }
        if err != nil {
                return "", fmt.Errorf("revoke passkey: %w", err)
        }
        // Last-method guard: only relevant when nothing else can sign in.
        // Passwords live on the principal tables themselves.
        passwordTable := "company_users"
        if principal.Type == EmployeePrincipal {
                passwordTable = "employees"
        }
        var remaining int
        var hasPassword bool
        if err := s.db.QueryRow(ctx, `
                SELECT
                  (SELECT COUNT(*) FROM passkey_credentials
                   WHERE principal_type = $1 AND principal_id = $2::uuid AND revoked_at IS NULL),
                  EXISTS (
                        SELECT 1 FROM ` + passwordTable + `
                        WHERE id = $2::uuid AND COALESCE(password_hash, '') <> ''
                  )
        `, principal.Type, principal.ID).Scan(&remaining, &hasPassword); err == nil && remaining == 0 && !hasPassword {
                // Undo the revoke — refusing AFTER the update keeps the
                // guard race-free without a transaction around two writes.
                _, _ = s.db.Exec(ctx, `
                        UPDATE passkey_credentials SET revoked_at = NULL WHERE id = $1::uuid
                `, credentialRowID)
                return "", ErrLastCredential
        }
        s.auditPasskeyEvent(ctx, principal, "passkey.deleted", "warning",
                "إبطال مفتاح دخول حيوي", map[string]any{"device_name": deviceName})
        return deviceName, nil
}

// auditPasskeyEvent routes passkey security events into the same platform
// audit stream as the login events — best-effort, never blocking.
func (s *Service) auditPasskeyEvent(ctx context.Context, principal *Principal, action, severity, summary string, extra map[string]any) {
        meta := RequestMeta{}
        s.auditPasskeyEventWithMeta(ctx, principal, meta, action, severity, summary, extra)
}

func (s *Service) auditPasskeyEventWithMeta(ctx context.Context, principal *Principal, meta RequestMeta, action, severity, summary string, extra map[string]any) {
        realm := AuthRealm("")
        if principal != nil && principal.Type == CompanyUserPrincipal && principal.Role == "super_admin" {
                realm = PlatformRealm
        } else if principal != nil {
                realm = PharmacyRealm
        }
        metadata := map[string]any{
                "ip_address": meta.IPAddress,
                "user_agent": meta.UserAgent,
        }
        if realm != "" {
                metadata["realm"] = string(realm)
        }
        for key, value := range extra {
                if value != nil {
                        metadata[key] = value
                }
        }
        db := s.auditDB
        if db == nil {
                if s.db == nil {
                        return
                }
                db = s.db
        }
        actor := auditlog.Actor{}
        entityID := ""
        companyID := ""
        if principal != nil {
                actor = auditlog.Actor{
                        ID:          principal.ID,
                        Email:       principal.Email,
                        DisplayName: principal.DisplayName,
                        Role:        principal.Role,
                }
                entityID = principal.ID
                metadata["principal_type"] = principal.Type
                if principal.CompanyID != "" {
                        companyID = principal.CompanyID
                        metadata["company_id"] = principal.CompanyID
                }
                if principal.PharmacyID != "" {
                        metadata["pharmacy_id"] = principal.PharmacyID
                }
        }
        if err := auditlog.Platform(ctx, db, actor,
                action, "authentication", "passkey", entityID, companyID, severity,
                metadata, summary); err != nil {
                log.Printf("[AUTH AUDIT] %s write failed: %v", action, err)
        }
}
