# Development Guide

## First-Time Setup

```bash
cp .env-example .env       # values match docker-compose.yml; never commit .env
make docker-up             # Postgres 17 :5634, PgBouncer :5633, floci :4667, stripe-mock :12111
make migrate
make run                   # API on :8180, metrics on :9190 (from .env-example)
```

Private platform modules: `GOPRIVATE=github.com/BCBP-SOLUTIONS-FZC-LLC/*` (set by the Makefile). Locally they are fetched over SSH; CI uses `GO_PRIVATE_TOKEN`. If a platform module can't be fetched, stop and say so. **Never stub or reimplement a platform library.**

## Running Tests

| Command | What | Needs |
|---|---|---|
| `make test` | all packages, `-race`; unit, contract, golden (OpenAPI, metric inventory) | — |
| `make test-integration` | `./test/...` with tag `integration`: Postgres (RLS, constraints, triggers), flows, bus, stripe-mock | Docker |
| `make test-contract` | `./test/contract/...` | — |
| `make helm-check` | chart lint, kubeconform, promtool, `test/helm` (tag `helm`) | helm, promtool |
| `make certify-rehearsal` | `cert-1.0` against stripe-mock | Docker |

Contract tests read consumers' schemas from `test/contract/testdata/consumers` (vendored, commit recorded in its `README.md`). `CONTRACT_CONSUMERS_DIR` points them at another copy; `.github/workflows/contract-live.yml` uses it to test against Entitlements' `main` daily.

Run one test: `go test ./internal/adapters/http -run TestB_I1 -count=1`; integration: add `-tags=integration`.

## Invariant tests

Every `BIL-*` invariant has at least one test whose name contains its ID (`TestBIL_SEAT_3_RenewalNeverBelowSeatsInUse`), and a comment with the ID at its enforcement point. Update `docs/invariants-coverage.md` (enforced in, test, status) in the same change.

## Adding or changing an endpoint

1. Check the LLD §5.3 catalogue and §5.4 spec: ID, method, path, permission, idempotency rule, error codes. Don't add endpoints the LLD doesn't define.
2. Add the row in `internal/adapters/http/routes.go` (route table) and its DTO; tenant routes need exactly one permission in `domain.TenantRoutePermission`.
3. Use case in `internal/app`; errors as `domain.NewError(domain.Err…, …)`, never a hand-written problem.
4. Regenerate the OpenAPI golden: `go test ./internal/adapters/http -run Golden -update`.
5. If the operator route list changes, update Helm `httproutes.yaml` (asserted by `test/helm` and `helm_routes_test.go`).

## Adding or changing an event

1. Edit `api/asyncapi.yaml` (the source), then `make schema-extract` → `internal/adapters/eventbus/schemas/*.json`.
2. Payload type and `Validate()` in `internal/domain/events.go`; add a sample in `test/contract` so it is validated against Billing's and consumers' schemas.
3. Publish only through the ordered outbox (`EventEnqueuer`), in the same transaction as the state change.
4. Schemas are open (additive changes only); Glue registration per environment precedes the deploy, since the server resolves schema versions at start.

## Adding an outbound HTTP client

Use `httpx.New(dependency, timeout)` (dependency metrics, tracing). Never call it inside a transaction (`BIL-CONC-2`). Timeouts come from config (`*_TIMEOUT_MS`), with bounds in `config.go` and values in Helm. The identity sent to IAM is `commercial-system` (CD-19).

## Database changes

New migration pair in `internal/adapters/postgres/migrations` (next number, up + down). Every tenant-scoped table: `ENABLE` + `FORCE ROW LEVEL SECURITY` and the `tenant_isolation` policy (`BIL-RLS-1`, checked by a test over `pg_class`). Add a test that each new CHECK or trigger rejects bad data. Never put card data, provider keys or webhook secrets in a column (`BIL-PCI-2`).

## Common Pitfalls

- **HTTP inside a transaction:** commit intent, call outside, then commit the outcome under `Service.locked` with a re-read.
- **Forgetting the tenant lock:** background writers (webhook processor, consumers, jobs) take the same lock as requests.
- **`float64` for money**, or rounding more than once.
- **Trusting a webhook payload:** re-read with `FetchCurrent`; resolve the tenant from the customer mapping.
- **Provider idempotency keys from request input:** derive from Billing ids (`bsub:<id>:…`, `seatpur:<purchase_id>`, …; §9.2).
- **sysPool in a tenant handler** (`BIL-RLS-3`): constructor injection keeps it out; don't pass it in.
- **Hand-editing generated files:** `docs/swagger/openapi.json`, `eventbus/schemas/*.json`, `docs/observability/metric-registry.md`.
- **New metric without the inventory:** run `make metrics-inventory`.
- **Turning on a feature flag in code or default values.**

## Observability

Logs (`gincommon` logger), metrics (`internal/adapters/metrics`, on gincommon's registry) and spans (`app.SetTracer`, `httpx`) only through platform-gincommon (`BIL-OPS-10`). No PII or card data in logs; hash identifiers with `LOG_HASH_SALT` where the LLD says (§11.4).
