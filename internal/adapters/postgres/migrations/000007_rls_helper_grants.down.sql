-- 000007 down: back to the grants of 000001 (PUBLIC stays revoked).
GRANT EXECUTE ON FUNCTION app_tenant_id(), rls_check_tenant(uuid, text)
  TO billing_app, billing_system, admin_readonly, billing_audit_drainer;
