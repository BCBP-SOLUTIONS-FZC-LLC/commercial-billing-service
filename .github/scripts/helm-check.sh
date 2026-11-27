#!/usr/bin/env bash
# Helm chart check (LLD §13.1, §13.3 "helm-check"):
#   1. helm lint --strict and helm template + kubeconform for base, dev,
#      staging and prod values (namespace billing);
#   2. promtool on the §11.5 alert rules and the vendored platform-audit rules;
#   3. the chart contract tests (test/helm, build tag helm): names and
#      namespace, XFCC annotation (BIL-AUTH-3), OPERATOR_PATH_LIVE=false
#      (BIL-AUTH-6), ten CronJobs = cmd/jobs, operator routes B-O1..B-O17,
#      NetworkPolicy (incl. Entitlements egress), HPA/PDB/migrate, alert →
#      runbook links.
# CRD kinds (HTTPRoute, SecurityPolicy, BackendTrafficPolicy,
# AuthorizationPolicy, ServiceMonitor, PrometheusRule, ExternalSecret) have no
# upstream schema; kubeconform skips them and the contract tests cover them.
set -euo pipefail
cd "$(dirname "$0")/../.."

CHART=deploy/helm
command -v helm >/dev/null || { echo "helm is required (v3.16+)"; exit 2; }
command -v kubeconform >/dev/null || go install github.com/yannh/kubeconform/cmd/kubeconform@v0.6.7
command -v promtool >/dev/null || { echo "promtool is required"; exit 2; }

fail=0
for vf in "" values-dev.yaml values-staging.yaml values-prod.yaml; do
  args=(--namespace billing)
  [[ -n "$vf" ]] && args+=(-f "$CHART/$vf")
  echo "── ${vf:-base}"
  helm lint --strict "$CHART" "${args[@]}" >/dev/null || { fail=1; helm lint --strict "$CHART" "${args[@]}"; }
  helm template commercial-billing "$CHART" "${args[@]}" |
    kubeconform -strict -kubernetes-version 1.31.0 -ignore-missing-schemas -summary || fail=1
done

promtool check rules deploy/monitoring/app-alerts.yml deploy/monitoring/recording-rules.yml deploy/monitoring/slo-rules.yml \
  deploy/monitoring/platform-audit.rules.yml deploy/monitoring/schema-registry-alerts.yml || fail=1
cmp -s deploy/monitoring/dashboards/commercial-billing.json "$CHART/files/dashboard-commercial-billing.json" \
  || { echo "$CHART/files/dashboard-commercial-billing.json differs from the dashboard source"; fail=1; }
for f in app-alerts.yml recording-rules.yml slo-rules.yml; do
  cmp -s "deploy/monitoring/$f" "$CHART/files/$f" || { echo "$CHART/files/$f differs from deploy/monitoring/$f"; fail=1; }
done
go test -tags=helm -count=1 ./test/helm/ || fail=1

if [[ $fail -ne 0 ]]; then echo "helm-check FAILED"; exit 1; fi
echo "helm-check passed"
