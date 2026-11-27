# Flows and Concurrency

LLD §8 (flows), §8.14 (subscription), §9 (concurrency, idempotency, sagas, failures). The transition tables in `internal/domain` are the single source; diagrams here are summaries.

## Subscription state machine (§8.14, `domain/subscription.go`)

```
[*] → incomplete     B-P2 checkout / B-I1 (the operator stub starts active)
incomplete → active  first payment confirmed
incomplete → ended   checkout expired / declined / 3-D Secure abandoned
active → past_due    cycle invoice failed          → TenantPaymentPastDue
past_due → active    invoice paid                  → TenantReactivated
active|past_due → cancelled  ended / dunning exhausted → TenantSubscriptionCancelled
cancelled → active   first invoice (subscription_create) of a new provider subscription → TenantReactivated
active|past_due|cancelled → ended  TenantOffboarded / RP-4 reversal / signup refund
```

At most one non-`ended` subscription per tenant (`uq_sub_one_live`, `BIL-SUB-3`). A callback that would move the status any other way is recorded `ignored`.

## Conversion saga (§9.3, `domain/saga.go`)

```
payment_confirmed → rp_convert_pending (same commit)              trial checkout (B-P2)
[*] → rp_convert_pending                                          operator stub (B-O1)
[*] → awaiting_tenant_converted                                   B-I1 paid (direct signup)
rp_convert_pending → awaiting_tenant_converted   RP-3 200 / 409 realm_already_dedicated
rp_convert_pending → rp_convert_pending          409 operation_in_progress / optimistic_lock_conflict, any 5xx, timeout (backoff)
rp_convert_pending → failed                      409 tenant_offboarded, other 409 (realm_name_unavailable, credential_replay_expired), 400, 403, 404
* → confirmed                                    TenantConverted (or B-O9 resolved_manually)
failed → rp_convert_pending                      B-O9 retry
rp_convert_pending|awaiting|failed → refund_pending → refunded   B-O9 refund
confirmed → reverting                            B-O8 records the request first (rev 0.63)
reverting → reverting                            RP-4 409 operation_in_progress / optimistic_lock_conflict, 5xx, timeout (saga worker, backoff)
reverting → reverted                             RP-4 200: reversal completed (B-O8 or saga worker)
reverting → confirmed                            terminal RP-4 answer: request dropped (worker: SEV-2 BillingReversalDecisionNeeded)
```

- **`BIL-SAGA-1`:** RP-3 only after server-side payment confirmation.
- **`BIL-SAGA-2`:** one saga per `billing_subscription_id`.
- **`BIL-OPS-5`:** `saga-sweeper` pages `BillingConversionSagaOverdue` (SEV-2) once when payment is older than 30 min and the saga isn't confirmed. It alerts first, then re-runs due sagas with at most 80% of the job's remaining time, so a hung RP cannot delay the page. Runbook: `docs/runbooks/stuck-conversion-saga.md` (LLD §20.1).

## Direct paid signup (B-I1, §8.3)

1. Claim the idempotency record. A completed record with an equal hash → replay. A different hash → `409 SUBSCRIPTION_ALREADY_EXISTS`. In flight → `409 REQUEST_IN_PROGRESS`.
2. Read tenant state in a tenant transaction:
   - a live non-`incomplete` subscription → 409;
   - an `incomplete` direct-signup one → continue it (3-D Secure retry);
   - older than `SCA_ABANDON_AFTER` → `410 CHARGE_EXPIRED` and expire it.
3. Provider customer, then subscription creation, **outside** any transaction (keys `bsub:<id>:…`).
4. Paid → transaction 2: subscription `active`, saga `awaiting_tenant_converted`, idempotency record `completed` with the `201`. **No `billing.events`.** The tenant becomes paid in IAM via `TenantConverted(direct_signup=true)`; seats follow 120 s after saga confirmation.

## Per-tenant lock and optimistic concurrency (§9.1)

| Rule | Meaning |
|---|---|
| `BIL-CONC-1` | Every write to a subscription, item, schedule, purchase or saga first takes `pg_advisory_xact_lock(hashtextextended('billing:' \|\| tenant_id, 0))` (`Service.locked`). This applies to requests, webhooks, consumers, workers and jobs alike |
| `BIL-CONC-2` | No provider, IAM or Entitlements HTTP call while a transaction is open. Commit intent (`pending_payment`, saga step, `applying`), call outside, then commit the outcome under the lock, re-checking state. A crash in between leaves a row that a worker, callback or `purchase-expiry` picks up |
| `BIL-CONC-3` | Client mutations carry `record_version`. A zero-row update is diagnosed (not found vs version moved → `409 OPTIMISTIC_LOCK_CONFLICT`) |
| `BIL-CONC-4` | Writes run in pgcommon's retrying transaction (40P01 / 40001, 3 times), then `409 CONCURRENT_UPDATE` |

## Idempotency (§9.2)

