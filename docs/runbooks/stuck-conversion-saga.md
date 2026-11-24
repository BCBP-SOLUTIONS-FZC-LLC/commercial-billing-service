# Stuck conversion saga

**Alerts:** `BillingConversionSagaOverdue` (SEV-2: a saga is not `confirmed` 30 minutes after payment, BIL-OPS-5), `BillingSagaFailed` (SEV-2: a saga entered `failed`). LLD §20.1, §8.2, §9.3.

**Effect:** the tenant has paid but IAM has not converted it (still on trial limits). No double charge is possible: the RP-3 key is stable (`convert:<tenant_id>:<billing_subscription_id>`, BIL-SAGA-2).

1. B-O7 `GET /api/v1/operator/tenants/:id/subscription` for the tenant: saga `state`, `attempts`, `last_error_code`, `payment_confirmed_at`. The `saga_id` and `tenant_id` are in the SEV-2 log line.
2. **`rp_convert_pending`** with `operation_in_progress` / `502` / `503`: check Realm Provisioner health and its `provisioning_operations` row for the key above. When RP is healthy, B-O9 `retry` (safe: same key).
3. **`awaiting_tenant_converted`**: ask IAM whether RP emitted `TenantConverted` (RP outbox). If it did, check `tenant-billing-q-dlq` and redrive ([consumer-dlq.md](consumer-dlq.md)); the saga confirms itself when the event matches. If RP shows the conversion done but no event will come, B-O9 `resolved_manually` after IAM re-emits or confirms.
4. **`failed`** (`tenant_offboarded`, `403`, `404`): fix admission or data with IAM, then B-O9 `retry`. If the tenant cannot be converted, B-O9 `refund`.
5. **Direct signup where RP-2 never succeeds:** coordinate with the BFF team; if the signup cannot complete, B-O9 `refund` (`signup_failed`).
6. **`IAMParkedTenantEventsStale` for a direct signup** (Billing's saga already confirmed; the alert is Core's and is routed to Billing on-call too): IAM re-drives I-1 or the backstop event. If IAM cannot complete it, refund through B-O3 (`signup_failed`) and record B-O9 `resolved_manually`.

**Verify:** `commercial_billing_sagas_overdue` returns to 0; the tenant's subscription view shows the saga `confirmed`.
