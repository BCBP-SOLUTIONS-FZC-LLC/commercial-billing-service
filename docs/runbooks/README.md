# Billing runbooks

Operational runbooks for `commercial-billing` (LLD §20). Every alert in
`deploy/monitoring/app-alerts.yml` links to one of these pages (its
`runbook_url`); `make helm-check` fails if a linked page is missing.

| Alert / situation | Severity | Runbook |
|---|---|---|
| `BillingConversionSagaOverdue`, `BillingSagaFailed` | SEV-2 | [stuck-conversion-saga.md](stuck-conversion-saga.md) |
| `CommercialPurchaseNotActive` | SEV-2 | [purchase-not-active.md](purchase-not-active.md) |
| `BillingWebhookSignatureFailures` | SEV-2 | [webhook-signature-failures.md](webhook-signature-failures.md) |
| `BillingWebhookBacklog` | SEV-3 | [webhook-backlog.md](webhook-backlog.md) |
| `BillingOutboxDeadLetter` | SEV-2 | [outbox-dead-letter.md](outbox-dead-letter.md) |
| `BillingDLQNotEmpty` | SEV-3 | [consumer-dlq.md](consumer-dlq.md) |
| `BillingI18Unavailable` | SEV-3 | [i18-unavailable.md](i18-unavailable.md) |
| `BillingProviderUnavailable` | SEV-2 | [provider-outage.md](provider-outage.md) |
| `BillingProviderNotCertified` | SEV-2 | [certification-lapsed.md](certification-lapsed.md) |
| `BillingSuspensionDraftsAwaitingDecision` | SEV-3 | [suspension-drafts.md](suspension-drafts.md) |
| `BillingReconciliationDrift` | SEV-3 | [reconciliation-drift.md](reconciliation-drift.md) |
| `BillingSeatProjectionDrift` | SEV-3 | [seat-projection-drift.md](seat-projection-drift.md) |
| `BillingReversalDecisionNeeded` (log-based) | SEV-2 | [reversal-decision.md](reversal-decision.md) |
| `BillingAvailabilityBudgetBurnFast` / `…Slow` | SEV-2 / SEV-3 | [slo-burn.md](slo-burn.md) |
| Provider certification (cmd/certify) | planned | [certification.md](certification.md) |
| Stripe key / webhook secret rotation | planned | [key-rotation.md](key-rotation.md) |
| Disaster recovery (RTO 30 min, RPO 5 min) | — | [disaster-recovery.md](disaster-recovery.md) |
| Dependency degradation matrix, business signals | reference | [dependency-degradation.md](dependency-degradation.md) |
| Stripe Test Clock rehearsal (go-live evidence) | staging | [test-clock-rehearsal.md](test-clock-rehearsal.md) |

Conventions: B-O routes are the operator API (LLD §5.4.4); every operator
mutation needs `Idempotency-Key` and a `reason` of 10–1000 characters, and
writes an AL-5 audit entry. Operator routes answer `403` until
`OPERATOR_PATH_LIVE=true` (BIL-AUTH-6).
