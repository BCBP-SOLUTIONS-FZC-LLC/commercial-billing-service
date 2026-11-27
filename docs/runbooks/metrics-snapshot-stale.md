# Metrics snapshot stale

**Alert:** `BillingMetricsSnapshotStale` (SEV-3): no successful database snapshot for more than 5 minutes (normally every 30 s). LLD §11.2.

The exporter refreshes `commercial_billing_webhook_backlog`, `commercial_billing_sagas_in_flight`, `commercial_billing_sagas_overdue`, `commercial_billing_subscriptions`, `commercial_billing_collection_paused`, `commercial_billing_invoices_held_for_review` and `commercial_billing_scheduled_emissions_pending` from the `billing_system` pool. When the query fails the gauges keep their last value and `commercial_billing_metrics_snapshot_last_success_timestamp_seconds` stops moving.

Impact: alerts on those gauges (saga overdue, webhook backlog, held drafts) and the HPA's external metric (`webhook_backlog{status="received"}`) read frozen values.

1. Log line `metrics snapshot failed; gauges keep their last value` (field `error`).
2. Database: `/readyz` (`sys_database`), `PG_STATEMENT_TIMEOUT` hits, the `billing_system` role's password or grants after a rotation.
3. If only one pod is failing, restart it; the gauges are aggregated with `max` across pods, so a healthy pod's reading wins for alerts but not for staleness.
4. While stale, read the tables directly (`payment_event_ledger`, `conversion_sagas`) before acting on a saga or backlog alert.
