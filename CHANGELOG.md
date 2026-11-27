# Changelog

All notable changes to this service are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versioning follows SemVer.

This service has never been deployed to any environment, and there is no released version yet, so everything is under `[Unreleased]`. Each entry names the date and the LLD revision that records it ([`docs/lld/commercial-lld-billing-service.md`](docs/lld/commercial-lld-billing-service.md), revision history at the top). The LLD keeps the full decision history, and `git log` the commit-by-commit build. The first entry below describes the service as it exists, and the entries after it record changes since the build.

## [Unreleased]

### Added (2026-11-27 — deploy references)

- **`deploy/iam/`** (`README.md`, `policy.json`, `policy.tf.example`): the least-privilege IRSA policy for `system:serviceaccount:billing:commercial-billing`.
  - **Grants:** SNS publish to `billing-events`; SQS consume on `tenant-billing-q` / `usage-billing-q`; `SendMessage` to their DLQs only; Glue read on the `billing-events` registry.
  - **Explicitly out of scope:** S3, KMS, Secrets Manager, CloudWatch, and Glue writes, which belong to the CI principal.
- **`deploy/monitoring/prometheus-adapter-rule.yaml`:** the External Metrics rule the HPA's `commercial_billing_webhook_backlog` metric needs. It takes `max` across pods, since every pod reports the same backlog.
- **`deploy/monitoring/schema-registry-alerts.yml`** and runbook `docs/runbooks/schema-registration.md`: four `sev3` alerts on the Glue registration pipeline (`BillingSchemaRegistrationFailed`, `…BreakingChangeBlocked`, `…RegistrationSpike`, `…VersionChurn`).
  - `schema-registry.yml` now records the diff's breaking count and runs `schema-gov metrics`, which pushes to the Pushgateway when `PUSHGATEWAY_URL` is set.
  - `helm-check` runs promtool on the file.
- **`deploy/monitoring/dashboards/platform-audit.json`:** the shared "Platform audit delivery" dashboard, templated on `service`. Its recording rules are in `platform-audit.rules.yml`.

### Added (2026-11-27 — CI/CD pipeline)

- **CI/CD pipeline** (`.github/`), modelled on the IAM services'.
  - **`ci.yml`:**
    - reusable `validate-test.yml`: race tests, every suite with the ≥ 96% coverage gate, arch-lint, OpenAPI drift, schema sync;
    - reusable `validate-quality.yml`: fmt, tidy, vet, lint, guards, metrics-lint, govulncheck, Dockerfile digest pins, Helm;
    - the image: Hadolint, a cached build, Trivy with SARIF and a CycloneDX SBOM, smoke tests;
    - a PR summary comment;
    - on `main`, a GHCR push signed and verified with Cosign.
  - **`release.yml`:** tag and CHANGELOG checks, Linux binaries, the **provider-certification gate** (LLD §14.6, `BIL-PROV-2`), the production Glue schema check, a signed semver image with SBOM and provenance, a Helm deploy gate with a 5xx check and rollback, and the GitHub Release.
  - **`schema-registry.yml`:** validate, diff against the registry (fails on a breaking change), register in Glue `billing-events`, verify.
  - **`changelog-check.yml`.**
  - **Scripts:** `.github/scripts/*`.
  - **Repository files:** `CODEOWNERS`, a PR template, `SECURITY.md`, issue templates, `.trivyignore`.
- The `Dockerfile` base images are pinned by digest. The runtime image moves from `distroless/static-debian12` to **`static-debian13`**: the newest Debian 12 build still carries fixed `ca-certificates` and `tzdata` advisories (DLA-4845-1, DLA-4792-1, DLA-4854-1, DLA-4867-1), which fail the Trivy gate. The binaries trust that CA bundle for TLS to Stripe, IAM and AWS.
- **Git hooks** (`.githooks/`, installed by `make setup` / `make install-hooks`).
  - `pre-commit`: refuses a staged `.env`, then runs tidy, fmt-check, lint, guards and openapi-check.
  - `pre-push`: `make trivy`. Skip it once with `SKIP_TRIVY=1`.

### Fixed (2026-11-27 — five bugs found by the coverage suites, LLD rev 0.58)

- **Concurrent first checkout or signup failed instead of continuing** (`app/conversion.go`, `app/direct_signup.go`).
  - **Cause:** the account was inserted without re-checking under the tenant lock. The losing request then "recovered" from the unique violation with a read on the transaction the violation had aborted.
  - **Fix:** the account is now re-read under the lock first.
    - A losing B-P2 continues like a second checkout: the earlier incomplete subscription is abandoned (§8.1.3).
    - A losing B-I1 answers `409 SUBSCRIPTION_ALREADY_EXISTS` with the winner's id.
    - A violation that can still occur (a writer outside the lock) is a retryable `409 CONCURRENT_UPDATE`.
  - **Tests:** `TestStartCheckoutConcurrentFirstCheckout` (which had asserted the failure) and the new `TestDirectSignupAccountCreatedConcurrently`.
