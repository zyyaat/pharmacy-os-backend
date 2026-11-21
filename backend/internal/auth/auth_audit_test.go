package auth

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// ============================================================================
// T-B5 — source contract (same pattern as the payment flip guard): every
// terminal path of Service.Login must write its audit row. A future error
// return added without an audit call fails these tests, so the guarantee
// cannot silently rot.
// ============================================================================

func loginBody(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	start := strings.Index(string(src), "func (s *Service) Login(")
	if start < 0 {
		t.Fatal("Login function not found in service.go")
	}
	end := strings.Index(string(src)[start:], "\nfunc ")
	if end < 0 {
		t.Fatal("cannot find the end of Login in service.go")
	}
	return string(src)[start : start+end]
}

func TestLoginAuditsEveryTerminalErrorPath(t *testing.T) {
	body := loginBody(t)
	// Login has exactly 9 audited terminal paths:
	// 8 × auditLoginFailure (unknown email / realm mismatch / pre-existing
	// lock / inactive / super-admin rate-limited / super-admin invalid
	// password / wrong password / unverified email) + 1 × auditAccountLocked
	// (the 5th-attempt transition) + 1 × auditLoginSuccess.
	wantFailures := 8
	gotFailures := strings.Count(body, "s.auditLoginFailure(")
	if gotFailures != wantFailures {
		t.Fatalf("auditLoginFailure calls in Login = %d, want %d — an error path is missing its audit row", gotFailures, wantFailures)
	}
	if got := strings.Count(body, "s.auditAccountLocked("); got != 1 {
		t.Fatalf("auditAccountLocked calls in Login = %d, want 1 (the lock transition)", got)
	}
	if got := strings.Count(body, "s.auditLoginSuccess("); got != 1 {
		t.Fatalf("auditLoginSuccess calls in Login = %d, want 1", got)
	}
	// Every "return nil, nil, Err*" inside Login must sit in a block whose
	// preceding 300 characters contain an audit call — catches new error
	// returns added without one.
	for _, sentinel := range []string{
		"ErrInvalidCredentials", "ErrAccountLocked", "ErrLoginRateLimited",
		"ErrAccountInactive", "ErrEmailNotVerified",
	} {
		idx := 0
		for {
			at := strings.Index(body[idx:], "return nil, nil, "+sentinel)
			if at < 0 {
				break
			}
			at += idx
			window := body[maxInt(0, at-300):at]
			if !strings.Contains(window, "s.auditLoginFailure(") &&
				!strings.Contains(window, "s.auditAccountLocked(") {
				t.Fatalf("a `return nil, nil, %s` path lacks an audit call before it — T-B5 contract broken", sentinel)
			}
			idx = at + 1
		}
	}
}

func TestLoginAuditReasonsMatchSentinels(t *testing.T) {
	body := loginBody(t)
	pairs := []struct{ sentinel, reason string }{
		{"ErrInvalidCredentials", "authReasonInvalidCredentials"},
		{"ErrAccountLocked", "authReasonAccountLocked"},
		{"ErrLoginRateLimited", "authReasonRateLimited"},
		{"ErrAccountInactive", "authReasonAccountInactive"},
		{"ErrEmailNotVerified", "authReasonEmailNotVerified"},
	}
	for _, p := range pairs {
		idx := 0
		found := false
		for {
			at := strings.Index(body[idx:], "return nil, nil, "+p.sentinel)
			if at < 0 {
				break
			}
			at += idx
			window := body[maxInt(0, at-300):at]
			if strings.Contains(window, "s.auditLoginFailure(") && strings.Contains(window, p.reason) {
				found = true
				break
			}
			idx = at + 1
		}
		if !found {
			t.Fatalf("a `return nil, nil, %s` path lacks an audit call with %s — the security feed would lose this reason", p.sentinel, p.reason)
		}
	}
}

func TestLoginSuccessAuditedOnlyAfterSessionIssued(t *testing.T) {
	body := loginBody(t)
	successAt := strings.Index(body, "s.auditLoginSuccess(")
	sessionAt := strings.Index(body, "s.createSession(")
	if successAt < 0 || sessionAt < 0 {
		t.Fatal("Login must contain both createSession and auditLoginSuccess")
	}
	if successAt < sessionAt {
		t.Fatal("login.success must be audited AFTER createSession — it means tokens were issued, not merely that credentials checked out")
	}
	if at := strings.Index(body[successAt:], "return principal, tokens, nil"); at < 0 {
		t.Fatal("successful return path missing from Login")
	}
}

// ============================================================================
// Behavioral tests — the exact row shape that reaches platform_audit_logs,
// driven through the real helpers with a capturing fake (auditDB seam).
// ============================================================================

type capturingExecer struct {
	sql  string
	args []any
}

