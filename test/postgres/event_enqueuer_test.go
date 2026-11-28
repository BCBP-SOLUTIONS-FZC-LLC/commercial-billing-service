//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/eventbus"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

type outboxRow struct {
	key  string
	seq  int64
	env  map[string]any
	data map[string]any
}

func outboxFor(t *testing.T, tenant string) []outboxRow {
	t.Helper()
	var rows []outboxRow
	err := pgcommon.RunInTx(context.Background(), superPool, pgcommon.TxOptions{}, func(ctx context.Context, tx pgcommon.Tx) error {
		r, err := tx.Query(ctx, `SELECT ordering_key, ordering_seq, payload::text FROM outbox_events WHERE tenant_id = $1 ORDER BY ordering_seq`, tenant)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var o outboxRow
			var raw string
			if err := r.Scan(&o.key, &o.seq, &raw); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(raw), &o.env); err != nil {
				return err
			}
			o.data, _ = o.env["data"].(map[string]any)
			rows = append(rows, o)
		}
		return r.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func newEnqueuer(t *testing.T) *pgadapter.EventEnqueuer {
	t.Helper()
	s, err := eventbus.LoadSchemas()
	if err != nil {
		t.Fatal(err)
	}
	return pgadapter.NewEventEnqueuer(eventbus.Source, eventbus.SpecVersion, s.Validate)
}

// BIL-EVT-1/2/5: events commit with the change, ordered per tenant, with
// strictly increasing Billing-clock times within one transaction.
func TestBIL_EVT_1_EventEnqueuer(t *testing.T) {
	ctx := context.Background()
	enq := newEnqueuer(t)
	runner := pgadapter.NewTxRunner(appPool)
	tenant := newID()
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	seats := domain.TenantSeatsChanged{TenantID: tenant, LicensedSeats: 7, Reason: domain.SeatsReactivation}
	react := domain.TenantReactivated{TenantID: tenant, Source: domain.ReactivationSource, ReactivatedAt: at, PreviousStatus: domain.StatusCancelled}
	meta := port.EventMeta{Actor: "user-1", IPAddress: "203.0.113.9", UserAgent: "ua/1", CorrelationID: "corr-1"}

	before := time.Now().Add(-time.Second)
	err := runner.RunInTenantTx(ctx, tenant, func(ctx context.Context) error {
		// BIL-EVT-5: seat event first, status event last.
		for _, p := range []domain.EventPayload{seats, react, seats} {
			if _, err := enq.Enqueue(ctx, p, meta); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := outboxFor(t, tenant)
	if len(rows) != 3 {
		t.Fatalf("got %d rows", len(rows))
	}
	var prev time.Time
	for i, r := range rows {
		ts, err := time.Parse(time.RFC3339Nano, r.env["time"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if ts.Before(before) || ts.After(time.Now().Add(time.Second)) {
			t.Errorf("row %d: time %s is not the database clock", i, ts)
		}
		if i > 0 && ts.Sub(prev) < time.Millisecond {
			t.Errorf("row %d: time %s not ≥1 ms after %s (BIL-EVT-1)", i, ts, prev)
		}
		prev = ts
		if r.key != tenant {
			t.Errorf("row %d: ordering key %q", i, r.key)
		}
		for k, want := range map[string]string{"source": "commercial-billing", "specversion": "1", "subject": tenant, "tenant_id": tenant,
			"actor": "user-1", "ip_address": "203.0.113.9", "user_agent": "ua/1", "correlation_id": "corr-1"} {
			if r.env[k] != want {
				t.Errorf("row %d: %s = %v, want %s", i, k, r.env[k], want)
			}
		}
		if r.data["tenant_id"] != tenant { // D-1
			t.Errorf("row %d: data.tenant_id %v", i, r.data["tenant_id"])
		}
	}
	if rows[0].env["type"] != "TenantSeatsChanged" || rows[1].env["type"] != "TenantReactivated" {
		t.Errorf("order: %v, %v", rows[0].env["type"], rows[1].env["type"])
	}

	// System defaults.
	tenant2 := newID()
	if err := runner.RunInTenantTx(ctx, tenant2, func(ctx context.Context) error {
		_, err := enq.Enqueue(ctx, domain.TenantSeatsChanged{TenantID: tenant2, LicensedSeats: 3, Reason: domain.SeatsResync}, port.EventMeta{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r := outboxFor(t, tenant2); len(r) != 1 || r[0].env["actor"] != port.SystemActor || r[0].env["ip_address"] != "system" {
		t.Errorf("system meta: %+v", r)
	}
}

// BIL-EVT-2: a rolled-back change leaves no event; nothing is written
// outside a transaction; an invalid payload is refused.
func TestBIL_EVT_2_EventEnqueuerAtomicity(t *testing.T) {
	ctx := context.Background()
	enq := newEnqueuer(t)
	runner := pgadapter.NewTxRunner(appPool)
	tenant := newID()
	ok := domain.TenantSeatsChanged{TenantID: tenant, LicensedSeats: 4, Reason: domain.SeatsPurchase, BillingRef: strp(newID())}

	boom := errors.New("business failure")
	err := runner.RunInTenantTx(ctx, tenant, func(ctx context.Context) error {
		if _, err := enq.Enqueue(ctx, ok, port.EventMeta{}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if r := outboxFor(t, tenant); len(r) != 0 {
		t.Fatalf("rolled-back event persisted: %d", len(r))
	}

	if _, err := enq.Enqueue(ctx, ok, port.EventMeta{}); err == nil {
		t.Error("enqueue outside a transaction accepted")
	}
	err = runner.RunInTenantTx(ctx, tenant, func(ctx context.Context) error {
		_, err := enq.Enqueue(ctx, domain.TenantSeatsChanged{TenantID: tenant, LicensedSeats: 0, Reason: domain.SeatsPurchase}, port.EventMeta{})
		return err
	})
	var de *domain.DomainError
	if !errors.As(err, &de) {
		t.Errorf("invalid payload: %v", err)
	}
}
