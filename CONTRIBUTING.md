# Contributing

`commercial-billing-service` is the Billing service of the XpertPMS Commercial domain. This guide covers development setup, how to extend the service, test requirements and the PR process.

Two documents govern every change:
- The LLD, [`docs/lld/commercial-lld-billing-service.md`](docs/lld/commercial-lld-billing-service.md), is the source of truth. **Implement what it says.** Don't invent endpoints, fields, events, statuses or configuration it doesn't define. If the code needs the design to change, propose the LLD change (with a revision row) first.
- [`docs/invariants-coverage.md`](docs/invariants-coverage.md) is the invariant ledger.

Billing moves money: the rules below exist to stop double charges, lost events, cross-tenant access and card data leaks.

## Prerequisites

- **Go 1.26.11+**, matching `go.mod`.
- **Docker**, for the integration suites (testcontainers Postgres 17, floci for SNS/SQS/Glue, stripe-mock) and for schema-gov.
- **golangci-lint** comes in as a Go tool (`go tool golangci-lint`, pinned in `go.mod`). There's nothing to install.
- **Private modules:** set `GOPRIVATE=github.com/BCBP-SOLUTIONS-FZC-LLC/*` and `GONOSUMDB` for the same prefix (the Makefile exports both). You also need an SSH key registered with the BCBP org, so `go mod download` can fetch `platform-gincommon`, `platform-pgcommon`, `platform-events` and `platform-audit`.
- **For `make helm-check`:** `helm` v3.16+, `promtool`, and `kubeconform` (installed by the script if missing).

## Development setup

```bash
git clone https://github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service
cd commercial-billing-service
make setup             # .env from .env-example (values match docker-compose.yml; never commit .env) + git hooks
make tidy
make docker-up         # Postgres 17 + PgBouncer + floci + stripe-mock
make migrate           # library schemas, then 000001–000006
make lint
make test              # unit, HTTP, contract, observability (no Docker)
make run               # API on :8180, metrics on :9190
```

To run the Docker suites (Postgres, flows, bus, stripe-mock, certification):

```bash
make test-integration
```

Host ports are offset from the IAM services' stacks so both can run side by side: direct Postgres `5634`, PgBouncer `5633`, floci `4667`, stripe-mock `12111`. Container-internal ports are unchanged.

## Git hooks

`make setup` (or `make install-hooks`) copies `.githooks/` into `.git/hooks/`. Re-run `make install-hooks` whenever `.githooks/` changes.

- **`pre-commit`** (about 15 s warm) runs the fast part of CI, stopping at the first failure:
  1. refuses a staged `.env`;
  2. `make tidy`, failing if it changed `go.mod` / `go.sum`;
  3. `fmt-check`;
  4. `lint`;
  5. `guards`;
  6. `openapi-check`.
- **`pre-push`** runs `make trivy`: it builds the image and fails on a CRITICAL, HIGH or UNKNOWN vulnerability that has a fix, the same check as CI's Trivy job.
  - It needs Docker and `trivy`.
  - A push that only deletes remote refs skips it.
  - To skip it once, use `SKIP_TRIVY=1 git push` or `git push --no-verify`.

The hooks cover formatting, lint and the guards. Run `make ci` (or the `/ci-local` Claude Code skill) before pushing for the tests and the rest of CI.

## Project layout

