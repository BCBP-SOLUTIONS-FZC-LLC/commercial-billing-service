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
cancelled → active   settled via a new provider subscription → TenantReactivated
active|past_due|cancelled → ended  TenantOffboarded / RP-4 reversal / signup refund
```

At most one non-`ended` subscription per tenant (`uq_sub_one_live`, `BIL-SUB-3`). A callback that would move the status any other way is recorded `ignored`.

## Conversion saga (§9.3, `domain/saga.go`)

```
payment_confirmed → rp_convert_pending (same commit)              trial checkout (B-P2)
[*] → rp_convert_pending                                          operator stub (B-O1)
[*] → awaiting_tenant_converted                                   B-I1 paid (direct signup)
rp_convert_pending → awaiting_tenant_converted   RP-3 200 / 409 realm_already_dedicated
rp_convert_pending → rp_convert_pending          409 in progress, 502, 503, timeout (backoff)
rp_convert_pending → failed                      409 tenant_offboarded, 400, 403, 404
* → confirmed                                    TenantConverted (or B-O9 resolved_manually)
failed → rp_convert_pending                      B-O9 retry
rp_convert_pending|awaiting|failed → refund_pending → refunded   B-O9 refund
confirmed → reverted                             B-O8 RP-4
```

- **`BIL-SAGA-1`:** RP-3 only after server-side payment confirmation.
- **`BIL-SAGA-2`:** one saga per `billing_subscription_id`.
- **`BIL-OPS-5`:** `saga-sweeper` pages `BillingConversionSagaOverdue` (SEV-2) once when payment is older than 30 min and the saga isn't confirmed. Runbook: `docs/runbooks/stuck-conversion-saga.md` (LLD §20.1).

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
| Provider call | `bsub:<id>:customer\|pm\|create\|confirm\|checkout`, `seatpur:<purchase_id>`, `planchg:<change_id>`, `addon:<purchase_id>`, `refund:<refund_id>`, `adj:<adjustment_id>`, `sched:<schedule_id>`, `cancel:<sub_id>:<n>` | Stripe keys live 24 h; Billing never reuses a key after 23 h |
| Provider callback | `(provider, provider_event_id)` ledger PK | Duplicate → `200`, no work |
| Consumed event | envelope `id` × consumer in `processed_events` | Claimed as the first statement of the handler's transaction |
| RP-3 / RP-4 | `convert:` / `revert:<tenant_id>:<billing_subscription_id>` | RP replays its stored result |

## Webhook pipeline (§8.15)

Pipeline:
1. B-W1 checks the body limit, then verifies the signature, tolerance and `livemode`. A failure → `400`, nothing stored (`BIL-PAY-2`).
2. Insert into the ledger (`BIL-PAY-1`) and answer `200`.
3. The processor claims rows with `SKIP LOCKED`. A tenant with an earlier pending row is skipped (`BIL-PAY-5`).
4. It resolves the tenant from the customer mapping (`BIL-PAY-3`) and re-reads with `FetchCurrent` (`BIL-PAY-4`).
5. It applies the change under the tenant lock, then retries with backoff up to `WEBHOOK_MAX_ATTEMPTS`.

Every `failed` row is retried, ignored by an operator with a reason, or listed by reconciliation (`BIL-OPS-6`).

## Events and timing (§7.5)

- State change + outbox row + audit entry in one transaction (`BIL-EVT-2`), ordered per tenant (`BIL-EVT-5`).
- Payloads are snapshots (absolute seats and plan), so redelivery converges (`BIL-EVT-4`).
- `scheduled_emissions` holds the seat event 120 s after conversion so Core applies `TenantConverted` first (`BIL-EVT-6`); Core also parks events for unknown tenants (EVT-18).

## Failure scenarios

LLD §9.5 (table of failure → behaviour) and §20 runbooks. Fail closed: Entitlements on purchase (`503 ENTITLEMENTS_UNAVAILABLE`), I-18 on downgrade. Fail soft: I-11 on display (fields `null`).
