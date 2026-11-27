# AWS IAM policy for commercial-billing

This directory holds the reference IAM policy for the service's IRSA role.

- The application does not create the role; platform Terraform owns it.
- Copy `policy.json` into the Terraform module (`policy.tf.example` shows the wiring), or apply it with `aws iam create-policy` / `put-role-policy`.

The role is assumed by the ServiceAccount `commercial-billing` in namespace `billing` (`BIL-OPS-4`). Its ARN is not kept in any values file: the deploy pipeline sets the ServiceAccount annotation from the GitHub environment variable `BILLING_IRSA_ROLE_ARN` (`release.yml`, deploy-gate), which renders as

```yaml
serviceAccount:
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::<account-id>:role/commercial-billing
```

For `APP_ENV` `staging` and `prod` the chart refuses to render when the annotation is missing, is not an IAM role ARN, or carries the placeholder account `000000000000`. The same applies to the SNS topic ARN and queue URLs (`BILLING_SNS_TOPIC_ARN`, `BILLING_TENANT_BILLING_QUEUE_URL`; `BILLING_USAGE_BILLING_QUEUE_URL` only when `USAGE_BILLING_CONSUMER_ENABLED` is `true`) and to `networkPolicy.postgresCIDRs` (`BILLING_POSTGRES_CIDRS`).

**Network path (go-live check).** The server's NetworkPolicy admits HTTPS only to public addresses by default. If SNS, SQS, Glue or STS are reached through VPC interface endpoints (private addresses), set `networkPolicy.awsEndpointCIDRs` to the endpoints' subnets (`BILLING_AWS_ENDPOINT_CIDRS` in release.yml), or `networkPolicy.egressGateway` for a mesh egress gateway. Otherwise the server cannot resolve its Glue schemas at start-up and crash-loops.

The pods set `automountServiceAccountToken: false`: the EKS pod identity webhook mounts the web-identity token (`aws-iam-token` projected volume) whatever that setting is, and the app never calls the Kubernetes API.

- **The CronJobs** run as the same ServiceAccount (they need its mesh identity for Core and RP) but make no AWS call. They use Postgres, Core, RP, the provider and the Pushgateway, and their events leave through the server's outbox relay.
- **The migrate Job** runs as its own ServiceAccount `commercial-billing-migrate` (a Helm hook, created before the Job and deleted after it, whether it succeeds or fails), which has no role: it uses Postgres only. Do not add it to the role's trust policy.

## What Billing needs

The pod's only AWS traffic is events (LLD §7). All of it goes through platform-events, plus Billing's Glue encode codec (`internal/adapters/eventbus/glue_codec.go`).

| Sid | AWS service | Actions | Purpose | LLD |
|---|---|---|---|---|
| `PublishBillingEvents` | SNS | `Publish`, `GetTopicAttributes` | The outbox relay publishes the produced event types to `billing-events` (`SNS_BILLING_EVENTS_TOPIC_ARN`), ordered by `tenant_id` (`BIL-EVT-2`). | §7.2 |
| `ConsumeTenantAndUsageQueues` | SQS | `ReceiveMessage`, `DeleteMessage`, `GetQueueAttributes`, `GetQueueUrl`, `ChangeMessageVisibility` | Two consumers: `tenant-billing-q` (tenant lifecycle from IAM) and `usage-billing-q` (usage from Usage & Metering). `GetQueueAttributes` also feeds `platform_queue_depth`. | §7.1 |
| `ForwardToDeadLetterQueues` | SQS | `SendMessage`, `GetQueueAttributes`, `GetQueueUrl` | platform-events' DLQ publisher reads each queue's `RedrivePolicy`, resolves the DLQ and forwards rejected messages (malformed, schema violation, over the retry threshold). Startup refuses, outside dev, a queue with no `RedrivePolicy`. `GetQueueAttributes` on the DLQ feeds `platform_dlq_depth` (`BillingDLQNotEmpty`). | §7.1, §11.5 |
| `GlueSchemaRegistryReadOnly` | Glue | `GetRegistry`, `GetSchema`, `GetSchemaVersion`, `GetSchemaByDefinition` | At startup the encode codec resolves every produced schema by definition in `billing-events` (`GLUE_REGISTRY_NAME`). It refuses to start if one is missing, so registration (`schema-registry.yml`) must come before a deploy. | §7.6 |

Consumed events are not looked up in Glue. `events.GlueDecodeCodec` only strips the header, and the payload is validated against Billing's embedded copies of the producers' schemas. So the role needs no access to IAM's or Usage's registries.

## Not needed

If you are about to add one of these, stop and check the LLD first.

- **No S3 and no KMS.** Billing stores no objects. RDS encrypts at rest with a key the pod never touches.
- **No Secrets Manager or SSM.** Secrets come from OpenBao through External Secrets Operator (`externalSecret` in the Helm values). The operator's own identity reads them, not the pod's.
- **No card data anywhere** (`BIL-PCI-1..3`, PCI SAQ A). The Stripe key is an OpenBao secret, and Stripe is called over HTTPS, not through AWS.
- **No CloudWatch.** Metrics are scraped by Prometheus (ServiceMonitor) or pushed to the Pushgateway (CronJobs). Logs go to stdout.
- **No `sqs:SendMessage` on the source queues.** Billing only reads them; it writes only to their DLQs.
- **No Glue write actions** (`CreateSchema`, `RegisterSchemaVersion`, `UpdateSchema`). Registration is CI-only, under a separate principal (see `policy.tf.example`).
- **No STS chaining.** The pod assumes nothing beyond its IRSA role.

## Rolling out

1. Apply the policy (Terraform or the IAM console), with the role's trust policy limited to `system:serviceaccount:billing:commercial-billing`.
2. Check each grant from a debug pod that uses the same ServiceAccount:
   ```bash
   aws sns get-topic-attributes --topic-arn <billing_events_topic_arn>                         # expect Attributes
   aws sqs get-queue-attributes --queue-url <tenant_billing_queue_url> --attribute-names RedrivePolicy   # expect a RedrivePolicy
   aws sqs get-queue-attributes --queue-url <usage_billing_queue_url>  --attribute-names RedrivePolicy
   aws glue get-registry --registry-id RegistryName=billing-events                             # expect a Registry
   ```
   Also run `make schema-verify` from CI: every produced schema must be registered and `AVAILABLE`.
3. Deploy the image and chart (`release.yml`, deploy gate).
4. Watch for a policy that didn't apply:
   - `platform_dlq_messages_total{service="commercial-billing",operation="outbox_publish"}` rising after rollout means SNS Publish is denied (`BillingOutboxDeadLetter`);
   - a pod in CrashLoopBackOff logging a Glue `AccessDeniedException` means `GetSchemaByDefinition` is denied.

   Roll back if either appears.

## Least privilege beyond this policy

Platform infrastructure manages these, not this repo:

- **`billing-events` topic policy:** only this role may publish, and only the consumers' queues may subscribe (Entitlements, and the others in LLD §7.2).
- **Queue policies on `tenant-billing-q` and `usage-billing-q`:** only the producing topic may send to each, and only this role may receive.
