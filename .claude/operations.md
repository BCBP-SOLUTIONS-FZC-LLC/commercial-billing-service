# Operations

LLD §11 (observability), §12 (configuration), §13 (deployment), §19 (first deployment, providers), §20 (runbooks). Nothing is deployed to any environment yet; go-live waits on the LLD §20.10 checks and the cross-team items in `docs/lld/billing-open-items.md`.

## Configuration (§12, `internal/config/config.go`)

`config.Load()` (server) / `config.LoadFor(config.BinaryJobs)` collects every missing, malformed or out-of-range value, and the binary refuses to start on any of them. Required secrets differ per binary (server: `DATABASE_URL`, `PG_SYSTEM_PASSWORD`, `PG_AUDIT_DRAINER_PASSWORD`, `STRIPE_API_KEY`, `STRIPE_WEBHOOK_SECRETS`, `LOG_HASH_SALT`, plus `MIGRATION_DATABASE_URL` only with `RUN_MIGRATIONS=true`; jobs: the same without the audit drainer; migrate: `MIGRATION_DATABASE_URL`). In Kubernetes (`KUBERNETES_SERVICE_HOST` set) an unset `APP_ENV` fails closed. Every key has bounds; change a default in `config.go`, Helm `values.yaml`, `.env-example` (if used locally) and LLD §12 together.

| Area | Keys (defaults) |
|---|---|
| Server | `APP_PORT` (8080), `METRICS_PORT` (9090, must differ), `REQUEST_TIMEOUT` (30 s, 5–60 s), `SHUTDOWN_DRAIN_DELAY` (2 s, 0–5 s) |
| Feature flags | `FEATURE_DIRECT_SIGNUP` (false), `FEATURE_TOKEN_ADDONS` (false), `USAGE_BILLING_CONSUMER_ENABLED` (false, BL-OQ-24), `FEATURE_SEAT_PURCHASE` (true in Helm), `OPERATOR_PATH_LIVE` (false), `AUTO_REVERT_ON_EFW` (false), `AUTO_PAUSE_ON_OPERATOR_SUSPENSION` (true) |
| Database | `PG_*` (app pool `billing_app`), `PG_SYSTEM_*` (`billing_system`), `PG_AUDIT_DRAINER_*`, `MIGRATION_DATABASE_URL`, `RUN_MIGRATIONS` (false in Kubernetes) |
| Bus | `SNS_BILLING_EVENTS_TOPIC_ARN`, `SQS_TENANT_BILLING_QUEUE_URL` (server only), `SQS_USAGE_BILLING_QUEUE_URL` (only with `USAGE_BILLING_CONSUMER_ENABLED=true`), `GLUE_REGISTRY_NAME`, `SQS_MAX_RECEIVE_COUNT`, `SQS_DRAIN_TIMEOUT` / `OUTBOX_DRAIN_TIMEOUT` (capped at 15 s), `OUTBOX_*` (HLD §9.3 v0.66: `OUTBOX_MAX_ATTEMPTS` 10, `OUTBOX_RETRY_BACKOFF` 30 s, `OUTBOX_MAX_RETRY_BACKOFF` 5 min) |
| Provider | `STRIPE_API_KEY`, `STRIPE_WEBHOOK_SECRETS`, `STRIPE_API_VERSION`, `STRIPE_TIMEOUT_MS` (8000), `ENABLED_PAYMENT_PROVIDERS`, `DEFAULT_PAYMENT_PROVIDER`, `PROVIDER_REGISTRY_REFRESH`, `PROVIDER_CERT_SUITE_VERSION` |
| Callees | `CORE_BASE_URL` / `CORE_TIMEOUT_MS` (3000), `RP_BASE_URL` / `RP_TIMEOUT_MS` (30000), `ENTITLEMENTS_BASE_URL` / `ENTITLEMENTS_TIMEOUT_MS` (1000) |
| Identity | `BFF_CALLER_PRINCIPAL`, `COMMERCIAL_SYSTEM_PRINCIPAL` |
| Timers | `SCA_ABANDON_AFTER` (30 min), `SAGA_ALERT_AFTER` (fixed 30 min, `BIL-OPS-5`), `SEATS_AFTER_CONVERSION_DELAY` (120 s), `IDEMPOTENCY_TTL_HOURS` (72), `PURCHASE_ABANDON_AFTER`, `CHECKOUT_TTL` |
| Money | `DEFAULT_CURRENCY`, `MIN_CHARGE_MINOR`, `REVIEW_THRESHOLD_MINOR`, `PRORATION_BELOW_MINIMUM` |

