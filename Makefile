.PHONY: run migrate-up migrate-down migrate-status sqlc-generate tidy docker-up docker-down docker-reset wait-db up down down-v restart db-clear db-migrate fresh-run

DATABASE_URL ?= postgres://orderly:orderly@localhost:5432/orderly?sslmode=disable
MIGRATIONS_DIR := db/migrations
COMPOSE := docker compose -f docker/docker-compose.yml
PG_CONTAINER := orderly-postgres

GOOSE := go run github.com/pressly/goose/v3/cmd/goose@v3.24.1
# Prefer a prebuilt binary: `go run` sqlc fails to compile pg_query on recent macOS SDKs.
SQLC := $(shell if [ -x "$(CURDIR)/.tools/sqlc" ]; then echo "$(CURDIR)/.tools/sqlc"; elif command -v sqlc >/dev/null 2>&1; then command -v sqlc; else echo "go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.28.0"; fi)

run:
	go run ./cmd/server

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
