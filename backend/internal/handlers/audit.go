package handlers

import (
        "context"
        "errors"
        "log"

        "github.com/jackc/pgx/v5"

        "github.com/pharmacy-os/backend/internal/auditlog"
        "github.com/pharmacy-os/backend/internal/auth"
)

// writeAuditLog is the canonical entry for the tenant audit_logs table
// (migration 4): it inserts one immutable audit entry inside the caller's
// transaction so the event is committed (or discarded) atomically with the
// mutation it describes. Since ADR-0005 item 3 (api_level 110) the INSERT
// itself lives in internal/auditlog (auditlog.Tenant) — the central
// libraries service writes its own audit rows through the same writer
// inside its own transactions — and this wrapper keeps the historical
// signature so every existing call site stays untouched.
//
// RLS note: audit_logs requires app.current_pharmacy_id to be set on the
// session, which every handler transaction already does before calling this.
func writeAuditLog(
	ctx context.Context,
	tx pgx.Tx,
	principal *auth.Principal,
	action, actionCategory, entityType, entityID string,
	newValues map[string]any,
	summary string,
) error {
	if principal == nil {
		// The historical contract required a principal (the account lookup
		// keyed on principal.PharmacyID); keep failing the same way instead
		// of inventing an anonymous tenant row.
		return errAuditPrincipalRequired
	}

	employeeID, _ := auth.ActorIDs(principal)
	// actor_id references employees(id) only — company-user principals have
	// no employees row, so actor_id stays NULL and the denormalized identity
	// columns (actor_email/actor_display_name) carry the who.
	actor := auditlog.Actor{
		ID:          employeeID,
		Email:       principal.Email,
		DisplayName: principal.DisplayName,
		Role:        principal.Role,
	}
	return auditlog.Tenant(ctx, tx, actor, principal.PharmacyID,
		action, actionCategory, entityType, entityID, newValues, summary)
}

// errAuditPrincipalRequired keeps the nil-principal failure explicit now
// that the INSERT moved to internal/auditlog (ADR-0005 item 3, api_level
// 110): the libraries service writes its own tenant audit rows through the
// same canonical writer.
var errAuditPrincipalRequired = errors.New("audit write requires an authenticated principal")

// auditFailure reports an audit write failure without aborting the business
// response path: the mutation already succeeded or the transaction will roll
// back on its own error handling. Losing an audit row is logged loudly so
// operations notices, but a barcode must not become unprintable because the
// audit insert hiccupped.
func auditFailure(scope string, err error) {
        log.Printf("[AUDIT] %s write failed: %v", scope, err)
}

// writePlatformAuditLog records a platform (global SaaS operator) action in
// platform_audit_logs (migration 30). The tenant writeAuditLog can never
// host these events: audit_logs.pharmacy_id is NOT NULL (tenant RLS) and a
// platform principal carries no pharmacy — every billing mutation used to
// fail that lookup silently under `_ =`, so NO platform audit row was ever
// written. companyID links the event to the affected account so the new
// per-company logs page can show one account's history; plan-scope events
// (plan.create/update) pass an empty companyID and land only in the global
// feed.
//
// Since T-B5 the INSERT itself lives in internal/auditlog (the auth service
// writes login audit rows through the same writer on the pool); this wrapper
// keeps its historical signature so the existing call sites stay untouched.
func writePlatformAuditLog(
        ctx context.Context,
        tx pgx.Tx,
        principal *auth.Principal,
        action, actionCategory, entityType, entityID, companyID string,
        metadata map[string]any,
        summary string,
) error {
        var actor auditlog.Actor
        if principal != nil {
                actor = auditlog.Actor{
                        ID:          principal.ID,
                        Email:       principal.Email,
                        DisplayName: principal.DisplayName,
                        Role:        principal.Role,
                }
        }
        return auditlog.Platform(ctx, tx, actor,
                action, actionCategory, entityType, entityID, companyID, "",
                metadata, summary)
}
