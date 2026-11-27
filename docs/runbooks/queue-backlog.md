# SQS queue backlog / stalled consumer

**Alerts:**
- `BillingQueueBacklog` (SEV-3): more than 100 messages waiting on `tenant-billing-q` or `usage-billing-q` for 10 minutes.
- `BillingQueueStalled` (SEV-2): messages waiting and none processed for 15 minutes, so the oldest message is at least 15 minutes old.

Source: `platform_queue_depth{queue}` (sampled by platform-events every `SQS_QUEUE_DEPTH_INTERVAL`, 60 s) and `platform_messages_processed_total{queue}`. No registry metric carries the oldest message's age; CloudWatch `ApproximateAgeOfOldestMessage` on the queue gives it if needed. `usage-billing-q` is consumed only with `USAGE_BILLING_CONSUMER_ENABLED=true` (BL-OQ-24), so it has no series otherwise.

Impact: `tenant-billing-q` carries IAM's tenant lifecycle events, including `TenantConverted`, which confirms conversion sagas. A stalled queue leads to `BillingConversionSagaOverdue` within 30 minutes.

1. Are consumers running? `kubectl -n billing get pods -l app.kubernetes.io/name=commercial-billing`; restarts point to `BillingBackgroundLoopStopped` ([background-loop-stopped.md](background-loop-stopped.md)). Logs: `sqs consumer stopped`, `background loop stopped unexpectedly`.
2. Are handlers failing? `rate(platform_messages_failed_total{service="commercial-billing"}[5m])` by queue, and the consumer error logs. Repeated failures end in the DLQ ([consumer-dlq.md](consumer-dlq.md)).
3. Database: handlers take the per-tenant lock (BIL-CONC-1). Check `/readyz`, pool saturation and lock waits.
4. Volume spike (bulk tenant changes in IAM): the backlog drains by itself if `platform_messages_processed_total` is rising; scale replicas if needed (each replica runs one consumer per queue).
5. AWS: SQS throttling or IRSA errors show in `platform_dependency_request_seconds_count{dependency="sqs",outcome="error"}`.

Resolve when `platform_queue_depth` is back near 0.
