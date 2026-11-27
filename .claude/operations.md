# Operations

LLD §11 (observability), §12 (configuration), §13 (deployment), §19 (first deployment, providers), §20 (runbooks). Nothing is deployed yet.

## Configuration (§12, `internal/config/config.go`)

`config.Load()` collects every missing, malformed or out-of-range value, and the server refuses to start on any of them. Every key has bounds; change a default in `config.go`, Helm `values.yaml`, `.env-example` (if used locally) and LLD §12 together.

| Area | Keys (defaults) |
|---|---|
| Server | `APP_PORT` (8080), `METRICS_PORT` (9090, must differ), `REQUEST_TIMEOUT` (30 s, 5–60 s) |
| Feature flags | `FEATURE_DIRECT_SIGNUP` (false), `FEATURE_TOKEN_ADDONS` (false), `FEATURE_SEAT_PURCHASE` (true in Helm), `OPERATOR_PATH_LIVE` (false), `AUTO_REVERT_ON_EFW` (false), `AUTO_PAUSE_ON_OPERATOR_SUSPENSION` (true) |
| Database | `PG_*` (app pool `billing_app`), `PG_SYSTEM_*` (`billing_system`), `PG_AUDIT_DRAINER_*`, `MIGRATION_DATABASE_URL`, `RUN_MIGRATIONS` (false in Kubernetes) |
| Bus | `SNS_BILLING_EVENTS_TOPIC_ARN`, `SQS_TENANT_BILLING_QUEUE_URL`, `SQS_USAGE_BILLING_QUEUE_URL`, `GLUE_REGISTRY_NAME`, `SQS_MAX_RECEIVE_COUNT`, `OUTBOX_*` |
| Provider | `STRIPE_API_KEY`, `STRIPE_WEBHOOK_SECRETS`, `STRIPE_API_VERSION`, `STRIPE_TIMEOUT_MS` (8000), `ENABLED_PAYMENT_PROVIDERS`, `DEFAULT_PAYMENT_PROVIDER`, `PROVIDER_REGISTRY_REFRESH`, `PROVIDER_CERT_SUITE_VERSION` |
| Callees | `CORE_BASE_URL` / `CORE_TIMEOUT_MS` (3000), `RP_BASE_URL` / `RP_TIMEOUT_MS` (30000), `ENTITLEMENTS_BASE_URL` / `ENTITLEMENTS_TIMEOUT_MS` (1000) |
| Identity | `BFF_CALLER_PRINCIPAL`, `COMMERCIAL_SYSTEM_PRINCIPAL` |
| Timers | `SCA_ABANDON_AFTER` (30 min), `SAGA_ALERT_AFTER` (fixed 30 min, `BIL-OPS-5`), `SEATS_AFTER_CONVERSION_DELAY` (120 s), `IDEMPOTENCY_TTL_HOURS` (72), `PURCHASE_ABANDON_AFTER`, `CHECKOUT_TTL` |
| Money | `DEFAULT_CURRENCY`, `MIN_CHARGE_MINOR`, `REVIEW_THRESHOLD_MINOR`, `PRORATION_BELOW_MINIMUM` |

Secrets (`STRIPE_*` keys, webhook secrets, DB passwords, `LOG_HASH_SALT`) come from `ExternalSecret`. They never go in values files, logs or `.env` commits. Live and test Stripe keys never coexist in one environment (`BIL-OPS-7`).

## CronJobs (§13.2, `cmd/jobs`)

Ten jobs, one binary (`jobs --job=<name>`), `concurrencyPolicy: Forbid`:

- `saga-sweeper`
- `seat-renewal`
- `downgrade-apply`
- `purchase-expiry`
- `seat-projection-check`
- `pii-minimisation`
- `outbox-prune`
- `processed-events-prune`
- `renewal-repricing`
- `idempotency-prune`

Schedules are in `cmd/jobs/main.go` and Helm `cronjobs.yaml` (`test/helm` asserts they match). `pii-minimisation` never deletes a row or touches a financial record (`BIL-OPS-9`).

## Deployment (§13.1, `deploy/helm`)

