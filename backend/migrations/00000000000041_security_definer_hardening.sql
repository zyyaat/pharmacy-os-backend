-- Migration: SECURITY DEFINER hardening + deny-by-default database boundary
-- (Task 153 — defense-in-depth behind the Task 152 permission fixes)
--
-- Task 152 (migrations 40 + the fail-closed gates) closed the APPLICATION
-- layer of the zero-privilege escalation. This migration closes the DATABASE
-- layer: the eight SECURITY DEFINER functions shipped since migrations 1/3/4/5
-- were created with PostgreSQL defaults and never hardened:
--
--   a) EXECUTE granted to PUBLIC — PostgreSQL's CREATE FUNCTION default.
--      On the managed Postgres provider (Supabase) the schema `public` is
--      served through the PostgREST Data API by default: any anon or
--      authenticated bearer of the project key can call any function in
--      `public` unless the project disables the Data API — which is platform
--      dashboard state, NOT repo state, and this deployment has no server
--      access (managed Dockhosting). The repo documents zero Supabase SDK /
--      Data API usage (docs/deployment.md: "Supabase is ONLY the Postgres
--      provider"), so closing function access cannot break any documented
--      path.
--
--   b) No pinned search_path — the bodies resolve unqualified names
--      (permissions, employees, employee_permissions, branches, ...) against
--      whatever the caller's search_path is, so a session that can plant
--      objects (temp tables, or CREATE on a shadowed schema) could hijack
--      privileged writes. Pinning `public, pg_temp` (pg_temp explicitly LAST)
--      removes the whole class.
--
-- Why the grant/revoke pair matters most: grant_permission_to_employee (and
-- its company_user twin) validates same-pharmacy only — it never checks that
-- p_granted_by is the caller or that the caller holds
-- employees.manage_permissions. Anyone who can EXECUTE it can mint
-- pharmacy.admin for any employee ID they can obtain. That is exactly the
-- privilege-escalation class Task 152 closed at the handler layer; these
-- functions are the same hole at the database layer.
--
-- Safety analysis (why this cannot break the running app):
--   1) The backend calls NONE of the eight — verified by repo-wide search:
--      zero call sites in Go/TS/Dart/docs. The backend writes
--      employee_permissions / company_user_permissions / audit_logs /
--      attendance_records directly (e.g. permissions_handler.go:775-794).
--   2) Migrations execute at server boot with the runtime DATABASE_URL
--      (docs/deployment.md), so the runtime role CREATED every object in
--      this schema and therefore OWNS it: owners always retain EXECUTE on
--      their own functions and are not subject to RLS (no FORCE is set).
--      Production already proves the bypass: employee_permissions has had
--      GUC-keyed RLS since migration 3 while the app works.
--   3) Every statement is idempotent and additive: REVOKE/ALTER/ENABLE only,
--      no DROP, no grants, no data changes.
--
-- Effects on lesser roles (anon / authenticated / anything non-owner):
--   - the eight functions become unreachable (EXECUTE revoked from PUBLIC
--     and from the provider's JWT roles where they exist);
--   - every public table without RLS gains deny-by-default RLS (zero
--     policies): reads and writes that were open to future GRANT mistakes
--     now default to nothing;
--   - CREATE on schema public is revoked, so lesser sessions can no longer
--     plant objects that shadow the pinned search_path;
--   - the four public views (owner-privileged by design, they ignore
--     base-table RLS) lose SELECT for PUBLIC/JWT roles.
--
-- The migration self-verifies at the end: if any check fails, the migration
-- (and therefore server boot) fails loudly — fail-closed, like migration 40.

-- ============================================
-- 1) Pin search_path on the eight SECURITY DEFINER functions
--    (pg_temp explicitly LAST so session temp objects cannot shadow
--    the unqualified public-table references in the bodies)
-- ============================================
ALTER FUNCTION get_or_create_default_branch(uuid)
    SET search_path TO public, pg_temp;
ALTER FUNCTION grant_permission_to_employee(uuid, varchar, uuid, text)
    SET search_path TO public, pg_temp;
