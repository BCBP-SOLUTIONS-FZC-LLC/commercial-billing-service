# commercial-billing-service

The Billing service of the Commercial domain: the **commercial system of record** for what a tenant has bought and paid for. It owns:
- subscriptions, renewals and seat licensing;
- plan changes and token add-ons;
- the payment-provider integration (Stripe, plus a `manual` stub provider) and its webhook ledger;
- invoices, refunds, adjustments and disputes as the platform sees them;
- the lifecycle events that tell IAM and Entitlements what a tenant is entitled to.

It never holds card data (PCI SAQ A), and it is on no per-request hot path.

**Repository:** `github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service`

**Module:** Go 1.26.11 · private module · three binaries from one image (`cmd/server`, `cmd/jobs`, `cmd/migrate`), plus the `cmd/certify` tool · Helm chart 0.7.0 · Stripe adapter 1.1.0

**Design:**
- Refines the Commercial Domain HLD **v0.65**.
- The LLD is [`docs/lld/commercial-lld-billing-service.md`](docs/lld/commercial-lld-billing-service.md), rev 0.61 (revision history at the top). It is the source of truth for everything here.
- [`ARCHITECTURE.md`](ARCHITECTURE.md) has the diagrams and runtime flows.

**State:**
- Phases 0–8 are implemented. Phases 0–5 are on the stacked branches `feat/billing-phase-0-scaffolding` … `feat/billing-phase-5-api`. Phases 6–8 and everything after them are on `feat/billing-phase-6-flows`: platform-library routing, RFC 9457 errors, the observability standard, the test suites, CI/CD, and the fixes from two production-readiness review rounds (LLD rev 0.59 and 0.60).
- `main` holds only the LLD. Nothing is deployed to any environment.
- Before go-live: the cross-team items in [`docs/lld/billing-open-items.md`](docs/lld/billing-open-items.md) (among them Entitlements' BL-OQ-28 findings and the outbox retry window, BL-OQ-29), certification of Stripe adapter 1.1.0, the Loki rules for the log-based alerts, and the LLD §20.10 go-live checks.

---

## Mental model

| This service owns | It does NOT own |
|---|---|
| `billing_accounts` — provider customer, fixed currency, billing contact, projection of the IAM lifecycle | Tenant data, plan projection, seat projection, lifecycle (IAM Core / Org & Membership — Billing publishes, Core projects) |
| `subscriptions` + `subscription_items` — plan, interval, status (`incomplete → active → past_due → cancelled → ended`), seat quantity | Realms and Keycloak (Realm Provisioner — reached only through RP-3, RP-4, RP-11) |
| `conversion_sagas` — trial → paid conversion across the provider and the Realm Provisioner | Quotas, effective entitlements, add-on balances (Entitlements — Billing sends `TenantQuotaAddOnChanged`, Entitlements applies it) |
| `seat_purchases`, `seat_change_schedules` — prepaid seats and next-renewal reductions | Usage counting (Usage & Metering — Billing keeps reference-only `usage_period_records`, prices nothing from them) |
| `quota_addon_purchases` — token packs bought | Card data (the provider — the browser tokenises, only `pm_…` tokens reach Billing) |
| `plan_prices`, `token_packs` — the price book (operator-managed, effective-dated) | Audit records (Audit Log — Billing writes AL-5 entries through platform-audit) |
| `invoices`, `refunds`, `adjustments`, `disputes` — Billing's view of the provider's money movement | Customer emails (Notification — consumes `TenantPaymentPastDue`, `TenantSubscriptionCancelled`) |
| `payment_event_ledger` — every provider callback, verified, before any domain write | Tenant authorization and roles (IAM — Billing reads `x-tenant-roles` only) |
| `provider_certifications` — which provider adapter version may take new accounts | |
| `billing.events` — `TenantPlanChanged`, `TenantSeatsChanged`, `TenantPaymentPastDue`, `TenantSubscriptionCancelled`, `TenantReactivated`, `TenantQuotaAddOnChanged` | |

The provider is the record of money movement, and Billing is the record of the subscription as the platform sees it. They are kept consistent by:
- synchronous provider responses;
- verified callbacks that are re-read from the provider;
- reconciliation runs (B-O5).

---

## Why this service exists

Every commercial decision the platform makes needs one answer to three questions: what has this tenant paid for, at what price, and in what state is that payment? Putting that in one service means:

- **No double charge, ever:**
  - charging routes are idempotent (`Idempotency-Key`, stored outcome);
  - provider calls carry idempotency keys derived from Billing ids;
  - every write takes a per-tenant lock.

  Those rules live in one place instead of in every caller (`BIL-IDEM-*`, `BIL-CONC-1`).
- **No lost or contradictory signal to IAM.** Each state change and its event commit in one transaction through the platform-events outbox, ordered per tenant (`BIL-EVT-2`, `BIL-EVT-5`), so Core's seat cap and Entitlements' quotas follow what was actually paid.
- **PCI scope stays small** (SAQ A):
  - one adapter package talks to Stripe;
  - one table records callbacks, with an allow-listed payload;
  - no card data exists anywhere in the platform.
- **The payment provider is replaceable.** The `PaymentProvider` port carries no provider type, and the certification gate (`cmd/certify`, B-O14, B-O17) controls which adapter version may take new accounts.

The hexagonal layout enforces this structurally: `internal/app` depends only on `internal/app/port` and `internal/domain`. `go-arch-lint`, depguard and four guard scripts keep the vendor libraries (stripe-go, pgcommon, platform-events, OpenTelemetry, Prometheus) behind their adapters.

---

## API overview

**45 endpoint IDs** across four route prefixes. 44 of them are in the route table (39 paths and 44 operations in the OpenAPI document); B-W1 is registered by the webhook adapter.

| Group | Prefix | IDs |
|---|---|---|
| Tenant | `/api/v1/tenants/:id/*` | 26 |
| Internal | `/api/v1/internal/*` | 1 |
| Webhook | `/api/v1/webhooks/:provider` | 1 |
| Operator | `/api/v1/operator/*` | 17 |

**Sources of truth:**
- `internal/adapters/http/routes.go` (the route table) and `internal/adapters/webhook/handler.go`;
- the generated REST contract [`docs/swagger/openapi.json`](docs/swagger/openapi.json) (OpenAPI 3.1, built from the route table and DTOs, kept in sync by `TestOpenAPIGolden`), also served at `GET /openapi.json`;
- the event contract [`api/asyncapi.yaml`](api/asyncapi.yaml), served at `GET /asyncapi`.

**Feature-flagged routes are not registered when their flag is off,** so they answer `404`:

| Flag | Routes |
|---|---|
| `FEATURE_DIRECT_SIGNUP` | B-I1 |
| `FEATURE_TOKEN_ADDONS` | B-P19…B-P23 |
| `FEATURE_SEAT_PURCHASE` | B-P6, B-P7 |

**Middleware chain:**
- **Global:** a body cap, then `gincommon.ObservabilityMiddlewares` (panic recovery → request ID → tracing → correlation → metrics → logging → request deadline).
- **Tenant group:** adds `gincommon.ProtectedMiddlewares` (RequireAuth → ContextMiddleware) → reject `commercial-system` → 64 KiB body limit → JSON content type → `x-tenant-id` must equal `:id` → per-route permission → idempotency.
- **Operator group:** adds the operator gate (`platform_operator`, `403` until `OPERATOR_PATH_LIVE`).

The service trusts gateway-injected headers (`x-user-id`, `x-tenant-id`, `x-tenant-roles`) and, for B-I1, the mesh client certificate. It parses no JWT.

**Permissions:**

| Permission | `tenant_owner` | `tenant_admin` |
|---|---|---|
| `commercial.billing.view` | ✓ | ✓ |
| `commercial.billing.download_invoice` | ✓ | ✓ |
| `commercial.subscription.manage` | ✓ | ✓ |
| `commercial.subscription.change_plan` | ✓ | — |
| `commercial.subscription.cancel` | ✓ | — |
| `commercial.payment_method.manage` | ✓ | — |

- Each tenant route requires exactly one permission, and none implies another (`BIL-AUTH-7`).
- `tender_admin`, `member` and `platform_operator` hold none on tenant routes.
- Missing permission → `403 INSUFFICIENT_PERMISSION` with extension `required_permission`.

### Tenant routes (26) — `/api/v1/tenants/:id/*`

| # | Method & path | Idempotency-Key | Permission | Purpose |
|---|---|---|---|---|
| B-P1 | `GET /subscription` | — | billing.view | Subscription summary for the Billing page |
| B-P2 | `POST /subscription/checkout` | required | subscription.change_plan | Hosted checkout to convert a trial (§8.1) |
| B-P3 | `POST /subscription/plan-change/preview` | — | subscription.change_plan | Price of an upgrade now / effect date of a downgrade |
| B-P4 | `POST /subscription/plan-change` | required | subscription.change_plan | Upgrade now (prorated) or schedule a downgrade (period end); I-18 check |
| B-P5 | `DELETE /subscription/plan-change` | optional | subscription.change_plan | Cancel a scheduled downgrade |
| B-P6 | `POST /subscription/seats/preview` | — | subscription.manage | Provider proration preview for added seats |
| B-P7 | `POST /subscription/seats/purchases` | required | subscription.manage | Buy seats (§8.5) |
| B-P8 | `GET /subscription/seats/purchases/:purchase_id` | — | billing.view | Seat purchase status |
| B-P9 | `GET /subscription/seats/purchases` | — | billing.view | Seat purchase history |
| B-P10 | `PUT /subscription/seats/scheduled-reduction` | required | subscription.manage | Schedule (or replace) a reduction for the next renewal |
| B-P11 | `DELETE /subscription/seats/scheduled-reduction` | optional | subscription.manage | Cancel the scheduled reduction |
| B-P12 | `POST /subscription/cancel` | optional | subscription.cancel | Cancel at period end (auto-renew off) |
| B-P13 | `POST /subscription/resume` | optional | subscription.cancel | Undo a pending cancellation |
| B-P14 | `POST /subscription/reactivate` | required | subscription.change_plan | Settle a `past_due` or `cancelled` subscription (§8.9) |
| B-P15 | `GET /payment-methods` | — | billing.view | Payment methods, read live from the provider (never stored) |
| B-P16 | `POST /payment-methods/setup-session` | required | payment_method.manage | Provider-hosted page to add or replace the default payment method |
| B-P17 | `GET /invoices` | — | billing.download_invoice | Invoice list |
| B-P18 | `GET /invoices/:invoice_id/download` | — | billing.download_invoice | `302` to a short-lived provider PDF link |
| B-P19 | `GET /token-packs` | — | billing.view | Active token packs and this tenant's eligibility |
| B-P20 | `POST /token-addons/purchases` | required | subscription.manage | Buy token packs (§8.7) — fresh Entitlements check, fail closed |
| B-P21 | `GET /token-addons/purchases/:purchase_id` | — | billing.view | Add-on purchase status |
| B-P22 | `GET /token-addons/purchases` | — | billing.view | Add-on purchase history |
| B-P23 | `POST /token-addons/purchases/:purchase_id/payment-session` | required | subscription.manage | Hosted payment page when the off-session charge needs the customer |
| B-P24 | `GET /subscription/history` | — | billing.view | Status, plan and seat history |
| B-P25 | `GET /billing-contact` | — | billing.view | Billing contact, company, country, tax id |
| B-P26 | `PUT /billing-contact` | optional | payment_method.manage | Update the billing contact |

### Internal route (1) — mesh mTLS, Signup BFF only

| # | Method & path | Purpose |
|---|---|---|
| B-I1 | `POST /api/v1/internal/subscriptions/authorize-and-charge` | Direct paid signup: create account and subscription, charge (§5.4.2, §8.3) |

**Identity:**
- The caller is the SPIFFE principal from `X-Forwarded-Client-Cert`, which the Istio sidecar sets. It must equal `BFF_CALLER_PRINCIPAL` (`cluster.local/ns/bff/sa/bff-signup`, `BIL-AUTH-3`).
- `Idempotency-Key` must be `charge:<tenant_id>`, and body `tenant_id` must equal `x-tenant-id`.

**Responses:**

| Response | Meaning |
|---|---|
| `201` | Charged |
| `200` + `Idempotent-Replayed` | Replay of a completed call |
| `402 PAYMENT_ACTION_REQUIRED` | 3-D Secure needed; retry with the same key and body |
| `402 PAYMENT_DECLINED` | Declined |
| `410 CHARGE_EXPIRED` | 3-D Secure not completed in time |
| `409 SUBSCRIPTION_ALREADY_EXISTS` | The tenant already has a live subscription, or a different body was sent under the same key |
| `409 REQUEST_IN_PROGRESS` | The first call with this key is still running |
| `409 PROVIDER_OPERATION_UNSUPPORTED` | The new-account provider can't charge off-session (configuration fault) |
| `422 CURRENCY_MISMATCH` | An account already exists in another currency |
| `422 PRICE_NOT_AVAILABLE` | No price for that plan, interval and currency |
| `403 INSUFFICIENT_ROLE` | Caller identity rejected |
| `503` | Retry with the same key |

BFF alignment status: [`docs/lld/billing-open-items.md`](docs/lld/billing-open-items.md) (BL-OQ-8).

### Webhook route (1) — provider signature only

| # | Method & path | Purpose |
|---|---|---|
| B-W1 | `POST /api/v1/webhooks/:provider` | Provider callback: verify signature, tolerance and `livemode`, insert into `payment_event_ledger`, answer `200`. A duplicate is `200` with no work; a bad signature is `400` and stores nothing; a body over `WEBHOOK_MAX_BODY_BYTES` is `413` |

### Operator routes (17) — `/api/v1/operator/*`, `platform_operator` only

All operator routes answer `403` outside dev and test until IAM's operator path is live (`OPERATOR_PATH_LIVE`, `BIL-AUTH-6`). Every mutation needs a `reason` of 10–1000 characters.

| # | Method & path | Purpose |
|---|---|---|
| B-O1 | `POST /tenants/:id/conversions` | Phase 1 conversion stub (`manual` provider) |
| B-O2 | `POST /tenants/:id/adjustments` | Credit or debit on the customer balance |
| B-O3 | `POST /tenants/:id/refunds` | Refund all or part of a paid invoice |
| B-O4 | `GET /tenants/:id/invoices` | Invoice list including void and uncollectible |
| B-O5 | `POST /billing/reconciliation-runs` | Start a provider reconciliation run |
| B-O6 | `GET /billing/reconciliation-runs/:run_id` | Reconciliation run status and findings |
| B-O7 | `GET /tenants/:id/subscription` | Full subscription, sagas, purchases, schedules, disputes, ledger excerpt |
| B-O8 | `POST /tenants/:id/conversion-reversals` | Operator-approved RP-4 reversal (§8.2) |
| B-O9 | `POST /tenants/:id/conversion-sagas/:saga_id/actions` | Stuck saga: `retry`, `refund`, `resolved_manually` |
| B-O10 | `PUT /tenants/:id/collection-pause` | Pause or continue billing (§8.10) |
| B-O11 | `POST /billing/plan-prices` | Add a price row (closes the current one) |
| B-O12 | `POST /billing/token-packs` | Add, change or retire a token pack |
| B-O13 | `POST /billing/webhook-events/:provider/:provider_event_id/replay` | Re-process a ledger row (§20.2) |
| B-O14 | `POST /billing/provider-certifications` | Record a certification run produced by `cmd/certify` (`pending_review`) |
| B-O15 | `POST /billing/provider-certifications/:cert_id/revoke` | Revoke a certification |
| B-O16 | `POST /tenants/:id/invoices/:invoice_id/disposition` | Finalise or void a draft held during a collection pause |
| B-O17 | `POST /billing/provider-certifications/:cert_id/review` | Approve or reject a submitted run — `pci_owner_reviewer`, not the submitter (`BIL-PROV-4`) |

### Infrastructure routes

| Route | Purpose |
|---|---|
| `GET /healthz` | Liveness — `200 {"status":"ok"}`; `503` only when the platform-audit runner is not alive (stopped background loops restart in-process). Never checks a dependency |
| `GET /readyz` | Readiness — the app pool (`database`) and the BYPASSRLS pool (`sys_database`); `503` problem with `failed_check` and `checks` on failure |
| `GET /openapi.json` | OpenAPI 3.1 document |
| `GET /asyncapi`, `GET /asyncapi.yaml` | AsyncAPI 3.0 document |
| `GET /metrics` | On `METRICS_PORT` only (separate listener) |

---

## Input validation and errors

Every error response is an RFC 9457 problem (`application/problem+json`) produced by **platform-gincommon** (`BIL-API-6`):
- domain errors (`domain.DomainError`) convert themselves to gincommon's `*problem.Error`, with one problem type per §17 catalogue code;
- handlers pass them to `gincommon.AbortWithProblem`;
- binding errors are `gincommon.BindingError`;
- a recovered panic is gincommon's 500.

Billing defines no problem model, error DTO, recovery or validation mapper; `check-problem-details.sh` fails the build on one.

```json
{
  "type": "https://errors.bcbpsolutions.com/commercial/subscription-already-exists",
  "title": "Subscription already exists",
  "status": 409,
  "detail": "a subscription already exists for this tenant",
  "instance": "/api/v1/internal/subscriptions/authorize-and-charge",
  "errorCode": "SUBSCRIPTION_ALREADY_EXISTS",
  "traceId": "4bf92f3577b34da6a3ce929d0e0e4736",
  "tenantId": "3fa85f64-5717-4562-b3fc-2c963f66afa6",
  "timestamp": "2026-11-27T10:45:22Z",
  "billing_subscription_id": "0193a8f0-1c2d-7e3f-9a4b-5c6d7e8f9a0b"
}
```

**Problem body:**
- `errorCode` is the stable, `UPPER_SNAKE` code. Branch on it, never on `title` or `detail`. Codes are frozen once published.
- Context such as `billing_subscription_id`, `required_permission`, `decline_reason`, `current_record_version` or `action` is sent as RFC 9457 extension members.
- An internal 500 carries no detail.

**Headers:**
- `Retry-After` accompanies `409 REQUEST_IN_PROGRESS` and every `503`.
- A replayed idempotent response carries `Idempotent-Replayed: true`.

### Validation rules (LLD §10.4)

| Rule | Enforcement |
|---|---|
| Tenant, user and roles are never taken from a body for authorization | Identity only from CD-19 headers or XFCC (`BIL-API-1`); `x-tenant-id` must equal `:id` before any query (`BIL-API-2`, `403`) |
| Unknown JSON fields | Rejected (`400 VALIDATION_ERROR`) on tenant and operator routes; tolerated on B-I1 |
| Body size | 64 KiB on tenant and operator routes; `WEBHOOK_MAX_BODY_BYTES` (256 KiB) on B-W1 (`413`) |
| `Idempotency-Key` | 8–200 characters `[A-Za-z0-9:_\-.]`; B-I1 exactly `charge:<tenant_id>` |
| Seats | 1–1000 per request (`MAX_SEATS_PER_PURCHASE`), ≤ 100000 total |
| Token packs | `pack_quantity` 1–100; the purchase needs a fresh Entitlements eligibility read (`BIL-ADD-1`, `503 ENTITLEMENTS_UNAVAILABLE` if unreachable) |
| Currency, country, email | ISO 4217 and equal to the account's (`422 CURRENCY_MISMATCH`); ISO 3166-1 alpha-2; RFC 5322 ≤ 254 |
| Operator `reason` | 10–1000 characters on every operator mutation |
| Optimistic locking | Client mutations of a versioned row carry `record_version`; a moved version is `409 OPTIMISTIC_LOCK_CONFLICT` with the current version |
| Same key, different body | `422 IDEMPOTENCY_KEY_REUSED` (B-I1: `409 SUBSCRIPTION_ALREADY_EXISTS`) |
| Concurrent writes | Per-tenant advisory lock; deadlock / serialization retried 3 times, then `409 CONCURRENT_UPDATE` |

Full catalogue: LLD §17 (codes, statuses, extension members) and §5.5 (status codes).

---

## Architecture

See [`ARCHITECTURE.md`](ARCHITECTURE.md) for the diagrams: layer model, dependency graph, request, write, saga and webhook flows, data model, RLS, concurrency, failure domains and threat model.

### Repository layout

```
cmd/
  server/                 commercial-billing: main.go, wiring.go, events.go, lifecycle.go (shutdown, loop supervision), service.go, identity.go
  jobs/                   one binary, --job=<name>, ten CronJobs
  migrate/                platform-events and platform-audit schemas, then 000001–000007
  certify/                provider certification tool (not in the image)
internal/
  domain/                 money, proration, transition tables, permissions, event payloads, §17 error catalogue
  app/                    use cases (Service, per-tenant lock); purchase_invoice.go settles purchase invoices (BIL-PAY-6/7)
  app/port/               every interface (repositories, PaymentProvider, IAM, events, audit, jobs, observability)
  adapters/
    http/ webhook/ consumer/                         inbound
    postgres/ (+ migrations/) eventbus/ (+ schemas/) outbound
    provider/ (registry) provider/stripe/ provider/manual/
    iamclient/ entitlementsclient/ httpx/
    metrics/ observability/
  bootstrap/              shared wiring for server, jobs and certify
  certify/                the cert-1.0 suite behind cmd/certify
  config/                 every key with bounds (§12)
pkg/requestctx/           request context: caller identity type, X-Request-ID forwarded to outbound calls (imported by adapters/http and adapters/httpx only)
api/                      asyncapi.yaml (event contract source) + embed.go
test/
  postgres/ flows/ bus/ stripemock/ certify/        integration suites (tag integration)
  e2e/                    the full HTTP stack on Postgres 17
  scenarios/{commerce,jobs,lifecycle}/              less-travelled use-case paths on Postgres
  testkit/                shared harness of e2e and scenarios
  contract/ (+ testdata/consumers)                  produced payloads vs consumers' schemas
  helm/ (+ testdata)      chart contract (tag helm)
deploy/
  helm/                   chart 0.7.0 (templates/, files/ rule and dashboard copies, values{,-dev,-staging,-prod}.yaml)
  monitoring/             alerts, recording and SLO rules, schema-registry alerts, Prometheus Adapter rule, dashboards/
  iam/                    IRSA role policy (platform Terraform owns the role)
docs/
  lld/                    the LLD and billing-open-items.md
  runbooks/               26 runbooks + README index
  observability/          metric-registry.md (generated), registry-proposals.md
  swagger/                openapi.json (generated)
  invariants-coverage.md
scripts/                  init-db.sql, init-floci.sh, check-first-deploy.sh, testclock/
.githooks/                pre-commit, pre-push
.github/                  workflows/ (7), scripts/, schema-gov-requirements.txt, CODEOWNERS, templates
.claude/                  Claude Code notes, hook, /ci-local skill, reviewer agents
```

Binaries are built into `bin/` (`make build`); never commit one. `.gitignore` also ignores root-level binaries from a bare `go build ./cmd/<name>` with root-anchored entries (`/server`, …), so the `cmd/server/` sources stay visible.

### Dependency rules (enforced in CI)

| Rule | Check |
|---|---|
| `domain` ← `port` ← `app` ← adapters ← `bootstrap` / `cmd`; adapters never import each other (except the provider registry → stripe, manual) | `make arch-lint` (`.go-arch-lint.yml`) |
| Only `adapters/provider/stripe` imports stripe-go (`BIL-PCI-1`) | `check-stripe-imports.sh` |
| Database only through platform-pgcommon, and pgcommon only in `adapters/postgres`, `bootstrap`, `certify`, `cmd/*` (`BIL-OPS-1`, `BIL-OPS-11`) | depguard `database`, `database-boundary`; forbidigo; `check-pgcommon.sh` |
| Events, outbox and dedup only through platform-events, behind the messaging boundary (`BIL-OPS-2`) | depguard `events`, `events-glue`, `events-boundary`; `check-outbox-access.sh` |
| Logs, metrics, traces only through platform-gincommon (`BIL-OPS-10`) | depguard `observability`, `metrics`; forbidigo |
| Errors only through platform-gincommon (`BIL-API-6`) | `check-problem-details.sh` |

### Storage and messaging

- **PostgreSQL 17:**
  - 25 Billing tables, 18 of them tenant-scoped with `FORCE ROW LEVEL SECURITY`;
  - platform-events' `outbox_events`, `outbox_dead_letters` and `processed_events`, plus platform-audit's `pending_audit_entries`;
  - two pools: `billing_app` (RLS) and `billing_system` (BYPASSRLS, for the listed cross-tenant paths only);
  - PgBouncer in transaction mode for the app pool; migrations go direct.
- **No shared cache.** One advisory in-process cache exists: the Entitlements display cache, 60 s, used by the B-P19 pack list.
- **SNS:**
  - Billing publishes `billing-events` through the ordered outbox, keyed by tenant;
  - Glue framing is applied at publish time.
- **SQS:**
  - Billing consumes `tenant-billing-q` (from `iam-tenant-events`) and `usage-billing-q` (from `usage-events`);
  - each queue has a `-dlq`.
- **Glue Schema Registry:** `billing-events`, with versions resolved by definition at startup.

### Shared library dependencies

| Library | Version | Used for |
|---|---|---|
| `platform-gincommon/v2` | v2.7.0 | HTTP middleware, RFC 9457 problems, logger, metrics registry and types, tracing |
| `platform-pgcommon/v2` | v2.0.2 | Pools, GUC binding, transactions with retry, SQLSTATE helpers, migration runner |
| `platform-events/v2` | v2.3.0 | Envelope, ordered outbox and runner, SNS publisher, SQS consumers, DLQ, inbox dedup, Glue decode |
| `platform-audit` | v0.1.2 | AL-5 audit entries in outbox mode, delivery runner |
| `stripe-go/v87` | v87.2.0 (Stripe API `2026-11-18.endive`) | Stripe adapter only (adapter version 1.1.0, re-certification pending) |
| `gin-gonic/gin` | v1.12.0 | HTTP engine (built with `gin.New()`, never `gin.Default()`) |
| `go.opentelemetry.io/otel` | v1.47.0 | Behind gincommon; named directly only in `adapters/observability` |

---

## Integrating with other services

### 1. Prerequisites

| Caller | Requirement |
|---|---|
| Tenant UI | Go through the gateway, which injects `x-user-id`, `x-tenant-id` and `x-tenant-roles` |
| Signup BFF (B-I1) | Run as `bff/bff-signup` on the mesh with sidecar injection, so XFCC carries its certificate. NetworkPolicy admits only that pod |
| Operator tooling | Go through the operator gateway (platform realm, `platform_operator`) — open only once OP-G2 / OP-G3 are live |

Billing's own outbound identity is `commercial-system` (CD-19). It is rejected inbound on every route (`BIL-AUTH-4`).

### 2. Signup BFF — direct paid signup (B-I1)

The contract is in LLD §5.4.2. In brief:
- `BILLING_TIMEOUT_MS` ≥ 15000.
- `Idempotency-Key: charge:<tenant_id>`, with the same key and body on every retry. That includes the retry after 3-D Secure, after a timeout and after a `503`.
- The BFF never provisions with the id returned on `409 SUBSCRIPTION_ALREADY_EXISTS`.
- After `201`, the BFF calls RP-2 and then Core I-1. Billing confirms the sale when `TenantConverted(direct_signup=true)` arrives. If it doesn't arrive within 30 min, `BillingConversionSagaOverdue` pages.

### 3. IAM Core (Org & Membership) — reads and events

| Direction | What | Notes |
|---|---|---|
| Billing → Core | **I-11** `GET /api/v1/internal/tenants/:id/seat-usage` | Seat floor for conversion, reactivation, renewal; display (fields `null` on failure) |
| Billing → Core | **I-18** `GET /api/v1/internal/tenants/:id/plan-change-check?target_plan=` | Every downgrade, at request and at effect; `503` fails closed |
| Billing → events → Core | `billing-orgm-q` | `TenantPlanChanged`, `TenantSeatsChanged`, `TenantPaymentPastDue`, `TenantSubscriptionCancelled`, `TenantReactivated` — snapshots, recency by envelope `time` |

### 4. Realm Provisioner — conversion and lifecycle

| Direction | What | Notes |
|---|---|---|
| Billing → RP | **RP-3** `POST /api/v1/internal/tenants/:id/convert` | Only after server-side payment confirmation; key `convert:<tenant_id>:<billing_subscription_id>`; saga retries with backoff |
| Billing → RP | **RP-4** `POST …/revert-conversion` | Operator-approved reversal (B-O8), within 7 days |
| Billing → RP | **RP-11** `GET …/realm` | Operator saga view (B-O7) only |
| RP → events → Billing | `tenant-billing-q` | `TenantConverted` (confirms the saga), `TenantSuspended`, `TenantReactivated`, `TenantOffboarded` |
| Billing → events → RP | `billing-realm-q` | `TenantReactivated {source: billing}` |

### 5. Entitlements — eligibility and add-ons

| Direction | What | Notes |
|---|---|---|
| Billing → Entitlements | `GET /api/v1/internal/tenants/:id/entitlements` | Add-on eligibility: fresh on purchase, 60 s cache for the pack list. Egress restricted to this route. Identity and header contract in LLD §10.2 |
| Billing → events → Entitlements | `billing-entitlements-q` | `TenantPlanChanged`, `TenantSubscriptionCancelled`, `TenantReactivated`, `TenantQuotaAddOnChanged {purchase \| refund}` (idempotent on `billing_ref`; refund `purchase_ref` is a UUID) |

The `TenantQuotaAddOnChanged` contract is tested in CI against Entitlements' consumed schema, both vendored and live from its `main` branch.

### 6. Audit Log, Notification, AuthZ Enrichment

| Consumer | Queue | Events |
|---|---|---|
| Audit Log | `billing-audit-q` | All six event types |
| Audit Log | AL-5 (platform-audit) | Operator and global changes, written in the business transaction |
| Notification | `billing-notification-q` | `TenantPaymentPastDue`, `TenantSubscriptionCancelled` |
| AuthZ Enrichment | `billing-authz-q` | `TenantPlanChanged`, `TenantSubscriptionCancelled`, `TenantReactivated` |

### 7. Subscribing to `billing.events`

| Event | Trigger | Payload highlights |
|---|---|---|
| `TenantPlanChanged` | Upgrade paid, or a scheduled downgrade applied | `plan` (absolute) |
| `TenantSeatsChanged` | Conversion (+120 s), seat purchase, renewal top-up or reduction, projection repair | `licensed_seats` (absolute), `reason`, deferral fields |
| `TenantPaymentPastDue` | Cycle invoice failed (`active → past_due`) | `past_due_since` |
| `TenantSubscriptionCancelled` | Subscription ended (customer, auto-renew off, dunning exhausted) | `cancelled_at` |
| `TenantReactivated` | `past_due → active`, or a cancelled subscription settled | `source: billing` |
| `TenantQuotaAddOnChanged` | Add-on purchase confirmed, or an add-on refund succeeded | `resource`, `delta`, `reason`, `billing_ref`, `purchased_at` / `purchase_ref` |

**Envelope:** platform-events' CloudEvents-style envelope, with:
- `id`, a UUID v7 and the dedup key;
- `source: commercial-billing`;
- `subject` = `tenant_id`;
- `time`: Billing's database clock at commit;
- `actor`, `trace_id` and `dataschema` (the Glue schema version).

**Wire format:** `data` is Glue-framed: `[0x03][0x00][16-byte schema version UUID]` followed by the JSON payload. Decode with `events.GlueDecodeCodec`. Dev runs publish plain JSON.

**Delivery and ordering:**
- Delivery is at least once. Deduplicate on the envelope `id` in the transaction that applies the event.
- Billing publishes one tenant's events in commit order, but SNS/SQS delivery is not ordered end to end. Apply snapshots by recency on `time`.
- See the consumer checklist in [`ARCHITECTURE.md`](ARCHITECTURE.md#consumer-conformance-checklist).

### 8. Handling errors

- Branch on `errorCode` (see the example above). The complete taxonomy is LLD §17.
- On `503` and `409 REQUEST_IN_PROGRESS`, retry with the same `Idempotency-Key` after `Retry-After`.

### 9. Background work you may observe

| Work | Where | Effect |
|---|---|---|
| Webhook processor | server loop | Applies ledger rows, per tenant in provider order |
| Saga worker | server loop | Calls RP-3 for due conversion sagas |
| Lifecycle follow-ups | server loop | Post-commit provider steps (for example, the default payment method after B-P16) |
| Reconciliation | server loop | Runs queued B-O5 runs |
| Delayed emissions | server loop | Publishes `TenantSeatsChanged` 120 s after a conversion |
| Consumers | server loops | `tenant-billing-q`; `usage-billing-q` only with `USAGE_BILLING_CONSUMER_ENABLED=true` (default `false`) |
| `saga-sweeper` | CronJob, every minute | Pages `BillingConversionSagaOverdue` once per saga |
| `seat-renewal`, `downgrade-apply`, `renewal-repricing` | CronJobs | Apply scheduled reductions, downgrades and price changes at renewal |
| `purchase-expiry` | CronJob, every 5 min | Expires abandoned checkouts, purchases and 3-D Secure attempts |
| `seat-projection-check` | CronJob, twice an hour | Re-emits the seat snapshot when Core's projection drifts |
| `pii-minimisation` | CronJob, daily | Nulls billing-contact PII after the retention window; never deletes a row |
| `outbox-prune`, `processed-events-prune`, `idempotency-prune` | CronJobs, daily | Operational retention |

---

## Local development

### Prerequisites

- Go 1.26.11, Docker, `make`.
- `GOPRIVATE=github.com/BCBP-SOLUTIONS-FZC-LLC/*` and `GONOSUMDB` (the Makefile exports both), plus an SSH key registered with the BCBP org for the private platform modules.
- For `make helm-check`: `helm` v3.19+, `promtool` and `kubeconform` (installed if missing).

### Setup

```bash
git clone https://github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service
cd commercial-billing-service
cp .env-example .env   # values match docker-compose.yml; never commit .env
make docker-up         # Postgres 17 :5634 · PgBouncer :5633 · floci :4667 · stripe-mock :12111
make migrate           # outbox + inbox + audit-outbox schemas, then 000001–000007
make run               # API on :8180, metrics on :9190 (from .env-example)
```

### Common commands

| Command | Description |
|---|---|
| `make help` | Every target, grouped |
| `make setup` | Copy `.env-example` to `.env` if missing, and install the git hooks |
| `make install-hooks` | (Re)install `.githooks/pre-commit` (`.env` refusal, tidy, fmt-check, lint, guards, openapi-check) and `.githooks/pre-push` (`make trivy`) |
| `make tidy` / `make fmt` / `make fmt-check` / `make vet` | Go basics; `fmt-check` mirrors CI and does not modify files; `vet` also runs with every test build tag |
| `make mod-verify` / `make vuln-check` | `go mod verify`; govulncheck (needs network) |
| `make lint` | golangci-lint through `go tool` (pinned in `go.mod`), default build and every test build tag (`integration`, `helm`) |
| `make arch-lint` | `go-arch-lint` against `.go-arch-lint.yml` (installs v1.15.0 if missing) |
| `make guards` | `check-stripe-imports.sh`, `check-outbox-access.sh`, `check-problem-details.sh`, `check-pgcommon.sh` |
| `make test` | All packages with `-race` — unit, HTTP, contract, OpenAPI golden, observability (no Docker) |
| `make test-integration` | `./test/...`, `./internal/...` and `./cmd/...` with tag `integration` — Postgres, flows, e2e, scenarios, bus, stripe-mock, certification (Docker; `INT_PARALLEL` packages at once, default 4) |
| `make test-postgres` / `test-flows` / `test-bus` / `test-stripemock` / `test-e2e` / `test-helm` | One integration suite at a time (verbose) |
| `make race` | `make test` + `make test-integration` |
| `make test-contract` | Produced payloads vs Billing's and consumers' schemas |
| `make metrics-lint` | Observability standard: metricslint on the scrape, every referenced metric registered or exposed, inventory current |
| `make metrics-inventory` | Regenerate `docs/observability/metric-registry.md` |
| `make openapi` / `make openapi-check` | Regenerate / verify `docs/swagger/openapi.json` from the route table |
| `make schema-extract` / `schema-check` / `schema-validate` | schema-gov 0.4 (Docker): `api/asyncapi.yaml` → embedded schemas |
| `make schema-diff CURRENT=… PROPOSED=…` | Compatibility diff of two schema versions |
| `make schema-register` / `make schema-verify` | Register the six produced schemas in Glue `billing-events` / check each definition is registered and `AVAILABLE` before a deploy (AWS or floci) |
| `make helm-check` | Helm lint, kubeconform, promtool, chart contract tests (`test/helm`) |
| `make certify-rehearsal` | `cert-1.0` provider certification suite against stripe-mock |
| `make testclock` | Stripe Test Clock rehearsal against staging ([runbook](docs/runbooks/test-clock-rehearsal.md)) |
| `make check-first-deploy ENV=<env>` | LLD §19.1 first-deployment order, read-only |
| `make cover` / `make cover-all` | Coverage over the production packages: Docker-free suites / including integration |
| `make cover-gate` | Fail if coverage over `internal/` + `pkg/` is below 96% (after `cover-all`) |
| `make cover-func` / `make cover-html` | Per-function summary / HTML report |
| `make build` | `bin/commercial-billing`, `bin/jobs`, `bin/migrate`, `bin/certify` |
| `make docker-build` / `make trivy` | Build the image (needs `GO_PRIVATE_TOKEN` or `gh auth`) / build and Trivy-scan it |
| `make pin-base-images` | Pin the Dockerfile base images to SHA digests |
| `make certify-adapter-version` | Print the Stripe adapter version certification is bound to |
| `make ci` | `fmt-check` + `vet` + `lint` + `arch-lint` + `guards` + `test` + `build` |
| `make docker-up` / `make docker-down` / `make docker-reset` | Start / stop / wipe and restart the local dependencies |
| `make migrate` / `make run` | Migrate / run the server against `.env` |
| `make clean` | Remove `bin/` and coverage files |

### Running a single test

```bash
go test ./internal/adapters/http -run TestB_I1 -count=1 -v
go test ./internal/domain -run TestBIL_SEAT -count=1 -v
go test ./test/postgres -tags=integration -run TestBIL_OPS_11_AccountExistsIsAPortError -count=1 -v
go test ./test/flows -tags=integration -run TestConversionFlow -count=1 -v
```

### Calling the API locally

In dev, operator routes are open (`APP_ENV=dev`), and the provider for B-O1 is `manual`, so no Stripe account is needed.

```bash
# After make docker-up, make migrate and make run (API on :8180):
TENANT=$(python3 -c 'import uuid; print(uuid.uuid4())')
OWNER=$(python3 -c 'import uuid; print(uuid.uuid4())')
OPERATOR=$(python3 -c 'import uuid; print(uuid.uuid4())')

# B-O1: operator conversion stub — creates an active subscription on the manual provider
curl -i -X POST http://localhost:8180/api/v1/operator/tenants/$TENANT/conversions \
  -H "Content-Type: application/json" -H "Idempotency-Key: stub-$TENANT" \
  -H "x-user-id: $OPERATOR" -H "x-tenant-id: $TENANT" -H "x-tenant-roles: platform_operator" \
  -d '{"plan":"pro","interval":"month","licensed_seats":10,"reason":"local walkthrough of the conversion stub"}'

# B-P1: the subscription as the tenant owner sees it
curl -i http://localhost:8180/api/v1/tenants/$TENANT/subscription \
  -H "x-user-id: $OWNER" -H "x-tenant-id: $TENANT" -H "x-tenant-roles: tenant_owner"

# B-P1 for another tenant — 403 INSUFFICIENT_ROLE before any query (BIL-API-2)
curl -i http://localhost:8180/api/v1/tenants/$TENANT/subscription \
  -H "x-user-id: $OWNER" -H "x-tenant-id: $(python3 -c 'import uuid; print(uuid.uuid4())')" -H "x-tenant-roles: tenant_owner"

# B-I1 — identity check only. With FEATURE_DIRECT_SIGNUP=false (the default) the route is not
# registered (404). With it on, a wrong or missing X-Forwarded-Client-Cert answers 403
# INSUFFICIENT_ROLE, and the right one passes the caller check, but the charge then needs a
# certified Stripe provider, which a local run doesn't have (certification gate, BIL-PROV-1).
# The full path is covered by TestB_I1 and test/flows/direct_signup_test.go.
curl -i -X POST http://localhost:8180/api/v1/internal/subscriptions/authorize-and-charge \
  -H "Content-Type: application/json" -H "Idempotency-Key: charge:$TENANT" -H "x-tenant-id: $TENANT" \
  -H 'X-Forwarded-Client-Cert: By=spiffe://cluster.local/ns/billing/sa/commercial-billing;URI=spiffe://cluster.local/ns/bff/sa/bff-signup' \
  -d "{\"tenant_id\":\"$TENANT\",\"plan_code\":\"pro\",\"licensed_seats\":5,\"payment_method_ref\":\"pm_card_visa\"}"
```

Interactive API docs: `GET /openapi.json` (paste it into any OpenAPI viewer) and `GET /asyncapi`.

---

## Testing domain events locally

Every state change publishes through the **transactional outbox → SNS → SQS** pipeline.

### How the pipeline works

```
HTTP write / webhook processor / consumer / CronJob
    │
    ▼
use case ──(same tx, tenant lock)──▶ outbox_events (Postgres)
                                          │
                                   outbox runner (OUTBOX_POLL_INTERVAL, 500 ms)
                                          │
                                          ▼
                             SNS billing-events (floci)
                                          │
              filtered fan-out to billing-orgm-q, billing-realm-q, billing-entitlements-q,
              billing-audit-q, billing-authz-q, billing-notification-q (local inspection queues)
```

**Outbound guarantees:**
- An outbox insert is atomic with the business write, so a `2xx` that changed state guarantees an `outbox_events` row.
- The payload is validated by its domain `Validate()` before the insert, and against the embedded schema at publish.
- The Glue header is added only at publish time, and only outside dev.

**Inbound pipeline,** per queue:
1. platform-events decodes the envelope and strips the Glue header.
2. The payload is checked against Billing's embedded consumed schema.
3. `inbox.Store.Process` claims the envelope `id` inside the handler's tenant transaction.
4. The handler runs.

Schema violations, undecodable bodies and future-dated events (> `MAX_LIFECYCLE_EVENT_SKEW_SECONDS`) go to the queue's DLQ immediately. Other errors are retried, then dead-lettered at the 4th receive (`SQS_MAX_RECEIVE_COUNT`).

### Step 1 — Start infrastructure

```bash
make docker-up
```

`scripts/init-floci.sh` runs as floci's ready-hook and provisions:
- the topics `billing-events`, `iam-tenant-events` and `usage-events`;
- Billing's inbound queues `tenant-billing-q` and `usage-billing-q`;
- the six `billing-events` subscriber queues (each with a `-dlq`, `maxReceiveCount` 5, filtered on `EventType`);
- the Glue registry `billing-events` with the six produced schemas, taken straight from `internal/adapters/eventbus/schemas`.

### Step 2 — Verify SNS, SQS and Glue

```bash
docker compose exec floci aws --region ap-south-1 sns list-topics
docker compose exec floci aws --region ap-south-1 sqs list-queues
docker compose exec floci aws --region ap-south-1 glue list-schemas --registry-id RegistryName=billing-events
```

### Step 3 — Run a conversion end to end

The operator stub (B-O1) starts a conversion saga. The Realm Provisioner isn't running locally, so the saga's RP-3 call backs off; the `TenantConverted` you publish confirms the saga anyway (`rp_convert_pending → confirmed`). 120 s later (`SEATS_AFTER_CONVERSION_DELAY`) Billing publishes `TenantSeatsChanged`.

```bash
# 1. B-O1 (from "Calling the API locally"): note billing_subscription_id in the 202
SUB=<billing_subscription_id from the B-O1 response>

# 2. Publish RP's TenantConverted on iam-tenant-events (fields per
#    internal/adapters/eventbus/schemas/iam_tenant_converted.json)
NOW=$(date -u +%Y-%m-%dT%H:%M:%SZ)
docker compose exec floci aws --region ap-south-1 sns publish \
  --topic-arn arn:aws:sns:ap-south-1:000000000000:iam-tenant-events \
  --message-attributes '{"EventType":{"DataType":"String","StringValue":"TenantConverted"}}' \
  --message "{\"id\":\"$(python3 -c 'import uuid; print(uuid.uuid4())')\",\"type\":\"TenantConverted\",\"specversion\":\"1\",\"source\":\"iam-realm-provisioner\",\"subject\":\"$TENANT\",\"tenant_id\":\"$TENANT\",\"time\":\"$NOW\",\"data\":{\"tenant_id\":\"$TENANT\",\"realm\":\"tenant-local\",\"plan\":\"pro\",\"billing_subscription_id\":\"$SUB\",\"users_migrated\":1,\"direct_signup\":false}}"

# 3. The delayed seat event, then the outbox row once it is published
docker compose exec postgres psql -U postgres -d billing \
  -c "SELECT event_type, due_at, status FROM scheduled_emissions ORDER BY created_at DESC LIMIT 3" \
  -c "SELECT id, event_type, ordering_key, published_at FROM outbox_events ORDER BY created_at DESC LIMIT 3"

# 4. Read what Core would receive (the message reappears after the visibility timeout)
docker compose exec floci aws --region ap-south-1 sqs receive-message \
  --queue-url http://localhost:4566/000000000000/billing-orgm-q --max-number-of-messages 5 \
  --message-attribute-names All
```

Publishing the same `TenantConverted` again (same envelope `id`) does nothing: `processed_events` already holds it. Other lifecycle events work the same way. Take the required fields from their `iam_*.json` schema; `TenantSuspended`, for example, needs `realm`, `suspended_at` and `source`.

### Troubleshooting events

| Symptom | Likely cause |
|---|---|
| Server fails at startup resolving Glue versions | A produced schema is not registered in `billing-events` (outside dev) — register it first; locally, `make docker-down && make docker-up` re-runs the hook |
| Outbox rows never get `published_at` | `SNS_BILLING_EVENTS_TOPIC_ARN` unset or wrong (in dev without a topic the runner is not started) |
| A consumed message lands in `<queue>-dlq` at once | Payload fails the consumed schema, envelope undecodable, or `time` more than 300 s in the future |
| Consumed event applied but nothing changed | Recency guard: an event older than `billing_accounts.iam_last_event_at` is skipped (`BIL-EVT-3`) |
| Duplicate delivery did nothing | Expected — `processed_events` already holds the envelope `id` |

---

## Testing

| Suite | Command | Needs |
|---|---|---|
| Unit, HTTP, contract, observability | `make test` | — |
| Postgres (RLS, constraints, triggers, repositories), flows, scenarios (`test/scenarios/{commerce,jobs,lifecycle}`), bus, stripe-mock, certification, end to end | `make test-integration` | Docker |
| End to end only: the full HTTP stack on Postgres 17 (harness `test/testkit`) | `make test-e2e` | Docker |
| Helm chart contract | `make helm-check` | helm, promtool |
| Stripe Test Clock (staging) | `make testclock` | Staging and Stripe test mode |

### Canonical tests (do not break)

| Test | Guards |
|---|---|
| `TestBIL_*` (named per invariant) | Every `BIL-*` invariant — the map is [`docs/invariants-coverage.md`](docs/invariants-coverage.md) |
| Proration golden tests (`internal/domain`) | LLD §14.1 examples, including the below-minimum deferral (`BIL-SEAT-5`) |
| `TestBIL_AUTH_RoleRouteMatrix` | Every role × tenant route pair (§5.2) |
| `TestOpenAPIGolden` | `docs/swagger/openapi.json` equals the route table |
| `TestEveryErrorIsAProblem` | RFC 9457 on every error path |
| RLS suite (`test/postgres`) | `FORCE` RLS on every tenant table; cross-tenant, missing and invalid GUC cases (§14.5) |
| `test/contract` | Produced payloads vs Core, RP, Audit and Entitlements consumed schemas |
| `TestScrapePassesMetricsLint`, `TestReferencedPlatformMetricsAreRegistered`, `TestInventoryUpToDate` | Enterprise Platform Observability Standard |
| `TestShutdownBudgetFits` (`cmd/server`) | The shutdown sequence fits the 50 s left after the 10 s `preStop` |
| `test/helm` | Chart contract: names, policies, component labels, native sidecars, secrets per workload, render guards, CronJobs = `cmd/jobs`, alert → runbook links |

**Coverage is 97.8% of statements over every package, and 98.4% over `internal/` and `pkg/`** (about 580 top-level test functions), measured with all suites (`make cover-all`). `make cover-gate` fails below 96% over `internal/` and `pkg/`, and CI enforces the same gate (`validate-test.yml`). LLD §14.6 targets ≥ 90% at first release.

---

## Environment variables

`config.Load()` validates every key against bounds, and the server refuses to start on any missing, malformed or out-of-range value (LLD §12). Defaults below are the code's; Helm `values*.yaml` set the deployed values.

| Area | Variable | Default | Notes |
|---|---|---|---|
| Server | `APP_PORT` / `METRICS_PORT` | `8080` / `9090` | Must differ |
| | `REQUEST_TIMEOUT` | `30s` (5–60 s) | gincommon request deadline |
| | `SHUTDOWN_DRAIN_DELAY` | `2s` (0–5 s) | Time `/readyz` fails after `SIGTERM` before the listener closes |
| | `APP_ENV` / `ENVIRONMENT` | — (`dev` outside Kubernetes) | `dev`, `test`, `local` are dev-like (operator gate open, plain JSON events). Inside Kubernetes (`KUBERNETES_SERVICE_HOST` set) an unset value fails closed: the binary refuses to start |
| | `LOG_LEVEL` / `LOG_HASH_SALT` | `info` / secret | Salt for hashed identifiers in logs |
| | `OTEL_TRACES_SAMPLER_ARG` | `0.2` | |
| Feature flags | `FEATURE_DIRECT_SIGNUP` | `false` | B-I1 |
| | `FEATURE_TOKEN_ADDONS` | `false` | B-P19…B-P23 |
| | `FEATURE_SEAT_PURCHASE` | `true` | B-P6, B-P7 |
| | `OPERATOR_PATH_LIVE` | `false` | Operator routes `403` outside dev/test until true (`BIL-AUTH-6`) |
| | `AUTO_REVERT_ON_EFW` | `false` | Early fraud warnings never revert automatically |
| | `AUTO_PAUSE_ON_OPERATOR_SUSPENSION` | `true` | §8.10 |
| | `USAGE_BILLING_CONSUMER_ENABLED` | `false` | Starts the `usage-billing-q` consumer (BL-OQ-24) |
| Database | `PG_HOST`, `PG_PORT`, `PG_DBNAME`, `PG_USER`, `PG_PASSWORD`, `PG_SSLMODE`, `PG_BOUNCER_MODE`, `PG_STATEMENT_TIMEOUT`, `PG_LOCK_TIMEOUT` | — | App pool (`billing_app`), read by `pgcommon.ConfigFromEnv` |
| | `PG_SYSTEM_USER` / `PG_SYSTEM_PASSWORD` / `PG_SYSTEM_MAX_CONNS` | `billing_system` / secret / `4` | BYPASSRLS pool (password required outside dev) |
| | `PG_AUDIT_DRAINER_USER` / `_PASSWORD` / `_MAX_CONNS` | `billing_audit_drainer` / secret / `2` | platform-audit runner |
| | `MIGRATION_DATABASE_URL` | — | Direct to Postgres, never PgBouncer |
| | `RUN_MIGRATIONS` / `MIGRATE_TIMEOUT` | `false` / `10m` | Helm keeps `false`; the hook Job migrates with `MIGRATE_TIMEOUT` `7m` (below its `activeDeadlineSeconds` 480) |
| Events | `SNS_BILLING_EVENTS_TOPIC_ARN`, `SQS_TENANT_BILLING_QUEUE_URL`, `SQS_USAGE_BILLING_QUEUE_URL` | — | The usage queue URL is required only when `USAGE_BILLING_CONSUMER_ENABLED=true` |
| | `GLUE_REGISTRY_NAME` | `billing-events` | |
| | `SQS_MAX_RECEIVE_COUNT` / `SQS_WAIT_TIME_SECONDS` / `SQS_VISIBILITY_TIMEOUT` | `4` / `20` / `60s` | |
| | `MAX_LIFECYCLE_EVENT_SKEW_SECONDS` | `300` | Future-time clamp (`BIL-EVT-3`) |
| | `OUTBOX_POLL_INTERVAL` / `OUTBOX_BATCH_SIZE` / `OUTBOX_MAX_ATTEMPTS` / `OUTBOX_RETENTION_DAYS` | `500ms` / `50` / `5` / `8` | |
| | `PROCESSED_EVENTS_TTL_DAYS` | `8` | |
| | `SEATS_AFTER_CONVERSION_DELAY` / `EMISSION_POLL_INTERVAL` | `120s` / `5s` | `BIL-EVT-6` |
| | `AWS_REGION`, `AWS_ENDPOINT_URL` | — | floci locally |
| Provider | `STRIPE_API_KEY`, `STRIPE_WEBHOOK_SECRETS` | secret | Test keys only outside prod (`BIL-OPS-7`) |
| | `STRIPE_API_VERSION` / `STRIPE_TIMEOUT_MS` / `STRIPE_MAX_RPS` / `STRIPE_MAX_RETRIES` | pinned / `8000` / `25` / `2` | A version change needs a new certification |
| | `ENABLED_PAYMENT_PROVIDERS` / `DEFAULT_PAYMENT_PROVIDER` | `stripe,manual` / `stripe` | |
| | `PROVIDER_CERT_SUITE_VERSION` / `PROVIDER_REGISTRY_REFRESH` | `cert-1.0` / `60s` | Certification gate (`BIL-PROV-1`) |
| | `WEBHOOK_TOLERANCE` / `WEBHOOK_MAX_BODY_BYTES` / `WEBHOOK_POLL_INTERVAL` / `WEBHOOK_MAX_ATTEMPTS` | `300s` / `262144` / `500ms` / `10` | |
| Callees | `CORE_BASE_URL` / `CORE_TIMEOUT_MS` | — / `3000` | I-11, I-18 |
| | `RP_BASE_URL` / `RP_TIMEOUT_MS` | — / `30000` | RP-3, RP-4, RP-11 |
| | `ENTITLEMENTS_BASE_URL` / `ENTITLEMENTS_TIMEOUT_MS` | — / `1000` | Eligibility GET |
| | `TENANT_APP_BASE_URL` | — | Return URLs for hosted provider pages |
| Identity | `BFF_CALLER_PRINCIPAL` | `cluster.local/ns/bff/sa/bff-signup` | B-I1 caller |
| | `COMMERCIAL_SYSTEM_PRINCIPAL` | `00000000-0000-0000-0000-0000000000c1` | Billing's outbound identity |
| Money and timers | `DEFAULT_CURRENCY` | `USD` | |
| | `MIN_CHARGE_MINOR` / `PRORATION_BELOW_MINIMUM` | `200` / `free_until_renewal` | `BIL-SEAT-5` |
| | `REVIEW_THRESHOLD_MINOR` | `100000` | Adjustments above it raise a review alert |
| | `MAX_SEATS_PER_PURCHASE` | `1000` | |
| | `CHECKOUT_TTL` / `SEAT_PREVIEW_TTL` / `PURCHASE_ABANDON_AFTER` / `SCA_ABANDON_AFTER` | `1h` / `10m` / `30m` / `30m` | |
| | `SEAT_RENEWAL_LEAD` / `SEAT_RESYNC_AFTER` / `DOWNGRADE_RECHECK_LEAD` / `REPRICE_CUTOFF` | `24h` / `10m` / `2h` / `1h` | |
| | `SAGA_POLL_INTERVAL` / `SAGA_ALERT_AFTER` / `SAGA_MAX_BACKOFF` | `2s` / `30m` (fixed) / `5m` | `BIL-OPS-5` |
| | `DUNNING_FINAL_ACTION` | `cancel` | |
| | `IDEMPOTENCY_TTL_HOURS` | `72` | `BIL-IDEM-3` |
| | `PII_MINIMISE_OFFBOARDED_DAYS` / `PII_MINIMISE_ABANDONED_DAYS` | `90` / `30` | `pii-minimisation` |
| Jobs | `PUSHGATEWAY_URL` | — | CronJob metrics; empty → the job logs its outcome instead |

---

## Security

| Area | Control |
|---|---|
| PCI scope | SAQ A. No PAN, CVV, expiry, provider key or webhook secret in any table, log, event, span or error (`BIL-PCI-2`); ledger payloads allow-listed (`BIL-PCI-3`); `client_secret` only in the 3-D Secure action returned to the payer (`BIL-API-4`) |
| Provider code isolation | stripe-go only in `adapters/provider/stripe` (`BIL-PCI-1`) |
| Webhooks | Signature, tolerance, `livemode` verified before storage; tenant from Billing's mapping; state re-read from the provider (`BIL-PAY-2..4`); gateway rate limit 50 rps per client IP |
| Purchase settlement | One provider invoice settles at most one purchase (`BIL-PAY-6`); a purchase is granted only for an invoice paid in its amount and currency, otherwise parked for an operator (`BIL-PAY-7`, `BillingPaymentParked`) |
| Tenant isolation | Headers-only identity, path-tenant match, `FORCE` RLS, transaction-local GUC, sysPool only on listed paths (`BIL-API-1..2`, `BIL-RLS-1..3`) |
| Service identity | B-I1 admits only the BFF's mTLS principal; Billing's outbound identity is rejected inbound |
| Operator actions | `platform_operator` only, gated by `OPERATOR_PATH_LIVE`; every mutation needs a reason and writes an AL-5 audit entry; certification approval needs a second person (`BIL-PROV-4`) |
| Secrets | `ExternalSecret` in Kubernetes; `.env` never committed (a Claude Code hook also blocks editing it) |
| Production changes | A reviewer from the PCI owner group (HLD §15.4); provider adapter changes need a new certification before release |

Full model: [`ARCHITECTURE.md`](ARCHITECTURE.md#threat-model) and LLD §10.

---

## Observability

### Metrics

The Enterprise Platform Observability Standard (LLD §11.2):

- **Tier 1 — `platform_*`:** registry names only, through gincommon's shared constructors and the platform libraries:
  - HTTP;
  - messaging (`platform_messages_*`, `platform_dlq_messages_total`, `platform_duplicate_messages_total`);
  - outbox;
  - database pools and queries;
  - platform-audit;
  - `platform_dependency_request_seconds{dependency=stripe|core|realm_provisioner|entitlements, operation, outcome}`.
- **Tier 2 — `commercial_*`:** none. Nothing is ratified; the candidates are in [`docs/observability/registry-proposals.md`](docs/observability/registry-proposals.md).
- **Tier 3 — `commercial_billing_*`:** 33 metrics, built with gincommon's validated constructors:
  - webhooks (count, signature failures, processing seconds, backlog);
  - sagas (duration, in flight, overdue, RP-3 attempts);
  - subscriptions by status, plan and provider;
  - purchases, seats, proration deferrals, add-ons;
  - payment failures, dunning;
  - reconciliation findings, collection pauses, held invoices, invoice dispositions;
  - scheduled emissions, provider certification, background loops running (`commercial_billing_background_loop_running{loop}`);
  - events emitted, operator actions, cross-tenant job runs, purchase-to-event seconds.

**Labels:** `domain="commercial"`, `service="commercial-billing"` and `environment` are injected centrally. High-cardinality labels (`tenant_id`, `user_id`, …) are refused.

**Inventory:** [`docs/observability/metric-registry.md`](docs/observability/metric-registry.md), generated by `make metrics-inventory`. Commit it with any metric change.

`make metrics-lint` (also run by `make test`) is the CI gate for the standard.

### Alerts and dashboards

| Asset | Location |
|---|---|
| Alert rules — 20 alerts | `deploy/monitoring/app-alerts.yml` |
| Recording and SLO rules | `recording-rules.yml`, `slo-rules.yml` |
| platform-audit rules (vendored) | `platform-audit.rules.yml` |
| Chart copies | `deploy/helm/files/` |
| "Commercial / Billing" Grafana dashboard | `deploy/monitoring/dashboards/commercial-billing.json` (shipped as a ConfigMap) |
| "Platform audit delivery" dashboard (shared, templated on `service`) | `deploy/monitoring/dashboards/platform-audit.json` |
| Schema registration alerts (4, from `schema-gov metrics` via the Pushgateway) | `deploy/monitoring/schema-registry-alerts.yml` |
| Prometheus Adapter rule for the HPA's webhook-backlog metric | `deploy/monitoring/prometheus-adapter-rule.yaml` |
| AWS IAM policy for the IRSA role (SNS, SQS, DLQs, Glue read) | `deploy/iam/` |

The 20 alerts (27 rules: `BillingWebhookBacklog` has two thresholds, `BillingCronJobNotSucceeding` one rule per job cadence, `BillingCronJobNeverSucceeded` one SEV-2 rule for `saga-sweeper` and one SEV-3 rule for the others):
- `BillingConversionSagaOverdue`, `BillingSagaFailed`, `CommercialPurchaseNotActive`;
- `BillingWebhookSignatureFailures`, `BillingWebhookBacklog`;
- `BillingOutboxDeadLetter`, `BillingDLQNotEmpty`, `BillingQueueBacklog`, `BillingQueueStalled`, `BillingOutboxLag`;
- `BillingBackgroundLoopStopped`, `BillingMetricsSnapshotStale`, `BillingCronJobNotSucceeding`, `BillingCronJobNeverSucceeded`;
- `BillingI18Unavailable`, `BillingProviderUnavailable`, `BillingProviderNotCertified`;
- `BillingSuspensionDraftsAwaitingDecision`, `BillingReconciliationDrift`, `BillingSeatProjectionDrift`.

Log-based alerts (Loki rules owned by the observability team, not in this file): `BillingReversalDecisionNeeded` (SEV-2), `BillingReviewTrigger` (no page, log line `alert=BillingOperatorReviewNeeded`), and the SEV-2 `BillingPaymentParked` for a payment on a superseded checkout, after the subscription ended, or for another amount or currency than its purchase ([`operator-review.md`](docs/runbooks/operator-review.md)).

Each alert links to a runbook in [`docs/runbooks/`](docs/runbooks/).

### Tracing and logs

- **Tracing:** `gincommon.InitTracingWithConfig`, with the same service, domain and environment identity as metrics. It covers:
  - server spans from the middleware;
  - client spans with W3C propagation from `httpx`;
  - pgcommon query spans;
  - consumer spans linked to the producer's trace.
- **Logs:** gincommon's structured logger, with `trace_id` and `span_id` correlation. Logs carry no card data, secret or personal data; identifiers are hashed with `LOG_HASH_SALT` where §11.4 requires.

---

## Deployment

### Container image — three binaries

| Binary | Path in image | Purpose |
|---|---|---|
| `commercial-billing` | `/commercial-billing` (entrypoint) | HTTP server, consumers, outbox and audit runners, webhook processor, saga, follow-up, reconciliation and emission workers, registry refresh, metrics snapshot |
| `jobs` | `/jobs` | One-shot CronJob binary, `--job=<name>` for the ten §13.2 jobs; pushes its run's metrics to `PUSHGATEWAY_URL` |
| `migrate` | `/migrate` | One-shot schema migration (platform-events outbox and inbox, platform-audit outbox, then 000001–000007); exit 0 or 1 |

The image is a two-stage `Dockerfile`: a `golang:1.26.11-alpine` builder, then `gcr.io/distroless/static-debian13:nonroot` with no shell, running as a non-root user. `cmd/certify` is built by `make build` but is not shipped in the image.

### Helm chart

`deploy/helm` (chart 0.7.0, `kubeVersion >=1.30`; Istio ≥ 1.24 and Helm ≥ 3.19 required), namespace `billing`, with ServiceAccount and pod label `commercial-billing`:

- **Workload:**
  - Deployment: 2 replicas, HPA 2–4 on CPU 70% (plus an optional webhook-backlog external metric), PDB;
  - resources: CPU `100m` / `500m`, memory `256Mi` / `512Mi`;
  - `terminationGracePeriodSeconds: 60` with a native `preStop` sleep of 10 s (Kubernetes ≥ 1.30); the app's shutdown fits in the remaining 50 s (worst case 48 s: readiness fails first, `SHUTDOWN_DRAIN_DELAY`, then HTTP drain in parallel with consumers, outbox and audit runner, then pools, metrics listener, tracer and logger flush);
  - startup runs its deterministic checks (configuration, Glue schema versions, each enabled queue's `RedrivePolicy` outside dev) before the listeners open, so a bad deploy fails before it takes traffic;
  - probes with a 6 s timeout; liveness fails only when the audit runner is not alive (a stopped in-pod loop is restarted in-process with backoff 1 s → 5 min and alerted, `BillingBackgroundLoopStopped`);
  - ten CronJobs (`concurrencyPolicy: Forbid`), with small pools (`PG_MAX_CONNS` 3, `PG_MIN_CONNS` 0, `PG_SYSTEM_MAX_CONNS` 2);
  - the migrate pre-install / pre-upgrade hook Job, with its own hook ServiceAccount `commercial-billing-migrate` (no IRSA) and hook Secret holding only `MIGRATION_DATABASE_URL`, created at hook weight -10 and deleted on success or failure (Helm ≥ 3.19); `activeDeadlineSeconds` 480, `MIGRATE_TIMEOUT` 7m;
  - pods labelled `app.kubernetes.io/component` `server` / `cronjob` / `migrate`; the Service, PDB, ServiceMonitor, NetworkPolicy and AuthorizationPolicy of the server select `component=server` only;
  - secrets split per workload (server, jobs, migrate: each gets only the keys its binary reads); `automountServiceAccountToken: false`.
- **Mesh and network:**
  - Istio native sidecars for the server and the CronJobs (Istio ≥ 1.24); the migrate Job runs without a sidecar. The sidecar's inbound listener appends the verified client certificate to `X-Forwarded-Client-Cert` (`APPEND_FORWARD`, the sidecar default; `forwardClientCertDetails` in ProxyConfig applies to gateways only), and B-I1 reads its caller from the last element. `PeerAuthentication` `STRICT` on Billing's workloads keeps plaintext callers from writing that element themselves;
  - `metrics.scrapeMode`: `plaintext` (default, Prometheus outside the mesh: the metrics port is `PERMISSIVE`, serves only `/metrics`, and is admitted from `metrics.monitoringCIDRs` or by the NetworkPolicy alone) or `mesh` (`STRICT` everywhere, admitted by namespace);
  - NetworkPolicies per workload: server (B-I1 from `bff/bff-signup` only, Entitlements egress on the eligibility `GET` only, `/metrics` from the monitoring namespace, HTTPS to public addresses plus `networkPolicy.awsEndpointCIDRs` for VPC interface endpoints and an optional `networkPolicy.egressGateway`), CronJobs (DNS, istiod, Postgres, Core and RP, 443, Pushgateway, OTel) and migrate (DNS and 5432); AuthorizationPolicy on the server.
- **Gateway:** tenant and operator `HTTPRoute`s plus gateway policies; tenant routes 20 rps per tenant; B-W1 50 rps per client IP (one bucket per IPv4 and per IPv6 client; the gateway's `numTrustedHops` must let it see the real client), with an optional source-CIDR allow-list (`gateway.webhook.allowedSourceCIDRs`, Stripe's published webhook IPs).
- **Render-time guards:** every environment needs `metrics.scrapeMode` `plaintext` or `mesh`. For staging and prod the chart refuses to render with `PeerAuthentication`, `AuthorizationPolicy` or `NetworkPolicy` disabled, the placeholder account `000000000000`, a missing IRSA role ARN, empty SNS topic or tenant queue values (and the usage queue when its consumer is on), or empty `networkPolicy.postgresCIDRs`. `release.yml` passes these values for the production deploy only, from the `production` environment's `BILLING_*` variables; a staging deploy must supply them itself.
- **Monitoring and secrets:** `ServiceMonitor`, `PrometheusRule`, dashboard ConfigMap, one `ExternalSecret` per workload (keys from `secretKeys.server` / `jobs` / `migrate`).
- **Values:** `values-dev.yaml`, `values-staging.yaml` and `values-prod.yaml` over `values.yaml`.
- **Outside the chart** (platform-owned, references in this repo):
  - the IRSA role, whose policy is [`deploy/iam/`](deploy/iam/README.md); its ARN goes in `serviceAccount.annotations`;
  - the Prometheus Adapter `externalRules` entry ([`deploy/monitoring/prometheus-adapter-rule.yaml`](deploy/monitoring/prometheus-adapter-rule.yaml)). Without it, the HPA's webhook-backlog metric is unavailable and the HPA scales on CPU only.

### Migration safety

- **One runner:** the hook Job runs migrations once per release, so replicas run with `RUN_MIGRATIONS=false`.
- **Direct connection:** the migration lock is session-scoped, so `MIGRATION_DATABASE_URL` connects directly, never through PgBouncer.
- **Each library has its own tracking table:** Billing's is `billing_schema_migrations`.
- **Failure messages:** a dirty migration, or a schema newer than the image, fails with an operator hint (LLD §19.5).
- **First deployment:** the §19.1 order is checked by `make check-first-deploy`.

---

## CI

Seven workflows in `.github/workflows/`, modelled on the IAM services' pipeline:

| Workflow | Trigger | What |
|---|---|---|
| `ci.yml` | PRs and pushes to `main`, dispatch | Orchestrator: `validate-test` ∥ `validate-quality` ∥ `build-image` → `trivy` ∥ `smoke` → `pr-summary` (PRs) / `push` (main) |
| `validate-test.yml` | called by `ci.yml`, `release.yml` | `make test` (race) → `make cover-all` (every suite, testcontainers) → coverage gate **≥ 96%** over `internal/` + `pkg/` → arch-lint → OpenAPI drift → AsyncAPI ↔ schema sync |
| `validate-quality.yml` | called by `ci.yml`, `release.yml` | modules, workflow hygiene, fmt, tidy, vet, lint (all build tags), `make guards`, `make metrics-lint`, govulncheck, Dockerfile digest pins, `make helm-check` (Helm v3.21.4) |
| `release.yml` | `v*` tags, dispatch | validate → build (tag on `main` and = HEAD, CHANGELOG entry, Linux binaries) → **provider-certification gate** (LLD §14.6) → schema verify (production Glue) → image (Trivy, SBOM, SLSA provenance, Cosign) → deploy gate (Helm, deployed digest = published digest, 5xx < 1% for 2 min, else `helm rollback`) → GitHub Release |
| `schema-registry.yml` | schema changes on PRs and `main`, releases, dispatch | validate; diff against the registry (fails on a breaking change); register in Glue `billing-events` (staging on `main`, production on release; blocked by `SCHEMA_FREEZE` unless forced) and verify |
| `changelog-check.yml` | PRs touching `internal/`, `api/`, `deploy/`, `cmd/`, `pkg/` | `CHANGELOG.md` must change too |
| `contract-live.yml` | daily, PRs, dispatch | `test/contract` against Entitlements' consumed schema on its `main` |

**Image pipeline:**
- **Every run** builds the amd64 image into the cache (after Hadolint), then runs Trivy (fails on CRITICAL / HIGH / UNKNOWN with a fix) and produces a CycloneDX SBOM.
- **Smoke tests:** size; the server refuses to start without configuration; the three binaries are present; no shell in the image.
- **On `main`:** the image is pushed to `ghcr.io/bcbp-solutions-fzc-llc/commercial-billing` with provenance and an SBOM, signed keylessly with Cosign, and the signature is verified.

**Configuration:**
- **Repository secrets:** `GO_PRIVATE_TOKEN` (private modules, schema-gov source, read access to `commercial-entitlements-service`); optional `CI_REPO_READ_TOKEN` for checkouts (falls back to `github.token`).
- **Release and registry** (per environment, `staging` / `production`):
  - `CERTIFY_CHECK_DSN` (staging, read-only);
  - AWS through GitHub OIDC: variable `AWS_ROLE_ARN` (the role assumed). Access keys are only a documented, commented-out fallback until the role exists;
  - `KUBECONFIG_B64` (production);
  - optional `PUSHGATEWAY_URL` and `ALARM_SNS_ARN` (schema registration metrics and alerts);
  - variables `AWS_REGION`, `K8S_NAMESPACE`, `PROMETHEUS_URL`;
  - production variables read by the deploy gate (the chart refuses to render without them): `BILLING_IRSA_ROLE_ARN`, `BILLING_SNS_TOPIC_ARN`, `BILLING_TENANT_BILLING_QUEUE_URL`, `BILLING_POSTGRES_CIDRS`; optional `BILLING_USAGE_BILLING_QUEUE_URL` (while the usage consumer is off, BL-OQ-24), `BILLING_WEBHOOK_SOURCE_CIDRS`, `BILLING_AWS_ENDPOINT_CIDRS`, `BILLING_MONITORING_CIDRS`;
  - optional `SCHEMA_FREEZE` (blocks schema registration).

  The registry jobs are skipped until `AWS_REGION` is set.
- **Supply chain:** third-party actions are pinned by commit SHA, and checkouts don't persist credentials. schema-gov is built from a pinned commit with hash-locked Python dependencies (`.github/schema-gov-requirements.txt`, `pip --require-hashes`, no credentials). In the validate workflows `GO_PRIVATE_TOKEN` is used only in the module-download step, through a credential helper in that step's environment (never on disk), before any code from the checkout runs; later steps run with `GOPROXY=off`. A release tag must be on `main` (`verify-release-tag.sh`; also enable tag rulesets on `v*`). The registry build cache is written only from `main`.
- **Settings:** the `production` environment must require reviewers from the PCI owner group (HLD §15.4).

Repository templates: `CODEOWNERS` (PCI owner group on the provider adapters, webhook, certification and production values), a pull-request template, `SECURITY.md`, issue templates, and a disabled Dependabot configuration (`.github/dependabot.yml.disabled`).

---

## Docker

### What the bundled `docker-compose.yml` starts

| Container | Image | Host port | Purpose |
|---|---|---|---|
| `postgres` | `postgres:17-alpine` | `5634 → 5432` | Primary store; `scripts/init-db.sql` creates the roles |
| `pgbouncer` | `edoburu/pgbouncer` | `5633 → 5432` | Transaction pooling, admits `billing_app` only |
| `floci` | `floci/floci:2.1.0-compat` | `4667 → 4566` | SNS, SQS and Glue Schema Registry; `scripts/init-floci.sh` provisions the topology |
| `stripe-mock` | `stripe/stripe-mock` | `12111` | Stripe API mock for adapter and certification tests |

Host ports are offset from the IAM services' stacks so both can run side by side. The service itself runs with `make run`, not in compose.

### Building the service image

```bash
docker build -t commercial-billing:local \
  --secret id=go_private_token,src=<(echo "$GO_PRIVATE_TOKEN") \
  --build-arg BUILD_VERSION=$(git describe --tags --always --dirty) .
```

### Health and readiness

| Endpoint | Returns | Checks |
|---|---|---|
| `GET /healthz` | `200 {"status":"ok"}` / `503` problem | Platform-audit runner alive; no dependency |
| `GET /readyz` | `200` / `503` problem (`failed_check`, `checks`) | App pool and BYPASSRLS pool, each through `pgcommon.Pool.Health` |

### Minimum required environment variables

```bash
# PostgreSQL (app pool through pgcommon; roles from scripts/init-db.sql)
PG_HOST=localhost PG_PORT=5634 PG_DBNAME=billing PG_USER=billing_app PG_PASSWORD=<password>
PG_SYSTEM_PASSWORD=<password> PG_AUDIT_DRAINER_PASSWORD=<password>
MIGRATION_DATABASE_URL=postgres://billing_migrator:<password>@localhost:5634/billing?sslmode=disable

# Events
SNS_BILLING_EVENTS_TOPIC_ARN=arn:aws:sns:ap-south-1:<account>:billing-events
SQS_TENANT_BILLING_QUEUE_URL=…
# SQS_USAGE_BILLING_QUEUE_URL=… only with USAGE_BILLING_CONSUMER_ENABLED=true (default false, BL-OQ-24)

# Provider
STRIPE_API_KEY=rk_test_… STRIPE_WEBHOOK_SECRETS=whsec_…

# Callees
CORE_BASE_URL=… RP_BASE_URL=… ENTITLEMENTS_BASE_URL=… TENANT_APP_BASE_URL=…
```

---

## Cross-service dependencies

### 1. Synchronous outbound calls

Tenant reads (B-P1, histories, invoices from Postgres) need no other service, except B-P15, which reads payment methods live from the provider.

| Operation | Dependency | Adapter | Posture | On failure |
|---|---|---|---|---|
| Any charge, checkout, purchase, refund, payment-method page | Stripe | `provider/stripe` | fail-closed | `503 PROVIDER_UNAVAILABLE` + `Retry-After`; retry with the same key never double-charges |
| Conversion (B-P2), reactivation (B-P14), renewal count, top-up | Core I-11 | `iamclient` | fail-closed | `503 IAM_UNAVAILABLE`; renewal reduction retried, then skipped (renews at current count) |
| B-P1 / B-P10 display | Core I-11 | `iamclient` | fail-soft | Fields `null` |
| Downgrade (B-P4) and its application | Core I-18 | `iamclient` | fail-closed | `503 IAM_UNAVAILABLE`; `BillingI18Unavailable` |
| Conversion saga | Realm Provisioner RP-3 | `iamclient` | retry | Backoff with the same key; pages at 30 min |
| Reversal (B-O8), saga view (B-O7) | Realm Provisioner RP-4 / RP-11 | `iamclient` | fail-closed | `503` |
| Add-on purchase (B-P20) | Entitlements | `entitlementsclient` | fail-closed | `503 ENTITLEMENTS_UNAVAILABLE` |
| Pack list (B-P19) | Entitlements | `entitlementsclient` | fail-soft | `eligible: false` (60 s cache) |
| Audit entries | Audit Log (AL-5) | platform-audit outbox | queued | Entries wait in `pending_audit_entries`; requests never fail or wait |

### 2. Inbound callers

| Caller | Endpoints | Purpose |
|---|---|---|
| Tenant admin UI (through the gateway) | B-P1…B-P26 | Billing page, checkout, plan and seat changes, add-ons, invoices, contact |
| Signup BFF | B-I1 | Direct paid signup |
| Stripe | B-W1 | Provider callbacks |
| Platform operators (operator gateway) | B-O1…B-O17 | Corrections, refunds, reconciliation, price book, certification, suspension decisions |

### 3. Async event dependencies

**Consumed:**

| Queue | Producer | Events |
|---|---|---|
| `tenant-billing-q` | Realm Provisioner (`iam-tenant-events`) | `TenantConverted`, `TenantSuspended`, `TenantReactivated`, `TenantOffboarded` |
| `usage-billing-q` | Usage & Metering (`usage-events`) | `UsagePeriodClosed` (reference only; contract unverified, BL-OQ-24) |

**Produced** on `billing-events`:

| Consumer | Queue | Events |
|---|---|---|
| Core (Org & Membership) | `billing-orgm-q` | `TenantPlanChanged`, `TenantSeatsChanged`, `TenantPaymentPastDue`, `TenantSubscriptionCancelled`, `TenantReactivated` |
| AuthZ Enrichment | `billing-authz-q` | `TenantPlanChanged`, `TenantSubscriptionCancelled`, `TenantReactivated` |
| Realm Provisioner | `billing-realm-q` | `TenantReactivated` |
| Entitlements | `billing-entitlements-q` | `TenantPlanChanged`, `TenantSubscriptionCancelled`, `TenantReactivated`, `TenantQuotaAddOnChanged` |
| Audit Log | `billing-audit-q` | All six |
| Notification | `billing-notification-q` | `TenantPaymentPastDue`, `TenantSubscriptionCancelled` |

### 4. Infrastructure dependencies

| System | Role |
|---|---|
| PostgreSQL 17 (+ PgBouncer) | Primary store; RLS; outbox, inbox, audit outbox |
| AWS SNS | `billing-events` |
| AWS SQS | `tenant-billing-q`, `usage-billing-q` (each with a `-dlq`) |
| AWS Glue Schema Registry | `billing-events` — versions resolved by definition at startup |
| Stripe | Payment provider (UAE account; test keys outside prod) |
| Pushgateway | CronJob metrics |
| S3 (evidence bucket) | Provider certification evidence (`cmd/certify`) |

---

## Out of scope

| Concern | Where it lives |
|---|---|
| Tenant record, plan and seat projection, memberships, seat-cap enforcement | IAM Core (Org & Membership) |
| Realms, Keycloak, conversion of the identity side | Realm Provisioner |
| Quotas, effective entitlements, add-on balances, quota corrections and expiry | Entitlements |
| Usage counting | Usage & Metering |
| Card data, payment processing | The payment provider |
| Audit storage | Audit Log |
| Customer emails | Notification |
| Authentication, JWT, roles | Keycloak and IAM |

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for setup, how to extend the service, testing requirements, guardrails, and the PR and commit checklists. In short:

1. **Read the LLD section first.** Implement what it says, and propose an LLD change (with a revision row) before diverging.
2. **Name invariant tests after the invariant.** Every `BIL-*` invariant you touch needs a test whose name contains its ID, and a row in [`docs/invariants-coverage.md`](docs/invariants-coverage.md).
3. **Run `make ci` before pushing.** `make test-integration` too when you change Postgres, flows or the bus. The `/ci-local` Claude Code skill runs every CI job locally.
4. **Regenerate generated files instead of editing them:**
   - OpenAPI: `go test ./internal/adapters/http -run Golden -update`;
   - event schemas: `make schema-extract`;
   - metric inventory: `make metrics-inventory`.
5. **Follow the branch rules:** one branch per phase or flow (`feat/billing-phase-<n>-<slug>`); never commit to `main`; production changes need a PCI owner group reviewer.

| Document | Description |
|---|---|
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | Architecture narrative with diagrams |
| [`CONTRIBUTING.md`](CONTRIBUTING.md) | Setup, extending the service, tests, guardrails, PR checklist |
| [`CHANGELOG.md`](CHANGELOG.md) | What changed, by date and LLD revision |
| [`VERSIONING.md`](VERSIONING.md) | SemVer and the runtime contract, provider adapter versions, release blockers and process |
| [`docs/lld/commercial-lld-billing-service.md`](docs/lld/commercial-lld-billing-service.md) | The LLD — source of truth; §16 open items, §17 errors, §20 operations |
| [`docs/lld/billing-open-items.md`](docs/lld/billing-open-items.md) | What other teams still owe, by team |
| [`docs/invariants-coverage.md`](docs/invariants-coverage.md) | Invariant → enforcement → test |
| [`docs/runbooks/`](docs/runbooks/) | 26 runbooks and an index: alerts, operator review, reversal decision, DR, key rotation, certification, Test Clock |
| [`docs/observability/`](docs/observability/) | Generated metric inventory and registry proposals |
| [`.claude/CLAUDE.md`](.claude/CLAUDE.md) | Guidance for Claude Code in this repo (plus topic notes, hooks, `/ci-local`, reviewer agents) |

---

## License / ownership

BCBP Solutions FZC LLC — internal platform service, owned by the Commercial Team. Not for external distribution.
