# Backend setup

This repository is a Go 1.25 Gin API. The Replit workflow runs:

```bash
go run ./cmd/server
```

The API listens on port `8080` by default. Health checks:

- `GET /health`
- `GET /api/v1/health`

For the full application, configure these environment variables in Replit Secrets:

- `DATABASE_URL` — PostgreSQL/Supabase connection string (the Replit runtime manages its own `DATABASE_URL`; replace it only when using an external database connection).
- `SUPABASE_URL` — Supabase project URL.
- `SUPABASE_JWT_SECRET` — Supabase JWT signing secret.
- `RIVER_DSN` — optional PostgreSQL queue connection string; defaults to the application database URL in the documented setup.

Set `CORS_ORIGINS` to the exact Vercel frontend origin, without a trailing slash, for example:

```text
https://your-frontend.vercel.app
```

Dependencies are installed with `go mod download`; use `go build ./cmd/server` to verify the server build.