//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

func TestMigrationsAppliedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	v, dirty, err := pgadapter.DomainVersion(ctx, migrateDSN)
	if err != nil || dirty || v != 11 {
		t.Fatalf("domain version %d dirty=%v err=%v, want 11 clean", v, dirty, err)
	}
	if err := pgadapter.MigrateSchema(ctx, migrateDSN, nil); err != nil {
		t.Fatalf("re-run must be a no-op: %v", err)
	}
	for _, tbl := range []string{"outbox_events", "outbox_dead_letters", "processed_events", "pending_audit_entries"} {
		if n, err := count(ctx, superPool, `SELECT count(*) FROM pg_tables WHERE schemaname='public' AND tablename=$1`, tbl); err != nil || n != 1 {
			t.Errorf("library table %s missing (§4.2.23)", tbl)
		}
	}
}

// BIL-RLS-1: every table with a tenant_id column has ENABLE + FORCE row
// security and the tenant_isolation policy (the CI query of §14.5).
func TestBIL_RLS_1_EveryTenantTableForcesRLS(t *testing.T) {
	ctx := context.Background()
	var missing []string
	checked := 0
	err := pgcommon.RunInTx(ctx, superPool, pgcommon.TxOptions{}, func(ctx context.Context, tx pgcommon.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity,
			       EXISTS (SELECT 1 FROM pg_policies p WHERE p.tablename = c.relname AND p.policyname = 'tenant_isolation')
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
			JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped
			WHERE c.relkind = 'r'
			  AND c.relname NOT IN ('payment_event_ledger', 'reconciliation_findings', 'rls_violation_log', 'outbox_events', 'outbox_dead_letters')`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var rls, force, policy bool
			if err := rows.Scan(&name, &rls, &force, &policy); err != nil {
				return err
			}
			checked++
			if !rls || !force || !policy {
				missing = append(missing, fmt.Sprintf("%s(rls=%v force=%v policy=%v)", name, rls, force, policy))
			}
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	// 18 §4.3 tables plus pending_audit_entries: guards against a query that matches nothing.
	if checked < len(tenantScopedTables)+1 {
		t.Fatalf("catalogue query checked %d tables, want at least %d", checked, len(tenantScopedTables)+1)
	}
	if len(missing) > 0 {
		t.Fatalf("tenant tables without FORCE RLS + tenant_isolation: %s", strings.Join(missing, ", "))
	}
}

// §14.5 for every tenant-scoped table: visible to its tenant, invisible to
// another tenant and without a GUC.
func TestRLSIsolationEveryTable(t *testing.T) {
	a, b := seedTenant(t), seedTenant(t)
	ctx := context.Background()
	for _, tbl := range tenantScopedTables {
		q := fmt.Sprintf(`SELECT count(*) FROM %s WHERE tenant_id = $1`, tbl)
		if n, err := count(asTenant(ctx, a.Tenant), appPool, q, a.Tenant); err != nil || n == 0 {
			t.Errorf("%s: tenant A sees %d own rows (err %v), want > 0", tbl, n, err)
		}
		if n, err := count(asTenant(ctx, b.Tenant), appPool, q, a.Tenant); err != nil || n != 0 {
			t.Errorf("%s: tenant B sees %d of A's rows (err %v), want 0", tbl, n, err)
		}
		if n, err := count(ctx, appPool, q, a.Tenant); err != nil || n != 0 {
			t.Errorf("%s: no GUC sees %d rows (err %v), want 0", tbl, n, err)
		}
		if n, err := count(asTenant(ctx, "not-a-uuid"), appPool, q, a.Tenant); err != nil || n != 0 {
			t.Errorf("%s: malformed GUC sees %d rows (err %v), want 0", tbl, n, err)
		}
	}
}

// §14.5 WITH CHECK: a tenant-bound session can neither write another
// tenant's row nor move a row to another tenant; updates of rows it cannot
// see touch nothing.
func TestRLSWithCheck(t *testing.T) {
	a, b := seedTenant(t), seedTenant(t)
	ctx := context.Background()
	err := exec(asTenant(ctx, b.Tenant), appPool,
		`INSERT INTO adjustments (adjustment_id, tenant_id, kind, amount_minor, currency, description, reason, actor_id, idempotency_key)
		 VALUES ($1, $2, 'credit', 100, 'USD', 'x-x', 'cross-tenant write attempt', $2, 'key-cross-1')`, newID(), a.Tenant)
	if errCode(err) != "42501" {
		t.Fatalf("insert for another tenant: want 42501 (RLS WITH CHECK), got %v", err)
	}
	err = exec(asTenant(ctx, a.Tenant), appPool, `UPDATE billing_accounts SET tenant_id = $1 WHERE tenant_id = $2`, b.Tenant, a.Tenant)
	if errCode(err) != "42501" && errCode(err) != "23505" {
		t.Fatalf("moving a row to another tenant must fail, got %v", err)
	}
	var affected int64
	err = pgcommon.RunInTx(asTenant(ctx, b.Tenant), appPool, pgcommon.TxOptions{}, func(ctx context.Context, tx pgcommon.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE billing_accounts SET company_name = 'hijack' WHERE tenant_id = $1`, a.Tenant)
		affected = tag.RowsAffected()
		return err
	})
	if err != nil || affected != 0 {
		t.Fatalf("update of another tenant's row affected %d (err %v), want 0", affected, err)
	}
}

