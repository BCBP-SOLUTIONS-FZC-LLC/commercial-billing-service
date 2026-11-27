# Payment provider outage

**Alert:** `BillingProviderUnavailable` (SEV-2): more than half of Stripe calls fail for 5 minutes. LLD §20.4.

1. Confirm on the Stripe status page. Tenant purchase routes already answer `503 PROVIDER_UNAVAILABLE` with retry guidance.
2. Nothing to fail over in Phase 1 (one provider). Access is unaffected: Billing is off the hot path.
3. Sagas wait (no provider dependency after payment); webhooks queue at Stripe and drain on recovery. CronJobs retry their items on the next run.
4. After recovery: watch the backlog drain ([webhook-backlog.md](webhook-backlog.md)) and run a reconciliation for the outage window (B-O5 with `auto_repair=true`).