Secrets (`STRIPE_*` keys, webhook secrets, DB passwords, `LOG_HASH_SALT`) come from `ExternalSecret`s, one key set per workload (`secretKeys.server` / `jobs` / `migrate`). They never go in values files, logs or `.env` commits. Live and test Stripe keys never coexist in one environment (`BIL-OPS-7`).

## CronJobs (§13.2, `cmd/jobs`)

Ten jobs, one binary (`jobs --job=<name>`), `concurrencyPolicy: Forbid`, `activeDeadlineSeconds` 900 (schedules from Helm `values.yaml`):

| Job | Schedule |
|---|---|
| `saga-sweeper` | `* * * * *` |
| `seat-renewal` | `*/15 * * * *` |
| `downgrade-apply` | `*/15 * * * *` |
| `purchase-expiry` | `*/5 * * * *` |
| `seat-projection-check` | `7,37 * * * *` |
| `pii-minimisation` | `0 5 * * *` |
| `outbox-prune` | `0 3 * * *` |
| `processed-events-prune` | `0 4 * * *` |
| `renewal-repricing` | `20 * * * *` |
| `idempotency-prune` | `30 4 * * *` |

Schedules are in `cmd/jobs/main.go` and Helm `values.yaml` (`cronjobs.jobs`, rendered by `cronjobs.yaml`; `test/helm` asserts they match). `pii-minimisation` never deletes a row or touches a financial record (`BIL-OPS-9`).

## Deployment (§13.1, `deploy/helm`)

