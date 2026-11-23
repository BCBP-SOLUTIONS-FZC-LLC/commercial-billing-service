package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events/mock"
)

const tenant = "3fa85f64-5717-4562-b3fc-2c963f66afa6"

type inbox struct{}

func (inbox) RunOnce(ctx context.Context, _, _, _, _ string, fn func(context.Context) error) error {
	return fn(ctx)
}

type lock struct{}

func (lock) LockTenant(context.Context, string) error { return nil }

type accounts struct {
	changes []domain.LifecycleChange
}

func (a *accounts) AccountForUpdate(context.Context, string) (domain.BillingAccount, bool, error) {
	return domain.BillingAccount{TenantID: tenant, IAMLifecycle: domain.LifecycleActive}, true, nil
}

func (a *accounts) ApplyLifecycle(_ context.Context, _ string, c domain.LifecycleChange) error {
	a.changes = append(a.changes, c)
	return nil
}

type usage struct{ got []domain.UsagePeriodRecord }

func (u *usage) Upsert(_ context.Context, r domain.UsagePeriodRecord) (bool, error) {
	u.got = append(u.got, r)
	return true, nil
}

type dlq struct{ reasons []string }

func (d *dlq) SendToDLQ(_ context.Context, _ string, _ []byte, _ map[string]string, reason string) error {
	d.reasons = append(d.reasons, reason)
	return nil
}
func (d *dlq) ResolveDLQ(context.Context, string) (string, error) { return "dlq", nil }

// validator stands in for eventbus.Schemas.ValidateConsumed.
func validator(_ string, payload []byte) error {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return err
	}
	if _, ok := m["tenant_id"]; !ok {
		return errors.New("tenant_id required")
	}
	return nil
}

func envelope(typ string, at time.Time, data string) events.Envelope[json.RawMessage] {
	e := events.NewEnvelope(typ, "iam-realm-provisioner", json.RawMessage(data), events.WithTenantID(tenant))
	e.Timestamp = at
	return e
}

func TestTenantBillingQueue(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	acc := &accounts{}
	uc := &app.LifecycleConsumer{Inbox: inbox{}, Lock: lock{}, Accounts: acc, MaxSkew: 300 * time.Second}
	d := &dlq{}
	c := &mock.Consumer{}
	c.SetHandler(RouteRejectsToDLQ(TenantBilling(uc, validator, nil), d, nil))

	// Applied: suspension with its source; export_s3_key never read.
	err := c.Inject(envelope("TenantSuspended", now.Add(-time.Minute),
		`{"tenant_id":"`+tenant+`","realm":"r","source":"operator","suspended_at":"2026-11-20T09:00:00Z","export_s3_key":"x"}`))
	if err != nil || len(acc.changes) != 1 || acc.changes[0].To != domain.LifecycleSuspended || !acc.changes[0].EventAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("suspended: %v %+v", err, acc.changes)
	}
	// Offboarded carries offboarded_at as closed_at.
	if err := c.Inject(envelope("TenantOffboarded", now, `{"tenant_id":"`+tenant+`","realm":"r","offboarded_at":"2026-11-20T09:30:00Z"}`)); err != nil {
		t.Fatal(err)
	}
	if ca := acc.changes[1].ClosedAt; ca == nil || ca.Format(time.RFC3339) != "2026-11-20T09:30:00Z" {
		t.Fatalf("closed_at %v", ca)
	}

	// Straight to the DLQ: future time, schema violation, tenant mismatch.
	_ = c.Inject(envelope("TenantReactivated", now.Add(10*time.Minute), `{"tenant_id":"`+tenant+`","realm":"r","source":"operator"}`))
	_ = c.Inject(envelope("TenantReactivated", now, `{"realm":"r"}`))
	_ = c.Inject(envelope("TenantReactivated", now, `{"tenant_id":"00000000-0000-0000-0000-000000000001","realm":"r","source":"operator"}`))
	want := []string{ReasonFutureTime, ReasonSchemaViolation, ReasonSchemaViolation}
	if len(d.reasons) != 3 || d.reasons[0] != want[0] || d.reasons[1] != want[1] || d.reasons[2] != want[2] {
		t.Fatalf("dlq reasons %v", d.reasons)
	}
	// Unexpected type: acknowledged, not dead-lettered, nothing written.
	n := len(acc.changes)
	if err := c.Inject(envelope("TrialStarted", now, `{}`)); err != nil || len(d.reasons) != 3 || len(acc.changes) != n {
		t.Fatalf("unexpected type: %v %v", err, d.reasons)
	}
}

func TestUsageBillingQueue(t *testing.T) {
	u := &usage{}
	uc := &app.UsageConsumer{Inbox: inbox{}, Usage: u}
	c := &mock.Consumer{}
	c.SetHandler(RouteRejectsToDLQ(UsageBilling(uc, validator, nil), &dlq{}, nil))
	at := time.Date(2026, 12, 1, 0, 0, 5, 0, time.UTC)
	e := envelope("UsagePeriodClosed", at, `{"tenant_id":"`+tenant+`","period_start":"2026-11-01T00:00:00Z","period_end":"2026-12-01T00:00:00Z",
		"tokens_total":1200,"api_requests_total":30,"addon_consumed":5,"limits":{"llm_tokens":500000}}`)
	if err := c.Inject(e); err != nil {
		t.Fatal(err)
	}
	r := u.got[0]
	if r.TokensTotal != 1200 || r.APIRequestsTotal != 30 || r.AddonConsumed != 5 || r.SourceEventID != e.ID ||
		!r.SourceEventTime.Equal(at) || string(r.Limits) != `{"llm_tokens":500000}` || r.PeriodStart.Month() != time.November {
		t.Fatalf("record %+v", r)
	}
}
