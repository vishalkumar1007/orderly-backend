# Operations: health, backups, recovery

Everything here has been run, not just written. The scripts live in `scripts/`; `make backup-selftest` re-proves the backup tooling any time (see §3).

## 1. Is it up? (R4)

| Endpoint | Answers | Use it for |
|---|---|---|
| `GET /health` | `200 {"status":"ok","version","uptime_seconds"}`. Touches nothing else, so a slow database never makes a healthy API look dead | Liveness: "is the process alive" |
| `GET /health/ready` | `200` with `database.ok`, `database.latency_ms`, `schema_version` (latest applied migration), `version`. `503 not_ready` if the database does not answer within 2 s | Readiness and uptime monitoring |

No auth, and they ignore tenants: they answer the same on `api.`, `admin.` and a tenant host, even a suspended one.

**What the public can see.** Set `HEALTH_DETAIL_TOKEN` in production. Then anyone gets only up/down (`status`, `database.ok`); the build `version`, `uptime_seconds`, `latency_ms` and `schema_version` need the header `X-Health-Token: <token>`. Left unset (local dev) everything is shown. The container healthcheck needs only the status code, so it is unaffected. `scripts/uptime-check.sh` sends the token when `HEALTH_TOKEN` is set in its environment.

**Container healthcheck.** The runtime image is distroless (no shell, no curl), so the binary probes itself: `/server healthcheck` calls `/health/ready` on its own port and exits 0 or 1. The Dockerfile runs it every 30 s; `docker ps` shows `(healthy)` / `(unhealthy)`. Stamp deploys so you can tell them apart:

```bash
docker build --build-arg VERSION=$(git rev-parse --short HEAD) -t <image> orderly-backend
```

**External monitor.** From any machine, or from cron:

```bash
scripts/uptime-check.sh https://api.orderly.qd.je 2000
# UP    https://api.orderly.qd.je ready in 46ms "version":"a1b2c3d"      (exit 0)
# DOWN  ... / SLOW  ...                                                    (exit 1, one line to mail or log)
```

Alert on: two consecutive failures · `schema_version` older than the latest migration after a deploy (you forgot to migrate) · `latency_ms` climbing.

## 2. Backups (R3)

`scripts/backup.sh` takes one consistent `pg_dump` (custom format), proves `pg_restore` can read it, writes a SHA-256 and a manifest (migration version and row count of every table), and only then prunes old ones. A failed run never deletes an existing backup and never leaves a half-written file.

```bash
# production: run the tools INSIDE the Postgres container so versions always match
PG_CONTAINER=fs-A1-d3e4-p5 PGUSER=orderly PGDATABASE=orderly \
  BACKUP_DIR=/var/backups/orderly KEEP_LAST=48 scripts/backup.sh
```

| Setting | Default | Meaning |
|---|---|---|
| `BACKUP_DIR` | `./backups` | Where files go. Not in git (`backups/`, `*.dump` are ignored) because they contain real customer data |
| `KEEP_LAST` | `48` | Newest backups kept. Hourly × 48 = 2 days of history |

**Permissions.** Backups hold the whole customer database, so the script creates everything private (`umask 077`: files `-rw-------`, a directory it creates `drwx------`) and `make backup-selftest` fails if that ever regresses. A `BACKUP_DIR` you created yourself keeps your permissions; make it `chmod 700`.

**Schedule (cron, hourly; `flock` stops overlaps):**

```cron
0 * * * *   flock -n /tmp/orderly-backup.lock env PG_CONTAINER=fs-A1-d3e4-p5 BACKUP_DIR=/var/backups/orderly /opt/orderly/scripts/backup.sh >> /var/log/orderly-backup.log 2>&1
30 3 * * 0  env PG_CONTAINER=fs-A1-d3e4-p5 BACKUP_DIR=/var/backups/orderly /opt/orderly/scripts/restore-test.sh >> /var/log/orderly-restore-test.log 2>&1
```

