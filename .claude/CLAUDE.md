# CLAUDE.md

This file provides guidance to Claude Code when working with **commercial-billing-service**, the Billing service of the Commercial domain.

## What This Repo Is

`commercial-billing` owns subscriptions, renewals, seat licensing, token add-ons, payment-provider integration (Stripe, `manual`), invoices, refunds and adjustments, and the billing lifecycle events (Go module `github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service`, Go 1.26.11).

**Libraries (go.mod):** platform-gincommon v2.7.0, platform-pgcommon v2.0.2, platform-events v2.3.0, platform-audit v0.1.2, gin v1.12.0, OpenTelemetry v1.47.0, stripe-go v87.2.0 (Stripe API `2026-11-18.endive`; Stripe adapter version 1.1.0, not yet certified). Binaries: `cmd/server` (`commercial-billing`), `cmd/jobs` (ten CronJobs), `cmd/migrate`, `cmd/certify` (not in the image). Helm chart 0.7.0 (Kubernetes ≥ 1.30, Istio ≥ 1.24 native sidecars, Helm ≥ 3.19).

**Owns:** its Postgres 17 database (RLS per tenant; roles `billing_app` and `billing_system`), the `billing.events` SNS topic (through the platform-events outbox), the provider ledger `payment_event_ledger`.

**Does NOT own:** tenant data and lifecycle (IAM Core / Org & Membership), realms (Realm Provisioner), quotas and eligibility (Entitlements), usage (Usage & Metering), card data (the provider: **PCI SAQ A**, no PAN ever reaches Billing).

**Namespace / identity:** namespace `billing`; ServiceAccount, pod label `app.kubernetes.io/name`, envelope `source` and metric prefix all exactly `commercial-billing` (`BIL-OPS-4`). Outbound identity to IAM is `commercial-system` (CD-19); inbound it is rejected (`BIL-AUTH-4`).

**Callers:**
- tenant routes B-P1…B-P26 (`/api/v1/tenants/:id/…`, through the gateway with CD-19 headers);
- internal B-I1 (Signup BFF only, mTLS principal from `X-Forwarded-Client-Cert`);
- webhook B-W1 `POST /api/v1/webhooks/:provider` (provider signature only);
- operator routes B-O1…B-O17 (`/api/v1/operator/…`; `403` until `OPERATOR_PATH_LIVE=true`, `BIL-AUTH-6`).

**Status:** Phases 0–8 are implemented. Phases 0–5 sit on the stacked branches `feat/billing-phase-0-scaffolding` … `feat/billing-phase-5-api`; phases 6–8 and all later work (platform-library routing, RFC 9457, observability standard, tests, CI/CD, and the two production-readiness review rounds of LLD rev 0.59 and 0.60) are on `feat/billing-phase-6-flows`. `main` holds only the LLD. Nothing is deployed to any environment. Go-live still waits on cross-team items (`docs/lld/billing-open-items.md`), the Stripe adapter 1.1.0 certification and the LLD §20.10 checks.

**LLD:** `docs/lld/commercial-lld-billing-service.md` (rev 0.70 at the time of writing; read the header) is the source of truth. **Implement what it says; don't invent endpoints, fields, events, statuses or config.** Open items and hand-offs to other teams: `docs/lld/billing-open-items.md` (there is no `docs/handoff/` directory any more). Invariant coverage: `docs/invariants-coverage.md`.

Detail lives in the topic files next to this one:
- `key-files.md`: per-file notes;
- `development-guide.md`: setup, tests, pitfalls;
- `api-and-events.md`: routes, outbound calls, events;
- `flows-and-concurrency.md`: state machines, locking, idempotency;
- `operations.md`: config, jobs, alerts, deployment.

---

## Common Commands

