//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// withDB rewrites dsn's database name.
func withDB(dsn, db string) string {
	i := strings.LastIndex(dsn, "/")
	j := strings.Index(dsn[i:], "?")
	if j < 0 {
		return dsn[:i+1] + db
	}
	return dsn[:i+1] + db + dsn[i+j:]
}

func superExec(t *testing.T, sql string) {
	t.Helper()
	if err := superPool.WithConn(context.Background(), func(ctx context.Context, c *pgcommon.Conn) error {
		_, err := c.Exec(ctx, sql)
		return err
	}); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

type lines struct{ n int }

func (l *lines) Debug(string, map[string]any) { l.n++ }
func (l *lines) Info(string, map[string]any)  { l.n++ }
func (l *lines) Warn(string, map[string]any)  { l.n++ }
func (l *lines) Error(string, map[string]any) { l.n++ }

// MigrateSchema on a fresh database applies every stage; re-run on a
// stage left dirty, it stops at that stage and says how to repair it
// (LLD §19.5).
func TestMigrateSchemaStagesAndDirtyState(t *testing.T) {
	ctx := context.Background()
	superExec(t, `CREATE DATABASE billing_cov_migrate`)
	t.Cleanup(func() { superExec(t, `DROP DATABASE billing_cov_migrate WITH (FORCE)`) })
	dsn := withDB(withUser(migrateDSN, "postgres", "testpassword"), "billing_cov_migrate")

	log := &lines{}
	if err := pgadapter.MigrateSchema(ctx, dsn, log); err != nil {
		t.Fatalf("fresh database: %v", err)
	}
	if log.n == 0 {
		t.Error("migration ran without logging through the adapter")
	}
	pool, err := pgcommon.NewPool(ctx, pgcommon.Config{DSN: dsn, PoolName: "cov_migrate", MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, s := range []struct{ table, stage string }{
		{"inbox_migrations", "inbox schema:"},
		{"platform_audit_schema_migrations", "audit outbox schema:"},
		{pgadapter.DomainMigrationsTable, "domain migrations:"},
	} {
		if err := exec(ctx, pool, `UPDATE `+s.table+` SET dirty = true`); err != nil {
			t.Fatalf("mark %s dirty: %v", s.table, err)
		}
		err := pgadapter.MigrateSchema(ctx, dsn, nil)
		if err == nil || !strings.HasPrefix(err.Error(), s.stage) || !strings.Contains(err.Error(), "dirty") {
			t.Errorf("%s dirty: %v", s.table, err)
		}
		if err := exec(ctx, pool, `UPDATE `+s.table+` SET dirty = false`); err != nil {
			t.Fatalf("restore %s: %v", s.table, err)
		}
	}
}

// OpenPools: the app pool opens, then a system role that cannot log in
// fails startup with the pool named and the app pool closed.
func TestOpenPoolsSystemRoleRefused(t *testing.T) {
	t.Setenv("DATABASE_URL", withUser(migrateDSN, "billing_app", "devpassword"))
	_, err := pgadapter.OpenPools(context.Background(), pgadapter.PoolOptions{System: pgadapter.Role{User: "billing_system", Password: "wrong"}})
	if err == nil || !strings.HasPrefix(err.Error(), "system pool:") {
		t.Fatalf("err = %v", err)
	}
	p, err := pgadapter.OpenPools(context.Background(), pgadapter.PoolOptions{System: pgadapter.Role{User: "billing_system", Password: "devpassword", MaxConns: 2}})
	if err != nil {
		t.Fatalf("valid roles: %v", err)
	}
	defer p.Close()
	if p.SystemReusesApp || p.App.Name() != pgadapter.AppPoolName || p.System.Name() != pgadapter.SystemPoolName {
		t.Errorf("pools %+v", p)
	}
}

// The inbox runs only inside a tenant transaction; a cancelled prune
// reports its error.
func TestInboxRunnerTenantAndPrune(t *testing.T) {
	r, err := pgadapter.NewInboxRunner(appPool, "cov-consumer")
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	err = r.RunOnce(context.Background(), "cov-consumer", newID(), "TenantSuspended", "", func(context.Context) error { ran = true; return nil })
	if !domain.HasCode(err, domain.ErrInternal) || ran {
		t.Errorf("no tenant: %v ran=%v", err, ran)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n, err := r.Prune(ctx, 24*time.Hour); err == nil || n != 0 {
		t.Errorf("cancelled prune: %d %v", n, err)
	}
}

// Update with a stale record_version is 409 OPTIMISTIC_LOCK_CONFLICT with
// the current version, and nothing changes.
func TestSubscriptionUpdateStaleVersion(t *testing.T) {
	ctx := context.Background()
	tenant, subID := newAccountAndSubscription(t)
	subs := pgadapter.NewSubscriptionRepository(appPool)
	tx := pgadapter.NewTxRunner(appPool)
	var cur domain.Subscription
	if err := tx.RunInTenantTx(ctx, tenant, func(ctx context.Context) error {
		var err error
		cur, err = subs.Get(ctx, subID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err := tx.RunInTenantTx(ctx, tenant, func(ctx context.Context) error {
		_, err := subs.Update(ctx, subID, cur.RecordVersion+5, func(s *domain.Subscription) { s.AutoRenew = false })
		return err
	})
	var de *domain.DomainError
	if !errors.As(err, &de) || de.Code != domain.ErrOptimisticLockConflict || de.Extensions["record_version"] != cur.RecordVersion {
		t.Fatalf("stale version: %v", err)
	}
	var after domain.Subscription
	_ = tx.RunInTenantTx(ctx, tenant, func(ctx context.Context) error {
		var err error
		after, err = subs.Get(ctx, subID)
		return err
	})
	if !after.AutoRenew || after.RecordVersion != cur.RecordVersion {
		t.Errorf("changed on conflict: %+v", after)
	}
}

// SQLSTATE handling on the write paths: one open plan change and one open
// seat purchase per subscription, one queued reconciliation run per
// provider, and a window that ends before it starts.
func TestWriteConstraintsBecomeDomainErrors(t *testing.T) {
	ctx := context.Background()
	tenant, subID := newAccountAndSubscription(t)
	tx := pgadapter.NewTxRunner(appPool)
	changes := pgadapter.NewPlanChangeRepository(appPool, systemPool)
	seats := pgadapter.NewSeatPurchaseRepository(appPool)
	change := func(key string) domain.PlanChangeRequest {
		return domain.PlanChangeRequest{ChangeID: newID(), TenantID: tenant, BillingSubscriptionID: subID, FromPlan: domain.PlanPro, ToPlan: domain.PlanStarter,
			Direction: "downgrade", Status: "scheduled", EffectiveAt: ptr(time.Now().Add(time.Hour)), RequestedBy: newID(), IdempotencyKey: key}
	}
	purchase := func(key string) domain.SeatPurchase {
		return domain.SeatPurchase{PurchaseID: newID(), TenantID: tenant, BillingSubscriptionID: subID, Reason: "purchase", SeatsAdded: 1, SeatsBefore: 10,
			SeatsAfter: 11, ProrationInstant: time.Now(), AmountMinor: 100, Currency: "USD", AmountSource: "provider_preview", Status: "pending_payment",
			RequestedBy: newID(), IdempotencyKey: key}
	}
	in := func(fn func(ctx context.Context) error) error { return tx.RunInTenantTx(ctx, tenant, fn) }
	if err := in(func(ctx context.Context) error { return changes.Insert(ctx, change("change-1")) }); err != nil {
		t.Fatal(err)
	}
	if err := in(func(ctx context.Context) error { return changes.Insert(ctx, change("change-2")) }); codeOf(err) != domain.ErrPlanChangePending {
		t.Errorf("second open change: %v", err)
	}
	if err := in(func(ctx context.Context) error { return seats.Insert(ctx, purchase("seats-1")) }); err != nil {
		t.Fatal(err)
	}
	if err := in(func(ctx context.Context) error { return seats.Insert(ctx, purchase("seats-2")) }); codeOf(err) != domain.ErrSeatPurchaseInProgress {
		t.Errorf("second open purchase: %v", err)
	}

	recon := pgadapter.NewReconRepository(systemPool)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	run := port.ReconRun{RunID: newID(), Provider: "manual", WindowStart: start, WindowEnd: start.AddDate(0, 0, 7), StartedBy: newID()}
	if err := recon.Queue(ctx, run); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec(ctx, superPool, `DELETE FROM reconciliation_runs WHERE run_id = $1::uuid`, run.RunID) })
	run.RunID = newID()
	if err := recon.Queue(ctx, run); codeOf(err) != domain.ErrReconciliationRunning {
		t.Errorf("second queued run: %v", err)
	}
	run.RunID, run.Provider, run.WindowEnd = newID(), "stripe", start.Add(-time.Hour)
	if err := recon.Queue(ctx, run); codeOf(err) != domain.ErrValidation {
		t.Errorf("reversed window: %v", err)
	}
}

// Writes that reach no row, or that Postgres refuses, are reported.
func TestWritesReportMissingRowsAndRefusals(t *testing.T) {
	ctx := context.Background()
	tenant, _ := newAccountAndSubscription(t)
	tx := pgadapter.NewTxRunner(appPool)
	in := func(fn func(ctx context.Context) error) error { return tx.RunInTenantTx(ctx, tenant, fn) }
	lifecycle := pgadapter.NewLifecycleRepository(appPool)

	// Unknown lifecycle value: refused by the enum.
	if err := in(func(ctx context.Context) error {
		return lifecycle.ApplyLifecycle(ctx, tenant, domain.LifecycleChange{To: "dormant", EventAt: time.Now()})
	}); err == nil {
		t.Error("unknown lifecycle stored")
	}
	// A tenant without an account: 404.
	other := newID()
	if err := tx.RunInTenantTx(ctx, other, func(ctx context.Context) error {
		return lifecycle.ApplyLifecycle(ctx, other, domain.LifecycleChange{To: domain.LifecycleActive, EventAt: time.Now()})
	}); codeOf(err) != domain.ErrSubscriptionNotFound {
		t.Errorf("no account: %v", err)
	}
	// A subscription without a seats item: 404.
	if err := in(func(ctx context.Context) error {
		return pgadapter.NewSubscriptionRepository(appPool).SetItem(ctx, domain.SubscriptionItem{BillingSubscriptionID: newID(), PriceID: newID(), Quantity: 3})
	}); codeOf(err) != domain.ErrSubscriptionNotFound {
		t.Errorf("no seats item: %v", err)
	}
	// invoice_links accepts only the documented reference types.
	if err := in(func(ctx context.Context) error {
		return pgadapter.NewInvoiceRepository(appPool).Link(ctx, newID(), tenant, "coupon", newID())
	}); err == nil {
		t.Error("unknown link type stored")
	}
	// The price book's plan is an enum: an unknown plan is refused.
	if _, err := pgadapter.NewAdminRepository(systemPool).AddPlanPrice(ctx, domain.PlanPrice{Plan: "gold", Interval: domain.IntervalMonth,
		Currency: "USD", Provider: "manual", EffectiveFrom: time.Now()}); err == nil {
		t.Error("unknown plan priced")
	}
}

func ptr[T any](v T) *T { return &v }
