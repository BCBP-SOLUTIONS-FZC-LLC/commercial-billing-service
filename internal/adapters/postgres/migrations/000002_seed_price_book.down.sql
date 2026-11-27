-- 000002 down
-- Generated from docs/lld/commercial-lld-billing-service.md §4 (rev 0.31). Table and
-- constraint SQL is copied verbatim from the LLD; change the LLD first.

DELETE FROM plan_prices WHERE provider = 'manual' AND reason = 'Phase 1 seed for the manual stub (HLD §6.3, LLD §4.2.2)';
