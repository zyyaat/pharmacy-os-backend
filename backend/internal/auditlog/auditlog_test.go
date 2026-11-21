package auditlog

import (
        "context"
        "encoding/json"
        "errors"
        "strings"
        "testing"

        "github.com/jackc/pgx/v5/pgconn"
)

// fakeExecer captures the last Exec call so tests can assert exactly what
// would reach platform_audit_logs — no database involved.
type fakeExecer struct {
        sql      string
        args     []any
        err      error
        calls    int
        panicked bool
}

func (f *fakeExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
        f.calls++
        f.sql = sql
        f.args = args
        if f.err != nil {
                return pgconn.CommandTag{}, f.err
        }
        return pgconn.NewCommandTag("INSERT 0 1"), nil
}

const insertSQLMarker = "INSERT INTO platform_audit_logs"

func TestPlatformInsertsIntoPlatformAuditLogs(t *testing.T) {
        db := &fakeExecer{}
        err := Platform(context.Background(), db, Actor{ID: "p-1", Email: "a@b.c", DisplayName: "أدمن", Role: "super_admin"},
                "plan.create", "billing", "plan", "plan-9", "11111111-1111-1111-1111-111111111111", "",
                map[string]any{"price": 100}, "إنشاء خطة")
        if err != nil {
                t.Fatalf("Platform returned error: %v", err)
        }
        if db.calls != 1 {
                t.Fatalf("expected exactly 1 Exec, got %d", db.calls)
        }
        if !strings.Contains(db.sql, insertSQLMarker) {
                t.Fatalf("expected INSERT into platform_audit_logs, got: %s", db.sql)
        }
        // Column order of the VALUES clause: actor_id, email, display_name, role,
        // action, category, entity_type, entity_id, company_id, metadata, summary, severity.
        want := []string{"p-1", "a@b.c", "أدمن", "super_admin", "plan.create", "billing", "plan", "plan-9",
                "11111111-1111-1111-1111-111111111111", `{"price":100}`, "إنشاء خطة", "info"}
        for i, w := range want {
                got, ok := db.args[i].(string)
                if !ok || got != w {
                        t.Fatalf("args[%d] = %#v, want %q", i, db.args[i], w)
                }
        }
        if len(db.args) != 12 {
                t.Fatalf("expected 12 bind args, got %d", len(db.args))
        }
}

func TestPlatformEmptySeverityDefaultsToInfo(t *testing.T) {
        db := &fakeExecer{}
        if err := Platform(context.Background(), db, Actor{}, "login.success", "authentication", "account", "x", "", "", nil, "s"); err != nil {
                t.Fatalf("Platform returned error: %v", err)
        }
        if got := db.args[11].(string); got != "info" {
                t.Fatalf("empty severity must default to info, got %q", got)
        }
}

func TestPlatformExplicitSeverityPassesThrough(t *testing.T) {
        db := &fakeExecer{}
        if err := Platform(context.Background(), db, Actor{}, "account.locked", "authentication", "account", "x", "", "warning", nil, "s"); err != nil {
                t.Fatalf("Platform returned error: %v", err)
        }
        if got := db.args[11].(string); got != "warning" {
                t.Fatalf("explicit severity must pass through, got %q", got)
        }
}

func TestPlatformNilMetadataBecomesEmptyObject(t *testing.T) {
        db := &fakeExecer{}
        if err := Platform(context.Background(), db, Actor{}, "login.failed", "authentication", "account", "", "", "", nil, "s"); err != nil {
                t.Fatalf("Platform returned error: %v", err)
        }
        blob, ok := db.args[9].(string)
        if !ok {
                t.Fatalf("metadata arg is not a string: %#v", db.args[9])
        }
        var parsed map[string]any
        if err := json.Unmarshal([]byte(blob), &parsed); err != nil {
                t.Fatalf("metadata blob is not valid JSON: %v (blob=%q)", err, blob)
        }
        if len(parsed) != 0 {
                t.Fatalf("nil metadata must serialize to an empty object, got %v", parsed)
        }
}

func TestPlatformAnonymousActorKeepsEmptyStringsForNullif(t *testing.T) {
        // The zero Actor is the failed-login-for-unknown-email case: the writer
        // must hand empty strings to the NULLIF casts, not skip the columns.
        db := &fakeExecer{}
        if err := Platform(context.Background(), db, Actor{}, "login.failed", "authentication", "account", "", "", "", map[string]any{"reason": "invalid_credentials"}, "s"); err != nil {
                t.Fatalf("Platform returned error: %v", err)
        }
        // Identity columns are indexes 0-3 (actor_id/email/display/role) plus
        // entity_id/company_id at 7/8; action/category/entity_type (4-6),
        // metadata (9), summary (10) and severity (11) stay populated.
        for _, i := range []int{0, 1, 2, 3, 7, 8} {
                if got, _ := db.args[i].(string); got != "" {
                        t.Fatalf("anonymous actor: args[%d] must be empty string, got %q", i, got)
                }
        }
}

func TestPlatformPropagatesExecError(t *testing.T) {
        // House contract: the writer returns errors, never swallows them — the
        // caller decides fatality (tx-scoped handlers) or best-effort (auth).
        db := &fakeExecer{err: errors.New("audit insert failed")}
        if err := Platform(context.Background(), db, Actor{}, "login.failed", "authentication", "account", "", "", "", nil, "s"); err == nil {
                t.Fatal("Platform must propagate the Exec error")
        }
}
