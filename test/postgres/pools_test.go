//go:build integration

package postgres_test

import (
	"context"
	"testing"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

func currentUser(ctx context.Context, t *testing.T, p *pgcommon.Pool) (user, timeout string) {
	t.Helper()
	err := pgcommon.RunInTx(ctx, p, pgcommon.TxOptions{}, func(ctx context.Context, tx pgcommon.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_user::text, current_setting('statement_timeout')`).Scan(&user, &timeout)
	})
	if err != nil {
		t.Fatal(err)
	}
	return user, timeout
}

// Every pool is pgcommon's environment config: the app pool as billing_app,
// the system pool and the audit drainer with their own role, all with
// pgcommon's settings (PG_STATEMENT_TIMEOUT here).
func TestPoolsFromPgcommonConfig(t *testing.T) {
	ctx := context.Background()
	t.Setenv("DATABASE_URL", withUser(migrateDSN, "billing_app", "devpassword"))
	t.Setenv("PG_STATEMENT_TIMEOUT", "4s")
	t.Setenv("PG_BOUNCER_MODE", "true")
	pools, err := pgadapter.OpenPools(ctx, pgadapter.PoolOptions{
		System: pgadapter.Role{User: "billing_system", Password: "devpassword", MaxConns: 2}})
	if err != nil {
		t.Fatal(err)
	}
	defer pools.Close()
	if pools.SystemReusesApp {
		t.Fatal("system pool reused the app role")
	}
	if u, to := currentUser(ctx, t, pools.App); u != "billing_app" || to != "4s" {
		t.Errorf("app pool: %s, statement_timeout %s", u, to)
	}
	if u, to := currentUser(ctx, t, pools.System); u != "billing_system" || to != "4s" {
		t.Errorf("system pool: %s, statement_timeout %s", u, to)
	}
	dcfg, err := pgadapter.AuditDrainerConfig(pgadapter.Role{User: "billing_audit_drainer", Password: "devpassword", MaxConns: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	drainer, err := pgcommon.NewPool(ctx, dcfg)
	if err != nil {
		t.Fatal(err)
	}
	defer drainer.Close()
	if u, _ := currentUser(ctx, t, drainer); u != "billing_audit_drainer" {
		t.Errorf("drainer pool: %s", u)
	}
	if _, err := pgadapter.AuditDrainerConfig(pgadapter.Role{User: "billing_audit_drainer"}, nil); err == nil {
		t.Error("drainer without a password accepted")
	}
}
