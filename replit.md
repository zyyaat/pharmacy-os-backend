# Pharmacy OS monorepo

This repository contains the Pharmacy OS frontend monorepo and backend:

- `frontend/apps/pharmacy-app/` — the main Next.js pharmacy application.
- `frontend/apps/admin-dashboard/` — the Next.js admin dashboard.
- `frontend/apps/marketing/` — the Next.js public marketing site.
- `backend/` — the Go 1.25 Gin API and its migrations.

## Backend setup

The Replit workflow runs:

```bash
cd backend && go run ./cmd/server
```

Production publishing builds and runs a non-ignored binary:

```bash
cd backend && go build -o ../pharmacy-api ./cmd/server
./pharmacy-api
```

The backend API listens on port `8080` by default. Health checks:

- `GET /health`
- `GET /api/v1/health`

For the backend, configure these environment variables in Replit Secrets:

- `DATABASE_URL` — PostgreSQL connection string. Supabase is used only as the database provider.
- `RIVER_DSN` — optional PostgreSQL queue connection string; defaults to the application database URL in the documented setup.
- `BREVO_API_KEY` — Brevo API key for email verification and password reset.
- `MAIL_FROM_EMAIL` — verified Brevo sender email.
- `MAIL_FROM_NAME` — optional sender name.
- `PUBLIC_APP_URL` — public Vercel URL used in email links.
- `AUTH_COOKIE_SECURE` — set to `true` in production.

Set `CORS_ORIGINS` to the exact Vercel frontend origins, without trailing slashes, for example:

```text
https://your-frontend.vercel.app
```

Apply `backend/migrations/00000000000006_go_auth.sql` before using the auth endpoints.
The Go API owns authentication: passwords, email tokens, opaque sessions, cookie
rotation, revocation, and CSRF validation. The frontend must use
`credentials: include`; it must not store auth tokens in localStorage.

## Frontend

The three frontend apps remain independently deployable on Vercel. Use these Vercel Root Directories:

- `frontend/apps/pharmacy-app`
- `frontend/apps/admin-dashboard`
- `frontend/apps/marketing`

The frontend repository was refreshed from its latest upstream commit. Its app-specific settings were not modified here.