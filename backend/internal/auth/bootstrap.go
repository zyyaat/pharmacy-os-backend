package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// BootstrapSuperAdmin creates the first platform administrator in one transaction.
// It is intentionally called only from a production startup path when the
// bootstrap password secret is present. Existing super admins are never changed.
func (s *Service) BootstrapSuperAdmin(
	ctx context.Context,
	email string,
	password string,
	firstName string,
	lastName string,
	companyName string,
) error {
	email = normalizeEmail(email)
	firstName = strings.TrimSpace(firstName)
	lastName = strings.TrimSpace(lastName)
	companyName = strings.TrimSpace(companyName)
	if email == "" || password == "" || firstName == "" || lastName == "" || companyName == "" {
		return errors.New("bootstrap super admin configuration is incomplete")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash bootstrap password: %w", err)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin super admin bootstrap: %w", err)
	}
	defer tx.Rollback(ctx)

	// Prevent two autoscale instances from bootstrapping simultaneously.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('pharmacy_os_bootstrap_super_admin'))`); err != nil {
		return fmt.Errorf("lock super admin bootstrap: %w", err)
	}

	var hasSuperAdmin bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM company_users
			WHERE role = 'super_admin' AND deleted_at IS NULL
		)
	`).Scan(&hasSuperAdmin); err != nil {
		return fmt.Errorf("check existing super admin: %w", err)
	}
	if hasSuperAdmin {
		return nil
	}

	var companyID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO companies (name, email, status, plan)
		VALUES ($1, $2, 'active', 'enterprise')
		RETURNING id::text
	`, companyName, email).Scan(&companyID); err != nil {
		return fmt.Errorf("create bootstrap company: %w", err)
	}

	var accountID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO accounts (
			company_id, company_name, contact_email, status,
			subscription_plan, default_currency, timezone, locale
		) VALUES ($1, $2, $3, 'active', 'enterprise', 'EGP', 'Africa/Cairo', 'ar-EG')
		RETURNING id::text
	`, companyID, companyName, email).Scan(&accountID); err != nil {
		return fmt.Errorf("create bootstrap account: %w", err)
	}

	var pharmacyID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO pharmacies (
			account_id, name, email, country, is_main_branch, currency
		) VALUES ($1, $2, $3, 'EG', true, 'EGP')
		RETURNING id::text
	`, accountID, companyName, email).Scan(&pharmacyID); err != nil {
		return fmt.Errorf("create bootstrap pharmacy: %w", err)
	}

	var branchID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO branches (pharmacy_id, name, code, country)
		VALUES ($1, 'الفرع الرئيسي', 'MAIN', 'EG')
		RETURNING id::text
	`, pharmacyID).Scan(&branchID); err != nil {
		return fmt.Errorf("create bootstrap branch: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE pharmacies SET default_branch_id = $2 WHERE id = $1
	`, pharmacyID, branchID); err != nil {
		return fmt.Errorf("set bootstrap default branch: %w", err)
	}

	var userID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO company_users (
			company_id, email, password_hash, first_name, last_name,
			role, email_verified_at
		) VALUES ($1, $2, $3, $4, $5, 'super_admin', NOW())
		RETURNING id::text
	`, companyID, email, string(hash), firstName, lastName).Scan(&userID); err != nil {
		return fmt.Errorf("create bootstrap super admin: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO company_user_permissions (company_user_id, permission_id, granted_by, notes)
		SELECT $1, p.id, $1, 'Initial platform administrator permissions'
		FROM permissions p
		ON CONFLICT (company_user_id, permission_id)
		WHERE revoked_at IS NULL
		DO UPDATE SET revoked_at = NULL, revocation_reason = NULL
	`, userID); err != nil {
		return fmt.Errorf("grant bootstrap permissions: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit super admin bootstrap: %w", err)
	}
	return nil
}