- **Workload:** Helm chart 0.7.0 (`kubeVersion >=1.30`); namespace `billing`; ServiceAccount and label `commercial-billing`; at least 2 replicas across AZs; HPA; PDB. Pods carry `app.kubernetes.io/component` `server` / `cronjob` / `migrate`, and server selectors pick `component=server` only. `terminationGracePeriodSeconds` 60 with a native `preStop` sleep of 10 s (Kubernetes ≥ 1.30); probe timeout 6 s. `automountServiceAccountToken: false`. CronJob pools: `PG_MAX_CONNS` 3, `PG_MIN_CONNS` 0, `PG_SYSTEM_MAX_CONNS` 2.
- **Migrations:** the migrate hook Job owns them, as hook ServiceAccount `commercial-billing-migrate` (no IRSA) with a hook Secret holding only `MIGRATION_DATABASE_URL` (weight -10, deleted on success or failure; needs Helm ≥ 3.19, CI pins 3.21.4), no sidecar. `MIGRATE_TIMEOUT` 7m and `activeDeadlineSeconds` 480, both under `release.yml`'s Helm `--timeout=10m`.
- **Startup and shutdown:** deterministic checks (configuration, Glue schema versions, each enabled queue's `RedrivePolicy` outside dev, a transient lookup error gets 3 attempts, then the server starts and resolves the DLQ again on forward) run before the listeners open. Shutdown: readiness off → `SHUTDOWN_DRAIN_DELAY` (≤ 5 s) → API drain ∥ workers (≤ 25 s) → pools closed once with `DrainAndClose` (≤ 11 s) → metrics listener (≤ 2 s) → tracer, logger last; worst case 48 s within 50 s. A background loop that stops is restarted in-process with backoff (1 s → 5 min) and alerted; liveness fails only on the platform-audit runner's `Alive()`.
- **Mesh:** Istio native sidecars for the server and the CronJobs (Istio ≥ 1.24, Kubernetes ≥ 1.30). The sidecar's inbound listener appends the verified client certificate to `X-Forwarded-Client-Cert` (`APPEND_FORWARD`, the sidecar default; `forwardClientCertDetails` in ProxyConfig applies to gateways only); B-I1 reads the last element (`BIL-AUTH-3`). `PeerAuthentication` `STRICT` on Billing's workloads prevents plaintext spoofing. `metrics.scrapeMode`: `plaintext` (default; Prometheus outside the mesh, metrics port `PERMISSIVE`, authorised by `metrics.monitoringCIDRs` or by NetworkPolicy alone) or `mesh` (`STRICT` everywhere, authorised by namespace). In `mesh` mode the ServiceMonitor's TLS settings are platform-specific.
- **NetworkPolicy and AuthorizationPolicy:**
  - B-I1 is admitted only from `bff/bff-signup` on 8080;
  - egress to Entitlements is allowed only for the eligibility `GET`;
  - the monitoring namespace may reach `/metrics`;
  - HTTPS egress to public addresses, plus `networkPolicy.awsEndpointCIDRs` (server only) for VPC interface endpoints and optional `networkPolicy.egressGateway`;
  - CronJobs and migrate have their own NetworkPolicies (CronJobs: DNS, istiod, Postgres, Core and RP, 443, Pushgateway, OTel; migrate: DNS and 5432).
- **Gateway:** tenant and operator `HTTPRoute`s; the operator route lists B-O1…B-O17; B-W1 limited to 50 rps per client IP (one bucket per IPv4 / IPv6 client; depends on the gateway's `numTrustedHops`), optional `gateway.webhook.allowedSourceCIDRs`.
- **Values:** `values-dev.yaml`, `values-staging.yaml`, `values-prod.yaml`. For staging and prod the chart refuses to render with `PeerAuthentication`, `AuthorizationPolicy` or `NetworkPolicy` disabled, the placeholder account `000000000000`, no IRSA role ARN, empty SNS / tenant queue values or empty `networkPolicy.postgresCIDRs`. `release.yml` sets these values for the production deploy only, from the `production` environment's `BILLING_*` variables; a staging deploy must supply them itself.
- **AWS:** the IRSA role's policy is in `deploy/iam/` (SNS publish, SQS consume and DLQ forward, Glue read-only). Don't add S3, KMS, Secrets Manager or CloudWatch, or Glue writes (CI-only).
- **HPA external metric:** needs `deploy/monitoring/prometheus-adapter-rule.yaml` in prometheus-adapter (`max`, not `sum`: every pod reports the same backlog).
- **Checks:** `make helm-check` (lint, kubeconform, promtool, chart contract tests in `test/helm`).
- **First deployment order:** LLD §19.1, checked with `make check-first-deploy ENV=…`.
- **Release gate:** a provider adapter change bumps `AdapterVersion()`. The release then fails until a certification record exists in staging (`certify check`, §14.6). The Stripe adapter is `1.1.0` (LLD rev 0.60) and still needs its certification.
- **Go-live checks:** LLD §20.10 (Pushgateway `honorLabels`, kube-state-metrics CronJob families, AWS endpoint CIDRs, client IP trust, scrape mode, Stripe 1.1.0 certification, `production` deployment-branch rules). Production changes need a PCI owner group reviewer.

## Observability (§11)

- **Metrics (Enterprise Platform Observability Standard):**
  - Tier 1 `platform_*`: registry names only.
  - Tier 2 `commercial_*`: none, since nothing is ratified; proposals are in `docs/observability/registry-proposals.md`.
  - Tier 3 `commercial_billing_*`: built with gincommon's validated constructors.
  - Identity labels are injected centrally.
  - The registry is `docs/observability/metric-registry.md`, generated with `make metrics-inventory`.
  - `make metrics-lint` checks naming, suffixes, labels and registry compliance, and that every metric referenced by alerts, rules, SLOs, the dashboard, HPA and runbooks exists.
- **Rules:** recording and SLO rules are in `deploy/monitoring/`. `schema-registry-alerts.yml` holds the four Glue registration alerts (from `schema-gov metrics` in `schema-registry.yml`, via the Pushgateway); their runbook is `docs/runbooks/schema-registration.md`.
- **Alerts:** `deploy/monitoring/app-alerts.yml`, 27 rules for 20 alerts. Each alert has a `runbook_url` into `docs/runbooks/`, which `test/helm` checks:
  - `BillingConversionSagaOverdue`, `BillingSagaFailed`, `CommercialPurchaseNotActive`;
  - `BillingWebhookSignatureFailures`, `BillingWebhookBacklog`;
  - `BillingOutboxDeadLetter`, `BillingDLQNotEmpty`, `BillingQueueBacklog`, `BillingQueueStalled`, `BillingOutboxLag`;
  - `BillingBackgroundLoopStopped` (the loop restarts in-process; no liveness failure), `BillingMetricsSnapshotStale`, `BillingCronJobNotSucceeding` (one rule per job cadence), `BillingCronJobNeverSucceeded` (kube-state-metrics; SEV-2 `saga-sweeper`, SEV-3 others);
  - `BillingI18Unavailable`, `BillingProviderUnavailable`, `BillingProviderNotCertified`;
  - `BillingSuspensionDraftsAwaitingDecision`, `BillingReconciliationDrift`, `BillingSeatProjectionDrift` (per-run gauge, `repeat="true"`).
- **Log-based alerts** (Loki rules owned by the observability team): `BillingReversalDecisionNeeded` (SEV-2), `BillingReviewTrigger` (no page), and the SEV-2 `BillingPaymentParked` (ledger `failed:checkout_superseded` / `failed:paid_after_end` / `failed:invoice_amount_mismatch`, runbook `operator-review.md`). `BillingReviewTrigger` is raised from the line `alert=BillingOperatorReviewNeeded`.
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

`docs/runbooks/`: 27 files, the `README.md` index plus 26 runbooks. Every `runbook_url` in `deploy/monitoring/*.yml` resolves to one of them (`test/helm` checks `app-alerts.yml`). Runbooks not behind a Prometheus alert:
- `operator-review` (the log-based `BillingPaymentParked` and `BillingReviewTrigger`);
- `reversal-decision` (the log-based `BillingReversalDecisionNeeded`);
- `dependency-degradation`;
- `disaster-recovery`;
- `key-rotation`;
- `certification`;
- `test-clock-rehearsal`.

## CI/CD (`.github/`)

- Seven workflows: `ci.yml` (orchestrator), `validate-test.yml`, `validate-quality.yml`, `release.yml`, `schema-registry.yml`, `changelog-check.yml`, `contract-live.yml`; scripts in `.github/scripts/` (guards, `coverage-gate.sh`, `helm-check.sh`, `verify-release-tag.sh`, `smoke-tests.sh`, …).
- Hardening (round 1/2): third-party actions pinned by commit SHA; checkouts with `persist-credentials: false` (token `CI_REPO_READ_TOKEN` or `github.token`); `GO_PRIVATE_TOKEN` only in the module-download step through a credential helper, then `GOPROXY=off`; schema-gov built from a pinned commit with hash-locked Python dependencies (`.github/schema-gov-requirements.txt`, `pip --require-hashes`); AWS through GitHub OIDC (`vars.AWS_ROLE_ARN`); a release tag must be on `main` (`verify-release-tag.sh`); the deploy gate checks the deployed digest, then 5xx < 1% for 2 min, else `helm rollback`.
- `release.yml` passes the production-only `BILLING_*` variables (IRSA role, SNS topic, tenant queue, Postgres CIDRs; optional usage queue, webhook source CIDRs, AWS endpoint CIDRs, monitoring CIDRs) to Helm.
