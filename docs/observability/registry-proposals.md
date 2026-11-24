# Registry proposals — Commercial domain (Tier 2) and platform entries Billing depends on

Submitted for observability governance approval under the Enterprise Platform Observability Standard
(rules 10–12, "Registry ratification requirement"). **Not adopted:** until a proposal is ratified,
Billing emits the concept as its own Tier 3 `commercial_billing_*` series
([metric-registry.md](metric-registry.md)). On ratification, Billing switches to the shared name
through gincommon's `NewShared*` constructors, together with its dashboards, alerts, recording rules,
SLOs and HPA references. No compatibility period is needed while no environment is deployed; once one
is, the standard's migration process applies (parallel emission, migrate consumers, deprecate, remove).

## 1. Proposed Commercial domain-shared metrics (Tier 2, domain `commercial`)

Rationale: Entitlements and Usage & Metering also enqueue domain events, expose operator routes and
run cross-tenant jobs. With these entries, the same question gets the same answer across the domain.
Billing is the only current emitter, so under the decision tree they stay Tier 3 until a second
service adopts the ratified name.

### `commercial_events_emitted_total` (counter)

| Field | Value |
|---|---|
| Semantics | Domain events enqueued in the service's transactional outbox, counted when the enqueue succeeds inside the business transaction (a rolled-back transaction is counted; `platform_outbox_*` covers publication) |
| Required labels | `domain`, `service`, `environment` (central) |
| Additional labels | `event_type` (registry label: envelope type, bounded by the producer's event catalogue) |
| Aggregation | `sum by (service, event_type) (rate(...))`; summed across pods |
| Cardinality | services × event types (Billing: 6) |
| Billing's current series | `commercial_billing_events_emitted_total{event_type}` |

### `commercial_operator_actions_total` (counter)

| Field | Value |
|---|---|
| Semantics | Calls to a service's operator routes (`/api/v1/operator/*`), counted after the response is written, refused calls included |
| Required labels | `domain`, `service`, `environment` |
| Additional labels | `action` — **new label, needs approval**: the route id (`B-O1`…`B-O17` for Billing; a closed set per service, ≤ 50); `outcome` (registry: `success` < 400, `failure` 4xx, `error` 5xx) |
| Aggregation | `sum by (service, action, outcome) (rate(...))` |
| Cardinality | services × route ids × 3 |
| Billing's current series | `commercial_billing_operator_actions_total{action, outcome}` |

### `commercial_cross_tenant_job_runs_total` (counter)

| Field | Value |
|---|---|
| Semantics | One completed run of a cross-tenant job (CronJob or in-pod run such as a reconciliation), by its final result |
| Required labels | `domain`, `service`, `environment` |
| Additional labels | `job_name` — **new label, needs approval**: the CronJob or job name (closed set per service, ≤ 20). `result` — **new label, needs approval**: `success`, `partial` (some items failed and are retried next run), `failure`. Proposed instead of `outcome`, because `partial` is not in `outcome`'s vocabulary |
| Aggregation | `sum by (service, job_name, result) (increase(...))`; CronJobs push to the Pushgateway with grouping key `job=<job_name>`, scraped with `honor_labels` |
| Cardinality | services × jobs × 3 |
| Billing's current series | `commercial_billing_cross_tenant_job_runs_total{job_name, result}` |

**Not proposed:** `commercial_purchase_confirmation_to_event_seconds`. Only Billing confirms purchases,
so it is permanently service-specific (`commercial_billing_purchase_confirmation_to_event_seconds`).

## 2. Platform registry entries Billing relies on (status "proposed")

These belong to the platform libraries; Billing uses them as defined and requests their ratification.
Alerts that read them carry `metric_status: proposed`.

| Metric | Emitted by | Billing's use |
|---|---|---|
| `platform_dependency_request_seconds{dependency, operation, outcome}` | Billing's `httpx` (stripe, core, realm_provisioner, entitlements), platform-events, platform-audit | `BillingProviderUnavailable`, `BillingI18Unavailable`, dashboard, recording rules |
| `platform_http_requests_total`, `platform_http_request_duration_seconds` | platform-gincommon | Availability and latency SLOs (`slo-rules.yml`) |
| `platform_event_propagation_seconds{queue, event_type}` | platform-events (consumers) | `CommercialPurchaseNotActive` (composed) |
| `platform_queue_depth`, `platform_dlq_depth{queue}` | platform-events (queue-depth sampler) | `BillingDLQNotEmpty`, dashboard |
| `platform_outbox_pending_events`, `platform_outbox_dead_letters` | platform-events (outbox runner) | `BillingOutboxDeadLetter`, dashboard |
| `platform_build_info{version}` | platform-gincommon | release tracking |

## 3. Label vocabulary notes for the registry

Billing's Tier 3 labels outside the shared vocabulary are service-owned (standard: Tier 3 needs no
cross-service alignment), bounded, and documented in [metric-registry.md](metric-registry.md):
`result`, `kind`, `type`, `status`, `saga_state`, `direction`, `blocker`, `pack_code`, `plan`,
`provider`, `currency`, `by`, `to`, `builder`, `repaired`, `adapter_version`, `job_name`, `action`.
`outcome` is used only with the registry values (`success`, `failure`, `error`).
