// Package auditlog hosts the single canonical writer for the platform audit
// trail (platform_audit_logs, migration 30). Before T-B5 this INSERT lived
// only in the handlers package (writePlatformAuditLog) which takes a pgx.Tx —
// unusable from the auth service whose login paths run on the pool with no
// ambient transaction, and unimportable from there anyway (import cycle:
// handlers → auth). The INSERT now lives here once; both callers delegate:
//
//   - handlers.writePlatformAuditLog keeps its exact signature (tx-scoped:
//     the event commits or rolls back atomically with the mutation it
//     describes) and unwraps its *auth.Principal into an Actor.
//   - the auth service passes its *pgxpool.Pool — a single-row INSERT is
//     atomic by itself, no transaction needed.
//
// Domain note (T-B5): authentication events (login.success / login.failed /
// account.locked) belong here and NOT in the tenant audit_logs table — that
// one is pharmacy_id NOT NULL with tenant RLS, and a failed login may carry
// no pharmacy at all. Tenant mutations (employee.created / permissions.* /
// settings.updated) keep using the tenant writeAuditLog so the pharmacy's
// own audit feed shows them.
//
// ADR-0005 note (api_level 110): the TENANT INSERT moved here for the same
// single-writer reason — the central-libraries service (internal/services)
// writes tenant audit rows inside its own transactions and must not import
// handlers. handlers.writeAuditLog keeps its exact signature and delegates;
// services call Tenant directly.
package auditlog

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Execer is the smallest surface both *pgxpool.Pool and pgx.Tx already
// satisfy. auditlog never needs Query — one INSERT, nothing else.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Querier extends Execer with the row lookup Tenant needs to resolve the
// owning account from the session pharmacy inside the caller's transaction.
// *pgxpool.Pool and pgx.Tx both satisfy it.
type Querier interface {
	Execer
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Actor carries the denormalized identity columns of platform_audit_logs.
// The zero value is a valid anonymous actor: NULLIF turns the empty ID into
// NULL and the empty strings land as-is, so failed-login rows for unknown
// emails keep only the attempted address — exactly what the security feed
// needs for brute-force forensics.
type Actor struct {
	ID          string
	Email       string
	DisplayName string
	Role        string
}

// Platform writes one row into platform_audit_logs. An empty severity
// defaults to 'info' (the previous hardcoded behavior of every existing
// caller). Errors are returned, never swallowed: the handler wrapper decides
// whether the event is fatal to its transaction, the auth paths log it
// best-effort so a broken audit feed can never block a login.
func Platform(
	ctx context.Context,
	db Execer,
	actor Actor,
	action, actionCategory, entityType, entityID, companyID, severity string,
	metadata map[string]any,
	summary string,
) error {
	if metadata == nil {
		metadata = map[string]any{}
	}
	blob, err := json.Marshal(metadata)
	if err != nil {
		blob = []byte(`{}`)
	}
	if severity == "" {
		severity = "info"
	}
	_, err = db.Exec(ctx, `
		INSERT INTO platform_audit_logs (
			actor_id, actor_email, actor_display_name, actor_role,
			action, action_category, entity_type, entity_id, company_id,
			metadata, changes_summary, severity
		) VALUES (
			NULLIF($1, ''), $2, $3, $4,
			$5, $6, $7, NULLIF($8, ''), NULLIF($9, '')::uuid,
			$10::jsonb, $11, $12
		)
	`, actor.ID, actor.Email, actor.DisplayName, actor.Role,
		action, actionCategory, entityType, entityID, companyID,
		string(blob), summary, severity)
	return err
}

// Tenant writes one immutable row into the tenant audit_logs table
// (migration 4) inside the CALLER'S transaction, exactly as
// handlers.writeAuditLog has always done — the event commits (or is
// discarded) atomically with the mutation it describes. The INSERT is moved
// verbatim (ADR-0005 item 3, api_level 110): same account resolution from
// the session pharmacy (audit_logs.account_id is NOT NULL), same
// actor_id = employees-only rule (company-user principals carry no
// employees row, so their identity rides the denormalized columns), same
// 'info' severity.
//
// RLS note: audit_logs requires app.current_pharmacy_id on the session —
// every caller (handlers and services alike) sets it inside its own
// transaction BEFORE calling this.
func Tenant(
	ctx context.Context,
	db Querier,
	actor Actor,
	pharmacyID string,
	action, actionCategory, entityType, entityID string,
	newValues map[string]any,
	summary string,
) error {
	// audit_logs.account_id is NOT NULL and references the tenant account;
	// resolve it from the session pharmacy inside the same transaction.
	var accountID string
	if err := db.QueryRow(ctx,
		`SELECT account_id::text FROM pharmacies WHERE id = $1 AND is_active = true`,
		pharmacyID,
	).Scan(&accountID); err != nil {
		return err
	}

	if newValues == nil {
		newValues = map[string]any{}
	}
	blob, err := json.Marshal(newValues)
	if err != nil {
		blob = []byte(`{}`)
	}

	if _, err := db.Exec(ctx, `
		INSERT INTO audit_logs (
			pharmacy_id, account_id, actor_id, actor_email, actor_display_name,
			actor_role, action, action_category, entity_type, entity_id,
			new_values, changes_summary, severity
		) VALUES (
			$1::uuid, $2::uuid, NULLIF($3, '')::uuid, $4, $5,
			$6, $7, $8, $9, NULLIF($10, '')::uuid,
			$11::jsonb, $12, 'info'
		)
	`, pharmacyID, accountID, actor.ID, actor.Email, actor.DisplayName,
		actor.Role, action, actionCategory, entityType, entityID,
		string(blob), summary); err != nil {
		return err
	}
	return nil
}
