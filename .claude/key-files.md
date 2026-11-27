# Key Files

Per-file notes on wiring decisions and where invariants are enforced. When a note and the code disagree, the code wins; fix the note.

## Entry points

- **`cmd/server/main.go`**: composition root. Order:
  1. `config.Load()` (collects every problem; any problem → log each and exit 1, §12);
  2. gincommon logger, tracing, metrics registry; pgcommon pool metrics; Billing metrics; `app.SetTracer`; `httpx.SetObserver`;
  3. optional `RUN_MIGRATIONS` (Kubernetes: the migrate hook Job owns migrations, replicas run with `false`);
  4. `bootstrap.Pools`: `App` (`billing_app`, RLS) and `System` (`billing_system`, BYPASSRLS);
  5. `bootstrap.Providers` + first `registry.Refresh` (certification gate; a failed read leaves no provider enabled for new accounts until the next refresh);
  6. `bootstrap.Audit` (platform-audit, outbox mode);
  7. `bootstrap.NewService`, then the event bus (`events.go`). The deterministic startup checks run here, before any listener opens: Glue schema versions resolved by definition (a deploy ahead of schema registration fails to start) and, outside dev, each enabled queue's `RedrivePolicy` (a transient lookup error gets 3 attempts, then the server starts and resolves the DLQ again on forward; a missing or unusable policy refuses startup);
  8. router: webhook route, API routes, docs routes; background workers started under the lifecycle supervisor;
  9. API on `APP_PORT` (8080), `/metrics` on `METRICS_PORT` (9090, own listener for the NetworkPolicy).

  Shutdown (`cmd/server/lifecycle.go`, budget 50 s, worst case 48 s, `TestShutdownBudgetFits`): `/readyz` fails → `SHUTDOWN_DRAIN_DELAY` → HTTP drain in parallel with consumers, outbox and audit runner → every pool closed once with `DrainAndClose` → metrics listener → tracer, logger flush last. `lifecycle.go` also supervises the in-pod loops: an unexpected stop sets `commercial_billing_background_loop_running{loop}` to 0 and restarts the loop in-process with backoff (1 s → 5 min); `/healthz` fails only on the platform-audit runner's `Alive()`. `main.go` calls `events.InitMetrics` so platform-events' `platform_*` series are emitted.
- **`cmd/server/lifecycle.go`** (new in the round-1/2 fixes): the shutdown plan with its budget comment, readiness/liveness checks, `supervise` (restartable loops; the audit runner is the one non-restartable loop) and the pool closer. Tests in `lifecycle_test.go`.
- **`cmd/server/wiring.go`**, **`events.go`**: route, bus and worker wiring; `events.go` builds the outbox runner and the SQS consumers (`usage-billing-q` only with `USAGE_BILLING_CONSUMER_ENABLED=true`) and holds `requireDLQ`. **`service.go`**: `pollLoop`, the shared poll loop of the in-pod workers. **`identity.go`**: `loggerEnv` (the environment is resolved by `config.ResolveEnv`, which fails closed in Kubernetes without `APP_ENV`).
- **`cmd/jobs/main.go`**: one binary, `--job=<name>`, for the ten CronJobs of LLD §13.2 (schedules in the file; Helm `values.yaml` `cronjobs` must match, asserted by `test/helm`). Exit 0 when the job ran, 1 when it couldn't.
- **`cmd/migrate`**: applies `internal/adapters/postgres/migrations` (`000001`…`000007`, up/down; `000007_rls_helper_grants` narrows `EXECUTE` on the RLS helpers).
- **`cmd/certify`** → **`internal/certify`**: the provider certification suite (`cert-1.0`, LLD §14.7: `run`, `submit` (B-O14), `review` (B-O17)) and `certify check` used by the release gate. Not shipped in the image.
- **`internal/bootstrap/bootstrap.go`**: composition shared by `cmd/server`, `cmd/jobs` and `internal/certify`; the only place (with `cmd/*`) that may import every adapter.

