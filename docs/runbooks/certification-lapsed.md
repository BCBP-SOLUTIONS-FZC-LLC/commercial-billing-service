# Provider certification lapsed

**Alert:** `BillingProviderNotCertified` (SEV-2): a provider in `ENABLED_PAYMENT_PROVIDERS` has no valid certification for the deployed `adapter_version` (BIL-PROV-1 / BIL-PROV-2). LLD §20.9, §14.7.

**Cause:** an adapter or pinned provider API version change deployed without a certification record, a suite version bump (`PROVIDER_CERT_SUITE_VERSION`), or a record revoked (B-O15).

**Effect:** the provider takes no new billing accounts (`503 PROVIDER_NOT_ENABLED` on B-P2, B-I1, and B-O1 if it is the default). Existing subscriptions, renewals, purchases and webhooks keep working.

1. Confirm the deployed `adapter_version` (the `adapter_version` label on `commercial_billing_provider_certified`).
2. Run `bin/certify run --target test-mode` for that version ([certification.md](certification.md)).
3. Upload the evidence, `bin/certify submit` (B-O14); a **different** PCI owner group member runs `bin/certify review --decision approve` (B-O17, BIL-PROV-4).
4. The registry enables the provider within `PROVIDER_REGISTRY_REFRESH` (60 s); the gauge returns to 1.
5. If the run fails, roll back to the last certified adapter version.
