# Hand-off to Web / growth — BL-OQ-8: Signup BFF alignment (SGN-2)

| Field | Value |
|---|---|
| To | Web / growth (Signup BFF, `bff-signup` in namespace `bff`) |
| From | Commercial Team (Billing) |
| Date | 2026-11-26 |
| Billing LLD | `commercial-lld-billing-service.md` rev 0.51: §5.4.2 (B-I1 contract), §18.7 (go-live order), §10.2 (network) |
| Billing item | BL-OQ-8, go-live gate for **direct paid signup**. Not a design blocker |
| Deployment state | No service is in production; all run in dev only. "Done" means merged **and** promoted dev → staging → production |

## 1. What we need from you

Direct paid signup calls Billing's internal route **B-I1** `POST /api/v1/internal/subscriptions/authorize-and-charge`. Billing has published the final contract (it was "indicative" in HLD §10.6). It is a superset of your current port, so your adapter needs four changes:

1. Set **`BILLING_TIMEOUT_MS` ≥ 15000**. B-I1 makes two to four provider calls: p95 6 s, p99 12 s.
2. **Map the fields** from `ChargeRequest {TenantID, PaymentToken, Plan, Seats, IdempotencyKey}`:

   | BFF field | B-I1 |
   |---|---|
   | `TenantID` | body `tenant_id` **and** header `x-tenant-id` (must be equal) |
   | `PaymentToken` | `payment_method_ref` |
   | `Plan` | `plan_code` |
   | `Seats` | `licensed_seats` |
   | `IdempotencyKey` | header `Idempotency-Key`, which must be exactly `charge:<tenant_id>` (SB-INV-3) |

3. **Handle 3-D Secure:** on `402 PAYMENT_ACTION_REQUIRED`, the browser authenticates with the provider's JS using `action.client_secret`, then the BFF **retries with the same `Idempotency-Key` and the same body**.
4. **Follow the go-live order** in §5 below.

**Until this is done:** Billing keeps `FEATURE_DIRECT_SIGNUP=false`, and signup works through trial conversion only.

## 2. The B-I1 contract

### Caller identity

| Item | Rule |
|---|---|
| Transport | Istio mTLS |
| Who may call | Only `cluster.local/ns/bff/sa/bff-signup` (`BIL-AUTH-3`). Billing reads the SPIFFE URI from `X-Forwarded-Client-Cert`, which the sidecar sets. Any other caller, or no certificate, gets `403 INSUFFICIENT_ROLE` |
| Network | Billing admits pods in namespace `bff` with label `app.kubernetes.io/name: bff-signup`, on port 8080, `POST` on this exact path only (NetworkPolicy + Istio AuthorizationPolicy, LLD §10.2). Your egress to namespace `billing` must allow the same |
| Do not send | Card data of any kind. Only the provider token from the browser (`pm_…`) |

### Request

```http
POST /api/v1/internal/subscriptions/authorize-and-charge
Idempotency-Key: charge:3fa85f64-5717-4562-b3fc-2c963f66afa6
x-tenant-id: 3fa85f64-5717-4562-b3fc-2c963f66afa6
Content-Type: application/json
```

```jsonc
{
  "tenant_id": "3fa85f64-5717-4562-b3fc-2c963f66afa6",   // required, UUID; equals x-tenant-id
  "plan_code": "pro",                                    // required: starter | pro | enterprise
  "licensed_seats": 10,                                  // required: 1–1000
  "payment_method_ref": "pm_1QAbCdEfGhIjKlMn",           // required: provider token from the browser
  "interval": "month",                                   // optional: month (default) | year
  "currency": "USD",                                     // optional: defaults to USD; needs a price row
  "billing_contact": { "name": "Dana Owner", "email": "owner@acme.example" },   // optional
  "company_name": "Acme Bidding Co",                     // optional, ≤ 200 chars
  "country": "AE"                                        // optional, ISO 3166-1 alpha-2
}
```

Unknown fields are tolerated, so you can add fields ahead of Billing.

### Responses and what the BFF does

Errors are RFC 9457 problems (`application/problem+json`) with an `errorCode`.