- **Stripe `409` treated as permanent** (`provider/stripe/adapter.go`). An in-flight request with the same idempotency key (`409`, `idempotency_key_in_use`), which stripe-go retries, was mapped to `ErrIdempotencyMismatch`, a terminal error that pages. It now maps to `ErrProviderUnavailable`, so the caller retries with the same key. LLD §18.8 is corrected.
- **Dispute and fraud-warning lookup error dropped** (`provider/stripe/api.go`). A failed invoice-payments lookup is now returned, so the callback is retried instead of being applied without its invoice. That miss would have skipped the CD-10 conversion-payment check.
- **`VerifyPrice` ignored `interval_count`.** A 3-monthly price is now refused for a monthly plan (B-O11, B-O12).
- **Certification `decline` case panicked** (`certify/cases.go`) on a nil evidence map when the signup failed. It now reports the signup error; its test now asserts the real error instead of the panic.

### Added (2026-11-27 — statement coverage from 74.2% to 98.7%)

- **Coverage:** 98.7% of statements over every package (8,661 of 8,774), and 99.2% over `internal/` and `pkg/` (from 74.2% and 78.2%), measured with every suite together (`make cover-all`).
- **New `make cover-gate`:** fails below `COVERAGE_MIN=96` over `internal/` and `pkg/`.
- **`make test-integration` and `cover-all`:** now include the integration-tagged tests that live beside the code (`internal/...`, `cmd/...`). They run at most 4 packages at once (`INT_PARALLEL`), because every package starting its own containers at the same time overwhelmed an 8 GB Docker.
- **New suites:**
  - `test/scenarios/jobs` covers CronJobs, workers, the price book and certification.
  - `test/scenarios/lifecycle` covers conversion, direct signup, cancellation, dunning and suspension.
  - `test/scenarios/commerce` covers refunds, disputes, reversal, plan change, seats, add-ons, tenant reads, operator views and reconciliation.

  All three use the real `Service` on Postgres 17 with fault injection on repositories, events, audit, the lock and the provider. They assert database state, money amounts, provider calls and idempotency keys, outbox events and audit rows.
- **Further tests:**
  - in-package provider tests against a fake Stripe API: request shapes, idempotency keys, every error mapping;
  - `internal/certify` and `cmd/{certify,jobs,migrate,server}` tests, including a full `certify run` on stripe-mock and a `run()` of the server through its lifecycle;
  - adapter, domain, config and bootstrap unit tests;
  - more `test/postgres` cases.
- **Remaining uncovered statements (113):**
  - mostly `cmd/server` start-up failure paths, which need a separate process because gincommon's metrics registry is process-wide;
  - code that can only run with a live Stripe account;
  - defensive branches that database constraints make unreachable.
- **Bugs the new tests found (fixed the same day; see Fixed, LLD rev 0.58):**
  - recovery after a concurrent first checkout or signup runs on an aborted transaction (`app/conversion.go:155`, `app/direct_signup.go:132`, `:155`);
  - the Stripe adapter maps HTTP 409 (request in progress) to a terminal error (`provider/stripe/adapter.go:125`);
  - the dispute and fraud-warning payment lookup drops a transient error (`stripe/api.go:668`);
  - `VerifyPrice` ignores `interval_count` (`stripe/api.go:895`);
  - the certification decline case panics on a nil map when signup fails (`certify/cases.go:830`). Its test asserts the panic, with a comment.

### Added (2026-11-27 — end-to-end suite over HTTP)

- **New `test/e2e`** (tag `integration`, `make test-e2e`, also run by `make test-integration`). It drives the production router and gincommon middleware chain, the tenant, internal, operator and webhook route groups (wired as `cmd/server` wires them), the real `app.Service` and repositories, on Postgres 17 through the production roles. Only the outbound world is faked. 26 tests:
  - **Error shape:** every failure kind answers an RFC 9457 problem; `required_permission` is named; validation lists fields without echoing values; the 64 KiB body limit; `415` on non-JSON.
  - **Security:**
    - tenant isolation (path tenant and RLS);
    - the role → permission matrix;
    - `commercial-system` refused inbound;
    - the operator gate across dev, closed and live;
    - the B-I1 XFCC principal (including a forged first element), key format and tenant match;
    - B-W1 signature, ledger-first, redelivery and the customer-mapping tenant;
    - no card data in responses.
  - **Flows:**
    - direct signup with Billing-id provider keys, no events and a completed idempotency record;
    - replay and a different body under the same key;
    - eight concurrent duplicates producing exactly one charge;
    - B-P1 permissions per role;
    - cancel and resume with optimistic locking;
    - seat purchase with its outbox event;
    - billing contact;
    - the B-O7 operator view.
  - **Infrastructure:** probes, API documents, feature flags unregistering routes, every catalogue route mounted, unmatched routes as problems.