## Domain (`internal/domain`, standard library only)

- **`subscription.go`**: the subscription transition table, the single source for `BIL-SUB-4` (LLD §8.14). Any other move is rejected (`500 INTERNAL` from code, `ignored` from a callback).
- **`events.go`**: produced event types (`ProducedEventTypes`) and their payload `Validate()`. Validation runs before enqueue and backs consumer contracts. For example, refund `purchase_ref` must be a canonical UUID because Entitlements decodes it as one.
- **`errors.go`**: `DomainError` and the §17 catalogue; each code converts to a gincommon `problem.Type` (`https://errors.bcbpsolutions.com/commercial/<code-kebab>`). Use `domain.HasCode` / `errors.As`, never string matching.
- **`money.go`, `money_ops.go`**: `Money{AmountMinor int64, Currency}`, ISO 4217 exponents. **`proration.go`**: fallback proration in `math/big`, half-up once at the end (golden tests from §14.1, `BIL-SEAT-5`).
- **`permission.go`**: `TenantRoutePermission` (one permission per route, `BIL-AUTH-7`) and the role → permission map.
- **`saga.go`**: saga transition table (§9.3). **`addon.go`, `seats.go`, `seat_purchase.go`, `plan_change.go`, `plan_change_request.go`, `conversion.go`, `lifecycle.go`, `certification.go`, `payment_event.go`**: per-flow rules and transition tables. **`entities.go`, `enums.go`**: aggregates and enum values. **`problem.go`**: domain error → gincommon `*problem.Error`.

## App (`internal/app`)

- **`service.go`**: `Service` holds the ports. `locked(ctx, tenantID, fn)` takes the per-tenant advisory lock (`BIL-CONC-1`); every subscription/purchase/saga write goes through it.
- **`authorizer.go`**: `RequirePermission`, `RequireTenantMatch`, `RequirePlatformOperator` (with the `OPERATOR_PATH_LIVE` gate), `RequireBFFCaller`, `RejectSystemCaller`.
- **`direct_signup.go`**: B-I1. Provider customer before the account row, outside any transaction (`BIL-CONC-2`). A retry after 3-D Secure continues the same `incomplete` subscription; a live one, or a different plan, interval or seats, is `409 SUBSCRIPTION_ALREADY_EXISTS`.
- **`conversion.go`, `operator_conversion.go`**: trial conversion saga (B-P2, B-O1), RP-3, `TenantConverted`.
- **`seats.go`, `plan_change.go`, `addons.go`, `lifecycle.go`, `suspension.go`, `money.go`, `admin.go`**: the §8 flows. **`tenant_reads.go`**: payment methods, invoices, history and billing contact (B-P15…B-P18, B-P24…B-P26). **`operator_views.go`**: B-O4…B-O7, B-O13. **`api.go`**: request types (with §10.4 `binding` tags) and §5.4 view types.
- **`purchase_invoice.go`** (rev 0.59/0.60, §8.5.1): settling a seat, upgrade or add-on purchase's invoice outside the happy path. `closeInvoice` re-reads the invoice outside any transaction (`open` → void, `draft` → `DeleteDraftInvoice`, `paid` → settle only when amount and currency match, `BIL-PAY-7`); an unrecorded invoice is found with `FindInvoice` (add-ons by metadata `purchase_id` with 1 min slack, proration invoices by subscription, amount, currency and lines, no backward slack); `recheckInvoice` / `invoiceClaimed` refuse an invoice already recorded on another purchase or plan change under the lock (`BIL-PAY-6`); a mismatch is parked and logged `BillingPaymentParked`.
- **`webhook_processor.go`**: claims ledger rows (`SKIP LOCKED`, per-tenant order), re-reads with `FetchCurrent`, dispatches (`BIL-PAY-4`, `BIL-PAY-5`). Parked outcomes (`failed:checkout_superseded`, `failed:paid_after_end`, `failed:invoice_amount_mismatch`) are not retried and log `BillingPaymentParked`; unlinked `invoice.paid` is resolved through the provider subscription's `billing_subscription_id` metadata (see `flows-and-concurrency.md`).
- **`emission_worker.go`**: `scheduled_emissions` (`BIL-EVT-6`, seats 120 s after conversion).
- **`lifecycle_consumer.go`**: IAM lifecycle events, last-writer-wins on envelope `time` with a 300 s future clamp (`BIL-EVT-3`).
- **`jobs.go`**: CronJob use cases, including `saga-sweeper` (`BillingConversionSagaOverdue`, `BIL-OPS-5`).
- **`unimplemented.go`**: `NotImplementedError` (gincommon 501), a build-phase marker; every route is implemented now, so no route returns it (only `problem_test.go` exercises it).
- **`logging.go`**, **`trace.go`**: the correlated logger (`trace_id`, `span_id`) and the span attribute keys (§11.3, §11.4).
- **`port/`**: every interface. `payment_provider.go` is the provider port (LLD §3.4); no provider type crosses it.

