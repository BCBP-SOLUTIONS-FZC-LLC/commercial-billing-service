# Seat projection drift

**Alert:** `BillingSeatProjectionDrift` (SEV-3): the last `seat-projection-check` run found a tenant whose `licensed_seats` in Core differed from Billing's seat item again within 24 h of a resync. LLD §8.6.4, BIL-EVT-5.

Source: `commercial_billing_seat_projection_last_run_mismatches{repeat="true"}`, a per-run gauge pushed by the job (`repeat="false"` counts first-time mismatches). The next clean run sets it to 0 and resolves the alert. If the job itself is failing, see [cronjob-not-succeeding.md](cronjob-not-succeeding.md).

**Go-live check:** the Pushgateway's ServiceMonitor (or scrape config) must use `honorLabels: true`. Otherwise the pushed `service` and `job` labels arrive as `exported_service` / `exported_job`, the `{service="commercial-billing"}` selector matches nothing and this alert can never fire. Check that `commercial_billing_seat_projection_last_run_mismatches{service="commercial-billing"}` returns series after the first run ([cronjob-not-succeeding.md](cronjob-not-succeeding.md#go-live-checks)).

`seat-projection-check` (every 30 min) queues `TenantSeatsChanged {reason: "resync"}` for each mismatch older than `SEAT_RESYNC_AFTER`. A repeat means the resync did not stick.

1. Find the tenant: the SEV-3 log line `seat projection mismatch again within 24 h` (fields `tenant_id`, `billing_seats`, `core_licensed_seats`). The metric carries no tenant.
2. Check that the resync event was published (outbox) and that Core consumed it on `billing-orgm-q` (Core's EVT-14 recency guard can skip an event stamped earlier than one it already applied — the root cause BL-OQ-2 tracks).
3. Billing's seat item is the record (it changes only on paid purchases and renewals, BIL-SUB-5). If Core is right (e.g. a manual IAM change), fix Billing's record through support with Finance; otherwise ask IAM to apply the resync.