- **Workload:** namespace `billing`; ServiceAccount and label `commercial-billing`; at least 2 replicas across AZs; HPA; PDB. The migrate hook Job owns migrations.
- **Mesh:** Istio. The pod annotation `proxy.istio.io/config` sets `forwardClientCertDetails: SANITIZE_SET` so B-I1 receives `X-Forwarded-Client-Cert` (`BIL-AUTH-3`).
- **NetworkPolicy and AuthorizationPolicy:**
  - B-I1 is admitted only from `bff/bff-signup` on 8080;
  - egress to Entitlements is allowed only for the eligibility `GET`;
  - the monitoring namespace may reach `/metrics`.
- **Gateway:** tenant and operator `HTTPRoute`s; the operator route lists B-O1…B-O17.
- **Values:** `values-dev.yaml`, `values-staging.yaml`, `values-prod.yaml`.
- **AWS:** the IRSA role's policy is in `deploy/iam/` (SNS publish, SQS consume and DLQ forward, Glue read-only). Don't add S3, KMS, Secrets Manager or CloudWatch, or Glue writes (CI-only).
- **HPA external metric:** needs `deploy/monitoring/prometheus-adapter-rule.yaml` in prometheus-adapter (`max`, not `sum`: every pod reports the same backlog).
- **Checks:** `make helm-check` (lint, kubeconform, promtool, chart contract tests).
- **First deployment order:** LLD §19.1, checked with `make check-first-deploy ENV=…`.
- **Release gate:** a provider adapter change bumps `AdapterVersion()`. The release then fails until a certification record exists in staging (`certify check`, §14.6). Production changes need a PCI owner group reviewer.

## Observability (§11)

- **Metrics (Enterprise Platform Observability Standard):**
  - Tier 1 `platform_*`: registry names only.
  - Tier 2 `commercial_*`: none, since nothing is ratified; proposals are in `docs/observability/registry-proposals.md`.
  - Tier 3 `commercial_billing_*`: built with gincommon's validated constructors.
  - Identity labels are injected centrally.
  - The registry is `docs/observability/metric-registry.md`, generated with `make metrics-inventory`.
  - `make metrics-lint` checks naming, suffixes, labels and registry compliance, and that every metric referenced by alerts, rules, SLOs, the dashboard, HPA and runbooks exists.
- **Rules:** recording and SLO rules are in `deploy/monitoring/`. `schema-registry-alerts.yml` holds the four Glue registration alerts (from `schema-gov metrics` in `schema-registry.yml`, via the Pushgateway); their runbook is `docs/runbooks/schema-registration.md`.
- **Alerts:** `deploy/monitoring/app-alerts.yml`. Each alert has a `runbook_url` into `docs/runbooks/`, which `test/helm` checks:
  - `BillingConversionSagaOverdue`, `BillingSagaFailed`, `CommercialPurchaseNotActive`;
  - `BillingWebhookSignatureFailures`, `BillingWebhookBacklog`;
  - `BillingOutboxDeadLetter`, `BillingDLQNotEmpty`;
  - `BillingI18Unavailable`, `BillingProviderUnavailable`, `BillingProviderNotCertified`;
  - `BillingSuspensionDraftsAwaitingDecision`, `BillingReconciliationDrift`, `BillingSeatProjectionDrift`.
- **Traces:** OTel through gincommon, with span links over events (§11.3).
- **Logs:** structured, with no PII or card data (§11.4).

## Dependencies and failure behaviour

| Dependency | Down → |
|---|---|
| Postgres | `503 DB_UNAVAILABLE`; `/readyz` fails (`database`, `sys_database`) |
| Stripe | `503 PROVIDER_UNAVAILABLE`, `Retry-After`; webhooks queue in the ledger |
| Core I-18 | Downgrade blocked (`503`), alert `BillingI18Unavailable` |
| Core I-11 | Display fields `null`; seat-floor operations fail with `503` |
| Realm Provisioner | Saga backs off and retries; pages after 30 min |
| Entitlements | Add-on purchase `503 ENTITLEMENTS_UNAVAILABLE` (fail closed); pack list from a 60 s cache |
| SNS / Glue | Outbox holds events and retries; dead letters alert. A missing Glue schema blocks startup |
| SQS consumers | Retries, then DLQ at receive 4 (`BillingDLQNotEmpty`) |

## Runbooks

`docs/runbooks/*.md`, one per alert plus:
- `disaster-recovery`;
- `key-rotation`;
- `certification`;
- `test-clock-rehearsal`.
