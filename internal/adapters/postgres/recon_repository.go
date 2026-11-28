package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// ReconRepository is port.ReconRepo on the system pool (§8.13: runs as
// billing_system across tenants).
type ReconRepository struct{ sys *pgcommon.Pool }

var _ port.ReconRepo = (*ReconRepository)(nil)

// NewReconRepository wraps the system pool.
func NewReconRepository(sysPool *pgcommon.Pool) *ReconRepository {
	return &ReconRepository{sys: sysPool}
}

// Queue implements port.ReconRepo; uq_rr_one_running → 409
// RECONCILIATION_RUNNING.
func (r *ReconRepository) Queue(ctx context.Context, run port.ReconRun) error {
	err := inTx(ctx, r.sys, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO reconciliation_runs (run_id, provider, window_start, window_end, auto_repair, status, started_by)
			VALUES ($1::uuid, $2, $3, $4, $5, 'queued', $6::uuid)`, run.RunID, run.Provider, run.WindowStart, run.WindowEnd, run.AutoRepair, run.StartedBy)
		return err
	})
	switch {
	case pgcommon.IsUniqueViolation(err):
		return domain.NewError(domain.ErrReconciliationRunning, "a reconciliation run for this provider is queued or running")
	case pgcommon.IsCheckViolation(err):
		return domain.NewError(domain.ErrValidation, "the window must end after it starts and span at most 35 days")
	}
	return err
}

// Claim implements port.ReconRepo: one queued run → running.
func (r *ReconRepository) Claim(ctx context.Context, worker string) (port.ReconRun, bool, error) {
	var run port.ReconRun
	err := inTx(ctx, r.sys, func(q querier) error {
		return q.QueryRow(ctx, `UPDATE reconciliation_runs SET status = 'running', claimed_by = $1, started_at = now()
			WHERE run_id = (SELECT run_id FROM reconciliation_runs WHERE status = 'queued' ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED)
			RETURNING run_id::text, provider, window_start, window_end, auto_repair, started_by::text`, worker).
			Scan(&run.RunID, &run.Provider, &run.WindowStart, &run.WindowEnd, &run.AutoRepair, &run.StartedBy)
	})
	if errors.Is(err, pgcommon.ErrNoRows) {
		return run, false, nil
	}
	return run, err == nil, err
}

// AddFinding implements port.ReconRepo.
func (r *ReconRepository) AddFinding(ctx context.Context, runID string, f port.ReconFinding) error {
	return inTx(ctx, r.sys, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO reconciliation_findings (run_id, tenant_id, kind, billing_value, provider_value, auto_repaired, repair_note)
			VALUES ($1::uuid, NULLIF($2, '')::uuid, $3, $4::jsonb, $5::jsonb, $6, $7)`, runID, f.TenantID, f.Kind, jsonText(f.BillingValue),
			jsonText(f.ProviderValue), f.AutoRepaired, f.RepairNote)
		return err
	})
}

