SHELL := /bin/sh

.PHONY: help build run test test-race vet fmt up down clean integration demo migrate-up migrate-down

help: ## List available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-18s %s\n", $$1, $$2}'

build: ## Build all binaries into ./bin
	go build -o bin/api ./cmd/api
	go build -o bin/migrate ./cmd/migrate

run: ## Run the API locally (requires PostgreSQL, SQS, Keycloak per .env.example)
	go run ./cmd/api

fmt: ## Format the code
	gofmt -l -w .

vet: ## Run go vet
	go vet ./...

test: ## Run unit tests
	go test ./...

test-race: ## Run unit tests with the race detector
	go test -race ./...

integration: ## Run integration tests inside the Compose network
	docker compose --profile test run --rm tests

up: ## Start the local stack (PostgreSQL, Keycloak, LocalStack, migrations, API)
	docker compose up -d --build

up-three: ## Start the stack with three API instances
	docker compose up -d --build --scale api=3

down: ## Stop the local stack
	docker compose down

clean: ## Stop the stack and remove volumes
	docker compose down -v

migrate-up: ## Apply migrations using the local DATABASE_URL
	DATABASE_URL=$${DATABASE_URL:-postgres://wager:wager@localhost:5432/wager?sslmode=disable} go run ./cmd/migrate -command up

migrate-down: ## Revert the last migration using the local DATABASE_URL
	DATABASE_URL=$${DATABASE_URL:-postgres://wager:wager@localhost:5432/wager?sslmode=disable} go run ./cmd/migrate -command down -steps 1

demo: ## Run the authenticated end-to-end demo against a running stack
	./scripts/demo.sh
