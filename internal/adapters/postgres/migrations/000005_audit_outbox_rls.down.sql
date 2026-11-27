-- 000005 down
-- Generated from docs/lld/commercial-lld-billing-service.md §4 (rev 0.31). Table and
-- constraint SQL is copied verbatim from the LLD; change the LLD first.

DROP POLICY IF EXISTS audit_drainer_all ON pending_audit_entries;
DROP POLICY IF EXISTS tenant_isolation ON pending_audit_entries;
ALTER TABLE pending_audit_entries NO FORCE ROW LEVEL SECURITY;
ALTER TABLE pending_audit_entries DISABLE ROW LEVEL SECURITY;
