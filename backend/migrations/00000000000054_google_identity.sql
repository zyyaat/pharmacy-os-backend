-- Migration 54 — Google identity linking (تسجيل الدخول بجوجل، api_level 103).
--
-- The Google sign-in flow resolves an account by the verified email inside
-- the ID token and then binds the stable `sub` claim to the row, so future
-- logins match the identity even if the email is reassigned on the Google
-- side. Owners live in company_users; staff (employees) keep resolving by
-- email only — a Google-verified email is proof enough for an existing row
-- (v1 scope), no shadow column there.
--
-- The unique index makes the binding a hard one-to-one: two company rows can
-- never claim the same Google identity.
--
-- Additive-only, replay-safe (IF NOT EXISTS), no destructive statements.
ALTER TABLE company_users
    ADD COLUMN IF NOT EXISTS google_sub TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS ux_company_users_google_sub
    ON company_users (google_sub)
    WHERE google_sub IS NOT NULL;
