#!/usr/bin/env bash
# Shared helpers for the Stripe Test Clock rehearsal (LLD §14.4, rev 0.36:
# go-live evidence). Sourced by run.sh. Staging only: refuses a live key.
#
# Required environment:
#   STRIPE_TEST_KEY   rk_test_… / sk_test_… key of the staging Stripe account
#   BILLING_URL       gateway base URL, e.g. https://api.staging.bcbpsolutions.com
#   TOKEN_CMD         command printing a bearer token: "$TOKEN_CMD <role> <tenant_id>"
#                     (role: operator | owner); staging's test identity tooling
#   PSQL_URL          staging DSN of an operator role allowed to run the
#                     §19.3 stub → Stripe switch (change-ticket SQL)
#   NEW_TENANT_CMD    command printing the id of a new trial tenant in IAM staging
#                     (B-O1 converts it, so it must exist in IAM)
#   EVIDENCE_DIR      where run records are written (default ./evidence)
# Optional hooks (scenarios that need IAM or a browser):
#   SUSPEND_CMD       "$SUSPEND_CMD <tenant_id>" suspends the tenant (IAM operator API)
#   REACTIVATE_CMD    "$REACTIVATE_CMD <tenant_id>" reactivates it
set -euo pipefail

STRIPE_API=${STRIPE_API:-https://api.stripe.com}
EVIDENCE_DIR=${EVIDENCE_DIR:-./evidence}
RUN_ID=${RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)}

die() { echo "FAIL: $*" >&2; exit 1; }
log() { echo "[$(date -u +%H:%M:%S)] $*" >&2; }

require_env() {
  local v
  for v in STRIPE_TEST_KEY BILLING_URL TOKEN_CMD PSQL_URL NEW_TENANT_CMD; do
    [[ -n "${!v:-}" ]] || die "$v is not set (see lib.sh header)"
  done
  case "$STRIPE_TEST_KEY" in
    rk_test_*|sk_test_*) ;;
    *) die "STRIPE_TEST_KEY must be a test-mode key: Test Clocks never run against live mode (BIL-OPS-7)" ;;
  esac
  command -v jq >/dev/null || die "jq is required"
  command -v psql >/dev/null || die "psql is required"
  mkdir -p "$EVIDENCE_DIR"
}

# stripe METHOD PATH [form args…] — form-encoded Stripe API call; prints JSON.
stripe() {
  local method=$1 path=$2; shift 2
  local args=()
  local kv
  [[ "$method" == GET ]] && args+=(-G)
  for kv in "$@"; do args+=(--data-urlencode "$kv"); done
  curl -sS -X "$method" "$STRIPE_API$path" -u "$STRIPE_TEST_KEY:" \
    -H "Idempotency-Key: tc-$RUN_ID-$(echo "$method $path $*" | shasum | cut -c1-16)" "${args[@]}"
}

# billing METHOD PATH ROLE TENANT [JSON body] — Billing API through the gateway.
billing() {
  local method=$1 path=$2 role=$3 tenant=$4 body=${5:-}
  local token; token=$($TOKEN_CMD "$role" "$tenant")
  local args=(-sS -X "$method" "$BILLING_URL$path" -H "Authorization: Bearer $token"
    -H "Idempotency-Key: tc-$RUN_ID-$(echo "$method $path $body" | shasum | cut -c1-24)")
  if [[ -n "$body" ]]; then args+=(-H "Content-Type: application/json" -d "$body"); fi
  curl "${args[@]}"
}

sql() { psql "$PSQL_URL" -v ON_ERROR_STOP=1 -qtAX -c "$1"; }

# new_clock NAME START_EPOCH — creates a Test Clock; prints its id.
new_clock() { stripe POST /v1/test_helpers/test_clocks "name=$1" "frozen_time=$2" | jq -r .id; }

# advance CLOCK EPOCH — advances and waits until the clock is ready.
advance() {
  local clock=$1 to=$2 status
  stripe POST "/v1/test_helpers/test_clocks/$clock/advance" "frozen_time=$to" >/dev/null
  for _ in $(seq 1 120); do
    status=$(stripe GET "/v1/test_helpers/test_clocks/$clock" | jq -r .status)
    [[ "$status" == ready ]] && return 0
    [[ "$status" == internal_failure ]] && die "clock $clock failed to advance"
    sleep 5
  done
  die "clock $clock still advancing after 10 minutes"
}

