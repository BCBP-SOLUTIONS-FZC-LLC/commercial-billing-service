# Seat projection drift

**Alert:** `BillingSeatProjectionDrift` (SEV-3): Core's `licensed_seats` differed from Billing's seat item more than once in 24 h. LLD §8.6.4, BIL-EVT-5.

`seat-projection-check` (every 30 min) queues `TenantSeatsChanged {reason: "resync"}` for each mismatch older than `SEAT_RESYNC_AFTER`. A repeat means the resync did not stick.

1. Find the tenant: the SEV-3 log line `seat projection mismatch again within 24 h` (fields `tenant_id`, `billing_seats`, `core_licensed_seats`). The metric carries no tenant.
2. Check that the resync event was published (outbox) and that Core consumed it on `billing-orgm-q` (Core's EVT-14 recency guard can skip an event stamped earlier than one it already applied — the root cause BL-OQ-2 tracks).
3. Billing's seat item is the record (it changes only on paid purchases and renewals, BIL-SUB-5). If Core is right (e.g. a manual IAM change), fix Billing's record through support with Finance; otherwise ask IAM to apply the resync.