ALTER FUNCTION revoke_permission_from_employee(uuid, varchar, uuid, text)
    SET search_path TO public, pg_temp;
ALTER FUNCTION log_audit(uuid, uuid, uuid, varchar, varchar, uuid, jsonb, jsonb, inet, text, boolean, varchar)
    SET search_path TO public, pg_temp;
ALTER FUNCTION clock_in_employee(uuid, uuid, inet, jsonb)
    SET search_path TO public, pg_temp;
ALTER FUNCTION clock_out_employee(uuid, text, inet)
    SET search_path TO public, pg_temp;
ALTER FUNCTION grant_permission_to_company_user(uuid, varchar, uuid, text)
    SET search_path TO public, pg_temp;
ALTER FUNCTION revoke_permission_from_company_user(uuid, varchar, uuid, text)
    SET search_path TO public, pg_temp;

-- ============================================
-- 2) Revoke EXECUTE on the eight functions
--    FROM PUBLIC (the PostgreSQL default grant) and from the provider's
--    JWT roles when they exist (anon / authenticated / service_role —
--    Supabase grants EXECUTE on new functions to these beyond PUBLIC via
--    platform default privileges). Role existence is checked so the same
--    migration also runs on plain PostgreSQL (CI / local).
-- ============================================
DO $m41_revoke_execute$
DECLARE
    fn record;
    r  text;
    jwt_roles text[] := ARRAY(
        SELECT rolname FROM pg_roles
        WHERE rolname IN ('anon', 'authenticated', 'service_role')
    );
BEGIN
    FOR fn IN
        SELECT p.oid::regprocedure::text AS sig
        FROM pg_proc p
        JOIN pg_namespace n ON n.oid = p.pronamespace
        WHERE n.nspname = 'public'
          AND p.proname IN (
              'get_or_create_default_branch',
              'grant_permission_to_employee',
              'revoke_permission_from_employee',
              'log_audit',
              'clock_in_employee',
              'clock_out_employee',
              'grant_permission_to_company_user',
              'revoke_permission_from_company_user'
          )
    LOOP
        EXECUTE format('REVOKE EXECUTE ON FUNCTION %s FROM PUBLIC', fn.sig);
        FOREACH r IN ARRAY jwt_roles LOOP
            EXECUTE format('REVOKE EXECUTE ON FUNCTION %s FROM %I', fn.sig, r);
        END LOOP;
    END LOOP;
END
$m41_revoke_execute$;

-- ============================================
-- 3) Schema hygiene: no object-planting for lesser sessions
--    (keeps USAGE — non-breaking — and removes CREATE, the only way a
--    lesser session could shadow the pinned search_path from part 1)
-- ============================================
REVOKE CREATE ON SCHEMA public FROM PUBLIC;

DO $m41_revoke_create$
DECLARE
    r text;
BEGIN
    FOR r IN
        SELECT rolname FROM pg_roles
        WHERE rolname IN ('anon', 'authenticated', 'service_role')
    LOOP
        EXECUTE format('REVOKE CREATE ON SCHEMA public FROM %I', r);
    END LOOP;
END
$m41_revoke_create$;

-- ============================================
-- 4) Deny-by-default RLS on every public table that lacks it
--    (37 tables as of migration 40: payments, plans, platform_*, auth_*,
--    support_*, global_products, permissions, sync_tombstones, ... plus
--    provider-created tables like the river_* job queue on production.
--    Zero policies are added ON PURPOSE: the table owner — the runtime
--    role — is unaffected, while any lesser role now defaults to nothing
--    instead of to "whatever a future GRANT forgets to scope".)
-- ============================================
DO $m41_rls$
DECLARE
    t record;
    n int := 0;
BEGIN
    FOR t IN
        SELECT c.oid::regclass AS tbl
        FROM pg_class c
        JOIN pg_namespace ns ON ns.oid = c.relnamespace
        WHERE ns.nspname = 'public'
          AND c.relkind IN ('r', 'p')   -- plain + partitioned tables
          AND NOT c.relrowsecurity
    LOOP
        EXECUTE format('ALTER TABLE %s ENABLE ROW LEVEL SECURITY', t.tbl);
        n := n + 1;
    END LOOP;
    RAISE NOTICE 'migration 41: RLS (deny-by-default) enabled on % tables', n;
