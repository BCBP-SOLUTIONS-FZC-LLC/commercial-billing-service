package stripe

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// fakeStripe is an httptest server standing in for api.stripe.com. Each test
// registers the responses it needs; any other request fails the test. Every
// request is recorded, so a test can assert the path, the form parameters and
// the Idempotency-Key header the adapter sent (LLD §18.8).
type fakeStripe struct {
	t      *testing.T
	mu     sync.Mutex
	routes map[string]func(*http.Request) (int, any)
	reqs   []stripeReq
}

type stripeReq struct {
	Method, Path, Idem string
	Form               url.Values
}

// newFakeStripe returns the fake and an adapter pointed at it: no client-side
// rate limit, no network retries, the fixed test clock.
func newFakeStripe(t *testing.T) (*fakeStripe, *Adapter) {
	t.Helper()
	f := &fakeStripe{t: t, routes: map[string]func(*http.Request) (int, any){}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	a, err := New(Config{APIKey: "rk_test_x", WebhookSecrets: []string{"whsec_current"}, BaseURL: srv.URL,
		Timeout: 5 * time.Second, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	return f, a
}

// on answers method+path with a fixed status and JSON body.
func (f *fakeStripe) on(method, path string, status int, body any) {
	f.routes[method+" "+path] = func(*http.Request) (int, any) { return status, body }
}

func (f *fakeStripe) serve(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	key := r.Method + " " + r.URL.Path
	f.mu.Lock()
	f.reqs = append(f.reqs, stripeReq{Method: r.Method, Path: r.URL.Path, Idem: r.Header.Get("Idempotency-Key"), Form: r.Form})
	h := f.routes[key]
	f.mu.Unlock()
	status, body := http.StatusNotFound, any(apiErr("invalid_request_error", "resource_missing"))
	if h == nil {
		f.t.Errorf("unexpected Stripe request %s %s", key, r.Form.Encode())
	} else {
		status, body = h(r)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// sent returns the requests made to method+path, in order.
func (f *fakeStripe) sent(method, path string) []stripeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []stripeReq
	for _, r := range f.reqs {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// only returns the single request made to method+path.
func (f *fakeStripe) only(method, path string) stripeReq {
	f.t.Helper()
	rs := f.sent(method, path)
	if len(rs) != 1 {
		f.t.Fatalf("%s %s sent %d times, want 1", method, path, len(rs))
	}
	return rs[0]
}

// wantForm asserts form parameters of a recorded request.
func wantForm(t *testing.T, r stripeReq, kv ...string) {
	t.Helper()
	for i := 0; i+1 < len(kv); i += 2 {
		if !r.Form.Has(kv[i]) || r.Form.Get(kv[i]) != kv[i+1] {
			t.Errorf("%s %s: %s = %q (present %v), want %q", r.Method, r.Path, kv[i], r.Form.Get(kv[i]), r.Form.Has(kv[i]), kv[i+1])
		}
	}
}

// wantNoForm asserts parameters that must not be sent.
func wantNoForm(t *testing.T, r stripeReq, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if r.Form.Has(k) {
			t.Errorf("%s %s: unexpected %s = %q", r.Method, r.Path, k, r.Form.Get(k))
		}
	}
}

func apiErr(typ, code string) map[string]any {
	return map[string]any{"error": map[string]any{"type": typ, "code": code, "message": "fake",
		"request_log_url": "https://dashboard.stripe.com/test/logs/req_fake1"}}
}

var unavailable = apiErr("api_error", "")

func list(url string, items ...any) map[string]any {
	if items == nil {
		items = []any{}
	}
	return map[string]any{"object": "list", "url": url, "has_more": false, "data": items}
}

// sub is a subscription with one seat item (periods live on items).
func sub(id, status string, extra map[string]any) map[string]any {
	s := map[string]any{
		"id": id, "object": "subscription", "status": status,
		"items": list("/v1/subscription_items", map[string]any{
			"id": "si_1", "object": "subscription_item", "quantity": 3,
			"current_period_start": fixedNow.Unix(), "current_period_end": fixedNow.AddDate(0, 1, 0).Unix(),
			"price": map[string]any{"id": "price_A", "object": "price", "unit_amount": 2300, "currency": "usd"},
		}),
	}
	for k, v := range extra {
		s[k] = v
	}
	return s
}

func invoice(id, status string, extra map[string]any) map[string]any {
	inv := map[string]any{"id": id, "object": "invoice", "status": status, "currency": "usd"}
	for k, v := range extra {
		inv[k] = v
	}
	return inv
}
