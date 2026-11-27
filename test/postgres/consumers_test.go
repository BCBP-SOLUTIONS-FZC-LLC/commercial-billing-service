//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

type accountRow struct {
	lifecycle, source *string
	lastEvent, closed *time.Time
}

func readAccount(t *testing.T, tenant string) accountRow {
	t.Helper()
	var a accountRow
	err := pgcommon.RunInTx(context.Background(), superPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT iam_lifecycle::text, iam_suspension_source, iam_last_event_at, closed_at FROM billing_accounts WHERE tenant_id = $1`, tenant).
			Scan(&a.lifecycle, &a.source, &a.lastEvent, &a.closed)
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func lifecycleConsumer(t *testing.T) *app.LifecycleConsumer {
	t.Helper()
	in, err := pgadapter.NewInboxRunner(appPool, app.ConsumerTenantBilling, app.ConsumerUsageBilling)
	if err != nil {
		t.Fatal(err)
	}
	return &app.LifecycleConsumer{Inbox: in, Lock: pgadapter.TenantLocker{}, Accounts: pgadapter.NewLifecycleRepository(appPool),
		Subs: pgadapter.NewSubscriptionRepository(appPool), MaxSkew: 300 * time.Second}
}

// §7.1 / BIL-EVT-3 on the real schema: exactly once, last writer wins,
// offboarded terminal, and the account CHECKs hold.
func TestLifecycleProjection(t *testing.T) {
	ctx := context.Background()
	s := seedTenant(t)
	c := lifecycleConsumer(t)
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)

	sus := app.TenantLifecycleEvent{EventID: uuid.NewString(), Type: app.EventTenantSuspended, Time: t0, TenantID: s.Tenant, Source: "operator"}
	for range 2 { // redelivery
		if err := c.Handle(ctx, sus); err != nil {
			t.Fatal(err)
		}
	}
	a := readAccount(t, s.Tenant)
	if *a.lifecycle != "suspended" || *a.source != "operator" || !a.lastEvent.Equal(t0) {
		t.Fatalf("suspended: %+v %v", *a.lifecycle, a.lastEvent)
	}
	if n, _ := count(ctx, superPool, `SELECT count(*) FROM processed_events WHERE event_id = $1 AND consumer = 'tenant-billing'`, sus.EventID); n != 1 {
		t.Fatalf("processed_events rows %d", n)
	}

	// Stale reactivation: skipped.
	if err := c.Handle(ctx, app.TenantLifecycleEvent{EventID: uuid.NewString(), Type: app.EventTenantReactivated, Time: t0, TenantID: s.Tenant, Source: "operator"}); err != nil {
		t.Fatal(err)
	}
	if a := readAccount(t, s.Tenant); *a.lifecycle != "suspended" {
		t.Fatalf("stale applied: %s", *a.lifecycle)
	}
	// Newer reactivation: applied, source cleared (chk_ba_suspension).
	if err := c.Handle(ctx, app.TenantLifecycleEvent{EventID: uuid.NewString(), Type: app.EventTenantReactivated, Time: t0.Add(time.Second), TenantID: s.Tenant, Source: "operator"}); err != nil {
		t.Fatal(err)
	}
	if a := readAccount(t, s.Tenant); *a.lifecycle != "active" || a.source != nil {
		t.Fatalf("reactivated: %+v", a)
	}
	// Offboarded: closed_at from offboarded_at (chk_ba_closed); terminal.
	off := t0.Add(2 * time.Second)
	if err := c.Handle(ctx, app.TenantLifecycleEvent{EventID: uuid.NewString(), Type: app.EventTenantOffboarded, Time: off, TenantID: s.Tenant, OffboardedAt: off}); err != nil {
		t.Fatal(err)
	}
	if err := c.Handle(ctx, app.TenantLifecycleEvent{EventID: uuid.NewString(), Type: app.EventTenantReactivated, Time: off.Add(time.Minute), TenantID: s.Tenant, Source: "operator"}); err != nil {
		t.Fatal(err)
	}
	if a := readAccount(t, s.Tenant); *a.lifecycle != "offboarded" || a.closed == nil || !a.closed.Equal(off) {
		t.Fatalf("offboarded: %+v", a)
	}

	// A trial tenant without an account: acknowledged and recorded, no write.
	if err := c.Handle(ctx, app.TenantLifecycleEvent{EventID: uuid.NewString(), Type: app.EventTenantSuspended, Time: t0, TenantID: newID(), Source: "operator"}); err != nil {
		t.Fatal(err)
	}
}

// §4.2.21: a re-run close replaces the stored period only when newer; a
// trial tenant (no account) is stored too.
func TestUsagePeriodUpsert(t *testing.T) {
	ctx := context.Background()
	in, _ := pgadapter.NewInboxRunner(appPool, app.ConsumerUsageBilling)
	uc := &app.UsageConsumer{Inbox: in, Usage: pgadapter.NewUsageRecordRepository(appPool)}
	tenant := newID()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	rec := func(tokens int64) domain.UsagePeriodRecord {
		return domain.UsagePeriodRecord{TenantID: tenant, PeriodStart: start, PeriodEnd: start.AddDate(0, 1, 0), TokensTotal: tokens,
			APIRequestsTotal: 3, Limits: []byte(`{"llm_tokens":500000}`)}
	}
	t1 := time.Date(2026, 11, 1, 0, 0, 5, 0, time.UTC)
	for _, e := range []app.UsagePeriodClosedEvent{
		{EventID: uuid.NewString(), Time: t1, Record: rec(100)},
		{EventID: uuid.NewString(), Time: t1.Add(-time.Minute), Record: rec(50)}, // older: ignored
		{EventID: uuid.NewString(), Time: t1.Add(time.Minute), Record: rec(120)}, // re-run close: replaces
	} {
		if err := uc.Handle(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	var tokens int64
	var limits string
	err := pgcommon.RunInTx(ctx, superPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT tokens_total, limits::text FROM usage_period_records WHERE tenant_id = $1`, tenant).Scan(&tokens, &limits)
	})
	if err != nil || tokens != 120 || limits != `{"llm_tokens": 500000}` {
		t.Fatalf("stored %d %s %v", tokens, limits, err)
	}
}

