package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

type fakeInbox struct {
	seen map[string]bool
	runs int
}

func (f *fakeInbox) RunOnce(ctx context.Context, consumer, id, _, _ string, fn func(context.Context) error) error {
	if f.seen[consumer+id] {
		return nil
	}
	f.runs++
	if err := fn(ctx); err != nil {
		return err
	}
	f.seen[consumer+id] = true
	return nil
}

type fakeLock struct{ locked []string }

func (f *fakeLock) LockTenant(_ context.Context, t string) error {
	f.locked = append(f.locked, t)
	return nil
}

type fakeAccounts struct {
	acct    *domain.BillingAccount
	applied []domain.LifecycleChange
}

func (f *fakeAccounts) AccountForUpdate(context.Context, string) (domain.BillingAccount, bool, error) {
	if f.acct == nil {
		return domain.BillingAccount{}, false, nil
	}
	return *f.acct, true, nil
}

func (f *fakeAccounts) ApplyLifecycle(_ context.Context, _ string, c domain.LifecycleChange) error {
	f.applied = append(f.applied, c)
	f.acct.IAMLifecycle, f.acct.IAMLastEventAt = c.To, &c.EventAt
	return nil
}

type fakeSubs struct {
	port.SubscriptionRepo
	sub  *domain.Subscription
	item domain.SubscriptionItem
}

func (f *fakeSubs) Live(context.Context, string) (domain.Subscription, bool, error) {
	if f.sub == nil {
		return domain.Subscription{}, false, nil
	}
	return *f.sub, true, nil
}

func (f *fakeSubs) Get(_ context.Context, id string) (domain.Subscription, error) {
	if f.sub == nil || f.sub.BillingSubscriptionID != id {
		return domain.Subscription{}, domain.NewError(domain.ErrSubscriptionNotFound, "nope")
	}
	return *f.sub, nil
}

func (f *fakeSubs) Item(context.Context, string) (domain.SubscriptionItem, error) { return f.item, nil }

type capturingLog struct{ msgs []string }

func (l *capturingLog) Debug(string, map[string]any)    {}
func (l *capturingLog) Info(m string, _ map[string]any) { l.msgs = append(l.msgs, m) }
func (l *capturingLog) Warn(m string, _ map[string]any) { l.msgs = append(l.msgs, m) }
func (l *capturingLog) Error(m string, _ map[string]any) {
	l.msgs = append(l.msgs, m)
}

const tenantA = "3fa85f64-5717-4562-b3fc-2c963f66afa6"

func newLifecycle(acct *domain.BillingAccount, subs *fakeSubs) (*LifecycleConsumer, *fakeAccounts, *fakeInbox, *capturingLog) {
	acc := &fakeAccounts{acct: acct}
	in := &fakeInbox{seen: map[string]bool{}}
	log := &capturingLog{}
	c := &LifecycleConsumer{Inbox: in, Lock: &fakeLock{}, Accounts: acc, MaxSkew: 300 * time.Second,
		Now: func() time.Time { return now }, Log: log}
	if subs != nil {
		c.Subs = subs
	}
	return c, acc, in, log
}

func ev(id, typ string, at time.Time) TenantLifecycleEvent {
	return TenantLifecycleEvent{EventID: id, Type: typ, Time: at, TenantID: tenantA}
}

