package app

import (
	"context"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

type fakeEmissions struct {
	port.EmissionRepo
	due      []domain.ScheduledEmission
	finished map[string]*string
}

func (f *fakeEmissions) DueTenants(context.Context, time.Time, int) ([]string, error) {
	if len(f.due) == 0 {
		return nil, nil
	}
	return []string{tenantA}, nil
}

func (f *fakeEmissions) ClaimDue(context.Context, string, time.Time) ([]domain.ScheduledEmission, error) {
	d := f.due
	f.due = nil
	return d, nil
}

func (f *fakeEmissions) Finish(_ context.Context, id string, ev *string) error {
	f.finished[id] = ev
	return nil
}

type fakeEnqueuer struct{ got []domain.EventPayload }

func (f *fakeEnqueuer) Enqueue(_ context.Context, p domain.EventPayload, _ port.EventMeta) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	f.got = append(f.got, p)
	return "evt-1", nil
}

func emission(id, builder string) domain.ScheduledEmission {
	return domain.ScheduledEmission{EmissionID: id, TenantID: tenantA, EventType: domain.EventTenantSeatsChanged, PayloadBuilder: builder, RefID: "s1"}
}

// BIL-EVT-6 / BIL-EVT-4: the delayed seat event carries the seat count at
// emission time; nothing is published for a subscription no longer paid.
func TestBIL_EVT_6_EmissionWorker(t *testing.T) {
	subs := &fakeSubs{sub: &domain.Subscription{BillingSubscriptionID: "s1", TenantID: tenantA, Status: domain.StatusActive},
		item: domain.SubscriptionItem{Quantity: 14}}
	em := &fakeEmissions{due: []domain.ScheduledEmission{emission("a", domain.BuilderSeatsAfterConversion)}, finished: map[string]*string{}}
	enq := &fakeEnqueuer{}
	lock := &fakeLock{}
	w := &EmissionWorker{Emissions: em, Subs: subs, Tx: &fakeTx{}, Lock: lock, Events: enq, Now: func() time.Time { return now }}
	if n, err := w.RunOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	p, ok := enq.got[0].(domain.TenantSeatsChanged)
	if !ok || p.LicensedSeats != 14 || p.Reason != domain.SeatsConversion || p.BillingSubscriptionID != "s1" {
		t.Fatalf("payload %+v", enq.got)
	}
	if em.finished["a"] == nil || *em.finished["a"] != "evt-1" || len(lock.locked) != 1 {
		t.Fatalf("finish %+v lock %v", em.finished, lock.locked)
	}

	for _, st := range []domain.SubscriptionStatus{domain.StatusIncomplete, domain.StatusCancelled, domain.StatusEnded} {
		subs.sub.Status = st
		em.due = []domain.ScheduledEmission{emission(string(st), domain.BuilderSeatsResync)}
		enq.got = nil
		if _, err := w.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if f, ok := em.finished[string(st)]; !ok || f != nil || len(enq.got) != 0 {
			t.Errorf("%s: not cancelled (%v, %v)", st, f, enq.got)
		}
	}
	// A missing subscription cancels rather than retrying forever.
	subs.sub = nil
	em.due = []domain.ScheduledEmission{emission("gone", domain.BuilderSeatsResync)}
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f, ok := em.finished["gone"]; !ok || f != nil {
		t.Errorf("missing subscription: %v", f)
	}
}
