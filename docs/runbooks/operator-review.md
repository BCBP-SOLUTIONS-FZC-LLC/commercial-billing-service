# Parked payment: superseded checkout, ended subscription or amount mismatch

**Alert:** `BillingPaymentParked` (SEV-2, log-based; LLD §8.1.1, §8.5.1, §8.15, `BIL-OPS-6`, `BIL-PAY-7`). Two log lines carry `alert=BillingPaymentParked` and `severity=SEV-2`:
- "provider payment on a checkout Billing replaced or ended; operator decision needed (SEV-2)" (`internal/app/conversion.go`), with `tenant_id`, `billing_subscription_id`, `provider`, `provider_event_id` and `reason`;
- "provider payment does not match the purchase; operator decision needed (SEV-2)" (`internal/app/purchase_invoice.go`, rev 0.60), with `tenant_id`, `purchase_kind` (`seats`, `token_addon`, `plan`), `purchase_id`, `provider_invoice_id`, `provider_event_id` (empty when `purchase-expiry` or a decline found it) and `reason`.

This page matches only `alert=BillingPaymentParked`. The no-page review trigger for refunds and adjustments above `REVIEW_THRESHOLD_MINOR` (`BillingReviewTrigger`, §10.6) is a different line: "operator money action above REVIEW_THRESHOLD_MINOR (review trigger)" with `alert=BillingOperatorReviewNeeded` (`internal/app/money.go`).

The paging rule is a Loki rule owned by the observability team (not in `deploy/monitoring/`): it must match `alert="BillingPaymentParked"`.

**What happened:** the provider took a payment Billing will not apply. The ledger row is `failed:<reason>` with no retry:
- `failed:checkout_superseded` — the customer completed a checkout session Billing had already expired (a newer B-P2 replaced it, or it lapsed). Billing never links it to the ended incomplete subscription;
- `failed:paid_after_end` — the first invoice of a provider subscription was paid after Billing ended the subscription it belongs to. For a direct signup (B-I1) this is a 3-D Secure payment completed after Billing expired the charge (rev 0.68): the customer was charged and got `410 CHARGE_EXPIRED`; refund it.
- `invoice_amount_mismatch` (rev 0.60, `BIL-PAY-7`) — a provider invoice Billing tied to a seat purchase, upgrade or token add-on was paid for another amount or currency than the purchase's. From `invoice.paid` the ledger row is `failed:invoice_amount_mismatch`; from `purchase-expiry` or a decline the purchase is closed as unpaid (expired or failed). Nothing is granted.

**For `checkout_superseded` and `paid_after_end`:** the provider now holds a live subscription and a payment that Billing has no subscription for. The tenant's own subscription (the newer checkout, if any) is unaffected.

1. Find the row: B-O7 for the tenant shows its recent ledger rows; the provider event id is in the log line. Confirm in the Stripe dashboard which subscription and invoice the event belongs to, and that it is not the tenant's current subscription (B-O7 `provider_subscription_id`).
2. In the Stripe dashboard, **cancel the stray provider subscription** (immediately, no proration) and **refund its payment in full**.
3. Record the decision and both Stripe ids in the incident. If the tenant meant to pay through that checkout, ask them to start a new one (B-P2).
4. Leave the ledger row as it is: it is the record of the incident. A B-O13 replay parks it again, since Billing's subscription stays ended.

**For `invoice_amount_mismatch`:** the customer paid an invoice that does not match what Billing priced, and the seats, plan or tokens were not granted.

1. Find the purchase: `purchase_id` and `provider_invoice_id` are in the log line; B-O7 shows the tenant's subscription and recent ledger rows.
2. In the Stripe dashboard, compare the invoice's amount paid, total and currency with the purchase's amount (a manual edit, a coupon or a customer-balance change on the invoice are the usual causes).
3. Decide with Finance: **refund** the invoice in full (B-O3 or the Stripe dashboard) and ask the tenant to buy again, or, when the amount difference is legitimate, **re-bill** the difference in the provider. Billing never grants the purchase from this invoice; do not edit the purchase row.
4. Record the decision and the Stripe ids in the incident. Leave the ledger row as it is; a B-O13 replay parks it again.

Never link the stray subscription by hand or edit the subscription row: a second live provider subscription would bill the tenant twice (`BIL-SUB-3`).