// BIL-EVT-6 on the real schema: the worker emits a due seat event built
// from the current seat item, in the outbox, and marks the row emitted.
func TestEmissionWorker(t *testing.T) {
	ctx := context.Background()
	s := seedTenant(t) // has a pending seats_resync due now, 10 seats, active
	repo := pgadapter.NewEmissionRepository(appPool, systemPool)
	runner := pgadapter.NewTxRunner(appPool)

	// One pending row per (tenant, builder, ref).
	err := runner.RunInTenantTx(ctx, s.Tenant, func(ctx context.Context) error {
		ins, err := repo.Schedule(ctx, domain.ScheduledEmission{TenantID: s.Tenant, EventType: domain.EventTenantSeatsChanged,
			PayloadBuilder: domain.BuilderSeatsResync, RefID: s.Sub, DueAt: time.Now()})
		if err == nil && ins {
			t.Error("second pending emission inserted")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	w := &app.EmissionWorker{Emissions: repo, Subs: pgadapter.NewSubscriptionRepository(appPool), Tx: runner,
		Lock: pgadapter.TenantLocker{}, Events: newEnqueuer(t), BatchSize: 10_000}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var status, emitted string
	err = pgcommon.RunInTx(ctx, superPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, emitted_event_id::text FROM scheduled_emissions WHERE tenant_id = $1`, s.Tenant).Scan(&status, &emitted)
	})
	if err != nil || status != "emitted" {
		t.Fatalf("emission %s %v", status, err)
	}
	rows := outboxFor(t, s.Tenant)
	if len(rows) != 1 || rows[0].env["id"] != emitted || rows[0].env["user_agent"] != "commercial-billing/worker:emissions" ||
		rows[0].data["licensed_seats"] != float64(10) || rows[0].data["reason"] != "resync" || rows[0].data["billing_subscription_id"] != s.Sub {
		t.Fatalf("outbox %+v", rows)
	}
}
