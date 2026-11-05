# Common development tasks. Run `make help` for a list.

GO        ?= go
BIN       ?= bin/ledger
PKG       := ./...
# Integration tests need a database whose name ends with _test.
TEST_DATABASE_DSN ?= root@tcp(127.0.0.1:3306)/ledger_test

.PHONY: help build run migrate test test-integration test-race lint fmt cover clean

help: ## Show this help
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-18s %s\n", $$1, $$2}'

build: ## Build the server binary into bin/
	$(GO) build -o $(BIN) ./cmd/ledger

run: ## Run the HTTP server (reads .env)
	$(GO) run ./cmd/ledger serve

migrate: ## Apply database migrations and seed the chart of accounts
	$(GO) run ./cmd/ledger migrate

test: ## Run unit tests (integration tests skip without TEST_DATABASE_DSN)
	$(GO) test $(PKG)

test-integration: ## Run all tests, including MySQL integration tests
	TEST_DATABASE_DSN='$(TEST_DATABASE_DSN)' $(GO) test -count=1 $(PKG)

test-race: ## Run all tests with the race detector against MySQL
	TEST_DATABASE_DSN='$(TEST_DATABASE_DSN)' $(GO) test -race -count=1 $(PKG)

lint: ## Check formatting and run go vet
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi
	$(GO) vet $(PKG)

fmt: ## Format all Go files
	gofmt -w .

cover: ## Run integration tests with a coverage report
	TEST_DATABASE_DSN='$(TEST_DATABASE_DSN)' $(GO) test -count=1 -coverprofile=coverage.out $(PKG)
	$(GO) tool cover -func=coverage.out | tail -n 1

clean: ## Remove build and coverage output
	rm -rf bin coverage.out coverage.html