**Recovery point:** up to 1 hour of orders can be lost in a disaster. For less, run it more often (a dump of a database this size takes seconds) or add WAL archiving (not done).

**Not automated, do it:** copy `BACKUP_DIR` off the server (another host or a bucket). A backup on the same disk as the database does not survive losing the machine. Wire the cron's non-zero exit to an alert.

**Host-tools mode** (`DATABASE_URL=…` instead of `PG_CONTAINER`) works only if this machine's PostgreSQL client is the same major version as the server. A newer client makes dumps the server cannot restore; the script refuses to run in that case rather than hand you an unusable backup.

## 3. Proving a backup restores

An untested backup is a hope. `scripts/restore-test.sh [dump]` (newest backup if omitted):

1. verifies the checksum;
2. restores into a throwaway database on the same server (never the real one), failing on any `pg_restore` error, which includes constraints and foreign keys;
3. checks the migration version matches the manifest;
4. checks every table in the manifest exists with a matching row count (tolerance `max(5, 2%)`, because a live database keeps changing while it is dumped);
5. drops the throwaway database, even on failure.

Exit 0 means restorable. Anything else means **do not trust that backup**; take a new one and find out why.

`make backup-selftest` proves the test itself can fail. It restores a good backup, then damages copies (flipped byte, truncated file, garbage, wrong row counts, missing table, wrong schema version, no backup at all) and requires every one to be refused. It also checks retention and that a failed backup touches nothing. Run it after changing any script here.

Locally: `make backup`, `make restore-test`, `make backup-selftest` (they use the `orderly-postgres` container).

## 4. Restoring after a disaster

Restore into a **new** database, check it, then swap names. The old database is kept for rollback and nothing is dropped until you choose to.

```bash
PG=fs-A1-d3e4-p5; U=orderly; DUMP=/var/backups/orderly/orderly-<UTC>.dump
P() { docker exec -i $PG psql -X -q -A -t -v ON_ERROR_STOP=1 -U $U -d postgres "$@"; }

scripts/restore-test.sh "$DUMP"                       # 1. is this backup good? stop here if not

docker stop fs-A1-d3e4-k9                             # 2. stop the API (no writes during the swap)
P -c "CREATE DATABASE orderly_new"
docker exec -i $PG pg_restore --no-owner --exit-on-error -U $U -d orderly_new < "$DUMP"   # 3. restore
P -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname IN ('orderly','orderly_new') AND pid <> pg_backend_pid()"
P -c "ALTER DATABASE orderly RENAME TO orderly_old_$(date -u +%Y%m%d)"                    # 4. swap
P -c "ALTER DATABASE orderly_new RENAME TO orderly"

docker start fs-A1-d3e4-k9                            # 5. start the API, then:
scripts/uptime-check.sh https://api.orderly.qd.je     #    UP? and schema_version as expected?
```

If the restored data is older than the last migration, run `make migrate-up` before step 5. Once you are sure, drop `orderly_old_<date>`. This exact sequence (restore, rename, keep old, verify) was rehearsed on scratch databases; the container names above are from `orderly-frontend/docs/deploy-codebook.md`.

## 5. When something fails

| Symptom | Meaning / action |
|---|---|
| `backup.sh`: `dump is not a readable pg_restore archive` | The dump was cut short (disk full, container restarted mid-run). Existing backups are untouched. Free space, rerun |
| `backup.sh`: `client is vN but the server is vM` | Host-tools mode with a newer client. Use `PG_CONTAINER` |
| `restore-test.sh`: `checksum mismatch` | The file changed after it was written (disk, copy, transfer). Use another backup; check the disk |
| `restore-test.sh`: `pg_restore reported errors` | The backup is not restorable. Treat it as no backup; take a new one now |
| `restore-test.sh`: `row counts … tolerance` on a quiet system | Data loss between the dump and the manifest, or a bad dump. Investigate before relying on it |
| `uptime-check`: `DOWN … database check is not ok` | API is up but cannot reach Postgres. Check the Postgres container and `DATABASE_URL` |
