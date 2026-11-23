#!/usr/bin/env bash
# Floci ready-hook for local runs (docker-compose): provisions Billing's
# messaging topology (LLD §7) and the billing-events Glue registry (§7.2).
# Mounted at /etc/floci/init/ready.d/ with the embedded schemas at
# /etc/floci/init/schemas (read-only), so schemas come straight from source.
#
# Creates:
#   SNS topics   billing-events (Billing's), iam-tenant-events and usage-events
#                (stand-ins for RP's and Usage & Metering's Terraform)
#   SQS queues   tenant-billing-q, usage-billing-q (Billing consumes), and the
#                six billing-events subscriber queues of §7.3.9 so published
#                events can be inspected; each with <queue>-dlq, maxReceiveCount 5
#   SNS→SQS      raw delivery, filter on the EventType attribute
#   Glue         registry billing-events with the six produced schemas,
#                registered in schema-gov's compact form so the GlueCodec's
#                GetSchemaByDefinition matches (dev runs use plain JSON unless
#                APP_ENV is not dev-like)
set -euo pipefail

AWS_REGION=ap-south-1
AWS_ACCOUNT=000000000000
MAX_RECEIVES=5
SCHEMAS_DIR=/etc/floci/init/schemas
# The compat image's AWS CLI defaults to us-east-1; floci isolates regions.
export AWS_DEFAULT_REGION="$AWS_REGION" AWS_REGION="$AWS_REGION"

topic_arn() { printf 'arn:aws:sns:%s:%s:%s' "$AWS_REGION" "$AWS_ACCOUNT" "$1"; }
queue_arn() { printf 'arn:aws:sqs:%s:%s:%s' "$AWS_REGION" "$AWS_ACCOUNT" "$1"; }

create_queue_with_dlq() {
  local queue="$1" attrs
  aws sqs create-queue --queue-name "${queue}-dlq" >/dev/null
  attrs=$(mktemp)
  printf '{"RedrivePolicy":"{\\"deadLetterTargetArn\\":\\"%s\\",\\"maxReceiveCount\\":\\"%s\\"}"}' \
    "$(queue_arn "${queue}-dlq")" "$MAX_RECEIVES" > "$attrs"
  aws sqs create-queue --queue-name "$queue" --attributes "file://$attrs" >/dev/null
  rm -f "$attrs"
}

# subscribe <topic> <queue> <EventType list as a JSON array>
subscribe() {
  local sub attrs
  sub=$(aws sns subscribe --topic-arn "$(topic_arn "$1")" --protocol sqs \
    --notification-endpoint "$(queue_arn "$2")" --attributes RawMessageDelivery=true \
    --query SubscriptionArn --output text)
  attrs=$(mktemp)
  printf '{"EventType": %s}' "$3" > "$attrs"
  aws sns set-subscription-attributes --subscription-arn "$sub" \
    --attribute-name FilterPolicy --attribute-value "file://$attrs" >/dev/null
  rm -f "$attrs"
}

# register_schema <schema-name> <file>: compact JSON, as schema-gov uploads.
register_schema() {
  local def
  def=$(python3 -c 'import json,sys; print(json.dumps(json.load(open(sys.argv[1])), separators=(",", ":")), end="")' "$SCHEMAS_DIR/$2")
  aws glue create-schema --registry-id RegistryName=billing-events --schema-name "$1" \
    --data-format JSON --compatibility BACKWARD --schema-definition "$def" >/dev/null
}

# ── Topics ──────────────────────────────────────────────────────────────
for t in billing-events iam-tenant-events usage-events; do
  aws sns create-topic --name "$t" >/dev/null
done

# ── Billing's inbound queues (§7.1) ─────────────────────────────────────
create_queue_with_dlq tenant-billing-q
subscribe iam-tenant-events tenant-billing-q '["TenantConverted","TenantSuspended","TenantReactivated","TenantOffboarded"]'
create_queue_with_dlq usage-billing-q
subscribe usage-events usage-billing-q '["UsagePeriodClosed"]'

# ── billing-events subscribers (§7.3.9), for inspecting published events ─
create_queue_with_dlq billing-orgm-q
subscribe billing-events billing-orgm-q '["TenantPlanChanged","TenantSeatsChanged","TenantPaymentPastDue","TenantSubscriptionCancelled","TenantReactivated"]'
create_queue_with_dlq billing-authz-q
subscribe billing-events billing-authz-q '["TenantPlanChanged","TenantSubscriptionCancelled","TenantReactivated"]'
create_queue_with_dlq billing-realm-q
subscribe billing-events billing-realm-q '["TenantReactivated"]'
create_queue_with_dlq billing-audit-q
subscribe billing-events billing-audit-q '["TenantPlanChanged","TenantSeatsChanged","TenantPaymentPastDue","TenantSubscriptionCancelled","TenantReactivated","TenantQuotaAddOnChanged"]'
create_queue_with_dlq billing-entitlements-q
subscribe billing-events billing-entitlements-q '["TenantPlanChanged","TenantSubscriptionCancelled","TenantReactivated","TenantQuotaAddOnChanged"]'
create_queue_with_dlq billing-notification-q
subscribe billing-events billing-notification-q '["TenantPaymentPastDue","TenantSubscriptionCancelled"]'

# ── Glue Schema Registry (§7.2) ─────────────────────────────────────────
aws glue create-registry --registry-name billing-events >/dev/null
register_schema TenantPlanChanged           tenant_plan_changed.json
register_schema TenantSeatsChanged          tenant_seats_changed.json
register_schema TenantPaymentPastDue        tenant_payment_past_due.json
register_schema TenantSubscriptionCancelled tenant_subscription_cancelled.json
register_schema TenantReactivated           tenant_reactivated.json
register_schema TenantQuotaAddOnChanged     tenant_quota_add_on_changed.json

echo "commercial-billing floci topology ready"
