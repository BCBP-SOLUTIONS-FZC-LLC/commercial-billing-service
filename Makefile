# commercial-billing-service — build, lint, test, schema and release entry points
# (LLD §13.3, §14). `make help` lists every target. CI (.github/workflows/ci.yml)
# runs `make fmt-check vet lint arch-lint guards test build`, `make helm-check`
# and schema-gov; `make ci` is the same gate locally.

# -----------------------------
# CONFIG
# -----------------------------
-include .env

APP_NAME      ?= commercial-billing
APP_ENV       ?= dev
GO            ?= go
BUILD_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Private platform modules over SSH (an SSH key registered with the
# BCBP-SOLUTIONS-FZC-LLC org), as in the IAM services. CI uses GO_PRIVATE_TOKEN.
export GOPRIVATE  ?= github.com/BCBP-SOLUTIONS-FZC-LLC/*
export GONOSUMDB  ?= github.com/BCBP-SOLUTIONS-FZC-LLC/*

# Docker socket for testcontainers-go (Docker Desktop on macOS uses a user socket).
DOCKER_SOCKET := $(shell [ -S /Users/$(USER)/.docker/run/docker.sock ] && echo unix:///Users/$(USER)/.docker/run/docker.sock || echo unix:///var/run/docker.sock)
export DOCKER_HOST ?= $(DOCKER_SOCKET)
export APP_NAME APP_ENV BUILD_VERSION

# Every build tag the test/ tree declares: integration (test/postgres, flows,
# bus, stripemock, certify) and helm (test/helm). vet and lint run a second
# pass with all of them, so tagged test files are vetted and linted too.
ALL_TEST_TAGS := integration,helm

# Integration suites (each starts its own containers through testcontainers-go):
# ./test/... plus the integration-tagged tests that live beside the code
# (internal/certify, internal/bootstrap, cmd/*).
TEST_INT_PKGS := ./test/... ./internal/... ./cmd/...
# Each integration package starts its own containers; cap how many run at
# once so a laptop's Docker (8 GB) is not overwhelmed.
INT_PARALLEL  ?= 4

# Coverage is measured over the production packages only.
COVER_PKG_LIST := $(shell $(GO) list ./internal/... ./pkg/... ./cmd/... 2>/dev/null | tr '\n' ',' | sed 's/,$$//')

# Schema governance (platform-schemagov 0.4), as in the IAM services. The image
# is published linux/amd64 only; Apple Silicon needs the platform flag.
SCHEMA_GOV_IMAGE    ?= ghcr.io/bcbp-solutions-fzc-llc/platform-schemagov:0.4
SCHEMA_GOV_PLATFORM ?= linux/amd64
SCHEMA_DIR          := internal/adapters/eventbus/schemas
SCHEMA_GOV          := docker run --rm --platform "$(SCHEMA_GOV_PLATFORM)" -v "$(CURDIR)":/workspace -w /workspace "$(SCHEMA_GOV_IMAGE)"
# Glue registry for billing-events (LLD §7.2); floci locally (AWS_ENDPOINT_URL).
GLUE_REGISTRY_NAME  ?= billing-events

LDFLAGS := -s -w -X main.buildVersion=$(BUILD_VERSION)

# -----------------------------
# HELP
# -----------------------------

.PHONY: help
help:
	@echo "Setup"
	@echo "  make setup             - copy .env-example to .env if missing; install .githooks/pre-commit + pre-push"
	@echo "  make install-hooks     - (re)install .githooks: pre-commit (.env, tidy, fmt, lint, guards, openapi-check), pre-push (make trivy)"
	@echo "  make tidy              - go mod tidy"
	@echo "  make godoc             - serve package docs locally (pkgsite) at http://localhost:8080"
	@echo ""
	@echo "Quality (CI 'validate-quality' / 'validate-test' jobs)"
	@echo "  make fmt / fmt-check   - gofmt write / verify (mirrors CI)"
	@echo "  make vet               - go vet (default build + every test build tag)"
	@echo "  make lint              - golangci-lint via go tool (default build + every test build tag)"
	@echo "  make arch-lint         - go-arch-lint against .go-arch-lint.yml (LLD §3.2)"
	@echo "  make guards            - BIL-PCI-1 stripe imports, BIL-OPS-2 outbox, BIL-API-6 problems, BIL-OPS-11 pgcommon"
	@echo "  make metrics-lint      - Enterprise Platform Observability Standard checks (also in make test)"
	@echo "  make metrics-inventory - regenerate docs/observability/metric-registry.md"
	@echo "  make openapi           - regenerate docs/swagger/openapi.json from the route table"
	@echo "  make openapi-check     - fail if docs/swagger/openapi.json is stale"
	@echo "  make mod-verify        - go mod verify"
	@echo "  make vuln-check        - govulncheck on cmd, internal and pkg (needs network)"
	@echo "  make ci                - fmt-check + vet + lint + arch-lint + guards + test + build (the CI gate)"
	@echo ""
	@echo "Tests"
	@echo "  make test              - all packages with -race: unit, HTTP, contract, observability (no Docker)"
	@echo "  make test-unit         - same as test, without -race and verbose"
	@echo "  make test-integration  - every ./test/... suite with tag integration (Docker)"
	@echo "  make test-postgres     - RLS, constraints, triggers, repositories (Docker)"
	@echo "  make test-flows        - every §8 flow with its failure branches (Docker)"
	@echo "  make test-bus          - SNS/SQS/Glue through floci (Docker)"
	@echo "  make test-stripemock   - the Stripe adapter against stripe-mock (Docker)"
	@echo "  make test-e2e          - the full HTTP stack end to end on Postgres 17 (Docker)"
	@echo "  make test-contract     - produced payloads vs Billing's and consumers' schemas"
	@echo "  make test-helm         - chart contract tests (needs helm)"
	@echo "  make race              - make test + make test-integration, both with -race"
	@echo "  make cover             - coverage profile + total (Docker-free suites)"
	@echo "  make cover-all         - coverage including the integration suites (Docker)"
	@echo "  make cover-func / cover-html - per-function summary / HTML report of coverage.out"
	@echo "  make cover-gate        - fail if coverage.out over internal/ + pkg/ is below COVERAGE_MIN (96)"
	@echo ""
	@echo "Run and build"
	@echo "  make docker-up / docker-down / docker-reset - local Postgres 17, PgBouncer, floci, stripe-mock"
	@echo "  make migrate           - apply library schemas + Billing migrations against .env"
	@echo "  make run               - run the server against .env (frees APP_PORT first)"
	@echo "  make build             - bin/commercial-billing, bin/jobs, bin/migrate, bin/certify"
	@echo "  make docker-build      - build the service image (needs GO_PRIVATE_TOKEN or gh auth)"
	@echo "  make trivy             - build the image and Trivy-scan it (CRITICAL/HIGH/UNKNOWN, fixed only); run by .githooks/pre-push"
	@echo "  make pin-base-images   - pin the Dockerfile base images to their current SHA digests"
	@echo "  make clean             - remove bin/, coverage and temp files"
	@echo ""
	@echo "Deployment checks"
	@echo "  make helm-check        - helm lint + kubeconform + promtool + chart contract tests (CI 'Helm check' job)"
	@echo "  make check-first-deploy ENV=<env> - LLD §19.1 first-deployment order (read-only)"
	@echo "  make certify-rehearsal - cert-1.0 provider certification suite against stripe-mock"
	@echo "  make certify-adapter-version - print the Stripe adapter version certification is bound to"
	@echo "  make testclock         - Stripe Test Clock rehearsal against staging"
	@echo ""
	@echo "Schema governance (platform-schemagov 0.4; CI 'Schema governance' job)"
	@echo "  make schema-pull       - pull the schema-gov image"
	@echo "  make schema-extract    - api/asyncapi.yaml -> $(SCHEMA_DIR)/*.json"
	@echo "  make schema-check      - fail if the embedded schemas drift from api/asyncapi.yaml"
	@echo "  make schema-validate   - schema-check + schema-gov validate (no AWS needed)"
	@echo "  make schema-diff CURRENT=<path> PROPOSED=<path> - compatibility diff of two schemas"
	@echo "  make schema-register   - register the six produced schemas in Glue $(GLUE_REGISTRY_NAME) (AWS or floci)"
	@echo "  make schema-verify     - pre-deploy: every produced definition registered + AVAILABLE (needs aws CLI)"

# -----------------------------
# SETUP
# -----------------------------

.PHONY: setup
setup:
	@test -f .env || cp .env-example .env
	@$(MAKE) --no-print-directory install-hooks
	@echo "Environment ready (.env, git hooks). Next: make docker-up && make migrate && make run"

# install-hooks: copy .githooks/* into the repository's hooks directory
# (pre-commit: .env, tidy, fmt-check, lint, guards, openapi-check;
# pre-push: make trivy). Re-run whenever .githooks/ changes.
.PHONY: install-hooks
install-hooks:
	@hooks=$$(git rev-parse --git-path hooks) && mkdir -p "$$hooks" && \
	 for h in pre-commit pre-push; do cp .githooks/$$h "$$hooks/$$h" && chmod +x "$$hooks/$$h"; done && \
	 echo "Installed git hooks (pre-commit, pre-push) into $$hooks"

.PHONY: tidy
tidy:
	$(GO) mod tidy

# godoc: package documentation with pkgsite; browse to the module path in the UI.
.PHONY: godoc
godoc:
	@echo "Starting pkgsite at http://localhost:8080 — press Ctrl-C to stop"
	$(GO) run golang.org/x/pkgsite/cmd/pkgsite@latest -open .

# -----------------------------
# GO BASICS AND LINT (LLD §3.2)
# -----------------------------

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
	$(GO) vet -tags=$(ALL_TEST_TAGS) ./...

.PHONY: lint
lint:
	$(GO) tool golangci-lint run ./...
	$(GO) tool golangci-lint run --build-tags=$(ALL_TEST_TAGS) ./...

.PHONY: arch-lint
arch-lint:
	bash .github/scripts/arch-lint.sh

# guards: what an import-level check cannot see — BIL-PCI-1 (stripe imports),
# BIL-OPS-2 (SQL on the platform-events tables), BIL-API-6 (hand-written
# problems, error DTOs, recovery) and BIL-OPS-11 (SQLSTATE literals,
# hand-written session settings).
.PHONY: guards
guards:
	bash .github/scripts/check-stripe-imports.sh
	bash .github/scripts/check-outbox-access.sh
	bash .github/scripts/check-problem-details.sh
	bash .github/scripts/check-pgcommon.sh

# Enterprise Platform Observability Standard (LLD §11.2): metricslint on the
# scrape (namespace tier, naming, _total / _seconds suffixes, required and
# forbidden labels, registry and label vocabulary), every metric referenced by
# alerts / rules / SLOs / dashboard / HPA / runbooks exposed or registered and
# not deprecated, and the inventory current. Also part of `make test`.
.PHONY: metrics-lint
metrics-lint:
	$(GO) test ./internal/adapters/metrics/ -count=1 -v \
	  -run 'TestScrapePassesMetricsLint|TestEveryLLDMetricRegistered|TestReferences|TestReferencedPlatformMetricsAreRegistered|TestInventoryUpToDate'

.PHONY: metrics-inventory
metrics-inventory: ## Regenerate docs/observability/metric-registry.md from the code (LLD §11.2)
	UPDATE_INVENTORY=1 $(GO) test -count=1 -run TestInventoryUpToDate ./internal/adapters/metrics/

# openapi: docs/swagger/openapi.json is generated from the route table and the
# request/response types (internal/adapters/http/openapi.go) — never edited by
# hand. TestOpenAPIGolden (part of `make test`) fails when it is stale.
.PHONY: openapi
openapi:
	$(GO) test ./internal/adapters/http -run TestOpenAPIGolden -count=1 -update
	@echo "docs/swagger/openapi.json regenerated — commit it with the route change"

.PHONY: openapi-check
openapi-check:
	$(GO) test ./internal/adapters/http -run TestOpenAPIGolden -count=1

.PHONY: mod-verify
mod-verify:
	$(GO) mod verify

.PHONY: vuln-check
vuln-check:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./cmd/... ./internal/... ./pkg/...

# -----------------------------
# TESTS (LLD §14)
# -----------------------------

# test: the Docker-free layers (unit, HTTP, contract, observability, golden
# tests) with the race detector. This is what CI runs.
.PHONY: test
test:
	$(GO) test ./... -race -count=1 -timeout 180s

.PHONY: test-unit
test-unit:
	$(GO) test ./... -count=1 -timeout 180s -v

# test-integration: every suite with tag integration — testcontainers
# Postgres 17, floci and stripe-mock are started per suite (make docker-up is
# not needed). Not yet in CI; run before a PR that touches Postgres, flows,
# the bus, the provider or certification.
.PHONY: test-integration
test-integration:
	$(GO) test $(TEST_INT_PKGS) -tags=integration -race -count=1 -timeout 1200s -p $(INT_PARALLEL)

.PHONY: test-postgres
test-postgres:
	$(GO) test ./test/postgres/... -tags=integration -count=1 -timeout 600s -v

.PHONY: test-flows
test-flows:
	$(GO) test ./test/flows/... -tags=integration -count=1 -timeout 600s -v

.PHONY: test-bus
test-bus:
	$(GO) test ./test/bus/... -tags=integration -count=1 -timeout 600s -v

.PHONY: test-stripemock
test-stripemock:
	$(GO) test ./test/stripemock/... -tags=integration -count=1 -timeout 300s -v

# test-contract: CONTRACT_CONSUMERS_DIR=<dir> points it at another copy of the
# consumers' schemas (contract-live.yml uses Entitlements' main).
.PHONY: test-contract
test-contract:
	$(GO) test ./test/contract/... -count=1 -timeout 300s

# test-e2e: the production router, middleware, use cases and repositories
# over HTTP on Postgres 17 with the production roles; only the outbound world
# is faked (test/testkit). Part of test-integration.
.PHONY: test-e2e
test-e2e:
	$(GO) test ./test/e2e/... -tags=integration -count=1 -timeout 600s -v

.PHONY: test-helm
test-helm:
	$(GO) test ./test/helm/... -tags=helm -count=1 -v

.PHONY: race
race: test test-integration

# Coverage over the production packages. `cover` is the Docker-free suites;
# `cover-all` adds the integration suites in the same run, so one profile
# counts each block once. LLD §14.6 targets >= 90% merged at first release;
# there is no CI gate yet.
.PHONY: cover
cover:
	$(GO) test ./... -count=1 -coverpkg=$(COVER_PKG_LIST) -coverprofile=coverage.out
	@$(GO) tool cover -func=coverage.out | tail -1

.PHONY: cover-all
cover-all:
	$(GO) test ./... -tags=integration -count=1 -timeout 1200s -p $(INT_PARALLEL) -coverpkg=$(COVER_PKG_LIST) -coverprofile=coverage.out
	@$(GO) tool cover -func=coverage.out | tail -1

# cover-gate: fail when coverage.out (from cover-all) is below COVERAGE_MIN,
# measured over internal/ and pkg/ as the IAM services gate it (cmd/* is
# startup wiring, partly reachable only in a separate process).
COVERAGE_MIN ?= 96
.PHONY: cover-gate
cover-gate:
	@test -f coverage.out || { echo "no coverage.out: run make cover-all first"; exit 1; }
	@grep -v '/cmd/' coverage.out > .cover-gate.out; \
	pct=$$($(GO) tool cover -func=.cover-gate.out | awk '/^total:/ {sub(/%/,"",$$NF); print $$NF}'); rm -f .cover-gate.out; \
	echo "coverage over internal/ + pkg/: $$pct% (minimum $(COVERAGE_MIN)%)"; \
	awk -v p=$$pct -v m=$(COVERAGE_MIN) 'BEGIN { exit (p+0 >= m+0) ? 0 : 1 }' || { echo "FAIL: coverage below $(COVERAGE_MIN)%"; exit 1; }

.PHONY: cover-func
cover-func:
	@test -f coverage.out || $(MAKE) cover
	$(GO) tool cover -func=coverage.out

.PHONY: cover-html
cover-html:
	@test -f coverage.out || $(MAKE) cover
	$(GO) tool cover -html=coverage.out

# -----------------------------
# RUN AND BUILD
# -----------------------------

.PHONY: docker-up
docker-up:
	docker compose up -d
	@echo "postgres :5634  pgbouncer :5633  floci :4667  stripe-mock :12111"

.PHONY: docker-down
docker-down:
	docker compose down

# docker-reset: wipe the volumes and start again (re-runs init-db.sql and the
# floci ready-hook, e.g. after a schema change).
.PHONY: docker-reset
docker-reset:
	docker compose down -v
	docker compose up -d

.PHONY: migrate
migrate:
	bash -c 'set -a && source .env && set +a && $(GO) run ./cmd/migrate'

.PHONY: run
run:
	@-lsof -ti :$${APP_PORT:-8080} | xargs kill -9 2>/dev/null; true
	bash -c 'set -a && source .env && set +a && BUILD_VERSION=$(BUILD_VERSION) $(GO) run -ldflags "-X main.buildVersion=$(BUILD_VERSION)" ./cmd/server'

.PHONY: build
build:
	@mkdir -p bin
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(APP_NAME) ./cmd/server
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/jobs ./cmd/jobs
	$(GO) build -trimpath -ldflags "-s -w" -o bin/migrate ./cmd/migrate
	$(GO) build -trimpath -ldflags "-s -w" -o bin/certify ./cmd/certify
	$(GO) build ./internal/... ./pkg/... ./api/...

# The Dockerfile needs the go_private_token build secret: GO_PRIVATE_TOKEN from
# the environment, else `gh auth token`.
IMAGE            ?= $(APP_NAME):local
GO_PRIVATE_TOKEN ?= $(shell gh auth token 2>/dev/null)
export GO_PRIVATE_TOKEN

.PHONY: docker-build
docker-build:
	@test -n "$$GO_PRIVATE_TOKEN" || { echo "GO_PRIVATE_TOKEN is empty: export it or run 'gh auth login'"; exit 1; }
	docker build --platform linux/amd64 \
	  --secret id=go_private_token,env=GO_PRIVATE_TOKEN \
	  --build-arg BUILD_VERSION=$(BUILD_VERSION) \
	  -t $(IMAGE) .

# trivy: the planned CI image gate, locally — fail on CRITICAL/HIGH/UNKNOWN
# CVEs that have a fix (a .trivyignore, if added, applies automatically).
.PHONY: trivy
trivy: docker-build
	@command -v trivy >/dev/null || { echo "trivy not found (brew install trivy)"; exit 1; }
	trivy image --severity CRITICAL,HIGH,UNKNOWN --ignore-unfixed --exit-code 1 $(IMAGE)

# pin-base-images: rewrite the Dockerfile FROM lines to the current SHA digests
# of the builder and runtime images (review and commit the result).
GOLANG_IMAGE  ?= golang:1.26.11-alpine
RUNTIME_IMAGE ?= gcr.io/distroless/static-debian13:nonroot

.PHONY: pin-base-images
pin-base-images:
	@GOLANG_DIGEST=$$(docker buildx imagetools inspect $(GOLANG_IMAGE) --format '{{.Manifest.Digest}}') && \
	 RUNTIME_DIGEST=$$(docker buildx imagetools inspect $(RUNTIME_IMAGE) --format '{{.Manifest.Digest}}') && \
	 sed -i.bak -E \
	   -e "s|^FROM $(GOLANG_IMAGE)(@sha256:[0-9a-f]+)?|FROM $(GOLANG_IMAGE)@$$GOLANG_DIGEST|" \
	   -e "s|^FROM $(RUNTIME_IMAGE)(@sha256:[0-9a-f]+)?|FROM $(RUNTIME_IMAGE)@$$RUNTIME_DIGEST|" \
	   Dockerfile && rm -f Dockerfile.bak && \
	 echo "Dockerfile pinned: $(GOLANG_IMAGE)@$$GOLANG_DIGEST, $(RUNTIME_IMAGE)@$$RUNTIME_DIGEST — review and commit"

.PHONY: clean
clean:
	rm -rf bin .tmp
	rm -f coverage.out coverage.html

# -----------------------------
# CI
# -----------------------------

# ci: the same gates as ci.yml's Validate job, in the same order. The Helm and
# schema jobs are `make helm-check` and `make schema-validate`; the
# integration suite (`make test-integration`) is not in CI yet.
.PHONY: ci
ci: fmt-check vet lint arch-lint guards test build

# -----------------------------
# DEPLOYMENT CHECKS
# -----------------------------

# helm-check: helm lint --strict and helm template + kubeconform for the base,
# dev, staging and prod values (namespace billing); promtool on the alert,
# recording and SLO rules; the chart contract tests (test/helm). Same script
# as CI's Helm check job (LLD §13.1, §13.3).
.PHONY: helm-check
helm-check: ## Lint, render and validate the Helm chart and alert rules (LLD §13.1)
	bash .github/scripts/helm-check.sh

.PHONY: check-first-deploy
check-first-deploy: ## Check the LLD §19.1 first-deployment order for ENV (read-only)
	bash scripts/check-first-deploy.sh $(ENV)

.PHONY: certify-rehearsal
certify-rehearsal: ## Run the cert-1.0 suite against stripe-mock and a throwaway Postgres (LLD §14.7)
	$(GO) test -tags=integration -count=1 ./test/certify/

# certify-adapter-version: the version a certification record must exist for
# before the provider takes new accounts (BIL-PROV-1, VERSIONING.md).
.PHONY: certify-adapter-version
certify-adapter-version:
	@$(GO) run ./cmd/certify adapter-version

.PHONY: testclock
testclock: ## Stripe Test Clock rehearsal against staging (LLD §14.4; see docs/runbooks/test-clock-rehearsal.md)
	bash scripts/testclock/run.sh

# -----------------------------
# SCHEMA GOVERNANCE (LLD §7.3, §7.6, §14.4)
# -----------------------------
# api/asyncapi.yaml is the source; $(SCHEMA_DIR)/*.json are derived (schema-gov
# extract) and embedded in the binary. Produced schemas are registered in Glue
# under their event type (tenant_plan_changed.json -> TenantPlanChanged); the
# server resolves each version by definition at startup and refuses to start
# ahead of the registry, so register before deploying.

.PHONY: schema-pull
schema-pull:
	docker pull --platform "$(SCHEMA_GOV_PLATFORM)" "$(SCHEMA_GOV_IMAGE)"

.PHONY: schema-extract
schema-extract:
	$(SCHEMA_GOV) extract --asyncapi api/asyncapi.yaml --schema-dir $(SCHEMA_DIR)
	@echo "Done. Commit $(SCHEMA_DIR)/ with the AsyncAPI change."

.PHONY: schema-check
schema-check:
	$(SCHEMA_GOV) extract --asyncapi api/asyncapi.yaml --schema-dir $(SCHEMA_DIR) --check

.PHONY: schema-validate
schema-validate: schema-check
	$(SCHEMA_GOV) validate --asyncapi api/asyncapi.yaml --schema-dir $(SCHEMA_DIR)

.PHONY: schema-diff
schema-diff:
	@test -n "$(CURRENT)" && test -n "$(PROPOSED)" || { \
	  echo "Usage: make schema-diff CURRENT=<current.json> PROPOSED=<proposed.json> [SCHEMA_NAME=<name>]"; \
	  exit 1; \
	}
	$(SCHEMA_GOV) diff --current "$(CURRENT)" --proposed "$(PROPOSED)" \
	  --schema-name "$(or $(SCHEMA_NAME),$(notdir $(basename $(PROPOSED))))"

# _stage-produced: copy the six produced schemas (tenant_*.json) to
# .tmp/glue/<EventType>.json — the Glue schema names, one per produced type.
.PHONY: _stage-produced
_stage-produced:
	@rm -rf .tmp/glue && mkdir -p .tmp/glue
	@for f in $(SCHEMA_DIR)/tenant_*.json; do \
	  stem=$$(basename "$$f" .json); \
	  name=$$(printf '%s' "$$stem" | awk -F_ '{for(i=1;i<=NF;i++) printf "%s", toupper(substr($$i,1,1)) substr($$i,2)}'); \
	  cp "$$f" ".tmp/glue/$$name.json"; \
	done
	@ls .tmp/glue | sed 's/\.json$$//' | tr '\n' ' '; echo

# schema-register: register the produced schemas in GLUE_REGISTRY_NAME. Needs
# AWS credentials, or AWS_ENDPOINT_URL for floci (make docker-up already
# registers them through scripts/init-floci.sh; use this after a schema change
# without restarting floci, or against a real account).
.PHONY: schema-register
schema-register: _stage-produced
	@echo "Registering produced schemas -> $(GLUE_REGISTRY_NAME)"
	docker run --rm --platform "$(SCHEMA_GOV_PLATFORM)" -v "$(CURDIR)":/workspace -w /workspace \
	  -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_SESSION_TOKEN \
	  -e AWS_REGION="$(AWS_REGION)" -e AWS_ENDPOINT_URL="$(AWS_ENDPOINT_URL)" \
	  "$(SCHEMA_GOV_IMAGE)" register --registry "$(GLUE_REGISTRY_NAME)" --schema-dir .tmp/glue

# schema-verify: fail unless every produced schema has an AVAILABLE version
# whose definition equals this checkout's file — the exact lookup the
# GlueCodec does at startup (GetSchemaByDefinition, compact + ASCII-escaped as
# schema-gov uploads it). Surfaces "the pod would not start" before a deploy.
# Needs the aws CLI, python3 and AWS credentials (or AWS_ENDPOINT_URL).
.PHONY: schema-verify
schema-verify: _stage-produced
	@failed=""; count=0; \
	for file in .tmp/glue/*.json; do \
	  name=$$(basename "$$file" .json); count=$$((count + 1)); \
	  def=$$(python3 -c 'import json,sys; sys.stdout.write(json.dumps(json.load(open(sys.argv[1])), separators=(",", ":")))' "$$file"); \
	  status=$$(aws glue get-schema-by-definition \
	      --schema-id "RegistryName=$(GLUE_REGISTRY_NAME),SchemaName=$$name" \
	      --schema-definition "$$def" \
	      --region "$(AWS_REGION)" $(if $(AWS_ENDPOINT_URL),--endpoint-url $(AWS_ENDPOINT_URL),) \
	      --query Status --output text 2>/dev/null); \
	  [ "$$status" = "AVAILABLE" ] || failed="$$failed $$name($${status:-not-registered})"; \
	done; \
	rm -rf .tmp/glue; \
	if [ -n "$$failed" ]; then \
	  echo "FAIL: not registered + AVAILABLE in $(GLUE_REGISTRY_NAME):$$failed"; \
	  echo "      run 'make schema-register' before deploying"; exit 1; \
	fi; \
	echo "OK: all $$count produced schema definitions registered and AVAILABLE in $(GLUE_REGISTRY_NAME)"
