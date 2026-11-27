-- 000001_initial_schema down
-- Generated from docs/lld/commercial-lld-billing-service.md §4 (rev 0.31). Table and
-- constraint SQL is copied verbatim from the LLD; change the LLD first.

DROP TABLE IF EXISTS reconciliation_findings;
DROP TABLE IF EXISTS reconciliation_runs;
DROP TABLE IF EXISTS usage_period_records;
DROP TABLE IF EXISTS scheduled_emissions;
DROP TABLE IF EXISTS idempotency_records;
DROP TABLE IF EXISTS disputes;
DROP TABLE IF EXISTS refunds;
DROP TABLE IF EXISTS adjustments;
DROP TABLE IF EXISTS payment_event_ledger;
DROP TABLE IF EXISTS invoice_links;
DROP TABLE IF EXISTS invoices;
DROP TABLE IF EXISTS quota_addon_purchases;
DROP TABLE IF EXISTS seat_change_schedules;
DROP TABLE IF EXISTS seat_purchases;
DROP TABLE IF EXISTS plan_change_requests;
DROP TABLE IF EXISTS conversion_sagas;
DROP TABLE IF EXISTS checkout_sessions;
DROP TABLE IF EXISTS subscription_status_history;
DROP TABLE IF EXISTS subscription_items;
DROP TABLE IF EXISTS subscriptions;
DROP TABLE IF EXISTS token_packs;
DROP TABLE IF EXISTS plan_prices;
DROP TABLE IF EXISTS billing_accounts;
DROP TABLE IF EXISTS rls_violation_log;
DROP FUNCTION IF EXISTS prevent_currency_change();
DROP FUNCTION IF EXISTS touch_row();
DROP FUNCTION IF EXISTS rls_check_tenant(uuid, text);
DROP FUNCTION IF EXISTS log_rls_violation(text, uuid, text);
DROP FUNCTION IF EXISTS app_tenant_id();
DROP TYPE IF EXISTS cancel_reason;
DROP TYPE IF EXISTS ledger_status;
DROP TYPE IF EXISTS invoice_status;
DROP TYPE IF EXISTS invoice_kind;
DROP TYPE IF EXISTS change_status;
DROP TYPE IF EXISTS schedule_status;
DROP TYPE IF EXISTS purchase_status;
DROP TYPE IF EXISTS saga_state;
DROP TYPE IF EXISTS saga_kind;
DROP TYPE IF EXISTS iam_lifecycle;
DROP TYPE IF EXISTS subscription_origin;
DROP TYPE IF EXISTS subscription_status;
DROP TYPE IF EXISTS billing_interval;
DROP TYPE IF EXISTS plan_code;
