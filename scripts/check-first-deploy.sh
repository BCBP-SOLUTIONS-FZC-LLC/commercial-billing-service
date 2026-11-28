#!/usr/bin/env bash
# First-deployment order check (LLD §19.1). Read-only: it changes nothing.
# Each step's preconditions are checked in order, and the script stops at the
# first step that is not complete, so steps are never taken out of order.
#
#   1. Commercial IaC   SNS billing-events, Glue registry billing-events,
#                       tenant-billing-q / usage-billing-q with DLQs and redrive
#   2. Database         billing database, the five roles, migrations applied
#   3. Price book       current stripe plan_prices rows carry provider_price_id
#   4. Stripe           webhook endpoint for this environment with the §7.4 types;
#                       key mode matches the environment (BIL-OPS-7)
#   5. Deploy           Deployment rolled out, /readyz ready, provider certified
#   6. Operator routes  OPERATOR_PATH_LIVE (informational until CD-52 OP-G1..G3)
#   7. Feature flags    seat purchase on; direct signup / token add-ons as decided
#
# Usage: scripts/check-first-deploy.sh <env>        (env: dev | staging | prod)
# Environment:
#   AWS_REGION, AWS credentials        read access to SNS, SQS, Glue
#   BILLING_DB_DSN                     admin_readonly on the billing database
#   STRIPE_API_KEY                     the environment's restricted key (read-only use)
#   STRIPE_FINAL_RETRY_ACTION          "cancel" once the Dashboard setting is checked (step 4)
#   WEBHOOK_URL                        https://<gateway>/api/v1/webhooks/stripe
#   KUBE_CONTEXT                       kubectl context of the environment (optional)
set -euo pipefail

env_name=${1:-}
[[ "$env_name" =~ ^(dev|staging|prod)$ ]] || { echo "usage: $0 dev|staging|prod" >&2; exit 64; }

step=0
pass() { echo "  ok   $*"; }
stop() {
  echo "  FAIL $*" >&2
  if [[ $step -eq 0 ]]; then echo "STOP: prerequisites missing." >&2; else echo "STOP at step $step: complete it before the next step (LLD §19.1)." >&2; fi
  exit 1
}
begin() { step=$1; echo "── step $1: $2"; }
need() { for c in "$@"; do command -v "$c" >/dev/null || stop "$c is required"; done; }

need aws psql curl jq
for v in AWS_REGION BILLING_DB_DSN STRIPE_API_KEY WEBHOOK_URL; do
  [[ -n "${!v:-}" ]] || stop "$v is not set"
done

# ── 1. Commercial IaC ───────────────────────────────────────────────────
begin 1 "Commercial IaC"
topic=$(aws sns list-topics --query "Topics[?ends_with(TopicArn, ':billing-events')].TopicArn | [0]" --output text)
[[ "$topic" == arn:* ]] || stop "SNS topic billing-events not found"
pass "SNS $topic"
aws glue get-registry --registry-id RegistryName=billing-events >/dev/null 2>&1 || stop "Glue registry billing-events not found"
pass "Glue registry billing-events"
for q in tenant-billing-q usage-billing-q; do
  url=$(aws sqs get-queue-url --queue-name "$q" --query QueueUrl --output text 2>/dev/null) || stop "SQS $q not found"
  aws sqs get-queue-url --queue-name "$q-dlq" >/dev/null 2>&1 || stop "SQS $q-dlq not found"
  redrive=$(aws sqs get-queue-attributes --queue-url "$url" --attribute-names RedrivePolicy --query Attributes.RedrivePolicy --output text)
  [[ "$redrive" == *"$q-dlq"* ]] || stop "$q has no redrive policy to $q-dlq"
  pass "SQS $q with $q-dlq"
done

# ── 2. Database ─────────────────────────────────────────────────────────
begin 2 "Database"
sqlq() { psql "$BILLING_DB_DSN" -v ON_ERROR_STOP=1 -qtAX -c "$1"; }
roles=$(sqlq "SELECT count(*) FROM pg_roles WHERE rolname IN ('billing_migrator','billing_app','billing_system','billing_audit_drainer','admin_readonly')") \
  || stop "cannot connect to the billing database"
[[ "$roles" == 5 ]] || stop "expected the five billing roles, found $roles"
pass "roles"
want=$(find "$(dirname "$0")/../internal/adapters/postgres/migrations" -name '*.up.sql' | wc -l | tr -d ' ')
got=$(sqlq "SELECT version || ':' || dirty FROM billing_schema_migrations") || stop "no billing_schema_migrations table: run cmd/migrate"
[[ "$got" == "$want:f" ]] || stop "migrations at $got, want $want:f (clean)"
pass "migrations $want applied"
[[ $(sqlq "SELECT bypassrls::text FROM pg_roles WHERE rolname = 'billing_app'") == false ]] || stop "billing_app must not bypass RLS"
pass "billing_app is RLS-bound"

