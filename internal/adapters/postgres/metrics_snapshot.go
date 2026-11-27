package postgres

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// SnapshotRepository reads the §11.2 exporter gauges across tenants
// (system pool, read-only; aggregates only, no tenant ids leave the query).
type SnapshotRepository struct{ sys *pgcommon.Pool }

var _ port.SnapshotReader = (*SnapshotRepository)(nil)

// NewSnapshotRepository wraps the system pool.
func NewSnapshotRepository(sysPool *pgcommon.Pool) *SnapshotRepository {
	return &SnapshotRepository{sys: sysPool}
}

// MetricsSnapshot implements port.SnapshotReader.
func (r *SnapshotRepository) MetricsSnapshot(ctx context.Context, overdueBefore time.Time) (port.MetricsSnapshot, error) {
	s := port.MetricsSnapshot{WebhookBacklog: map[string]int{}, SagasInFlight: map[string]int{}, Subscriptions: map[[3]string]int{},
		CollectionPaused: map[string]int{}, InvoicesHeld: map[string]int{}, EmissionsPending: map[string]int{}}
	err := inTx(ctx, r.sys, func(q querier) error {
		groups := []struct {
			sql  string
			into map[string]int
			args []any
		}{
			{`SELECT status::text, count(*) FROM payment_event_ledger WHERE status IN ('received','failed') GROUP BY 1`, s.WebhookBacklog, nil},
			{`SELECT state::text, count(*) FROM conversion_sagas WHERE state IN ('payment_confirmed','rp_convert_pending','awaiting_tenant_converted','failed') GROUP BY 1`, s.SagasInFlight, nil},
			{`SELECT coalesce(collection_paused_by,'unknown'), count(*) FROM subscriptions WHERE collection_paused AND status <> 'ended' GROUP BY 1`, s.CollectionPaused, nil},
			{`SELECT currency, count(*) FROM invoices WHERE held_for_review AND disposition IS NULL AND status = 'draft' GROUP BY 1`, s.InvoicesHeld, nil},
			{`SELECT payload_builder, count(*) FROM scheduled_emissions WHERE status = 'pending' GROUP BY 1`, s.EmissionsPending, nil},
		}
		for _, g := range groups {
			if err := groupInto(ctx, q, g.into, g.sql, g.args...); err != nil {
				return err
			}
		}
		if err := q.QueryRow(ctx, `SELECT count(*) FROM conversion_sagas WHERE payment_confirmed_at < $1
			AND state IN ('payment_confirmed','rp_convert_pending','awaiting_tenant_converted')`, overdueBefore).Scan(&s.SagasOverdue); err != nil {
			return err
		}
		rows, err := q.Query(ctx, `SELECT status::text, plan::text, provider, count(*) FROM subscriptions WHERE status <> 'ended' GROUP BY 1, 2, 3`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k [3]string
			var n int
			if err := rows.Scan(&k[0], &k[1], &k[2], &n); err != nil {
				return err
			}
			s.Subscriptions[k] = n
		}
		return rows.Err()
	})
	return s, err
}

func groupInto(ctx context.Context, q querier, into map[string]int, sql string, args ...any) error {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return err
		}
		into[k] = n
	}
	return rows.Err()
}
