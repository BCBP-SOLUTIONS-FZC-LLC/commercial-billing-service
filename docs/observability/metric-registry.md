# commercial-billing metric inventory

Generated from the code (internal/adapters/metrics) by `make metrics-inventory`; CI fails when it drifts.
Enterprise Platform Observability Standard (LLD §11.2, rev 0.43): Tier 1 `platform_*` only from the
Platform Observability Registry, no Tier 2 `commercial_*` until ratified (proposals in
[registry-proposals.md](registry-proposals.md)), Billing's own series as Tier 3 `commercial_billing_*`.
Every series carries the central const labels `domain="commercial"`, `service="commercial-billing"`, `environment`
(platform-gincommon); no tenant, user, request or event id is ever a label.

Series emitted by the platform libraries (HTTP, messaging, outbox, DB pools, platform-audit) are
listed in their registries, not here.

| Metric | Type | Tier | Labels | Meaning |
|---|---|---|---|---|
| `commercial_billing_addon_purchases_total` | counter | 3 service | `pack_code`, `result` | Token pack purchases, by pack and result. |
| `commercial_billing_collection_paused` | gauge | 3 service | `by` | Subscriptions with collection paused (CD-24), by origin. |
| `commercial_billing_conversion_saga_duration_seconds` | histogram | 3 service | `kind` | Payment confirmed to TenantConverted, by saga kind. |
| `commercial_billing_cross_tenant_job_runs_total` | counter | 3 service | `job_name`, `result` | Cross-tenant job runs (CronJobs and reconciliation), by job name and result (success \| partial \| failure). |
| `commercial_billing_downgrades_blocked_total` | counter | 3 service | `blocker` | Downgrades blocked by I-18, by blocker. |
| `commercial_billing_dunning_transitions_total` | counter | 3 service | `to` | Lapse-flow transitions, by target status. |
| `commercial_billing_events_emitted_total` | counter | 3 service | `event_type` | billing.events events enqueued, by event type. |
| `commercial_billing_invoice_dispositions_total` | counter | 3 service | `action` | B-O16 decisions, by action. |
| `commercial_billing_invoices_held_for_review` | gauge | 3 service | `currency` | Drafts held during a collection pause with no B-O16 decision, by currency. |
| `commercial_billing_operator_actions_total` | counter | 3 service | `action`, `outcome` | Operator route calls, by action (route id) and outcome. |
| `commercial_billing_payment_failures_total` | counter | 3 service | `reason` | Payment declines, by mapped reason. |
| `commercial_billing_proration_deferred_amount_minor_total` | counter | 3 service | `currency` | Sum of deferred_amount_minor not charged, by currency. |
| `commercial_billing_proration_deferred_total` | counter | 3 service | `currency`, `reason` | Seat purchases added free until renewal (§8.5.3), by currency and reason. |
| `commercial_billing_provider_certified` | gauge | 3 service | `adapter_version`, `provider` | 1 when the loaded adapter version has a valid certification (BIL-PROV-1). |
| `commercial_billing_purchase_confirmation_to_event_seconds` | histogram | 3 service | `kind` | Payment confirmed to the billing.events event committed, by purchase kind (Billing's share of the 60 s targets). |
| `commercial_billing_reconciliation_findings` | gauge | 3 service | `kind`, `repaired` | Findings of the last reconciliation run, by kind and repaired. |
| `commercial_billing_repricing_scheduled_total` | counter | 3 service | `direction` | Renewal price moves scheduled (§8.16), by direction. |
| `commercial_billing_rp_convert_attempts_total` | counter | 3 service | `result` | RP-3 calls, by result. |
| `commercial_billing_sagas_in_flight` | gauge | 3 service | `saga_state` | Open conversion sagas, by saga state. |
| `commercial_billing_sagas_overdue` | gauge | 3 service | — | Sagas not confirmed 30 minutes after payment (alert source). |
| `commercial_billing_scheduled_emissions_pending` | gauge | 3 service | `builder` | Pending delayed emissions (BIL-EVT-6), by builder. |
| `commercial_billing_seat_changes_total` | counter | 3 service | `direction`, `result` | Seat changes, by direction and result. |
| `commercial_billing_seat_projection_mismatches_total` | counter | 3 service | — | Seat projection mismatches found (§8.6.4). |
| `commercial_billing_subscriptions` | gauge | 3 service | `plan`, `provider`, `status` | Subscriptions, by status, plan and provider (exporter). |
| `commercial_billing_unmatched_conversions_total` | counter | 3 service | — | TenantConverted events with no saga. |
| `commercial_billing_webhook_backlog` | gauge | 3 service | `status` | Ledger rows waiting, by status (exporter, 30 s). |
| `commercial_billing_webhook_processing_seconds` | histogram | 3 service | `kind`, `provider` | Callback received to processed. |
| `commercial_billing_webhook_signature_failures_total` | counter | 3 service | `provider` | Callbacks refused for a bad signature or livemode (BIL-PAY-2). |
| `commercial_billing_webhooks_total` | counter | 3 service | `provider`, `result`, `type` | Provider callbacks, by provider, normalised type and result. |
| `platform_dependency_request_seconds` | histogram | 1 platform (registry) | `dependency`, `operation`, `outcome` | Client-observed wall time of one call to an external dependency — SNS (publish, publish_batch), SQS (receive_message, delete_message, change_message_visibility, send_message, get_queue_attributes, get_queue_url) or the configured schema-registry codec (encode, decode) — by outcome. The _count series counts calls. receive_message is a long poll: its duration includes up to WaitTimeSeconds (default 20 s) of waiting on an empty queue, so exclude it from latency views (it stays meaningful for error rates). |

## Deprecated (compatibility period)

Emitted in parallel with their replacements while `OBSERVABILITY_LEGACY_METRICS=true` (default during the
compatibility period). No alert, recording rule, SLO, dashboard, HPA or runbook reads them (CI). Removed after
the approved sunset.

| Deprecated name | Replacement |
|---|---|
| `commercial_cross_tenant_job_runs_total` | `commercial_billing_cross_tenant_job_runs_total` |
| `commercial_events_emitted_total` | `commercial_billing_events_emitted_total` |
| `commercial_operator_actions_total` | `commercial_billing_operator_actions_total` |
| `commercial_purchase_confirmation_to_event_seconds` | `commercial_billing_purchase_confirmation_to_event_seconds` |
