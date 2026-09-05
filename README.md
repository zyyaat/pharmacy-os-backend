# Pharmacy OS

Pharmacy OS is a pharmacy-management system split into two independently deployable applications:

## Frontend

The `frontend/` directory contains the existing Next.js 16 / React 19 dashboard. It covers:

- Dashboard metrics
- Inventory and medication management
- Employee management
- Attendance tracking
- Branch management
- Reports and settings

The frontend's existing Vercel-oriented configuration is kept unchanged. Its Supabase and API environment variables are documented in `frontend/README.md`.

## Backend

The `backend/` directory contains the Go 1.25 Gin REST API. It provides the backend services for pharmacies, inventory, employees, branches, attendance, dashboard data, permissions, and multi-tenant company support.

Backend documentation, migrations, tests, and dependency files are in `backend/README.md`.

To run the backend locally:

```bash
cd backend
go mod download
go run ./cmd/server
```

The API listens on port `8080`.