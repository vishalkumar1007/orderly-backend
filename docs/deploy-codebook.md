# Deploy codebook (private)

Owner/developer documentation only. Do **not** put semantic words like
`frontend`, `backend`, or `orderly` into AWS resource names.

## Private codebook

| Code | Meaning |
|------|---------|
| `fs-A1-d3e4` | Project |
| `ef3` | Frontend (semantic component) |
| `fe4b` | Backend (semantic component) |
| `x7` | Frontend runtime / UI |
| `k9` | Backend runtime / API |
| `n2` | Docker network |
| `p5` | PostgreSQL |
| `v8` | PostgreSQL volume |
| `i6` | Image/archive storage |
| `u4` | Deployment user |

## Hierarchy

```text
fs-A1-d3e4
├── ef3
│   └── x7
├── fe4b
│   └── k9
├── n2
├── p5
├── v8
├── i6
└── u4
```

Runtime Docker relationship:

```text
fs-A1-d3e4-n2
├── fs-A1-d3e4-x7
├── fs-A1-d3e4-k9
└── fs-A1-d3e4-p5
    └── fs-A1-d3e4-v8
```

Infrastructure resource names (opaque):

| Role | Name |
|------|------|
| Frontend runtime | `fs-A1-d3e4-x7` |
| Backend runtime | `fs-A1-d3e4-k9` |
| Network | `fs-A1-d3e4-n2` |
| PostgreSQL | `fs-A1-d3e4-p5` |
| Volume | `fs-A1-d3e4-v8` |
| Deploy user | `fs-A1-d3e4-u4` |
| Images (k9) | `/opt/fs-A1-d3e4/k9/images/` |
| Images (x7) | `/opt/fs-A1-d3e4/x7/images/` |

### Semantic vs runtime

| Layer | Frontend | Backend |
|-------|----------|---------|
| Semantic (docs only) | `ef3` | `fe4b` |
| Runtime (AWS/Docker) | `x7` | `k9` |

- `ef3` → Frontend → `x7` → frontend runtime
- `fe4b` → Backend → `k9` → backend/API runtime

Infrastructure on the server only exposes opaque codes: `x7`, `k9`, `n2`, `p5`,
`v8`, `i6`, `u4` (with the `fs-A1-d3e4-` prefix where applicable).

**Never use as resource names:** `frontend`, `backend`, `api`, `web`, `orderly`,
`fs-A1-d3e4-frontend`, `fs-A1-d3e4-backend`, `/opt/fs-A1-d3e4/frontend/`,
`/opt/fs-A1-d3e4/backend/`.

## Current CI scope (this repository)

Workflow: `.github/workflows/ci-deploy.yml` (**CI Deploy**)

Five jobs:

1. Checks
2. Build
3. Create Docker Image
4. SSH Access
5. Deployment

Deploys **Backend only** (`fe4b` → `k9`). No registry (no GHCR / Docker Hub / ECR).
Image transfer: Docker image → tar → gzip → GitHub Actions artifact → SCP →
`docker load` on AWS.

| Kind | Value |
|------|--------|
| Image | `fs-a1-d3e4-k9:<github-sha>` (lowercase; Docker requirement) |
| Container | `fs-A1-d3e4-k9` |
| Archive | `fs-a1-d3e4-k9-<sha>.tar.gz` |
| Artifact | `fs-a1-d3e4-k9-<sha>` |
| Network | `fs-A1-d3e4-n2` |
| Env file | `/opt/fs-A1-d3e4/config/k9.env` |

Frontend (`ef3` / `x7`) is deployed from the **orderly-frontend** repository via
its own `ci-deploy.yml` (same opaque project, same host/user/network). See
`orderly-frontend/docs/deploy-codebook.md`. This Backend workflow must never
modify `fs-A1-d3e4-x7` or `fs-A1-d3e4-p5`.

## Server layout

```text
/opt/fs-A1-d3e4/
├── k9/
│   └── images/     # i6 for Backend archives
├── x7/
│   └── images/     # i6 for Frontend archives (sibling CI)
└── config/
    ├── k9.env
    └── x7.env      # created for Frontend deploy
```

## Protected workloads

Never stop, remove, restart, prune, or modify:

- `fs-worker`
- Trino
- Ollama
- `fs-A1-d3e4-x7` (Frontend sibling)
- `fs-A1-d3e4-p5` (Postgres)
- any unrelated container, network, volume, or directory

Never run broad cleanup (`docker system prune`, `docker image prune -a`,
`docker container prune`, `docker rm $(docker ps -aq)`, etc.).

Operate only on `fs-A1-d3e4-k9` (and required shared project network/env paths).

## PostgreSQL (outside CI)

`fs-A1-d3e4-p5` and volume `fs-A1-d3e4-v8` are provisioned **separately** on
`fs-A1-d3e4-n2` (see setup checklist below).

This GitHub Actions workflow must **never** create, replace, stop, or remove
PostgreSQL. Do **not** publish PostgreSQL to a host port.

## NO ROLLBACK

- Archives under `/opt/fs-A1-d3e4/k9/images/` (latest 5) are **not** a rollback system.
- There is no automatic rollback, rollback job, rollback script, or
  restore-on-failure.
- If deployment fails, the workflow fails; unrelated resources stay untouched.
- The target container is stopped/replaced only after its configured image
  identity is verified to start with `fs-a1-d3e4-k9:`.
- Post-deploy check: target container is running (`docker ps`). No
  application-level health endpoint / rollback loop yet.

