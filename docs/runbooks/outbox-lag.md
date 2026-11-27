# Outbox lag

**Alert:** `BillingOutboxLag` (SEV-2): the oldest unpublished `billing.events` row in the outbox is older than 5 minutes, for 10 minutes. LLD §7.3, BIL-EVT-2.

Source: `platform_outbox_oldest_pending_age_seconds` (platform-events, refreshed every `OUTBOX_GAUGE_INTERVAL`, 15 s; when its query fails the gauge keeps its last value and `platform_outbox_errors_total{operation="oldest_pending"}` counts the failure). Related: `platform_outbox_pending_events`, `platform_outbox_ordering_blocked_events`, `platform_outbox_errors_total`.

Impact: IAM (seats, plan) and Entitlements (add-ons) see paid changes late; `CommercialPurchaseNotActive` follows.

1. Is the outbox runner alive on every pod? `commercial_billing_background_loop_running{loop="outbox"}` and the log `background loop stopped unexpectedly` ([background-loop-stopped.md](background-loop-stopped.md)).
2. SNS or Glue failing? `platform_dependency_request_seconds_count{dependency="sns",outcome="error"}`, publish errors in the logs. Glue schema errors: [schema-registration.md](schema-registration.md). The runner backs off per row (`OUTBOX_RETRY_BACKOFF`, max `OUTBOX_MAX_RETRY_BACKOFF`) and dead-letters after `OUTBOX_MAX_ATTEMPTS` ([outbox-dead-letter.md](outbox-dead-letter.md)).
3. Ordering blocked? Events are ordered per tenant: one failing head blocks its tenant's later rows (`platform_outbox_ordering_blocked_events`). Find the head in `outbox_events` and its error.
4. Database slow or pool exhausted: the claim query runs on the app pool.

**After a SIGKILL or OOM kill:** rows the killed pod had claimed stay leased for the claim lease, about 10 min 20 s (`outboxClaimLease` in `cmd/server/events.go`: the larger of 10 min and `OUTBOX_BATCH_SIZE` × the 10 s publish timeout + 2 min). No other pod publishes them before the lease expires, and because events are ordered per tenant, the later events of those tenants wait behind them. The rows are not lost: they are published once the lease expires. The lag alert (5 min age, `for: 10m`) may therefore fire only after the lease has already expired and the backlog is draining; check `platform_outbox_pending_events` falling before acting. A graceful shutdown (SIGTERM) releases nothing early either, but drains the batch in flight (`OUTBOX_DRAIN_TIMEOUT`).

Do not edit `outbox_events` by hand (BIL-OPS-2); use platform-events' reprocess / discard operations on dead letters.
