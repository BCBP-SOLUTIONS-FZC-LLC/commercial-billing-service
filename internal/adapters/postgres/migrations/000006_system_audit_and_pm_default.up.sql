-- 000006_system_audit_and_pm_default (LLD rev 0.36, §4.2.7, §4.4, §10.6)
-- Generated from docs/lld/commercial-lld-billing-service.md §4. Change the LLD first.

-- Global operator changes (price book, provider certifications,
-- reconciliation runs) are written as billing_system; their AL-5 entry is
-- enqueued in the same transaction (BIL-AUD-1). billing_system has BYPASSRLS,
-- so no policy is needed.
GRANT SELECT, INSERT ON pending_audit_entries TO billing_system;

-- A completed payment-method setup session whose new method has been made the
-- provider default for the customer and subscription (B-P16; lifecycle
-- follow-up worker). NULL until applied. No payment-method data is stored.
ALTER TABLE checkout_sessions ADD COLUMN default_applied_at timestamptz;
CREATE INDEX idx_cs_pm_pending ON checkout_sessions (tenant_id)
  WHERE purpose = 'payment_method_setup' AND status = 'completed' AND default_applied_at IS NULL;
