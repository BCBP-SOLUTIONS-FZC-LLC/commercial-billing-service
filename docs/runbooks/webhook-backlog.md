# Webhook backlog

**Alert:** `BillingWebhookBacklog` (SEV-3): `received` ledger rows waiting more than 5 minutes (`branch=received`), or `failed` rows for 30 minutes (`branch=failed`). LLD §20.2, §8.15.

1. Read `commercial_billing_webhook_backlog` by status, `commercial_billing_webhook_processing_seconds`, and dependency errors (`platform_dependency_request_seconds{dependency="stripe"}`).
2. **`received` growing:** the processor is stuck or the DB is slow. Check pods, `claimed_until` leases, DB locks. The HPA adds replicas when the backlog exceeds 200.
3. **`failed` growing:** read the `outcome` codes on the rows (B-O7 shows the tenant's recent ledger rows):
   - `provider_unavailable` — wait, see [provider-outage.md](provider-outage.md);
   - `failed:metadata_mismatch` — the object names another tenant: investigate as possible misuse (SEV-3, BIL-PAY-3);
   - `failed:unknown_customer` — objects created outside Billing: reconcile;
   - `failed:checkout_superseded` / `failed:paid_after_end` — parked for an operator, never retried (rev 0.59); they page `BillingPaymentParked` (SEV-2), see [operator-review.md](operator-review.md). A B-O13 replay parks them again;
   - `failed:SUBSCRIPTION_NOT_FOUND` on `invoice.paid`, with a next attempt — the provider subscription is not linked yet (it arrived before `checkout.session.completed`). It is retryable, not ignored: it resolves once the link commits, or from `billing_subscription_id` in the subscription metadata; it parks after `WEBHOOK_MAX_ATTEMPTS` only when neither works;
   - database errors — fix the cause.
4. After the fix, B-O13 `POST /api/v1/operator/billing/webhook-events/:provider/:provider_event_id/replay` replays one row. For bulk replay, an operator SQL script sets `status='received', attempts=0` on the selected rows (same effect; record it in the change ticket).
5. Events Billing never received (endpoint misconfigured): use the provider's "resend", or run a reconciliation with `auto_repair=true`, which ledgers them.

**BIL-OPS-6:** every `failed` row ends retried to success, set `ignored` by an operator replay with a reason, or listed by the next reconciliation run. `failed` rows do not hold back the tenant's later callbacks: the claim skips only tenants with a row in `processing`, and each handler re-reads the provider object.