// Finish implements port.ReconRepo.
func (r *ReconRepository) Finish(ctx context.Context, runID, status string, counts map[string]int, errText *string) error {
	c, _ := json.Marshal(counts)
	return inTx(ctx, r.sys, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE reconciliation_runs SET status = $2, counts = $3::jsonb, error = $4, finished_at = now()
			WHERE run_id = $1::uuid`, runID, status, string(c), errText)
		return err
	})
}

// Get implements port.ReconRepo.
func (r *ReconRepository) Get(ctx context.Context, runID string) (port.ReconRun, []port.ReconFinding, bool, error) {
	var run port.ReconRun
	var findings []port.ReconFinding
	err := inTx(ctx, r.sys, func(q querier) error {
		var counts *string
		if err := q.QueryRow(ctx, `SELECT run_id::text, provider, window_start, window_end, auto_repair, started_by::text, status, counts::text
			FROM reconciliation_runs WHERE run_id = $1::uuid`, runID).
			Scan(&run.RunID, &run.Provider, &run.WindowStart, &run.WindowEnd, &run.AutoRepair, &run.StartedBy, &run.Status, &counts); err != nil {
			return err
		}
		if counts != nil {
			_ = json.Unmarshal([]byte(*counts), &run.Counts)
		}
		rows, err := q.Query(ctx, `SELECT coalesce(tenant_id::text, ''), kind, coalesce(billing_value::text, 'null'), coalesce(provider_value::text, 'null'),
			auto_repaired, repair_note FROM reconciliation_findings WHERE run_id = $1::uuid ORDER BY finding_id`, runID)
		if err != nil {
			return err
		}
		findings, err = collect(rows, func(row pgcommon.Row) (port.ReconFinding, error) {
			var f port.ReconFinding
			var bv, pv string
			err := row.Scan(&f.TenantID, &f.Kind, &bv, &pv, &f.AutoRepaired, &f.RepairNote)
			f.BillingValue, f.ProviderValue = []byte(bv), []byte(pv)
			return f, err
		})
		return err
	})
	if errors.Is(err, pgcommon.ErrNoRows) {
		return run, nil, false, nil
	}
	return run, findings, err == nil, err
}

// BillingSubscriptions implements port.ReconRepo: provider subscription id
// → Billing's status, seat quantity, provider price refs and in-flight
// changes for the provider (§8.13).
func (r *ReconRepository) BillingSubscriptions(ctx context.Context, provider string) (map[string]port.ReconSubscription, error) {
	out := map[string]port.ReconSubscription{}
	err := inTx(ctx, r.sys, func(q querier) error {
		rows, err := q.Query(ctx, `
			SELECT s.provider_subscription_id, s.tenant_id::text, s.status::text, s.plan::text, coalesce(i.quantity, 0),
			       coalesce(p.provider_price_id, ''), coalesce(sp.provider_price_id, ''),
			       EXISTS (SELECT 1 FROM seat_purchases x WHERE x.billing_subscription_id = s.billing_subscription_id
			                 AND x.status IN ('pending_payment', 'requires_action'))
			       OR EXISTS (SELECT 1 FROM plan_change_requests c WHERE c.billing_subscription_id = s.billing_subscription_id
			                 AND c.status = 'pending_payment')
			FROM subscriptions s
			LEFT JOIN subscription_items i ON i.billing_subscription_id = s.billing_subscription_id AND i.kind = 'seats'
			LEFT JOIN plan_prices p ON p.price_id = i.price_id
			LEFT JOIN plan_prices sp ON sp.price_id = i.scheduled_price_id
			WHERE s.provider = $1 AND s.provider_subscription_id IS NOT NULL`, provider)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var b port.ReconSubscription
			if err := rows.Scan(&id, &b.TenantID, &b.Status, &b.Plan, &b.Quantity, &b.PriceRef, &b.ScheduledPriceRef, &b.PendingUpdate); err != nil {
				return err
			}
			out[id] = b
		}
		return rows.Err()
	})
	return out, err
}

// InvoiceMirrors implements port.ReconRepo.
func (r *ReconRepository) InvoiceMirrors(ctx context.Context, provider string, ids []string) (map[string]port.ReconInvoice, error) {
	out := map[string]port.ReconInvoice{}
	err := inTx(ctx, r.sys, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT provider_invoice_id, tenant_id::text, status::text, currency, amount_due_minor, amount_paid_minor
			FROM invoices WHERE provider = $1 AND provider_invoice_id = ANY($2)`, provider, ids)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var m port.ReconInvoice
			if err := rows.Scan(&id, &m.TenantID, &m.Status, &m.Currency, &m.AmountDueMinor, &m.AmountPaidMinor); err != nil {
				return err
			}
			out[id] = m
		}
		return rows.Err()
	})
	return out, err
}

// KnownIDs implements port.ReconRepo: which of ids exist as ledger events
// ("events") or mirrored invoices ("invoices").
func (r *ReconRepository) KnownIDs(ctx context.Context, provider, kind string, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	query := `SELECT provider_event_id FROM payment_event_ledger WHERE provider = $1 AND provider_event_id = ANY($2)`
	if kind == "invoices" {
		query = `SELECT provider_invoice_id FROM invoices WHERE provider = $1 AND provider_invoice_id = ANY($2)`
	}
	err := inTx(ctx, r.sys, func(q querier) error {
		rows, err := q.Query(ctx, query, provider, ids)
		if err != nil {
			return err
		}
		ids, err := collect(rows, scanOne[string])
		for _, id := range ids {
			out[id] = true
		}
		return err
	})
	return out, err
}

// UnknownCustomerEvents implements port.ReconRepo.
func (r *ReconRepository) UnknownCustomerEvents(ctx context.Context, provider string, from, to time.Time) ([]string, error) {
	var out []string
	err := inTx(ctx, r.sys, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT provider_event_id FROM payment_event_ledger WHERE provider = $1 AND outcome = 'failed:unknown_customer'
			AND provider_created_at >= $2 AND provider_created_at < $3`, provider, from, to)
		if err != nil {
			return err
		}
		out, err = collect(rows, scanOne[string])
		return err
	})
	return out, err
}
