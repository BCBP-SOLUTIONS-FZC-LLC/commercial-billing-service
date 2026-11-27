# Dependency degradation and business signals

LLD §20.7, §20.8.

| Operation | Sync dependency | Posture | On failure |
|---|---|---|---|
| Conversion checkout (B-P2) | Core I-11, Stripe | fail closed | `503`; retry later |
| Saga RP step | RP-3 | retry | Backoff; alert at 30 min |
| Downgrade (B-P4) | Core I-18, Stripe | fail closed | `503 IAM_UNAVAILABLE` / `PROVIDER_UNAVAILABLE` |
| Scheduled downgrade at effect | Core I-18 | fail closed | Schedule released; renews on current plan |
| Seat purchase (B-P7) | Stripe | fail closed | `503`; no seats added |
| Seat renewal reduction | Core I-11, Stripe | skip reduction | Renew at current count; notify |
| Token add-on (B-P20) | Entitlements, Stripe | fail closed | `503` |
| Reactivation (B-P14) | Core I-11 (cancelled), Stripe | fail closed | `503` |
| Webhook intake (B-W1) | Postgres | — | `503` → Stripe retries |
| New account on a provider (B-P2, B-I1, B-O1) | Valid certification (BIL-PROV-1) | fail closed for new accounts | `503 PROVIDER_NOT_ENABLED`; existing accounts unaffected |
| Event publish | SNS | outbox retry | Dead letter → page |
| Audit | AL-5 | async | Buffered in `pending_audit_entries` |
| Reads (B-P1, B-P17) | Postgres | — | Unaffected by IAM / provider; B-P15 `503` if provider down |

**Business signals, not incidents:** `402 PAYMENT_DECLINED` rates, `PLAN_CHANGE_BLOCKED`, `ADDON_NOT_ELIGIBLE` and `SEATS_BELOW_IN_USE` counts are dashboard signals, not alerts.