// §14.5: billing_app has no access to the ledger or reconciliation tables;
// the price book is read-only for it; billing_system can write them.
func TestBIL_RLS_3_RoleGrants(t *testing.T) {
	ctx := context.Background()
	for _, tbl := range []string{"payment_event_ledger", "reconciliation_runs", "reconciliation_findings"} {
		_, err := count(ctx, appPool, fmt.Sprintf(`SELECT count(*) FROM %s`, tbl))
		if errCode(err) != "42501" {
			t.Errorf("billing_app reading %s: want 42501, got %v", tbl, err)
		}
		if _, err := count(ctx, systemPool, fmt.Sprintf(`SELECT count(*) FROM %s`, tbl)); err != nil {
			t.Errorf("billing_system reading %s: %v", tbl, err)
		}
	}
	if n, err := count(ctx, appPool, `SELECT count(*) FROM plan_prices`); err != nil || n == 0 {
		t.Errorf("billing_app reads the price book: %d %v", n, err)
	}
	err := exec(ctx, appPool, `UPDATE plan_prices SET reason = 'app must not write prices' WHERE provider = 'manual'`)
	if errCode(err) != "42501" {
		t.Errorf("billing_app writing plan_prices: want 42501, got %v", err)
	}
	if n, err := count(ctx, appPool, `SELECT count(*) FROM provider_certifications`); err != nil || n == 0 {
		t.Errorf("billing_app reads provider_certifications: %d %v", n, err)
	}
	err = exec(ctx, appPool, `INSERT INTO provider_certifications (provider, adapter_version, suite_version, status, proration_mode, capabilities, results, certified_by)
		VALUES ('stripe', 'x', 'cert-1.0', 'pending_review', 'native', '{}', '[]', $1)`, newID())
	if errCode(err) != "42501" {
		t.Errorf("billing_app writing provider_certifications: want 42501, got %v", err)
	}
	// History is append-only for billing_app.
	a := seedTenant(t)
	err = exec(asTenant(ctx, a.Tenant), appPool, `DELETE FROM subscription_status_history WHERE tenant_id = $1`, a.Tenant)
	if errCode(err) != "42501" {
		t.Errorf("billing_app deleting history: want 42501, got %v", err)
	}
	// admin_readonly can read, not write.
	if err := exec(ctx, readPool, `UPDATE plan_prices SET reason = 'readonly must not write' WHERE provider = 'manual'`); errCode(err) != "42501" {
		t.Errorf("admin_readonly writing: want 42501, got %v", err)
	}
}

