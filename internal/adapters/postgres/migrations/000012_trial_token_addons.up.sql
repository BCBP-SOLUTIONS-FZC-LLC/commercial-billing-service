-- 000012_trial_token_addons (rev 0.75, HLD v0.69 P-5): token add-ons for
-- trial tenants (BIL-ADD-1 (b), BIL-ADD-5).
-- (1) A purchase bought during a trial is marked: it counts toward the
--     trial cap and is reported in the quarterly trial-cost review (§4.2.12).
ALTER TABLE quota_addon_purchases
  ADD COLUMN trial_purchase boolean NOT NULL DEFAULT false;

-- (2) A platform_operator's raised trial add-on cap for one tenant (B-O18,
--     §4.2.25). No row means the default cap (1 pack, USD 50). Deleted when
--     the conversion is confirmed (§7.1 TenantConverted).
CREATE TABLE trial_addon_caps (
  tenant_id         uuid PRIMARY KEY,
  max_packs         int    NOT NULL CHECK (max_packs BETWEEN 1 AND 10),
  max_amount_minor  bigint NOT NULL CHECK (max_amount_minor BETWEEN 1 AND 200000),   -- USD minor units (P-5e)
  reason            text   NOT NULL CHECK (length(reason) BETWEEN 10 AND 1000),
  set_by            uuid   NOT NULL,                  -- operator sub
  set_at            timestamptz NOT NULL DEFAULT now(),
  record_version    bigint NOT NULL DEFAULT 1
);

-- RLS and grants as every tenant table (BIL-RLS-1, §4.3).
ALTER TABLE trial_addon_caps ENABLE ROW LEVEL SECURITY;
ALTER TABLE trial_addon_caps FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON trial_addon_caps
  USING      (rls_check_tenant(tenant_id, 'trial_addon_caps'))
  WITH CHECK (rls_check_tenant(tenant_id, 'trial_addon_caps'));
REVOKE ALL ON trial_addon_caps FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON trial_addon_caps TO billing_app;
GRANT SELECT ON trial_addon_caps TO admin_readonly;
-- billing_system (BYPASSRLS) reads tenant tables (§4.3 GUC binding table).
GRANT SELECT ON trial_addon_caps TO billing_system;
