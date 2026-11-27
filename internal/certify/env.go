// Package certify is the provider certification suite (LLD §14.7, §19.6):
// it runs a payment adapter through Billing's real app layer — use cases,
// webhook intake (B-W1), the callback processor and the CronJob use cases —
// against a throwaway database, and records one result per case of the
// suite version (domain.CertSuite).
//
// IAM (Core, Realm Provisioner) and Entitlements are stubbed: the suite
// certifies the provider and Billing's mapping of it, not IAM.
//
// Targets:
//   - TargetTestMode — the provider's test mode. Cases assert provider
//     behaviour (state after the call, events, renewals on a Test Clock).
//     Only a test-mode record may be submitted (B-O14).
//   - TargetStripeMock — stripe-mock (stateless fixtures). Cases assert the
//     call path and Billing's mapping only; the record is a rehearsal and
//     carries provider_account_ref "stripe-mock", which cmd/certify refuses
//     to submit.
package certify

import (
	"context"
	"sync"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// Target is where the provider calls go.
type Target string

// Targets.
const (
	TargetTestMode   Target = "test-mode"
	TargetStripeMock Target = "stripe-mock"
)

// MockAccountRef marks a rehearsal record (never submittable).
const MockAccountRef = "stripe-mock"

// Env is what a case runs against.
type Env struct {
	Target   Target
	Svc      *app.Service
	Provider port.PaymentProvider
	DB       *DB
	Core     *CoreStub
	Deliver  *Deliverer
	Clock    Clock
	// Prices are the provider price ids the run created, by plan.
	Prices    map[domain.PlanCode]string
	TokenPack string
	RunStart  time.Time
	Log       port.Logger
}

// Stateful reports whether cases can assert provider state (test mode).
func (e *Env) Stateful() bool { return e.Target == TargetTestMode }

// Clock moves time for renewals: a Stripe Test Clock in test mode, Billing's
// clock only against stripe-mock.
type Clock interface {
	Now() time.Time
	Advance(ctx context.Context, to time.Time) error
}

// FakeClock is Billing's clock alone (stripe-mock).
type FakeClock struct {
	mu sync.Mutex
	t  time.Time
}

// NewFakeClock starts at start.
func NewFakeClock(start time.Time) *FakeClock { return &FakeClock{t: start} }

// Now implements Clock.
func (c *FakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

// Advance implements Clock.
func (c *FakeClock) Advance(_ context.Context, to time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if to.After(c.t) {
		c.t = to
	}
	return nil
}

// ── IAM and Entitlements stubs (not under certification) ────────────────

// CoreStub answers I-11 and I-18.
type CoreStub struct {
	mu    sync.Mutex
	Usage port.SeatUsage
	Check port.PlanChangeCheck
}

// SeatUsage implements port.CoreClient.
func (c *CoreStub) SeatUsage(context.Context, string) (port.SeatUsage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Usage, nil
}

// PlanChangeCheck implements port.CoreClient.
func (c *CoreStub) PlanChangeCheck(context.Context, string, domain.PlanCode) (port.PlanChangeCheck, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Check, nil
}

// SetUsage sets the next I-11 answer.
func (c *CoreStub) SetUsage(active, licensed int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Usage = port.SeatUsage{ActiveUsers: active, LicensedSeats: licensed}
}

// RPStub accepts every conversion.
type RPStub struct{}

// Convert implements port.RealmProvisionerClient.
func (RPStub) Convert(context.Context, string, port.ConvertRequest, string) domain.RP3Answer {
	return domain.RP3Answer{Status: 200}
}

// RevertConversion implements port.RealmProvisionerClient.
func (RPStub) RevertConversion(context.Context, string, string, string) domain.RP3Answer {
	return domain.RP3Answer{Status: 200}
}

// Realm implements port.RealmProvisionerClient.
func (RPStub) Realm(context.Context, string) (port.RealmView, error) { return port.RealmView{}, nil }

// EntitlementsStub reports a finite, eligible token quota.
type EntitlementsStub struct{}

// TokenQuota implements port.EntitlementsReader.
func (EntitlementsStub) TokenQuota(context.Context, string) (port.TokenQuota, error) {
	limit := int64(5_000_000)
	return port.TokenQuota{MonthlyLimit: &limit, Eligibility: "eligible"}, nil
}

// Registry serves the adapter under certification for new accounts: the
// run certifies it, so the BIL-PROV-1 gate does not apply inside the suite.
type Registry struct{ P port.PaymentProvider }

// Loaded implements port.ProviderLookup.
func (r Registry) Loaded(name string) (port.PaymentProvider, bool) {
	if name == r.P.Name() {
		return r.P, true
	}
	return nil, false
}

// ForNewAccount implements app.ProviderRegistry.
func (r Registry) ForNewAccount(name string) (port.PaymentProvider, error) {
	if p, ok := r.Loaded(name); ok {
		return p, nil
	}
	return nil, port.ProviderNotEnabled(name, "is not under certification")
}