- **New `test/testkit`:** the shared harness (Postgres 17 container with migrations and production roles, seed data, recording fakes for the provider with a test webhook signature, Core, Realm Provisioner and Entitlements, and the `Service` constructor).
- **Mutation-checked:** disabling the path-tenant guard or the `commercial-system` rejection makes the matching tests fail. The first version of the `commercial-system` test passed without its guard (a later check refused the request); it now holds a role that would otherwise be admitted.
- **Corrections the suite surfaced in the documentation:**
  - B-P7 answers `202`, not `201` (`ARCHITECTURE.md`);
  - the idempotency replay is the same JSON document, not the same bytes, because `idempotency_records.response_body` is `jsonb` (`ARCHITECTURE.md`, `.claude/key-files.md`). LLD rev 0.42 still says "byte for byte".
- **depguard:** the `database-boundary` rule exempts `test/` harness code.

### Added (2026-11-27 — service documentation)

- **`README.md`** replaces the two-line placeholder:
  - service overview, and the 45 endpoint IDs with idempotency rule and permission;
  - errors and validation, and integration notes for every partner service;
  - local development and API calls, and an end-to-end events walkthrough on floci;
  - every configuration key with its code default;
  - security, observability, deployment, CI, Docker and cross-service dependencies.
- **`ARCHITECTURE.md`:**
  - layer model and package dependency graph (mirroring `.go-arch-lint.yml`);
  - request, B-I1, write, saga and webhook flows;
  - cache strategy, data model, event flow, RLS, concurrency, failure domains, key invariants, deployment and testing;
  - a consumer checklist, the schema lifecycle and a threat model.
