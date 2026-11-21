package auth

// P5 regression gate for R-13 — refresh-token reuse detection, against the
// real migration chain (1→44) through passkeyTestPool (TEST_DATABASE_URL,
// same harness as the Task 151 ledger gates).
//
// Before this wave, Service.Refresh answered a replayed (already-rotated)
// refresh token with ErrInvalidToken and nothing else: the lookup filtered
// revoked_at IS NULL, so the revoked row was invisible and family_id —
// though stored and indexed — was never acted on. A thief who stole a
// refresh token and raced the real user's rotation could keep one live
// session alive forever. The wave pins:
//
//   - rotation still works exactly as before (new live session, old one
//     revoked with replaced_by_session_id set),
//   - THE HOLE, now closed: replaying the old (revoked) refresh token is
//     rejected with the SAME ErrInvalidToken wire contract, AND the whole
//     family is revoked inside the same committed transaction — including
//     the live session the thief was racing,
//   - the replay of the formerly-live rotated token is rejected too (its
//     family is already dead; no rows resurrect),
//   - an audit row lands: platform_audit_logs action='refresh.reuse_detected'
//     with the family id and the number of live sessions the kill took down,
//   - a DIFFERENT family is untouched by the kill (scope stays the family,
//     never the principal), and an unknown token (no revoked match) revokes
//     nothing anywhere.
//
// Run:
//
//	TEST_DATABASE_URL=postgres://.../pharmacy_scratch go test ./internal/auth/ -run TestRefreshReuseDB

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func TestRefreshReuseDB(t *testing.T) {
	pool := passkeyTestPool(t)
	ctx := context.Background()
	stamp := time.Now().UnixNano()

	svc := NewService(pool, Config{})
	meta := RequestMeta{UserAgent: "reuse-gate", IPAddress: ""}

	// ---- seed a super_admin company_user (the PlatformRealm shape) ----
	var companyID, userID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO companies (name, email) VALUES ($1, $2) RETURNING id::text
	`, "reuse-gate-co", fmt.Sprintf("reuse-gate-%d@example.com", stamp)).Scan(&companyID))
	var liveFamilyA, liveFamilyB string
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM platform_audit_logs WHERE metadata->>'family_id' IN ($1, $2)`, liveFamilyA, liveFamilyB)
		_, _ = pool.Exec(ctx, `DELETE FROM auth_sessions WHERE principal_id = $1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM companies WHERE id = $1::uuid`, companyID)
	})
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO company_users (company_id, email, password_hash, first_name, last_name, role, email_verified_at)
		VALUES ($1::uuid, $2, 'x', 'Reuse', 'Gate', 'super_admin', NOW())
		RETURNING id::text
	`, companyID, fmt.Sprintf("reuse-gate-user-%d@example.com", stamp)).Scan(&userID))
	principal := &Principal{Type: CompanyUserPrincipal, ID: userID,
		Email: fmt.Sprintf("reuse-gate-user-%d@example.com", stamp), Role: "super_admin",
		IsActive: true}

	// ---- family A: create → rotate (the happy path, unchanged) ----
	tokens1, err := svc.createSession(ctx, principal, PlatformRealm, meta)
	require.NoError(t, err)
	var familyA string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT family_id::text FROM auth_sessions WHERE refresh_token_hash = $1
	`, tokenHash(tokens1.RefreshToken)).Scan(&familyA))
	liveFamilyA = familyA

	p, tokens2, err := svc.Refresh(ctx, tokens1.RefreshToken, PlatformRealm, meta)
	require.NoError(t, err, "first rotation must succeed exactly as before")
	require.Equal(t, principal.ID, p.ID)
	require.NotEqual(t, tokens1.RefreshToken, tokens2.RefreshToken)
	var replacedBy string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COALESCE(replaced_by_session_id::text, '') FROM auth_sessions
		WHERE refresh_token_hash = $1
	`, tokenHash(tokens1.RefreshToken)).Scan(&replacedBy))
	require.NotEmpty(t, replacedBy, "rotated session points at its replacement")
	require.Equal(t, 1, countLiveFamily(t, pool, familyA),
		"family A has exactly the rotated live session")

	// ---- THE HOLE: replay the old token → rejected AND family killed ----
	_, _, err = svc.Refresh(ctx, tokens1.RefreshToken, PlatformRealm, meta)
	require.ErrorIs(t, err, ErrInvalidToken, "wire contract unchanged: ErrInvalidToken")
	require.Equal(t, 0, countLiveFamily(t, pool, familyA),
		"R-13: replaying a rotated token revokes the WHOLE family — the thief's race window is gone")
	require.Equal(t, 2, countFamily(t, pool, familyA),
		"family A holds the two sessions it held; both revoked, none deleted")

	// ---- audit row: reuse detected with family id + kill count ----
	var audits int
	var killed string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(MAX(metadata->>'sessions_killed'), '')
		FROM platform_audit_logs
		WHERE action = 'refresh.reuse_detected'
		  AND metadata->>'family_id' = $1
	`, familyA).Scan(&audits, &killed))
	require.Equal(t, 1, audits, "exactly one reuse audit row for family A")
	require.Equal(t, "1", killed, "the kill took down the one live session")

	// ---- replaying the formerly-live token is also dead (no resurrection) ----
	_, _, err = svc.Refresh(ctx, tokens2.RefreshToken, PlatformRealm, meta)
	require.ErrorIs(t, err, ErrInvalidToken)
	require.Equal(t, 0, countLiveFamily(t, pool, familyA))

	// ---- control family B: the kill is scoped to the family, not the principal ----
	tokensB1, err := svc.createSession(ctx, principal, PlatformRealm, meta)
	require.NoError(t, err)
	var familyB string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT family_id::text FROM auth_sessions WHERE refresh_token_hash = $1
	`, tokenHash(tokensB1.RefreshToken)).Scan(&familyB))
	require.NotEqual(t, familyA, familyB, "fresh sessions mint a fresh family")
	liveFamilyB = familyB
	_, _, err = svc.Refresh(ctx, tokensB1.RefreshToken, PlatformRealm, meta)
	require.NoError(t, err, "family B rotates normally after family A died")
	require.Equal(t, 1, countLiveFamily(t, pool, familyB),
		"family B keeps its rotated live session")

	// ---- unknown token: no revoked match, no collateral revocation ----
	_, _, err = svc.Refresh(ctx, fmt.Sprintf("unknown-token-%d", stamp), PlatformRealm, meta)
	require.ErrorIs(t, err, ErrInvalidToken)
	require.Equal(t, 1, countLiveFamily(t, pool, familyB),
		"an unknown token must not revoke anything outside a revoked-row match")
}

func countLiveFamily(t *testing.T, pool *pgxpool.Pool, familyID string) int {
	t.Helper()
	ctx := context.Background()
	var n int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM auth_sessions WHERE family_id = $1::uuid AND revoked_at IS NULL
	`, familyID).Scan(&n))
	return n
}

func countFamily(t *testing.T, pool *pgxpool.Pool, familyID string) int {
	t.Helper()
	ctx := context.Background()
	var n int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM auth_sessions WHERE family_id = $1::uuid
	`, familyID).Scan(&n))
	return n
}
