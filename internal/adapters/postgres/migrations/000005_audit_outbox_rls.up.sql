-- 000005_audit_outbox_rls: RLS on platform-audit's pending_audit_entries (§4.2.23, library LLD §4.3)
-- Generated from docs/lld/commercial-lld-billing-service.md §4 (rev 0.31). Table and
-- constraint SQL is copied verbatim from the LLD; change the LLD first.

DO $$
BEGIN
  IF to_regclass('public.pending_audit_entries') IS NULL THEN
    RAISE EXCEPTION '000005: pending_audit_entries does not exist; run auditoutbox.ApplySchema first (cmd/migrate does)';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'billing_audit_drainer') THEN
    RAISE EXCEPTION '000005: role billing_audit_drainer (NOBYPASSRLS) must exist; the platform DB bootstrap creates it';
  END IF;
END $$;

ALTER TABLE pending_audit_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE pending_audit_entries FORCE  ROW LEVEL SECURITY;
-- WITH CHECK admits platform_tenant (…00b1) rows written from a tenant-bound session.
CREATE POLICY tenant_isolation ON pending_audit_entries
  USING      (rls_check_tenant(tenant_id, 'pending_audit_entries'))
  WITH CHECK (CASE WHEN tenant_id = '00000000-0000-0000-0000-0000000000b1' THEN true
                   ELSE rls_check_tenant(tenant_id, 'pending_audit_entries') END);
-- The drainer must see every row (auditoutbox.Runner).
CREATE POLICY audit_drainer_all ON pending_audit_entries
  FOR ALL TO billing_audit_drainer USING (true) WITH CHECK (true);

REVOKE ALL ON pending_audit_entries FROM PUBLIC;
GRANT SELECT, INSERT ON pending_audit_entries TO billing_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON pending_audit_entries TO billing_audit_drainer;
GRANT SELECT ON pending_audit_entries TO admin_readonly;
