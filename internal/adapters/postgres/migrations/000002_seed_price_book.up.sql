-- 000002_seed_price_book: plan_prices seed (§4.2.2, HLD §6.3) as provider = 'manual' rows only
-- (the Phase 1 operator stub's prices). Stripe rows need a provider_price_id
-- (chk_pp_provider_ref) and are created per environment through B-O11 /
-- price-book-sync. Final Phase 1 commercial prices are loaded as data
-- (BL-OQ-22); these rows are the stub/dev seed.
-- Generated from docs/lld/commercial-lld-billing-service.md §4 (rev 0.31). Table and
-- constraint SQL is copied verbatim from the LLD; change the LLD first.

INSERT INTO plan_prices (plan, interval, currency, unit_amount_minor, provider, provider_price_id, effective_from, created_by, reason) VALUES
  ('starter', 'month', 'USD', 3500, 'manual', NULL, '2026-01-01T00:00:00Z', '00000000-0000-0000-0000-0000000000c1', 'Phase 1 seed for the manual stub (HLD §6.3, LLD §4.2.2)'),
  ('starter', 'year', 'USD', 35700, 'manual', NULL, '2026-01-01T00:00:00Z', '00000000-0000-0000-0000-0000000000c1', 'Phase 1 seed for the manual stub (HLD §6.3, LLD §4.2.2)'),
  ('pro', 'month', 'USD', 6900, 'manual', NULL, '2026-01-01T00:00:00Z', '00000000-0000-0000-0000-0000000000c1', 'Phase 1 seed for the manual stub (HLD §6.3, LLD §4.2.2)'),
  ('pro', 'year', 'USD', 70380, 'manual', NULL, '2026-01-01T00:00:00Z', '00000000-0000-0000-0000-0000000000c1', 'Phase 1 seed for the manual stub (HLD §6.3, LLD §4.2.2)'),
  ('enterprise', 'month', 'USD', 17900, 'manual', NULL, '2026-01-01T00:00:00Z', '00000000-0000-0000-0000-0000000000c1', 'Phase 1 seed for the manual stub (HLD §6.3, LLD §4.2.2)'),
  ('enterprise', 'year', 'USD', 182580, 'manual', NULL, '2026-01-01T00:00:00Z', '00000000-0000-0000-0000-0000000000c1', 'Phase 1 seed for the manual stub (HLD §6.3, LLD §4.2.2)');