```bash
make docker-up        # Postgres :5634, PgBouncer :5633, floci (SNS/SQS/Glue) :4667, stripe-mock :12111
make migrate          # cmd/migrate against .env
make run              # go run ./cmd/server (sources .env)

make fmt / fmt-check  # gofmt
make vet
make lint             # go tool golangci-lint (v2, pinned in go.mod `tool`), default build + every test tag (integration, helm)
make arch-lint        # go-arch-lint (.go-arch-lint.yml, LLD §3.2)
make guards           # BIL-PCI-1 stripe imports, BIL-OPS-2 outbox access, RFC 9457 problem details, BIL-OPS-11 pgcommon

make test             # go test ./... -race (unit + contract, no Docker)
make test-integration # ./test/... ./internal/... ./cmd/... -tags=integration (testcontainers Postgres 17; Docker)
make test-contract    # ./test/contract/... (produced payloads vs consumers' schemas)
make cover

make schema-extract   # api/asyncapi.yaml → internal/adapters/eventbus/schemas/*.json (schema-gov 0.4, Docker)
make schema-check     # embedded schemas in sync with asyncapi.yaml
make schema-validate
make metrics-lint     # observability standard: metricslint on the scrape + every referenced metric registered/exposed + inventory
make metrics-inventory # regenerate docs/observability/metric-registry.md

make helm-check       # helm lint + kubeconform + promtool + chart contract tests (test/helm, tag helm)
make certify-rehearsal # cert-1.0 suite against stripe-mock (LLD §14.7)
make testclock        # Stripe Test Clock rehearsal against staging (docs/runbooks/test-clock-rehearsal.md)
make check-first-deploy ENV=<env>  # LLD §19.1 order, read-only

make build            # bin/commercial-billing, bin/jobs, bin/migrate, bin/certify
make openapi          # regenerate docs/swagger/openapi.json (openapi-check verifies)
make test-postgres | test-flows | test-bus | test-stripemock | test-e2e | test-helm   # one suite at a time
make cover-all / cover-gate                   # every suite incl. integration; gate ≥ 96% over internal/ + pkg/
make schema-register / schema-verify          # Glue billing-events (AWS or floci)
make docker-build / trivy                     # image build / CVE scan
make help            # every target, grouped
make ci               # fmt-check vet lint arch-lint guards test build
```

`docs/swagger/openapi.json` is generated by a golden test: `go test ./internal/adapters/http -run Golden -update`.

---

## Architecture Rules (go-arch-lint + guard scripts)

- `internal/domain` imports only the standard library (plus `platform-gincommon/v2/pkg/problem`, net/http only, for error types: LLD §3.2 rev 0.42).
- `app/port` → `domain`; `app` → `domain`, `port`.
- Inbound adapters (`http`, `webhook`, `consumer`) → `domain`, `port`, `app`, `metrics`.
- Outbound adapters (`postgres`, `eventbus`, `provider/*`, `iamclient`, `entitlementsclient`) → `domain`, `port` (+ `httpx` for HTTP clients). Adapters never import each other (`.go-arch-lint.yml` allows only the provider `registry` → `stripe`, `manual`).
- `pkg/requestctx` (request context: the caller identity type and the `X-Request-ID` carried to outbound calls) imports nothing from the project; only `http` and `httpx` may import it.
- Only `internal/bootstrap` and `cmd/*` wire adapters to ports.
- **`BIL-PCI-1`:** only `internal/adapters/provider/stripe` imports `github.com/stripe/*`; no provider type crosses a port (`check-stripe-imports.sh`).
- **`BIL-OPS-1` / `BIL-OPS-11`:** every pool, setting, statement and migration goes through platform-pgcommon. Drivers are denied everywhere (depguard `database`); pgcommon itself only in `adapters/postgres`, `bootstrap`, `certify` and `cmd/*` (depguard `database-boundary`). Classify DB errors with `pgcommon.Is*` and map them to domain codes or port errors (`port.ErrAccountExists`) inside `adapters/postgres`; no SQLSTATE literals, `SQLState()` interfaces or hand-written `set_config` / `SET LOCAL` (`check-pgcommon.sh`).
- **`BIL-OPS-2`:** events, the outbox, dead letters and `processed_events` only through platform-events (`events.NewEnvelope`, `outbox.EnqueueOrdered`, `inbox.Store.Process`), and no hand-written SQL on those tables outside migrations and tests (`check-outbox-access.sh`). platform-events only in `adapters/eventbus`, `adapters/postgres`, `adapters/consumer`, `bootstrap`, `certify` and `cmd/*`; use cases see `port.EventEnqueuer` / `port.InboxRunner` (depguard `events-boundary`). SNS and SQS SDKs are denied everywhere; the Glue SDK only in `adapters/eventbus`, for the encode codec platform-events doesn't ship yet.
- **`BIL-API-6`:** every error response comes from platform-gincommon (`gincommon.AbortWithProblem`, `ClassifyError`, `BindingError`; domain errors convert to `*problem.Error` with one `problem.Type` per §17 code). Problems from callees are read with gincommon's `problem.Decode` / `FromResponse`. No hand-written problem bodies, error DTOs or parsers, `c.JSON(4xx/5xx)`, `AbortWithStatus*`, `gin.Recovery` or `recover()` in HTTP adapters (`check-problem-details.sh`). The spec names "exception" types such as `ResourceNotFound`; gincommon's real API is `problem.NewNotFoundError`, `NewConflictError`, `NewBusinessRuleError`, `NewValidationError`, `MustType`.
- **`BIL-OPS-10`:** logs, metrics and traces only through platform-gincommon. depguard denies Prometheus imports in all production code and OpenTelemetry outside `adapters/observability`, plus `log`, `slog`, `zap`, `logrus` and `zerolog`; forbidigo denies `fmt.Print*`, `println` and `gin.Default` / `gin.Logger`. Metrics use `gincommon.CounterVec` etc. and `gincommon.New*Vec`.

