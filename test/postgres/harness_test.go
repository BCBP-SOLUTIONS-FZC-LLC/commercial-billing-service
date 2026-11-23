//go:build integration

// Package postgres_test runs Billing's schema tests against a real Postgres
// (LLD §14.2, §14.5): one container per package, the roles of
// scripts/init-db.sql, and migrations applied as billing_migrator exactly as
// cmd/migrate does, so grants are exercised as in production.
package postgres_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// Pools shared by every test in the package.
var (
	superPool  *pgcommon.Pool // postgres superuser: seeding only (bypasses RLS)
	appPool    *pgcommon.Pool // billing_app: RLS-bound, GUC bound per transaction
	systemPool *pgcommon.Pool // billing_system: BYPASSRLS
	readPool   *pgcommon.Pool // admin_readonly
	migrateDSN string
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("billing"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("testpassword"),
		tcpostgres.WithInitScripts("../../scripts/init-db.sql"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(90*time.Second)),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres container: %v\n", err)
		return 1
	}
	defer func() { _ = container.Terminate(ctx) }()

	superDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "connection string: %v\n", err)
		return 1
	}
	migrateDSN = withUser(superDSN, "billing_migrator", "devpassword")
	if err := pgadapter.MigrateSchema(ctx, migrateDSN, nil); err != nil {
		fmt.Fprintf(os.Stderr, "migrate as billing_migrator: %v\n", err)
		return 1
	}

	mk := func(dsn string, guc bool, name string) *pgcommon.Pool {
		cfg := pgcommon.Config{DSN: dsn, PoolName: name, MaxConns: 4}
		if guc {
			cfg.GUCProvider = pgcommon.GUCSetFromContext
		}
		p, err := pgcommon.NewPool(ctx, cfg)
		if err != nil {
			panic(fmt.Sprintf("pool %s: %v", name, err))
		}
		return p
	}
	superPool = mk(superDSN, false, "super")
	appPool = mk(withUser(superDSN, "billing_app", "devpassword"), true, "app")
	systemPool = mk(withUser(superDSN, "billing_system", "devpassword"), false, "system")
	readPool = mk(withUser(superDSN, "admin_readonly", "devpassword"), false, "readonly")
	defer func() {
		for _, p := range []*pgcommon.Pool{superPool, appPool, systemPool, readPool} {
			p.Close()
		}
	}()
	return m.Run()
}

func withUser(dsn, user, password string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		panic(err)
	}
	u.User = url.UserPassword(user, password)
	return u.String()
}

// asTenant returns ctx with app.tenant_id bound for the app pool.
func asTenant(ctx context.Context, tenantID string) context.Context {
	return pgcommon.WithGUCSet(ctx, domain.GUCSet{TenantID: tenantID})
}

// exec runs one statement in its own transaction on pool.
func exec(ctx context.Context, pool *pgcommon.Pool, sql string, args ...any) error {
	return pgcommon.RunInTx(ctx, pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

// count returns SELECT count(*) on pool.
func count(ctx context.Context, pool *pgcommon.Pool, sql string, args ...any) (int, error) {
	var n int
	err := pgcommon.RunInTx(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	})
	return n, err
}

func mustExec(t *testing.T, pool *pgcommon.Pool, sql string, args ...any) {
	t.Helper()
	if err := exec(context.Background(), pool, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", firstLine(sql), err)
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

// errCode returns the SQLSTATE of err, or "".
func errCode(err error) string {
	type sqlstate interface{ SQLState() string }
	for e := err; e != nil; {
		if s, ok := e.(sqlstate); ok {
			return s.SQLState()
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return ""
		}
		e = u.Unwrap()
	}
	return ""
}
