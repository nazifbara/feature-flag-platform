include .env
export
export GOOSE_MIGRATION_DIR  ?= ./backend/sql/schema

proto-gen:
	buf generate

proto-lint:
	buf lint

# Every Go module in the repository. ./... stops at module boundaries, so each
# is run on its own.
GO_MODULES := . evaluator

go-test:
	for m in $(GO_MODULES); do (cd $$m && go test ./...) || exit 1; done

sqlc:
	sqlc generate .

up:
	goose postgres $(DB_URL) up

reset-db:
	goose postgres $(DB_URL) reset
	goose postgres $(DB_URL) up

reset-test-db:
	goose postgres $(TEST_DB_URL) reset
	goose postgres $(TEST_DB_URL) up

db-integration-test: reset-test-db
	go test ./backend/internal/store/... -count=1 -cover

down:
	goose postgres $(DB_URL) down

dev-backend:
	go run ./backend/cmd/server

dev-frontend:
	pnpm --filter frontend dev

build-backend:
	go build -o bin/server ./backend/cmd/server
