package auth

// Task 151 — scoped step-up verification + passkey account recovery.
//
// Step-up (owner rule #3): NO blanket "verified for N minutes" authority.
// Each successful re-verification mints a grant bound to exactly ONE
// allowlisted purpose, ONE principal, with a short TTL, consumed atomically
// on first use. Sensitive operations demand their own fresh grant.
//
// Recovery (owner rule #4): a trusted-device passkey whose assertion was
// issued under a recovery-specific challenge unlocks a single-use password
// reset grant. The use revokes EVERY session of the account (announced in
// the security email) and sends a security notification — losing a device
// means the attacker holding a session loses it the moment recovery runs.
//
// Lost-everything path (owner rule #5): the classic email reset flow remains
// untouched — passkey recovery is an ADDITIONAL path, never the only one.

import (
        "context"
        "errors"
        "fmt"
        "log"
        "net/http"
        "time"

        "github.com/gin-gonic/gin"
        "github.com/go-webauthn/webauthn/protocol"
        "github.com/go-webauthn/webauthn/webauthn"
        "github.com/jackc/pgx/v5"
        "golang.org/x/crypto/bcrypt"
)

const (
        // StepUpHeaderName carries the grant token on the protected request.
        StepUpHeaderName = "X-Step-Up-Token"

        StepUpPurposeChangePassword = "change_password"
        StepUpPurposeManagePasskeys = "manage_passkeys"

        // Internal grant purpose for the recovery reset itself — not part of
        // the interactive step-up allowlist.
        RecoveryGrantPurpose = "recovery_password_reset"

        stepUpGrantTTL   = 5 * time.Minute
        recoveryGrantTTL = 10 * time.Minute
)

// stepUpPurposes is the allowlist of operation classes a grant may certify.
// Anything outside it is refused at begin AND at consume — a grant minted
// for one purpose can never unlock another.
var stepUpPurposes = map[string]string{
        StepUpPurposeChangePassword: "تغيير كلمة المرور",
        StepUpPurposeManagePasskeys: "إدارة مفاتيح الدخول الحيوية",
}

var ErrStepUpUnsupported = errors.New("unsupported step-up purpose")

// ---------------------------------------------------------------------------
// Interactive step-up — fresh verification for one specific operation
// ---------------------------------------------------------------------------

func validateStepUpPurpose(purpose string) error {
        if _, ok := stepUpPurposes[purpose]; !ok {
                return ErrStepUpUnsupported
        }
        return nil
}

// BeginStepUp issues assertion options restricted to the account's own
// passkeys, under a challenge bound to the account, the session and the
// stepup_verify purpose. Requires at least one active passkey — clients
// fall back to the password step-up when this answers ErrNoActivePasskey.
func (s *Service) BeginStepUp(ctx context.Context, principal *Principal, sessionID, purpose string) (*protocol.CredentialAssertion, error) {
        if err := validateStepUpPurpose(purpose); err != nil {
                return nil, err
        }
        relay, err := s.relay()
        if err != nil {
                return nil, err
        }
        user, err := s.loadPasskeyUser(ctx, principal)
        if err != nil {
                return nil, err
        }
        if len(user.credentials) == 0 {
                return nil, ErrNoActivePasskey
        }
        raw, err := randomChallenge()
        if err != nil {
                return nil, err
        }
        assertion, session, err := relay.BeginLogin(user,
                webauthn.WithChallenge(raw),
                webauthn.WithUserVerification("required"),
                webauthn.WithAllowedCredentials(credentialsToDescriptors(user.credentials)),
        )
        if err != nil {
                return nil, fmt.Errorf("begin step-up: %w", err)
        }
        if err := s.storeChallenge(ctx, session.Challenge, ChallengePurposeStepUp,
                principal.Type, principal.ID, sessionID, session, challengeTTL); err != nil {
                return nil, err
        }
        return assertion, nil
}

