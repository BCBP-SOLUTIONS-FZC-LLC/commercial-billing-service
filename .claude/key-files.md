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
  7. `bootstrap.NewService`, then the event bus (Glue schema versions are resolved here, so a deploy ahead of schema registration fails to start);
  8. router: webhook route, API routes, docs routes; background workers;
  9. API on `APP_PORT` (8080), `/metrics` on `METRICS_PORT` (9090, own listener for the NetworkPolicy).

  Shutdown: HTTP drain → workers stop → both pools drain → `gincommon.Shutdown`.
- **`cmd/server/wiring.go`**, **`events.go`**: route, bus and worker wiring. **`identity.go`**: `resolveAppEnv`.
- **`cmd/jobs/main.go`**: one binary, `--job=<name>`, for the ten CronJobs of LLD §13.2 (schedules in the file; Helm `cronjobs.yaml` must match, asserted by `test/helm`). Exit 0 when the job ran, 1 when it couldn't.
- **`cmd/migrate`**: applies `internal/adapters/postgres/migrations` (`000001`…`000005`, up/down).
- **`cmd/certify`**: the provider certification suite (`cert-1.0`, LLD §14.7) and `certify check` used by the release gate.
- **`internal/bootstrap/bootstrap.go`**: composition shared by `cmd/server`, `cmd/jobs` and `internal/certify`; the only place (with `cmd/*`) that may import every adapter.

## Domain (`internal/domain`, standard library only)

- **`subscription.go`**: the subscription transition table, the single source for `BIL-SUB-4` (LLD §8.14). Any other move is rejected (`500 INTERNAL` from code, `ignored` from a callback).
- **`events.go`**: produced event types (`ProducedEventTypes`) and their payload `Validate()`. Validation runs before enqueue and backs consumer contracts. For example, refund `purchase_ref` must be a canonical UUID because Entitlements decodes it as one.
- **`errors.go`**: `DomainError` and the §17 catalogue; each code converts to a gincommon `problem.Type` (`https://errors.bcbpsolutions.com/commercial/<code-kebab>`). Use `domain.HasCode` / `errors.As`, never string matching.
- **`money.go`, `money_ops.go`**: `Money{AmountMinor int64, Currency}`, ISO 4217 exponents. **`proration.go`**: fallback proration in `math/big`, half-up once at the end (golden tests from §14.1, `BIL-SEAT-5`).
- **`permission.go`**: `TenantRoutePermission` (one permission per route, `BIL-AUTH-7`) and the role → permission map.
- **`saga.go`**: saga transition table (§9.3). **`addon.go`, `seats.go`, `seat_purchase.go`, `plan_change.go`, `conversion.go`, `lifecycle.go`, `certification.go`, `payment_event.go`**: per-flow rules and transition tables. **`problem.go`**: domain error → gincommon `*problem.Error`.

## App (`internal/app`)

- **`service.go`**: `Service` holds the ports. `locked(ctx, tenantID, fn)` takes the per-tenant advisory lock (`BIL-CONC-1`); every subscription/purchase/saga write goes through it.
- **`authorizer.go`**: `RequirePermission`, `RequireTenantMatch`, `RequirePlatformOperator` (with the `OPERATOR_PATH_LIVE` gate), `RequireBFFCaller`, `RejectSystemCaller`.
- **`direct_signup.go`**: B-I1. Provider customer before the account row, outside any transaction (`BIL-CONC-2`). A retry after 3-D Secure continues the same `incomplete` subscription; a live one, or a different plan, interval or seats, is `409 SUBSCRIPTION_ALREADY_EXISTS`.
- **`conversion.go`, `operator_conversion.go`**: trial conversion saga (B-P2, B-O1), RP-3, `TenantConverted`.
- **`seats.go`, `plan_change.go`, `addons.go`, `lifecycle.go`, `suspension.go`, `money.go`, `admin.go`**: the §8 flows.
- **`webhook_processor.go`**: claims ledger rows (`SKIP LOCKED`, per-tenant order), re-reads with `FetchCurrent`, dispatches (`BIL-PAY-4`, `BIL-PAY-5`).
- **`emission_worker.go`**: `scheduled_emissions` (`BIL-EVT-6`, seats 120 s after conversion).
- **`lifecycle_consumer.go`**: IAM lifecycle events, last-writer-wins on envelope `time` with a 300 s future clamp (`BIL-EVT-3`).
- **`jobs.go`**: CronJob use cases, including `saga-sweeper` (`BillingConversionSagaOverdue`, `BIL-OPS-5`).
- **`unimplemented.go`**: `NotImplementedError` (501) for routes whose flow isn't built yet; each marked with its phase.
- **`port/`**: every interface. `payment_provider.go` is the provider port (LLD §3.4); no provider type crosses it.

## Adapters (`internal/adapters`)

- **`http/api.go`**: prefixes (`/api/v1/tenants/:id`, `/api/v1/operator`). **`routes.go`**: the route table: ID, method, path, idempotency rule, handler. **`router.go`**: groups and middleware. **`middleware.go`**: CD-19 headers and the XFCC principal.
- **`http/idempotency.go`**: §9.2: claim `idempotency_records`, replay byte for byte with `Idempotent-Replayed: true`, `409 REQUEST_IN_PROGRESS` + `Retry-After`; B-I1 body mismatch → `409 SUBSCRIPTION_ALREADY_EXISTS`, elsewhere `422 IDEMPOTENCY_KEY_REUSED`.
- **`http/bind.go`**: local JSON decode (unknown fields rejected on tenant/operator routes, tolerated on B-I1, §10.4); errors via `gincommon.BindingError`.
- **`http/openapi.go`**: OpenAPI 3.1 generated from the route table and DTOs, served at `/openapi.json`; `docs/swagger/openapi.json` is kept equal by `TestOpenAPIGolden`.
- **`webhook/handler.go`**: B-W1 `POST /api/v1/webhooks/:provider`: body limit, verify signature, ledger insert before anything else (`BIL-PAY-1`), duplicate → `200`.
- **`consumer/consumer.go`**: `tenant-billing-q`, `usage-billing-q`; Glue decode, embedded-schema check, DLQ on decode, schema or future-time failures.
- **`eventbus/`**: Glue codecs, `ValidatingCodec`, embedded `schemas/*.json` (generated from `api/asyncapi.yaml`, don't edit).
- **`postgres/`**: repositories, `tx.go` (`RunInTenantTx`, `RunInSystemTx`, `app.tenant_id` transaction-local), `event_enqueuer.go` (ordered outbox), `audit_writer.go`, `migrations/`.
- **`provider/stripe/`**: the only `github.com/stripe/*` importer (`BIL-PCI-1`); webhook verifier, normaliser, error mapper. **`provider/manual/`**: the stub provider. **`provider/` (registry)**: certification gate (`BIL-PROV-1`).
- **`iamclient/`**: Core (I-11, I-18) and Realm Provisioner (RP-3, RP-4, RP-11) as `commercial-system`. **`entitlementsclient/`**: the eligibility `GET`. Both use **`httpx`** (timeouts, dependency metrics, tracing).
- **`metrics/`**: §11.2 metrics on gincommon's registry; `TestInventoryUpToDate` keeps `docs/observability/metric-registry.md` in sync.

## Tests (`test/`)

`postgres/` (RLS, constraints, triggers; tag `integration`), `flows/` (per-flow integration), `bus/` (floci SNS/SQS/Glue), `stripemock/`, `certify/`, `contract/` (produced payloads vs Core, RP, Audit and Entitlements consumed schemas, vendored under `testdata/consumers`), `helm/` (tag `helm`), `testclock/`.
