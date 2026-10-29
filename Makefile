include .env
export
export GOOSE_MIGRATION_DIR  ?= ./backend/sql/schema

.PHONY: gen dev-backend dev-frontend build-backend sqlc migrate-up migrate-down


proto-gen:
	buf generate

proto-lint:
	buf lint

go-test:
	go test ./...

sqlc:
	sqlc generate .

up:
	goose postgres $(DB_URL) up

reset-test:
	goose postgres $(TEST_DB_URL) reset
	goose postgres $(TEST_DB_URL) up

db-integration-test: reset-test
	go test ./backend/internal/store/... -count=1

down:
	goose postgres $(DB_URL) down

dev-backend:
	go run ./backend/cmd/server

dev-frontend:
	pnpm --filter frontend dev

build-backend:
	go build -o bin/server ./backend/cmd/server
