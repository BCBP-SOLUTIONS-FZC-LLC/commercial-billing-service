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
	switch sqlState(err) {
	case "23505":
		return domain.NewError(domain.ErrReconciliationRunning, "a reconciliation run for this provider is queued or running")
	case "23514":
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
// → (tenant, status) for the provider.
func (r *ReconRepository) BillingSubscriptions(ctx context.Context, provider string) (map[string][2]string, error) {
	out := map[string][2]string{}
	err := inTx(ctx, r.sys, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT provider_subscription_id, tenant_id::text, status::text FROM subscriptions
			WHERE provider = $1 AND provider_subscription_id IS NOT NULL`, provider)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, t, st string
			if err := rows.Scan(&id, &t, &st); err != nil {
				return err
			}
			out[id] = [2]string{t, st}
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
