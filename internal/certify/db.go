package certify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// DB is the throwaway database of one run (§14.7): created on a scratch
// Postgres server, migrated with the service's own migrations, used through
// the service's roles, and dropped at the end.
type DB struct {
	Name                  string
	App, System, Super    *pgcommon.Pool
	serverDSN, superDBDSN string
}

// dbName is the only shape a run database may have: it is interpolated
// into CREATE / DROP DATABASE (no bind parameters for identifiers).
var dbName = regexp.MustCompile(`^certify_[a-z0-9_]{1,50}$`)

var roles = []string{"billing_migrator", "billing_app", "billing_system", "billing_audit_drainer", "admin_readonly"}

// OpenDB creates database certify_<run> on serverDSN (a superuser DSN of a
// scratch server, never a shared environment), creates the five roles with
// a fresh password when missing, migrates, and opens the pools.
func OpenDB(ctx context.Context, serverDSN, runID string) (*DB, error) {
	name := "certify_" + strings.ToLower(strings.ReplaceAll(runID, "-", "_"))
	if !dbName.MatchString(name) {
		return nil, fmt.Errorf("run id %q gives an unsafe database name", runID)
	}
	pw := randomHex(16)
	stmts := []string{fmt.Sprintf(`CREATE DATABASE %q`, name)}
	for _, r := range roles {
		bypass := "NOBYPASSRLS"
		if r == "billing_system" {
			bypass = "BYPASSRLS"
		}
		stmts = append(stmts, fmt.Sprintf(`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '%[1]s') THEN CREATE ROLE %[1]s LOGIN NOSUPERUSER %[2]s; END IF;
			ALTER ROLE %[1]s PASSWORD '%[3]s'; END $$`, r, bypass, pw))
	}
	if err := execOn(ctx, serverDSN, stmts...); err != nil {
		return nil, fmt.Errorf("prepare scratch database: %w", err)
	}
	d := &DB{Name: name, serverDSN: serverDSN, superDBDSN: withDB(serverDSN, name, "", "")}
	if err := execOn(ctx, d.superDBDSN,
		`CREATE EXTENSION IF NOT EXISTS pgcrypto`, `CREATE EXTENSION IF NOT EXISTS btree_gist`,
		fmt.Sprintf(`GRANT ALL PRIVILEGES ON DATABASE %s TO billing_migrator`, name),
		`GRANT USAGE, CREATE ON SCHEMA public TO billing_migrator`,
		fmt.Sprintf(`GRANT CONNECT ON DATABASE %s TO billing_app, billing_system, billing_audit_drainer, admin_readonly`, name),
		`GRANT USAGE ON SCHEMA public TO billing_app, billing_system, billing_audit_drainer, admin_readonly`); err != nil {
		_ = d.Drop(ctx)
		return nil, err
	}
	if err := pgadapter.MigrateSchema(ctx, withDB(serverDSN, name, "billing_migrator", pw), nil); err != nil {
		_ = d.Drop(ctx)
		return nil, fmt.Errorf("migrate: %w", err)
	}
	mk := func(user, pool string, guc bool) (*pgcommon.Pool, error) {
		cfg := pgcommon.Config{DSN: withDB(serverDSN, name, user, pw), PoolName: pool, MaxConns: 6, PGBouncerMode: true}
		if user == "" {
			cfg.DSN, cfg.PGBouncerMode = d.superDBDSN, false
		}
		if guc {
			cfg.GUCProvider = pgcommon.GUCSetFromContext
		}
		return pgcommon.NewPool(ctx, cfg)
	}
	var err error
	if d.App, err = mk("billing_app", "app", true); err == nil {
		if d.System, err = mk("billing_system", "system", false); err == nil {
			d.Super, err = mk("", "certify_super", false)
		}
	}
	if err != nil {
		_ = d.Drop(ctx)
		return nil, err
	}
	return d, nil
}

// Drop closes the pools and drops the database.
func (d *DB) Drop(ctx context.Context) error {
	for _, p := range []*pgcommon.Pool{d.App, d.System, d.Super} {
		if p != nil {
			p.Close()
		}
	}
	return execOn(ctx, d.serverDSN, fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, d.Name))
}

// Exec runs setup statements as the superuser in the run's database.
func (d *DB) Exec(ctx context.Context, sql string, args ...any) error {
	return pgcommon.RunInTx(ctx, d.Super, pgcommon.TxOptions{}, func(ctx context.Context, tx pgcommon.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

// Scalar reads one value as the superuser (assertions).
func Scalar[T any](ctx context.Context, d *DB, sql string, args ...any) (T, error) {
	var v T
	err := pgcommon.RunInTx(ctx, d.Super, pgcommon.TxOptions{}, func(ctx context.Context, tx pgcommon.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&v)
	})
	return v, err
}

// execOn runs statements outside a transaction (CREATE / DROP DATABASE) on
// a one-connection pgcommon pool (BIL-OPS-1).
func execOn(ctx context.Context, dsn string, stmts ...string) error {
	p, err := pgcommon.NewPool(ctx, pgcommon.Config{DSN: dsn, PoolName: "certify_admin", MaxConns: 1})
	if err != nil {
		return err
	}
	defer p.Close()
	return p.WithConn(ctx, func(ctx context.Context, c *pgcommon.Conn) error {
		for _, s := range stmts {
			if _, err := c.Exec(ctx, s); err != nil {
				return fmt.Errorf("%s: %w", strings.Fields(s)[0], err)
			}
		}
		return nil
	})
}

func withDB(dsn, db, user, pw string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + db
	if user != "" {
		u.User = url.UserPassword(user, pw)
	}
	return u.String()
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Certified reports whether a passed (or exempt) record exists for the
// provider, adapter version and suite version — the release gate (§14.6,
// BIL-PROV-2). dsn is a read-only role on the environment's database.
func Certified(ctx context.Context, dsn, provider, adapterVersion, suite string) (bool, error) {
	p, err := pgcommon.NewPool(ctx, pgcommon.Config{DSN: dsn, PoolName: "certify_check", MaxConns: 1})
	if err != nil {
		return false, err
	}
	defer p.Close()
	var n int
	err = p.WithConn(ctx, func(ctx context.Context, c *pgcommon.Conn) error {
		return c.QueryRow(ctx, `SELECT count(*) FROM provider_certifications
			WHERE provider = $1 AND adapter_version = $2 AND suite_version = $3 AND status IN ('passed','exempt')`,
			provider, adapterVersion, suite).Scan(&n)
	})
	return n > 0, err
}

// DSN returns the run database's DSN for role (tests and the check gate).
func (d *DB) DSN() string { return d.superDBDSN }