# ── 3. Price book ───────────────────────────────────────────────────────
begin 3 "Price book"
n=$(sqlq "SELECT count(*) FROM plan_prices WHERE provider = 'stripe' AND effective_to IS NULL AND provider_price_id IS NOT NULL")
[[ "$n" -ge 3 ]] || stop "no current stripe price rows with provider_price_id (run price-book-sync.sh through B-O11)"
missing=$(sqlq "SELECT string_agg(DISTINCT plan::text, ',') FROM (VALUES ('starter'),('pro'),('enterprise')) p(plan)
  WHERE NOT EXISTS (SELECT 1 FROM plan_prices r WHERE r.plan::text = p.plan AND r.provider = 'stripe' AND r.effective_to IS NULL)")
[[ -z "$missing" ]] || stop "no current stripe price for: $missing"
pass "$n current stripe price rows"

# ── 4. Stripe ───────────────────────────────────────────────────────────
begin 4 "Stripe"
case "$env_name:$STRIPE_API_KEY" in
  prod:rk_live_*|dev:rk_test_*|staging:rk_test_*) pass "restricted key mode matches $env_name (BIL-OPS-7)" ;;
  *) stop "the key must be rk_live_ in prod and rk_test_ elsewhere (BIL-OPS-7)" ;;
esac
endpoints=$(curl -sS -u "$STRIPE_API_KEY:" "https://api.stripe.com/v1/webhook_endpoints?limit=100") || stop "cannot list webhook endpoints"
ep=$(jq -c --arg u "$WEBHOOK_URL" '.data[] | select(.url == $u and .status == "enabled")' <<<"$endpoints")
[[ -n "$ep" ]] || stop "no enabled webhook endpoint for $WEBHOOK_URL"
required=(checkout.session.completed checkout.session.expired invoice.paid invoice.payment_failed invoice.payment_action_required
  invoice.updated invoice.created invoice.finalized invoice.voided invoice.marked_uncollectible
  customer.subscription.updated customer.subscription.deleted customer.updated payment_method.attached payment_method.detached
  subscription_schedule.released subscription_schedule.completed subscription_schedule.canceled
  charge.refunded refund.updated refund.failed charge.dispute.created charge.dispute.updated charge.dispute.closed
  radar.early_fraud_warning.created)
enabled=$(jq -r '.enabled_events[]' <<<"$ep")
if ! grep -qx '\*' <<<"$enabled"; then
  for t in "${required[@]}"; do grep -qx "$t" <<<"$enabled" || stop "webhook endpoint lacks $t (§7.4)"; done
fi
pass "webhook endpoint with the §7.4 event types"
# DUNNING_FINAL_ACTION=cancel (§8.8, §12): the provider's final-retry action is a
# Stripe Dashboard setting the API does not expose, so a person confirms it:
# Settings → Billing → Subscriptions → "If all retries for a payment fail" →
# cancel the subscription (rev 0.73, D-L19).
[[ "${STRIPE_FINAL_RETRY_ACTION:-}" == "cancel" ]] || stop "confirm the Stripe final-retry action is 'cancel the subscription', then set STRIPE_FINAL_RETRY_ACTION=cancel (DUNNING_FINAL_ACTION, §8.8)"
pass "final retry cancels the subscription (confirmed, DUNNING_FINAL_ACTION)"

# ── 5. Deploy ───────────────────────────────────────────────────────────
begin 5 "Deploy"
if command -v kubectl >/dev/null; then
  k=(kubectl -n billing); [[ -n "${KUBE_CONTEXT:-}" ]] && k+=(--context "$KUBE_CONTEXT")
  "${k[@]}" rollout status deployment/commercial-billing --timeout=30s >/dev/null || stop "commercial-billing is not rolled out"
  pass "deployment rolled out"
  live=$("${k[@]}" get deployment commercial-billing -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="OPERATOR_PATH_LIVE")].value}')
  flags=$("${k[@]}" get deployment commercial-billing -o json | jq -r '.spec.template.spec.containers[0].env[] | select(.name|startswith("FEATURE_")) | "\(.name)=\(.value)"')
else
  echo "  skip kubectl not available: rollout, OPERATOR_PATH_LIVE and feature flags not checked"
  live="" flags=""
fi
cert=$(sqlq "SELECT count(*) FROM provider_certifications WHERE provider = 'stripe' AND status = 'passed'")
[[ "$cert" -ge 1 ]] || stop "no passed stripe certification (BIL-PROV-1): run cmd/certify, submit (B-O14), review (B-O17)"
pass "stripe certified (the release gate checks the exact adapter version: certify check stripe <adapter_version>)"
if [[ "$env_name" != prod ]]; then
  echo "  note run the CD-19 smoke and the Test Clock rehearsal (scripts/testclock) before prod"
fi

# ── 6. Operator routes ──────────────────────────────────────────────────
begin 6 "Operator routes"
if [[ "$live" == true ]]; then pass "OPERATOR_PATH_LIVE=true"; else echo "  info OPERATOR_PATH_LIVE=${live:-unknown}: operator routes answer 403 until CD-52 OP-G1..G3 are live"; fi

# ── 7. Feature flags ────────────────────────────────────────────────────
begin 7 "Feature flags"
if [[ -n "$flags" ]]; then
  grep -qx 'FEATURE_SEAT_PURCHASE=true' <<<"$flags" || stop "seat purchase must be on (§19.1 step 7)"
  pass "seat purchase on"
  while IFS= read -r f; do echo "  info $f"; done <<<"$flags"
fi
echo "first-deployment order: all checked steps complete for $env_name"