END
$m41_rls$;

-- ============================================
-- 5) Views: revoke SELECT for PUBLIC/JWT roles
--    (views run with the owner's privileges and IGNORE base-table RLS —
--    current_inventory is cross-pharmacy and
--    v_company_user_with_permissions is a permission map, so they must
--    never be exposed to lesser roles by a future platform default)
-- ============================================
DO $m41_views$
DECLARE
    v record;
    r text;
    jwt_roles text[] := ARRAY(
        SELECT rolname FROM pg_roles
        WHERE rolname IN ('anon', 'authenticated', 'service_role')
    );
BEGIN
    FOR v IN
        SELECT c.oid::regclass AS view_name
        FROM pg_class c
        JOIN pg_namespace ns ON ns.oid = c.relnamespace
        WHERE ns.nspname = 'public' AND c.relkind = 'v'
    LOOP
        EXECUTE format('REVOKE SELECT ON %s FROM PUBLIC', v.view_name);
        FOREACH r IN ARRAY jwt_roles LOOP
            EXECUTE format('REVOKE SELECT ON %s FROM %I', v.view_name, r);
        END LOOP;
    END LOOP;
END
$m41_views$;

-- ============================================
-- 6) Self-verification — fail the migration (and boot) loudly on drift
-- ============================================
DO $m41_verify$
DECLARE
    missing_rls int;
    sig text;
    cfg text;
    jwt_roles text[] := ARRAY(
        SELECT rolname FROM pg_roles
        WHERE rolname IN ('anon', 'authenticated', 'service_role')
    );
    r text;
BEGIN
    -- 6a) no public table may remain without RLS
    SELECT count(*) INTO missing_rls
    FROM pg_class c
    JOIN pg_namespace ns ON ns.oid = c.relnamespace
    WHERE ns.nspname = 'public'
      AND c.relkind IN ('r', 'p')
      AND NOT c.relrowsecurity;
    IF missing_rls > 0 THEN
        RAISE EXCEPTION 'migration 41 verification failed: % public table(s) still without RLS', missing_rls;
    END IF;

    -- 6b) none of the eight may be executable by PUBLIC or the JWT roles
    FOR sig IN
        SELECT p.oid::regprocedure::text
        FROM pg_proc p
        JOIN pg_namespace n ON n.oid = p.pronamespace
        WHERE n.nspname = 'public'
          AND p.proname IN (
              'get_or_create_default_branch',
              'grant_permission_to_employee',
              'revoke_permission_from_employee',
              'log_audit',
              'clock_in_employee',
              'clock_out_employee',
              'grant_permission_to_company_user',
              'revoke_permission_from_company_user'
          )
    LOOP
        IF has_function_privilege('public', sig, 'EXECUTE') THEN
            RAISE EXCEPTION 'migration 41 verification failed: PUBLIC can still EXECUTE %', sig;
        END IF;
        FOREACH r IN ARRAY jwt_roles LOOP
            IF has_function_privilege(r, sig, 'EXECUTE') THEN
                RAISE EXCEPTION 'migration 41 verification failed: % can still EXECUTE %', r, sig;
            END IF;
        END LOOP;
    END LOOP;

    -- 6c) all eight must carry the pinned search_path
    FOR cfg IN
        SELECT p.proconfig::text
        FROM pg_proc p
        JOIN pg_namespace n ON n.oid = p.pronamespace
        WHERE n.nspname = 'public'
          AND p.prosecdef
    LOOP
        IF cfg IS NULL OR position('search_path' in cfg) = 0 THEN
            RAISE EXCEPTION 'migration 41 verification failed: a SECURITY DEFINER function lacks a pinned search_path (proconfig=%)', cfg;
        END IF;
    END LOOP;
END
$m41_verify$;
