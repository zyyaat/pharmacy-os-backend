package handlers

import (
        "os"
        "strings"
        "testing"
)

// T-B5 — source contracts pinning the tenant audit events to their mutation
// transactions: the audit call must exist INSIDE the function and BEFORE its
// tx.Commit, so the event commits (or rolls back) atomically with the change
// it describes. These tests fail if someone moves the commit earlier, drops
// the audit call, or renames the action strings the pharmacy feed filters on.

type tB5Contract struct {
        file       string
        fn         string
        fnMarker   string // the Go signature line identifying the function
        action     string // the audited action string that must appear
        commitSync string // the commit statement the audit must precede
}

func tB5Contracts() []tB5Contract {
        return []tB5Contract{
                {
                        file: "permissions_handler.go", fn: "CreatePharmacyEmployee",
                        fnMarker:   "func (h *Handler) CreatePharmacyEmployee(",
                        action:     `"employee.created"`,
                        commitSync: "tx.Commit(",
                },
                {
                        file: "permissions_handler.go", fn: "UpdateEmployeePermissions",
                        fnMarker:   "func (h *Handler) UpdateEmployeePermissions(",
                        action:     `"permissions.granted"`,
                        commitSync: "tx.Commit(",
                },
                {
                        file: "permissions_handler.go", fn: "UpdateEmployeePermissions",
                        fnMarker:   "func (h *Handler) UpdateEmployeePermissions(",
                        action:     `"permissions.revoked"`,
                        commitSync: "tx.Commit(",
                },
                {
                        file: "permissions_handler.go", fn: "UpdateEmployeePermissions",
                        fnMarker:   "func (h *Handler) UpdateEmployeePermissions(",
                        action:     `"permissions.updated"`,
                        commitSync: "tx.Commit(",
                },
                {
                        file: "pharmacy_settings_handler.go", fn: "UpdatePharmacySettings",
                        fnMarker:   "func (h *Handler) UpdatePharmacySettings(",
                        action:     `"settings.updated"`,
                        commitSync: "auditTx.Commit(",
                },
        }
}

func TestTenantAuditEventsExistBeforeCommit(t *testing.T) {
        sources := map[string]string{}
        for _, c := range tB5Contracts() {
                src, ok := sources[c.file]
                if !ok {
                        raw, err := os.ReadFile(c.file)
                        if err != nil {
                                t.Fatalf("read %s: %v", c.file, err)
                        }
                        src = string(raw)
                        sources[c.file] = src
                }
                start := strings.Index(src, c.fnMarker)
                if start < 0 {
                        t.Fatalf("%s: %s not found", c.file, c.fn)
                }
                end := strings.Index(src[start:], "\nfunc ")
                if end < 0 {
                        // Last function in the file — body runs to EOF.
                        end = len(src) - start
                }
                body := src[start : start+end]

                auditAt := strings.Index(body, "writeAuditLog(")
                if auditAt < 0 {
                        t.Fatalf("%s.%s: no writeAuditLog call — T-B5 event missing", c.fn, c.action)
                }
                if !strings.Contains(body, c.action) {
                        t.Fatalf("%s.%s: audited action %s missing", c.fn, c.action, c.action)
                }
                commitAt := strings.LastIndex(body, c.commitSync)
                if commitAt < 0 {
                        t.Fatalf("%s.%s: no %s in body", c.fn, c.action, c.commitSync)
                }
                if auditAt > commitAt {
                        t.Fatalf("%s.%s: writeAuditLog must run BEFORE %s so the event is atomic with the mutation", c.fn, c.action, c.commitSync)
                }
                // Failure path must be best-effort (auditFailure), never fatal to the
                // business response — house pattern from the barcode feature.
                if !strings.Contains(body, "auditFailure(") {
                        t.Fatalf("%s.%s: audit failures must be logged via auditFailure, not returned to the caller", c.fn, c.action)
                }
        }
}

func TestPermissionTransactionsCarryTenantRLSContext(t *testing.T) {
        // writeAuditLog's contract expects app.current_pharmacy_id on the
        // session. Harmless while RLS isn't forced, load-bearing for batch D —
        // both permission transactions must set it.
        for _, fn := range []string{"CreatePharmacyEmployee", "UpdateEmployeePermissions"} {
                src, err := os.ReadFile("permissions_handler.go")
                if err != nil {
                        t.Fatalf("read permissions_handler.go: %v", err)
                }
                start := strings.Index(string(src), "func (h *Handler) "+fn+"(")
                if start < 0 {
                        t.Fatalf("%s not found", fn)
                }
                end := strings.Index(string(src)[start:], "\nfunc ")
                body := string(src)[start : start+end]
                if !strings.Contains(body, "set_config('app.current_pharmacy_id'") {
                        t.Fatalf("%s: transaction must set app.current_pharmacy_id (tenant RLS context)", fn)
                }
        }
}

func TestPlatformAuditWriterIsTheSharedInsert(t *testing.T) {
        // The INSERT into platform_audit_logs must live ONLY in internal/auditlog
        // — writePlatformAuditLog delegates to it. A second literal INSERT would
        // recreate the schema-drift risk T-B5 closed.
        src, err := os.ReadFile("audit.go")
        if err != nil {
                t.Fatalf("read audit.go: %v", err)
        }
        if strings.Contains(string(src), "INSERT INTO platform_audit_logs") {
                t.Fatal("handlers/audit.go must not carry its own platform_audit_logs INSERT — delegate to internal/auditlog")
        }
        if !strings.Contains(string(src), "auditlog.Platform(") {
                t.Fatal("writePlatformAuditLog must delegate to auditlog.Platform")
        }
}