# wait_for DESCRIPTION SQL EXPECTED — polls the staging DB until the query
# returns EXPECTED (webhooks and the processor take seconds).
wait_for() {
  local what=$1 q=$2 want=$3 got=""
  for _ in $(seq 1 60); do
    got=$(sql "$q" || true)
    [[ "$got" == "$want" ]] && { log "ok: $what = $want"; return 0; }
    sleep 5
  done
  die "$what: got '$got', want '$want'"
}

# stub_tenant PLAN INTERVAL SEATS — a new staging tenant with a manual stub
# subscription (B-O1); prints "tenant_id billing_subscription_id".
stub_tenant() {
  local plan=$1 interval=$2 seats=$3 tenant out
  tenant=$($NEW_TENANT_CMD)
  out=$(billing POST "/api/v1/operator/tenants/$tenant/conversions" operator "$tenant" \
    "{\"plan\":\"$plan\",\"interval\":\"$interval\",\"licensed_seats\":$seats,\"reason\":\"Test Clock rehearsal $RUN_ID\"}")
  local sub; sub=$(echo "$out" | jq -r '.billing_subscription_id // empty')
  [[ -n "$sub" ]] || die "B-O1 stub conversion failed: $out"
  wait_for "stub conversion confirmed" "SELECT state FROM conversion_sagas WHERE billing_subscription_id='$sub'" confirmed
  echo "$tenant $sub"
}

# link_to_clock TENANT SUB CLOCK PLAN INTERVAL SEATS [PM] — creates the
# Stripe customer on the clock and its subscription with Billing's metadata,
# then runs the §19.3 one-transaction switch. Prints the Stripe subscription id.
link_to_clock() {
  local tenant=$1 sub=$2 clock=$3 plan=$4 interval=$5 seats=$6 pm=${7:-pm_card_visa}
  local cus price psub
  cus=$(stripe POST /v1/customers "test_clock=$clock" "metadata[tenant_id]=$tenant" \
    "payment_method=$pm" "invoice_settings[default_payment_method]=$pm" | jq -r .id)
  price=$(sql "SELECT provider_price_id FROM plan_prices WHERE provider='stripe' AND plan='$plan' AND interval='$interval'
    AND currency='USD' AND effective_to IS NULL")
  [[ -n "$price" ]] || die "no current stripe price for $plan/$interval"
  # §19.3 step 2: trial_end = the end of the period the stub already covers,
  # so nothing is charged twice and the first charge is the renewal.
  local trial_end; trial_end=$(period_end "$sub")
  psub=$(stripe POST /v1/subscriptions "customer=$cus" "items[0][price]=$price" "items[0][quantity]=$seats" \
    "trial_end=$trial_end" "metadata[tenant_id]=$tenant" "metadata[billing_subscription_id]=$sub" | jq -r .id)
  [[ "$psub" == sub_* ]] || die "stripe subscription not created"
  sql "BEGIN;
    UPDATE billing_accounts SET provider='stripe', provider_customer_id='$cus' WHERE tenant_id='$tenant';
    UPDATE subscriptions SET provider='stripe', provider_subscription_id='$psub' WHERE billing_subscription_id='$sub';
    UPDATE subscription_items SET price_id=(SELECT price_id FROM plan_prices WHERE provider_price_id='$price')
      WHERE billing_subscription_id='$sub';
    COMMIT;" >/dev/null
  echo "$psub"
}

sub_status() { echo "SELECT status FROM subscriptions WHERE billing_subscription_id='$1'"; }

# evidence SCENARIO JSON — appends one run record (clock, objects, final states).
evidence() {
  local file="$EVIDENCE_DIR/testclock-$RUN_ID.jsonl"
  jq -c --arg s "$1" --arg run "$RUN_ID" '. + {scenario: $s, run_id: $run, recorded_at: (now | todate)}' <<<"$2" >>"$file"
  log "evidence: $1 → $file"
}

period_end() { sql "SELECT extract(epoch FROM current_period_end)::bigint FROM subscriptions WHERE billing_subscription_id='$1'"; }
