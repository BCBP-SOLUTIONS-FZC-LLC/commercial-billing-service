//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

func ledgerRow(tenant *string, id string, created time.Time) domain.LedgerEntry {
	return domain.LedgerEntry{Provider: "stripe", ProviderEventID: id, EventType: "invoice.paid", NormalisedKind: "invoice_paid",
		TenantID: tenant, ProviderCreatedAt: created, Payload: []byte(`{}`)}
}

// BIL-PAY-5 and §8.15: one tenant's rows in provider order; a tenant with a
// row in live processing is skipped; expired claims are reclaimable; parked
// rows are never claimed; backoff times are respected.
func TestBIL_PAY_5_ClaimOrderAndRetry(t *testing.T) {
	ctx := context.Background()
	mustExec(t, superPool, `DELETE FROM payment_event_ledger`)
	ledger := pgadapter.NewLedgerRepository(systemPool)
	tA, tB := newID(), newID()
	base := time.Now().Add(-time.Hour)
	for _, e := range []domain.LedgerEntry{
		ledgerRow(&tA, "evt_a2", base.Add(2*time.Second)),
		ledgerRow(&tA, "evt_a1", base.Add(1*time.Second)),
		ledgerRow(&tB, "evt_b1", base.Add(3*time.Second)),
	} {
		if _, err := ledger.Insert(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	// First claim of one row: the oldest overall (a1).
	got, err := ledger.Claim(ctx, 1, time.Minute)
	if err != nil || len(got) != 1 || got[0].ProviderEventID != "evt_a1" || got[0].Status != domain.LedgerProcessing || got[0].Attempts != 1 {
		t.Fatalf("first claim: %+v %v", got, err)
	}
	// While a1 is processing, tenant A's a2 must wait; tenant B's b1 can go.
	got, _ = ledger.Claim(ctx, 10, time.Minute)
	if len(got) != 1 || got[0].ProviderEventID != "evt_b1" {
		t.Fatalf("second claim must skip tenant A: %+v", got)
	}
	// a1 done → a2 claimable.
	if err := ledger.Finish(ctx, "stripe", "evt_a1", domain.LedgerProcessed, "applied:invoice_paid", nil); err != nil {
		t.Fatal(err)
	}
	got, _ = ledger.Claim(ctx, 10, time.Minute)
	if len(got) != 1 || got[0].ProviderEventID != "evt_a2" {
		t.Fatalf("after a1 finished: %+v", got)
	}
	// a2 fails with a retry in the future: not claimable yet.
	future := time.Now().Add(time.Hour)
	_ = ledger.Finish(ctx, "stripe", "evt_a2", domain.LedgerFailed, "failed:DB_UNAVAILABLE", &future)
	// b1 parked (failed without retry): never claimable.
	_ = ledger.Finish(ctx, "stripe", "evt_b1", domain.LedgerFailed, "failed:unknown_customer", nil)
	if got, _ := ledger.Claim(ctx, 10, time.Minute); len(got) != 0 {
		t.Fatalf("future retry and parked rows must not be claimed: %+v", got)
	}
	// A due retry is claimable, with attempts counted.
	mustExec(t, superPool, `UPDATE payment_event_ledger SET next_attempt_at = now() - interval '1 second' WHERE provider_event_id = 'evt_a2'`)
	got, _ = ledger.Claim(ctx, 10, time.Minute)
	if len(got) != 1 || got[0].ProviderEventID != "evt_a2" || got[0].Attempts != 2 {
		t.Fatalf("due retry: %+v", got)
	}
	// An expired claim is reclaimable.
	mustExec(t, superPool, `UPDATE payment_event_ledger SET claimed_until = now() - interval '1 second' WHERE provider_event_id = 'evt_a2'`)
	got, _ = ledger.Claim(ctx, 10, time.Minute)
	if len(got) != 1 || got[0].ProviderEventID != "evt_a2" || got[0].Attempts != 3 {
		t.Fatalf("expired claim: %+v", got)
	}
	// Outcome bookkeeping.
	_ = ledger.Finish(ctx, "stripe", "evt_a2", domain.LedgerIgnored, "ignored:transition_not_allowed", nil)
	e, _ := ledger.Get(ctx, "stripe", "evt_a2")
	if e.Status != domain.LedgerIgnored || e.Outcome == nil || *e.Outcome != "ignored:transition_not_allowed" {
		t.Fatalf("finish: %+v", e)
	}
	parked, _ := ledger.Get(ctx, "stripe", "evt_b1")
	if parked.NextAttemptAt.Year() != 9999 {
		t.Fatalf("parked row must have next_attempt_at = 9999-12-31, got %v", parked.NextAttemptAt)
	}
}

// F-7 (rev 0.71): Defer reschedules a claimed row and gives back the attempt
// its claim counted, so a callback waiting on a conversion reversal never
// reaches WEBHOOK_MAX_ATTEMPTS.
func TestLedgerDeferGivesBackTheAttempt(t *testing.T) {
	ctx := context.Background()
	mustExec(t, superPool, `DELETE FROM payment_event_ledger`)
	ledger := pgadapter.NewLedgerRepository(systemPool)
	tA := newID()
	if _, err := ledger.Insert(ctx, ledgerRow(&tA, "evt_defer", time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		got, err := ledger.Claim(ctx, 10, time.Minute)
		if err != nil || len(got) != 1 || got[0].Attempts != 1 {
			t.Fatalf("claim %d: %+v %v", i, got, err)
		}
		if err := ledger.Defer(ctx, "stripe", "evt_defer", "failed:REQUEST_IN_PROGRESS", time.Now().Add(-time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	e, _ := ledger.Get(ctx, "stripe", "evt_defer")
	if e.Status != domain.LedgerFailed || e.Attempts != 0 || e.Outcome == nil || *e.Outcome != "failed:REQUEST_IN_PROGRESS" || e.NextAttemptAt.IsZero() {
		t.Fatalf("deferred row %+v", e)
	}
}

// BIL-PAY-5 (D-L11, rev 0.73): one tenant's rows claimed in the same batch
// come back in provider order, whatever order they were received in.
func TestBIL_PAY_5_ClaimedBatchInProviderOrder(t *testing.T) {
	ctx := context.Background()
	mustExec(t, superPool, `DELETE FROM payment_event_ledger`)
	ledger := pgadapter.NewLedgerRepository(systemPool)
	tA := newID()
	base := time.Now().Add(-time.Hour)
	for _, e := range []domain.LedgerEntry{
		ledgerRow(&tA, "evt_o3", base.Add(3*time.Second)),
		ledgerRow(&tA, "evt_o1", base.Add(1*time.Second)),
		ledgerRow(&tA, "evt_o2", base.Add(2*time.Second)),
	} {
		if _, err := ledger.Insert(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ledger.Claim(ctx, 10, time.Minute)
	if err != nil || len(got) != 3 || got[0].ProviderEventID != "evt_o1" || got[1].ProviderEventID != "evt_o2" || got[2].ProviderEventID != "evt_o3" {
		t.Fatalf("claimed %+v %v", got, err)
	}
}
