# Consumer DLQ not empty

**Alert:** `BillingDLQNotEmpty` (SEV-3): messages in `tenant-billing-q-dlq` or `usage-billing-q-dlq` (§7.1). The gauge `platform_dlq_depth` is labelled with the source queue (`queue="tenant-billing-q"`), not the DLQ's name.

1. Read the DLQ message attributes: `dlq_reason` is `schema_violation` (payload fails Billing's consumed schema or cannot be decoded), `future_time_clamp` (envelope time beyond `MAX_LIFECYCLE_EVENT_SKEW_SECONDS`), or absent (handler failed 4 receives).
2. **`schema_violation`:** a producer contract change. Compare with `test/contract/testdata/consumers/` and raise it with the producer (IAM for tenant-billing-q, Usage & Metering for usage-billing-q). Do not redrive until Billing accepts the payload.
3. **`future_time_clamp`:** the producer's clock is wrong; fix it, then redrive.
4. **Handler failures:** read the Billing logs for the `event_id`; fix (database, lock contention), then redrive with the SQS console "start DLQ redrive". Handlers are idempotent (inbox, PE-1), so a redrive never double-applies.
5. A `TenantConverted` stuck here keeps a saga in `awaiting_tenant_converted` — see [stuck-conversion-saga.md](stuck-conversion-saga.md).
