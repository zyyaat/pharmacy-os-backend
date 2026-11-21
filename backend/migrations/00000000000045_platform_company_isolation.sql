-- Migration 45 — Platform company isolation (api_level 103).
--
-- Root cause (operator report): the company that hosts the platform Super
-- Admin was created by the bootstrap (auth/bootstrap.go) as a fully shaped
-- SaaS tenant — a companies row, an accounts row, a subscriptions row
-- (Task 90 kept the plan gate resolvable) and a pharmacy + branch — with
-- NOTHING distinguishing it from a paying customer. It therefore showed up
-- as "a company among companies" in every platform list and stat, and worse:
-- the suspend action copies subscription suspension onto the company row,
-- so the operator could suspend the platform shell itself by accident.
--
-- This migration:
--   1. adds companies.is_platform (the missing discriminator);
--   2. marks the existing platform company on live databases — the company
--      hosting the singleton super_admin user (migration 10's unique index
--      guarantees at most one match), idempotent on replay;
--   3. freezes it at the DATABASE level with a BEFORE UPDATE/DELETE
--      trigger: status, soft delete, unmarking and hard delete are all
--      refused, even for raw SQL — the application layer (Task 164) returns
--      clean 403s, this trigger is the backstop that makes the invariant
--      hold under any access path.
--
-- Fresh databases are marked at insert time by bootstrap.go; the UPDATE
-- below remains a no-op there. Profile edits (name, contact, branding) stay
-- legal: the trigger only fires when is_platform / status / deleted_at
-- actually change.

ALTER TABLE companies
    ADD COLUMN IF NOT EXISTS is_platform BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN companies.is_platform IS
    'TRUE for the infrastructure company hosting the platform Super Admin — excluded from customer lists and stats, frozen against status/delete mutations';

UPDATE companies c
SET is_platform = true
WHERE NOT c.is_platform
  AND EXISTS (
        SELECT 1 FROM company_users cu
        WHERE cu.company_id = c.id
          AND cu.role = 'super_admin'
          AND cu.deleted_at IS NULL
  );

CREATE OR REPLACE FUNCTION prevent_platform_company_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF OLD.is_platform AND (
        TG_OP = 'DELETE'
        OR NEW.is_platform IS DISTINCT FROM OLD.is_platform
        OR NEW.deleted_at IS NOT NULL
        OR NEW.status IS DISTINCT FROM OLD.status
    ) THEN
        RAISE EXCEPTION
            'the platform company is infrastructure: is_platform, status and deletion are immutable'
            USING ERRCODE = 'check_violation';
    END IF;
    IF TG_OP = 'DELETE' THEN
        -- A BEFORE DELETE trigger must return OLD: NEW is NULL in a DELETE
        -- trigger and a NULL return silently skips the row — every customer
        -- hard delete would no-op without an error.
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS protect_platform_company ON companies;
CREATE TRIGGER protect_platform_company
    BEFORE UPDATE OR DELETE ON companies
    FOR EACH ROW
    EXECUTE FUNCTION prevent_platform_company_mutation();