```
cmd/
  server/              ← Composition root: config, pools, providers, audit, event bus, router, background loops, shutdown
  jobs/                ← One binary, --job=<name>, the ten CronJobs of LLD §13.2
  migrate/             ← Helm pre-install/pre-upgrade hook: library schemas, then Billing's migrations
  certify/             ← Provider certification suite and release gate (not shipped in the image)
internal/
  domain/              ← Money, proration, transition tables, permissions, event payloads, §17 error catalogue
                         (standard library + gincommon/pkg/problem only)
  app/                 ← Use cases (one file per flow), authorizer, webhook processor, workers, jobs
  app/port/            ← Every interface: repositories, PaymentProvider, Core/RP/Entitlements clients,
                         EventEnqueuer, InboxRunner, AuditWriter, Tracer, Metrics, Logger
  bootstrap/           ← Composition shared by cmd/server, cmd/jobs and internal/certify
  config/              ← Every setting with its default and bounds (LLD §12)
  certify/             ← cert-1.0 cases, throwaway database, evidence
  adapters/
    http/              ← Route table (routes.go), router, guards, idempotency, binding, OpenAPI generation
    webhook/           ← B-W1: verify, ledger insert, 200
    consumer/          ← tenant-billing-q, usage-billing-q handlers
    postgres/          ← Repositories, tx runners, outbox enqueuer, inbox, audit writer, migrations/
    eventbus/          ← Embedded schemas (generated), ValidatingCodec, Glue encode codec
    provider/          ← Registry + certification gate; stripe/ (the only stripe-go importer); manual/
    iamclient/         ← Core I-11, I-18 · Realm Provisioner RP-3, RP-4, RP-11
    entitlementsclient/← Eligibility GET
    httpx/             ← Outbound HTTP: CD-19 headers, spans, dependency metrics, problem decoding
    metrics/           ← commercial_billing_* on gincommon's registry; Pushgateway push
    observability/     ← port.Tracer on gincommon's tracer
pkg/requestctx/        ← RequestContext{UserID, TenantID, Roles, ClientIP, UserAgent}
api/asyncapi.yaml      ← AsyncAPI 3.0 — the source of every event schema
docs/swagger/openapi.json ← OpenAPI 3.1, generated from the route table (TestOpenAPIGolden)
deploy/helm/, deploy/monitoring/ ← Chart, alerts, recording and SLO rules, dashboard
test/                  ← Suites that cross packages: postgres, flows, bus, stripemock, certify, contract, helm
                         (unit tests live beside the code in internal/**/_test.go)
```

**Dependency rule:** `domain` ← `port` ← `app` ← adapters ← `bootstrap` / `cmd`.
- Adapters never import each other; the exception is the provider registry, which imports `stripe` and `manual`.
- Inbound adapters may import `metrics`.

