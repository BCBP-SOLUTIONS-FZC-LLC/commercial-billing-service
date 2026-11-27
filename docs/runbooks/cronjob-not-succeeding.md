# CronJob not succeeding

**Alerts** (SEV-2 for `saga-sweeper`, SEV-3 otherwise), LLD §13.2:
- `BillingCronJobNotSucceeding`: a Billing CronJob has not exited 0 within about three schedule intervals (it succeeded before, so a success stamp exists).
- `BillingCronJobNeverSucceeded`: a Billing CronJob that Kubernetes has scheduled at least once has no success at all, neither recorded by Kubernetes nor pushed, for 15 minutes. Typical cause: a run refused for its configuration exits before it pushes anything, so `BillingCronJobNotSucceeding` has no series to read.

Each run of `jobs --job=<name>` pushes its metrics to the Pushgateway under `job=<name>`. A run that exits 0 (including `partial`: failed items are retried next run) sets `commercial_billing_cron_job_last_success_timestamp_seconds{job_name}` and replaces the group; a failed run is POSTed, keeping the previous stamp. `commercial_billing_cross_tenant_job_runs_total{job_name,result}` holds the last run's result.

| Job | Schedule | Fires after |
|---|---|---|
| `saga-sweeper` | every minute | 10 min |
| `purchase-expiry` | every 5 min | 20 min |
| `seat-renewal`, `downgrade-apply` | every 15 min | 45 min |
| `seat-projection-check` | :07, :37 | 90 min |
| `renewal-repricing` | hourly | 3 h |
| `pii-minimisation`, `outbox-prune`, `processed-events-prune`, `idempotency-prune` | daily | 26 h |

1. Did it run? `kubectl -n billing get jobs,cronjobs`; `concurrencyPolicy: Forbid` skips a run while the previous one still runs, and `startingDeadlineSeconds` drops missed starts.
2. Why did it fail? `kubectl -n billing logs job/<job>`: `job run failed` / `job setup failed` (configuration, database, provider registry).
3. Configuration: jobs need `DATABASE_URL`, `PG_SYSTEM_PASSWORD`, `LOG_HASH_SALT`, `STRIPE_API_KEY`, `STRIPE_WEBHOOK_SECRETS` outside dev; `invalid configuration` names the key.
4. Pushgateway down or `PUSHGATEWAY_URL` wrong: the job logs `job metrics not pushed` and the stamp does not move although the job ran. A Pushgateway restart without persistence loses the stamps; `BillingCronJobNotSucceeding` then stays silent until the next success, and `BillingCronJobNeverSucceeded` stays silent while Kubernetes still records a past success (`kube_cronjob_status_last_successful_time`).

`BillingCronJobNeverSucceeded` reads kube-state-metrics: `kube_cronjob_status_last_schedule_time` (exists once a Job was created, so a fresh deployment does not page before the first schedule), `kube_cronjob_status_last_successful_time`, and the pushed stamp mapped to the CronJob name `commercial-billing-<job>`. Check the history with `kubectl -n billing get cronjob commercial-billing-<job> -o jsonpath='{.status}'` and `kubectl -n billing get jobs -l app.kubernetes.io/component=cronjob`.

## Go-live checks

- **Pushgateway scrape uses `honorLabels: true`.** The ServiceMonitor (or scrape config) for the Pushgateway must set `honorLabels: true`. Otherwise Prometheus renames the pushed `job` and `service` labels to `exported_job` / `exported_service`, every `{service="commercial-billing"}` selector on pushed series matches nothing, and neither `BillingCronJobNotSucceeding` nor `BillingSeatProjectionDrift` can fire (`BillingCronJobNeverSucceeded` still catches a job that never succeeded, from kube-state-metrics). Check: `commercial_billing_cron_job_last_success_timestamp_seconds{service="commercial-billing"}` returns one series per job after the first runs, and no series carries `exported_service`.
- **kube-state-metrics exposes the CronJob families** (`kube_cronjob_status_last_schedule_time`, `kube_cronjob_status_last_successful_time`) for namespace `billing`, with the CronJob name in the `cronjob` label (the kube-state-metrics default; check any `--metric-allowlist` / `--namespaces` flags). Without them `BillingCronJobNeverSucceeded` never fires.

Effects of a stopped job are in LLD §13.2 (for `saga-sweeper`: overdue sagas are not alerted, BIL-OPS-5).
