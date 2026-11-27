-- 000001_initial_schema: extensions, enums, all §4.2 tables, RLS helpers, RLS, triggers, grants (LLD §4.4)
-- Generated from docs/lld/commercial-lld-billing-service.md §4 (rev 0.31). Table and
-- constraint SQL is copied verbatim from the LLD; change the LLD first.

-- Roles (created by the platform DB bootstrap; grants in 000001)
--   billing_migrator  owns the schema; used only by cmd/migrate
--   billing_app       NOBYPASSRLS; HTTP handlers, consumers, workers once the tenant is bound
--   billing_system    BYPASSRLS; webhook pre-resolution, cron tenant listing, reconciliation, price-book writes
--   admin_readonly    NOBYPASSRLS; support queries

CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TYPE plan_code            AS ENUM ('starter','pro','enterprise');   -- Catalog plan codes (HLD §2.3)
CREATE TYPE billing_interval     AS ENUM ('month','year');
CREATE TYPE subscription_status  AS ENUM ('incomplete','active','past_due','cancelled','ended');
CREATE TYPE subscription_origin  AS ENUM ('trial_conversion','direct_signup','operator_stub');
CREATE TYPE iam_lifecycle        AS ENUM ('trial','active','suspended','offboarded');
CREATE TYPE saga_kind            AS ENUM ('trial_conversion','direct_signup','operator_stub');
CREATE TYPE saga_state           AS ENUM ('payment_confirmed','rp_convert_pending','awaiting_tenant_converted',
                                          'confirmed','failed','refund_pending','refunded','reverted');
CREATE TYPE purchase_status      AS ENUM ('pending_payment','requires_action','confirmed','failed','expired','cancelled');
CREATE TYPE schedule_status      AS ENUM ('scheduled','applying','applied','skipped','cancelled');
CREATE TYPE change_status        AS ENUM ('pending_payment','scheduled','applied','blocked','failed','cancelled');
CREATE TYPE invoice_kind         AS ENUM ('subscription_create','subscription_cycle','proration','token_addon','manual');
CREATE TYPE invoice_status       AS ENUM ('draft','open','paid','void','uncollectible');
CREATE TYPE ledger_status        AS ENUM ('received','processing','processed','ignored','failed');
CREATE TYPE cancel_reason        AS ENUM ('dunning_exhausted','customer','auto_renew_off','operator_reversal','offboarded','signup_failed');

-- ── RLS helpers (platform pattern, as in the IAM services) ─────────────────
-- app_tenant_id(): the transaction-local app.tenant_id GUC bound by pgcommon
-- (BIL-RLS-2); NULL on a missing or malformed value, so RLS fails closed.
CREATE FUNCTION app_tenant_id() RETURNS uuid
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = public AS $$
DECLARE v text;
BEGIN
    v := current_setting('app.tenant_id', true);
    IF v IS NULL OR v = '' THEN RETURN NULL; END IF;
    RETURN v::uuid;
EXCEPTION WHEN OTHERS THEN RETURN NULL;
END;
$$;

