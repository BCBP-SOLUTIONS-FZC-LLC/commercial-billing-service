package webhook

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

func init() { gin.SetMode(gin.TestMode) }

type fakeProvider struct {
	port.PaymentProvider
	webhooks bool
	verifyOK bool
}

func (f fakeProvider) Capabilities() port.ProviderCapabilities {
	return port.ProviderCapabilities{WebhookRedelivery: f.webhooks}
}

func (f fakeProvider) VerifyAndNormalise(context.Context, http.Header, []byte) (domain.PaymentEvent, error) {
	if !f.verifyOK {
		return domain.PaymentEvent{}, port.ErrInvalidSignature
	}
	return domain.PaymentEvent{Provider: "stripe", ProviderEventID: "evt_1", EventType: "invoice.paid", Kind: domain.KindInvoicePaid,
		CustomerRef: "cus_1", Object: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: "in_1"},
		ProviderCreatedAt: time.Now(), Payload: []byte(`{}`)}, nil
}

type lookup map[string]port.PaymentProvider

func (l lookup) Loaded(n string) (port.PaymentProvider, bool) { p, ok := l[n]; return p, ok }

type fakeLedger struct {
	port.LedgerRepo
	inserted []domain.LedgerEntry
	err      error
}

func (f *fakeLedger) Insert(_ context.Context, e domain.LedgerEntry) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	f.inserted = append(f.inserted, e)
	return true, nil
}

type customers map[string]string

func (c customers) TenantForCustomer(_ context.Context, _, id string) (string, bool, error) {
	t, ok := c[id]
	return t, ok, nil
}

func serve(h *Handler, path, body string) *httptest.ResponseRecorder {
	r := gin.New()
	h.Register(r)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Stripe-Signature", "t=1,v1=abc")
	r.ServeHTTP(w, req)
	return w
}

func TestBIL_PAY_1_B_W1(t *testing.T) {
	failures := 0
	newHandler := func(p fakeProvider, l *fakeLedger) *Handler {
		return &Handler{Providers: lookup{"stripe": p, "manual": fakeProvider{}}, Ledger: l, Customers: customers{"cus_1": "t1"},
			MaxBodyBytes: 64, OnSignatureFailure: func(string) { failures++ }}
	}

	// Valid: stored with the tenant resolved from the customer mapping (BIL-PAY-3), 200.
	l := &fakeLedger{}
	w := serve(newHandler(fakeProvider{webhooks: true, verifyOK: true}, l), "/api/v1/webhooks/stripe", `{"id":"evt_1"}`)
	if w.Code != http.StatusOK || len(l.inserted) != 1 || l.inserted[0].TenantID == nil || *l.inserted[0].TenantID != "t1" ||
		l.inserted[0].NormalisedKind != "invoice_paid" || *l.inserted[0].ProviderObjectRef != "in_1" {
		t.Fatalf("valid: %d %+v", w.Code, l.inserted)
	}

	// BIL-PAY-2: invalid signature → 400, nothing stored, counter incremented.
	l = &fakeLedger{}
	w = serve(newHandler(fakeProvider{webhooks: true}, l), "/api/v1/webhooks/stripe", `{}`)
	if w.Code != http.StatusBadRequest || len(l.inserted) != 0 || failures != 1 || !strings.Contains(w.Body.String(), "INVALID_SIGNATURE") {
		t.Fatalf("invalid: %d %s stored=%d failures=%d", w.Code, w.Body.String(), len(l.inserted), failures)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content type %q", ct)
	}

	// Body over the limit → 413, nothing stored.
	l = &fakeLedger{}
	w = serve(newHandler(fakeProvider{webhooks: true, verifyOK: true}, l), "/api/v1/webhooks/stripe", strings.Repeat("x", 65))
	if w.Code != http.StatusRequestEntityTooLarge || len(l.inserted) != 0 || !isProblem(w) {
		t.Fatalf("too large: %d %q", w.Code, w.Header().Get("Content-Type"))
	}

	// Unknown provider, and manual (no callbacks) → 404.
	for _, path := range []string{"/api/v1/webhooks/adyen", "/api/v1/webhooks/manual"} {
		if w := serve(newHandler(fakeProvider{webhooks: true, verifyOK: true}, &fakeLedger{}), path, `{}`); w.Code != http.StatusNotFound || !isProblem(w) {
			t.Errorf("%s: %d, want 404", path, w.Code)
		}
	}

	// DB down → 503 so the provider redelivers.
	w = serve(newHandler(fakeProvider{webhooks: true, verifyOK: true}, &fakeLedger{err: errors.New("db down")}), "/api/v1/webhooks/stripe", `{}`)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" || !isProblem(w) {
		t.Fatalf("db down: %d", w.Code)
	}

	// Unknown customer: still stored (tenant NULL) so the processor can flag it.
	l = &fakeLedger{}
	h := newHandler(fakeProvider{webhooks: true, verifyOK: true}, l)
	h.Customers = customers{}
	if w := serve(h, "/api/v1/webhooks/stripe", `{}`); w.Code != http.StatusOK || len(l.inserted) != 1 || l.inserted[0].TenantID != nil {
		t.Fatalf("unknown customer: %d %+v", w.Code, l.inserted)
	}
}

// isProblem: every B-W1 error is gincommon's RFC 9457 response.
func isProblem(w *httptest.ResponseRecorder) bool {
	return strings.HasPrefix(w.Header().Get("Content-Type"), "application/problem+json") && strings.Contains(w.Body.String(), `"type"`)
}

// BIL-AUTH-5: B-W1 binds no tenant and trusts no header but the provider
// signature. CD-19 identity headers naming another tenant neither admit a
// badly signed callback nor change the tenant, which comes from Billing's
// customer mapping only.
func TestBIL_AUTH_5_WebhookTrustsOnlyTheSignature(t *testing.T) {
	identity := map[string]string{"x-user-id": "4b1e8a52-1111-4222-8333-944455556666", "x-tenant-id": "t-other",
		"x-tenant-roles": "tenant_owner,platform_operator"}
	post := func(h *Handler, withIdentity bool) *httptest.ResponseRecorder {
		r := gin.New()
		h.Register(r)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/stripe", strings.NewReader(`{"id":"evt_1"}`))
		req.Header.Set("Stripe-Signature", "t=1,v1=abc")
		if withIdentity {
			for k, v := range identity {
				req.Header.Set(k, v)
			}
		}
		r.ServeHTTP(w, req)
		return w
	}
	newHandler := func(verifyOK bool, l *fakeLedger) *Handler {
		return &Handler{Providers: lookup{"stripe": fakeProvider{webhooks: true, verifyOK: verifyOK}}, Ledger: l,
			Customers: customers{"cus_1": "t1"}, MaxBodyBytes: 1024}
	}

	// A bad signature is refused whatever identity the headers claim.
	l := &fakeLedger{}
	if w := post(newHandler(false, l), true); w.Code != http.StatusBadRequest || len(l.inserted) != 0 {
		t.Fatalf("bad signature with identity headers: %d stored=%d", w.Code, len(l.inserted))
	}
	// A valid callback needs no identity, and identity headers do not bind
	// the tenant: it is the customer mapping's.
	for _, withIdentity := range []bool{false, true} {
		l := &fakeLedger{}
		w := post(newHandler(true, l), withIdentity)
		if w.Code != http.StatusOK || len(l.inserted) != 1 || l.inserted[0].TenantID == nil || *l.inserted[0].TenantID != "t1" {
			t.Fatalf("identity headers %v: %d %+v", withIdentity, w.Code, l.inserted)
		}
	}
}