// FinishStepUp verifies the assertion and mints the scoped grant. The token
// is returned ONCE; only its SHA-256 is stored.
func (s *Service) FinishStepUp(ctx context.Context, principal *Principal, sessionID, purpose, challengeKey string, r *http.Request) (string, error) {
        if err := validateStepUpPurpose(purpose); err != nil {
                return "", err
        }
        relay, err := s.relay()
        if err != nil {
                return "", err
        }
        record, err := s.consumeChallenge(ctx, challengeKey, ChallengePurposeStepUp,
                principal.Type, principal.ID, sessionID)
        if err != nil {
                return "", err
        }
        user, err := s.loadPasskeyUser(ctx, principal)
        if err != nil {
                return "", err
        }
        credential, err := relay.FinishLogin(user, record.Session, r)
        if err != nil {
                log.Printf("[PASSKEY] step-up rejected for principal %s: %v", principal.ID, err)
                s.auditPasskeyEvent(ctx, principal, "stepup.failed", "warning",
                        "فشل التحقق الحيوي الثانوي", map[string]any{"purpose": purpose})
                return "", ErrPasskeyRejected
        }
        s.touchPasskeyCredential(ctx, credential)
        token, err := s.issueStepUpGrant(ctx, principal, purpose, "passkey", credential.ID, stepUpGrantTTL)
        if err != nil {
                return "", err
        }
        s.auditPasskeyEvent(ctx, principal, "stepup.granted", "info",
                "تحقق ثانوي ناجح — إذن محدود الغرض", map[string]any{
                        "purpose": purpose, "method": "passkey",
                })
        return token, nil
}

// ---------------------------------------------------------------------------
// Grants — scoped, single-use, hashed at rest (owner rule #3)
// ---------------------------------------------------------------------------

// issueStepUpGrant mints a random grant token bound to one principal, one
// purpose and one method. Only the SHA-256 of the token is stored; the token
// is shown to the client exactly once.
func (s *Service) issueStepUpGrant(ctx context.Context, principal *Principal, purpose, method string, credentialID []byte, ttl time.Duration) (string, error) {
        token, err := randomToken(32)
        if err != nil {
                return "", err
        }
        _, err = s.db.Exec(ctx, `
                INSERT INTO auth_stepup_grants (
                        principal_type, principal_id, purpose, token_hash,
                        method, credential_id, expires_at
                ) VALUES ($1, $2::uuid, $3, $4, $5, $6, $7)
        `, principal.Type, principal.ID, purpose, tokenHash(token), method, credentialID,
                time.Now().UTC().Add(ttl))
        if err != nil {
                return "", fmt.Errorf("issue step-up grant: %w", err)
        }
        return token, nil
}

// ConsumeStepUpGrant verifies + burns a grant atomically. Purpose and
// principal MUST match the grant row — a grant for change_password can never
// authorize manage_passkeys, and a grant can never be spent for another
// account even if the token leaks.
func (s *Service) ConsumeStepUpGrant(ctx context.Context, principal *Principal, purpose, token string) (bool, error) {
        if token == "" || purpose == "" {
                return false, nil
        }
        tag, err := s.db.Exec(ctx, `
                UPDATE auth_stepup_grants SET consumed_at = NOW()
                WHERE token_hash = $1
                  AND principal_type = $2 AND principal_id = $3::uuid
                  AND purpose = $4
                  AND consumed_at IS NULL AND expires_at > NOW()
        `, tokenHash(token), principal.Type, principal.ID, purpose)
        if err != nil {
                return false, fmt.Errorf("consume step-up grant: %w", err)
        }
        return tag.RowsAffected() == 1, nil
}

// consumeGrantRow is the unauthenticated twin used by recovery: the token
// itself identifies the row; the principal comes back with it.
func (s *Service) consumeGrantRow(ctx context.Context, token, purpose string) (*Principal, error) {
        if token == "" {
                return nil, ErrChallengeInvalid
        }
        var principalType, principalID string
        err := s.db.QueryRow(ctx, `
                UPDATE auth_stepup_grants SET consumed_at = NOW()
                WHERE token_hash = $1 AND purpose = $2
                  AND consumed_at IS NULL AND expires_at > NOW()
                RETURNING principal_type, principal_id::text
        `, tokenHash(token), purpose).Scan(&principalType, &principalID)
        if errors.Is(err, pgx.ErrNoRows) {
                return nil, ErrChallengeInvalid
        }
        if err != nil {
                return nil, fmt.Errorf("consume grant: %w", err)
        }
        principal, err := s.findPrincipalByID(ctx, principalType, principalID)
        if err != nil {
                return nil, ErrChallengeInvalid
        }
        return principal, nil
}

