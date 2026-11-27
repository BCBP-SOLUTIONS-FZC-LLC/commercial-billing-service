# Versioning and releases

This repository is a **deployed Go microservice**, not a library other services `go get`. `pkg/requestctx` is an internal helper, and the `go.mod` module path exists so this repository compiles, not for import by sibling repos.

**What gets versioned:**
- the **container image** `ghcr.io/bcbp-solutions-fzc-llc/commercial-billing`;
- the **Helm chart** (`deploy/helm/`) that deploys it;
- the runtime contract that the tenant admin UI, the Signup BFF, IAM Core, the Realm Provisioner, Entitlements, Audit Log, Notification, AuthZ Enrichment, the payment provider and operators depend on.

Versions are published as **Git tags** and described in [CHANGELOG.md](./CHANGELOG.md).

**Billing versions one more thing than the IAM services: the payment-provider adapter.** Its version (`AdapterVersion()`) decides whether a provider may take new accounts (the certification gate, `BIL-PROV-1`). It moves independently of the service version; see [Provider adapter version](#provider-adapter-version).

The SemVer, image-tag and maintainer-process layout follows `iam-org-membership/VERSIONING.md`, itself modelled on `iam-realm-provisioner`'s and `iam-authz-enrichment`'s. Where Billing's pipeline does not exist yet, this document says so instead of describing it.

## Semantic versioning (SemVer)

We use [SemVer 2.0.0](https://semver.org/): `MAJOR.MINOR.PATCH`, for example `v1.2.3`.

| Bump | When you change | Examples |
|------|-----------------|----------|
| **MAJOR** | Breaking change in the runtime contract (below) | Removing or renaming a route or its ID, a produced event `type` or a payload field, a consumed queue's `processed_events` consumer name, a required env var, an `errorCode`, the B-I1 `Idempotency-Key` format, a required permission's name, or an incompatible `values.yaml` restructure |
| **MINOR** | New backward-compatible capability | A new route (new ID — never reuse one), a new optional env var or Helm value, a new produced or consumed event type, an additive payload field (schemas are open), a new `commercial_billing_*` metric, a new provider adapter, a new feature flag (default off) |
| **PATCH** | Backward-compatible fix | Bug fix, performance improvement, a dependency bump with no observable change, a documentation-only correction |

### What counts as the runtime contract

Billing has no Go package for another service to import. Its "public API" is the wire and deployment contract. The authoritative definitions are the LLD sections listed in the right-hand cells and the generated documents:
- [`docs/swagger/openapi.json`](docs/swagger/openapi.json), generated from the route table;
- [`api/asyncapi.yaml`](api/asyncapi.yaml), the source of every event schema.

| In scope (SemVer applies) | Out of scope (may change without MAJOR) |
|---------------------------|------------------------------------------|
| **The 45 endpoint IDs** (LLD §5.3, `internal/adapters/http/routes.go`, `internal/adapters/webhook`): tenant `/api/v1/tenants/:id/*` (B-P1…B-P26), internal B-I1, webhook B-W1, operator `/api/v1/operator/*` (B-O1…B-O17) — method, path, documented request and response field names, idempotency rule, required permission | `internal/*` package structure, exported Go identifiers, file layout; gin route-group internals |
| **Authorization contract:** the six `commercial.*` permission names and the role → permission map (Finance-approved, `BIL-AUTH-7`), the CD-19 headers, the B-I1 caller principal (`BFF_CALLER_PRINCIPAL`), `Idempotency-Key: charge:<tenant_id>` on B-I1, `Idempotent-Replayed` and `Retry-After` semantics | The idempotency record's internal storage (`idempotency_records` columns) |
| **The six produced event types** on `billing-events` and their payload fields (LLD §7.3, `internal/domain/events.go`, `api/asyncapi.yaml`): `TenantPlanChanged`, `TenantSeatsChanged`, `TenantPaymentPastDue`, `TenantSubscriptionCancelled`, `TenantReactivated`, `TenantQuotaAddOnChanged` — plus the envelope fields consumers read (`id`, `type`, `source = commercial-billing`, `subject`, `tenant_id`, `time`) and their semantics (snapshots, `time` = Billing's clock at commit, `billing_ref` idempotency, refund `purchase_ref` is a UUID) | Envelope field order and JSON key order; Glue wire-format header internals; the Glue schema-version UUID (it changes with every schema version by design); which outbox row id an event got |
| **Consumed events and their names:** `tenant-billing-q` (`TenantConverted`, `TenantSuspended`, `TenantReactivated`, `TenantOffboarded`) and `usage-billing-q` (`UsagePeriodClosed`); the inbox consumer names `processed_events.consumer = "tenant-billing"` / `"usage-billing"` (renaming one would re-process every retained message) | Internal dedup mechanics (`inbox.Store`) |
| **Error codes** (`internal/domain/errors.go`, the 71-code §17 catalogue — e.g. `SUBSCRIPTION_ALREADY_EXISTS`, `PAYMENT_ACTION_REQUIRED`, `ENTITLEMENTS_UNAVAILABLE`), their HTTP statuses, the problem type URIs `https://errors.bcbpsolutions.com/commercial/<code-kebab>`, and documented extension members (`billing_subscription_id`, `required_permission`, `decline_reason`, `action`, …). Codes are frozen once published (CD-31) | Exact `title` and `detail` wording; `traceId` / `instance` / `timestamp` values |
| **Required env var names and semantics** (LLD §12, `internal/config/config.go`): the `PG_*` app pool, `PG_SYSTEM_PASSWORD`, `PG_AUDIT_DRAINER_PASSWORD`, `MIGRATION_DATABASE_URL`, `SNS_BILLING_EVENTS_TOPIC_ARN`, `SQS_*_QUEUE_URL`, `STRIPE_API_KEY`, `STRIPE_WEBHOOK_SECRETS`, `CORE_BASE_URL`, `RP_BASE_URL`, `ENTITLEMENTS_BASE_URL`, `TENANT_APP_BASE_URL`; and the **meaning** of every feature flag and gate (`FEATURE_DIRECT_SIGNUP`, `FEATURE_TOKEN_ADDONS`, `FEATURE_SEAT_PURCHASE`, `OPERATOR_PATH_LIVE`, `AUTO_REVERT_ON_EFW`, `AUTO_PAUSE_ON_OPERATOR_SUSPENSION`) | Env var **defaults** and bounds (timeouts, TTLs, poll intervals) — tunable in a MINOR or PATCH unless the new default breaks a documented invariant. `SAGA_ALERT_AFTER` is fixed at 30 min (`BIL-OPS-5`) and `SEATS_AFTER_CONVERSION_DELAY` is relied on by Core; changing either is coordinated, not silent |
| **`deploy/helm/values.yaml` top-level keys** operators set: `image.*`, `serviceAccount.*`, `service.*`, `replicaCount`, `resources`, `jobResources`, `autoscaling.*`, `podDisruptionBudget.*`, `istio.*`, `probes.*`, `env.*`, `secretKeys`, `externalSecret.*`, `existingSecret`, `migrate.*`, `cronjobs.*`, `networkPolicy.*`, `gateway.*`, `dashboard.*`, `serviceMonitor.*` | Chart internals (`_helpers.tpl`, template structure) not exposed as a value |
| **Metric names and labels** Billing owns: Tier 3 `commercial_billing_*` (generated inventory [`docs/observability/metric-registry.md`](docs/observability/metric-registry.md)) — dashboards, alerts, SLO rules and the HPA key off them. Renames after the first deployment follow the Observability Standard's migration (parallel emission → migrate consumers → deprecate → remove), not a silent change | Histogram buckets; Tier 1 `platform_*` names owned by platform-gincommon, platform-events, platform-pgcommon and platform-audit |
| **Alert names** (`deploy/monitoring/app-alerts.yml`) and their `runbook_url`s — on-call routing keys off them | Alert thresholds and `for:` durations (tuning) |
| **Probes:** `GET /healthz` (liveness: platform-audit runner only; stopped loops restart in-process) and `GET /readyz` (both pools) — existence and meaning | `GET /openapi.json`, `/asyncapi`, `/asyncapi.yaml` — documentation surfaces. `METRICS_PORT`'s value is a Helm default |
| **Database roles and the RLS contract** the platform's IaC creates: `billing_migrator`, `billing_app`, `billing_system`, `billing_audit_drainer`, `admin_readonly`, and the `app.tenant_id` GUC | Table and column layout (owned by Billing alone; migrations are two-release safe, LLD §13.4) |

**Retired IDs.** No Billing endpoint or event has been retired yet. When one is, its ID stays permanently unregistered and is never reused: reusing an ID would be a breaking and confusing change even though the ID is technically free.

### Provider adapter version

Each payment-provider adapter reports `AdapterVersion()`. For Stripe this is `adapterSemver + "+stripeapi." + <pinned Stripe API version>`, currently `1.1.0+stripeapi.2026-11-18.endive` (stripe-go v87.2.0, `internal/adapters/provider/stripe/adapter.go`; `1.1.0` since LLD rev 0.60, which needs its own certification before Stripe takes new accounts).

| Change | Adapter version | Service version |
|---|---|---|
| Any change to the adapter's mapping (calls, normalisation, error mapping, §18.8 table) | Bump `adapterSemver` (MAJOR / MINOR / PATCH by the same rules, applied to the provider mapping) | Usually PATCH or MINOR |
| Pinning a new Stripe API version (a stripe-go bump that changes `stripego.APIVersion`) | The `+stripeapi.<date>` suffix changes | PATCH or MINOR |
| Adding a provider | A new adapter with its own version | MINOR |

**Every new adapter version needs a new certification run** before the provider may take new accounts (`BIL-PROV-2`):
1. `certify run` produces the run;
2. `certify submit` records it (B-O14, `pending_review`);
3. a second PCI owner group member approves it (`certify review`, B-O17, `BIL-PROV-4`).

Until a `passed` record exists for that adapter version and suite (`PROVIDER_CERT_SUITE_VERSION`, currently `cert-1.0`):
- the registry keeps the provider out of new accounts;
- running subscriptions continue;
- `BillingProviderNotCertified` fires.

The release gate in LLD §14.6 makes this explicit: a release that changes an adapter version fails until `certify check <provider> <adapter_version>` finds the record in staging. `release.yml`'s `certify-gate` job enforces it (see below).

### Guarantees

- **Pre-`v1.0.0` (current status):** per [SemVer §4](https://semver.org/#spec-item-4), anything may change while the major version is `0`. Billing has never been deployed and has no Git tag. The table above still ranks how disruptive each change is, but callers should not assume `v0.x` compatibility across a MINOR bump.
- **Coordination today:** within `v0.x`, a change to a contract another team consumes still follows the cross-team process, even without a MAJOR bump. That covers an event payload, B-I1 or the Entitlements header contract. Record it in [`docs/lld/billing-open-items.md`](docs/lld/billing-open-items.md), and keep the contract tests green (`test/contract`, `contract-live.yml`).
- **MAJOR (once `v1` ships):** no breaking changes to the runtime contract within `v1.x`. A breaking change ships as `v2.0.0` with migration notes in the CHANGELOG.
- **MINOR:** safe to redeploy without changing caller URLs, permissions, event filters or Helm values, unless you opt into a new capability. New feature flags default off.
- **PATCH:** a drop-in image replacement.

## Supported releases

| Version | Status | Image tag | Notes |
|---------|--------|-----------|-------|
| *(none tagged)* | **Unreleased** | — | [CHANGELOG.md](CHANGELOG.md) is entirely `[Unreleased]`. Helm chart `version` and `appVersion` are `0.7.0` (bumped per implementation phase during the build; not a published release). No deployment, so no support window has started |
| `< v0.1.0` | — | — | No tagged Git releases |

**Billing is not yet releasable, even once `make ci` is green.** The blockers, all recorded in [`CHANGELOG.md`](CHANGELOG.md) (Outstanding) and [`docs/lld/billing-open-items.md`](docs/lld/billing-open-items.md):

1. **The code is not on `main`.** Phases 0–8 sit on the stacked `feat/billing-phase-*` branches; `main` holds only the LLD.
2. **The release pipeline has never run.** `release.yml` exists (maintainer process below), but its gates need configuration that doesn't exist yet: `CERTIFY_CHECK_DSN`, AWS credentials for Glue, `KUBECONFIG_B64`, `PROMETHEUS_URL`, and a `production` environment that requires PCI owner group reviewers.
3. **The operator path is not live** (IAM OP-G2 / OP-G3). Operator routes answer `403` outside dev and test, and `OPERATOR_PATH_LIVE` must stay `false` until IAM confirms the path.
4. **No certified provider exists in any environment.** The first Stripe certification (B-O14 → B-O17), now for adapter `1.1.0`, needs the operator path. Until then, no account can be created on Stripe.
5. **The platform infrastructure isn't provisioned:** the IaC topology (SNS topic, queues and DLQs, Glue registry with the six schemas, database roles), Istio strict mTLS, and sidecar injection on `bff` for B-I1.
6. **There's no production review yet.** The PCI owner group review (HLD §15.4) and the `production-data-migrations` approval are needed for the first production deploy.

No support-window policy exists yet. Once `v1.0.0` runs in production, this section will define how long a superseded major line receives security-only fixes. Expect the platform convention: security fixes only, for a period the platform team sets, typically about 6 months after the next major.

## Consume a release

This service is **not** consumed with `go get`; don't add this module as a dependency. It is consumed as:
- a **container image** on the mesh: tenant routes through the gateway, B-I1 for the Signup BFF only, B-W1 for the provider, operator routes through the operator gateway;
- the **Helm chart** in `deploy/helm/`;
- **events** on `billing-events`.

### Pull and verify the image

Once a tag exists and the release pipeline publishes it:

```bash
docker pull ghcr.io/bcbp-solutions-fzc-llc/commercial-billing:v0.1.0
```

**Today:**
- every merge to `main` pushes `ghcr.io/bcbp-solutions-fzc-llc/commercial-billing:main` and `:sha-<short>`, signed with Cosign (`ci.yml` push job);
- tags publish semver images (`release.yml`).

No merge to `main` has happened yet, so no image exists. Build locally:

```bash
docker build -t commercial-billing:local \
  --secret id=go_private_token,src=<(echo "$GO_PRIVATE_TOKEN") \
  --build-arg BUILD_VERSION=$(git describe --tags --always --dirty) .
```

Images are signed keylessly with Sigstore / Cosign. Verify before deploying:

```bash
# main-branch builds (ci.yml push job)
cosign verify \
  --certificate-identity "https://github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/.github/workflows/ci.yml@refs/heads/main" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/bcbp-solutions-fzc-llc/commercial-billing@<digest>

# tagged releases (release.yml; a manual dispatch signs from main)
cosign verify \
  --certificate-identity-regexp "^https://github\.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/\.github/workflows/release\.yml@refs/(tags/v[0-9][^@]*|heads/main)$" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/bcbp-solutions-fzc-llc/commercial-billing@<digest>
```

The binaries carry their version from the build: `-X main.buildVersion=$(git describe --tags --always --dirty)`. It is reported as `version` in the startup log and as the build-info metric.

### Image tag scheme

The scheme is the same as the IAM services', produced by `docker/metadata-action` per tag push:

| Tag pattern | Produced for | Example (from `v1.2.3`) |
|-------------|--------------|--------------------------|
| `vMAJOR.MINOR.PATCH` | Every release | `v1.2.3` |
| `vMAJOR.MINOR` | Every release | `v1.2` |
| `vMAJOR` | Every release | `v1` |
| `latest` | Stable releases only (no pre-release suffix) | `latest` |

Merges to `main` are to push git-SHA tags. `values.yaml` notes that CI sets `image.tag` to the git SHA, falling back to `appVersion`. Pin production by a **release tag or digest**, never `latest` or an untagged `main` build.

| Pin style | Use when |
|-----------|----------|
| `vMAJOR.MINOR.PATCH` or digest | Production (recommended; enables signature verification) |
| `vMAJOR.MINOR` | Accept PATCH updates automatically |
| `vMAJOR` | Accept MINOR / PATCH automatically — not before `v1.0.0` |
| `latest` / untagged `main` | Local and dev experiments only |

### Deploy via Helm

```bash
helm upgrade commercial-billing ./deploy/helm \
  --install \
  --namespace billing \
  -f deploy/helm/values-staging.yaml \
  --set image.tag=v0.1.0
```

**Versions:** `image.tag` falls back to `Chart.yaml`'s `appVersion` when unset. Chart `version` and `appVersion` are bumped by hand with the Git tag; nothing derives them from the tag.

**What the chart deploys, all from the same image:**
- **one Deployment**: the server, with every route group, the tenant-billing-q consumer (and usage-billing-q when `USAGE_BILLING_CONSUMER_ENABLED=true`), the outbox and audit runners, and the background workers;
- **ten CronJobs**: `/jobs --job=<name>`;
- **the migrate hook Job**: `/migrate`, pre-install / pre-upgrade, which runs before new replicas.

**Required Helm settings:**
- the namespace must be `billing`, and the ServiceAccount and pod label `commercial-billing` (`BIL-OPS-4`, asserted by `make helm-check`);
- keep `env.OPERATOR_PATH_LIVE: "false"` and the feature flags as decided per environment;
- staging and prod need the account values the chart's render guards check (IRSA role, SNS topic, tenant queue, `networkPolicy.postgresCIDRs`). `release.yml` supplies them for production only, from the `production` environment's `BILLING_*` variables; optional ones are `BILLING_USAGE_BILLING_QUEUE_URL`, `BILLING_WEBHOOK_SOURCE_CIDRS`, `BILLING_AWS_ENDPOINT_CIDRS` (`networkPolicy.awsEndpointCIDRs`, VPC interface endpoints) and `BILLING_MONITORING_CIDRS` (`metrics.monitoringCIDRs`). A staging deploy must pass them itself;
- the `production` environment must have deployment-branch rules allowing only tags `v*` and the `main` branch (Settings → Environments → Deployment branches and tags), so a `workflow_dispatch` from another branch cannot reach its secrets.

The remaining go-live checks are LLD §20.10.

**First deployment:** follow LLD §19.1, checked with `make check-first-deploy ENV=<env>`. Glue schemas must be registered before the image starts.

## Maintainer release process

`.github/workflows/release.yml` automates steps 2, 4, 6 (verification) and 7–8; the others are by hand. It has not run yet (see the blockers above).

1. **Merge to `main`.** The phase branches merge first, then all changes for the release. Check the [blockers](#supported-releases) still open.
2. **Update `CHANGELOG.md`.** Move `[Unreleased]` entries into a new `## [X.Y.Z] - YYYY-MM-DD` section. Keep the Outstanding list current. `release.yml` fails without it (`verify-changelog-entry.sh`).
3. **Bump `deploy/helm/Chart.yaml` `version` and `appVersion`** to `X.Y.Z`. For the first tag, move from the phase-numbered `0.7.0` to the release version. A consumer who deploys without setting `image.tag` gets whatever `appVersion` was last committed, so a forgotten bump ships a stale image.
4. **Check the provider certification.** If the release changes `AdapterVersion()` (an adapter change or a Stripe API pin), make sure a `passed` record exists in staging:

   ```bash
   CERTIFY_CHECK_DSN=… go run ./cmd/certify check stripe "$(go run ./cmd/certify adapter-version)"
   ```

   `release.yml`'s `certify-gate` job runs exactly this check and fails the release without a passed record.
5. **Run the full gate locally:**

   ```bash
   make ci                  # fmt-check + vet + lint + arch-lint + guards + test (race) + build
   make cover-all && make cover-gate   # every suite, coverage ≥ 96% (as validate-test.yml)
   make helm-check
   make schema-validate
   ```

   Or run `/ci-local`, which runs all of the above and the live Entitlements contract check.
6. **Register the event schemas.** Any new or changed produced schema must be registered in Glue `billing-events` in every target environment **before** the image rolls out. The server resolves schema versions by definition at startup and refuses to start ahead of the registry.
7. **Tag and push:**

   ```bash
   git tag -a v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```

   The tag triggers `release.yml`:
   - **validate:** `validate-test` and `validate-quality` re-run at the tagged commit, integration suites and coverage gate included;
   - **build:** the tag must equal HEAD, the CHANGELOG entry must exist, and Linux amd64 / arm64 binaries are built with checksums;
   - **certify-gate:** the Stripe adapter version must be certified in staging;
   - **schema-verify:** every produced schema must be registered and `AVAILABLE` in production Glue;
   - **docker:** semver tags; Trivy fails on CRITICAL / HIGH; CycloneDX SBOM; SLSA provenance; Cosign sign and verify;
   - **deploy-gate:** the `production` environment waits for its reviewers, then a Helm deploy pinned by digest checks the deployed digest and rollout, holds the 5xx rate under 1% for 2 minutes, and runs `helm rollback` on failure;
   - **publish:** a GitHub Release with the CHANGELOG section, binaries, checksums, SBOM and provenance.

   Only a release that passed the deploy gate is published.
8. **Production approval:** a reviewer from the PCI owner group (HLD §15.4), plus `production-data-migrations` approval for any migration.
9. **Notify consumers** with upgrade notes on MINOR and MAJOR:
   - Core / Org & Membership (`billing-orgm-q`, I-11 and I-18 caller);
   - Realm Provisioner (RP-3, RP-4, RP-11, `billing-realm-q`);
   - Entitlements (eligibility `GET`, `billing-entitlements-q`, the contract test);
   - the Signup BFF (B-I1);
   - Audit Log, Notification and AuthZ Enrichment (their queues);
   - the tenant admin UI;
   - whoever owns the Helm deployment.

   Coordinate first anything that touches an event type or payload, B-I1, a permission name, the inbox consumer names or a feature flag's per-environment value. Those have the widest reach.

### Pre-release tags (optional)

| Tag pattern | Meaning |
|-------------|---------|
| `v1.1.0-rc.1` | Release candidate; not for production unless approved |
| `v1.1.0-beta.1` | Early integration testing (for example, the staging Test-Clock E2E with the Signup BFF) |

Both are to produce a signed, scanned image, never a `latest` tag.

## Compatibility matrix

| commercial-billing | Go (`go.mod`) | Shared platform libraries | Provider | `platform-schemagov` |
|---|---|---|---|---|
| Unreleased (phase branches) | `1.26.11` | `platform-gincommon/v2` v2.7.0 (first release with the `commercial` domain), `platform-pgcommon/v2` v2.0.2, `platform-events/v2` v2.3.0, `platform-audit` v0.1.2 | `stripe-go/v87` v87.2.0, Stripe API `2026-11-18.endive`, adapter `1.1.0+stripeapi.2026-11-18.endive`, suite `cert-1.0` | `0.4` (`SCHEMA_GOV_IMAGE`, CI installs v0.4.0) |

**Runtime infrastructure:**
- PostgreSQL 17, with PgBouncer in transaction mode;
- AWS SNS, SQS and the Glue Schema Registry;
- Kubernetes ≥ 1.30 (`Chart.yaml` `kubeVersion`; the native `preStop` sleep action);
- Istio ≥ 1.24 (native sidecars), with `PeerAuthentication` `STRICT` on Billing's workloads; the sidecar's default `APPEND_FORWARD` puts the verified client certificate last in `X-Forwarded-Client-Cert` (B-I1).

None of the libraries is re-exported. Callers depend on the HTTP and event contract, never on a Go package here.

**Library upgrades:**
- **stripe-go:** a bump that changes the pinned API version is an adapter-version change (see above) and needs a new certification.
- **platform-gincommon, pgcommon, events, audit:** take the latest platform release, and check its CHANGELOG for registry, schema or label changes. A gincommon registry change can turn `make metrics-lint` red.

## Related files

| File | Purpose |
|------|---------|
| [CHANGELOG.md](./CHANGELOG.md) | History per version — today entirely `[Unreleased]`, with the Outstanding list |
| [README.md](./README.md) | Mental model, API and event overview, local dev, CI summary |
| [CONTRIBUTING.md](./CONTRIBUTING.md#deployment) | Development process and the deployment pipeline as it exists today |
| [ARCHITECTURE.md](./ARCHITECTURE.md) | Layer model, flows, failure domains, key invariants, threat model |
| [docs/lld/commercial-lld-billing-service.md](./docs/lld/commercial-lld-billing-service.md) | The runtime contract in full — §5 API, §7 events, §12 configuration, §13 deployment, §14.6–14.7 gates and certification, §17 errors, §19 first deployment and providers |
| [docs/lld/billing-open-items.md](./docs/lld/billing-open-items.md) | Cross-team items that block a release |
| [api/asyncapi.yaml](./api/asyncapi.yaml) · [docs/swagger/openapi.json](./docs/swagger/openapi.json) | Event and REST contracts |
| [.github/workflows/](./.github/workflows/) | `ci.yml` (validate, image, Trivy, smoke, push), `release.yml` (certification gate, signed release, deploy gate), `schema-registry.yml`, `changelog-check.yml`, `contract-live.yml` |
| [deploy/helm/Chart.yaml](./deploy/helm/Chart.yaml) | Chart version and app version |
| [internal/adapters/provider/stripe/adapter.go](./internal/adapters/provider/stripe/adapter.go) | `AdapterVersion` — the certified provider version |
| [go.mod](./go.mod) | Module path and Go version — not a consumable package |
