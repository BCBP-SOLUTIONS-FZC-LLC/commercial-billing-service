#!/usr/bin/env bash
# Coverage gate (validate-test.yml): statement coverage over internal/ and
# pkg/ — the IAM services' gate scope; cmd/* is start-up wiring, partly
# reachable only in a separate process — from coverage.out (make cover-all).
# Threshold COVERAGE_THRESHOLD, default 96. Emits pct (internal + pkg) and
# pct_all (every package) for the PR summary.
set -euo pipefail

THRESHOLD="${COVERAGE_THRESHOLD:-96}"

test -f coverage.out || {
  echo "::error file=coverage.out,title=Coverage gate::coverage.out missing — run 'make cover-all' before the gate"
  exit 1
}

grep -v '/cmd/' coverage.out > .cover-gate.out
pct=$(go tool cover -func=.cover-gate.out | awk '/^total:/ {sub(/%/,"",$NF); print $NF}')
pct_all=$(go tool cover -func=coverage.out | awk '/^total:/ {sub(/%/,"",$NF); print $NF}')
rm -f .cover-gate.out

echo "::group::Coverage"
echo "internal/ + pkg/: ${pct}%   every package: ${pct_all}%   (threshold ${THRESHOLD}%)"
echo "::endgroup::"
if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "pct=${pct}" >> "$GITHUB_OUTPUT"
  echo "pct_all=${pct_all}" >> "$GITHUB_OUTPUT"
fi
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  echo "### Coverage: ${pct}% (internal/ + pkg/), ${pct_all}% (every package) — threshold ${THRESHOLD}%" >> "$GITHUB_STEP_SUMMARY"
fi

if awk -v p="$pct" -v t="$THRESHOLD" 'BEGIN { exit (p+0 < t+0) ? 0 : 1 }'; then
  echo "::error file=coverage.out,title=Coverage gate::Coverage is ${pct}% — below ${THRESHOLD}%. Run 'make cover-all && make cover-func' to find the gaps."
  exit 1
fi
echo "Coverage gate passed."
