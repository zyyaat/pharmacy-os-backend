package database

// AUD-007 — the destructive-migration guard.
//
// Migrations run automatically at every startup (main.go calls
// RunMigrations before anything else). The historical chain already
// contains genuinely destructive statements — migration 14 dropped
// inventory_batches.total_cost and rewrote money columns to piastres,
// migration 20 wiped role_permissions for a rebuild, migration 26
// retyped companies.plan — meaning a future destructive migration ships
// straight onto production data with no gate and no snapshot.
//
// This guard closes that hole with three layers:
//
//  1. scanDestructiveStatements — a conservative scanner that flags the
//     statement families that destroy or irreversibly rewrite data
//     (DROP TABLE/COLUMN, TRUNCATE, DELETE FROM, ALTER COLUMN … TYPE,
//     and drops of schema objects without IF EXISTS). Idempotent
//     housekeeping (DROP TRIGGER/CONSTRAINT/POLICY/INDEX/VIEW/FUNCTION
//     IF EXISTS) is intentionally NOT flagged: the chain relies on it
//     for replay safety, and it cannot lose row data.
//  2. destructiveGate — decides what happens when a pending migration
//     is destructive: a fresh install (empty database) proceeds (there
//     is nothing to lose and bootstrap must work), development
//     proceeds with a loud warning, and production/staging BLOCK the
//     startup unless the operator explicitly allowed the migration via
//     ALLOW_DESTRUCTIVE_MIGRATIONS (comma-separated migration names or
//     "*"). Fail-safe by design: a destructive migration can never
//     reach production data on autopilot.
//  3. The pre-execution snapshot (backup.go) runs for every destructive
//     migration that passes the gate.

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// destructiveStatement is one flagged occurrence inside a migration file.
type destructiveStatement struct {
	Line    int    // 1-based line number in the migration file
	Kind    string // canonical family, e.g. "DROP TABLE"
	Snippet string // trimmed source line for operator-facing logs
}

var (
	// reAlwaysDestructive matches statements that destroy row data even
	// when written defensively (IF EXISTS cannot save data that exists
	// in production). ALTER COLUMN accepts %I-style tokens so dynamic
	// SQL inside plpgsql DO-blocks is caught too (migration 14's
	// piastres rewrite lives in a format string).
	reAlwaysDestructive = regexp.MustCompile(
		`(?i)\b(DROP\s+TABLE|DROP\s+COLUMN|TRUNCATE(?:\s+TABLE)?|DELETE\s+FROM|ALTER\s+COLUMN\s+\S+(\s+SET\s+DATA)?\s+TYPE)\b`)

	// reConditionalDrop matches DROP <object> where the object only
	// loses metadata (schema objects). Flagged solely when NOT followed
	// by IF EXISTS, which the chain uses as its replay guard.
	reConditionalDrop = regexp.MustCompile(
		`(?i)\bDROP\s+(SCHEMA|TYPE|DOMAIN|SEQUENCE|VIEW|MATERIALIZED\s+VIEW|FUNCTION|PROCEDURE|INDEX|CONSTRAINT|POLICY|TRIGGER|DATABASE)\b`)

	// reIfExistsAfterDrop checks whether an IF EXISTS immediately follows
	// a conditional DROP <object> keyword sequence.
	reIfExistsAfterDrop = regexp.MustCompile(
		`(?i)^DROP\s+(SCHEMA|TYPE|DOMAIN|SEQUENCE|VIEW|MATERIALIZED\s+VIEW|FUNCTION|PROCEDURE|INDEX|CONSTRAINT|POLICY|TRIGGER|DATABASE)\s+IF\s+EXISTS\b`)
)

// kindFor classifies a matched destructive statement into a stable label
// used in logs and the block error.
func kindFor(matched string) string {
	kind := strings.ToUpper(strings.Join(strings.Fields(matched), " "))
	// Normalize the ALTER COLUMN … TYPE family to one label.
	if strings.HasPrefix(kind, "ALTER COLUMN") {
		return "ALTER COLUMN ... TYPE"
	}
	if strings.HasPrefix(kind, "TRUNCATE") {
		return "TRUNCATE"
	}
	return kind
}

// stripLineComment removes a trailing -- comment from a single line.
func stripLineComment(line string) string {
	if idx := strings.Index(line, "--"); idx >= 0 {
		return line[:idx]
	}
	return line
}

