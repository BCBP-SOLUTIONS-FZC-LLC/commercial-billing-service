# billing.events outbox dead letter

**Alert:** `BillingOutboxDeadLetter` (SEV-2): `platform_outbox_dead_letters{service="commercial-billing"} > 0`. An event (`TenantSeatsChanged`, `TenantPlanChanged`, `TenantPaymentPastDue`, `TenantSubscriptionCancelled`, `TenantReactivated`, `TenantQuotaAddOnChanged`) failed `OUTBOX_MAX_ATTEMPTS` publishes, so IAM, Entitlements, Notification and Audit did not receive it.

1. Inspect `outbox_dead_letters` (admin_readonly): `event_type`, `last_error`. Typical causes: SNS permissions (IRSA role), topic ARN, Glue schema version missing (deploy ahead of schema registration), payload over the SNS limit.
2. Fix the cause, then requeue with the platform-events runbook (move the row back to `outbox_events`; ordering is per tenant, so requeue a tenant's rows in `ordering_seq` order).
3. Seat drift is self-healing (`seat-projection-check`, §8.6.4); lifecycle events (`TenantPaymentPastDue`, `TenantSubscriptionCancelled`, `TenantReactivated`) are not, so confirm IAM received them after the requeue.
4. Never edit the payload: events are snapshots stamped at commit (BIL-EVT-1).
