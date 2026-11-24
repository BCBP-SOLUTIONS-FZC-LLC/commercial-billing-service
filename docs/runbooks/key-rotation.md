# Stripe key and webhook secret rotation

Planned (90 days for the restricted key, with every webhook endpoint rotation). LLD §20.5, §10.5.

1. Create the new restricted key (permissions in LLD §10.5) and the new webhook endpoint secret in the Stripe dashboard.
2. In OpenBao (`commercial/<env>/billing`): set `STRIPE_WEBHOOK_SECRETS` to `old,new` and `STRIPE_API_KEY` to the new key.
3. Roll the pods (External Secrets refreshes within `refreshInterval`; then `kubectl rollout restart deployment/commercial-billing -n billing`).
4. Switch the Stripe endpoint to the new secret; wait for Stripe's overlap window.
5. Remove the old secret (`STRIPE_WEBHOOK_SECRETS=new`); roll again; revoke the old key.

Watch `BillingWebhookSignatureFailures` throughout ([webhook-signature-failures.md](webhook-signature-failures.md)). Startup refuses a live key outside prod and a test key in prod (BIL-OPS-7).