-- rls_violation_log: 1%-sampled audit trail; RLS disabled on itself (§4.2.23).
CREATE TABLE rls_violation_log (
    id               bigserial   PRIMARY KEY,
    table_name       text        NOT NULL,
    row_tenant_id    uuid,
    app_tenant_id    uuid,
    violation_type   text        NOT NULL,   -- 'missing_or_invalid_guc' | 'cross_tenant_access'
    user_id          uuid,
    session_role     text        DEFAULT SESSION_USER,
    client_addr      inet        DEFAULT inet_client_addr(),
    application_name text        DEFAULT current_setting('application_name', true),
    query_text       text,
    occurred_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_rls_violation_log_occurred ON rls_violation_log (occurred_at);
ALTER TABLE rls_violation_log DISABLE ROW LEVEL SECURITY;

CREATE FUNCTION log_rls_violation(p_table_name text, p_row_tenant_id uuid, p_violation_type text) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = public AS $$
BEGIN
    IF random() > 0.01 THEN RETURN; END IF;
    INSERT INTO rls_violation_log (table_name, row_tenant_id, app_tenant_id, violation_type, query_text)
    VALUES (p_table_name, p_row_tenant_id, app_tenant_id(), p_violation_type, current_query());
EXCEPTION WHEN OTHERS THEN
    NULL;
END;
$$;

-- rls_check_tenant(row_tenant, tbl): missing or malformed app.tenant_id → false
-- (missing_or_invalid_guc); another tenant → false (cross_tenant_access) (§4.3).
CREATE FUNCTION rls_check_tenant(p_tenant_id uuid, p_table_name text) RETURNS boolean
    LANGUAGE plpgsql STABLE STRICT SECURITY DEFINER
    SET search_path = public AS $$
DECLARE v_app uuid;
BEGIN
    v_app := app_tenant_id();
    IF v_app IS NULL THEN
        PERFORM log_rls_violation(p_table_name, p_tenant_id, 'missing_or_invalid_guc');
        RETURN false;
    END IF;
    IF p_tenant_id <> v_app THEN
        PERFORM log_rls_violation(p_table_name, p_tenant_id, 'cross_tenant_access');
        RETURN false;
    END IF;
    RETURN true;
END;
$$;

-- ── §4.2.1 ──
CREATE TABLE billing_accounts (
  tenant_id              uuid PRIMARY KEY,                          -- IAM tenant id (no FK: other database)
  provider               text NOT NULL CHECK (provider IN ('stripe','manual')),
  provider_customer_id   text,                                      -- NULL only for 'manual'
  currency               char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'), -- fixed once a subscription exists (BIL-SUB-6)
  billing_contact_name   text CHECK (length(billing_contact_name) <= 200),
  billing_contact_email  text CHECK (length(billing_contact_email) <= 254),   -- PII (§15.2)
  company_name           text CHECK (length(company_name) <= 200),
  country                char(2) CHECK (country ~ '^[A-Z]{2}$'),
  tax_id                 text CHECK (length(tax_id) <= 64),          -- Phase 2 tax invoices
  default_pm_set         boolean NOT NULL DEFAULT false,            -- provider reports a default payment method (no PM data stored)
  iam_lifecycle          iam_lifecycle NOT NULL DEFAULT 'trial',    -- from tenant-billing-q (§7.1)
  iam_suspension_source  text CHECK (iam_suspension_source IN ('billing_lapse','operator')),
  iam_last_event_at      timestamptz,                               -- BIL-EVT-3 recency mark for IAM lifecycle events
  closed_at              timestamptz,                               -- TenantOffboarded
  pii_minimised_at       timestamptz,                               -- §15.3
  record_version         bigint NOT NULL DEFAULT 1 CHECK (record_version > 0),
  created_at             timestamptz NOT NULL DEFAULT now(),
  updated_at             timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT uq_ba_provider_customer UNIQUE (provider, provider_customer_id),
  CONSTRAINT chk_ba_customer CHECK (provider = 'manual' OR provider_customer_id IS NOT NULL),
  CONSTRAINT chk_ba_suspension CHECK ((iam_lifecycle = 'suspended') = (iam_suspension_source IS NOT NULL)),
  CONSTRAINT chk_ba_closed CHECK ((iam_lifecycle = 'offboarded') = (closed_at IS NOT NULL))
);

-- ── §4.2.2 ──
CREATE TABLE plan_prices (
  price_id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  plan               plan_code NOT NULL,
  interval           billing_interval NOT NULL,
  currency           char(3) NOT NULL,
  unit_amount_minor  bigint NOT NULL CHECK (unit_amount_minor > 0),   -- per seat per interval
  provider           text NOT NULL CHECK (provider IN ('stripe','manual')),
  provider_price_id  text,                                            -- Stripe Price id; NULL for 'manual'
  effective_from     timestamptz NOT NULL,
  effective_to       timestamptz,                                     -- NULL = current
  created_by         uuid NOT NULL,                                   -- operator sub
  reason             text NOT NULL CHECK (length(reason) BETWEEN 10 AND 1000),
  created_at         timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT chk_pp_provider_ref CHECK (provider = 'manual' OR provider_price_id IS NOT NULL),
  CONSTRAINT chk_pp_range CHECK (effective_to IS NULL OR effective_to > effective_from),
  CONSTRAINT excl_pp_overlap EXCLUDE USING gist (
    plan WITH =, interval WITH =, currency WITH =, provider WITH =,
    tstzrange(effective_from, effective_to) WITH &&)
);

-- ── §4.2.3 ──
CREATE TABLE token_packs (
  pack_code           text NOT NULL CHECK (pack_code ~ '^[a-z0-9_]{3,40}$'),   -- e.g. 'tokens_1m', 'tokens_5m'
  resource            text NOT NULL DEFAULT 'llm_tokens' CHECK (resource = 'llm_tokens'),  -- CD-55 scope: tokens only in v1
  tokens              bigint NOT NULL CHECK (tokens > 0),
  currency            char(3) NOT NULL,
  amount_minor        bigint NOT NULL CHECK (amount_minor > 0),
  provider            text NOT NULL,
  provider_price_id   text,
  monthly_cap_tokens  bigint CHECK (monthly_cap_tokens IS NULL OR monthly_cap_tokens > 0),  -- per tenant per UTC month; NULL = no cap (CD-55)
  active              boolean NOT NULL DEFAULT true,
  effective_from      timestamptz NOT NULL,
  effective_to        timestamptz,
  created_by          uuid NOT NULL,
  reason              text NOT NULL,
  PRIMARY KEY (pack_code, currency, effective_from)
);

-- ── §4.2.4 ──
CREATE TABLE subscriptions (
  billing_subscription_id   uuid PRIMARY KEY,                       -- UUID v7, Billing-owned, provider-neutral
  tenant_id                 uuid NOT NULL REFERENCES billing_accounts(tenant_id),
  provider                  text NOT NULL,
  provider_subscription_id  text,                                   -- replaced on reactivation after cancellation (§8.9)
  origin                    subscription_origin NOT NULL,
  plan                      plan_code NOT NULL,
  interval                  billing_interval NOT NULL,
  status                    subscription_status NOT NULL DEFAULT 'incomplete',
  auto_renew                boolean NOT NULL DEFAULT true,          -- false ⇔ provider cancel_at_period_end
  current_period_start      timestamptz,
  current_period_end        timestamptz,
  collection_paused         boolean NOT NULL DEFAULT false,         -- CD-24 (§8.10)
  collection_paused_by      text CHECK (collection_paused_by IN ('auto','operator')),
  payment_confirmed_at      timestamptz,                            -- first payment confirmed (incomplete → active)
  converted_at              timestamptz,                            -- TenantConverted applied; RP-4 window starts here
  past_due_since            timestamptz,
  cancelled_at              timestamptz,
  cancel_reason             cancel_reason,
  ended_at                  timestamptz,
  record_version            bigint NOT NULL DEFAULT 1 CHECK (record_version > 0),
  created_at                timestamptz NOT NULL DEFAULT now(),
  updated_at                timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT uq_sub_provider UNIQUE (provider, provider_subscription_id),
  CONSTRAINT chk_sub_past_due  CHECK ((status = 'past_due') = (past_due_since IS NOT NULL)),
  CONSTRAINT chk_sub_cancelled CHECK (status NOT IN ('cancelled') OR cancelled_at IS NOT NULL),
  CONSTRAINT chk_sub_ended     CHECK ((status = 'ended') = (ended_at IS NOT NULL)),
  CONSTRAINT chk_sub_active_period CHECK (status NOT IN ('active','past_due') OR current_period_end IS NOT NULL),
  CONSTRAINT chk_sub_pause     CHECK (collection_paused = (collection_paused_by IS NOT NULL))
);
CREATE UNIQUE INDEX uq_sub_one_live   ON subscriptions (tenant_id) WHERE status <> 'ended';           -- BIL-SUB-3
CREATE INDEX idx_sub_period_end       ON subscriptions (current_period_end) WHERE status IN ('active','past_due');
CREATE INDEX idx_sub_tenant_created   ON subscriptions (tenant_id, created_at DESC);

-- ── §4.2.5 ── (the view is migration 000003)
CREATE TABLE subscription_items (
  item_id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id                 uuid NOT NULL,
  billing_subscription_id   uuid NOT NULL REFERENCES subscriptions(billing_subscription_id),
  kind                      text NOT NULL CHECK (kind IN ('seats')),
  price_id                  uuid NOT NULL REFERENCES plan_prices(price_id),
  quantity                  int  NOT NULL CHECK (quantity BETWEEN 1 AND 100000),
  provider_item_id          text,
  scheduled_price_id        uuid REFERENCES plan_prices(price_id),  -- §8.16 renewal repricing
  scheduled_price_at        timestamptz,                            -- renewal instant R
  record_version            bigint NOT NULL DEFAULT 1,
  updated_at                timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT uq_si_kind UNIQUE (billing_subscription_id, kind),
  CONSTRAINT chk_si_scheduled CHECK ((scheduled_price_id IS NULL) = (scheduled_price_at IS NULL))
);

-- ── §4.2.6 ──
CREATE TABLE subscription_status_history (
  id                        bigserial PRIMARY KEY,
  tenant_id                 uuid NOT NULL,
  billing_subscription_id   uuid NOT NULL,
  from_status               subscription_status,
  to_status                 subscription_status NOT NULL,
  from_plan                 plan_code,
  to_plan                   plan_code,
  from_seats                int,
  to_seats                  int,
  cause                     text NOT NULL,   -- 'webhook:<provider type>' | 'api:<route id>' | 'operator:<route id>' | 'iam:<event type>' | 'job:<name>'
  payment_event_ref         text,            -- '<provider>:<provider_event_id>' when caused by a callback
  actor_id                  uuid NOT NULL,   -- tenant user, operator sub, or commercial-system 00000000-0000-0000-0000-0000000000c1
  occurred_at               timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_ssh_tenant ON subscription_status_history (tenant_id, occurred_at DESC);
-- append-only: billing_app gets SELECT, INSERT only (grants below)

-- ── §4.2.7 ──
CREATE TABLE checkout_sessions (
  session_id               uuid PRIMARY KEY,
  tenant_id                uuid NOT NULL,
  purpose                  text NOT NULL CHECK (purpose IN ('conversion','reactivation','payment_method_setup','addon_payment')),
  provider_session_id      text NOT NULL,
  billing_subscription_id  uuid REFERENCES subscriptions(billing_subscription_id),
  addon_purchase_id        uuid,
  requested_plan           plan_code,
  requested_interval       billing_interval,
  requested_seats          int CHECK (requested_seats IS NULL OR requested_seats BETWEEN 1 AND 1000),
  status                   text NOT NULL DEFAULT 'open' CHECK (status IN ('open','completed','expired')),
  requested_by             uuid NOT NULL,
  expires_at               timestamptz NOT NULL,
  created_at               timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT uq_cs_provider UNIQUE (provider_session_id)
);
CREATE UNIQUE INDEX uq_cs_one_open ON checkout_sessions (tenant_id, purpose)
  WHERE status = 'open' AND purpose IN ('conversion','reactivation');
CREATE INDEX idx_cs_expiry ON checkout_sessions (expires_at) WHERE status = 'open';

-- ── §4.2.8 ──
CREATE TABLE conversion_sagas (
  saga_id                  uuid PRIMARY KEY,
  tenant_id                uuid NOT NULL,
  billing_subscription_id  uuid NOT NULL UNIQUE REFERENCES subscriptions(billing_subscription_id),  -- BIL-SAGA-2
  kind                     saga_kind NOT NULL,
  state                    saga_state NOT NULL,
  payment_confirmed_at     timestamptz NOT NULL,          -- clock for the 30-minute alert (HLD §8.2 step 6)
  rp_idempotency_key       text,                          -- 'convert:<tenant_id>:<billing_subscription_id>' (trial, stub)
  attempts                 int NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  next_attempt_at          timestamptz,
  last_error_code          text,                          -- e.g. 'operation_in_progress', 'conversion_failed', 'timeout'
  last_error_at            timestamptz,
  rp_accepted_at           timestamptz,
  tenant_converted_event   uuid,                          -- envelope id of the matching TenantConverted
  confirmed_at             timestamptz,
  alerted_at               timestamptz,
  resolution               text CHECK (resolution IN ('retry','refund','resolved_manually','reverted')),
  resolution_reason        text,
  resolved_by              uuid,
  record_version           bigint NOT NULL DEFAULT 1,
  created_at               timestamptz NOT NULL DEFAULT now(),
  updated_at               timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT chk_saga_confirmed CHECK ((state = 'confirmed') <= (confirmed_at IS NOT NULL)),
  CONSTRAINT chk_saga_key CHECK (kind = 'direct_signup' OR rp_idempotency_key IS NOT NULL)
);
CREATE INDEX idx_saga_due     ON conversion_sagas (next_attempt_at) WHERE state = 'rp_convert_pending';
CREATE INDEX idx_saga_waiting ON conversion_sagas (payment_confirmed_at)
  WHERE state IN ('payment_confirmed','rp_convert_pending','awaiting_tenant_converted');

-- ── §4.2.9 ──
CREATE TABLE plan_change_requests (
  change_id                uuid PRIMARY KEY,
  tenant_id                uuid NOT NULL,
  billing_subscription_id  uuid NOT NULL REFERENCES subscriptions(billing_subscription_id),
  from_plan                plan_code NOT NULL,
  to_plan                  plan_code NOT NULL,
  direction                text NOT NULL CHECK (direction IN ('upgrade','downgrade')),
  status                   change_status NOT NULL,
  effective_at             timestamptz,                 -- upgrade: payment confirmation; downgrade: current_period_end
  amount_minor             bigint,                      -- upgrade proration amount
  currency                 char(3),
  i18_checked_at           timestamptz,                 -- last I-18 call (downgrade)
  blocker_codes            text[],                      -- I-18 blockers when status = 'blocked' (e.g. {sso_idp_configured})
  provider_invoice_id      text,                        -- upgrade
  provider_schedule_id     text,                        -- downgrade
  client_action            jsonb,                       -- {"type":"3ds","client_secret":…} until resolved; cleared after
  requested_by             uuid NOT NULL,
  idempotency_key          text NOT NULL,
  created_at               timestamptz NOT NULL DEFAULT now(),
  updated_at               timestamptz NOT NULL DEFAULT now(),
  CHECK (from_plan <> to_plan),
  CONSTRAINT uq_pcr_key UNIQUE (tenant_id, idempotency_key)
);
CREATE UNIQUE INDEX uq_pcr_one_open ON plan_change_requests (tenant_id) WHERE status IN ('pending_payment','scheduled');
CREATE INDEX idx_pcr_due ON plan_change_requests (effective_at) WHERE status = 'scheduled';

-- ── §4.2.10 ──
CREATE TABLE seat_purchases (
  purchase_id              uuid PRIMARY KEY,
  tenant_id                uuid NOT NULL,
  billing_subscription_id  uuid NOT NULL REFERENCES subscriptions(billing_subscription_id),
  reason                   text NOT NULL DEFAULT 'purchase' CHECK (reason IN ('purchase','renewal_topup')),
  seats_added              int NOT NULL CHECK (seats_added BETWEEN 1 AND 1000),
  seats_before             int NOT NULL,
  seats_after              int NOT NULL,
  proration_instant        timestamptz NOT NULL,        -- instant the preview was priced at; reused for the charge
  amount_minor             bigint NOT NULL CHECK (amount_minor >= 0),
  currency                 char(3) NOT NULL,
  amount_source            text NOT NULL CHECK (amount_source IN ('provider_preview','billing_calculator')),
  -- Fallback calculator inputs and unrounded result (§8.5.3, BL-OQ-6); NULL when the provider prorates
  unit_amount_minor        bigint,
  remaining_seconds        bigint,
  period_seconds           bigint,
  amount_unrounded_minor   numeric(38,12),
  -- Below the provider minimum (free_until_renewal): amount not charged, billed from renewal
  deferred_amount_minor    bigint CHECK (deferred_amount_minor > 0),
  deferral_reason          text CHECK (deferral_reason IN ('below_provider_minimum')),
  status                   purchase_status NOT NULL,
  provider_invoice_id      text,
  client_action            jsonb,
  requested_by             uuid NOT NULL,
  idempotency_key          text NOT NULL,
  confirmed_at             timestamptz,
  created_at               timestamptz NOT NULL DEFAULT now(),
  updated_at               timestamptz NOT NULL DEFAULT now(),
  CHECK (seats_after = seats_before + seats_added),
  CONSTRAINT chk_sp_calc_inputs CHECK (
    (amount_source = 'billing_calculator') =
    (unit_amount_minor IS NOT NULL AND remaining_seconds IS NOT NULL
     AND period_seconds > 0 AND amount_unrounded_minor IS NOT NULL)),               -- BIL-SEAT-5
  CONSTRAINT chk_sp_deferral CHECK (
    (deferral_reason IS NULL AND deferred_amount_minor IS NULL)
    OR (deferral_reason IS NOT NULL AND deferred_amount_minor IS NOT NULL
        AND amount_minor = 0 AND amount_source = 'billing_calculator')),
  CONSTRAINT uq_sp_key UNIQUE (tenant_id, idempotency_key)
);
CREATE UNIQUE INDEX uq_sp_one_open ON seat_purchases (billing_subscription_id)
  WHERE status IN ('pending_payment','requires_action');                                    -- BIL-SEAT-1
CREATE INDEX idx_sp_expiry ON seat_purchases (created_at) WHERE status IN ('pending_payment','requires_action');

-- ── §4.2.11 ──
CREATE TABLE seat_change_schedules (
  schedule_id              uuid PRIMARY KEY,
  tenant_id                uuid NOT NULL,
  billing_subscription_id  uuid NOT NULL REFERENCES subscriptions(billing_subscription_id),
  target_seats             int NOT NULL CHECK (target_seats >= 1),
  effective_at             timestamptz NOT NULL,          -- the renewal it applies at (current_period_end at scheduling)
  status                   schedule_status NOT NULL DEFAULT 'scheduled',
  seats_in_use_at_apply    int,                           -- I-11 active_users + pending_invitations at application
  renewal_seats            int,                           -- max(target_seats, seats_in_use) (BIL-SEAT-3)
  final_seats              int,                           -- after the renewal top-up check (§8.6 step 4)
  provider_schedule_id     text,
  skip_reason              text CHECK (skip_reason IN ('iam_unavailable','subscription_not_active','superseded')),
  requested_by             uuid NOT NULL,
  record_version           bigint NOT NULL DEFAULT 1,
  created_at               timestamptz NOT NULL DEFAULT now(),
  updated_at               timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uq_scs_one_open ON seat_change_schedules (billing_subscription_id)
  WHERE status IN ('scheduled','applying');
CREATE INDEX idx_scs_due ON seat_change_schedules (effective_at) WHERE status IN ('scheduled','applying');

-- ── §4.2.12 ──
CREATE TABLE quota_addon_purchases (
  purchase_id              uuid PRIMARY KEY,              -- string form is billing_ref in TenantQuotaAddOnChanged
  tenant_id                uuid NOT NULL,
  resource                 text NOT NULL DEFAULT 'llm_tokens' CHECK (resource = 'llm_tokens'),
  pack_code                text NOT NULL,
  pack_quantity            int  NOT NULL CHECK (pack_quantity BETWEEN 1 AND 100),
  tokens_total             bigint NOT NULL CHECK (tokens_total > 0),
  amount_minor             bigint NOT NULL CHECK (amount_minor > 0),
  currency                 char(3) NOT NULL,
  status                   purchase_status NOT NULL,
  provider_invoice_id      text,
  client_action            jsonb,
  requested_by             uuid NOT NULL,
  idempotency_key          text NOT NULL,
  confirmed_at             timestamptz,                   -- payment confirmation = purchased_at (12-month expiry basis)
  event_id                 uuid,                          -- envelope id of the emitted TenantQuotaAddOnChanged
  created_at               timestamptz NOT NULL DEFAULT now(),
  updated_at               timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT uq_qap_key UNIQUE (tenant_id, idempotency_key),
  CONSTRAINT chk_qap_confirmed CHECK ((status = 'confirmed') = (confirmed_at IS NOT NULL))
);
CREATE INDEX idx_qap_month ON quota_addon_purchases (tenant_id, confirmed_at) WHERE status = 'confirmed';
CREATE INDEX idx_qap_expiry ON quota_addon_purchases (created_at) WHERE status IN ('pending_payment','requires_action');

-- ── §4.2.13 ──
CREATE TABLE invoices (
  invoice_id           uuid PRIMARY KEY,
  tenant_id            uuid NOT NULL,
  provider             text NOT NULL,
  provider_invoice_id  text NOT NULL,
  number               text,
  kind                 invoice_kind NOT NULL,
  status               invoice_status NOT NULL,
  currency             char(3) NOT NULL,
  amount_due_minor     bigint NOT NULL,
  amount_paid_minor    bigint NOT NULL DEFAULT 0,
  amount_refunded_minor bigint NOT NULL DEFAULT 0,
  period_start         timestamptz,
  period_end           timestamptz,
  hosted_invoice_url   text,                               -- provider-hosted page (no card data)
  issued_at            timestamptz,
  paid_at              timestamptz,
  provider_updated_at  timestamptz NOT NULL,               -- ordering guard for the mirror (BIL-PAY-4)
  -- CD-24 (§8.10): draft created while collection was paused; needs an operator decision (B-O16)
  held_for_review      boolean NOT NULL DEFAULT false,
  disposition          text CHECK (disposition IN ('finalize','void')),
  disposition_reason   text CHECK (length(disposition_reason) BETWEEN 10 AND 1000),
  disposed_by          uuid,                                -- operator sub
  disposed_at          timestamptz,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT uq_inv_provider UNIQUE (provider, provider_invoice_id),
  CHECK (amount_refunded_minor <= amount_paid_minor),
  CONSTRAINT chk_inv_disposition CHECK (
    (disposition IS NULL AND disposition_reason IS NULL AND disposed_by IS NULL AND disposed_at IS NULL)
    OR (held_for_review AND disposition IS NOT NULL AND disposition_reason IS NOT NULL
        AND disposed_by IS NOT NULL AND disposed_at IS NOT NULL))                         -- BIL-SUB-7
);
CREATE INDEX idx_inv_tenant_issued ON invoices (tenant_id, issued_at DESC, invoice_id);
CREATE INDEX idx_inv_held ON invoices (tenant_id) WHERE held_for_review AND disposition IS NULL;

-- ── §4.2.14 ──
CREATE TABLE invoice_links (
  invoice_id   uuid NOT NULL REFERENCES invoices(invoice_id),
  tenant_id    uuid NOT NULL,
  ref_type     text NOT NULL CHECK (ref_type IN ('subscription','seat_purchase','plan_change','quota_addon_purchase','saga','adjustment')),
  ref_id       uuid NOT NULL,
  PRIMARY KEY (invoice_id, ref_type, ref_id)
);
CREATE INDEX idx_il_ref ON invoice_links (ref_type, ref_id);

-- ── §4.2.15 ──
CREATE TABLE payment_event_ledger (
  provider             text NOT NULL,
  provider_event_id    text NOT NULL,
  event_type           text NOT NULL,                     -- provider type, e.g. 'invoice.paid'
  normalised_kind      text NOT NULL,                     -- domain.PaymentEvent kind (§7.4)
  tenant_id            uuid,                              -- NULL until resolved from the provider customer
  provider_object_ref  text,                              -- 'sub_…', 'in_…', 'pi_…', 'cs_…', 'dp_…'
  livemode             boolean NOT NULL,
  provider_created_at  timestamptz NOT NULL,
  received_at          timestamptz NOT NULL DEFAULT now(),
  payload              jsonb NOT NULL,                    -- allow-listed fields only (BIL-PCI-3)
  status               ledger_status NOT NULL DEFAULT 'received',
  attempts             int NOT NULL DEFAULT 0,
  next_attempt_at      timestamptz NOT NULL DEFAULT now(),
  claimed_until        timestamptz,
  processed_at         timestamptz,
  outcome              text,                              -- 'applied:<transition>' | 'ignored:<reason>' | 'failed:<code>'
  PRIMARY KEY (provider, provider_event_id)
);
CREATE INDEX idx_pel_work   ON payment_event_ledger (next_attempt_at) WHERE status IN ('received','failed');
CREATE INDEX idx_pel_tenant ON payment_event_ledger (tenant_id, provider_created_at);
CREATE INDEX idx_pel_recv   ON payment_event_ledger (received_at);

-- ── §4.2.16 ──
CREATE TABLE adjustments (
  adjustment_id    uuid PRIMARY KEY,
  tenant_id        uuid NOT NULL,
  kind             text NOT NULL CHECK (kind IN ('credit','debit')),
  amount_minor     bigint NOT NULL CHECK (amount_minor > 0),
  currency         char(3) NOT NULL,
  description      text NOT NULL CHECK (length(description) BETWEEN 3 AND 500),   -- shown on the next invoice
  reason           text NOT NULL CHECK (length(reason) BETWEEN 10 AND 1000),      -- internal, audited
  provider_ref     text,                                   -- customer balance transaction id
  actor_id         uuid NOT NULL,
  idempotency_key  text NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT uq_adj_key UNIQUE (tenant_id, idempotency_key)
);

-- ── §4.2.17 ──
CREATE TABLE refunds (
  refund_id           uuid PRIMARY KEY,
  tenant_id           uuid NOT NULL,
  invoice_id          uuid NOT NULL REFERENCES invoices(invoice_id),
  amount_minor        bigint NOT NULL CHECK (amount_minor > 0),
  currency            char(3) NOT NULL,
  category            text NOT NULL CHECK (category IN ('billing_error','duplicate_charge','fraud','goodwill','signup_failed','conversion_reversal','other')),
  reason              text NOT NULL CHECK (length(reason) BETWEEN 10 AND 1000),
  status              text NOT NULL CHECK (status IN ('pending','succeeded','failed')),
  provider_refund_id  text,
  addon_purchase_id   uuid REFERENCES quota_addon_purchases(purchase_id), -- set when the invoice is a token add-on invoice
  addon_tokens        bigint CHECK (addon_tokens > 0),                     -- tokens reversed: floor(tokens_total × amount_minor / purchase amount_minor)
  addon_event_id      uuid,                                                -- envelope id of the emitted TenantQuotaAddOnChanged (reason refund)
  actor_id            uuid NOT NULL,
  idempotency_key     text NOT NULL,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT uq_ref_key UNIQUE (tenant_id, idempotency_key)
);

-- ── §4.2.18 ──
CREATE TABLE disputes (
  dispute_id            uuid PRIMARY KEY,
  tenant_id             uuid NOT NULL,
  provider              text NOT NULL,
  provider_dispute_id   text NOT NULL,
  kind                  text NOT NULL CHECK (kind IN ('dispute','early_fraud_warning')),
  payment_ref           text NOT NULL,                    -- provider charge / payment id
  invoice_id            uuid REFERENCES invoices(invoice_id),
  is_conversion_payment boolean NOT NULL,                 -- payment of the subscription's first invoice
  within_revert_window  boolean NOT NULL,                 -- opened ≤ 7 days after converted_at (CD-10)
  reason                text,                             -- provider reason (e.g. 'fraudulent')
  provider_status       text NOT NULL,
  revert_decision       text NOT NULL DEFAULT 'none' CHECK (revert_decision IN ('none','pending','reverted','declined')),
  decided_by            uuid,
  opened_at             timestamptz NOT NULL,
  updated_at            timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT uq_disp_provider UNIQUE (provider, provider_dispute_id)
);

-- ── §4.2.19 ──
CREATE TABLE idempotency_records (
  tenant_id       uuid NOT NULL,
  scope           text NOT NULL,              -- route id ('B-I1', 'B-P6', 'B-O3', …) or 'preview'
  key             text NOT NULL CHECK (length(key) BETWEEN 8 AND 200),
  request_hash    bytea NOT NULL,             -- SHA-256 of the canonical JSON body + route params
  state           text NOT NULL CHECK (state IN ('in_flight','completed')),
  status_code     int,
  response_body   jsonb,
  locked_until    timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now(),
  expires_at      timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, scope, key)
);
CREATE INDEX idx_idem_expiry ON idempotency_records (expires_at);

-- ── §4.2.20 ──
CREATE TABLE scheduled_emissions (
  emission_id      uuid PRIMARY KEY,
  tenant_id        uuid NOT NULL,
  event_type       text NOT NULL,
  payload_builder  text NOT NULL CHECK (payload_builder IN ('seats_after_conversion','seats_resync')),
  ref_id           uuid NOT NULL,              -- billing_subscription_id
  due_at           timestamptz NOT NULL,
  status           text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','emitted','cancelled')),
  emitted_event_id uuid,
  created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uq_se_one_pending ON scheduled_emissions (tenant_id, payload_builder, ref_id) WHERE status = 'pending';
CREATE INDEX idx_se_due ON scheduled_emissions (due_at) WHERE status = 'pending';

-- ── §4.2.21 ──
CREATE TABLE usage_period_records (
  tenant_id           uuid NOT NULL,
  period_start        timestamptz NOT NULL,
  period_end          timestamptz NOT NULL,
  tokens_total        bigint NOT NULL CHECK (tokens_total >= 0),
  api_requests_total  bigint NOT NULL CHECK (api_requests_total >= 0),
  addon_consumed      bigint NOT NULL DEFAULT 0 CHECK (addon_consumed >= 0),
  limits              jsonb NOT NULL,
  source_event_id     uuid NOT NULL,
  source_event_time   timestamptz NOT NULL,
  received_at         timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, period_start)
);

-- ── §4.2.22 ──
CREATE TABLE reconciliation_runs (
  run_id         uuid PRIMARY KEY,
  provider       text NOT NULL,
  window_start   timestamptz NOT NULL,
  window_end     timestamptz NOT NULL,
  auto_repair    boolean NOT NULL DEFAULT false,
  status         text NOT NULL CHECK (status IN ('queued','running','completed','failed')),
  started_by     uuid NOT NULL,                 -- operator sub
  claimed_by     text,                          -- pod name
  counts         jsonb,                         -- {"subscriptions":…, "invoices":…, "events":…, "findings":…, "repaired":…}
  error          text,
  created_at     timestamptz NOT NULL DEFAULT now(),
  started_at     timestamptz,
  finished_at    timestamptz,
  CHECK (window_end > window_start AND window_end - window_start <= interval '35 days')
);
CREATE UNIQUE INDEX uq_rr_one_running ON reconciliation_runs (provider) WHERE status IN ('queued','running');

CREATE TABLE reconciliation_findings (
  run_id         uuid NOT NULL REFERENCES reconciliation_runs(run_id),
  finding_id     bigserial,
  tenant_id      uuid,
  kind           text NOT NULL CHECK (kind IN ('missing_ledger_event','status_mismatch','seats_mismatch','plan_mismatch',
                                               'invoice_missing','invoice_mismatch','orphan_provider_object','unknown_customer')),
  billing_value  jsonb,
  provider_value jsonb,
  auto_repaired  boolean NOT NULL DEFAULT false,
  repair_note    text,
  PRIMARY KEY (run_id, finding_id)
);

-- ── §4.5 Triggers ──
CREATE FUNCTION touch_row() RETURNS trigger AS $$
BEGIN
  NEW.updated_at := now();
  IF TG_TABLE_NAME IN ('billing_accounts','subscriptions','subscription_items','conversion_sagas','seat_change_schedules') THEN
    NEW.record_version := OLD.record_version + 1;
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER trg_touch_billing_accounts BEFORE UPDATE ON billing_accounts
  FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION touch_row();
CREATE TRIGGER trg_touch_subscriptions BEFORE UPDATE ON subscriptions
  FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION touch_row();
CREATE TRIGGER trg_touch_subscription_items BEFORE UPDATE ON subscription_items
  FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION touch_row();
CREATE TRIGGER trg_touch_conversion_sagas BEFORE UPDATE ON conversion_sagas
  FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION touch_row();
CREATE TRIGGER trg_touch_plan_change_requests BEFORE UPDATE ON plan_change_requests
  FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION touch_row();
CREATE TRIGGER trg_touch_seat_purchases BEFORE UPDATE ON seat_purchases
  FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION touch_row();
CREATE TRIGGER trg_touch_seat_change_schedules BEFORE UPDATE ON seat_change_schedules
  FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION touch_row();
CREATE TRIGGER trg_touch_quota_addon_purchases BEFORE UPDATE ON quota_addon_purchases
  FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION touch_row();
CREATE TRIGGER trg_touch_invoices BEFORE UPDATE ON invoices
  FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION touch_row();
CREATE TRIGGER trg_touch_refunds BEFORE UPDATE ON refunds
  FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION touch_row();
CREATE TRIGGER trg_touch_disputes BEFORE UPDATE ON disputes
  FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION touch_row();

-- BIL-SUB-6: an account's currency is fixed while it has a non-ended subscription.
CREATE FUNCTION prevent_currency_change() RETURNS trigger AS $$
BEGIN
  IF NEW.currency IS DISTINCT FROM OLD.currency
     AND EXISTS (SELECT 1 FROM subscriptions s WHERE s.tenant_id = OLD.tenant_id AND s.status <> 'ended') THEN
    RAISE EXCEPTION 'billing_accounts.currency is fixed while a subscription is live (BIL-SUB-6)' USING ERRCODE = 'P0001';
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER trg_prevent_currency_change BEFORE UPDATE OF currency ON billing_accounts
  FOR EACH ROW EXECUTE FUNCTION prevent_currency_change();

-- ── §4.3 Row-Level Security (BIL-RLS-1) ──
ALTER TABLE billing_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing_accounts FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing_accounts
  USING      (rls_check_tenant(tenant_id, 'billing_accounts'))
  WITH CHECK (rls_check_tenant(tenant_id, 'billing_accounts'));
REVOKE ALL ON billing_accounts FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON billing_accounts TO billing_app;
GRANT SELECT ON billing_accounts TO admin_readonly;
ALTER TABLE subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscriptions FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON subscriptions
  USING      (rls_check_tenant(tenant_id, 'subscriptions'))
  WITH CHECK (rls_check_tenant(tenant_id, 'subscriptions'));
REVOKE ALL ON subscriptions FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON subscriptions TO billing_app;
GRANT SELECT ON subscriptions TO admin_readonly;
ALTER TABLE subscription_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscription_items FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON subscription_items
  USING      (rls_check_tenant(tenant_id, 'subscription_items'))
  WITH CHECK (rls_check_tenant(tenant_id, 'subscription_items'));
REVOKE ALL ON subscription_items FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON subscription_items TO billing_app;
GRANT SELECT ON subscription_items TO admin_readonly;
ALTER TABLE subscription_status_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscription_status_history FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON subscription_status_history
  USING      (rls_check_tenant(tenant_id, 'subscription_status_history'))
  WITH CHECK (rls_check_tenant(tenant_id, 'subscription_status_history'));
REVOKE ALL ON subscription_status_history FROM PUBLIC;
GRANT SELECT, INSERT ON subscription_status_history TO billing_app;
GRANT SELECT ON subscription_status_history TO admin_readonly;
ALTER TABLE checkout_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE checkout_sessions FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON checkout_sessions
  USING      (rls_check_tenant(tenant_id, 'checkout_sessions'))
  WITH CHECK (rls_check_tenant(tenant_id, 'checkout_sessions'));
REVOKE ALL ON checkout_sessions FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON checkout_sessions TO billing_app;
GRANT SELECT ON checkout_sessions TO admin_readonly;
ALTER TABLE conversion_sagas ENABLE ROW LEVEL SECURITY;
ALTER TABLE conversion_sagas FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON conversion_sagas
  USING      (rls_check_tenant(tenant_id, 'conversion_sagas'))
  WITH CHECK (rls_check_tenant(tenant_id, 'conversion_sagas'));
REVOKE ALL ON conversion_sagas FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON conversion_sagas TO billing_app;
GRANT SELECT ON conversion_sagas TO admin_readonly;
ALTER TABLE plan_change_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE plan_change_requests FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON plan_change_requests
  USING      (rls_check_tenant(tenant_id, 'plan_change_requests'))
  WITH CHECK (rls_check_tenant(tenant_id, 'plan_change_requests'));
REVOKE ALL ON plan_change_requests FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON plan_change_requests TO billing_app;
GRANT SELECT ON plan_change_requests TO admin_readonly;
ALTER TABLE seat_purchases ENABLE ROW LEVEL SECURITY;
ALTER TABLE seat_purchases FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON seat_purchases
  USING      (rls_check_tenant(tenant_id, 'seat_purchases'))
  WITH CHECK (rls_check_tenant(tenant_id, 'seat_purchases'));
REVOKE ALL ON seat_purchases FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON seat_purchases TO billing_app;
GRANT SELECT ON seat_purchases TO admin_readonly;
ALTER TABLE seat_change_schedules ENABLE ROW LEVEL SECURITY;
ALTER TABLE seat_change_schedules FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON seat_change_schedules
  USING      (rls_check_tenant(tenant_id, 'seat_change_schedules'))
  WITH CHECK (rls_check_tenant(tenant_id, 'seat_change_schedules'));
REVOKE ALL ON seat_change_schedules FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON seat_change_schedules TO billing_app;
GRANT SELECT ON seat_change_schedules TO admin_readonly;
ALTER TABLE quota_addon_purchases ENABLE ROW LEVEL SECURITY;
ALTER TABLE quota_addon_purchases FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON quota_addon_purchases
  USING      (rls_check_tenant(tenant_id, 'quota_addon_purchases'))
  WITH CHECK (rls_check_tenant(tenant_id, 'quota_addon_purchases'));
REVOKE ALL ON quota_addon_purchases FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON quota_addon_purchases TO billing_app;
GRANT SELECT ON quota_addon_purchases TO admin_readonly;
ALTER TABLE invoices ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoices FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON invoices
  USING      (rls_check_tenant(tenant_id, 'invoices'))
  WITH CHECK (rls_check_tenant(tenant_id, 'invoices'));
REVOKE ALL ON invoices FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON invoices TO billing_app;
GRANT SELECT ON invoices TO admin_readonly;
ALTER TABLE invoice_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoice_links FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON invoice_links
  USING      (rls_check_tenant(tenant_id, 'invoice_links'))
  WITH CHECK (rls_check_tenant(tenant_id, 'invoice_links'));
REVOKE ALL ON invoice_links FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON invoice_links TO billing_app;
GRANT SELECT ON invoice_links TO admin_readonly;
ALTER TABLE adjustments ENABLE ROW LEVEL SECURITY;
ALTER TABLE adjustments FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON adjustments
  USING      (rls_check_tenant(tenant_id, 'adjustments'))
  WITH CHECK (rls_check_tenant(tenant_id, 'adjustments'));
REVOKE ALL ON adjustments FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON adjustments TO billing_app;
GRANT SELECT ON adjustments TO admin_readonly;
ALTER TABLE refunds ENABLE ROW LEVEL SECURITY;
ALTER TABLE refunds FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON refunds
  USING      (rls_check_tenant(tenant_id, 'refunds'))
  WITH CHECK (rls_check_tenant(tenant_id, 'refunds'));
REVOKE ALL ON refunds FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON refunds TO billing_app;
GRANT SELECT ON refunds TO admin_readonly;
ALTER TABLE disputes ENABLE ROW LEVEL SECURITY;
ALTER TABLE disputes FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON disputes
  USING      (rls_check_tenant(tenant_id, 'disputes'))
  WITH CHECK (rls_check_tenant(tenant_id, 'disputes'));
REVOKE ALL ON disputes FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON disputes TO billing_app;
GRANT SELECT ON disputes TO admin_readonly;
ALTER TABLE idempotency_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_records FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON idempotency_records
  USING      (rls_check_tenant(tenant_id, 'idempotency_records'))
  WITH CHECK (rls_check_tenant(tenant_id, 'idempotency_records'));
REVOKE ALL ON idempotency_records FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON idempotency_records TO billing_app;
GRANT SELECT ON idempotency_records TO admin_readonly;
ALTER TABLE scheduled_emissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE scheduled_emissions FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON scheduled_emissions
  USING      (rls_check_tenant(tenant_id, 'scheduled_emissions'))
  WITH CHECK (rls_check_tenant(tenant_id, 'scheduled_emissions'));
REVOKE ALL ON scheduled_emissions FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON scheduled_emissions TO billing_app;
GRANT SELECT ON scheduled_emissions TO admin_readonly;
ALTER TABLE usage_period_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE usage_period_records FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON usage_period_records
  USING      (rls_check_tenant(tenant_id, 'usage_period_records'))
  WITH CHECK (rls_check_tenant(tenant_id, 'usage_period_records'));
REVOKE ALL ON usage_period_records FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON usage_period_records TO billing_app;
GRANT SELECT ON usage_period_records TO admin_readonly;

-- ── Grants (§4.1, §4.3): explicit, never assumed through ownership ──
-- Global price book: billing_app reads; billing_system writes (B-O11, B-O12).
REVOKE ALL ON plan_prices, token_packs FROM PUBLIC;
GRANT SELECT ON plan_prices, token_packs TO billing_app, admin_readonly;
GRANT SELECT, INSERT, UPDATE ON plan_prices, token_packs TO billing_system;

-- billing_system only: ledger and reconciliation (billing_app has no grant).
REVOKE ALL ON payment_event_ledger, reconciliation_runs, reconciliation_findings FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE ON payment_event_ledger, reconciliation_runs, reconciliation_findings TO billing_system;
GRANT USAGE, SELECT ON SEQUENCE reconciliation_findings_finding_id_seq TO billing_system;
GRANT SELECT ON payment_event_ledger, reconciliation_runs, reconciliation_findings TO admin_readonly;

-- billing_system (BYPASSRLS) reads tenant tables for webhook pre-resolution,
-- cron tenant listing and reconciliation (§4.3 GUC binding table, BIL-RLS-3).
GRANT SELECT ON billing_accounts, subscriptions, subscription_items, invoices, conversion_sagas,
  seat_change_schedules, plan_change_requests, seat_purchases, quota_addon_purchases,
  scheduled_emissions, idempotency_records, checkout_sessions, disputes TO billing_system;

GRANT USAGE, SELECT ON SEQUENCE subscription_status_history_id_seq TO billing_app;

-- RLS helper functions: callable by the roles whose queries evaluate the policy.
REVOKE ALL ON FUNCTION app_tenant_id(), log_rls_violation(text, uuid, text), rls_check_tenant(uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_tenant_id(), rls_check_tenant(uuid, text) TO billing_app, billing_system, admin_readonly, billing_audit_drainer;
GRANT SELECT ON rls_violation_log TO admin_readonly;

-- platform-events tables (outbox.ApplySchema, inbox.ApplySchema run first, §4.4).
GRANT SELECT, INSERT, UPDATE, DELETE ON outbox_events, outbox_dead_letters TO billing_app;
GRANT SELECT, INSERT, DELETE ON processed_events TO billing_app;
-- Outbox sequences only (named by platform-events), not every sequence in the schema.
DO $$
DECLARE r record;
BEGIN
  FOR r IN SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
           WHERE n.nspname = 'public' AND c.relkind = 'S' AND c.relname LIKE 'outbox\_%' LOOP
    EXECUTE format('GRANT USAGE, SELECT ON SEQUENCE public.%I TO billing_app', r.relname);
  END LOOP;
END $$;
