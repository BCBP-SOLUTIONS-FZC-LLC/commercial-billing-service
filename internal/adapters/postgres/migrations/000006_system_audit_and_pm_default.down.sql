-- 000006 down
DROP INDEX IF EXISTS idx_cs_pm_pending;
ALTER TABLE checkout_sessions DROP COLUMN IF EXISTS default_applied_at;
REVOKE SELECT, INSERT ON pending_audit_entries FROM billing_system;
