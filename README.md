# Orderly Backend

Go API for multi-tenant food SaaS (subdomain tenancy).

## Stack

- Go + chi
- PostgreSQL 16
- goose + sqlc
- JWT auth (separate admin vs tenant login)

## Quick start

From **repo root** (`orderly/`) — same pattern as Avanor `up` / `down`:

```bash
cp orderly-backend/.env.example orderly-backend/.env
make up
make run
```

From this directory:

```bash
cp .env.example .env
make up
make run
```

**Fresh DB (no tenants):** `make down-v && make up && make run`, or `make fresh-run` from repo root.

See [`../run_app_guid.md`](../run_app_guid.md) for details.

API:

- Platform: `http://api.localhost:8080`
- Shop: `http://{slug}.api.localhost:8080` (also accepts legacy `Host: {slug}.localhost:8080`)

First run: open the Super Admin portal → **Create super admin** at `/superadmin/setup`, then sign in.

## Auth

- `GET /api/v1/auth/admin/setup-status` — `{ needs_setup }` (platform host)
- `POST /api/v1/auth/admin/setup` — first Super Admin (email + password)
- `POST /api/v1/auth/admin/login` — SUPER_ADMIN only
- `POST /api/v1/auth/tenant/login` — requires `Host: {slug}.api.localhost:8080` (or `{slug}.localhost:8080`)
- `POST /api/v1/auth/setup-password` — invite token → set password

Tenant APIs require host subdomain to match JWT `tenant_id`.
