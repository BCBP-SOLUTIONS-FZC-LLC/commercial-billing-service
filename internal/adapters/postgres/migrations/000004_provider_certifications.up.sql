-- 000004_provider_certifications: §4.2.24 table, trg_pc_guard, grants, manual exempt record
-- Generated from docs/lld/commercial-lld-billing-service.md §4 (rev 0.31). Table and
-- constraint SQL is copied verbatim from the LLD; change the LLD first.

CREATE TYPE certification_status AS ENUM ('pending_review', 'passed', 'failed', 'rejected', 'revoked', 'exempt');

CREATE TABLE provider_certifications (
  id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  provider              text NOT NULL CHECK (provider ~ '^[a-z][a-z0-9_]{1,30}$'),   -- 'stripe', 'checkout_com', 'adyen', 'manual'
  adapter_version       text NOT NULL,                 -- AdapterVersion(): '<adapter semver>+<provider api version>'
  provider_api_version  text,                          -- e.g. the Stripe API version pinned by the adapter; NULL for 'manual'
  suite_version         text NOT NULL,                 -- certification suite version (§14.7)
  status                certification_status NOT NULL,
  proration_mode        text NOT NULL CHECK (proration_mode IN ('native', 'fallback', 'n/a')),
  capabilities          jsonb NOT NULL,                -- ProviderCapabilities as declared by the adapter
  results               jsonb NOT NULL,                -- [{capability, case, passed, evidence_ref}] (§19.6)
  provider_account_ref  text,                          -- test-mode account the suite ran against (never a secret)
  run_started_at        timestamptz,
  run_finished_at       timestamptz,
  certified_by          uuid NOT NULL,                 -- Billing engineer (operator sub) who submitted the run (B-O14)
  certified_at          timestamptz NOT NULL DEFAULT now(),
  reviewed_by           uuid,                          -- PCI owner group member who decided the run (B-O17, BL-OQ-21)
  reviewed_at           timestamptz,
  review_reason         text CHECK (length(review_reason) BETWEEN 10 AND 1000),
  revoked_at            timestamptz,
  revoked_by            uuid,
  revoked_reason        text,
  CONSTRAINT chk_pc_revoked CHECK ((status = 'revoked') = (revoked_at IS NOT NULL)),
  CONSTRAINT chk_pc_passed_reviewed CHECK (status NOT IN ('passed', 'rejected', 'revoked')
        OR (reviewed_by IS NOT NULL AND reviewed_at IS NOT NULL AND review_reason IS NOT NULL)),
  CONSTRAINT chk_pc_unreviewed CHECK (status NOT IN ('pending_review', 'failed', 'exempt') OR reviewed_by IS NULL),
  CONSTRAINT chk_pc_two_people CHECK (reviewed_by IS NULL OR reviewed_by <> certified_by),   -- BIL-PROV-4
  CONSTRAINT chk_pc_exempt CHECK (status <> 'exempt' OR provider = 'manual'),
  CONSTRAINT chk_pc_fallback_named CHECK (status <> 'passed' OR proration_mode IN ('native', 'fallback'))
);
-- One live record per run: a failed, rejected or revoked run can be submitted again
CREATE UNIQUE INDEX uq_pc_run ON provider_certifications (provider, adapter_version, suite_version)
  WHERE status IN ('pending_review', 'passed', 'exempt');
CREATE INDEX idx_provider_cert_current ON provider_certifications (provider, adapter_version)
  WHERE status IN ('passed', 'exempt');

-- BIL-PROV-4: never deleted; after insert only the review decision (B-O17) and revocation (B-O15) may change a row
CREATE FUNCTION provider_certifications_guard() RETURNS trigger AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'provider_certifications rows are never deleted' USING ERRCODE = 'P0001';
  END IF;
  IF (NEW.id, NEW.provider, NEW.adapter_version, NEW.provider_api_version, NEW.suite_version, NEW.proration_mode,
      NEW.capabilities, NEW.results, NEW.provider_account_ref, NEW.run_started_at, NEW.run_finished_at,
      NEW.certified_by, NEW.certified_at)
     IS DISTINCT FROM
     (OLD.id, OLD.provider, OLD.adapter_version, OLD.provider_api_version, OLD.suite_version, OLD.proration_mode,
      OLD.capabilities, OLD.results, OLD.provider_account_ref, OLD.run_started_at, OLD.run_finished_at,
      OLD.certified_by, OLD.certified_at) THEN
    RAISE EXCEPTION 'provider_certifications: run, evidence and submitter are immutable' USING ERRCODE = 'P0001';
  END IF;
  IF OLD.status = 'pending_review' AND NEW.status IN ('passed', 'rejected')
     AND OLD.reviewed_by IS NULL AND NEW.revoked_at IS NULL THEN
    RETURN NEW;                                                  -- B-O17 decision
  END IF;
  IF OLD.status = 'passed' AND NEW.status = 'revoked'
     AND (NEW.reviewed_by, NEW.reviewed_at, NEW.review_reason)
         IS NOT DISTINCT FROM (OLD.reviewed_by, OLD.reviewed_at, OLD.review_reason) THEN
    RETURN NEW;                                                  -- B-O15 revocation
  END IF;
  RAISE EXCEPTION 'provider_certifications: % -> % not allowed', OLD.status, NEW.status USING ERRCODE = 'P0001';
END $$ LANGUAGE plpgsql;
CREATE TRIGGER trg_pc_guard BEFORE UPDATE OR DELETE ON provider_certifications
  FOR EACH ROW EXECUTE FUNCTION provider_certifications_guard();
REVOKE ALL ON provider_certifications FROM PUBLIC;
GRANT SELECT ON provider_certifications TO billing_app, admin_readonly;
GRANT SELECT, INSERT, UPDATE ON provider_certifications TO billing_system;

-- The manual adapter's exempt record for its compiled adapter_version (§4.4).
-- internal/adapters/provider/manual must report exactly this version.
INSERT INTO provider_certifications (provider, adapter_version, provider_api_version, suite_version, status,
  proration_mode, capabilities, results, certified_by)
VALUES ('manual', '1.0.0+manual', NULL, 'cert-1.0', 'exempt', 'n/a', '{}'::jsonb, '[]'::jsonb,
  '00000000-0000-0000-0000-0000000000c1');
