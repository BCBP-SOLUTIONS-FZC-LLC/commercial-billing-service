# Webhook signature failures

**Alert:** `BillingWebhookSignatureFailures` (SEV-2): more than 5 refused callbacks per minute for 5 minutes (`commercial_billing_webhook_signature_failures_total`, BIL-PAY-2). Refused callbacks are never stored.

1. **Secret mismatch after a rotation** (most likely when it starts right after a deploy or a Stripe endpoint change): `STRIPE_WEBHOOK_SECRETS` must hold the endpoint's current secret (two during the overlap). Fix in OpenBao and roll pods ([key-rotation.md](key-rotation.md)).
2. **Livemode mismatch:** a test-mode endpoint pointing at production (or the reverse) is refused (BIL-OPS-7). Fix the endpoint in the Stripe dashboard.
3. **Clock skew:** timestamps outside `WEBHOOK_TOLERANCE` (300 s) are refused. Check node NTP.
4. **Abuse:** a burst from unknown sources is an attack on the public endpoint; the gateway limits B-W1 to 50 rps. Escalate to Security (HLD §11.6 review trigger).
5. Stripe redelivers refused events for up to 3 days; after the fix they arrive. For anything older, run a reconciliation for the window ([reconciliation-drift.md](reconciliation-drift.md)), which ledgers missing events.
