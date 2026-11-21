package auth

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Task 163 — بوابة قاعدة البيانات لإدارة الجلسات (نمط passkey_ledger_db_test):
// schema ذاتي الاحتواء لجدول auth_sessions وحده (ميجريشن 6 مصغّرًا — العقد
// الذي تقرأه وتكتبه دوال الجلسات الجديدة)، ثم أربع محطات انحدار:
// القائمة تعرض الحيّة فقط مع is_current بالهاش، الإبطال الفردي مشروط
// بالملكية وidempotent، «إنهاء الأخرى» لا يمس الجلسة الحالية، و«الجهاز
// المعروف» يستثني أحدث جلسة. التشغيل:
//
//      TEST_DATABASE_URL=postgres://…/pharmacy_scratch go test ./internal/auth/ -run TestSessions -v

func sessionsTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — session gates need a disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := runSessionsSchema(ctx, pool); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return pool
}

func runSessionsSchema(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
                CREATE EXTENSION IF NOT EXISTS "pgcrypto";
                CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
                CREATE TABLE IF NOT EXISTS auth_sessions (
                    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
                    family_id UUID NOT NULL DEFAULT uuid_generate_v4(),
                    auth_realm VARCHAR(16) NOT NULL DEFAULT 'pharmacy',
                    principal_type VARCHAR(32) NOT NULL
                        CHECK (principal_type IN ('company_user', 'employee')),
                    principal_id UUID NOT NULL,
                    access_token_hash BYTEA NOT NULL UNIQUE,
                    refresh_token_hash BYTEA NOT NULL UNIQUE,
                    access_expires_at TIMESTAMPTZ NOT NULL,
                    refresh_expires_at TIMESTAMPTZ NOT NULL,
                    last_used_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                    user_agent TEXT,
                    ip_address INET,
                    replaced_by_session_id UUID REFERENCES auth_sessions(id),
                    revoked_at TIMESTAMPTZ,
                    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
                );
        `)
	return err
}

type sessionSeed struct {
	ua             string
	ip             string
	revoked        bool
	expiredRefresh bool
}

// seedSession يزرع جلسة واحدة ويرجّع (id, accessRaw, refreshRaw) —
// التوكن الخام مطلوب للاستعلام بلا هاش خارجي.
func seedSession(t *testing.T, ctx context.Context, pool *pgxpool.Pool, principalID string, s sessionSeed) (string, string, string) {
	t.Helper()
	access, err := randomToken(32)
	if err != nil {
		t.Fatalf("seed access token: %v", err)
	}
	refresh, err := randomToken(48)
	if err != nil {
		t.Fatalf("seed refresh token: %v", err)
	}
	var id string
	revokedClause := "NULL"
	if s.revoked {
		revokedClause = "NOW()"
	}
	refreshExpiry := "NOW() + INTERVAL '30 days'"
	if s.expiredRefresh {
		refreshExpiry = "NOW() - INTERVAL '1 hour'"
	}
	if err := pool.QueryRow(ctx, `
                INSERT INTO auth_sessions (
                    principal_type, principal_id, access_token_hash, refresh_token_hash,
                    access_expires_at, refresh_expires_at, user_agent, ip_address, revoked_at
                ) VALUES ('employee', $1, $2, $3,
                          NOW() + INTERVAL '15 minutes', `+refreshExpiry+`,
                          $4, NULLIF($5, '')::inet, `+revokedClause+`)
                RETURNING id::text
        `, principalID, tokenHash(access), tokenHash(refresh), s.ua, s.ip).Scan(&id); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return id, access, refresh
}

func newSessionsService(pool *pgxpool.Pool) *Service {
	return &Service{db: pool}
}

// cleanSessionFixtures — عزل بوابات DB عن بقايا المشغّلات السابقة: كل
// اختبار يزرع بمفاتيح UUID ثابتة، وبقية جلسات تشغيل سابق على المفتاح نفسه
// تقلب العدّادات (جلسة «جديدة» قديمة تجعل الجهاز معروفًا، جلسة إضافية
// تزياد من العدد المنتهي). البوابات في CI تعمل على قاعدة أولى النسخ،
// وعلى scratch محلية متراكمة يجب أن تبدأ كل بوابة من صفر.
func cleanSessionFixtures(t *testing.T, ctx context.Context, pool *pgxpool.Pool, principalIDs ...string) {
	t.Helper()
	for _, id := range principalIDs {
		if _, err := pool.Exec(ctx, `DELETE FROM auth_sessions WHERE principal_id = $1`, id); err != nil {
			t.Fatalf("clean fixtures %s: %v", id, err)
		}
	}
}

// TestSessionsListFiltersAndMarksCurrent — القائمة تعرض الجلسات الحية فقط
// (الملغاة تختفي، رفريش المنتهي ميت)، وis_current يهبط على جلسة التوكن
// الحالي وحدها، والترتيب آخر استخدام أولًا.
func TestSessionsListFiltersAndMarksCurrent(t *testing.T) {
	pool := sessionsTestPool(t)
	ctx := context.Background()
	s := newSessionsService(pool)
	principalID := "11111111-1111-1111-1111-111111111111"
	cleanSessionFixtures(t, ctx, pool, principalID)

	phoneID, _, _ := seedSession(t, ctx, pool, principalID, sessionSeed{ua: "iPhone Safari Mobile", ip: "10.0.0.9"})
	_, _, _ = seedSession(t, ctx, pool, principalID, sessionSeed{ua: "stale", revoked: true})
	_, _, _ = seedSession(t, ctx, pool, principalID, sessionSeed{ua: "expired-refresh", expiredRefresh: true})
	// الجلسة الحالية: توكنها هو الممرر لـListSessions.
	var currentAccess string
	var err error
	{
		currentAccess, err = randomToken(32)
		if err != nil {
			t.Fatalf("token: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `
                INSERT INTO auth_sessions (
                    principal_type, principal_id, access_token_hash, refresh_token_hash,
                    access_expires_at, refresh_expires_at, user_agent
                ) VALUES ('employee', $1, $2, $3,
                          NOW() + INTERVAL '15 minutes', NOW() + INTERVAL '30 days', 'Chrome Windows Desktop')
        `, principalID, tokenHash(currentAccess), tokenHash("unused-refresh")); err != nil {
		t.Fatalf("seed current: %v", err)
	}

	sessions, err := s.ListSessions(ctx, &Principal{Type: "employee", ID: principalID}, currentAccess)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("want 2 live sessions (phone + current), got %d: %+v", len(sessions), sessions)
	}
	currentCount := 0
	for _, sess := range sessions {
		switch sess.ID {
		case phoneID:
			if sess.IsCurrent {
				t.Fatalf("phone session wrongly marked current")
			}
		default:
			currentCount++
		}
	}
	if currentCount != 1 {
		t.Fatalf("exactly one session must be current, got %d", currentCount)
	}
}

// TestRevokeSessionOwnershipAndIdempotence — إنهاء جلسة حساب آخر يرفض
// بصمت (0 صف)، وإنهاء الجلسة الملغاة سابقًا لا يفشل (idempotent)،
// وإنهاء جلسة صاحبها يعمل مرة واحدة.
func TestRevokeSessionOwnershipAndIdempotence(t *testing.T) {
	pool := sessionsTestPool(t)
	ctx := context.Background()
	s := newSessionsService(pool)
	me := "22222222-2222-2222-2222-222222222222"
	other := "33333333-3333-3333-3333-333333333333"
	cleanSessionFixtures(t, ctx, pool, me, other)

	mine, _, _ := seedSession(t, ctx, pool, me, sessionSeed{ua: "mine"})
	theirs, _, _ := seedSession(t, ctx, pool, other, sessionSeed{ua: "theirs"})

	// جلسة غريب: لا ملكية → لا تأثير.
	affected, err := s.RevokeSessionByID(ctx, &Principal{Type: "employee", ID: me}, theirs)
	if err != nil || affected {
		t.Fatalf("foreign session must not be revocable: affected=%v err=%v", affected, err)
	}
	var stillAlive int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM auth_sessions WHERE id = $1 AND revoked_at IS NULL`, theirs).Scan(&stillAlive); err != nil || stillAlive != 1 {
		t.Fatalf("foreign session must stay alive: count=%d err=%v", stillAlive, err)
	}

	// جلستي: تنجح مرة ثم idempotent.
	affected, err = s.RevokeSessionByID(ctx, &Principal{Type: "employee", ID: me}, mine)
	if err != nil || !affected {
		t.Fatalf("own session must be revocable: affected=%v err=%v", affected, err)
	}
	affected, err = s.RevokeSessionByID(ctx, &Principal{Type: "employee", ID: me}, mine)
	if err != nil || affected {
		t.Fatalf("second revoke must be a no-op: affected=%v err=%v", affected, err)
	}
}

