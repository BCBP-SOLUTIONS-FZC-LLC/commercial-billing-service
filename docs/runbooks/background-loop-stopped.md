# Background loop stopped

**Alert:** `BillingBackgroundLoopStopped` (SEV-3): an in-pod loop returned while the pod was not shutting down. LLD §13.1.

Every in-pod worker runs supervised (`cmd/server/lifecycle.go`): `outbox`, `sqs:tenant-billing-q`, `sqs:usage-billing-q`, `webhook-processor`, `saga-worker`, `lifecycle-follow-up`, `reconciliation`, `emissions`, `metrics-snapshot`, `provider-registry`, `audit-runner`. When one returns, its `commercial_billing_background_loop_running{loop}` drops to 0 and the loop is restarted **in the same process** with backoff (1 s, doubling to 5 min; the gauge returns to 1 at each restart). Liveness is not failed: a pod restart cannot cure a deterministic failure and would take the tenant API and B-W1 down on every replica at once. The alert keeps firing for 10 minutes after the last stop, so a restart loop stays visible.

Deterministic problems are checked before the listeners open, so a bad deploy fails startup (and `helm --atomic` rolls it back) instead of the running pods: configuration, Glue schema versions, and each enabled queue's RedrivePolicy outside dev (a transient lookup error is retried three times, then tolerated).

The exception is `audit-runner` (platform-audit's runner is single-use): it is not restarted, and its own `Alive()` (stopped runner or stalled heartbeat) fails `/healthz` (check `background_loops`), so the kubelet restarts the container.

1. Find the cause: log line `background loop stopped unexpectedly; restarting` (fields `loop`, `error`, `retry_in_ms`) on the pod (`kubectl -n billing logs <pod>`).
2. Typical causes:
   - `sqs:*`: the consumer's DLQ check failed at start (RedrivePolicy removed after the deploy, or SQS / IAM failing); see [consumer-dlq.md](consumer-dlq.md) and the IRSA policy in `deploy/iam/`. The consumer retries on its own once the queue is fixed.
   - `outbox`: the runner was already running or its schema is missing (`outbox.ApplySchema`, run by the migrate hook).
   - `audit-runner`: Audit Log heartbeat stall; see the platform-audit runbook (`PlatformAuditDeliveryStalled`); this one restarts the pod.
3. A loop that keeps stopping (the log line every few minutes, gauge mostly 0): fix the dependency or configuration it names; the loop recovers by itself at the next retry. If a bug causes it, roll back the release. Restarting the pod is not needed (and only helps for `audit-runner`).

Liveness never checks dependencies (Postgres, SNS, Stripe); those fail readiness or their own alerts.
