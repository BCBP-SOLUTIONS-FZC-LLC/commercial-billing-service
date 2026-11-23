-- 000003_views: v_subscription_summary, security_invoker so RLS applies (§4.2.5)
-- Generated from docs/lld/commercial-lld-billing-service.md §4 (rev 0.31). Table and
-- constraint SQL is copied verbatim from the LLD; change the LLD first.

CREATE VIEW v_subscription_summary WITH (security_invoker = true) AS
SELECT s.*, i.quantity AS licensed_seats, i.price_id, p.unit_amount_minor, p.currency
FROM subscriptions s
JOIN subscription_items i ON i.billing_subscription_id = s.billing_subscription_id AND i.kind = 'seats'
JOIN plan_prices p ON p.price_id = i.price_id;          -- security_invoker = true (RLS applies)
GRANT SELECT ON v_subscription_summary TO billing_app, admin_readonly;
