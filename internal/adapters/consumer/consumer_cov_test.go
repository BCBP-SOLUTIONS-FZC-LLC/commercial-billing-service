package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events/mock"
)

// recLog records warnings.
type recLog struct {
	mu    sync.Mutex
	warns []string
}

func (*recLog) Debug(string, map[string]any) {}
func (*recLog) Info(string, map[string]any)  {}
func (*recLog) Error(string, map[string]any) {}
func (l *recLog) Warn(msg string, _ map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
}

type failingDLQ struct{ dlq }

func (*failingDLQ) SendToDLQ(context.Context, string, []byte, map[string]string, string) error {
	return errors.New("sqs down")
}

// A payload that passes the schema check but does not decode is a schema violation.
func TestTenantBillingUndecodablePayload(t *testing.T) {
	uc := &app.LifecycleConsumer{Inbox: inbox{}, Lock: lock{}, Accounts: &accounts{}, MaxSkew: 300 * time.Second}
	err := tenantBilling(uc, validator, nil)(context.Background(), envelope("TenantSuspended", time.Now(), `{"tenant_id":42}`))
	if !errors.Is(err, errSchemaViolation) {
		t.Fatalf("err %v, want schema violation", err)
	}
}

func TestUsageBillingRejections(t *testing.T) {
	u := &usage{}
	log := &recLog{}
	h := usageBilling(&app.UsageConsumer{Inbox: inbox{}, Usage: u}, validator, log)
	ctx := context.Background()
	// Unexpected type: acknowledged with a warning, nothing written.
	if err := h(ctx, envelope("UsageSampled", time.Now(), `{}`)); err != nil || len(log.warns) != 1 {
		t.Fatalf("unexpected type: %v %v", err, log.warns)
	}
	for name, data := range map[string]string{
		"schema":   `{"period_start":"2026-11-01T00:00:00Z"}`,
		"decode":   `{"tenant_id":"` + tenant + `","tokens_total":"many"}`,
		"mismatch": `{"tenant_id":"00000000-0000-0000-0000-000000000001"}`,
	} {
		if err := h(ctx, envelope("UsagePeriodClosed", time.Now(), data)); !errors.Is(err, errSchemaViolation) {
			t.Errorf("%s: %v, want schema violation", name, err)
		}
	}
	if len(u.got) != 0 {
		t.Errorf("rejected periods written: %+v", u.got)
	}
}

func rejecting(err error) events.Handler {
	return func(context.Context, events.Envelope[json.RawMessage]) error { return err }
}

// Without a DLQ publisher (or outside a consumer) the error is returned so SQS retries.
func TestRouteRejectsWithoutDLQ(t *testing.T) {
	bad := errSchemaViolation
	if err := RouteRejectsToDLQ(rejecting(bad), nil, nil)(context.Background(), envelope("TenantSuspended", time.Now(), `{}`)); !errors.Is(err, bad) {
		t.Fatalf("no source message: %v", err)
	}
	c := &mock.Consumer{}
	c.SetHandler(RouteRejectsToDLQ(rejecting(bad), nil, nil))
	if err := c.Inject(envelope("TenantSuspended", time.Now(), `{}`)); !errors.Is(err, bad) {
		t.Fatalf("nil DLQ: %v", err)
	}
	// A retryable error is returned untouched.
	transient := errors.New("db down")
	c.SetHandler(RouteRejectsToDLQ(rejecting(transient), &dlq{}, nil))
	if err := c.Inject(envelope("TenantSuspended", time.Now(), `{}`)); !errors.Is(err, transient) {
		t.Fatalf("transient: %v", err)
	}
}

// A failed DLQ send keeps the message on the queue; a successful one is logged.
func TestRouteRejectsDLQOutcomesLogged(t *testing.T) {
	log := &recLog{}
	c := &mock.Consumer{}
	c.SetHandler(RouteRejectsToDLQ(rejecting(errSchemaViolation), &failingDLQ{}, log))
	if err := c.Inject(envelope("TenantSuspended", time.Now(), `{}`)); !errors.Is(err, errSchemaViolation) {
		t.Fatalf("DLQ send failure: %v", err)
	}
	d := &dlq{}
	c.SetHandler(RouteRejectsToDLQ(rejecting(errSchemaViolation), d, log))
	if err := c.Inject(envelope("TenantSuspended", time.Now(), `{}`)); err != nil || len(d.reasons) != 1 {
		t.Fatalf("dead-lettered: %v %v", err, d.reasons)
	}
	if len(log.warns) != 2 || log.warns[0] != "DLQ send failed; message stays on the queue" || log.warns[1] != "message dead-lettered" {
		t.Errorf("warnings %v", log.warns)
	}
}
