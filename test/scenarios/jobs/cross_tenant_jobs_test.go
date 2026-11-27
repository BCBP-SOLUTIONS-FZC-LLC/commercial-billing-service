//go:build integration

package jobs_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
)

// BIL-OPS-5: an overdue saga raises BillingConversionSagaOverdue (SEV-2)
// once; a failure selecting overdue sagas fails the run before any alert;
// a failure selecting due sagas fails the run after the alert (the overdue
// check runs first).
func TestSagaSweeperAlertsOnceAndFailsOnSelection(t *testing.T) {
	e := newEnv(t)
	tenant, sub := e.active("pro", 3)
	e.exec(`UPDATE conversion_sagas SET state = 'awaiting_tenant_converted', confirmed_at = NULL,
		payment_confirmed_at = now() - interval '1 hour', alerted_at = NULL WHERE billing_subscription_id = $1`, sub)
	e.f.fail("Jobs.OverdueSagas", 0)
	e.runErr(e.svc.SweepSagas)
	e.f.clear()
	if n := scalar[int](t, `SELECT count(*) FROM conversion_sagas WHERE tenant_id = $1 AND alerted_at IS NOT NULL`, tenant); n != 0 {
		t.Fatal("alerted by a failed run")
	}
	e.f.fail("Sagas.Due", 0)
	e.runErr(e.svc.SweepSagas)
	e.f.clear()
	if !e.log.has("ERROR BillingConversionSagaOverdue") {
		t.Fatal("SEV-2 not logged before the saga re-runs")
	}

	e.run(e.svc.SweepSagas)
	if e.met.get("sagas_overdue:1") == 0 {
		t.Fatalf("gauge %v", e.met.counts)
	}
	if n := scalar[int](t, `SELECT count(*) FROM conversion_sagas WHERE tenant_id = $1 AND alerted_at IS NOT NULL`, tenant); n != 1 {
		t.Fatal("alerted_at not set")
	}
}

// §8.6.4 / BIL-EVT-5: selection failure fails the run; I-11, item and
// resync-count failures fail the tenant only; a second mismatch within 24 h
// is SEV-3.
func TestSeatProjectionFailuresAndRepeatedMismatch(t *testing.T) {
	e := newEnv(t)
	tenant, _ := e.active("pro", 10)
	e.f.fail("Jobs.LiveSubscriptions", 0)
	e.runErr(e.svc.CheckSeatProjection)
	e.f.clear()

	e.now = time.Now().UTC().Add(11 * time.Minute)
	e.fakes.Core.Usage = port.SeatUsage{ActiveUsers: 3, LicensedSeats: 8}
	resyncs := func() int {
		return scalar[int](t, `SELECT count(*) FROM scheduled_emissions WHERE tenant_id = $1 AND payload_builder = 'seats_resync'`, tenant)
	}
	failOnce := func(setup func()) {
		t.Helper()
		setup()
		if res := e.run(e.svc.CheckSeatProjection); res.Failed != 1 {
			t.Fatalf("result %+v", res)
		}
		if resyncs() != 0 {
			t.Fatal("resync queued by a failed check")
		}
		e.f.clear()
	}
	failOnce(func() { e.fakes.Core.Err = fmt.Errorf("I-11: %w", errInjected) })
	e.fakes.Core.Err = nil
	failOnce(func() { e.f.fail("Subs.Item", 0) })
	failOnce(func() { e.f.fail("Jobs.RecentResyncs", 0) })

	e.run(e.svc.CheckSeatProjection)
	if resyncs() != 1 || !e.log.has("WARN seat projection mismatch; resync queued") {
		t.Fatalf("first mismatch: %d resyncs", resyncs())
	}
	// The per-run gauge BillingSeatProjectionDrift reads: one mismatch, no
	// repeat; then the repeat within 24 h.
	if e.met.get("seat_projection_run:1:0") != 1 {
		t.Fatalf("first run gauge %v", e.met.counts)
	}
	e.run(e.svc.CheckSeatProjection)
	if !e.log.has("ERROR seat projection mismatch again within 24 h") {
		t.Fatal("second mismatch not SEV-3")
	}
	if e.met.get("seat_projection_run:1:1") != 1 {
		t.Fatalf("repeat run gauge %v", e.met.counts)
	}
}