## Adapters (`internal/adapters`)

- **`http/api.go`**: prefixes (`/api/v1/tenants/:id`, `/api/v1/operator`). **`routes.go`**: the route table (44 IDs: B-P1…B-P26, B-I1, B-O1…B-O17; B-W1 is the webhook adapter's): ID, method, path, idempotency rule, handler. **`router.go`**: groups, middleware, probes. **`middleware.go`**: CD-19 headers and the XFCC principal (last element). **`errors.go`**: `writeError` → `gincommon.AbortWithProblem`.
- **`http/idempotency.go`**: §9.2: claim `idempotency_records`, replay the stored response (the same JSON, not the same bytes: `response_body` is `jsonb`) with `Idempotent-Replayed: true`, `409 REQUEST_IN_PROGRESS` + `Retry-After`; B-I1 body mismatch → `409 SUBSCRIPTION_ALREADY_EXISTS`, elsewhere `422 IDEMPOTENCY_KEY_REUSED`.
- **`http/bind.go`**: local JSON decode (unknown fields rejected on tenant/operator routes, tolerated on B-I1, §10.4); errors via `gincommon.BindingError`.
- **`http/openapi.go`**: OpenAPI 3.1 generated from the route table and DTOs, served at `/openapi.json`; `docs/swagger/openapi.json` is kept equal by `TestOpenAPIGolden`.
- **`webhook/handler.go`**: B-W1 `POST /api/v1/webhooks/:provider`: body limit, verify signature, ledger insert before anything else (`BIL-PAY-1`), duplicate → `200`.
- **`consumer/consumer.go`**: `tenant-billing-q`, `usage-billing-q`; Glue decode, embedded-schema check, DLQ on decode, schema or future-time failures.
- **`eventbus/`**: Glue encode codec (`glue_codec.go`, `glue_client.go`; platform-events ships only the decoder), `ValidatingCodec`, embedded `schemas/*.json` (generated from `api/asyncapi.yaml`, don't edit).
- **`postgres/`**: repositories, `tx.go` (`RunInTenantTx`, `RunInSystemTx`, `app.tenant_id` transaction-local), `event_enqueuer.go` (ordered outbox), `audit_writer.go`, `metrics_snapshot.go` (cross-tenant gauges, system pool, aggregates only), `outbox_prune.go`, `migrate.go`, `migrations/`.
- **`provider/stripe/`**: the only `github.com/stripe/*` importer (`BIL-PCI-1`); webhook verifier, normaliser, error mapper. **`provider/manual/`**: the stub provider. **`provider/` (registry)**: certification gate (`BIL-PROV-1`).
- **`iamclient/`**: Core (I-11, I-18) and Realm Provisioner (RP-3, RP-4, RP-11) as `commercial-system`. **`entitlementsclient/`**: the eligibility `GET`. Both use **`httpx`** (timeouts, dependency metrics, tracing).
- **`metrics/`**: the 33 Tier-3 `commercial_billing_*` metrics (§11.2) on gincommon's registry, including `commercial_billing_background_loop_running{loop}`; `enqueuer.go` (`CountingEnqueuer`, events emitted), `push.go` (CronJob Pushgateway push). `TestInventoryUpToDate` keeps `docs/observability/metric-registry.md` in sync.
- **`observability/`**: `port.Tracer` on gincommon's tracer (the only package allowed OpenTelemetry types).

## Shared package (`pkg/`)

- **`pkg/requestctx`**: `RequestContext{UserID, TenantID, Roles, ClientIP, UserAgent}` carried through a request (`With`, `From`, `HasRole`). Imports nothing from the project; only `adapters/http` (router) and `adapters/httpx` (`call.go`, `X-Request-ID` forwarding) import it (`.go-arch-lint.yml`). Production code uses only `WithRequestID` / `RequestID`; `With`, `From` and `HasRole` have no production caller, and the CD-19 headers are constants in `call.go`.

## Deployment (`deploy/`)

- **`helm/templates/_helpers.tpl`**: names, labels, component selector labels (`server` / `cronjob` / `migrate`), per-workload secret keys, `commercial-billing.validate` (the render guards: `metrics.scrapeMode`; for staging and prod, PeerAuthentication, AuthorizationPolicy and NetworkPolicy on, no `000000000000` placeholder, IRSA role ARN, SNS and tenant-queue values, `networkPolicy.postgresCIDRs`), `istioAnnotations` (native sidecar).
- **`helm/templates/_networkpolicy.tpl`**: egress rules shared by the server, CronJob and migrate NetworkPolicies (DNS, istiod, Postgres, observability, HTTPS with the optional `egressGateway`, `awsEndpointCIDRs`). **`networkpolicy.yaml`**: the three policies.
- **`helm/templates/peerauthentication.yaml`**: `STRICT` mTLS on the server and CronJobs; the metrics port `PERMISSIVE` only with `metrics.scrapeMode: plaintext`.
- **`helm/templates/migrate-job.yaml`**: hook ServiceAccount `<fullname>-migrate` (no IRSA) and hook Secret (`MIGRATION_DATABASE_URL` only) at weight -10, deleted on success or failure; the Job at weight -5, no sidecar, `activeDeadlineSeconds` 480, `MIGRATE_TIMEOUT` 7m.
- **`helm/templates/gateway-policies.yaml`**: rate limits (tenant 20 rps per tenant; B-W1 50 rps per client IP, one rule for IPv4 and one for IPv6) and the optional webhook source CIDRs.
- **`iam/`**: the IRSA role's reference policy (`policy.json`, `policy.tf.example`, `README.md`); platform Terraform owns the role.
- **`monitoring/`**: `app-alerts.yml`, `recording-rules.yml`, `slo-rules.yml`, `platform-audit.rules.yml`, `schema-registry-alerts.yml`, `prometheus-adapter-rule.yaml`, `dashboards/` (chart copies of the rules and the Billing dashboard in `helm/files/`).

## Tests (`test/`, tag `integration` unless noted)

- `postgres/`: RLS, constraints, triggers, repositories, ledger claims, outbox, inbox.
- `flows/`: per-flow integration (§8).
- `e2e/`: the production router and middleware over HTTP on the real `Service` and Postgres 17.
- `scenarios/{commerce,jobs,lifecycle}/`: less-travelled paths of the use cases (scripted provider outcomes, injected storage faults) on Postgres.
- `testkit/`: the shared harness of `e2e/` and `scenarios/` (Postgres 17 testcontainer with the production roles, seeded price book, recording fakes for the provider, Core, RP and Entitlements; a non-test package behind the `integration` tag).
- `bus/` (floci SNS/SQS/Glue), `stripemock/`, `certify/`.
- `contract/` (no tag): produced payloads vs Core, RP, Audit and Entitlements consumed schemas, vendored under `testdata/consumers`.
- `helm/` (tag `helm`): chart contract.

The Test Clock rehearsal is `scripts/testclock/` (`make testclock`), not a Go suite.