| Status | Body (key fields) | BFF action |
|---|---|---|
| `201` | `billing_subscription_id`, `status: active`, `plan_code`, `licensed_seats`, `interval`, `current_period_end`, `provider` | Charged; payment confirmed server-side by the provider. Continue to RP-2 with `billing_subscription_id` |
| `200` + `Idempotent-Replayed: true` | Same body as the first `201` | Replay of a completed call. Treat as `201` |
| `402 PAYMENT_ACTION_REQUIRED` | `action: {type: "3ds", client_secret}`, `billing_subscription_id`, `expires_at` | Browser completes 3-D Secure with `client_secret`; then retry with the **same key and body**. Billing answers `201`, `402 PAYMENT_DECLINED`, or `402 PAYMENT_ACTION_REQUIRED` again if still pending |
| `402 PAYMENT_DECLINED` | `decline_reason`: `card_declined`, `insufficient_funds`, `expired_card`, `incorrect_cvc`, `processing_error`, `authentication_failed`, `other` | Nothing payable was created. Map to `422 PAYMENT_DECLINED` for the browser |
| `410 CHARGE_EXPIRED` | — | 3-D Secure not completed within 30 min (`SCA_ABANDON_AFTER`). Start over with a fresh signup |
| `409 SUBSCRIPTION_ALREADY_EXISTS` | `billing_subscription_id` | Same tenant with a different key or body. Use the returned id only if your own record matches (SGN-2 behaviour) |
| `409 REQUEST_IN_PROGRESS` | `Retry-After` | The first call with this key is still running. Retry with the same key after `Retry-After` |
| `400 VALIDATION_ERROR` / `400 TENANT_MISMATCH` | `validationErrors[]` | Bug in the request (wrong key format, body ≠ header tenant). Fix, don't retry |
| `422 PRICE_NOT_AVAILABLE` | — | No price for that plan, interval and currency |
| `403 INSUFFICIENT_ROLE` | — | Caller identity rejected (wrong service account, or mesh not forwarding the certificate) |
| `503 PROVIDER_UNAVAILABLE` / `503 DB_UNAVAILABLE` | `Retry-After` | Safe to retry with the same key |

### Retries and timeouts

- The key is per tenant (`charge:<tenant_id>`), so **every retry, including after a BFF timeout, reuses the same key and body**. Billing returns the stored outcome or continues the call in flight. Nothing is charged twice.
- A different body under the same tenant gets `409 SUBSCRIPTION_ALREADY_EXISTS`, not a second subscription.
- Billing sends no `billing.events` for this call. The tenant becomes paid in IAM through `TenantConverted(direct_signup=true)` after RP-2; seats follow when Billing's saga confirms.

## 3. Checklist for Web / growth

- [ ] `BILLING_TIMEOUT_MS` ≥ 15000 in every environment
- [ ] Field mapping as in §1, including `x-tenant-id` = body `tenant_id`
- [ ] `Idempotency-Key` is exactly `charge:<tenant_id>`, reused on every retry
- [ ] 3-D Secure: `402 PAYMENT_ACTION_REQUIRED` → browser → retry with the same key and body; `410 CHARGE_EXPIRED` → fresh signup
- [ ] Handling for `402 PAYMENT_DECLINED` (→ `422` to the browser), `409 SUBSCRIPTION_ALREADY_EXISTS`, `409 REQUEST_IN_PROGRESS`, `503` with `Retry-After`
- [ ] Calls go out as service account `bff-signup` in namespace `bff` over mesh mTLS; egress to `billing` allowed
- [ ] Pact consumer contract published for the cases in §4
- [ ] Helm values ready for step 4 of §5 (`PAID_SIGNUP_ENABLED`, `BILLING_BASE_URL`, `BILLING_TIMEOUT_MS=15000`)

## 4. Done when

1. **Pact contract green.** The consumer-driven contract from the BFF adapter (LLD §14.3) covers: `201`, `200` replay, `402 PAYMENT_DECLINED`, `402 PAYMENT_ACTION_REQUIRED`, `409 SUBSCRIPTION_ALREADY_EXISTS`, `503`. Billing verifies it in its CI.
2. **Test-Clock end-to-end run passes in staging**: a direct paid signup, including one 3-D Secure card, through BFF → Billing → RP-2 → paid tenant.

## 5. Go-live order (LLD §18.7, per environment)

| Step | What | Owner | State (2026-11-26) |
|---|---|---|---|
| 1 | SRE-3: Core's `pending_tenant_events` table (was migration `000004`, now in Core's base migration `000000_initial_schema`) and the `parked-events-replay` CronJob deployed | IAM Core + Platform-SRE | Code ready; deploy per environment |
| 2 | B-I1 contract published; BFF aligned (this hand-off) | Billing; **Web / growth** | Contract published; **BFF alignment open** |
| 3 | Billing deployed with `FEATURE_DIRECT_SIGNUP=true` in staging; Pact green; Test-Clock E2E green | Billing (+ Web / growth for Pact) | After steps 1, 2 |
| 4 | BFF Helm: `PAID_SIGNUP_ENABLED=true`, `BILLING_BASE_URL`, `BILLING_TIMEOUT_MS=15000` | **Web / growth** | After step 3 |
| 5 | BL-OQ-2 (Core's recency guard) closed | IAM Core | **Done** (2026-11-19) |

Direct paid signup also needs a live payment provider: Stripe takes new accounts only after the operator path is live (OP-G2, OP-G3) and the first provider certification is approved (B-O17). That is not on Web / growth, but it bounds the production date.

## 6. Contacts and references

- Billing LLD §5.4.2 (full contract), §5.3 (route catalogue), §10.2 (network), §14.3 (Pact), §18.7 (go-live order).
- Billing open items: `docs/lld/billing-open-items.md` §4.7.
- Questions on the contract: Commercial Team (Billing). Reply to this hand-off with your target date for step 2.