// RequireStepUp is the gin gate for sensitive operations: a valid,
// unconsumed, unexpired grant bound to THIS principal and THIS purpose must
// ride the X-Step-Up-Token header. Anything else is 403 stepup_required.
func (s *Service) RequireStepUp(purpose string) gin.HandlerFunc {
        return func(c *gin.Context) {
                principal, ok := PrincipalFromContext(c)
                if !ok {
                        writeError(c, http.StatusUnauthorized, "authentication_required", "Authentication required")
                        return
                }
                granted, err := s.ConsumeStepUpGrant(c.Request.Context(), principal, purpose, c.GetHeader(StepUpHeaderName))
                if err != nil {
                        log.Printf("[PASSKEY] step-up gate error: %v", err)
                        writeError(c, http.StatusInternalServerError, "stepup_unavailable", "تعذر التحقق الثانوي الآن. حاول بعد لحظات")
                        return
                }
                if !granted {
                        writeError(c, http.StatusForbidden, "stepup_required", "يتطلب هذا الإجراء تحققًا ثانويًا حديثًا")
                        return
                }
                c.Next()
        }
}

// PasswordStepUp is the second verification method: re-entering the current
// password mints the same scoped grant. This is what keeps passkey
// management operable when the passkey itself is the thing being removed.
func (s *Service) PasswordStepUp(ctx context.Context, principal *Principal, purpose, password string) (string, error) {
        if err := validateStepUpPurpose(purpose); err != nil {
                return "", err
        }
        if principal.PasswordHash == "" || bcrypt.CompareHashAndPassword([]byte(principal.PasswordHash), []byte(password)) != nil {
                s.auditPasskeyEvent(ctx, principal, "stepup.failed", "warning",
                        "فشل التحقق الثانوي بكلمة المرور", map[string]any{"purpose": purpose})
                return "", ErrInvalidCredentials
        }
        token, err := s.issueStepUpGrant(ctx, principal, purpose, "password", nil, stepUpGrantTTL)
        if err != nil {
                return "", err
        }
        s.auditPasskeyEvent(ctx, principal, "stepup.granted", "info",
                "تحقق ثانوي ناجح — إذن محدود الغرض", map[string]any{
                        "purpose": purpose, "method": "password",
                })
        return token, nil
}

// ---------------------------------------------------------------------------
// Account recovery — the trusted-device path (owner rule #4)
// ---------------------------------------------------------------------------

// BeginPasskeyRecovery issues assertion options for a password-reset via a
// registered passkey. Unknown/ambiguous/passkey-less emails get an equally
// valid-looking decoy ceremony so the endpoint cannot enumerate accounts:
// the decoy challenge is never bound to a principal and its finish always
// fails.
func (s *Service) BeginPasskeyRecovery(ctx context.Context, email string) (*protocol.CredentialAssertion, error) {
        relay, err := s.relay()
        if err != nil {
                return nil, err
        }
        principal, err := s.FindPrincipal(ctx, email, "", "")
        if err == nil {
                user, userErr := s.loadPasskeyUser(ctx, principal)
                if userErr == nil && len(user.credentials) > 0 && principal.IsActive {
                        raw, rawErr := randomChallenge()
                        if rawErr != nil {
                                return nil, rawErr
                        }
                        assertion, session, beginErr := relay.BeginLogin(user,
                                webauthn.WithChallenge(raw),
                                webauthn.WithUserVerification("required"),
                                webauthn.WithAllowedCredentials(credentialsToDescriptors(user.credentials)),
                        )
                        if beginErr == nil {
                                if storeErr := s.storeChallenge(ctx, session.Challenge, ChallengePurposeRecovery,
                                        principal.Type, principal.ID, "", session, challengeTTL); storeErr == nil {
                                        return assertion, nil
                                }
                        }
                }
        }
        // Decoy: indistinguishable options, unbound challenge.
        raw, err := randomChallenge()
        if err != nil {
                return nil, err
        }
        assertion, session, err := relay.BeginDiscoverableLogin(
                webauthn.WithChallenge(raw),
                webauthn.WithUserVerification("required"),
        )
        if err != nil {
                return nil, fmt.Errorf("begin recovery decoy: %w", err)
        }
        // Task 162 — the empty list was itself the enumeration oracle: a bound
        // ceremony (real account) carries the account's credential ids while
        // BeginDiscoverableLogin leaves allowCredentials empty, so an attacker
        // could read "this email owns a passkey" straight off the response
        // shape without ever finishing a ceremony. One random 32-byte
        // descriptor restores wire-shape parity with the dominant real shape
        // (a single registered passkey): it is never satisfiable (the finish
        // refuses unbound rows before any credential matching) and never
        // collides with a real credential id (fresh 256-bit draw per request).
        fakeID, fakeErr := randomChallenge()
        if fakeErr != nil {
                return nil, fakeErr
        }
        assertion.Response.AllowedCredentials = []protocol.CredentialDescriptor{{
                Type:         protocol.PublicKeyCredentialType,
                CredentialID: protocol.URLEncodedBase64(fakeID),
        }}
        if err := s.storeChallenge(ctx, session.Challenge, ChallengePurposeRecovery,
                "", "", "", session, challengeTTL); err != nil {
                return nil, err
        }
        return assertion, nil
}

