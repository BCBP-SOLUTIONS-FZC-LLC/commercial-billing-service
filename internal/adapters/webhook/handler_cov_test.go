package webhook

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

type dupLedger struct{ fakeLedger }

func (*dupLedger) Insert(context.Context, domain.LedgerEntry) (bool, error) { return false, nil }

type failingCustomers struct{}

func (failingCustomers) TenantForCustomer(context.Context, string, string) (string, bool, error) {
	return "", false, errors.New("db down")
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// A redelivery (ledger primary key hit) is 200 and counted as a duplicate.
func TestB_W1DuplicateCounted(t *testing.T) {
	var dups []string
	h := &Handler{Providers: lookup{"stripe": fakeProvider{webhooks: true, verifyOK: true}}, Ledger: &dupLedger{}, Customers: customers{},
		OnDuplicate: func(p, k string) { dups = append(dups, p+"/"+k) }}
	if w := serve(h, "/api/v1/webhooks/stripe", `{}`); w.Code != http.StatusOK {
		t.Fatalf("duplicate: %d", w.Code)
	}
	if len(dups) != 1 || dups[0] != "stripe/invoice_paid" {
		t.Errorf("duplicates %v", dups)
	}
}

// A customer-mapping read failure is a 503 (nothing stored, provider redelivers).
func TestB_W1CustomerLookupFailure(t *testing.T) {
	l := &fakeLedger{}
	h := &Handler{Providers: lookup{"stripe": fakeProvider{webhooks: true, verifyOK: true}}, Ledger: l, Customers: failingCustomers{}}
	w := serve(h, "/api/v1/webhooks/stripe", `{}`)
	if w.Code != http.StatusServiceUnavailable || len(l.inserted) != 0 || !isProblem(w) {
		t.Fatalf("lookup failure: %d stored=%d", w.Code, len(l.inserted))
	}
}

// MaxBodyBytes 0 falls back to the §12 default (256 KiB): a 100 KiB body is accepted.
func TestB_W1DefaultBodyLimit(t *testing.T) {
	l := &fakeLedger{}
	h := &Handler{Providers: lookup{"stripe": fakeProvider{webhooks: true, verifyOK: true}}, Ledger: l, Customers: customers{}}
	if w := serve(h, "/api/v1/webhooks/stripe", strings.Repeat("x", 100<<10)); w.Code != http.StatusOK || len(l.inserted) != 1 {
		t.Fatalf("100 KiB body: %d", w.Code)
	}
	if w := serve(h, "/api/v1/webhooks/stripe", strings.Repeat("x", 262145)); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over default limit: %d", w.Code)
	}
}

// An unreadable body is a binding problem, nothing stored.
func TestB_W1BodyReadError(t *testing.T) {
	l := &fakeLedger{}
	h := &Handler{Providers: lookup{"stripe": fakeProvider{webhooks: true, verifyOK: true}}, Ledger: l, Customers: customers{}}
	r := gin.New()
	h.Register(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/stripe", errReader{}))
	if w.Code != http.StatusBadRequest || len(l.inserted) != 0 || !isProblem(w) {
		t.Fatalf("read error: %d %s", w.Code, w.Body.String())
	}
}
