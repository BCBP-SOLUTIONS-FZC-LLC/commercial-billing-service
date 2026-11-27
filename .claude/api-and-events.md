# API and Events

Summary only. The contracts are LLD §5 (API), §7 (events) and §18 (integrations); the route table is `internal/adapters/http/routes.go`; the generated OpenAPI is `docs/swagger/openapi.json`; the event schemas are `api/asyncapi.yaml`.

## Inbound HTTP (LLD §5.3)

45 endpoint IDs: 44 in the route table (39 paths and 44 operations in the OpenAPI document) plus B-W1, which the webhook adapter registers.

| Group | IDs | Path prefix | Caller and guard |
|---|---|---|---|
| Tenant | B-P1 … B-P26 | `/api/v1/tenants/:id/…` | Gateway with CD-19 headers `x-user-id`, `x-tenant-id`, `x-tenant-roles`. `x-tenant-id` = path `:id` (`BIL-API-2`); one permission per route (`BIL-AUTH-1`, `BIL-AUTH-7`) |
| Internal | B-I1 | `POST /api/v1/internal/subscriptions/authorize-and-charge` | Signup BFF only: SPIFFE `cluster.local/ns/bff/sa/bff-signup` from `X-Forwarded-Client-Cert` (`BIL-AUTH-3`, `BFF_CALLER_PRINCIPAL`) |
| Webhook | B-W1 | `POST /api/v1/webhooks/:provider` | Provider signature only; no tenant header trusted (`BIL-AUTH-5`). Gateway rate limit 50 rps per client IP (one bucket per IPv4 / IPv6 client), optional source CIDRs |
| Operator | B-O1 … B-O17 | `/api/v1/operator/…` | `platform_operator` (`BIL-AUTH-2`); `403` until `OPERATOR_PATH_LIVE=true` (`BIL-AUTH-6`); B-O17 also `pci_owner_reviewer`, not the submitter (`BIL-PROV-4`) |
| Docs, probes | — | `/openapi.json`, `/asyncapi`, `/asyncapi.yaml`, `/healthz`, `/readyz` | `/metrics` is on `METRICS_PORT` only. `/healthz` fails only when the platform-audit runner is not alive |

`commercial-system` is rejected inbound on every route (`BIL-AUTH-4`).

**Conventions (§5.1):**
- Every error is an RFC 9457 problem from platform-gincommon with an `UPPER_SNAKE` `errorCode` (§17 catalogue, frozen once published).
- `Retry-After` comes on `409 REQUEST_IN_PROGRESS` and `503`.
- `Idempotency-Key` is required where §5 says. Replays carry `Idempotent-Replayed: true`.
- Unknown JSON fields are rejected on tenant and operator routes and tolerated on B-I1.
- `record_version` is required on client mutations of a versioned row.

### B-I1 (direct paid signup) in brief

Full contract: LLD §5.4.2; BFF alignment status: `docs/lld/billing-open-items.md` (BL-OQ-8).

- **Request.** Key `charge:<tenant_id>`; body `tenant_id` must equal `x-tenant-id`. Required fields: `tenant_id`, `plan_code`, `licensed_seats` (1–1000), `payment_method_ref`.
- **Success.** `201` (charged), or `200` replay.
- **Payment outcomes.** `402 PAYMENT_ACTION_REQUIRED` (3-D Secure `client_secret`; retry with the same key and body), `402 PAYMENT_DECLINED`, `410 CHARGE_EXPIRED`.
- **Conflicts.** `409 SUBSCRIPTION_ALREADY_EXISTS` (the BFF never provisions with it), `409 REQUEST_IN_PROGRESS`, `409 PROVIDER_OPERATION_UNSUPPORTED`.
- **Request errors.** `422 CURRENCY_MISMATCH`, `422 PRICE_NOT_AVAILABLE`, `400 VALIDATION_ERROR` / `TENANT_MISMATCH`, `403 INSUFFICIENT_ROLE`.
- **Retry.** `503 PROVIDER_UNAVAILABLE` / `DB_UNAVAILABLE`.
- **Flag.** Behind `FEATURE_DIRECT_SIGNUP` (default `false`).

## Outbound HTTP (LLD §18), all outside transactions