CI enforces it with `go-arch-lint` (`.go-arch-lint.yml`, `make arch-lint`). The vendor-library rules are under [Guardrails](#guardrails-and-forbidden-patterns).

For the full picture see [`ARCHITECTURE.md`](ARCHITECTURE.md) and [`.claude/key-files.md`](.claude/key-files.md).

## Extending the service

### Adding domain logic

1. Put the type or rule in `internal/domain/`, using only the standard library.
2. **Money** is `domain.Money{AmountMinor int64, Currency}`. Never use `float64`. Prorated amounts use `math/big` and round half-up once, at the end (`BIL-SEAT-5`).
3. **State changes** go through the transition table of their aggregate (`subscription.go`, `saga.go`, `seat_purchase.go`, `plan_change.go`, …). Add the transition to the table, never an ad-hoc status write. Test the allowed and the forbidden moves.
4. **New errors** are a code in the §17 catalogue (`errors.go`, with its HTTP status). It becomes a gincommon problem type automatically. Add it to LLD §17 in the same change; codes are frozen once published.

### Adding a use case

1. Add the port method in `internal/app/port/` if it needs a new repository or outbound capability.
2. Implement it in `internal/app/`, in the file for its flow. The write pattern is fixed:
   - Take the per-tenant lock with `Service.locked` for any write to a subscription, item, schedule, purchase or saga (`BIL-CONC-1`).
   - **Never make an HTTP call** (provider, Core, Realm Provisioner, Entitlements) while a transaction is open (`BIL-CONC-2`):
     1. commit the intent;
     2. call outside the transaction;
     3. commit the outcome under the lock, after a re-read.
   - The state change, its event (`port.EventEnqueuer`) and its AL-5 audit entry (`port.AuditWriter`) commit in **one** transaction (`BIL-EVT-2`).
   - Provider idempotency keys come from Billing ids only (`bsub:<id>:…`, `seatpur:<purchase_id>`, …, §9.2), never from request input (`BIL-IDEM-2`).
3. Unit-test it with fakes beside the code. Add a `test/flows` integration test for the flow and its failure branches.

### Adding an HTTP endpoint

1. **Check the LLD first.** The endpoint must be in the §5.3 catalogue with an ID (`B-P*`, `B-I*`, `B-O*`) and specified in §5.4: path, method, permission, idempotency rule, errors.
2. **Add one row to `internal/adapters/http/routes.go`:** ID, method, path, idempotency rule (`idemRequired` / `idemOptional` / `idemNone`) and handler. The router mounts it on the right group with that group's guards:
   - tenant: path-tenant match, then permission;
   - operator: `platform_operator` and the `OPERATOR_PATH_LIVE` gate;
   - internal: the XFCC principal.

   Authorization always runs before any database access.
3. **Tenant routes:** add exactly one permission to `domain.TenantRoutePermission` (`BIL-AUTH-7`). The role × route matrix test covers it automatically.
4. **Errors** are returned as `domain.NewError(code, message)`. Never write a response body for an error (`BIL-API-6`, enforced by `check-problem-details.sh`).
5. **Feature-flagged routes:** add the ID to `disabledRoutes` in `cmd/server/wiring.go`.
6. **Regenerate the OpenAPI golden** and commit it: `go test ./internal/adapters/http -run Golden -update`.
7. **Operator routes:** add the path to the operator `HTTPRoute` in `deploy/helm/templates/httproutes.yaml`. `test/helm` and `helm_routes_test.go` assert the list.
8. **Update** the README API tables.

### Adding a migration

1. Add `NNNNNN_description.up.sql` and `.down.sql` to `internal/adapters/postgres/migrations/`, numbered after the last one (`000006`). **Never edit or renumber an existing migration.** They are embedded and applied in order, and Billing's history is tracked in `billing_schema_migrations`.
2. **Tenant-scoped tables:**
   - `ENABLE` + `FORCE ROW LEVEL SECURITY`;
   - the `tenant_isolation` policy using `rls_check_tenant(tenant_id, '<table>')`;
   - grants to `billing_app`, and `SELECT` to `admin_readonly`.

   `TestBIL_RLS_1_EveryTenantTableForcesRLS` fails on any tenant table without it.
3. **Global tables** (no tenant) are written only through `billing_system`. Say so in the LLD, and keep tenant handlers away from the sysPool (`BIL-RLS-3`).
4. **Optimistic locking:** a table that takes client `record_version` gets `record_version bigint NOT NULL DEFAULT 1` and the `touch_row()` BEFORE-UPDATE trigger (`TestTouchRowTrigger`). Updates filter on `record_version`; client code never sets it.
5. **Constraints and triggers:** every new CHECK, unique index, exclusion constraint or trigger that carries an invariant gets a test proving it rejects bad data (`test/postgres/constraints_test.go`).
6. **Two-release safety** (LLD §13.4, HLD §15.5):
   - changes are backward-compatible with the previous release, since the hook Job runs before new replicas;
   - destructive changes ship in a later release than the last code that reads the object;
   - indexes on large tables use `CONCURRENTLY` (outside a transaction);
   - a new FK or CHECK on a populated table is `NOT VALID` first, then `VALIDATE` later.

   Production migrations need the `production-data-migrations` approval.
7. **Never rewrite `payment_event_ledger` payloads,** and never add a column for card data, provider keys or webhook secrets (`BIL-PCI-2`).
8. **Library tables:** never touch platform-events' (`outbox_events`, `outbox_dead_letters`, `processed_events`) or platform-audit's tables in a Billing migration, beyond the grants and RLS that §4.4 lists.
9. **Verify:** `make migrate`, then `make test-integration` (`TestMigrationsAppliedAndIdempotent` applies the full set).

### Adding a domain event

1. **Check the LLD first.** The event must be in §7.3, with its trigger, payload and consumers.
2. Add the message and payload schema to `api/asyncapi.yaml`, then `make schema-extract`. Commit the regenerated `internal/adapters/eventbus/schemas/*.json`; never edit them by hand. Schemas are **open**: additive changes only, and consumers tolerate unknown fields.
3. Add the payload type and its `Validate()` to `internal/domain/events.go`, and add the type to `ProducedEventTypes`.
4. Enqueue it through `port.EventEnqueuer` inside the state change's transaction. The enqueuer builds the platform-events envelope and calls `outbox.EnqueueOrdered`, keyed by tenant.
5. **Test it:** add a sample to `test/contract` so the payload is validated against Billing's schema and every consumer's vendored schema. If a new consumer subscribes, vendor its consumed schema under `test/contract/testdata/consumers/<service>/` and record the commit in that README.
6. **Register the schema** in Glue `billing-events`, in each environment, **before** the deploy. The server resolves schema versions at startup and refuses to start ahead of the registry.
7. **Update** the README event table, `scripts/init-floci.sh` (local subscriber queues and schemas), and LLD §7.3.

### Adding a consumed event

1. Add the consumed schema to `api/asyncapi.yaml` and run `make schema-extract`. Add the event to the queue's SNS filter in `scripts/init-floci.sh` and in the IaC request.
2. Handle it in `internal/app` (for example `lifecycle_consumer.go`), behind `port.InboxRunner`, so the envelope `id` is claimed inside the handler's own transaction. IAM lifecycle events follow the recency rule (`BIL-EVT-3`).

### Adding an outbound HTTP call

1. Use `httpx.New(dependency, timeout)` and `httpx.Call`. Together they give:
   - the CD-19 `commercial-system` headers;
   - spans and W3C propagation;
   - `platform_dependency_request_seconds`;
   - problem decoding.
2. **Timeout:** a `*_TIMEOUT_MS` setting with bounds in `config.go`, and a value in Helm.
3. **Failure behaviour:** decide fail-open or fail-closed with the LLD (§9.5) and document it in the README's dependency table.
4. **Network:** add the egress to the NetworkPolicy and Istio policies, limited to the routes used, as was done for Entitlements.

### Adding a configuration key

1. **Define it in `internal/config/config.go`** with a default and bounds (`intIn`, `durIn`, `oneOf`, …). `config.Load()` must reject a bad value at startup. Secrets use `secret()`.
2. **Add it to every copy of the configuration:**
   - `deploy/helm/values.yaml`, and the environment values files if they differ;
   - `.env-example` if a local run needs it;
   - LLD §12;
   - the README's environment table.
3. **Feature flags default off,** except where §12 says otherwise. Never switch one on in code or in default values.

### Adding a metric, alert or CronJob

1. **Metrics:**
   - Tier 3 names are `commercial_billing_*`, built with gincommon's constructors in `internal/adapters/metrics`.
   - Never invent a `platform_*` or `commercial_*` name; propose it in `docs/observability/registry-proposals.md`.
   - Run `make metrics-inventory` and commit the inventory.
   - `make metrics-lint` must pass.
2. **Alerts** go in `deploy/monitoring/app-alerts.yml` and its chart copy `deploy/helm/files/app-alerts.yml` (`helm-check` compares them). Give each a `runbook_url` to a runbook in `docs/runbooks/`.
3. **CronJobs:** add the job to `cmd/jobs/main.go` (name, schedule, use case) **and** `deploy/helm/templates/cronjobs.yaml` (`test/helm` asserts they match), and record it in LLD §13.2. A job never deletes or minimises a financial record (`BIL-OPS-9`).

## Testing requirements

| Layer | Location | Docker | Notes |
|---|---|---|---|
| Unit | `internal/**/_test.go` | No | Domain tables, proration golden tests, use cases with fakes, adapters with `httptest` |
| HTTP | `internal/adapters/http` | No | Every route's guards, idempotency and error codes; RFC 9457 on every error path; OpenAPI golden |
| Contract | `test/contract` | No | Produced payloads vs Billing's and consumers' schemas; Entitlements decode and producer guards |
| Observability | `internal/adapters/metrics` | No | metricslint on the scrape, metric references, inventory |
| Postgres | `test/postgres` (`-tags=integration`) | Yes | RLS, constraints, triggers, repositories, ledger claims, outbox, inbox |
| Flows | `test/flows` (`-tags=integration`) | Yes | Every §8 flow with its failure branches |
| Bus | `test/bus` (`-tags=integration`) | Yes (floci) | Framed publish, ordered delivery, dedup, DLQ |
| Stripe | `test/stripemock` (`-tags=integration`) | Yes (stripe-mock) | The Stripe adapter |
| Certification | `test/certify` (`-tags=integration`) | Yes | `cert-1.0` end to end |
| End to end | `test/e2e` (`-tags=integration`) | Yes | The production router and middleware over HTTP on the real `Service` and Postgres 17; only outbound calls are faked (`test/testkit`) |
| Helm | `test/helm` (`-tags=helm`) | No (helm) | Chart contract |

- `make test` runs the Docker-free layers with `-race`; it is what CI runs.
- `make test-integration` runs the rest.
- Every behaviour change needs a test in the right layer, and a `test/flows` test when it changes a flow.

### Invariant tests

Every `BIL-*` invariant has:
- a comment with its ID at the enforcement point;
- at least one test whose **name contains the ID**, for example `TestBIL_SEAT_3_RenewalSeats` or `TestBIL_PAY_5_ClaimOrderAndRetry`.

When you implement, change or test an invariant, update its row in [`docs/invariants-coverage.md`](docs/invariants-coverage.md) (enforced in, test, status) in the same PR.

### Canonical test cases

These tests carry the service's money, security and isolation guarantees. Don't weaken or delete them without a design conversation:

- **`TestBIL_RLS_1_EveryTenantTableForcesRLS`**, **`TestRLSIsolationEveryTable`**, **`TestRLSWithCheck`** (`test/postgres`): every tenant table forces RLS; a tenant can't read or write another's rows; a missing or invalid `app.tenant_id` sees nothing (§14.5).
- **`TestBIL_SUB_3_OneLiveSubscription`**, **`TestBIL_SEAT_1_and_2_OneOpen`** (`test/postgres`): the partial unique indexes behind one live subscription and one open purchase or schedule. They hold even if a code path forgets the lock.
- **`TestBIL_PROV_4_CertificationGuard`** (`test/postgres`): `trg_pc_guard` and `chk_pc_two_people`. A certification can't pass without a second person, and records are immutable.
- **`TestBIL_PAY_5_ClaimOrderAndRetry`** (`test/postgres`): webhook rows are claimed per tenant in provider order.
- **`TestIdempotencyReclaimAndUnlock`** (`test/postgres`) and **`TestB_I1`** (`internal/adapters/http`): one charge per key, `409 REQUEST_IN_PROGRESS` in flight, the B-I1 key rules and the 3-D Secure retry.
- **`TestEventEnqueuerAtomicity`** (`test/postgres`): a rolled-back business transaction leaves no outbox row.
- **`TestBIL_SEAT_5_ProrationGoldenTable`** (`internal/domain`): the LLD §14.1 golden table, including the below-minimum deferral. If the table changes, the LLD changes first.
- **`TestBIL_AUTH_RoleRouteMatrix`**: every role × tenant route pair.
- **`TestEveryErrorIsAProblem`**: RFC 9457 on every error path.
- **`TestOpenAPIGolden`**: the committed OpenAPI equals the route table.
- **`test/contract`**: produced payloads against every consumer's schema.
- **`TestScrapePassesMetricsLint`**, **`TestReferencedPlatformMetricsAreRegistered`**: the observability standard.
- **`test/e2e`:**
  - `TestE2E_TenantIsolation`, `TestE2E_RolePermissions`, `TestE2E_CommercialSystemRejectedInbound`, `TestE2E_OperatorGate`, `TestE2E_DirectSignupCallerAndKey`, `TestE2E_WebhookSignatureAndLedger`: the security guards over real HTTP;
  - `TestE2E_DirectSignupConcurrentDuplicates`: eight concurrent B-I1 calls with one key, exactly one charge;
  - `TestE2E_EveryCatalogueRouteIsMounted`: every endpoint ID is on the router.

  Each security test holds a control case that would pass without its guard; keep it when you change one.

Run a single test:

```bash
go test ./internal/adapters/http -run TestB_I1 -count=1 -v
go test ./internal/domain -run TestBIL_SEAT_5 -count=1 -v
go test ./test/postgres -tags=integration -run TestRLSIsolationEveryTable -count=1 -v
go test ./test/flows -tags=integration -run TestConversionFlow -count=1 -v
```

### Coverage

Coverage is **98.7%** of statements over every package and **99.2%** over `internal/` and `pkg/`, with all suites (`make cover-all`). `make cover-gate` fails below 96% (`COVERAGE_MIN`) over `internal/` and `pkg/`, the scope the IAM services gate. Run it after `make cover-all` before a PR; CI enforces the same gate (`validate-test.yml`). LLD §14.6 targets:
- ≥ 90% merged statement coverage at first release (95% later);
- 100% of the transition tables, the proration calculator and the permission map.

The suites that carry the coverage:
- `test/scenarios/{jobs,lifecycle,commerce}`: the use cases, with fault injection on repositories, events, audit and the provider;
- in-package provider tests against a fake Stripe API;
- `internal/certify` and `cmd/*` integration tests;
- the adapter and domain unit tests.

When you add code, add its tests in the same layer.

```bash
make cover   # profile + total
```

Aim coverage where it matters: transition tables, money arithmetic, guards, webhook processing, saga branches and failure paths.

### Testcontainers note

The `integration` suites start their own Postgres 17, floci and stripe-mock containers. Docker must be running, and `make docker-up` is not required for them. On macOS the Makefile points `DOCKER_HOST` at the Docker Desktop socket.

### PgBouncer note

Deployed app pools run behind PgBouncer in transaction mode with the simple protocol (`PG_BOUNCER_MODE=true`). `app.tenant_id` is bound transaction-locally by pgcommon's `GUCProvider` on every checkout, so nothing session-scoped may be set; see the guardrails. The local `.env-example` connects the app pool directly to Postgres while keeping `PG_BOUNCER_MODE=true`, so the protocol matches production.

## Linting

```bash
make lint        # golangci-lint, default build and every test build tag (integration, helm)
make arch-lint   # go-arch-lint
make guards      # the four guard scripts
```

**`.golangci.yml`** enables the standard set plus:
- `bodyclose`, `exhaustive`, `nilerr`, `noctx`, `revive`, `unconvert`, `unparam`;
- `depguard` (library boundaries);
- `forbidigo` (pool construction, `fmt.Print*`, `gin.Default` / `gin.Logger`).

Give every new package a package doc comment, and every exported symbol a godoc comment.

## Guardrails and forbidden patterns

CI enforces each of these:

| Never | Instead | Enforced by |
|---|---|---|
| Card data (PAN, CVV, expiry), provider keys or webhook secrets in a table, log, event, span, error or response | Provider tokens and ids only; `client_secret` only in the 3-D Secure action (`BIL-PCI-*`, `BIL-API-4`) | Review, `invariant-reviewer` agent, ledger allow-list |
| Import `github.com/stripe/*` outside `adapters/provider/stripe` | The `PaymentProvider` port (`BIL-PCI-1`) | `check-stripe-imports.sh` |
| Make an HTTP call inside a transaction | Commit intent → call → commit outcome under the lock (`BIL-CONC-2`) | Review, flow tests |
| Write a subscription, purchase or saga without the tenant lock | `Service.locked` (`BIL-CONC-1`) | Review, flow tests |
| Publish an event outside the outbox, or SQL on `outbox_events` / `outbox_dead_letters` / `processed_events` | `port.EventEnqueuer`, platform-events (`BIL-OPS-2`) | `check-outbox-access.sh`, depguard `events*` |
| Import `pgx`, `pgconn`, `database/sql`, an ORM or a migration tool; use pgcommon outside `adapters/postgres`, `bootstrap`, `certify`, `cmd`; compare SQLSTATE literals; `set_config` / `SET LOCAL` by hand | pgcommon pools, `RunInTx*`, `Is*` helpers, `GUCProvider` (`BIL-OPS-1`, `BIL-OPS-11`) | depguard `database`, `database-boundary`; forbidigo; `check-pgcommon.sh` |
| Hand-write an error response, problem model, error DTO, problem parser or recovery | `domain.NewError` → gincommon; `problem.Decode` to read one (`BIL-API-6`) | `check-problem-details.sh` |
| Import Prometheus, or OpenTelemetry outside `adapters/observability`; log with `log`, `slog`, `zap`, `fmt.Print*` | gincommon's metric types, tracer and logger (`BIL-OPS-10`) | depguard `observability`, `metrics`; forbidigo |
| Trust a tenant, user or role from a request body | CD-19 headers or XFCC (`BIL-API-1`) | `TestValidation`, `TestTenantGuards` |
| Derive a provider idempotency key from request input | Billing ids (`BIL-IDEM-2`) | Review, flow tests |
| Apply a webhook from its payload, or resolve its tenant from metadata | Ledger first, `FetchCurrent` re-read, customer mapping (`BIL-PAY-*`) | `TestB_W1`, webhook processor tests |
| Turn on `FEATURE_DIRECT_SIGNUP`, `FEATURE_TOKEN_ADDONS` or `AUTO_REVERT_ON_EFW`, or work around the operator `403` gate | Flags flip per environment in Helm, by decision | Review, `test/helm` |
| Edit generated files (`docs/swagger/openapi.json`, `eventbus/schemas/*.json`, `docs/observability/metric-registry.md`) | Regenerate them | Golden and drift tests, `schema-check`; a Claude Code hook blocks the edit |
| Commit `.env` or any secret | `.env-example`; `ExternalSecret` in Kubernetes | `.gitignore`, review |

## Documentation update checklist

| What changed | Documents to update |
|---|---|
| Design or behaviour | LLD section + a revision row at the top + the header `Status` line (rev bump) |
| HTTP endpoint | `routes.go` · OpenAPI golden (`-update`) · README API tables · `.claude/api-and-events.md` · Helm `httproutes.yaml` (operator) |
| Error code | `internal/domain/errors.go` · LLD §17 |
| Produced event | `api/asyncapi.yaml` · `make schema-extract` · `internal/domain/events.go` · `test/contract` sample · `scripts/init-floci.sh` · README event tables · LLD §7.3 |
| Configuration key | `config.go` · Helm values · `.env-example` · README environment table · LLD §12 · `.claude/operations.md` |
| Migration or table | LLD §4 · `ARCHITECTURE.md` data model (new table) · `CHANGELOG.md` |
| Flow or concurrency | `ARCHITECTURE.md` sequence diagram · `.claude/flows-and-concurrency.md` |
| Outbound dependency | `ARCHITECTURE.md` · README dependency tables · NetworkPolicy / Istio policy · `contract-checker` agent table |
| Metric, alert, runbook, CronJob | `make metrics-inventory` · LLD §11 / §13.2 · `docs/runbooks/` · chart copies |
| Invariant | `docs/invariants-coverage.md` |
| Cross-team item | `docs/lld/billing-open-items.md` · LLD §16.1 |
| Anything user-visible | `CHANGELOG.md` `[Unreleased]` — dated entry naming the LLD revision |

The `docs-consistency-reviewer` Claude Code agent checks a diff against this list.

## Branch naming

| Form | Use |
|---|---|
| `feat/billing-phase-<n>-<slug>` | Implementation phases 0–8 |
| `feat/<topic>` | New capability outside a phase |
| `fix/<topic>` | Bug fix |
| `docs/<topic>` | Documentation and LLD only, e.g. `docs/billing-lld-rev-0.14-signoff` |
| `chore/<topic>` | Dependencies, tooling, CI, refactors with no behavioural change |

**Base branch:** the phase branches are stacked (each phase on the previous one), and `main` holds only the LLD until they merge. Base new work on the current phase branch, not `main`, and say which one in the PR.

**Never** commit to `main`, force-push a shared branch, or merge your own PR.

## Pull request checklist

- [ ] The change matches the LLD; any design change has an LLD revision row and a bumped `Status` line
- [ ] `make ci` passes locally: `fmt-check` + `vet` + `lint` + `arch-lint` + `guards` + `test` (race) + `build`
- [ ] `make test-integration` passes if the change touches Postgres, flows, the bus, the provider or certification
- [ ] `make helm-check` passes if the change touches `deploy/` or adds a route, CronJob, alert or setting
- [ ] Generated files regenerated and committed: OpenAPI golden, `make schema-extract`, `make metrics-inventory`
- [ ] Every touched invariant has a test named with its ID and an updated row in `docs/invariants-coverage.md`
- [ ] No new cross-tenant path; RLS tests updated for any new tenant table
- [ ] No card data, secret or personal data in logs, events, errors or new columns
- [ ] Feature flags and the operator gate untouched
- [ ] Documentation update checklist reviewed; `CHANGELOG.md` `[Unreleased]` updated
- [ ] The PR description lists the LLD sections implemented, the invariants covered, open questions and what was deliberately left out
- [ ] Production-bound changes: a PCI owner group reviewer is requested (HLD §15.4); a provider adapter change has a new certification run (`BIL-PROV-2`)

## Commit style

Keep subjects short, imperative and ≤ 72 characters, with a conventional prefix. The body explains the *why*, names the LLD section or invariant, and ends with the attribution line your environment requires. Examples from this repository:

```
feat: Phase 6d — seat purchase and scheduled reduction (B-P6–B-P11)
fix: a reverted tenant can convert again (LLD rev 0.46)
refactor: route events, outbox and processed-message dedup through platform-events
test: Entitlements contract test for TenantQuotaAddOnChanged
docs: LLD rev 0.48 — BL-OQ-27 (a) merged; direct-signup request withdrawn
chore: add Claude Code project setup (.claude)
```

```
fix: a reverted tenant can convert again (LLD rev 0.46)

RP-4 gives the tenant a new trial, but Billing does not consume
TrialReactivated, so iam_lifecycle stayed active and B-P2 / B-O1 refused
reconversion. B-O8's final commit now sets iam_lifecycle = 'trial' and
advances iam_last_event_at to the commit time (BIL-EVT-3).
```

An LLD-only change is `docs: LLD rev X.YY — <what changed>`. A code change that implements a design change cites the revision in its subject or body.

## Deployment

Nothing is deployed yet. The pipeline (`.github/workflows/`, README "CI"):

1. **On every PR:**
   - `validate-test`: race tests, every suite with coverage ≥ 96%, arch-lint, OpenAPI drift, schema sync;
   - `validate-quality`: fmt, tidy, vet, lint, guards, metrics-lint, govulncheck, digest pins, Helm;
   - the image build, Trivy, smoke tests and a PR summary comment;
   - the changelog check, and the schema registry validation when schemas change.
2. **On merge to `main`:**
   - the image is pushed to GHCR, signed with Cosign and verified;
   - changed schemas are registered in staging Glue.
3. **On a `v*` tag,** `release.yml` runs:
   - the CHANGELOG and tag checks;
   - the **provider-certification gate**: a change under `internal/adapters/provider/<name>/`, or to the pinned provider API version, bumps `AdapterVersion()` and fails the release until `certify check` finds a passed record in staging;
   - the production schema check;
   - the signed image;
   - the deploy gate with automatic rollback;
   - the GitHub Release.

4. **Rollout:**
   - Helm applies the chart per environment, namespace `billing`;
   - the migrate hook Job runs before new replicas;
   - the CronJobs use the same image with `/jobs --job=<name>`.
5. **First deployment** follows LLD §19.1, checked with `make check-first-deploy ENV=<env>`. Glue schemas must be registered before the deploy.
6. **Production** changes need a PCI owner group reviewer (HLD §15.4). Migrations need the `production-data-migrations` approval.
6. **Coordinate with the owning team** before changing:
   - a contract other services consume: an event payload, B-I1, or the Entitlements header contract;
   - a flag's per-environment value;
   - `SEATS_AFTER_CONVERSION_DELAY` (Core relies on it);
   - `MAX_LIFECYCLE_EVENT_SKEW_SECONDS` (it must match Core's clamp).

   Record the change in [`docs/lld/billing-open-items.md`](docs/lld/billing-open-items.md).
