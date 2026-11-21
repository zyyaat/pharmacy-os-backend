-- Migration: Permission Tiers + permissions_initialized (Zero-Permission Fail-Open Fix)
--
-- The vulnerability: requirePharmacyPermission treated "zero explicit permission
-- rows" as FULL ACCESS (permissions_handler.go legacy branch), so an employee who
-- was never consciously initialized — or one whose permissions were all revoked —
-- silently kept (or regained) everything.
--
-- The fix model (three states, agreed with the owner):
--   1) UNINITIALIZED  — zero historical rows in employee_permissions (revoked ones
--      included). Fallback = OPERATIONAL-tier keys only.
--   2) INITIALIZED + zero ACTIVE rows (conscious empty save, or everything
--      revoked) — Deny All. Failure closed.
--   3) INITIALIZED + active rows — exact key match, pharmacy.admin still the
--      master bypass.
--
-- permissions_initialized = EXISTS(any historical row, revoked included).
-- The backfill below grants NOTHING and revokes NOTHING: every currently-active
-- permission row keeps working exactly as before (test 1/5 of the fix contract).
-- Employees with zero history become "uninitialized" and drop from full access
-- to operational-only fallback — that narrowing IS the security fix.
--
-- Tier classification (agreed design — pharmacy realm 43 keys):
--   operational   27  — day-to-day work
--   administrative 14 — staff/settings/structure/financial-reports management
--   privileged     2  — pharmacy.admin (master bypass), products.global.manage
--                        (cross-tenant catalog — System Admin only per mig3:70;
--                        the earlier draft had misfiled it into a "platform"
--                        bucket, which broke the 43/17 catalog boundary).
--   platform      17  — mig5 company/company_users/accounts/platform keys. These
--                        are outside the pharmacy-employee realm entirely.
--
-- Judgment calls pinned here for the owner to veto (the 14th administrative seat):
--   - inventory.import  → administrative (bulk spreadsheet migration, category
--     'admin', gated behind the import settings panel; not day-to-day).
--   - inventory.writeoff → operational (routine expired/damaged stock removal,
--     mirrors inventory.adjust; present in the أمين مخزن template).
--   - customers.delete   → operational (routine business-data management; the
--     sensitive surface — balances and collections — is customers.view/payments,
--     which the design already treats as operational).

-- ============================================
-- 1) Tier column on the catalog (single source of truth)
-- ============================================
ALTER TABLE permissions ADD COLUMN IF NOT EXISTS tier VARCHAR(20);

-- Pharmacy realm — operational (27, sorted for diffability)
UPDATE permissions SET tier = 'operational' WHERE key IN (
    'attendance.clock_in_out',
    'attendance.view',
    'branches.view',
    'customers.create',
    'customers.delete',
    'customers.payments',
    'customers.update',
    'customers.view',
    'dashboard.view',
    'employees.view',
    'inventory.adjust',
    'inventory.manage_products',
    'inventory.movements.view',
    'inventory.receive',
    'inventory.transfer',
    'inventory.view',
    'inventory.writeoff',
    'pos.access',
    'products.pharmacy.add',
    'products.pharmacy.pricing',
    'reports.inventory',
    'reports.movements',
    'reports.sales',
    'sales.returns',
    'sales.view',
    'settings.labels',
    'settings.receipts'
);

-- Pharmacy realm — administrative (14, sorted)
UPDATE permissions SET tier = 'administrative' WHERE key IN (
    'attendance.manage',
    'branches.create',
    'branches.delete',
    'branches.update',
    'employees.create',
    'employees.delete',
    'employees.manage_permissions',
    'employees.update',
    'inventory.import',
    'reports.employees',
    'reports.financial',
    'settings.billing',
    'settings.general',
    'settings.integrations'
);

-- Pharmacy realm — privileged (2): the master bypass key and the cross-tenant
-- catalog key. Neither may ever fall inside the uninitialized fallback.
UPDATE permissions SET tier = 'privileged' WHERE key IN (
    'pharmacy.admin',
    'products.global.manage'
);

-- Platform realm (17 — migration 5 catalog: outside pharmacy employees, sorted)
UPDATE permissions SET tier = 'platform' WHERE key IN (
    'accounts.create',
    'accounts.delete',
    'accounts.update',
    'accounts.view',
    'companies.create',
    'companies.delete',
    'companies.manage_subscription',
    'companies.update',
    'companies.view',
    'company_users.create',
    'company_users.delete',
    'company_users.manage_permissions',
    'company_users.update',
    'company_users.view',
    'platform.admin',
    'platform.analytics',
    'platform.audit'
);

-- Fail-closed default: any key the classification missed (or a future insert
-- before its tier is decided) must NOT land in the operational fallback.
UPDATE permissions SET tier = 'administrative' WHERE tier IS NULL;

ALTER TABLE permissions ALTER COLUMN tier SET DEFAULT 'administrative';
ALTER TABLE permissions ALTER COLUMN tier SET NOT NULL;

ALTER TABLE permissions ADD CONSTRAINT permissions_tier_chk
    CHECK (tier IN ('operational', 'administrative', 'privileged', 'platform'));

-- ============================================
-- 2) permissions_initialized on employees
--    No DEFAULT on purpose: every INSERT path must set the flag consciously
--    (creation = initialized; the only INSERT site is CreatePharmacyEmployee).
-- ============================================
ALTER TABLE employees ADD COLUMN IF NOT EXISTS permissions_initialized BOOLEAN;

-- Backfill: initialized ⇐ any historical row (active OR revoked).
-- Employees with zero history stay NULL→false = uninitialized (operational-only
-- fallback). This is the safe-rollout rule from migration 20, narrowed.
UPDATE employees e
SET permissions_initialized = EXISTS (
    SELECT 1 FROM employee_permissions ep WHERE ep.employee_id = e.id
)
WHERE e.permissions_initialized IS NULL;

-- Lock it down: after the backfill no row may stay NULL, and future INSERTs
-- must state the flag explicitly (fail-closed for new code paths).
ALTER TABLE employees ALTER COLUMN permissions_initialized SET NOT NULL;

-- ============================================
-- 3) Full index for history lookups
--    The existing indexes are partial (WHERE is_active), so the EXISTS probe
--    and the revoked-included history scan have no covering index.
-- ============================================
CREATE INDEX IF NOT EXISTS idx_employee_permissions_employee_all
    ON employee_permissions(employee_id);
