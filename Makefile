.DEFAULT_GOAL := help
SHELL := /bin/bash

GO          ?= go
COMPOSE     ?= docker compose -f deploy/compose/docker-compose.dev.yml
BIN         := bin/proxui
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_TIME  ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.buildTime=$(BUILD_TIME)

# Load .env for targets that run the app locally.
ifneq (,$(wildcard .env))
include .env
export
endif

.PHONY: help
help: ## Show available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: dev
dev: up ## Start dependencies and run the API with live config
	@test -f .env || cp .env.example .env
	$(GO) run ./cmd/proxui --role=all

.PHONY: up
up: ## Start dev dependencies (postgres+timescale, redis)
	$(COMPOSE) up -d --wait

.PHONY: down
down: ## Stop dev dependencies (keeps volumes)
	$(COMPOSE) down

.PHONY: clean-data
clean-data: ## Stop dev dependencies and delete their volumes
	$(COMPOSE) down -v

.PHONY: build
build: ## Build the binary into bin/
	@mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/proxui

# Integration tests run against their own database so they never disturb dev
# data (a polluted users table silently disables first-run bootstrap).
TEST_DATABASE_URL ?= postgres://proxui:proxui@127.0.0.1:5432/proxui_test?sslmode=disable

.PHONY: test
test: ## Run unit tests with the race detector
	$(GO) test -race -count=1 ./...

.PHONY: test-integration
test-integration: up test-db ## Run every test including database integration tests
	PROXUI_TEST_DATABASE_URL='$(TEST_DATABASE_URL)' $(GO) test -race -count=1 ./...

.PHONY: test-db
test-db: ## Create the integration test database if it is missing
	@$(COMPOSE) exec -T db psql -U proxui -d proxui -tc \
		"SELECT 1 FROM pg_database WHERE datname='proxui_test'" | grep -q 1 || \
		$(COMPOSE) exec -T db psql -U proxui -d proxui -c 'CREATE DATABASE proxui_test OWNER proxui'

.PHONY: cover
cover: ## Run tests and report coverage
	$(GO) test -race -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: lint
lint: ## Run formatting and vet checks
	@test -z "$$(gofmt -l cmd internal)" || { echo "gofmt needed:"; gofmt -l cmd internal; exit 1; }
	$(GO) vet ./...

.PHONY: fmt
fmt: ## Format the Go sources
	gofmt -w cmd internal

.PHONY: tidy
tidy: ## Sync go.mod/go.sum
	$(GO) mod tidy

.PHONY: migrate
migrate: ## Apply pending migrations against the dev database
	$(GO) run ./cmd/proxui --role=api & \
		PID=$$!; sleep 4; kill $$PID 2>/dev/null || true

.PHONY: docker-build
docker-build: ## Build the runtime container image
	docker build -t proxui:$(VERSION) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) .

.PHONY: ci
ci: lint test build ## Everything CI enforces