---

## Critical Invariants (76 `BIL-*`; full list and coverage in `docs/invariants-coverage.md`)

Each invariant is referenced by ID in a comment at its enforcement point and has a test whose name contains the ID (e.g. `TestBIL_SEAT_3_…`). Update `docs/invariants-coverage.md` in the same change.

- **Money:** `domain.Money{AmountMinor int64, Currency}` everywhere; never `float64`. Fallback proration uses `math/big` and rounds half-up once at the end (§8.5.3, `BIL-SEAT-5`).
- **PCI (`BIL-PCI-1..3`, `BIL-API-4`):** no PAN, CVV, expiry, provider key or webhook secret in any table, log, event, span or error. `client_secret` only in the 3-D Secure action object returned to the caller that started the payment. The ledger payload is allow-listed.
- **`BIL-CONC-1`:** every write to a subscription, item, schedule, purchase or saga first takes the per-tenant advisory lock (`Service.locked`).
- **`BIL-CONC-2` / `BIL-OPS-3`:** no provider, IAM or Entitlements HTTP call inside an open transaction. Commit intent → call outside → commit outcome under the lock with a re-read.
- **`BIL-EVT-2` / `BIL-EVT-5`:** a state change and its event commit in one transaction through the ordered outbox keyed by `tenant_id`; nothing is published outside the outbox. Audit (AL-5) goes in the same transaction through the `AuditWriter` port.
- **`BIL-PAY-1..7`:** every provider callback is in `payment_event_ledger` before any domain write; bad signature/tolerance/livemode → `400`, never stored; tenant from Billing's customer mapping, not metadata; re-read with `FetchCurrent`, don't trust the payload; per-tenant order; one provider invoice settles at most one purchase; a purchase is granted only when the invoice's amount and currency match (else SEV-2 log `BillingPaymentParked`; `BIL-PAY-6`, `BIL-PAY-7`, `internal/app/purchase_invoice.go`).
- **`BIL-IDEM-1..3`:** a charging route stores its outcome before returning; provider idempotency keys derive from Billing ids only, never request input; records live 72 h.
- **`BIL-SAGA-1`:** RP-3 only after payment is confirmed server-side. **`BIL-OPS-5`:** a saga not confirmed 30 min after payment pages once (`BillingConversionSagaOverdue`).
- **`BIL-SUB-3`:** at most one non-`ended` subscription per tenant. **`BIL-SUB-4`:** status moves only along `domain/subscription.go`'s table. **`BIL-SUB-1`:** Billing never emits `suspended`, `trial_expired` or `offboarded`.
- **`BIL-RLS-1..3`:** every tenant table has `FORCE ROW LEVEL SECURITY`; `app.tenant_id` is transaction-local; `billing_system` (BYPASSRLS) pool only on the listed cross-tenant paths, never in tenant handlers.
- **`BIL-PROV-1..4`:** a provider takes new accounts only with a `passed` / `exempt` certification for its adapter version; `passed` only via B-O17 by a second `pci_owner_reviewer`.
- **`BIL-AUTH-*`:** identity only from headers (`x-user-id`, `x-tenant-id`, `x-tenant-roles`), never the body; one permission per tenant route; B-I1 only `cluster.local/ns/bff/sa/bff-signup`.

## Safe defaults (never flip in code)

`FEATURE_DIRECT_SIGNUP=false`, `FEATURE_TOKEN_ADDONS=false`, `USAGE_BILLING_CONSUMER_ENABLED=false`, `AUTO_REVERT_ON_EFW=false`, `AUTO_PAUSE_ON_OPERATOR_SUSPENSION=true`, `OPERATOR_PATH_LIVE=false`. Don't add workarounds around the operator `403` gate.

---

## Working rules

- **The LLD wins.** If code needs the design to change, propose the LLD change (revision row + status line) rather than diverging silently. If the LLD is ambiguous, stop and ask with section numbers.
- Every LLD revision gets a row in its revision history and the header `Status` line bumps to the new rev.
- Generated files are never hand-edited: `docs/swagger/openapi.json`, `internal/adapters/eventbus/schemas/*.json`, `docs/observability/metric-registry.md` (a hook blocks it).
- `.env` holds secrets and is never edited; change `.env-example`.
- One branch per phase/flow (`feat/billing-phase-<n>-<slug>`); never commit to `main`; production changes need a PCI owner group reviewer (HLD §15.4).
- Cross-team requests and hand-offs are recorded in `docs/lld/billing-open-items.md` (by team, plus the §7 tracker), not in separate files.
- Never commit binaries: build with `make build` (`bin/`); `.gitignore` ignores only the root-level `/server`, `/jobs`, `/migrate`, `/certify` and `/commercial-billing` (root-anchored, so `cmd/server/` stays visible).