// TestRevokeOtherSessionsKeepsCurrent — «إنهاء الأخرى» ينهي كل جلسات
// المالك ما عدا الجلسة المارة توكنها، حتى لو كان بعضها في عالم آخر،
// ولا يلمس جلسات غيره.
func TestRevokeOtherSessionsKeepsCurrent(t *testing.T) {
	pool := sessionsTestPool(t)
	ctx := context.Background()
	s := newSessionsService(pool)
	me := "44444444-4444-4444-4444-444444444444"
	other := "55555555-5555-5555-5555-555555555555"
	cleanSessionFixtures(t, ctx, pool, me, other)

	_, currentAccess, _ := seedSession(t, ctx, pool, me, sessionSeed{ua: "current device"})
	seedSession(t, ctx, pool, me, sessionSeed{ua: "old phone"})
	seedSession(t, ctx, pool, me, sessionSeed{ua: "old laptop"})
	seedSession(t, ctx, pool, other, sessionSeed{ua: "another user"})

	ended, err := s.RevokeOtherSessions(ctx, &Principal{Type: "employee", ID: me}, currentAccess)
	if err != nil {
		t.Fatalf("RevokeOtherSessions: %v", err)
	}
	if ended != 2 {
		t.Fatalf("want exactly 2 other sessions ended, got %d", ended)
	}
	var aliveMine, aliveOthers int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM auth_sessions WHERE principal_id = $1 AND revoked_at IS NULL`, me).Scan(&aliveMine); err != nil {
		t.Fatalf("count mine: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM auth_sessions WHERE principal_id = $1 AND revoked_at IS NULL`, other).Scan(&aliveOthers); err != nil {
		t.Fatalf("count others: %v", err)
	}
	if aliveMine != 1 {
		t.Fatalf("only the current session must survive for the owner, got %d", aliveMine)
	}
	if aliveOthers != 1 {
		t.Fatalf("another user's sessions must be untouched, got %d alive", aliveOthers)
	}
}

