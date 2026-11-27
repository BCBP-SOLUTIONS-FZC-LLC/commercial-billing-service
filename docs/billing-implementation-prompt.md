# Implementation Prompt — Commercial Billing Service

Paste everything below the line into a coding agent (for example Claude Code) started in the root of the `commercial-billing-service` repository. Run it **one phase at a time**: give the agent the whole prompt, then say "do Phase 0", review the PR, then "do Phase 1", and so on.

---

## Role and goal

You are implementing the **Commercial Billing service** (`commercial-billing`) in Go, from its Low-Level Design:

- **Source of truth:** `docs/lld/commercial-lld-billing-service.md` (rev 0.32 or later, base HLD v0.65). Read it in full before writing any code. It defines the package layout, data model, API, events, flows, invariants, configuration, deployment and tests. **Implement what it says. Do not invent behaviour, endpoints, fields, events, statuses or configuration that it doesn't define.**
- **Context documents** (read, don't implement from them): `docs/lld/billing-design-signoff-blockers.md` (why decisions were made), `docs/lld/billing-open-items-handoff.md` (what other teams still owe), `docs/lld/billing-iam-actions.md`, `docs/lld/billing-decided-items-entitlements-build.md`.
- **Parent HLD:** `../commercial-hld-docs/hld-commercial-domain-v0.65.md`. Consult it only when the LLD points to it.

The repository today holds only `README.md`, `.gitignore` and `docs/`. You are building the service from scratch.

## Rules that apply to every phase

1. **The LLD wins.** If the LLD and your instinct disagree, follow the LLD. If the LLD is ambiguous, contradicts itself, or is silent on something you need, **stop and list the question** with the section numbers. Don't guess. Known issues are listed under "Known LLD quirks" below; handle those as described.
2. **Invariants are requirements.** The LLD has about 70 invariants named `BIL-<AREA>-n` (§3.2, §4.6, §5.2, §7, §9.7 and elsewhere). For each one:
   - enforce it where the LLD says (database constraint, trigger, lint rule, code path);
   - reference its ID in a code comment at the enforcement point;
   - cover it with at least one test whose name contains the ID, such as `TestBIL_SEAT_3_RenewalNeverBelowSeatsInUse`.

   Keep `docs/invariants-coverage.md` up to date with: invariant, enforcement location, test name.
3. **Architecture is enforced.** Follow the hexagonal layout in §3 exactly. `.go-arch-lint.yml` (§3.2) must pass:
   - `internal/domain` imports only the standard library;
   - only `internal/adapters/provider/stripe` imports `github.com/stripe/*` (`BIL-PCI-1`);
   - every DB pool comes from `pgcommon.NewPool` (`BIL-OPS-1`);
   - there are no HTTP calls inside a DB transaction (`BIL-OPS-3`, `BIL-CONC-2`).
4. **Platform libraries, not hand-rolled equivalents** (§3.1, §3.3):
   - `platform-gincommon/v2` v2.7.0 or later: HTTP, RFC 9457 errors, metrics (earlier releases reject the `commercial` domain).
   - `platform-pgcommon/v2`: pools, GUC, transactions, migrations.
   - `platform-events/v2` v2.3.0: outbox, SQS, inbox, Glue codecs.
   - `platform-audit` v0.1.2: AL-5 in **outbox mode** only.
   - `stripe-go/v83`, pinned.
   - Go 1.26.11. Use the latest platform releases (see `/Users/vijayaragavan/XPertSharedLib/go`).

   They are private modules: set `GOPRIVATE=github.com/BCBP-SOLUTIONS-FZC-LLC/*`. If you can't fetch one, stop and say so. **Never stub or reimplement a platform library.**
5. **Safe defaults.** Every feature flag defaults to off or its §12 default: `FEATURE_TOKEN_ADDONS=false`, `FEATURE_DIRECT_SIGNUP=false`, `AUTO_REVERT_ON_EFW=false`, `AUTO_PAUSE_ON_OPERATOR_SUSPENSION=true`; `FEATURE_SEAT_PURCHASE` as §12 says. Operator routes answer `403` until the operator path is live (`BIL-AUTH-6`). B-O17 requires the `pci_owner_reviewer` role (`BIL-PROV-4`).
6. **PCI scope is SAQ A** (§10.1). No card data in Billing: no PAN, CVV or expiry in any table, log, event, trace or error. Tests must scan logs and the ledger for PAN patterns (§14).
7. **Money:** `domain.Money{AmountMinor int64, Currency}` everywhere. The fallback proration calculator uses exact `math/big` arithmetic and rounds half-up once, at the end (§8.5.3). Never use `float64` for money.
8. **Idempotency and ordering:**
   - every mutating route takes `Idempotency-Key` where §5 says (§9.2);
   - every provider call passes an idempotency key;
   - every state change and its event go in one transaction through `outbox.EnqueueOrdered` keyed by `tenant_id` (`BIL-EVT-5`);
   - audit entries are written in the same transaction through the `AuditWriter` port (§3.3, §10.6).
9. **Tests come with the code**, not after. Unit tests for domain logic; integration tests with testcontainers-go Postgres 17 (as the IAM services), real migrations and RLS; stripe-mock; floci for SNS/SQS/Glue; `httptest` fakes for Core, Realm Provisioner and Entitlements (§14.1, §14.2). Coverage gates per §14.6.
10. **Git workflow:**
    - one branch and one PR per phase, named `feat/billing-phase-<n>-<slug>`;
    - never commit to `main`; never force-push; never commit secrets or `.env` files;
    - each commit message ends with the attribution line your environment requires;
    - each PR description lists the LLD sections implemented, the invariants covered, any open questions, and what was deliberately left out.
11. **Definition of done for a phase:**
    - `make lint`, `make arch-lint`, `make test`, `make test-integration` all green;
    - the invariants coverage table updated;
    - the LLD sections for the phase fully implemented, with a checklist in the PR;
    - no TODOs without an issue reference.

## Known LLD quirks — handle as stated

| Quirk | What to do |
|---|---|
| Module path | `github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service` (LLD rev 0.31). Service name, ServiceAccount, pod label, envelope `source` and metric prefix stay `commercial-billing` (CD-17, CD-19) |
| §3 layout's `cmd/jobs/jobs/` list predates two jobs | **§13.2 is authoritative.** Ten CronJobs, including `pii-minimisation` and `renewal-repricing` |
| `BIL-PAY-1` is stated twice (§4.6 and §9.7) with the same meaning | Treat it as one invariant |
| Entitlements' consumed schema isn't published yet (BL-OQ-3, 4, 20) | Build against LLD §7.3.6 and §18.4 with an `httptest` fake and recorded responses. Leave a contract-test slot (§14.3) for Entitlements' schema, marked pending |
| `UsagePeriodClosed` contract is unverified (BL-OQ-24) | Implement the consumer per §7.1 and §8.12, records reference-only, behind configuration so it can stay disabled. Nothing is priced from it |
| Operator path not live (OP-G2/OP-G3) | Implement every operator route fully. The `403` gate is `BIL-AUTH-6`; don't add other workarounds |
| BL-OQ-15 (multi-provider migration, §19.3) is deferred to Phase 3 | Implement the provider registry and `PaymentProvider` port fully, with Stripe and manual adapters only. Don't build §19.3's provider-migration tooling |

## Phases

### Phase 0 — Scaffolding and CI

**LLD:** §3 (layout), §3.1, §3.2, §12 (config loading), §13.3 (CI), §14.6 (gates).

Build:
- `go.mod` (module path above, Go 1.26, pinned dependencies);
- the directory tree from §3 with empty packages;
- `Makefile` targets: `build`, `lint`, `arch-lint`, `test`, `test-integration`, `test-contract`, `migrate`, `run`, `certify`;
- `.golangci.yml` with the `forbidigo` bans in §3.2;
- `.go-arch-lint.yml` exactly as in §3.2;
- `.github/scripts/check-outbox-access.sh` (`BIL-OPS-2`);
- `docker-compose.yml` with Postgres 17, PgBouncer, floci and stripe-mock;
- `Dockerfile`, distroless and non-root;
- GitHub Actions for lint, arch-lint, unit, integration, `schema-gov validate | extract --check | diff`, and `helm-check` (`BIL-OPS-4`);
- `cmd/server` that loads config (§12), builds pools and serves `/healthz`, `/readyz` and metrics on 9090, with graceful shutdown;
- `cmd/server/identity.go` (`resolveAppEnv`).

**Done when:** CI is green on an empty service that starts, reports ready and shuts down cleanly.

### Phase 1 — Domain model (pure Go)

**LLD:** §4.6, §5.2 (permissions), §8.5.3 (proration calculator), §8.14 (subscription state machine), §9.3 (saga state machine), §17 (error catalogue), §7.3 (event payload types).

Build `internal/domain`:
- money with ISO 4217 exponents;
- subscription, saga, plan-change, seat, add-on, invoice, refund, adjustment, dispute and payment-event types with explicit transition tables;
- permission constants and the role → permission map (§5.2, Finance-approved; `BIL-AUTH-7`);
- the fallback proration calculator with the §14.1 **golden tests**, including the below-minimum deferral example (115 deferred, `BIL-SEAT-5`);
- the `DomainError` → `errorCode` catalogue.

**Done when:** every transition table is tested for allowed and forbidden moves, the golden tests match §14.1 exactly, and the domain imports only the standard library.

### Phase 2 — Database, RLS and repositories

**LLD:** §4.1 to §4.5, §4.4 (migration order), §13.4, §14.5 (RLS tests).

Build:
- migrations `000000` to `000005` in the exact order of §4.4 (`000004` includes `trg_pc_guard`; `auditoutbox.ApplySchema` runs between the outbox and initial schemas);
- every table, enum, constraint, index, trigger, grant and view in §4.2 to §4.5, including:
  - `seat_purchases` calculation and deferral columns;
  - `invoices.held_for_review` and its disposition columns;
  - `subscription_items.scheduled_price_*`;
  - `provider_certifications` statuses and the `trg_pc_guard` immutability trigger;
  - `billing_accounts.pii_minimised_at`;
- roles `billing_app` (RLS) and `billing_system` (BYPASSRLS);
- `cmd/migrate`;
- repositories behind the ports in `app/port/repositories.go`.

**Done when:**
- every §14.5 RLS case passes: a tenant can't read or write another tenant's rows, and a missing or invalid GUC returns 0 rows;
- every CHECK constraint and trigger has a test proving it rejects bad data;
- `trg_pc_guard` rejects every forbidden update and every delete.

### Phase 3 — Provider abstraction, Stripe and manual adapters, webhook pipeline

**LLD:** §3.4, §3.5 (certification gate), §7.4, §8.15, §9.4, §10.3, §18.8, §19.6.

Build:
- the `PaymentProvider` port exactly as §3.4, including `FinalizeInvoice`, `PauseCollection` semantics and `ScheduleAtPeriodEnd` with `ToPriceRef`;
- the `manual` adapter;
- the Stripe adapter: stripe-go calls, webhook verifier, normaliser, error mapper, idempotency keys, and the §18.8 mapping table, including `auto_advance = false` on open invoices during a pause;
- the provider registry with the certification gate: a provider takes new accounts only with a `passed` or `exempt` record; it refreshes every `PROVIDER_REGISTRY_REFRESH`; `BIL-PROV-1` to `BIL-PROV-4`;
- the B-W1 webhook pipeline: body limit, verify, ledger insert **before any domain write** (`BIL-PAY-1`), claim with `SKIP LOCKED`, re-read through `FetchCurrent`, dispatch, retry with backoff.

**Done when:** stripe-mock integration tests cover every §7.4 event mapping; signature failure returns `400` and writes nothing; replays are no-ops; the registry refuses an uncertified provider.

### Phase 4 — Events: outbox, schemas, consumers

**LLD:** §7 (all), §7.6 (AsyncAPI), §19.4.

Build:
- `api/asyncapi.yaml` (AsyncAPI 3.0) with the produced payloads of §7.3, open schemas;
- `schema-gov extract` into `internal/adapters/eventbus/schemas/`;
- a Glue `ValidatingCodec` publisher on `billing-events`, through the ordered outbox;
- the two SQS consumers `tenant-billing-q` and `usage-billing-q` with `GlueDecodeCodec`, `pkg/inbox` dedup and DLQ forwarding;
- the delayed-emission worker (`scheduled_emissions`; `BIL-EVT-6` 120 s delay).

**Done when:**
- floci integration tests show framed publish, ordered delivery per tenant, and consumption with dedup and DLQ;
- contract tests validate Billing's produced payloads against Core's, Realm Provisioner's and Audit's consumed schemas (§14.3), with the Entitlements slot marked pending.

### Phase 5 — HTTP API and authorization

**LLD:** §5 (all 45 endpoint IDs: B-P1 to B-P26, B-I1, B-W1, B-O1 to B-O17), §5.1, §5.2, §5.5, §9.2, §10.4, §17.

Build:
- the Gin router with tenant, internal, webhook and operator groups and their middlewares (§3.3);
- `authorizer.go`: `RequirePermission`, `RequirePlatformOperator`, `RequireBFFCaller`; the extra `pci_owner_reviewer` check on B-O17 (`BIL-AUTH-*`, `BIL-PROV-4`); the operator gate (`BIL-AUTH-6`);
- the idempotency middleware (`idempotency_records`, replay returns the stored response, a body mismatch returns `422`);
- DTOs and validation per §10.4;
- RFC 9457 errors with the §17 codes;
- `/asyncapi` and swagger.

Handlers call use cases; use cases may be stubs that return `501` until Phase 6. Mark each with its phase.

**Done when:** every endpoint exists with the exact path, method, permission, idempotency rule and error codes of §5; authorization tests cover every role × route pair in §5.2, including the owner-only and overlap rules (`BIL-AUTH-7`).

### Phase 6 — Business flows

**LLD:** §8.1 to §8.16, §9.1 to §9.6, §18.1 to §18.7.

Implement the use cases one flow per PR if large:
- §8.1 conversion saga (trial, operator stub B-O1), with RP-3 and `TenantConverted` handling;
- §8.2 conversion reversal (B-O8; SEV-2 alert; `AUTO_REVERT_ON_EFW=false`);
- §8.3 direct paid signup (B-I1, 3-D Secure, behind `FEATURE_DIRECT_SIGNUP`);
- §8.4 plan change (upgrade on payment; downgrade with an I-18 re-check);
- §8.5 seat purchase, including fallback proration and below-minimum deferral, and §8.6 scheduled reduction, renewal and top-up;
- §8.7 token add-on purchase (behind `FEATURE_TOKEN_ADDONS`; fresh Entitlements read; fail closed `503`);
- §8.8 renewal, dunning, cancellation, offboarding; §8.9 reactivation;
- §8.10 operator suspension: auto-pause, retry freeze, held drafts, B-O16 disposition (`BIL-SUB-7`);
- §8.11 operator adjustments and refunds (`BIL-ADD-3`, `BIL-ADD-4`);
- §8.12 usage records; §8.13 reconciliation (B-O5);
- §8.16 renewal repricing (`BIL-SUB-8`).

Use the per-tenant lock and optimistic concurrency of §9.1. Calls to IAM and Entitlements go through `iamclient` and `entitlementsclient` with the §12 timeouts and failure behaviour of §9.5 and §20.7.

**Done when:**
- each flow's sequence diagram is covered by an integration test, including its failure branches;
- the Stripe Test Clock scenarios in §14.4 are scripted for staging: monthly and annual renewal, dunning to cancellation, suspension across a renewal, repricing at renewal.

### Phase 7 — Jobs, observability, deployment

**LLD:** §11 (SLOs, metrics, tracing, logs, alerts), §12, §13.1, §13.2, §15.3, §20.

Build:
- `cmd/jobs` with all ten §13.2 CronJobs (`pii-minimisation` never deletes a row and never touches a financial record: `BIL-OPS-9`);
- every §11.2 metric with the exact names and labels;
- OTel tracing per §11.3, including span links over events;
- structured logs per §11.4, with no PII and no card data;
- `PrometheusRule` alerts per §11.5;
- the Helm chart per §13.1: namespace `billing`; ServiceAccount and label `commercial-billing`; NetworkPolicy and Istio policies per §10.2, including egress to Entitlements on the one `GET`; HPA; PDB; migrate Job; CronJobs; the tenant and operator `HTTPRoute`s, with the operator route listing **B-O1 to B-O17**;
- runbooks under `docs/runbooks/` from §20.

**Done when:** `helm-check` passes with the exact names; each alert has a runbook link; each metric is registered and tested.

### Phase 8 — Certification suite and release readiness

**LLD:** §14.7, §19.6, §20.9, §19.1.

Build:
- `cmd/certify`: runs the versioned suite (`cert-1.0`) in the provider's test mode through the real app layer against a throwaway database;
- the record file in the §19.6 format, with evidence written to the evidence bucket;
- the submission flow: B-O14 records the run as `pending_review`, and a **different** PCI owner group member approves it with B-O17;
- the `BillingProviderNotCertified` alert;
- the §19.1 first-deployment order checked by a script.

**Done when:** the suite passes against stripe-mock locally and is ready to run against Stripe test mode; a record can't become `passed` without a second person.

## What not to do

- Don't change `docs/lld/*`. If the code needs the design to change, stop and propose the LLD change separately.
- Don't add endpoints, events, statuses, config keys or tables the LLD doesn't define.
- Don't call IAM, Entitlements or the provider inside a DB transaction.
- Don't log or store card data, provider secrets or webhook secrets.
- Don't switch on `FEATURE_TOKEN_ADDONS` or `FEATURE_DIRECT_SIGNUP` by default, and don't bypass the operator `403` gate.
- Don't mock the platform libraries in production code paths; fakes belong in tests only.
- Don't merge your own PRs. Production deploys need a PCI owner group reviewer (HLD §15.4).

## Start

Begin by reading the LLD end to end. Then reply with:
1. A short summary of the service in your own words (10 lines at most).
2. Any questions or contradictions you found, with section numbers.
3. Your Phase 0 plan as a file list.

Wait for approval before writing code.