| Operation | Key | Replay |
|---|---|---|
| Tenant / internal / operator mutation | `Idempotency-Key` → `idempotency_records (tenant_id, scope, key)`, 72 h | Same hash → stored response; different hash → `422 IDEMPOTENCY_KEY_REUSED` (B-I1: `409 SUBSCRIPTION_ALREADY_EXISTS`); in flight (`locked_until` 30 s) → `409 REQUEST_IN_PROGRESS`, `Retry-After: 2` |
| Provider call | `bsub:<id>:customer\|pm\|create\|confirm\|checkout\|expire`, `seatpur:<purchase_id>`, `planchg:<change_id>`, `addon:<purchase_id>` (voids: `addon:\|seatpur:\|planchg:<id>:void` on a decline, `…:expire` from purchase-expiry), `refund:<refund_id>`, `adj:<adjustment_id>`, `sched:<schedule_id>`, `cancel:<sub_id>:<n>` | Stripe keys live 24 h; Billing never reuses a key after 23 h. A retry under the same client key (after a 5xx) resumes the purchase / adjustment / refund row and its provider key; a different body → `422 IDEMPOTENCY_KEY_REUSED`; a concurrent insert → `409 REQUEST_IN_PROGRESS` |
| Provider callback | `(provider, provider_event_id)` ledger PK | Duplicate → `200`, no work |
| Consumed event | envelope `id` × consumer in `processed_events` | Claimed as the first statement of the handler's transaction |
| RP-3 / RP-4 | `convert:` / `revert:<tenant_id>:<billing_subscription_id>` | RP replays its stored result |
| B-O8 completion | `cancel:<billing_subscription_id>:reversal`, `refund:reversal:<billing_subscription_id>` | B-O8 and the saga worker never cancel or refund twice; a recorded refund resumes with its own body |

## Webhook pipeline (§8.15)

Pipeline:
1. B-W1 checks the body limit, then verifies the signature, tolerance and `livemode`. A failure → `400`, nothing stored (`BIL-PAY-2`).
2. Insert into the ledger (`BIL-PAY-1`) and answer `200`.
3. The processor claims rows with `SKIP LOCKED`. A tenant with an earlier pending row is skipped (`BIL-PAY-5`).
4. It resolves the tenant from the customer mapping (`BIL-PAY-3`) and re-reads with `FetchCurrent` (`BIL-PAY-4`).
5. It applies the change under the tenant lock, then retries with backoff up to `WEBHOOK_MAX_ATTEMPTS`.

Every `failed` row is retried, ignored by an operator with a reason, or listed by reconciliation (`BIL-OPS-6`).

Parked outcomes (rev 0.59, no retry, SEV-2 log `BillingPaymentParked`): `failed:checkout_superseded` (a session Billing expired was completed anyway, and its subscription is ended or linked to another provider subscription; one naming the already-linked provider subscription is `ignored:already_linked`, rev 0.60), `failed:paid_after_end` (first invoice paid for an ended subscription) and `failed:invoice_amount_mismatch` (rev 0.60, `BIL-PAY-7`: a purchase's invoice paid for another amount or currency; nothing granted). The operator cancels, refunds or re-bills in the provider. Claim still skips only tenants with a row in `processing`.

Unlinked `invoice.paid` (rev 0.60, LLD §8.1.1 step 4): the Billing subscription is resolved from `billing_subscription_id` in the provider subscription's metadata on the re-read invoice. An incomplete trial conversion is linked and confirmed (its checkout session is marked `completed` at the same time); an ended one is parked `paid_after_end`; no usable metadata → `ignored:unknown_subscription`; a Billing subscription linked to a different provider subscription → `ignored:superseded_provider_subscription` (warning, nothing mirrored), except a `cancelled` subscription's `subscription_create` invoice, which retries while a B-P14 reactivation awaits its link; any other live subscription without a link retries.

Settling a purchase's invoice (rev 0.59, 0.60, LLD §8.5.1; `internal/app/purchase_invoice.go`): `purchase-expiry` and declines re-read the invoice outside any transaction; `open` → void, `draft` → `DeleteDraftInvoice`; `paid` settles the purchase only when amount paid (or total) and currency equal the purchase's (`BIL-PAY-7`), else the purchase closes unpaid and `BillingPaymentParked` is logged. An unrecorded invoice is looked up with `FindInvoice`: add-ons by exact metadata `purchase_id` (1 min clock slack), proration invoices (seats, upgrades) by subscription, `subscription_update`, amount, currency and a positive line for `seats_after` or the target price, created at or after the purchase (no backward slack). A looked-up invoice already recorded on another purchase or plan change is never used, re-checked under the lock (`BIL-PAY-6`). Checkout sessions are expired at the provider first (B-P2 keys); `ErrCheckoutCompleted` leaves them open for the callbacks.

## Events and timing (§7.5)

- State change + outbox row + audit entry in one transaction (`BIL-EVT-2`), ordered per tenant (`BIL-EVT-5`).
- Payloads are snapshots (absolute seats and plan), so redelivery converges (`BIL-EVT-4`).
- `scheduled_emissions` holds the seat event 120 s after conversion so Core applies `TenantConverted` first (`BIL-EVT-6`); Core also parks events for unknown tenants (EVT-18).

## Failure scenarios

LLD §9.5 (table of failure → behaviour) and §20 runbooks. Fail closed: Entitlements on purchase (`503 ENTITLEMENTS_UNAVAILABLE`), I-18 on downgrade. Fail soft: I-11 on display (fields `null`).