// FinishPasskeyRecovery verifies the assertion, then: revokes EVERY session
// of the account (announced policy), mints the single-use reset grant and
// fires the security notification. The reset itself is a separate call so
// the new password never crosses the ceremony request.
func (s *Service) FinishPasskeyRecovery(ctx context.Context, challengeKey string, r *http.Request, meta RequestMeta) (token string, email string, err error) {
        relay, err := s.relay()
        if err != nil {
                return "", "", err
        }
        record, err := s.consumeRecoveryChallenge(ctx, challengeKey)
        if err != nil {
                return "", "", err
        }
        // Unbound rows are decoys — they exist so unknown emails look
        // identical, and they can never succeed.
        if record.PrincipalID == "" || record.PrincipalType == "" {
                return "", "", ErrPasskeyRejected
        }
        // Task 161 — begin is a BOUND ceremony (relay.BeginLogin with the
        // account's allowed credentials), so finish must be bound too. TWO
        // stacked defects made every real recovery fail right after a
        // successful fingerprint (field report 2026-11-09):
        //   1. consumeChallenge with all-empty expectations turns them into
        //      typed NULLs (IS NOT DISTINCT FROM) which never match the BOUND
        //      row the begin wrote → ErrChallengeInvalid before the relay;
        //      fixed by the dedicated consumeRecoveryChallenge (key + purpose
        //      + expiry + single-use only, binding proven below).
        //   2. relay.FinishDiscoverableLogin hard-rejects any session whose
        //      UserID is set (vendor guard, login.go ValidatePasskeyLogin);
        //      fixed by verifying against the account the challenge was
        //      issued FOR. The vendor then re-proves the binding three ways
        //      before checking the signature (session.UserID ==
        //      user.WebAuthnID, assertion ∈ session allow-list, response
        //      userHandle == user.WebAuthnID), and an account deleted between
        //      begin and finish fails closed below.
        principal, err := s.findPrincipalByID(ctx, record.PrincipalType, record.PrincipalID)
        if err != nil {
                log.Printf("[PASSKEY] recovery principal load failed: %v", err)
                return "", "", ErrPasskeyRejected
        }
        user, err := s.loadPasskeyUser(ctx, principal)
        if err != nil || user == nil || len(user.credentials) == 0 {
                log.Printf("[PASSKEY] recovery credential load failed: %v", err)
                return "", "", ErrPasskeyRejected
        }
        credential, err := relay.FinishLogin(user, record.Session, r)
        if err != nil {
                log.Printf("[PASSKEY] recovery rejected: %v", err)
                return "", "", ErrPasskeyRejected
        }
        if !principal.IsActive {
                return "", "", ErrAccountInactive
        }

        // Rule #4 — announced session invalidation: every live session dies
        // the moment recovery verification succeeds.
        if err := s.RevokeAll(ctx, principal); err != nil {
                log.Printf("[PASSKEY] recovery session revoke failed: %v", err)
        }
        token, err = s.issueStepUpGrant(ctx, principal, RecoveryGrantPurpose, "passkey", credential.ID, recoveryGrantTTL)
        if err != nil {
                return "", "", err
        }
        s.touchPasskeyCredential(ctx, credential)
        s.auditPasskeyEvent(ctx, principal, "passkey.recovery.used", "warning",
                "استعادة حساب عبر المفتاح الحيوي — أُلغيت جميع الجلسات", map[string]any{
                        "ip_address": meta.IPAddress,
                })
        return token, principal.Email, nil
}

// RecoveryPasswordReset spends the recovery grant and sets the new password.
// Runs through the same update path as the email reset so password policy,
// password_changed_at and side effects stay identical.
func (s *Service) RecoveryPasswordReset(ctx context.Context, token, newPassword string) (*Principal, error) {
        principal, err := s.consumeGrantRow(ctx, token, RecoveryGrantPurpose)
        if err != nil {
                return nil, err
        }
        hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
        if err != nil {
                return nil, fmt.Errorf("hash password: %w", err)
        }
        if err := s.updatePassword(ctx, principal, string(hash)); err != nil {
                return nil, err
        }
        s.auditPasskeyEvent(ctx, principal, "password.reset.passkey_recovery", "warning",
                "إعادة تعيين كلمة المرور عبر استعادة المفتاح الحيوي", nil)
        return principal, nil
}
