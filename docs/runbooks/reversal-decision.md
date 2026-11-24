# Conversion reversal decision

**Alert:** `BillingReversalDecisionNeeded` (SEV-2, log-based): a dispute with reason `fraudulent` or an early fraud warning arrived on a conversion payment within 7 days of `converted_at`. LLD §8.2 (Finance and Security approved, BL-OQ-7). `AUTO_REVERT_ON_EFW=false`: an operator decides.

1. Review the evidence with Security (B-O7: dispute or warning, payment, tenant activity).
2. **Revert:** B-O8 `POST /api/v1/operator/tenants/:id/conversion-reversals` with a reason. Billing calls RP-4 (only within 7 days; `409 REVERT_WINDOW_EXPIRED` after), ends the subscription and refunds the conversion payment unless the dispute already returned the funds. AL-5 `billing.conversion.reverted`.
   - **Add-ons are not undone by B-O8** (Finance approved the conversion payment only, BL-OQ-7). List the add-on purchases since `converted_at` (B-O7 `quota_addon_purchases[]`; invoices through B-O4) and refund each paid token add-on invoice through B-O3 `POST /api/v1/operator/tenants/:id/refunds` with category `fraud`; that emits `TenantQuotaAddOnChanged{reason: refund}` and Entitlements removes the unused tokens (`BIL-ADD-4`).
   - **Plan and seats:** Billing sends no event. Org & Membership resets the plan to the trial plan and seats to the trial allowance when it applies RP's `TrialReactivated{reason: conversion_revert}` (LLD §8.2 step 3, BL-OQ-27). Until that ships, the reverted tenant keeps the paid plan and seats: record it in the incident and ask IAM to correct the tenant.
3. **Keep:** record the decision in the incident; respond to the dispute in the Stripe dashboard.
