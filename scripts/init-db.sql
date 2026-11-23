-- Local dev bootstrap for the billing database (LLD §4.1, §12).
--
-- Creates the roles the LLD names. In shared environments the platform DB
-- bootstrap creates them; this file only serves docker-compose. None is a
-- superuser: a superuser bypasses RLS, so the app must never be one.
--   billing_migrator       owns the schema; used only by cmd/migrate
--   billing_app            NOBYPASSRLS; handlers, consumers, workers
--   billing_system         BYPASSRLS; webhook pre-resolution, cron listing, reconciliation, price book
--   billing_audit_drainer  NOBYPASSRLS; platform-audit drainer (policy audit_drainer_all, migration 000005)
--   admin_readonly         NOBYPASSRLS; support queries

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'billing_migrator') THEN
    CREATE ROLE billing_migrator LOGIN PASSWORD 'devpassword' NOSUPERUSER NOBYPASSRLS;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'billing_app') THEN
    CREATE ROLE billing_app LOGIN PASSWORD 'devpassword' NOSUPERUSER NOBYPASSRLS;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'billing_system') THEN
    CREATE ROLE billing_system LOGIN PASSWORD 'devpassword' NOSUPERUSER BYPASSRLS;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'billing_audit_drainer') THEN
    CREATE ROLE billing_audit_drainer LOGIN PASSWORD 'devpassword' NOSUPERUSER NOBYPASSRLS;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
    CREATE ROLE admin_readonly LOGIN PASSWORD 'devpassword' NOSUPERUSER NOBYPASSRLS;
  END IF;
END
$$;

-- Extensions used by 000001 (gen_random_uuid, the price-row exclusion constraint).
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS btree_gist;

-- Postgres 15+ no longer lets PUBLIC create in schema public.
GRANT ALL PRIVILEGES ON DATABASE billing TO billing_migrator;
GRANT USAGE, CREATE ON SCHEMA public TO billing_migrator;
GRANT CONNECT ON DATABASE billing TO billing_app, billing_system, billing_audit_drainer, admin_readonly;
GRANT USAGE ON SCHEMA public TO billing_app, billing_system, billing_audit_drainer, admin_readonly;
