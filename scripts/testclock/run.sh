#!/usr/bin/env bash
# Stripe Test Clock rehearsal for commercial-billing (LLD §14.4). Phase 7
# go-live evidence (rev 0.36): run in staging, attach evidence/*.jsonl to the
# go-live ticket. Runbook: docs/runbooks/test-clock-rehearsal.md.
#
# Usage: scripts/testclock/run.sh [scenario …]
#   scenarios: renewal_month renewal_year dunning suspension
#              reactivation_past_due downgrade reduction addon_3ds   (default: all)
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
source "$here/lib.sh"
# shellcheck source=scenarios.sh
source "$here/scenarios.sh"

all=(renewal_month renewal_year dunning suspension reactivation_past_due downgrade reduction addon_3ds)
selected=("$@")
[[ ${#selected[@]} -gt 0 ]] || selected=("${all[@]}")

require_env
for s in "${selected[@]}"; do
  log "=== $s"
  case "$s" in
    renewal_month) scenario_renewal month ;;
    renewal_year) scenario_renewal year ;;
    dunning) scenario_dunning ;;
    suspension) scenario_suspension ;;
    reactivation_past_due) scenario_reactivation_past_due ;;
    downgrade) scenario_downgrade ;;
    reduction) scenario_reduction ;;
    addon_3ds) scenario_addon_3ds ;;
    *) die "unknown scenario $s" ;;
  esac
done
log "all selected scenarios passed; evidence in $EVIDENCE_DIR/testclock-$RUN_ID.jsonl"
