## Description

What changes and why. Name the LLD sections implemented or changed.

**LLD:** §… · **Revision:** rev … (or "no design change")
**Invariants touched:** `BIL-…`

---

## Type of change
- [ ] Bug fix
- [ ] New feature (in the LLD)
- [ ] Refactor (no behaviour change)
- [ ] Documentation / LLD only
- [ ] Tests only
- [ ] New migration
- [ ] Event contract change (`api/asyncapi.yaml`, embedded schemas)
- [ ] Provider adapter change (needs a new certification run — `BIL-PROV-2`)
- [ ] Breaking change (route, event, error code, permission, env var, Helm key — VERSIONING.md)

---

## Testing
- [ ] `make ci` passes (fmt-check, vet, lint, arch-lint, guards, race tests, build)
- [ ] `make test-integration` passes when Postgres, flows, the bus, the provider or certification changed
- [ ] `make cover-all && make cover-gate` — coverage ≥ 96% over internal/ + pkg/
- [ ] `make helm-check` when `deploy/` changed or a route, CronJob, alert or setting was added
- [ ] Every touched invariant has a test whose name contains its ID, and a row in `docs/invariants-coverage.md`

---

## Money, PCI and correctness
- [ ] No card data (PAN, CVV, expiry), provider key or webhook secret in a table, log, event, span, error or response (`BIL-PCI-*`)
- [ ] Money is `domain.Money` (int64 minor units) — no `float64`; proration rounds once (`BIL-SEAT-5`)
- [ ] Writes to a subscription, purchase, schedule or saga take the tenant lock (`BIL-CONC-1`)
- [ ] No provider, IAM or Entitlements HTTP call inside a transaction (`BIL-CONC-2`)
- [ ] State change, event and audit entry commit in one transaction through the outbox (`BIL-EVT-2`)
- [ ] Provider idempotency keys derive from Billing ids only (`BIL-IDEM-2`)
- [ ] Feature flags and the operator `403` gate untouched (`FEATURE_*`, `OPERATOR_PATH_LIVE`)

## Contracts
- [ ] Route changes: `routes.go` + OpenAPI golden regenerated (`make openapi`) + README API tables
- [ ] New error code: `internal/domain/errors.go` + LLD §17
- [ ] Event change: `api/asyncapi.yaml` → `make schema-extract`; payload `Validate()`; `test/contract` sample; additive only (schemas are open)
- [ ] Schema rollout order: register in Glue (schema-registry.yml) before the image that produces it is deployed
- [ ] Consumers told (`docs/lld/billing-open-items.md`) when a contract they read changes

## Database
- [ ] Migration has `.up.sql` and `.down.sql`, numbered after the last one; existing migrations unedited
- [ ] New tenant table: `ENABLE` + `FORCE ROW LEVEL SECURITY` + `tenant_isolation` (`TestBIL_RLS_1_EveryTenantTableForcesRLS`)
- [ ] Two-release safe (LLD §13.4): additive now, destructive later; `CONCURRENTLY` indexes; `NOT VALID` then `VALIDATE`

## Documentation
- [ ] `CHANGELOG.md` `[Unreleased]` — dated entry naming the LLD revision
- [ ] README / ARCHITECTURE / CONTRIBUTING / `.claude` updated where they describe what changed

---

## Production
- [ ] Reviewer from the PCI owner group requested (HLD §15.4) if this reaches production
- [ ] Deployment notes below: migrations, new required env vars, schema registration, flag changes per environment

## Deployment notes

## Related issue
Closes #
