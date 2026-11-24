# commercial-billing-service — build, lint and test entry points (LLD §13.3, §14).
-include .env

APP_NAME      ?= commercial-billing
APP_ENV       ?= dev
GO            ?= go
BUILD_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Private platform modules over SSH (an SSH key registered with the
# BCBP-SOLUTIONS-FZC-LLC org), as in the IAM services.
export GOPRIVATE  ?= github.com/BCBP-SOLUTIONS-FZC-LLC/*
export GONOSUMDB  ?= github.com/BCBP-SOLUTIONS-FZC-LLC/*

DOCKER_SOCKET := $(shell [ -S /Users/$(USER)/.docker/run/docker.sock ] && echo unix:///Users/$(USER)/.docker/run/docker.sock || echo unix:///var/run/docker.sock)
export DOCKER_HOST ?= $(DOCKER_SOCKET)
export APP_NAME APP_ENV BUILD_VERSION

# Schema governance (platform-schemagov 0.4), as in the IAM services.
SCHEMA_GOV_IMAGE    ?= ghcr.io/bcbp-solutions-fzc-llc/platform-schemagov:0.4
SCHEMA_GOV_PLATFORM ?= linux/amd64
SCHEMA_DIR          := internal/adapters/eventbus/schemas
SCHEMA_GOV          := docker run --rm --platform "$(SCHEMA_GOV_PLATFORM)" -v "$(CURDIR)":/workspace -w /workspace "$(SCHEMA_GOV_IMAGE)"

LDFLAGS := -s -w -X main.buildVersion=$(BUILD_VERSION)

.PHONY: help
help:
	@echo "build lint arch-lint guards test test-integration test-contract ci run migrate docker-up docker-down tidy fmt fmt-check"
	@echo "schema-extract schema-check schema-validate (schema-gov via $(SCHEMA_GOV_IMAGE))"

# ── Build ───────────────────────────────────────────────────────────────
.PHONY: build
build:
	@mkdir -p bin
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(APP_NAME) ./cmd/server
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/jobs ./cmd/jobs
	$(GO) build -trimpath -ldflags "-s -w" -o bin/migrate ./cmd/migrate
	$(GO) build -trimpath -ldflags "-s -w" -o bin/certify ./cmd/certify
	$(GO) build ./internal/... ./pkg/... ./api/...

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: fmt
fmt:
	@gofmt -l -w .

.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -l . 2>/dev/null); \
	if [ -n "$$unformatted" ]; then echo "FAIL: unformatted files:"; echo "$$unformatted"; exit 1; fi
	@echo "gofmt: all files formatted"

.PHONY: vet
vet:
	$(GO) vet ./...

# ── Lint and architecture (LLD §3.2) ────────────────────────────────────
.PHONY: lint
lint:
	$(GO) tool golangci-lint run ./...
	$(GO) tool golangci-lint run --build-tags=integration ./...

.PHONY: arch-lint
arch-lint:
	bash .github/scripts/arch-lint.sh

# BIL-PCI-1 (stripe imports) and BIL-OPS-2 (outbox access).
.PHONY: guards
guards:
	bash .github/scripts/check-stripe-imports.sh
	bash .github/scripts/check-outbox-access.sh
	bash .github/scripts/check-problem-details.sh

# ── Tests (LLD §14) ─────────────────────────────────────────────────────
.PHONY: test
test:
	$(GO) test ./... -race -count=1 -timeout 180s

# testcontainers-go Postgres (Phase 2); floci and stripe-mock join in later phases.
.PHONY: test-integration
test-integration:
	$(GO) test ./test/... -tags=integration -race -count=1 -timeout 900s

.PHONY: test-contract
test-contract:
	$(GO) test ./test/contract/... -tags=contract -count=1 -timeout 300s

.PHONY: cover
cover:
	$(GO) test ./... -count=1 -coverprofile=coverage.out
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: ci
ci: fmt-check vet lint arch-lint guards test build

# ── Schema governance (LLD §7.3, §14.4) ─────────────────────────────────
# api/asyncapi.yaml is the source; $(SCHEMA_DIR)/*.json are derived and embedded.
.PHONY: schema-extract
schema-extract:
	$(SCHEMA_GOV) extract --asyncapi api/asyncapi.yaml --schema-dir $(SCHEMA_DIR)

.PHONY: schema-check
schema-check:
	$(SCHEMA_GOV) extract --asyncapi api/asyncapi.yaml --schema-dir $(SCHEMA_DIR) --check

.PHONY: schema-validate
schema-validate: schema-check
	$(SCHEMA_GOV) validate --asyncapi api/asyncapi.yaml --schema-dir $(SCHEMA_DIR)

# ── Run ─────────────────────────────────────────────────────────────────
.PHONY: docker-up
docker-up:
	docker compose up -d
	@echo "postgres :5634  pgbouncer :5633  floci :4667  stripe-mock :12111"

.PHONY: docker-down
docker-down:
	docker compose down

.PHONY: migrate
migrate:
	bash -c 'set -a && source .env && set +a && $(GO) run ./cmd/migrate'

.PHONY: run
run:
	bash -c 'set -a && source .env && set +a && BUILD_VERSION=$(BUILD_VERSION) $(GO) run -ldflags "-X main.buildVersion=$(BUILD_VERSION)" ./cmd/server'

.PHONY: clean
clean:
	rm -rf bin coverage.out

.PHONY: helm-check
helm-check: ## Lint, render and validate the Helm chart and alert rules (LLD §13.1)
	bash .github/scripts/helm-check.sh

.PHONY: testclock
testclock: ## Stripe Test Clock rehearsal against staging (LLD §14.4; see docs/runbooks/test-clock-rehearsal.md)
	bash scripts/testclock/run.sh

.PHONY: certify-rehearsal
certify-rehearsal: ## Run the cert-1.0 suite against stripe-mock and a throwaway Postgres (LLD §14.7)
	$(GO) test -tags=integration -count=1 ./test/certify/

.PHONY: check-first-deploy
check-first-deploy: ## Check the LLD §19.1 first-deployment order for ENV (read-only)
	bash scripts/check-first-deploy.sh $(ENV)

.PHONY: metrics-inventory
metrics-inventory: ## Regenerate docs/observability/metric-registry.md from the code (LLD §11.2)
	UPDATE_INVENTORY=1 $(GO) test -count=1 -run TestInventoryUpToDate ./internal/adapters/metrics/