// fullPage is a jobBatch (200) page of live subscriptions; the job must
// ask for the next page after the last id.
func fullPage(tenant, sub string, at time.Time, same bool) []port.LiveSubscription {
	page := make([]port.LiveSubscription, 200)
	for i := range page {
		id := sub
		if !same {
			id = fmt.Sprintf("00000000-0000-0000-0000-%012d", i)
		}
		page[i] = port.LiveSubscription{TenantID: tenant, BillingSubscriptionID: id, ItemUpdatedAt: at}
	}
	return page
}

// seat-projection-check pages through live subscriptions by id.
func TestSeatProjectionPagesPastFullBatch(t *testing.T) {
	e := newEnv(t)
	e.now = time.Now().UTC()
	e.jobs.firstPage = fullPage(newTenant(), "", e.now, false) // all changed recently: skipped
	res := e.run(e.svc.CheckSeatProjection)
	if res.Handled != 200 || res.Failed != 0 {
		t.Fatalf("result %+v", res)
	}
	if len(e.jobs.afters) != 2 || e.jobs.afters[1] != "00000000-0000-0000-0000-000000000199" {
		t.Fatalf("pages requested after %q", e.jobs.afters)
	}
}

// §15.3 / BIL-OPS-9: selection failure fails the run; an unloaded adapter
// or a provider failure clearing the customer's email leaves the contact
// for the next run; a provider customer already gone (not found) does not
// block minimisation.
func TestPIIMinimisationFailuresAndMissingProviderCustomer(t *testing.T) {
	e := newEnv(t)
	tenant, _ := e.active("pro", 3)
	e.exec(`UPDATE billing_accounts SET billing_contact_name = 'Asha Rao', billing_contact_email = 'billing@acme.example',
		iam_lifecycle = 'offboarded', closed_at = now() - interval '91 days' WHERE tenant_id = $1`, tenant)
	contact := func() string {
		return scalar[string](t, `SELECT coalesce(billing_contact_email,'∅')||'/'||(pii_minimised_at IS NOT NULL) FROM billing_accounts WHERE tenant_id = $1`, tenant)
	}
	e.f.fail("Jobs.PIICandidates", 0)
	e.runErr(e.svc.MinimisePII)
	e.f.clear()

	failOnce := func() {
		t.Helper()
		if res := e.run(e.svc.MinimisePII); res.Failed != 1 {
			t.Fatalf("result %+v", res)
		}
		if got := contact(); got != "billing@acme.example/false" {
			t.Fatalf("contact after a failed run %s", got)
		}
		if !e.log.has("WARN pii-minimisation: account not minimised") {
			t.Fatal("not logged")
		}
		e.f.clear()
	}
	stripe := e.svc.Providers
	e.unload()
	failOnce()
	e.svc.Providers = stripe
	e.f.fail("prov.UpdateCustomer", 0)
	failOnce()

	e.f.failN("prov.UpdateCustomer", 0, 1, port.ErrNotFound)
	e.run(e.svc.MinimisePII)
	if got := contact(); got != "∅/true" {
		t.Fatalf("contact %s", got)
	}
}

// §9.2 idempotency-prune: selection failure fails the run; a tenant whose
// prune fails is counted failed and keeps its records.
func TestIdempotencyPruneFailures(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, _ := e.active("pro", 3)
	if _, err := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 1}); err != nil {
		t.Fatal(err)
	}
	e.now = time.Now().UTC().Add(time.Hour)
	e.f.fail("Jobs.ExpiredIdempotencyTenants", 0)
	e.runErr(e.svc.PruneIdempotency)
	e.f.clear()
	e.f.fail("Jobs.PruneIdempotency", 0)
	if res := e.run(e.svc.PruneIdempotency); res.Failed != 1 || res.Handled != 0 {
		t.Fatalf("result %+v", res)
	}
	if n := scalar[int](t, `SELECT count(*) FROM idempotency_records WHERE tenant_id = $1 AND scope = 'preview'`, tenant); n != 1 {
		t.Fatalf("records after a failed prune %d", n)
	}
	e.f.clear()
	e.run(e.svc.PruneIdempotency)
	if n := scalar[int](t, `SELECT count(*) FROM idempotency_records WHERE tenant_id = $1 AND scope = 'preview'`, tenant); n != 0 {
		t.Fatalf("records after prune %d", n)
	}
}
