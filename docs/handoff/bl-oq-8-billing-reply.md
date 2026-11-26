# Reply: Billing to Signup BFF — BL-OQ-8 (B-I1 alignment)

| Field | Value |
|---|---|
| To | Web / growth (Signup BFF, `bff-signup`, namespace `bff`) |
| From | Commercial Team (Billing) |
| Date | 2026-11-27 |
| About | Your reply of 2026-11-27 to `docs/handoff/bl-oq-8-signup-bff.md` (`bff-signup` PR #6, BFF LLD v1.3 §19; merged 2026-11-26, `c2e9a1d`) |
| Billing LLD | `commercial-lld-billing-service.md` **rev 0.52**: §5.4.2 updated from your reply |

Thank you. We checked your checklist against our hand-off and it matches. The remaining work is the Pact contract, the Test-Clock E2E run and sidecar injection on `bff`.

## 1. Your choices (§2 of your reply)

| # | Your choice | Billing |
|---|---|---|
| 2.1 | Never provision on `409 SUBSCRIPTION_ALREADY_EXISTS`; answer `409 INVALID_STATE`, keep the attempt, no RP-2 | **Accepted.** It is stricter than SGN-2 and safe: a 409 for this tenant means a subscription this attempt did not create. LLD §5.4.2 and the §8.3 failure table now say the BFF never provisions with the returned id |
| 2.2 | Only the four required fields; keep the key on every non-definite outcome; drop it only on `PAYMENT_DECLINED`, `CHARGE_EXPIRED`, `PRICE_NOT_AVAILABLE` | **Accepted.** Repeat calls with the same key and body get the stored outcome (`200` + `Idempotent-Replayed: true`) or continue the call in flight. Recorded in §5.4.2 |
| 2.3 | B-I1 timeout 15 s; SB-5 deadline 30 s; public route 35 s; `Retry-After` in delta-seconds | **Fine.** Billing sends `Retry-After` only as delta-seconds |
| 2.4 | W3C `traceparent` sent | **Fine.** Billing joins the trace; no change needed |
| 2.5 | `licensed_seats` and `plan_code` checked before the call | **Fine.** |

## 2. Answers to your questions

**Q1. Paid but never provisioned.** Billing already has a safety net; it is an alert followed by an operator refund, not an automatic void:

- **Alert.** Billing's `saga-sweeper` raises `BillingConversionSagaOverdue` (SEV-2) once for any direct-signup saga whose payment was confirmed more than 30 minutes ago (`SAGA_ALERT_AFTER`) without `TenantConverted(direct_signup=true)` (LLD §8.2, `BIL-OPS-5`). This matches your 30-minute session window: when the alert fires, the BFF can no longer finish that signup.
- **Action.** Runbook LLD §20.1: Billing on-call checks with you; if the signup cannot complete, an operator refunds through B-O9 `refund` (category `signup_failed`; subscription `ended`). If RP-2 succeeded but I-1 failed, Core's `IAMParkedTenantEventsStale` also pages Billing on-call (LLD §8.3).
- **Not automatic in Phase 1.** A refund also reverses an identity step that may have half-succeeded, so it stays an operator decision. The B-O9 operator route needs IAM's operator path (OP-G2, OP-G3), which production direct signup already waits for.

If you need an automatic void before step 4, tell us. It would be a design change to the saga (LLD §8.2) and needs Commercial Team approval.

**Q2. Pact.**

- Billing has no Pact tooling in CI yet. The provider verification is Billing's work, tracked under BL-OQ-8.
- We will send these before you need them:
  1. Pact broker URL and credentials path: **to confirm** (Billing + Platform);
  2. provider name: `commercial-billing` (the service name, as in LLD §1);
  3. verification: Billing's CI verifies the contract tagged `main` from `bff-signup`, plus your PR branch when its pipeline triggers our verification job.
- You can write the consumer contract now against the cases in our hand-off §4.

**Q3. Codes missing from §5.4.2.** Added in rev 0.52, with `409 REQUEST_IN_PROGRESS` and `403 INSUFFICIENT_ROLE`, which were also missing from the table:

| Code | When | BFF handling |
|---|---|---|
| `409 PROVIDER_OPERATION_UNSUPPORTED` | The configured new-account provider can't charge off-session (for example `manual`). A Billing configuration fault | Your current handling is right: unexpected, `500`, attempt kept. Don't show it to the user |
| `422 CURRENCY_MISMATCH` | An account already exists for the tenant in another currency. Unreachable while you send no `currency` and `DEFAULT_CURRENCY` doesn't change | Same: unexpected, `500`, attempt kept |

**Q4. Test-Clock E2E in staging.** Billing drives it, since it is step 3 of the go-live order. We need:

- PR #6 promoted to staging;
- `FEATURE_DIRECT_SIGNUP=true` on Billing and `PAID_SIGNUP_ENABLED=true` on the BFF for the run;
- sidecar injection on `bff`;
- SRE-3 deployed.

Date: **to confirm** once those are in place. We'll propose a slot.

**Q5. LLD revision.** Rev 0.52 (this reply's changes on top of 0.51) will be committed on Billing's branch `feat/billing-phase-6-flows`; we will tell you when it is on `main`.

## 3. Go-live order (LLD §18.7), updated

| Step | Owner | State (2026-11-27) |
|---|---|---|
| 1. SRE-3 deployed | IAM Core + Platform-SRE | Code ready; deploy per environment |
| 2. B-I1 published; BFF aligned | Billing; Web / growth | Contract published (rev 0.52). **BFF code merged** (PR #6, `c2e9a1d`); promote dev → staging, date to confirm |
| 3. Billing `FEATURE_DIRECT_SIGNUP=true` in staging; Pact green; Test-Clock E2E green | Billing (+ Web / growth for Pact) | Waiting on Q2 (broker details, Billing's verification job) and Q4 (slot) |
| 4. BFF Helm `PAID_SIGNUP_ENABLED=true` | Web / growth | Values ready; after step 3 |
| 5. BL-OQ-2 closed | IAM Core | Done (2026-11-19) |

Also before production, outside this hand-off:

- Platform-SRE: Istio sidecar injection on `bff`, without which B-I1 answers `403`;
- your SRE items BFF-1, BFF-2 and BFF-4;
- Billing's operator path (OP-G2, OP-G3) and the first provider certification (B-O17).
