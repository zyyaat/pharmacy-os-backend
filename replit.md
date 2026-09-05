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

- `DATABASE_URL` — PostgreSQL/Supabase connection string (the Replit runtime manages its own `DATABASE_URL`; replace it only when using an external database connection).
- `SUPABASE_URL` — Supabase project URL.
- `SUPABASE_JWT_SECRET` — Supabase JWT signing secret.
- `RIVER_DSN` — optional PostgreSQL queue connection string; defaults to the application database URL in the documented setup.

Set `CORS_ORIGINS` to the exact Vercel frontend origin, without a trailing slash, for example:

```text
https://your-frontend.vercel.app
```

Dependencies are installed with `go mod download`; use `go build ./cmd/server` to verify the server build.

## Frontend

The three frontend apps remain independently deployable on Vercel. Use these Vercel Root Directories:

- `frontend/apps/pharmacy-app`
- `frontend/apps/admin-dashboard`
- `frontend/apps/marketing`

The frontend repository was refreshed from its latest upstream commit. Its app-specific settings were not modified here.