# Conversion reversal decision

**Alert:** `BillingReversalDecisionNeeded` (SEV-2, log-based): a dispute with reason `fraudulent` or an early fraud warning arrived on a conversion payment within 7 days of `converted_at`. LLD §8.2 (Finance and Security approved, BL-OQ-7). `AUTO_REVERT_ON_EFW=false`: an operator decides.

1. Review the evidence with Security (B-O7: dispute or warning, payment, tenant activity).
2. **Revert:** B-O8 `POST /api/v1/operator/tenants/:id/conversion-reversals` with a reason. Billing calls RP-4 (only within 7 days; `409 REVERT_WINDOW_EXPIRED` after), ends the subscription and refunds the conversion payment unless the dispute already returned the funds. AL-5 `billing.conversion.reverted`.
3. **Keep:** record the decision in the incident; respond to the dispute in the Stripe dashboard.