| Callee | Route | Use | Timeout |
|---|---|---|---|
| Core (Org & Membership) | **I-11** `GET /api/v1/internal/tenants/:id/seat-usage` | Seat floor (`BIL-SEAT-3`), renewal, projection check, display (30 s cache for display only) | `CORE_TIMEOUT_MS` |
| Core | **I-18** `GET /api/v1/internal/tenants/:id/plan-change-check?target_plan=` | Every downgrade, at request and at effect; `503` fails closed | `CORE_TIMEOUT_MS` |
| Realm Provisioner | **RP-3** `POST …/tenants/:id/convert` (key `convert:<tenant_id>:<billing_subscription_id>`) | Conversion saga, only after payment is confirmed (`BIL-SAGA-1`) | `RP_TIMEOUT_MS` |
| Realm Provisioner | **RP-4** `POST …/tenants/:id/revert-conversion` (key `revert:…`) | Reversal (B-O8), within 7 days. B-O8 records the request on the saga (`reverting`) first; `409 operation_in_progress` / `optimistic_lock_conflict`, 5xx, timeout → `503 IAM_UNAVAILABLE` and the saga worker completes it | `RP_TIMEOUT_MS` |
| Realm Provisioner | **RP-11** `GET …/tenants/:id/realm` | Operator saga view (B-O7) only | 2 s (its own budget, LLD §18.10) |
| Entitlements | `GET /api/v1/internal/tenants/:id/entitlements` | Add-on eligibility (`BIL-ADD-1`): fresh on purchase (B-P20, fail closed `503 ENTITLEMENTS_UNAVAILABLE`), 60 s cache for B-P19. Egress restricted to this route (W-10); identity contract LLD §10.2 | `ENTITLEMENTS_TIMEOUT_MS` (1000) |
| Audit Log | AL-5 via platform-audit, **outbox mode** | Audit entries written in the same transaction (§10.6) | — |
| Stripe | stripe-go v87.2.0 (API `2026-11-18.endive`) in `provider/stripe` only | Every call with an idempotency key from Billing ids (`BIL-IDEM-2`) | `STRIPE_TIMEOUT_MS` |

Billing never calls RP-2 (the BFF's), RP-10 or RP-14.

## Events produced: `billing.events` (LLD §7.3)

Through the platform-events ordered outbox, keyed by `tenant_id`, Glue-framed with `ValidatingCodec`. The envelope `source` is `commercial-billing`, and `time` is Billing's clock at commit (`BIL-EVT-1`).

| Event | Consumers (queue) |
|---|---|
| `TenantPlanChanged` | Core (`billing-orgm-q`), Entitlements, Audit |
| `TenantSeatsChanged` (absolute `licensed_seats`; held 120 s after conversion, `BIL-EVT-6`) | Core, Audit |
| `TenantPaymentPastDue` | Core, Audit |
| `TenantSubscriptionCancelled` | Core, Entitlements, Audit |
| `TenantReactivated {source: billing}` | Core, RP (`billing-realm-q`), Entitlements, Audit |
| `TenantQuotaAddOnChanged {purchase \| refund}` (`billing_ref` idempotency; refund `purchase_ref` is a UUID) | Entitlements (`billing-entitlements-q`), Audit |

No event for an `incomplete` subscription (`BIL-EVT-8`); status events only on an actual transition (`BIL-EVT-7`). Contract tests: `test/contract` validates every produced sample against Billing's schema and each consumer's vendored schema.

## Events consumed (LLD §7.1)

| Queue | Events | Notes |
|---|---|---|
| `tenant-billing-q` (topic `iam-tenant-events`) | `TenantConverted`, `TenantSuspended`, `TenantReactivated`, `TenantOffboarded` | Last-writer-wins on envelope `time`, 300 s future clamp → DLQ (`BIL-EVT-3`). `TenantConverted` with no saga increments `commercial_billing_unmatched_conversions_total`. `export_s3_key` is never read or logged |
| `usage-billing-q` (topic `usage-events`) | `UsagePeriodClosed` | Off by default: the consumer starts only with `USAGE_BILLING_CONSUMER_ENABLED=true` (then `SQS_USAGE_BILLING_QUEUE_URL` is required). Reference-only records, prices nothing; contract unverified (BL-OQ-24) |

Dedup through platform-events' `inbox.Store.Process` (`processed_events`); DLQ at receive 4 (`SQS_MAX_RECEIVE_COUNT`). Outside dev, startup refuses an enabled queue without a usable `RedrivePolicy`.

## Provider callbacks (LLD §7.4, §8.15)

These are not bus events. B-W1 verifies the signature, then inserts into `payment_event_ledger` (allow-listed payload) and answers `200`. The webhook processor claims rows per tenant in `provider_created_at` order, re-reads each with `FetchCurrent` and applies it. The Stripe mapping table is in §18.8. Payments that cannot be applied safely are parked, not retried, and logged SEV-2 `BillingPaymentParked` (`BIL-PAY-6`, `BIL-PAY-7`; details in `flows-and-concurrency.md`).