## Container start (no host ports)

```bash
docker run -d \
  --name fs-A1-d3e4-k9 \
  --restart unless-stopped \
  --network fs-A1-d3e4-n2 \
  --env-file /opt/fs-A1-d3e4/config/k9.env \
  fs-a1-d3e4-k9:<sha>
```

Listens inside the network on `:8080`. Do **not** publish host ports (existing
service already uses host 8080).

## `k9.env` keys (from application config)

Required / expected:

```env
DATABASE_URL=postgres://USER:PASSWORD@fs-A1-d3e4-p5:5432/DBNAME?sslmode=disable
JWT_ACCESS_SECRET=...
JWT_REFRESH_SECRET=...
CONFIG_ENCRYPTION_KEY=...
HTTP_ADDR=:8080
APP_ENV=production
LOG_LEVEL=info
BASE_DOMAIN=orderly.qd.je
FRONTEND_PORT=5173
```

`DATABASE_URL` is required or the process exits. Secrets stay on the server —
never in GitHub Actions YAML or repository secrets for app credentials.

Public domain `orderly.qd.je` is configuration only, not an infrastructure name.

---

# Setup checklist

Complete these **before** the first successful **Deployment** job.

## A. GitHub Actions secrets

Repository → **Settings → Secrets and variables → Actions**:

| Secret | Value |
|--------|--------|
| `FS_A1_D3E4_DEPLOY_HOST` | AWS public IP or hostname |
| `FS_A1_D3E4_DEPLOY_USER` | Must be exactly `fs-A1-d3e4-u4` |
| `FS_A1_D3E4_DEPLOY_SSH_KEY` | Private key PEM for that user |
| `FS_A1_D3E4_SSH_KNOWN_HOSTS` | Pinned `known_hosts` line(s) for the host |

### Pinning `known_hosts`

On a trusted machine (once):

```bash
ssh-keyscan -H YOUR_HOST_OR_IP
```

Copy the output into `FS_A1_D3E4_SSH_KNOWN_HOSTS`. The workflow uses
`StrictHostKeyChecking=yes` and does **not** trust a fresh `ssh-keyscan` at
deploy time.

## B. Create deployment user on AWS (one-time)

Run as a privileged admin on the server (manually). Do not touch existing
workloads.

1. Create user `fs-A1-d3e4-u4`
2. Add their public key to `~fs-A1-d3e4-u4/.ssh/authorized_keys`
3. Add the user to the `docker` group so `docker info` works without root
4. Confirm login: `ssh fs-A1-d3e4-u4@HOST`

## C. Directories (one-time)

```bash
sudo mkdir -p /opt/fs-A1-d3e4/k9/images \
             /opt/fs-A1-d3e4/x7/images \
             /opt/fs-A1-d3e4/config
sudo chown -R fs-A1-d3e4-u4:fs-A1-d3e4-u4 /opt/fs-A1-d3e4
```

Create `/opt/fs-A1-d3e4/config/k9.env` owned by `fs-A1-d3e4-u4` with the keys
listed above (strong secrets; not committed to git).

For Frontend, also create `/opt/fs-A1-d3e4/config/x7.env` (see
`orderly-frontend/docs/deploy-codebook.md`).

## D. Docker network (one-time)

Inspect first; abort if the name exists but is unexpected:

```bash
docker network ls
docker network create fs-A1-d3e4-n2
```

Do not attach unrelated existing containers to this network.

## E. PostgreSQL (one-time, outside CI)

Prefer **no host port**. Create only if names are free. CI will not manage this.

```bash
docker volume create fs-A1-d3e4-v8

docker run -d \
  --name fs-A1-d3e4-p5 \
  --restart unless-stopped \
  --network fs-A1-d3e4-n2 \
  -e POSTGRES_USER='choose_user' \
  -e POSTGRES_PASSWORD='strong_password' \
  -e POSTGRES_DB='choose_db' \
  -v fs-A1-d3e4-v8:/var/lib/postgresql/data \
  postgres:16-alpine
```

Point `DATABASE_URL` in `k9.env` at host `fs-A1-d3e4-p5`.

If names/ports conflict with an existing Postgres or other workload: **stop and
report** — do not replace.

Run application migrations against this database once before first API traffic.

### Optional DB GUI access

Do not open PostgreSQL publicly. Use an SSH tunnel, for example:

```bash
ssh -L 5433:fs-A1-d3e4-p5:5432 fs-A1-d3e4-u4@HOST
```

(Requires the SSH session can reach the container network, e.g. via docker
exec/`psql` on the host, or a carefully designed tunnel target. Prefer
`docker exec -it fs-A1-d3e4-p5 psql ...` on the server when possible.)

## F. Verify before first push

- [ ] All four GitHub secrets set
- [ ] User `fs-A1-d3e4-u4` can SSH and run `docker info`
- [ ] Directories and `k9.env` exist
- [ ] Network `fs-A1-d3e4-n2` exists
- [ ] Container `fs-A1-d3e4-p5` healthy on that network
- [ ] `fs-worker` / Trino / Ollama still running and untouched
- [ ] Push to `main` (or run **CI Deploy** via `workflow_dispatch`)

## G. After a green Deployment

- Actions shows five green jobs
- `docker ps` lists `fs-A1-d3e4-k9`
- Archives under `/opt/fs-A1-d3e4/k9/images/` (max 5 kept; **not** a rollback system)
- Reverse proxy / public hostname wiring remains a later change
