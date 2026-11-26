---
name: ci-local
description: Run this repo's full CI gate locally (validate, helm, schema, dockerfile and contract jobs) and report one pass/fail table. Use before pushing or opening a PR.
disable-model-invocation: true
argument-hint: "[quick]  (quick = skip integration tests, helm-check and schema-gov)"
---

# /ci-local — commercial-billing CI gate, locally

Reproduce what `.github/workflows/ci.yml` (jobs `validate`, `helm`, `schema`,
`dockerfile`) and `contract-live.yml` check on a PR, so failures show up before the push.

Run every step even when an earlier one fails, then report all results together.
Do **not** fix anything unless the user asks; this skill reports.

If `$ARGUMENTS` contains `quick`, skip steps 4, 5 and 7 and say so in the report.

## 1. Modules

```bash
go mod download && go mod verify
make tidy && git diff --exit-code go.mod go.sum   # tidy must be a no-op
```

If a private `BCBP-SOLUTIONS-FZC-LLC` module can't be fetched, stop and tell the user
(SSH key or `GOPRIVATE`); every later step needs them.

## 2. Validate job (same order as CI)

```bash
make fmt-check
make vet
make lint          # go tool golangci-lint: the version pinned in go.mod, so no local install needed
make arch-lint     # installs go-arch-lint v1.15.0 if missing
make guards        # BIL-PCI-1, BIL-OPS-2, RFC 9457 problem details
make test          # -race, all packages (unit, contract, OpenAPI golden, metric inventory)
make build
```

If `make test` fails only in `TestOpenAPIGolden` or `TestInventoryUpToDate`, the fix is
regeneration (`go test ./internal/adapters/http -run Golden -update`,
`make metrics-inventory`). Report it that way.

## 3. Generated files are current

```bash
git diff --exit-code -- docs/swagger/ docs/observability/metric-registry.md
```

A diff here, after step 2, means a golden or inventory was regenerated and not committed.

## 4. Schema job (schema-gov)

Locally through Docker (CI installs schema-gov from source):

```bash
make schema-validate     # = schema-check (embedded schemas in sync with api/asyncapi.yaml) + validate
```

If Docker or the `platform-schemagov` image isn't available, mark SKIPPED.

## 5. Helm job

```bash
make helm-check
```

Needs `helm` (v3.16+) and `promtool`. If either is missing, mark SKIPPED, not failed.

## 6. Dockerfile

```bash
command -v hadolint >/dev/null && hadolint Dockerfile || docker run --rm -i hadolint/hadolint < Dockerfile
```

SKIPPED if neither is available.

## 7. Integration tests (not yet in CI; LLD §14.2 requires them per phase)

```bash
docker ps >/dev/null 2>&1 && make test-integration
```

SKIPPED without Docker. Report the failing test names only.

## 8. Contract against Entitlements' main (contract-live.yml)

Only if `gh` is authenticated with access to `commercial-entitlements-service`:

```bash
dir=$(mktemp -d) && cp -R test/contract/testdata/consumers/. "$dir" && \
gh api -H "Accept: application/vnd.github.raw+json" \
  "repos/BCBP-SOLUTIONS-FZC-LLC/commercial-entitlements-service/contents/internal/adapter/outbound/eventbus/schemas/inbound_tenant_quota_add_on_changed.json?ref=main" \
  > "$dir/commercial-entitlements/inbound_tenant_quota_add_on_changed.json" && \
diff -q test/contract/testdata/consumers/commercial-entitlements/inbound_tenant_quota_add_on_changed.json \
        "$dir/commercial-entitlements/inbound_tenant_quota_add_on_changed.json"; \
CONTRACT_CONSUMERS_DIR="$dir" go test ./test/contract/... -count=1
```

A `diff` difference is a WARNING (refresh the vendored copy and its commit in
`test/contract/testdata/consumers/README.md`); a test failure is FAIL. Otherwise SKIPPED.

## Report

One table, then details only for failures:

| Step | Result | Note |
|---|---|---|
| tidy / verify | PASS / FAIL / SKIPPED | … |
| … | | |

For each failure: the command, the relevant 10–20 lines of output (not the full
log), and the likely fix. End with one line: ready to push, or what blocks it.
