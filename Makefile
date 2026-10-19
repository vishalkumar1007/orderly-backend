.PHONY: backup restore-test backup-selftest uptime-check run check check-integration migrate-up migrate-down migrate-status sqlc-generate tidy docker-up docker-down docker-reset wait-db up down down-v restart db-clear db-migrate fresh-run

PG_CONTAINER := orderly-postgres
# Host port for Postgres: the one our running container already uses, else the
# first free port from 5432 (another project may hold 5432). Override: PG_PORT=…
ifndef PG_PORT
PG_PORT := $(shell p=$$(docker port $(PG_CONTAINER) 5432/tcp 2>/dev/null | head -1 | sed 's/.*://'); \
	if [ -z "$$p" ]; then for p in 5432 5433 5434 5435 5436 5437 5438 5439 5440; do \
	lsof -nP -iTCP:$$p -sTCP:LISTEN >/dev/null 2>&1 || break; done; fi; echo $$p)
endif
export PG_PORT
DATABASE_URL ?= postgres://orderly:orderly@localhost:$(PG_PORT)/orderly?sslmode=disable
MIGRATIONS_DIR := db/migrations
COMPOSE := docker compose -f docker/docker-compose.yml

GOOSE := go run github.com/pressly/goose/v3/cmd/goose@v3.24.1
# Prefer a prebuilt binary: `go run` sqlc fails to compile pg_query on recent macOS SDKs.
SQLC := $(shell if [ -x "$(CURDIR)/.tools/sqlc" ]; then echo "$(CURDIR)/.tools/sqlc"; elif command -v sqlc >/dev/null 2>&1; then command -v sqlc; else echo "go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.28.0"; fi)

# API port: first free from 8080 (override: API_PORT=…). Written to .api-port so
# the frontend's Vite proxy follows it. Env vars win over .env.
run:
	@p=$${API_PORT:-}; if [ -z "$$p" ]; then for p in 8080 8081 8082 8083 8084 8085 8086 8087 8088 8089 8090; do \
		lsof -nP -iTCP:$$p -sTCP:LISTEN >/dev/null 2>&1 || break; done; fi; \
	echo $$p > .api-port; echo "API on localhost:$$p"; \
	DATABASE_URL="$(DATABASE_URL)" HTTP_ADDR=":$$p" go run ./cmd/server

# Same gates as the GitHub Actions "Checks" job: run before push.
# Clears DATABASE_URL / CONFIG_ENCRYPTION_KEY so a local .env does not force
# integration tests (those skip in CI when no DB is configured).
check:
	@echo "==> go vet"
	go vet ./...
	@echo "==> go test"
	DATABASE_URL= CONFIG_ENCRYPTION_KEY= go test ./...
	@echo "==> go build ./cmd/server"
	go build -o /dev/null ./cmd/server
	@echo "Checks passed."

# Full suite including DB-backed integration tests (requires: make up).
check-integration:
	@echo "==> go vet"
	go vet ./...
	@echo "==> go test (with DATABASE_URL from environment/.env)"
	go test ./...
	@echo "==> go build ./cmd/server"
	go build -o /dev/null ./cmd/server
	@echo "Integration checks passed."

migrate-up:
	$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" up

migrate-down:
	$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" down

migrate-status:
	$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" status

sqlc-generate:
	cd db && $(SQLC) generate

tidy:
	go mod tidy

docker-up:
	@echo "Postgres on localhost:$(PG_PORT)"
	$(COMPOSE) up -d

docker-down:
	$(COMPOSE) down

docker-reset:
	$(COMPOSE) down -v
	$(COMPOSE) up -d

# Avanor-style: down keeps volumes; down-v wipes data; up starts DB + migrations.
down: docker-down

down-v:
	$(COMPOSE) down -v

up: docker-up wait-db migrate-up

restart: down up

wait-db:
	@echo "Waiting for Postgres..."
	@for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30; do \
		if docker exec $(PG_CONTAINER) pg_isready -U orderly -d orderly >/dev/null 2>&1; then \
			echo "Postgres is ready."; \
			exit 0; \
		fi; \
		sleep 1; \
	done; \
	echo "Postgres did not become ready in 30s"; \
	exit 1

# Wipe data, then bring DB up with migrations (Super Admin via /superadmin/setup).
db-clear: down-v up

# Migrations only (Postgres must already be running).
db-migrate: migrate-up

fresh-run: db-clear run

# ---- Backups (see docs/operations.md). Local default talks to the dev container;
# in production set PG_CONTAINER to the Postgres container and BACKUP_DIR to a real path.
PG_CONTAINER ?= $(PG_CONTAINER_DEFAULT)
PG_CONTAINER_DEFAULT := orderly-postgres
export PG_CONTAINER

backup:
	scripts/backup.sh

# Restores the newest backup (or BACKUP=path) into a throwaway database and checks it.
restore-test:
	scripts/restore-test.sh $(BACKUP)

# Proves the restore test can fail: damages copies of a backup and expects refusal.
backup-selftest:
	scripts/backup-selftest.sh

# One-shot uptime probe: make uptime-check URL=https://api.orderly.qd.je
uptime-check:
	scripts/uptime-check.sh $(URL)
