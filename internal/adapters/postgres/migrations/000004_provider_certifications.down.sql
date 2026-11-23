-- 000004 down
-- Generated from docs/lld/commercial-lld-billing-service.md §4 (rev 0.31). Table and
-- constraint SQL is copied verbatim from the LLD; change the LLD first.

DROP TRIGGER IF EXISTS trg_pc_guard ON provider_certifications;
DROP TABLE IF EXISTS provider_certifications;
DROP FUNCTION IF EXISTS provider_certifications_guard();
DROP TYPE IF EXISTS certification_status;
