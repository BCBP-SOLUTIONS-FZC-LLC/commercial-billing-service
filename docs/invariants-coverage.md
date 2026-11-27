# Invariants coverage

Every `BIL-*` invariant in `docs/lld/commercial-lld-billing-service.md`, where it is enforced and which test proves it (implementation prompt rule 2). Update this file in the same PR that implements or tests an invariant.

Status: **pending** = not implemented yet · **partial** = enforced, test missing · **done** = enforced and tested.

| Invariant | LLD § | Rule (abridged) | Enforced in | Test | Status |
|---|---|---|---|---|---|
| `BIL-ADD-1` | §4.6 | A token add-on is sold only when: the subscription is `active` and not collection-paused; `iam_lifecycle = 'active'`; the tenant is not on trial … | — | — | pending |
| `BIL-ADD-2` | §4.6 | `TenantQuotaAddOnChanged` is emitted exactly once per confirmed purchase, with `billing_ref = purchase_id` (`quota_addon_purchases.event_id`). | — | — | pending |
| `BIL-ADD-3` | §4.6 | Add-on purchases have no customer refund path; a refund of an add-on invoice exists only as an operator action with category `billing_error`, … | — | — | pending |
| `BIL-ADD-4` | §4.6 | When a refund of an add-on invoice reaches `succeeded`, `TenantQuotaAddOnChanged {reason: "refund", delta: −addon_tokens, billing_ref: refund_id, … | — | — | pending |
| `BIL-API-1` | §5.1 | Billing never takes tenant id, user id or role from a request body for authorization, RLS scoping or ownership. The one body `tenant_id` (B-I1) … | — | — | pending |
| `BIL-API-2` | §5.1 | On tenant routes, `x-tenant-id` must equal the path `:id`; otherwise `403 INSUFFICIENT_ROLE` before any query. | — | — | pending |
| `BIL-API-3` | §5.1 | A route that can create a charge never charges twice for one `Idempotency-Key` (§9.2). | — | — | pending |
| `BIL-API-4` | §5.1 | No response body contains card data, `client_secret` (except the 3-D Secure action object returned to the caller that started the payment), … | — | — | pending |
| `BIL-API-5` | §5.1 | Provider-hosted URLs returned to the browser (`checkout_url`, `setup_url`, `payment_url`, invoice links) come from the provider adapter only and … | — | — | pending |
| `BIL-AUTH-1` | §5.2 | Every tenant route checks its permission before any provider call or DB write; missing → `403 INSUFFICIENT_PERMISSION` with extension … | — | — | pending |
| `BIL-AUTH-2` | §5.2 | `platform_operator` is accepted only on `/api/v1/operator/*`, re-checked in the handler before any DB access (IAM AUTH-6 pattern). A … | — | — | pending |
| `BIL-AUTH-3` | §5.2 | B-I1 admits only the mTLS principal `cluster.local/ns/bff/sa/bff-signup`; any other caller is `403 INSUFFICIENT_ROLE`. | — | — | pending |
| `BIL-AUTH-4` | §5.2 | `commercial-system` (`00000000-0000-0000-0000-0000000000c1`) is Billing's **outbound** identity only; inbound it is rejected on every route. | — | — | pending |
| `BIL-AUTH-5` | §5.2 | The webhook route binds no tenant and trusts no header except the provider signature. | — | — | pending |
| `BIL-AUTH-6` | §5.2 | Until IAM's operator path is live end to end (CD-52; AuthZ Enrichment LLD §8.9.1 OP-G1 platform realm, OP-G2 gateway `jwt_authn` provider, OP-G3 … | — | — | pending |
| `BIL-AUTH-7` | §5.2 | Each tenant route requires exactly one permission (§5.3). `commercial.payment_method.manage` and `commercial.subscription.change_plan` are never … | — | — | pending |
| `BIL-CONC-1` | §9.1 | Every write that changes a subscription, its items, schedules, purchases or saga — from a tenant request, operator request, webhook, consumer, … | — | — | pending |
| `BIL-CONC-2` | §9.1 | No provider, IAM or Entitlements HTTP call is made while a transaction is open. Pattern: commit intent (`pending_payment`, saga step, `applying`) … | — | — | pending |
| `BIL-CONC-3` | §9.1 | Mutations a client issues against a version it read carry `record_version` (§5.1); background writers do not use `record_version` but hold the … | — | — | pending |
| `BIL-CONC-4` | §9.1 | Writes run in `RunInTxWithRetryOpts` (pgcommon retries 40P01 / 40001 up to 3 times); after retries → `409 CONCURRENT_UPDATE`. | — | — | pending |
| `BIL-EVT-1` | §7.5 | `time` is Billing's clock inside the transaction that commits the change (`clock_timestamp()` read in that transaction), never the provider's … | — | — | pending |
| `BIL-EVT-2` | §7.5 | Every state change and its outbox row commit in one transaction; nothing is published outside the outbox. | — | — | pending |
| `BIL-EVT-3` | §7.5 | IAM lifecycle events are applied last-writer-wins on envelope `time` with a 300 s future-time clamp; `offboarded` is terminal. | — | — | pending |
| `BIL-EVT-4` | §7.5 | Payloads are snapshots (`licensed_seats` absolute, `plan` absolute), so redelivery and reordering converge. `TenantQuotaAddOnChanged.delta` is the … | — | — | pending |
| `BIL-EVT-5` | §7.5 | Billing's own events for one tenant are published in commit order (ordered outbox, key `tenant_id`). When one change produces a status-bearing … | — | — | pending |
| `BIL-EVT-6` | §7.5 | After `TenantConverted`, the seat event is held for `SEATS_AFTER_CONVERSION_DELAY` (120 s) in `scheduled_emissions` before it is published, so … | — | — | pending |
| `BIL-EVT-7` | §7.5 | `TenantPaymentPastDue`, `TenantSubscriptionCancelled` and `TenantReactivated` are emitted only on an actual transition of Billing's status, never … | — | — | pending |
| `BIL-EVT-8` | §7.5 | No event for a subscription that is `incomplete`. | — | — | pending |
| `BIL-IDEM-1` | §9.2 | A route that can charge stores its outcome before returning; a retry never charges twice (`BIL-API-3`). | — | — | pending |
| `BIL-IDEM-2` | §9.2 | Provider idempotency keys are derived from Billing ids only, never from request input. | — | — | pending |
| `BIL-IDEM-3` | §9.2 | Idempotency records live 72 h (`IDEMPOTENCY_TTL_HOURS`); a key reused after that is a new request (the BFF's `charge:<tenant_id>` cannot recur … | — | — | pending |
| `BIL-OPS-1` | §3.2 | Every DB pool is created by `pgcommon.NewPool` | `.golangci.yml` forbidigo | `make lint` | done |
| `BIL-OPS-2` | §3.2 | Every `outbox_events` mutation goes through `platform-events` | `.github/scripts/check-outbox-access.sh` | `make guards` | done |
| `BIL-OPS-3` | §3.2 | No HTTP call inside an open transaction (`BIL-CONC-2`) | — | — | pending |
| `BIL-OPS-4` | §3.2 | Namespace `billing`, ServiceAccount and `app.kubernetes.io/name` exactly `commercial-billing` (CD-19) | — | — | pending |
| `BIL-OPS-5` | §9.7 | A saga not `confirmed` 30 minutes after payment confirmation always pages, once. | — | — | pending |
| `BIL-OPS-6` | §9.7 | Every `failed` ledger row is retried to success, set `ignored` by an operator replay with a reason, or listed by the next reconciliation run. | — | — | pending |
| `BIL-OPS-7` | §9.7 | Live and test provider keys never coexist in one environment (`livemode` check, startup check of the key prefix `rk_live_` vs `rk_test_` against … | `internal/config` `checkStripeKey` | `TestBIL_OPS_7_LiveAndTestKeysNeverMix` | partial |
| `BIL-OPS-8` | §9.7 | A purchase confirmed by payment always produces its event (outbox in the same commit), so "paid but nothing happened" can only be downstream lag, … | — | — | pending |
| `BIL-OPS-9` | §9.7 | No job deletes or minimises a financial record (CD-14, §15.3). The only scheduled data changes are the operational prunes (outbox, processed … | — | — | pending |
| `BIL-PAY-1` | §4.6 | Every provider callback is in `payment_event_ledger` before any domain write. | — | — | pending |
| `BIL-PAY-2` | §7.4 | A callback failing signature, tolerance or `livemode` is `400` and never stored. | — | — | pending |
| `BIL-PAY-3` | §7.4 | The tenant is resolved from Billing's `(provider, provider_customer_id)` mapping. Metadata (`tenant_id`, `billing_subscription_id`) only … | — | — | pending |
| `BIL-PAY-4` | §7.4 | **Re-read, don't trust.** For every kind that changes state the processor calls `FetchCurrent` and applies the provider's current object, not the … | — | — | pending |
| `BIL-PAY-5` | §7.4 | Callbacks for one tenant are processed in `provider_created_at` order (claim query orders per tenant and skips a tenant that has an earlier row in … | — | — | pending |
| `BIL-PCI-1` | §3.2 | Only `adapters/provider/stripe` imports `github.com/stripe/*`; no provider type crosses a port; the domain and events are provider-neutral (CD-56) | `.go-arch-lint.yml` + `.github/scripts/check-stripe-imports.sh` | `make arch-lint guards` | done |
| `BIL-PCI-2` | §4 | No table holds a PAN, CVV, full expiry date, bank account number, provider API key or webhook secret. Payment-method display data (brand, last … | — | — | pending |
| `BIL-PCI-3` | §4 | `payment_event_ledger.payload` holds only allow-listed fields (§4.2.15); billing details, addresses, card fingerprints and `client_secret` values … | — | — | pending |
| `BIL-PROV-1` | §4.6 | A provider is enabled for new billing accounts only if a `passed` (or, for `manual`, `exempt`) `provider_certifications` row exists for its … | — | — | pending |
| `BIL-PROV-2` | §4.6 | Changing the adapter version or the pinned provider API version requires a new certification run; until it passes, the provider takes no new … | — | — | pending |
| `BIL-PROV-3` | §4.6 | A provider certified with `proration_mode = fallback` always uses Billing's calculator for previews and charges (§8.5.3); one certified `native` … | — | — | pending |
| `BIL-PROV-4` | §4.6 | A certification becomes `passed` only through B-O17, by a caller holding `pci_owner_reviewer` who did not submit the run (`chk_pc_two_people`, … | — | — | pending |
| `BIL-RLS-1` | §4.3 | Every tenant-scoped table has `ENABLE` + `FORCE ROW LEVEL SECURITY` and the `tenant_isolation` policy; a CI test lists `pg_class` and fails on any … | — | — | pending |
| `BIL-RLS-2` | §4.3 | `app.tenant_id` is bound transaction-locally on every app-pool checkout (`set_config(…, true)`), never session-wide. | — | — | pending |
| `BIL-RLS-3` | §4.3 | `billing_system` is used only by the paths listed above; handlers for tenant routes have no access to the sysPool (constructor injection). | — | — | pending |
| `BIL-SAGA-1` | §8.1.1 | RP-3 is called only after payment is confirmed server-side — a verified callback re-read from the provider, or a provider API response — never on … | — | — | pending |
| `BIL-SAGA-2` | §4.6 | One saga per `billing_subscription_id`; RP-3 is always called with `Idempotency-Key: convert:<tenant_id>:<billing_subscription_id>`. | — | — | pending |
| `BIL-SEAT-1` | §4.6 | One open seat purchase per subscription (`uq_sp_one_open`). | — | — | pending |
| `BIL-SEAT-2` | §4.6 | One open seat change schedule per subscription (`uq_scs_one_open`). | — | — | pending |
| `BIL-SEAT-3` | §4.6 | `renewal_seats = max(target_seats, seats_in_use)`; after the renewal top-up check the licensed count is never below I-11's `active_users + … | — | — | pending |
| `BIL-SEAT-4` | §4.6 | A seat change schedule never changes `licensed_seats` before the renewal and never blocks a purchase or a member addition (HLD §8.5.1 step 2). | — | — | pending |
| `BIL-SEAT-5` | §4.6 | A seat purchase priced by Billing's fallback calculator stores its inputs, the unrounded amount and the rounded `amount_minor` … | — | — | pending |
| `BIL-SUB-1` | §2.3 | Billing never emits an event that sets `suspended`, `trial_expired` or `offboarded`; those are IAM's. | — | — | pending |
| `BIL-SUB-2` | §2.3 | `offboarded` is terminal (Core PAID-1). Billing closes the account; a returning customer is a new tenant with a new account and a new … | — | — | pending |
| `BIL-SUB-3` | §4.6 | At most one non-`ended` subscription per tenant (`uq_sub_one_live`). | — | — | pending |
| `BIL-SUB-4` | §4.6 | Status moves only along the transition table in §8.14; a callback that would move it any other way is recorded `ignored` (`BIL-PAY-4`). | — | — | pending |
| `BIL-SUB-5` | §4.6 | The seat item `quantity` changes only on a **confirmed** payment (conversion, purchase, top-up) or at a renewal applying a reduction; never on a … | — | — | pending |
| `BIL-SUB-6` | §4.6 | An account's currency is fixed while it has a non-`ended` subscription. | — | — | pending |
| `BIL-SUB-7` | §4.6 | While collection is paused (CD-24, §8.10) no payment is attempted: new invoices are held as drafts (`held_for_review`), automatic retries on … | — | — | pending |
| `BIL-SUB-8` | §4.6 | A live subscription's price changes only at a renewal or with a plan change the customer makes (BL-OQ-16, §8.16). At renewal `R` the seat item … | — | — | pending |

`BIL-OPS-7` is partial: the live/test key check runs at config load; the provider-side `livemode` check arrives with the Stripe adapter (Phase 3).

Total: 70 invariants.