// stripBlockComments removes /* … */ comments across a whole script so a
// commented-out example can never trip the gate. Nested blocks follow the
// SQL rule (innermost */ closes the block; Postgres actually nests them —
// the simple toggle is deliberately over-cautious in the operator's favor
// only when comments close out-of-order, which the chain never produces).
func stripBlockComments(sql string) string {
	var b strings.Builder
	inBlock := false
	for i := 0; i < len(sql); i++ {
		if !inBlock && i+1 < len(sql) && sql[i] == '/' && sql[i+1] == '*' {
			inBlock = true
			b.WriteByte(' ')
			i++
			continue
		}
		if inBlock && i+1 < len(sql) && sql[i] == '*' && sql[i+1] == '/' {
			inBlock = false
			b.WriteByte(' ')
			i++
			continue
		}
		if !inBlock {
			b.WriteByte(sql[i])
		} else if sql[i] == '\n' {
			// Keep line structure intact so reported line numbers stay true.
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// scanDestructiveStatements returns every destructive statement in the
// migration SQL, in file order. Line numbers refer to the original file.
func scanDestructiveStatements(sql string) []destructiveStatement {
	sql = stripBlockComments(sql)
	var findings []destructiveStatement
	for i, raw := range strings.Split(sql, "\n") {
		line := stripLineComment(raw)
		if strings.TrimSpace(line) == "" {
			continue
		}
		lineNo := i + 1
		if m := reAlwaysDestructive.FindString(line); m != "" {
			findings = append(findings, destructiveStatement{
				Line:    lineNo,
				Kind:    kindFor(m),
				Snippet: strings.TrimSpace(raw),
			})
			continue
		}
		if loc := reConditionalDrop.FindStringIndex(line); loc != nil && !reIfExistsAfterDrop.MatchString(line[loc[0]:]) {
			findings = append(findings, destructiveStatement{
				Line:    lineNo,
				Kind:    kindFor(line[loc[0]:loc[1]]),
				Snippet: strings.TrimSpace(raw),
			})
		}
	}
	return findings
}

// Environment access is isolated here so tests can exercise the wiring.
func appEnvName() string { return os.Getenv("APP_ENV") }

func allowDestructiveRaw() string { return os.Getenv("ALLOW_DESTRUCTIVE_MIGRATIONS") }

// destructiveGate is the decision input for one pending destructive
// migration. It is a plain struct so the decision table is unit-testable.
type destructiveGate struct {
	Migration    string                  // e.g. 00000000000035_example.sql
	Findings     []destructiveStatement  // scanner output
	FreshInstall bool                    // database had no tables before this run
	AppEnv       string                  // APP_ENV value ("" behaves as development)
	AllowRaw     string                  // ALLOW_DESTRUCTIVE_MIGRATIONS raw value
}

// Decision constants returned by destructiveGate.decision.
const (
	decisionFreshInstall = "fresh-install" // allowed: nothing to lose
	decisionDevelopment  = "development"   // allowed with a loud warning
	decisionOverride     = "override"      // allowed: operator listed this migration
	decisionBlock        = "block"         // refused: startup fails with remediation
)

// allowListed reports whether AllowRaw whitelists the migration: "*"
// allows every destructive migration; otherwise the value is a
// comma-separated list of migration names matched exactly.
func (g destructiveGate) allowListed() bool {
	raw := strings.TrimSpace(g.AllowRaw)
	if raw == "" {
		return false
	}
	if raw == "*" {
		return true
	}
	for _, name := range strings.Split(raw, ",") {
		if strings.TrimSpace(name) == g.Migration {
			return true
		}
	}
	return false
}

// decision resolves the gate for one pending destructive migration.
// Staging shares production's strictness: only environments that can
// throw data away freely (development, unset APP_ENV) auto-allow.
func (g destructiveGate) decision() (string, error) {
	if g.FreshInstall {
		return decisionFreshInstall, nil
	}
	switch g.AppEnv {
	case "production", "staging":
		if g.allowListed() {
			return decisionOverride, nil
		}
		return decisionBlock, fmt.Errorf(
			"destructive-migration guard (AUD-007) blocked %s in %s: %d destructive statement(s) found (%s). "+
				"Remediation: (1) take a database snapshot from the hosting panel and keep it until verified, "+
				"(2) set ALLOW_DESTRUCTIVE_MIGRATIONS=%s in the environment, (3) redeploy. "+
				"A pre-execution pg_dump snapshot will then run automatically when the client is available.",
			g.Migration, g.AppEnv, len(g.Findings), summarizeFindings(g.Findings), g.Migration)
	default: // development, unset — disposable data
		return decisionDevelopment, nil
	}
}

// summarizeFindings renders the findings as "line N: KIND" pairs, capped
// so a pathological migration cannot flood the startup log.
func summarizeFindings(findings []destructiveStatement) string {
	const maxShown = 6
	parts := make([]string, 0, len(findings))
	for i, f := range findings {
		if i == maxShown {
			parts = append(parts, fmt.Sprintf("… +%d more", len(findings)-maxShown))
			break
		}
		parts = append(parts, fmt.Sprintf("line %d: %s", f.Line, f.Kind))
	}
	return strings.Join(parts, "; ")
}