func TestViewRespectsRLS(t *testing.T) {
	a, b := seedTenant(t), seedTenant(t)
	ctx := context.Background()
	if n, _ := count(asTenant(ctx, a.Tenant), appPool, `SELECT count(*) FROM v_subscription_summary WHERE tenant_id = $1 AND licensed_seats = 10`, a.Tenant); n != 1 {
		t.Errorf("tenant A sees %d summary rows, want 1", n)
	}
	if n, _ := count(asTenant(ctx, b.Tenant), appPool, `SELECT count(*) FROM v_subscription_summary WHERE tenant_id = $1`, a.Tenant); n != 0 {
		t.Errorf("security_invoker view leaked %d rows to tenant B", n)
	}
}

// 000007: the SECURITY DEFINER RLS helpers are not executable by PUBLIC;
// only the NOBYPASSRLS roles whose queries evaluate tenant_isolation may call
// rls_check_tenant, and app_tenant_id / log_rls_violation are reachable only
// through it. Every role still queries the tenant tables it is granted.
func TestRLSHelperFunctionGrants(t *testing.T) {
	ctx := context.Background()
	fns := map[string]string{
		"rls_check_tenant":  "rls_check_tenant(uuid, text)",
		"app_tenant_id":     "app_tenant_id()",
		"log_rls_violation": "log_rls_violation(text, uuid, text)",
	}
	want := map[string]map[string]bool{
		"billing_app":           {"rls_check_tenant": true},
		"admin_readonly":        {"rls_check_tenant": true},
		"billing_audit_drainer": {"rls_check_tenant": true},
		"billing_system":        {},
	}
	for role, allowed := range want {
		for name, sig := range fns {
			n, err := count(ctx, superPool, `SELECT count(*) FROM pg_proc WHERE oid = $1::regprocedure AND has_function_privilege($2, oid, 'EXECUTE')`, sig, role)
			if err != nil {
				t.Fatalf("%s on %s: %v", role, sig, err)
			}
			if got := n == 1; got != allowed[name] {
				t.Errorf("%s EXECUTE on %s = %v, want %v", role, sig, got, allowed[name])
			}
		}
	}
	for name, sig := range fns {
		n, err := count(ctx, superPool, `SELECT count(*) FROM pg_proc p, aclexplode(coalesce(p.proacl, acldefault('f', p.proowner))) a
			WHERE p.oid = $1::regprocedure AND a.grantee = 0 AND a.privilege_type = 'EXECUTE'`, sig)
		if err != nil || n != 0 {
			t.Errorf("PUBLIC has EXECUTE on %s (%d, %v)", name, n, err)
		}
	}

	// The policies still evaluate for every role that queries tenant tables.
	s := seedTenant(t)
	if n, err := count(asTenant(ctx, s.Tenant), appPool, `SELECT count(*) FROM subscriptions WHERE tenant_id = $1`, s.Tenant); err != nil || n == 0 {
		t.Errorf("billing_app reads its tenant's subscriptions: %d %v", n, err)
	}
	if _, err := count(ctx, readPool, `SELECT count(*) FROM subscriptions`); err != nil {
		t.Errorf("admin_readonly reads subscriptions: %v", err)
	}
	if n, err := count(ctx, systemPool, `SELECT count(*) FROM subscriptions WHERE tenant_id = $1`, s.Tenant); err != nil || n == 0 {
		t.Errorf("billing_system (BYPASSRLS) reads subscriptions: %d %v", n, err)
	}
	drainer, err := pgcommon.NewPool(ctx, pgcommon.Config{DSN: withUser(migrateDSN, "billing_audit_drainer", "devpassword"), PoolName: "drainer", MaxConns: 2, PGBouncerMode: true})
	if err != nil {
		t.Fatalf("drainer pool: %v", err)
	}
	defer drainer.Close()
	if _, err := count(ctx, drainer, `SELECT count(*) FROM pending_audit_entries`); err != nil {
		t.Errorf("billing_audit_drainer reads pending_audit_entries: %v", err)
	}
}