func (c *capturingExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c.sql = sql
	c.args = args
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func newAuditedService() (*Service, *capturingExecer) {
	db := &capturingExecer{}
	return &Service{auditDB: db}, db
}

func TestAuditLoginSuccessBuildsPlatformRow(t *testing.T) {
	s, db := newAuditedService()
	p := &Principal{
		ID: "cu-1", Type: CompanyUserPrincipal, Email: "owner@pharma.eg",
		DisplayName: "المالك", Role: "company_admin", CompanyID: "co-9",
	}
	s.auditLoginSuccess(context.Background(), p, PharmacyRealm,
		RequestMeta{IPAddress: "10.0.0.1", UserAgent: "UA/1"})

	if !strings.Contains(db.sql, "INSERT INTO platform_audit_logs") {
		t.Fatalf("expected platform_audit_logs insert, got %s", db.sql)
	}
	if got := db.args[4].(string); got != "login.success" {
		t.Fatalf("action = %q", got)
	}
	if got := db.args[5].(string); got != "authentication" {
		t.Fatalf("category = %q", got)
	}
	// company_id set for company users → per-company logs page shows sign-ins.
	if got := db.args[8].(string); got != "co-9" {
		t.Fatalf("company_id = %q, want co-9", got)
	}
	blob := db.args[9].(string)
	for _, needle := range []string{`"realm":"pharmacy"`, `"ip_address":"10.0.0.1"`, `"principal_type":"company_user"`, `"company_id":"co-9"`} {
		if !strings.Contains(blob, needle) {
			t.Fatalf("metadata missing %s: %s", needle, blob)
		}
	}
}

func TestAuditLoginFailureUnknownEmailKeepsAttemptedAddressOnly(t *testing.T) {
	s, db := newAuditedService()
	s.auditLoginFailure(context.Background(), nil, "ghost@unknown.io", PharmacyRealm,
		RequestMeta{IPAddress: "203.0.113.5"}, authReasonInvalidCredentials)

	if got := db.args[0].(string); got != "" {
		t.Fatalf("actor_id for anonymous attempt = %q, want empty (NULLIF → NULL)", got)
	}
	if got := db.args[1].(string); got != "ghost@unknown.io" {
		t.Fatalf("attempted email = %q", got)
	}
	if got := db.args[11].(string); got != "info" {
		t.Fatalf("unknown-email failure severity = %q, want info (scanner noise)", got)
	}
	if got := db.args[7].(string); got != "" {
		t.Fatalf("entity_id for anonymous attempt = %q, want empty", got)
	}
}

func TestAuditLoginFailureKnownAccountIsWarningWithReason(t *testing.T) {
	s, db := newAuditedService()
	p := &Principal{ID: "emp-1", Type: EmployeePrincipal, Email: "staff@pharma.eg", Role: "pharmacist", PharmacyID: "ph-9"}
	s.auditLoginFailure(context.Background(), p, p.Email, PharmacyRealm,
		RequestMeta{}, authReasonAccountInactive)

	if got := db.args[11].(string); got != "warning" {
		t.Fatalf("known-account failure severity = %q, want warning (targeted attempt)", got)
	}
	if !strings.Contains(db.args[9].(string), `"reason":"account_inactive"`) {
		t.Fatalf("metadata missing reason: %s", db.args[9].(string))
	}
	// Employees have no company: company_id stays empty, pharmacy_id is in metadata.
	if got := db.args[8].(string); got != "" {
		t.Fatalf("company_id for employee = %q, want empty", got)
	}
	if !strings.Contains(db.args[9].(string), `"pharmacy_id":"ph-9"`) {
		t.Fatalf("metadata missing pharmacy_id: %s", db.args[9].(string))
	}
}

func TestAuditAccountLockedRowShape(t *testing.T) {
	s, db := newAuditedService()
	p := &Principal{ID: "emp-2", Type: EmployeePrincipal, Email: "cashier@pharma.eg", Role: "cashier"}
	s.auditAccountLocked(context.Background(), p, PharmacyRealm, RequestMeta{IPAddress: "198.51.100.7"})

	if got := db.args[4].(string); got != "account.locked" {
		t.Fatalf("action = %q", got)
	}
	if got := db.args[11].(string); got != "warning" {
		t.Fatalf("account.locked severity = %q, want warning", got)
	}
	if !strings.Contains(db.args[9].(string), `"reason":"invalid_credentials"`) {
		t.Fatalf("account.locked metadata must carry the cause: %s", db.args[9].(string))
	}
}

func TestAuditAuthEventNilStoresAreSafe(t *testing.T) {
	// &Service{} construction (existing unit tests) must never panic through
	// the audit helpers — best-effort means best-effort.
	s := &Service{}
	s.auditLoginSuccess(context.Background(), &Principal{ID: "x"}, PlatformRealm, RequestMeta{})
	s.auditLoginFailure(context.Background(), nil, "a@b.c", PlatformRealm, RequestMeta{}, authReasonInvalidCredentials)
	s.auditAccountLocked(context.Background(), &Principal{ID: "x"}, PlatformRealm, RequestMeta{})
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
