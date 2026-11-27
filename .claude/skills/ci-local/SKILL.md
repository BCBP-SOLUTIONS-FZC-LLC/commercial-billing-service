---
name: ci-local
description: Run this repo's full CI gate locally (validate-test, validate-quality, image build, smoke and contract jobs) and report one pass/fail table. Use before pushing or opening a PR.
disable-model-invocation: true
argument-hint: "[quick]  (quick = skip the coverage suites, helm-check, schema-gov and the image)"
---

# /ci-local — commercial-billing CI gate, locally

Reproduce what `.github/workflows/ci.yml` checks on a PR, so failures show up before the push:
- the reusable `validate-test.yml` and `validate-quality.yml`;
- `build-image` and `smoke`;
- `contract-live.yml`.

Trivy and the push to GHCR are CI-only.

Run every step even when an earlier one fails, then report all results together.
Do **not** fix anything unless the user asks; this skill reports.

If `$ARGUMENTS` contains `quick`, skip steps 4, 5, 6 and 7 and say so in the report.

## 1. Modules

```bash
go mod download && go mod verify
make tidy && git diff --exit-code go.mod go.sum   # tidy must be a no-op
```

If a private `BCBP-SOLUTIONS-FZC-LLC` module can't be fetched, stop and tell the user
(SSH key or `GOPRIVATE`); every later step needs them.

## 2. validate-quality / validate-test (fast steps)

```bash
make fmt-check
make vet
make lint          # go tool golangci-lint: the version pinned in go.mod, so no local install needed
make arch-lint     # installs go-arch-lint v1.15.0 if missing
make guards        # BIL-PCI-1, BIL-OPS-2, RFC 9457 problem details, BIL-OPS-11 pgcommon
make metrics-lint  # observability standard
make openapi-check
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

## 4. Schema sync (validate-test, schema-gov)

Locally through Docker. CI instead builds schema-gov v0.4.0 from a pinned commit with hash-locked Python dependencies (`.github/schema-gov-requirements.txt`, `pip --require-hashes`), without credentials:

```bash
make schema-validate     # = schema-check (embedded schemas in sync with api/asyncapi.yaml) + validate
```

If Docker or the `platform-schemagov` image isn't available, mark SKIPPED.

## 5. Helm (validate-quality)

```bash
make helm-check
```

Needs `helm` (v3.19+; CI pins v3.21.4) and `promtool`. If either is missing, mark SKIPPED, not failed.

## 6. Image (build-image, smoke)

```bash
command -v hadolint >/dev/null && hadolint Dockerfile || docker run --rm -i hadolint/hadolint < Dockerfile
make docker-build && IMAGE=commercial-billing:local GITHUB_STEP_SUMMARY=/dev/null bash .github/scripts/smoke-tests.sh
```

SKIPPED without Docker. The image build needs `GO_PRIVATE_TOKEN` or SSH access to the private modules.

## 7. Every suite with the coverage gate (validate-test)

```bash
docker ps >/dev/null 2>&1 && make cover-all INT_PARALLEL=2 && bash .github/scripts/coverage-gate.sh
```

SKIPPED without Docker. `cover-all` runs every suite (`./...` with tag `integration`: unit, `test/postgres`, `test/flows`, `test/e2e`, `test/scenarios/*`, bus, stripe-mock, certify). The gate is ≥ 96% over `internal/` + `pkg/` (last measured 98.4%). Report the failing test names, or the coverage figure if it is below the gate.

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
