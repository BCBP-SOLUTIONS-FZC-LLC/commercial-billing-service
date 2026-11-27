//go:build integration

package jobs_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// hungRP is a Realm Provisioner that never answers RP-3 before the
// caller's context ends.
type hungRP struct{ port.RealmProvisionerClient }

func (hungRP) Convert(ctx context.Context, _ string, _ port.ConvertRequest, _ string) domain.RP3Answer {
	<-ctx.Done()
	return domain.RP3Answer{Status: 503}
}

// seq records tenant locks and provider calls in order.
type seq struct {
	mu  sync.Mutex
	log []string
}

func (s *seq) add(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, v)
}

func (s *seq) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.log...)
}

type seqLocker struct {
	port.TenantLocker
	s *seq
}

func (l seqLocker) LockTenant(ctx context.Context, tenantID string) error {
	l.s.add("lock " + tenantID)
	return l.TenantLocker.LockTenant(ctx, tenantID)
}

type seqProvider struct {
	*provider
	s *seq
}

func (p seqProvider) CreateSubscription(ctx context.Context, in port.SubscriptionInput, key string) (port.SubscriptionResult, error) {
	p.s.add("CreateSubscription")
	return p.provider.CreateSubscription(ctx, in, key)
}

func (p seqProvider) ConfirmInitialPayment(ctx context.Context, ref port.SubscriptionRef, key string) (port.PaymentOutcome, error) {
	p.s.add("ConfirmInitialPayment")
	return p.provider.ConfirmInitialPayment(ctx, ref, key)
}

// BIL-OPS-5: with the Realm Provisioner hung (each RP-3 waits until its
// context ends) the sweeper still pages the overdue saga, and returns
// before the job's deadline: the overdue check runs first and the saga
// re-runs get only part of the remaining time.
func TestBIL_OPS_5_SweeperAlertsBeforeHungSagaReruns(t *testing.T) {
	e := newEnv(t)
	tenant, sub := e.active("pro", 3)
	e.exec(`UPDATE conversion_sagas SET state = 'rp_convert_pending', confirmed_at = NULL, next_attempt_at = now() - interval '1 minute',
		payment_confirmed_at = now() - interval '1 hour', alerted_at = NULL WHERE billing_subscription_id = $1`, sub)
	e.svc.RP = hungRP{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := e.svc.SweepSagas(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("the sweep ran into the job deadline")
	}
	if !e.log.has("ERROR BillingConversionSagaOverdue") {
		t.Fatal("SEV-2 not logged")
	}
	if n := scalar[int](t, `SELECT count(*) FROM conversion_sagas WHERE tenant_id = $1 AND alerted_at IS NOT NULL`, tenant); n != 1 {
		t.Fatal("alerted_at not set")
	}
}

// BIL-CONC-1: the sweeper's alerted_at write takes the tenant lock.
func TestBIL_CONC_1_SagaAlertTakesTenantLock(t *testing.T) {
	e := newEnv(t)
	tenant, sub := e.active("pro", 3)
	e.exec(`UPDATE conversion_sagas SET state = 'awaiting_tenant_converted', confirmed_at = NULL,
		payment_confirmed_at = now() - interval '1 hour', alerted_at = NULL WHERE billing_subscription_id = $1`, sub)
	s := &seq{}
	e.svc.Lock = seqLocker{e.svc.Lock, s}
	e.run(e.svc.SweepSagas)
	if got := s.all(); len(got) != 1 || got[0] != "lock "+tenant {
		t.Fatalf("locks %v", got)
	}
}

// BIL-CONC-1: B-I1 records the provider subscription id under the tenant
// lock (between creating the provider subscription and confirming it).
func TestBIL_CONC_1_DirectSignupRecordsProviderSubUnderLock(t *testing.T) {
	e := newEnv(t)
	s := &seq{}
	e.svc.Lock = seqLocker{e.svc.Lock, s}
	reg := e.fakes.Registry()
	reg["stripe"] = seqProvider{e.prov, s}
	e.svc.Providers = reg
	tenant := newTenant()
	if _, err := e.svc.AuthorizeAndCharge(context.Background(), app.Caller{Principal: "bff"}, app.AuthorizeChargeRequest{TenantID: tenant,
		PlanCode: "pro", LicensedSeats: 3, PaymentMethodRef: "pm_1"}); err != nil {
		t.Fatal(err)
	}
	got := s.all()
	created, confirmed := -1, -1
	for i, v := range got {
		switch v {
		case "CreateSubscription":
			created = i
		case "ConfirmInitialPayment":
			confirmed = i
		}
	}
	if created < 0 || confirmed != created+2 || got[created+1] != "lock "+tenant {
		t.Fatalf("calls %v: want CreateSubscription, lock, ConfirmInitialPayment", got)
	}
}