- **`CHANGELOG.md`:** this file.
- **`VERSIONING.md`:**
  - SemVer, and what counts as the runtime contract;
  - the provider adapter version, which is bound to certification;
  - the current release blockers;
  - consuming a release, and the maintainer process (marking what isn't automated yet);
  - the compatibility matrix.
- **`Makefile` reorganised** into sections, with a grouped `make help`. Existing targets keep their behaviour.
  - `vet` and `lint` also run with every test build tag (`integration,helm`).
  - New targets:
    - `setup`, `godoc`, `mod-verify`, `vuln-check`;
    - `openapi` / `openapi-check`;
    - `test-unit`, `test-postgres`, `test-flows`, `test-bus`, `test-stripemock`, `test-helm`, `race`;
    - `cover-all`, `cover-func`, `cover-html`;
    - `docker-reset`, `docker-build`, `trivy`, `pin-base-images`;
    - `certify-adapter-version`;
    - `schema-pull`, `schema-diff`, `schema-register`, `schema-verify` (Glue `billing-events`, the same by-definition lookup the server does at startup).
- **`CONTRIBUTING.md`:**
  - prerequisites, setup and project layout;
  - step-by-step recipes for domain logic, use cases, endpoints, migrations, produced and consumed events, outbound calls, configuration, metrics, alerts and CronJobs;
  - testing requirements and canonical tests;
  - guardrails with what enforces each one;
  - a documentation update checklist, branch naming, the PR checklist, commit style and deployment.
- **Discrepancies the documentation records rather than repeats:**
  - LLD §6's price-book and I-11 display caches are not implemented;
  - the Glue encoder is Billing's own, because platform-events ships only a decoder;
  - `adapters/observability` still names OpenTelemetry types.

### Fixed (2026-11-27 — `BillingOutboxDeadLetter` could never fire, LLD rev 0.57)

- **The outbox dead-letter alert queried a metric that does not exist.**
  - **Before:** `BillingOutboxDeadLetter`, its runbook, LLD §11.5 and `registry-proposals.md` read `platform_outbox_dead_letters`. That name is in no registry and no library emits it, so a `billing.events` row reaching the dead letters would never have paged.
  - **Now:** the alert reads `sum(increase(platform_dlq_messages_total{service="commercial-billing",operation="outbox_publish"}[24h])) > 0`. That is platform-events' approved count of rows moved to `outbox_dead_letters` (`metric_status: approved`).
  - **Trade-off:** the registry has no dead-letter backlog gauge, so the alert stays firing for 24 h after the last dead letter. The runbook closes the incident only when the table is empty.
- **New test `TestReferencedPlatformMetricsAreRegistered`:** every `platform_*` name in alerts, recording and SLO rules, the dashboard, HPA values and runbooks must be a registry entry that is neither deprecated nor removed. gincommon's `metricslint refs` checks only names with a series suffix, so a gauge emitted by a library slipped through. The test failed on exactly the two references above before the fix.
- **New `make metrics-lint`:** runs the observability standard's compliance tests (also part of `make test`).
- **Audit result, no other change:** Billing already met the Enterprise Platform Observability Standard:
  - Tier 1 registry names only;
  - no Tier 2, since nothing is ratified;
  - Tier 3 `commercial_billing_*`;
  - identity labels injected centrally;
  - high-cardinality labels refused.

### Changed (2026-11-27 — RFC 9457 problems are also *read* through platform-gincommon, LLD rev 0.56)

- **`httpx.Call` parses a callee's error answer with gincommon's `problem.Decode`.** This covers Core, Realm Provisioner and Entitlements; before, a one-field struct read `errorCode`. A legacy `application/json` body carrying an `errorCode` is still read. `Accept` now names `application/problem+json`. Test: `TestCallReadsProblemErrorCode`.
- **The idempotency middleware reads its captured response with `problem.Decode`** to detect `409 REQUEST_IN_PROGRESS`.
- **`check-problem-details.sh` now also fails on:**
  - service-owned error DTOs or problem parsers on the HTTP error path (`errorCode` / `validationErrors` JSON tags; `ErrorResponse`, `ApiError`, `ProblemDetails`-style types);
  - `recover()` in the HTTP adapters.
- Error codes, statuses and the wire contract are unchanged.

### Changed (2026-11-27 — events, outbox and dedup stay behind the platform-events boundary, LLD rev 0.55)

- **The audit found no bypass,** so no code change was needed. Every path already used platform-events:
  - `events.NewEnvelope`, `outbox.EnqueueOrdered`, the runner and `PrunePublished`;
  - the SNS publisher, the SQS consumers and DLQ forwarding;
  - `inbox.Store.Process` and `Prune`;
  - `ApplySchema` and `GlueDecodeCodec`.
- **depguard `events`:** now denies the SNS and SQS SDKs everywhere (`adapters/eventbus` included), and AWS SDK v1.
- **New depguard rules:**
  - `events-glue` limits the Glue and AWS config SDKs to `adapters/eventbus`;
  - `events-boundary` limits `platform-events` to `adapters/eventbus`, `adapters/postgres`, `adapters/consumer`, `bootstrap`, `certify` and `cmd/*`.
- **`check-outbox-access.sh`** now catches every statement form on `outbox_events`, `outbox_dead_letters` and `processed_events`: TRUNCATE, COPY, ALTER, LOCK, MERGE and REFERENCES, including schema-qualified and quoted names.
- **Remaining exception:** the Glue **encode** codec (`adapters/eventbus/glue_codec.go`), until platform-events ships an encoder.

### Changed (2026-11-27 — database access stays behind the platform-pgcommon boundary, LLD rev 0.54)

- **Repositories classify database errors only with pgcommon's helpers:**
  - `IsUniqueViolation` and `IsCheckViolation`;
  - the price-book exclusion constraint by `pgcommon.ConstraintName`.

  This replaces hand-coded SQLSTATE literals (`"23505"`, `"23514"`, `"23P01"`) in six repositories.
- **New port error `port.ErrAccountExists`.** `AccountRepo.Create` returns it when the tenant already has an account. The conversion (B-P2) and direct-signup (B-I1) use cases test it with `errors.Is`, instead of inspecting a SQLSTATE in the app layer. Behaviour is unchanged. Test: `TestBIL_OPS_11_AccountExistsIsAPortError`.
- **depguard:**
  - new rule `database-boundary` limits `platform-pgcommon` to `adapters/postgres`, `bootstrap`, `certify` and `cmd/*`;
  - rule `database` also denies `golang-migrate`, `goose`, `gorm` and `sqlx`.
- **New guard `check-pgcommon.sh`** (in `make guards`) fails on:
  - SQLSTATE literals;
  - hand-rolled `SQLState()` interfaces;
  - hand-written session settings (`set_config`, `SET LOCAL`, statement and lock timeouts) outside migrations and tests.

### Changed (2026-11-27 — metrics through platform-gincommon with no exception, LLD rev 0.53)

- **`adapters/metrics` no longer imports Prometheus.** Collectors are gincommon's types (`gincommon.CounterVec`, `GaugeVec`, `HistogramVec`, `Counter`, `Gauge`, `Registerer`) built with its constructors.
- **Shared histogram:** `platform_dependency_request_seconds` is registered before the event bus is built, and platform-events reuses the same collector.
- **CronJob Pushgateway push without the Prometheus push library:**
  - it renders the text exposition `gincommon.MetricsHandler` serves;
  - keeps the job's own `commercial_billing_*` series;
  - sends a plain HTTP `PUT` with a 10 s timeout.

  Tests: `TestFilterExposition`, `TestPushRunReportsGatewayError`, plus the existing `TestPushRun`.
- **Lint:**
  - depguard rule `metrics` denies `github.com/prometheus` in all production code;
  - `logrus` and `zerolog` join the denied loggers;
  - forbidigo denies `fmt.Print*`, `print` / `println` and `gin.Default` / `gin.Logger*`.
- **Remaining exception:** `adapters/observability` still names OpenTelemetry types, for span attributes, span links and the log span id. gincommon has no helper for them yet.

### Added (2026-11-27 — Claude Code project setup)

- **`.claude/` topic notes:**
  - `CLAUDE.md`, `key-files.md`, `development-guide.md`;
  - `api-and-events.md`, `flows-and-concurrency.md`, `operations.md`.
- **Hooks:**
  - a PreToolUse guard blocks edits to generated files (`docs/swagger/openapi.json`, `internal/adapters/eventbus/schemas/*.json`, `docs/observability/metric-registry.md`) and to `.env`;
  - `gofmt` runs after Go edits.
- **The `/ci-local` skill.**
- **Three read-only reviewer agents:** `contract-checker`, `invariant-reviewer` and `docs-consistency-reviewer`.

### Changed (2026-11-27 — B-I1 response table complete after the Signup BFF reply, LLD rev 0.52)

- **§5.4.2 now lists every code B-I1 returns:**
  - added `409 REQUEST_IN_PROGRESS`, `409 PROVIDER_OPERATION_UNSUPPORTED`, `422 CURRENCY_MISMATCH` and `403 INSUFFICIENT_ROLE`;
  - `409 SUBSCRIPTION_ALREADY_EXISTS`: the BFF never provisions with the returned id (§5.4.2 and the §8.3 failure table);
  - new note on the BFF's retry behaviour.
- **Hand-off documents** in `docs/handoff/`: `bl-oq-8-signup-bff.md` (to Web / growth) and `bl-oq-8-billing-reply.md` (answers to the BFF's questions). Removed on 2026-11-27, with `docs/billing-implementation-prompt.md`; the outcome is recorded in LLD §5.4.2 and `docs/lld/billing-open-items.md` (BL-OQ-8).
- **BL-OQ-8 status:** `bff-signup` PR #6 merged (`c2e9a1d`), not yet in staging. The Pact contract and the staging Test-Clock E2E are pending.
- No Billing code change.

### Added (2026-11-26 — Entitlements contract test for `TenantQuotaAddOnChanged`, LLD rev 0.51)

- **`test/contract/entitlements_addon_test.go` checks three things:**
  - schema compatibility both ways: Billing's canonical schema guarantees everything Entitlements' consumed schema requires or constrains, and every resource Billing may send has an Entitlements limit;
  - every produced payload decodes into Entitlements' inbound structs and meets the rules its consumer applies;
  - Billing's domain rejects the payloads Entitlements cannot apply.
- **Vendored schema:** Entitlements' consumed schema is vendored from `dc9b654` under `test/contract/testdata/consumers/commercial-entitlements/`.
- **New workflow `.github/workflows/contract-live.yml`** runs the suite daily, on PRs and on dispatch against Entitlements' `main`, and warns when the vendored copy drifts. `CONTRACT_CONSUMERS_DIR` points the suite at any schema copy.

### Fixed (2026-11-26 — refund `purchase_ref` must be a UUID, LLD rev 0.51)

- **`TenantQuotaAddOnChanged.Validate` now rejects a refund whose `purchase_ref` is not a canonical UUID.** Entitlements decodes it as a UUID, so such an event could never have been applied. The contract test found the gap. The Glue schema is unchanged; the rule is recorded as a producer guarantee in the contract test.

### Changed (2026-11-26 — Billing → Entitlements identity and header contract, LLD rev 0.50)

- **§10.2 states the contract** for `GET /api/v1/internal/tenants/:id/entitlements`, agreed with Entitlements as F-2:
  - XFCC principal `cluster.local/ns/billing/sa/commercial-billing`;
  - the header table;
  - fail-closed rules and verification.

  The build gap (`X-Request-ID`) is recorded.
- **W-8, W-9 and W-10 approved:**
  - add-on events published only on a confirmed purchase or refund;
  - fresh eligibility on purchase, with caching only for pack discovery;
  - least-privilege egress to the one eligibility `GET`.
- No code change.

### Changed (2026-11-26 — open items in one file, LLD rev 0.49)

- **The ten working documents in `docs/lld/` are consolidated** into [`docs/lld/billing-open-items.md`](docs/lld/billing-open-items.md). It records what each team still owes, IAM's 2026-11-26 status, the approvals and the go-live tracker. `docs/lld/` now holds only the LLD and that file. No code change.

### Changed (2026-11-25 — conversion reversal: plan, seats and BL-OQ-27, LLD rev 0.45, 0.47, 0.48)

- **Who resets what after a reversal:**
  - Billing sends no `billing.events` after a reversal.
  - Org & Membership resets `plan` (to the recorded trial plan) and `licensed_seats` (to the trial allowance) when it applies RP's `TrialReactivated{reason: conversion_revert}`.
- **Guarantee recorded:** no Billing seat event follows a reversal. The delayed `BIL-EVT-6` event is cancelled for an `ended` subscription.
- **Add-ons:** a reversal does not undo them. The operator refunds add-on invoices since conversion through B-O3 (category `fraud`), which emits `TenantQuotaAddOnChanged{reason: refund}`. The runbook `reversal-decision.md` is updated.
- **BL-OQ-27:** Org & Membership PRs #69–#71 are merged (not deployed). A reversed direct signup becomes a trial of its signup plan. No Billing change was needed.

### Fixed (2026-11-25 — a reverted tenant can convert again, LLD rev 0.46)

- **Reconversion after an RP-4 reversal was refused.**
  - **Before:** RP-4 gives the tenant a new trial, but Billing does not consume `TrialReactivated`. `billing_accounts.iam_lifecycle` stayed `active`, so B-P2 and B-O1 answered `409 SUBSCRIPTION_ALREADY_EXISTS`, and B-P1 showed the tenant as not on trial.
  - **Now:** B-O8's final commit sets `iam_lifecycle = 'trial'`, clears `iam_suspension_source` and advances `iam_last_event_at` to the commit time, so lifecycle events stamped before the reversal are skipped (`BIL-EVT-3`). An offboarded account is left unchanged.
- No new event, queue, schema or config.

### Removed (2026-11-24 — legacy metric names, LLD rev 0.44)

- **The four unratified `commercial_*` names of earlier revisions are no longer emitted:** `commercial_purchase_confirmation_to_event_seconds`, `commercial_events_emitted_total`, `commercial_operator_actions_total` and `commercial_cross_tenant_job_runs_total`. Their Tier 3 `commercial_billing_*` equivalents replace them.
- **No compatibility period:** no environment is deployed, so nothing read the old names. `OBSERVABILITY_LEGACY_METRICS` is gone.
- **After the first deployment,** renames follow the standard's process: parallel emission, migrate consumers, deprecate, remove.

### Changed (2026-11-24 — metrics on the Enterprise Platform Observability Standard, LLD rev 0.43)

- **The three tiers:**
  - Tier 1 `platform_*`: registry names only, through gincommon's shared constructors.
  - Tier 2 `commercial_*`: none.
  - Tier 3 `commercial_billing_*`: everything else, with gincommon's validated constructors.
- **Ratification submission:** [`docs/observability/registry-proposals.md`](docs/observability/registry-proposals.md), with semantics, labels, values and aggregation for the Commercial domain candidates and the proposed platform entries Billing relies on.
- **New observability assets:**
  - recording rules and SLO definitions for §11.1, with burn-rate alerts;
  - the "Commercial / Billing" Grafana dashboard (chart ConfigMap);
  - the generated inventory [`docs/observability/metric-registry.md`](docs/observability/metric-registry.md) (`make metrics-inventory`).
- **CI:**
  - `metricslint` on the scrape;
  - `metricslint refs` on every rule, dashboard, HPA value and runbook;
  - every `commercial_*` reference exposed and not deprecated;
  - inventory drift, promtool, and chart copies of every rule file.

### Changed (2026-11-24 — RFC 9457 problems only through platform-gincommon, LLD rev 0.42)

- **New invariant `BIL-API-6`:** every error response is produced by gincommon (`AbortWithProblem`, `ClassifyError`, `BindingError`). `domain.DomainError` and `NotImplementedError` convert themselves to `*problem.Error`, with one `problem.Type` per §17 code (`https://errors.bcbpsolutions.com/commercial/<code-kebab>`).
- **Billing defines no problem model, error DTO, recovery middleware or validation mapper.** Unmatched routes and methods, recovered panics and failed probes are gincommon problems too.
- **Deliberate local exceptions:**
  - the `Retry-After` header;
  - the per-route JSON decode (B-I1 tolerates unknown fields);
  - the byte-for-byte idempotency replay.
- **New guard `check-problem-details.sh`** in `make guards`.

### Changed (2026-11-24 — events, outbox and dedup only through platform-events, LLD rev 0.41)

- **New invariant `BIL-OPS-2`:**
  - events are built with platform-events' envelope and enqueued with `outbox.EnqueueOrdered`;
  - published by its runner and SNS publisher;
  - consumed with its SQS consumers, DLQ publisher and `inbox.Store`;
  - configured through its `config` package.
- **Billing's own versions are removed:** the outbox and `processed_events` dedup code and the direct SNS, SQS and Glue SDK calls (except the Glue encode codec).
- **New guard `check-outbox-access.sh`** and depguard rule `events`.

### Changed (2026-11-24 — logs, metrics, traces and the database only through the platform libraries, LLD rev 0.40)

- **New invariant `BIL-OPS-10`:** the app layer uses `port.Tracer`, implemented on gincommon's tracer.
  - Outbound HTTP (`httpx`) uses gincommon span tracers, `InjectTraceContext` and the shared `platform_dependency_request_seconds`, replacing `otelhttp`.
  - CronJobs no longer keep their own registry.
  - The logger is gincommon's everywhere.
- **New invariant `BIL-OPS-11`:**
  - pools from `pgcommon.NewPool` in `pgadapter.OpenPools`;
  - settings from `pgcommon.ConfigFromEnv`, which also applies `PG_STATEMENT_TIMEOUT` and `PG_LOCK_TIMEOUT` (correcting rev 0.38's "not yet wired");
  - statements in `pgcommon.RunInTx*` on pgcommon's types.
- **depguard rules `observability` and `database`.**

### Added (2026-11-24 — Phase 8: provider certification suite, LLD rev 0.39)

- **`cmd/certify`** runs the versioned suite `cert-1.0` through Billing's real app layer, against a throwaway `certify_<run>` database. It has 30 required cases, and each test-mode customer is on a Stripe Test Clock.
  - Subcommands: `run`, `submit` (B-O14 → `pending_review`), `review` (B-O17, a second `pci_owner_reviewer`), `check` (release gate, `CERTIFY_CHECK_DSN`), `adapter-version`.
- **`make certify-rehearsal`** runs the suite against stripe-mock. The record carries `provider_account_ref: "stripe-mock"`, and `certify submit` refuses it.
- **`make check-first-deploy ENV=…`** checks the LLD §19.1 first-deployment order, read-only.

### Added (2026-11-24 — Phase 7: jobs, observability, deployment, LLD rev 0.37, 0.38)

- **`cmd/jobs` with the ten CronJobs of §13.2:**
  - `saga-sweeper`, `seat-renewal`, `downgrade-apply`, `purchase-expiry`, `seat-projection-check`;
  - `pii-minimisation` (never deletes a row, `BIL-OPS-9`);
  - `outbox-prune`, `processed-events-prune`, `renewal-repricing`, `idempotency-prune`.
- **Shared composition in `internal/bootstrap`.**
- **Pushgateway:** new key `PUSHGATEWAY_URL`; each job's metrics go there, grouped by `job=<cronjob name>`.
- **Reconciliation auto-repair (B-O5):**
  - a `status_mismatch` where the provider has ended the subscription is repaired through a ledger row `recon:<run_id>:<provider_object_id>`, which the normal processor re-reads;
  - a missing event is ledgered from the provider's event listing.
- **Renewal top-up:** applies the quantity even when the invoice is unpaid (`QuantityChange.KeepIfUnpaid`).
- **Observability:**
  - metrics, OTel tracing with span links over events, and log correlation;
  - label renames to the platform vocabulary: `result` on webhooks, RP-3 attempts, seat changes and add-on purchases; `saga_state` on sagas in flight.
- **Deployment:**
  - alert rules and the Helm chart: namespace `billing`, NetworkPolicy and Istio policies, `forwardClientCertDetails: SANITIZE_SET`, HPA, PDB, migrate hook Job, CronJobs, tenant and operator `HTTPRoute`s listing B-O1…B-O17;
  - `make helm-check` as its own CI job.
- **Runbooks:** one per alert in `docs/runbooks/`, plus the Stripe Test Clock rehearsal (`scripts/testclock`, `make testclock`).

### Added (2026-11-24 — Phase 6: business flows, LLD rev 0.35, 0.36)

- **Outbound clients:** Core (I-11, I-18), Realm Provisioner (RP-3, RP-4, RP-11) and Entitlements, all as `commercial-system`. Also AL-5 audit through platform-audit in outbox mode.
- **Conversion:**
  - the trial → paid saga (B-P2, B-O1), with RP-3 and `TenantConverted` handling;
  - direct paid signup (B-I1) with 3-D Secure;
  - the invoice mirror.
- **Plan change** (B-P3–B-P5), with an I-18 re-check at effect.
- **Seats:**
  - seat purchase with fallback proration and below-minimum deferral (B-P6–B-P9);
  - scheduled reduction and renewal top-up (B-P10, B-P11).
- **Lifecycle:**
  - cancellation, dunning, recovery and reactivation (B-P12–B-P14);
  - operator suspension with auto-pause, a retry freeze and held drafts (B-O10, B-O16, `BIL-SUB-7`).
- **Token add-ons** (B-P19–B-P23) with a fresh Entitlements check that fails closed.
- **Tenant reads, payment methods and billing contact:** tenant reads, payment-method setup (B-P15, B-P16) and billing contact (B-P25, B-P26).
- **Operator money and record-keeping:**
  - adjustments, refunds and disputes (B-O2, B-O3), and conversion reversal (B-O8);
  - the price book (B-O11, B-O12, with `VerifyPrice`, `422 PROVIDER_PRICE_MISMATCH`), webhook replay (B-O13) and the certification routes (B-O14, B-O15, B-O17);
  - operator views (B-O4, B-O6, B-O7, B-O9) and reconciliation runs (B-O5).
- **Ordering corrections (rev 0.35):**
  - `EnsureCustomer` runs before the first account insert;
  - `checkout_sessions` stores a `pending:<session_id>` placeholder;
  - the renewal's I-11 read and top-up move out of the `invoice.paid` transaction into `seat-renewal`.
- **New invariant `BIL-AUD-1`:** a global change and its AL-5 entry commit in one `billing_system` transaction (migration `000006`).
- **B-P16:** the new default payment method is applied by the lifecycle follow-up worker, outside any transaction.

### Added (2026-11-24 — Phase 5: HTTP API, LLD rev 0.34)

- **The route table for the §5.3 catalogue:** 26 tenant routes, B-I1 and 17 operator routes, with B-W1 already mounted. Each has its method, path, permission and `Idempotency-Key` rule.
- **`app/authorizer.go`:**
  - `RejectSystemCaller` (`BIL-AUTH-4`), `RequireTenantMatch` (`BIL-API-2`), `RequirePermission` (`BIL-AUTH-1`, `BIL-AUTH-7`);
  - `RequirePlatformOperator` with the `OPERATOR_PATH_LIVE` gate (`BIL-AUTH-6`), `RequirePCIReviewer` (B-O17);
  - `RequireBFFCaller`, with the principal taken from `X-Forwarded-Client-Cert` (`BIL-AUTH-3`).
- **The idempotency middleware** (§9.2).
- **Binding and validation:** strict JSON binding, §10.4 validation, 64 KiB bodies, `415` on non-JSON.
- **The OpenAPI 3.1 document,** generated from the route table (`GET /openapi.json`, `docs/swagger/openapi.json`, `TestOpenAPIGolden`).
- **Clarifications (rev 0.34):**
  - new setting `OPERATOR_PATH_LIVE` (default `false`);
  - `validationErrors[]` uses gincommon's `{field, message}` shape;
  - the B-I1 caller comes from XFCC.

### Added (2026-11-23 to 2026-11-24 — Phases 3 and 4: providers, webhooks, events, LLD rev 0.33)

- **Phase 3 — providers and webhooks:**
  - the `PaymentProvider` port (§3.4) and the `manual` adapter;
  - the Stripe adapter on stripe-go **v87.2.0** (Stripe API `2026-11-18.endive`, adapter version `1.0.0+stripeapi.2026-11-18.endive`);
  - the provider registry with the certification gate (`BIL-PROV-1`–`BIL-PROV-4`);
  - B-W1 and the ledger processor (`BIL-PAY-1`–`BIL-PAY-5`).
- **Phase 4 — events:**
  - `api/asyncapi.yaml` and the six produced schemas, extracted with schema-gov 0.4;
  - Glue codecs and the ordered outbox publisher on `billing-events`;
  - the `tenant-billing-q` and `usage-billing-q` consumers with inbox dedup and DLQ forwarding;
  - the delayed-emission worker (`BIL-EVT-6`);
  - floci bus tests and `scripts/init-floci.sh`;
  - contract tests against Core's, Realm Provisioner's and Audit's consumed schemas.

### Added (2026-11-23 — Phases 0 to 2: scaffolding, domain, schema, LLD rev 0.31, 0.32)

- **Phase 0 — scaffolding:**
  - module `github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service`, Go 1.26.11;
  - `platform-gincommon/v2` v2.7.0 (the first release with the `commercial` domain), `platform-events/v2` v2.3.0, `platform-pgcommon/v2` v2.0.2, `platform-audit` v0.1.2;
  - configuration loading and validation (§12), the server skeleton with probes, golangci-lint (forbidigo bans), `.go-arch-lint.yml`, `check-outbox-access.sh`;
  - docker-compose (Postgres 17, PgBouncer, floci, stripe-mock), the distroless non-root `Dockerfile`, the CI workflow and `docs/invariants-coverage.md`.
- **Phase 1 — domain model** (standard library only):
  - `Money` with ISO 4217 exponents;
  - transition tables for subscriptions, sagas, purchases, plan changes and the lifecycle;
  - the permission map;
  - the fallback proration calculator with the §14.1 golden tests;
  - the §17 error catalogue.
- **Phase 2 — schema and repositories:**
  - migrations `000001`–`000005` (roles, tables, constraints, triggers including `trg_pc_guard`, RLS, views, seed price book, provider certifications, audit-outbox RLS);
  - the transaction runner and repositories;
  - RLS and constraint tests on Postgres 17 with testcontainers.

### Outstanding (not yet done)

- **Other teams** (tracked in [`docs/lld/billing-open-items.md`](docs/lld/billing-open-items.md)):
  - **IAM:** the operator path, OP-G2 / OP-G3. Until it is live, operator routes answer `403` outside dev/test.
  - **Signup BFF (BL-OQ-8):** promote to staging, the Pact contract, and the staging Test-Clock E2E. Billing must also provide the Pact broker details and its provider verification job.
  - **Usage & Metering:** the `UsagePeriodClosed` contract (BL-OQ-24).
  - **Notification:** Billing messages (BL-OQ-12).
  - **Platform-SRE:** Istio sidecar injection on `bff`, and the IaC topology.
- **Platform libraries:**
  - a Glue encode codec in platform-events, so Billing can drop its own;
  - span attribute, link and span-id helpers in platform-gincommon, so `adapters/observability` can drop OpenTelemetry;
  - `metricslint refs` checking unregistered gauges;
  - a header hook for `Retry-After`.
- **This repository:**
  - the price-book and I-11 display caches of LLD §6 (pricing reads Postgres and I-11 is called every time today);
  - integration tests, image build, Trivy, a coverage gate and the provider-certification release gate in CI (LLD §14.6);
  - merging the stacked `feat/billing-phase-*` branches to `main`;
  - the first deployment (§19.1) and the PCI owner group review for production.
