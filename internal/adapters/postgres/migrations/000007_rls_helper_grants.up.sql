-- 000007_rls_helper_grants: least privilege on the SECURITY DEFINER RLS helpers (§4.3)
--
-- app_tenant_id(), log_rls_violation() and rls_check_tenant() run as their
-- owner (billing_migrator). PostgreSQL grants EXECUTE to PUBLIC on every new
-- function; 000001 revoked that, and this migration re-asserts it and narrows
-- the remaining grants to the roles whose queries evaluate a policy:
--
--   rls_check_tenant  called by every tenant_isolation policy (000001, 000005):
--                     billing_app, admin_readonly and billing_audit_drainer
--                     (NOBYPASSRLS; the drainer's audit_drainer_all policy is
--                     OR-ed with tenant_isolation, which is still evaluated).
--                     billing_system has BYPASSRLS, so no policy is evaluated
--                     for it and it needs no grant.
--   app_tenant_id     called only from inside rls_check_tenant and
--   log_rls_violation log_rls_violation, i.e. as the owner: no role needs a grant.
--
-- The owner keeps EXECUTE implicitly; superusers are unaffected.

REVOKE ALL ON FUNCTION app_tenant_id(), log_rls_violation(text, uuid, text), rls_check_tenant(uuid, text) FROM PUBLIC;

REVOKE ALL ON FUNCTION app_tenant_id(), log_rls_violation(text, uuid, text)
  FROM billing_app, billing_system, admin_readonly, billing_audit_drainer;

REVOKE ALL ON FUNCTION rls_check_tenant(uuid, text) FROM billing_system;
GRANT EXECUTE ON FUNCTION rls_check_tenant(uuid, text) TO billing_app, admin_readonly, billing_audit_drainer;