// BIL-EVT-3: last writer wins on envelope time; equal is stale; offboarded
// is terminal; a future time beyond the skew is a poison pill.
func TestBIL_EVT_3_LifecycleRecency(t *testing.T) {
	ctx := context.Background()
	c, acc, _, _ := newLifecycle(&domain.BillingAccount{TenantID: tenantA, IAMLifecycle: domain.LifecycleActive}, nil)

	t1 := now.Add(-10 * time.Minute)
	sus := ev("e1", EventTenantSuspended, t1)
	sus.Source = domain.SuspensionOperator
	if err := c.Handle(ctx, sus); err != nil {
		t.Fatal(err)
	}
	if acc.acct.IAMLifecycle != domain.LifecycleSuspended || *acc.applied[0].SuspensionSource != "operator" {
		t.Fatalf("suspend: %+v", acc.applied)
	}
	// Older and equal-time events are skipped.
	for _, at := range []time.Time{t1.Add(-time.Second), t1} {
		if err := c.Handle(ctx, ev("old"+at.String(), EventTenantReactivated, at)); err != nil {
			t.Fatal(err)
		}
	}
	if len(acc.applied) != 1 {
		t.Fatalf("stale events applied: %+v", acc.applied)
	}
	// Newer reactivation applies and clears the source.
	if err := c.Handle(ctx, ev("e2", EventTenantReactivated, t1.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	if last := acc.applied[len(acc.applied)-1]; last.To != domain.LifecycleActive || last.SuspensionSource != nil {
		t.Fatalf("reactivate: %+v", last)
	}
	// Offboarded is terminal, even for a newer event.
	off := ev("e3", EventTenantOffboarded, t1.Add(2*time.Second))
	off.OffboardedAt = t1.Add(2 * time.Second)
	if err := c.Handle(ctx, off); err != nil {
		t.Fatal(err)
	}
	if last := acc.applied[len(acc.applied)-1]; last.To != domain.LifecycleOffboarded || last.ClosedAt == nil || !last.ClosedAt.Equal(off.OffboardedAt) {
		t.Fatalf("offboard: %+v", last)
	}
	n := len(acc.applied)
	if err := c.Handle(ctx, ev("e4", EventTenantReactivated, now)); err != nil || len(acc.applied) != n {
		t.Fatalf("event after offboarded applied: %v %d", err, len(acc.applied))
	}
	// Future-time clamp: 301 s ahead → poison pill; 299 s ahead is fine.
	if err := c.Handle(ctx, ev("e5", EventTenantSuspended, now.Add(301*time.Second))); !errors.Is(err, ErrFutureTime) {
		t.Fatalf("future: %v", err)
	}
	c2, acc2, _, _ := newLifecycle(&domain.BillingAccount{TenantID: tenantA, IAMLifecycle: domain.LifecycleActive}, nil)
	s := ev("e6", EventTenantSuspended, now.Add(299*time.Second))
	s.Source = domain.SuspensionOperator
	if err := c2.Handle(ctx, s); err != nil || len(acc2.applied) != 1 {
		t.Fatalf("within skew: %v %+v", err, acc2.applied)
	}
}

func TestLifecycleConsumerEdges(t *testing.T) {
	ctx := context.Background()
	// Exactly once per event id.
	c, acc, in, _ := newLifecycle(&domain.BillingAccount{TenantID: tenantA, IAMLifecycle: domain.LifecycleTrial}, nil)
	e := ev("dup", EventTenantConverted, now.Add(-time.Minute))
	for range 2 {
		if err := c.Handle(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if in.runs != 1 || len(acc.applied) != 1 || acc.applied[0].To != domain.LifecycleActive {
		t.Fatalf("dedup: runs=%d %+v", in.runs, acc.applied)
	}
	// No account (trial tenant): acknowledged, nothing written.
	c, acc, _, log := newLifecycle(nil, nil)
	if err := c.Handle(ctx, ev("x", EventTenantSuspended, now)); err != nil || len(acc.applied) != 0 || len(log.msgs) != 1 {
		t.Fatalf("no account: %v %+v %v", err, acc.applied, log.msgs)
	}
	// Unsupported type.
	if err := c.Handle(ctx, ev("y", "TrialStarted", now)); !errors.Is(err, ErrUnsupportedEvent) {
		t.Fatalf("unsupported: %v", err)
	}
	// Billing lapse while Billing is not cancelled → SEV-3 mismatch logged.
	subs := &fakeSubs{sub: &domain.Subscription{BillingSubscriptionID: "s1", Status: domain.StatusActive}}
	c, _, _, log = newLifecycle(&domain.BillingAccount{TenantID: tenantA, IAMLifecycle: domain.LifecycleActive}, subs)
	lapse := ev("z", EventTenantSuspended, now)
	lapse.Source = domain.SuspensionBillingLapse
	if err := c.Handle(ctx, lapse); err != nil {
		t.Fatal(err)
	}
	if len(log.msgs) != 1 || log.msgs[0][:19] != "lifecycle_mismatch:" {
		t.Fatalf("mismatch: %v", log.msgs)
	}
}

type fakeConversions struct{ matched bool }

func (f fakeConversions) ConfirmConversion(context.Context, TenantLifecycleEvent) (bool, error) {
	return f.matched, nil
}

// TenantConverted with no matching saga writes nothing (§7.1).
func TestConvertedWithoutSaga(t *testing.T) {
	c, acc, _, _ := newLifecycle(&domain.BillingAccount{TenantID: tenantA, IAMLifecycle: domain.LifecycleTrial}, nil)
	c.Conversions = fakeConversions{matched: false}
	if err := c.Handle(context.Background(), ev("c1", EventTenantConverted, now)); err != nil || len(acc.applied) != 0 {
		t.Fatalf("%v %+v", err, acc.applied)
	}
	c.Conversions = fakeConversions{matched: true}
	if err := c.Handle(context.Background(), ev("c2", EventTenantConverted, now)); err != nil || len(acc.applied) != 1 {
		t.Fatalf("%v %+v", err, acc.applied)
	}
}