// TestIsKnownDeviceExcludesLatest — الجهاز المعروف: UA ظهر في جلسة قديمة
// = معروف؛ UA لم يظهر قط = غير معروف؛ وأحدث جلسة (جلسة الدخول نفسها)
// مستثناة من الفحص حتى لا يرسل بريد «جهاز جديد» عن نفسه.
func TestIsKnownDeviceExcludesLatest(t *testing.T) {
	pool := sessionsTestPool(t)
	ctx := context.Background()
	s := newSessionsService(pool)
	me := "66666666-6666-6666-6666-666666666666"
	cleanSessionFixtures(t, ctx, pool, me)

	seedSession(t, ctx, pool, me, sessionSeed{ua: "Recurring Chrome UA"})
	_, _, _ = seedSession(t, ctx, pool, me, sessionSeed{ua: "Brand new UA"}) // أحدث جلسة — تستثنى من الفحص

	p := &Principal{Type: "employee", ID: me}
	known, err := s.IsKnownDevice(ctx, p, "Recurring Chrome UA")
	if err != nil || !known {
		t.Fatalf("recurring UA must be known: known=%v err=%v", known, err)
	}
	known, err = s.IsKnownDevice(ctx, p, "Never seen UA")
	if err != nil || known {
		t.Fatalf("unseen UA must be unknown: known=%v err=%v", known, err)
	}
	// أحدث جلسة نفسها: UA المكرر فيها لا يجعل الجهاز «معروفًا» من نفسها.
	known, err = s.IsKnownDevice(ctx, p, "Brand new UA")
	if err != nil || known {
		t.Fatalf("latest session must be excluded from its own probe: known=%v err=%v", known, err)
	}
}
