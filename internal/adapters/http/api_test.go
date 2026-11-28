package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

const (
	tenantA  = "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	tenantB  = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	someID   = "0193a7c2-5d1e-7b40-8f2a-1c9e3b7d6a01"
	userID   = "4b1e8a52-1111-4222-8333-944455556666"
	bffXFCC  = `By=spiffe://cluster.local/ns/billing/sa/commercial-billing;Hash=abc;URI=spiffe://cluster.local/ns/bff/sa/bff-signup`
	reason10 = "A sufficiently long operator reason"
)

// ── fakes ───────────────────────────────────────────────────────────────

type memIdem struct {
	mu   sync.Mutex
	recs map[string]*domain.IdempotencyRecord
}

func newMemIdem() *memIdem { return &memIdem{recs: map[string]*domain.IdempotencyRecord{}} }

func k(t, s, key string) string { return t + "|" + s + "|" + key }

func (m *memIdem) Begin(_ context.Context, r domain.IdempotencyRecord) (*domain.IdempotencyRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.recs[k(r.TenantID, r.Scope, r.Key)]; ok {
		cp := *e
		return &cp, nil
	}
	r.State = domain.IdempotencyInFlight
	m.recs[k(r.TenantID, r.Scope, r.Key)] = &r
	return nil, nil
}

func (m *memIdem) Complete(_ context.Context, t, s, key string, status int, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.recs[k(t, s, key)]
	e.State, e.StatusCode, e.ResponseBody, e.LockedUntil = domain.IdempotencyCompleted, &status, body, nil
	return nil
}

func (m *memIdem) Release(_ context.Context, t, s, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.recs, k(t, s, key))
	return nil
}

func (m *memIdem) Reclaim(_ context.Context, r domain.IdempotencyRecord) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.recs[k(r.TenantID, r.Scope, r.Key)]
	if e.ExpiresAt.Before(time.Now()) || (e.State == domain.IdempotencyInFlight && bytes.Equal(e.RequestHash, r.RequestHash) &&
		(e.LockedUntil == nil || e.LockedUntil.Before(time.Now()))) {
		r.State = domain.IdempotencyInFlight
		m.recs[k(r.TenantID, r.Scope, r.Key)] = &r
		return true, nil
	}
	return false, nil
}

func (m *memIdem) Unlock(_ context.Context, t, s, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	past := time.Now().Add(-time.Millisecond)
	m.recs[k(t, s, key)].LockedUntil = &past
	return nil
}

type passTx struct{}

func (passTx) RunInTenantTx(ctx context.Context, _ string, fn func(context.Context) error) error {
	return fn(ctx)
}

// recorder counts use-case calls; everything else is Unimplemented (501).
type recorder struct {
	app.Unimplemented
	mu          sync.Mutex
	checkout    int
	checkoutErr error
	charge      func() (app.ChargeResult, error)
}

func (r *recorder) StartCheckout(context.Context, app.Caller, string, app.CheckoutRequest) (app.CheckoutResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checkout++
	if r.checkoutErr != nil {
		err := r.checkoutErr
		r.checkoutErr = nil
		return app.CheckoutResult{}, err
	}
	return app.CheckoutResult{CheckoutSessionID: someID, CheckoutURL: "https://checkout.example/cs_1", BillingSubscriptionID: someID}, nil
}

func (r *recorder) AuthorizeAndCharge(context.Context, app.Caller, app.AuthorizeChargeRequest) (app.ChargeResult, error) {
	return r.charge()
}

func newAPI(t *testing.T, live bool) (*Router, *recorder, *memIdem) {
	t.Helper()
	rec := &recorder{charge: func() (app.ChargeResult, error) {
		return app.ChargeResult{BillingSubscriptionID: someID, Status: "active"}, nil
	}}
	idem := newMemIdem()
	api := &APIConfig{Tenant: rec, Internal: rec, Operator: rec,
		Idem:             &Idempotency{Repo: idem, Tx: passTx{}, TTL: 72 * time.Hour, Lock: 30 * time.Second},
		OperatorPathLive: live, BFFCallerPrincipal: "cluster.local/ns/bff/sa/bff-signup"}
	return NewRouter(RouterConfig{GinConfig: testGin(), Routes: []func(gin.IRouter){api.Register}}), rec, idem
}

type req struct {
	method, path, body string
	headers            map[string]string
}

func do(r *Router, q req) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	hr := httptest.NewRequest(q.method, q.path, strings.NewReader(q.body))
	if q.body != "" {
		hr.Header.Set("Content-Type", "application/json")
	}
	for k, v := range q.headers {
		hr.Header.Set(k, v)
	}
	r.Handler().ServeHTTP(w, hr)
	return w
}

func errorCode(w *httptest.ResponseRecorder) string {
	var p struct {
		ErrorCode string `json:"errorCode"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	return p.ErrorCode
}

func identity(tenant, roles string) map[string]string {
	return map[string]string{"x-user-id": userID, "x-tenant-id": tenant, "x-tenant-roles": roles}
}

// concretePath fills a route's path parameters.
func concretePath(p string) string {
	r := strings.NewReplacer(":id", tenantA, ":purchase_id", someID, ":invoice_id", someID, ":run_id", someID,
		":saga_id", someID, ":cert_id", someID, ":provider_event_id", "evt_1", ":provider", "stripe")
	return r.Replace(p)
}

// validBodies pass §10.4 validation, so an authorized request reaches the
// use case (501 until Phase 6).
var validBodies = map[string]string{
	"B-P2": `{"plan":"pro","interval":"month","licensed_seats":25}`, "B-P3": `{"target_plan":"enterprise"}`,
	"B-P4": `{"target_plan":"enterprise","record_version":7,"preview_id":"` + someID + `"}`,
	"B-P6": `{"add_seats":5}`, "B-P7": `{"add_seats":5,"preview_id":"` + someID + `"}`, "B-P10": `{"target_seats":60}`,
	"B-P12": `{"record_version":7,"reason":"switching_vendor"}`, "B-P13": `{"record_version":8}`, "B-P14": `{}`, "B-P16": `{}`,
	"B-P20": `{"pack_code":"tokens_5m","pack_quantity":2,"accept_terms":true}`, "B-P23": `{}`,
	"B-P26": `{"billing_contact_email":"billing@acme.example","country":"AE","tax_id":"100123-AB","record_version":4}`,
	"B-O1":  `{"plan":"pro","interval":"year","licensed_seats":40,"reason":"` + reason10 + `"}`,
	"B-O2":  `{"kind":"credit","amount":{"amount_minor":50000,"currency":"USD"},"description":"Service credit","reason":"` + reason10 + `"}`,
	"B-O3":  `{"invoice_id":"` + someID + `","amount":{"amount_minor":100,"currency":"USD"},"category":"duplicate_charge","reason":"` + reason10 + `"}`,
	"B-O5":  `{"provider":"stripe","window_start":"2026-10-01T00:00:00Z","window_end":"2026-11-01T00:00:00Z","reason":"` + reason10 + `"}`,
	"B-O8":  `{"refund":true,"reason":"` + reason10 + `"}`,
	"B-O9":  `{"action":"retry","reason":"` + reason10 + `"}`,
	"B-O10": `{"paused":true,"reason":"` + reason10 + `"}`,
	"B-O11": `{"plan":"pro","interval":"month","currency":"AED","unit_amount_minor":25300,"provider":"stripe","provider_price_id":"price_1","effective_from":"2026-12-01T00:00:00Z","reason":"` + reason10 + `"}`,
	"B-O12": `{"pack_code":"tokens_5m","tokens":5000000,"currency":"USD","amount_minor":19900,"provider":"stripe","provider_price_id":"price_1","active":true,"effective_from":"2026-12-01T00:00:00Z","reason":"` + reason10 + `"}`,
	"B-O13": `{"reason":"` + reason10 + `"}`, "B-O15": `{"reason":"` + reason10 + `"}`,
	"B-O14": `{"provider":"stripe","adapter_version":"1.0.0","provider_api_version":"2026-11-18.endive","suite_version":"cert-1.0","proration_mode":"native","capabilities":{},"results":[{"capability":"rs","case":"create","passed":true,"evidence_ref":"s3://e/1"}],"provider_account_ref":"acct_1","run_started_at":"2026-11-14T08:00:00Z","run_finished_at":"2026-11-14T08:40:00Z","reason":"` + reason10 + `"}`,
	"B-O16": `{"action":"void","reason":"` + reason10 + `"}`,
	"B-O17": `{"decision":"approve","reason":"` + reason10 + `"}`,
	"B-O18": `{"max_packs":5,"max_amount_minor":50000,"reason":"` + reason10 + `"}`,
}

func call(r *Router, rt Route, headers map[string]string, withKey bool) *httptest.ResponseRecorder {
	h := map[string]string{}
	for k, v := range headers {
		h[k] = v
	}
	if withKey {
		h[headerIdempotencyKey] = "key-" + strings.ReplaceAll(rt.ID, "-", "") + "-0001"
	}
	return do(r, req{rt.Method, concretePath(rt.Path), validBodies[rt.ID], h})
}

// ── tests ───────────────────────────────────────────────────────────────

// Every endpoint of §5.3 exists exactly once with its method and path.
func TestCatalogue(t *testing.T) {
	routes := Routes()
	if len(routes) != 45 { // + B-W1 in the webhook adapter = 46
		t.Fatalf("%d routes", len(routes))
	}
	seen := map[string]bool{}
	tenant := 0
	for _, r := range routes {
		if seen[r.ID] || seen[r.Method+" "+r.Path] {
			t.Errorf("duplicate %s %s %s", r.ID, r.Method, r.Path)
		}
		seen[r.ID], seen[r.Method+" "+r.Path] = true, true
		if strings.HasPrefix(r.ID, "B-P") {
			tenant++
			if _, ok := domain.TenantRoutePermission[r.ID]; !ok {
				t.Errorf("%s has no permission", r.ID)
			}
		}
	}
	if tenant != 26 {
		t.Errorf("%d tenant routes", tenant)
	}
}

// §5.2 / BIL-AUTH-1, -2, -7: every role × tenant route. A role without the
// route's permission is 403 INSUFFICIENT_PERMISSION before the use case; a
// role with it reaches the use case (501 until Phase 6).
func TestBIL_AUTH_1_RoleRouteMatrix(t *testing.T) {
	r, _, _ := newAPI(t, true)
	roles := []string{domain.RoleTenantOwner, domain.RoleTenantAdmin, domain.RoleTenderAdmin, domain.RoleMember, domain.RolePlatformOperator, ""}
	for _, rt := range Routes() {
		perm, ok := domain.TenantRoutePermission[rt.ID]
		if !ok {
			continue
		}
		for _, role := range roles {
			w := call(r, rt, identity(tenantA, role), true)
			allowed := domain.HasPermission([]string{role}, perm)
			switch {
			case allowed && w.Code != http.StatusNotImplemented && (w.Code < 200 || w.Code > 299):
				t.Errorf("%s as %q: %d %s, want the use case (2xx, or 501 until Phase 6)", rt.ID, role, w.Code, w.Body.String())
			case !allowed && (w.Code != 403 || errorCode(w) != "INSUFFICIENT_PERMISSION"):
				t.Errorf("%s as %q: %d %s, want 403 INSUFFICIENT_PERMISSION", rt.ID, role, w.Code, errorCode(w))
			}
		}
	}
}

// BIL-AUTH-7: subscription.manage + billing.view never allow a payment
// method change, a contact change or a new contractual commitment.
func TestBIL_AUTH_7_OverlapRule(t *testing.T) {
	r, _, _ := newAPI(t, true)
	for _, id := range append([]string{"B-P16", "B-P26"}, domain.NewContractualCommitmentRoutes...) {
		var rt Route
		for _, x := range Routes() {
			if x.ID == id {
				rt = x
			}
		}
		w := call(r, rt, identity(tenantA, domain.RoleTenantAdmin), true)
		if w.Code != 403 {
			t.Errorf("%s as tenant_admin: %d", id, w.Code)
		}
		var p struct {
			Required string `json:"required_permission"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &p)
		if p.Required != string(domain.TenantRoutePermission[id]) {
			t.Errorf("%s: required_permission %q", id, p.Required)
		}
	}
}

// BIL-API-2, BIL-AUTH-4, 401 without identity, 415.
func TestBIL_API_2_TenantGuards(t *testing.T) {
	r, _, _ := newAPI(t, true)
	path := "/api/v1/tenants/" + tenantA + "/subscription"
	if w := do(r, req{"GET", path, "", identity(tenantB, "tenant_owner")}); w.Code != 403 || errorCode(w) != "INSUFFICIENT_ROLE" {
		t.Errorf("tenant mismatch: %d %s", w.Code, errorCode(w))
	}
	if w := do(r, req{"GET", path, "", nil}); w.Code != 401 {
		t.Errorf("no identity: %d", w.Code)
	}
	sys := identity(tenantA, "tenant_owner")
	sys["x-user-id"] = domain.CommercialSystemPrincipal
	if w := do(r, req{"GET", path, "", sys}); w.Code != 403 || errorCode(w) != "INSUFFICIENT_ROLE" {
		t.Errorf("commercial-system inbound: %d", w.Code)
	}
	h := identity(tenantA, "tenant_owner")
	h["Content-Type"] = "text/plain"
	h[headerIdempotencyKey] = "key-12345678"
	w := httptest.NewRecorder()
	hr := httptest.NewRequest("POST", "/api/v1/tenants/"+tenantA+"/subscription/checkout", strings.NewReader("plan=pro"))
	for k, v := range h {
		hr.Header.Set(k, v)
	}
	r.Handler().ServeHTTP(w, hr)
	if w.Code != 415 || errorCode(w) != "UNSUPPORTED_MEDIA_TYPE" {
		t.Errorf("content type: %d %s", w.Code, errorCode(w))
	}
}

// §10.4: unknown fields and out-of-range values are 400 VALIDATION_ERROR.
func TestBIL_API_1_Validation(t *testing.T) {
	r, rec, _ := newAPI(t, true)
	h := identity(tenantA, "tenant_owner")
	h[headerIdempotencyKey] = "key-validation-1"
	path := "/api/v1/tenants/" + tenantA + "/subscription/checkout"
	for name, body := range map[string]string{
		"unknown field":  `{"plan":"pro","interval":"month","licensed_seats":2,"tenant_id":"x"}`,
		"seats too high": `{"plan":"pro","interval":"month","licensed_seats":1001}`,
		"bad plan":       `{"plan":"gold","interval":"month","licensed_seats":2}`,
		"wrong type":     `{"plan":"pro","interval":"month","licensed_seats":"2"}`,
		"not json":       `{`,
	} {
		if w := do(r, req{"POST", path, body, h}); w.Code != 400 || errorCode(w) != "VALIDATION_ERROR" {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if rec.checkout != 0 {
		t.Errorf("use case called on invalid input")
	}
	if w := do(r, req{"GET", "/api/v1/tenants/" + tenantA + "/subscription/seats/purchases/not-a-uuid", "", identity(tenantA, "tenant_owner")}); w.Code != 400 {
		t.Errorf("path uuid: %d", w.Code)
	}
}

// §9.2: required keys, replay, reuse with another body, key format.
func TestBIL_API_3_Idempotency(t *testing.T) {
	r, rec, _ := newAPI(t, true)
	path := "/api/v1/tenants/" + tenantA + "/subscription/checkout"
	body := validBodies["B-P2"]
	h := identity(tenantA, "tenant_owner")
	if w := do(r, req{"POST", path, body, h}); w.Code != 400 || !strings.Contains(w.Body.String(), headerIdempotencyKey) {
		t.Fatalf("missing key: %d %s", w.Code, w.Body.String())
	}
	h[headerIdempotencyKey] = "short"
	if w := do(r, req{"POST", path, body, h}); w.Code != 400 {
		t.Fatalf("bad key: %d", w.Code)
	}
	h[headerIdempotencyKey] = "checkout-0001"
	first := do(r, req{"POST", path, body, h})
	second := do(r, req{"POST", path, body, h})
	if first.Code != 201 || second.Code != 200 || second.Header().Get("Idempotent-Replayed") != "true" ||
		first.Body.String() != second.Body.String() || rec.checkout != 1 {
		t.Fatalf("replay: %d %d %q calls=%d", first.Code, second.Code, second.Header().Get("Idempotent-Replayed"), rec.checkout)
	}
	if w := do(r, req{"POST", path, `{"plan":"enterprise","interval":"month","licensed_seats":25}`, h}); w.Code != 422 || errorCode(w) != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("reuse: %d %s", w.Code, errorCode(w))
	}
	// A validation failure is not stored: the fixed request runs.
	h[headerIdempotencyKey] = "checkout-0002"
	_ = do(r, req{"POST", path, `{"plan":"x"}`, h})
	if w := do(r, req{"POST", path, body, h}); w.Code != 201 {
		t.Fatalf("after 400: %d", w.Code)
	}
	// D-L3 (rev 0.73): a retryable 409 is not stored, so the retry with the
	// same key runs instead of replaying the conflict for 72 h.
	for i, code := range []domain.ErrorCode{domain.ErrConcurrentUpdate, domain.ErrOptimisticLockConflict} {
		h[headerIdempotencyKey] = fmt.Sprintf("checkout-409-%d", i)
		rec.checkoutErr = domain.NewError(code, "conflict")
		if w := do(r, req{"POST", path, body, h}); w.Code != 409 || errorCode(w) != string(code) {
			t.Fatalf("%s: %d %s", code, w.Code, errorCode(w))
		}
		if w := do(r, req{"POST", path, body, h}); w.Code != 201 || w.Header().Get("Idempotent-Replayed") != "" {
			t.Fatalf("retry after %s: %d", code, w.Code)
		}
	}
}

// B-I1: BIL-AUTH-3 principal, key rule, tenant match, reuse → 409, and a
// 3-D Secure answer that leaves the key runnable.
func TestB_I1(t *testing.T) {
	r, rec, _ := newAPI(t, true)
	path := "/api/v1/internal/subscriptions/authorize-and-charge"
	body := `{"tenant_id":"` + tenantA + `","plan_code":"pro","licensed_seats":10,"payment_method_ref":"pm_1","future_field":1}`
	hdr := func(xfcc, key string) map[string]string {
		return map[string]string{"x-tenant-id": tenantA, headerXFCC: xfcc, headerIdempotencyKey: key}
	}
	if w := do(r, req{"POST", path, body, hdr(strings.Replace(bffXFCC, "ns/bff/sa/bff-signup", "ns/x/sa/y", 1), "charge:"+tenantA)}); w.Code != 403 {
		t.Errorf("wrong principal: %d", w.Code)
	}
	if w := do(r, req{"POST", path, body, hdr(bffXFCC, "charge:someone-else")}); w.Code != 400 {
		t.Errorf("wrong key: %d", w.Code)
	}
	mismatch := strings.Replace(body, tenantA, tenantB, 1)
	if w := do(r, req{"POST", path, mismatch, hdr(bffXFCC, "charge:"+tenantA)}); w.Code != 400 || errorCode(w) != "TENANT_MISMATCH" {
		t.Errorf("tenant mismatch: %d %s", w.Code, errorCode(w))
	}

	// 3-D Secure: 402, then the retry with the same key and body runs again.
	calls := 0
	rec.charge = func() (app.ChargeResult, error) {
		calls++
		if calls == 1 {
			return app.ChargeResult{}, domain.NewError(domain.ErrPaymentActionRequired, "authenticate").
				With("action", map[string]string{"type": "3ds", "client_secret": "pi_secret"}).With("billing_subscription_id", someID)
		}
		return app.ChargeResult{BillingSubscriptionID: someID, Status: "active", Provider: "stripe"}, nil
	}
	w1 := do(r, req{"POST", path, body, hdr(bffXFCC, "charge:"+tenantA)})
	w2 := do(r, req{"POST", path, body, hdr(bffXFCC, "charge:"+tenantA)})
	w3 := do(r, req{"POST", path, body, hdr(bffXFCC, "charge:"+tenantA)})
	if w1.Code != 402 || errorCode(w1) != "PAYMENT_ACTION_REQUIRED" || !strings.Contains(w1.Body.String(), "pi_secret") ||
		w2.Code != 201 || w3.Code != 200 || w3.Header().Get("Idempotent-Replayed") != "true" || calls != 2 {
		t.Fatalf("3ds: %d %d %d calls=%d %s", w1.Code, w2.Code, w3.Code, calls, w1.Body.String())
	}
	// Same key, different body → 409 SUBSCRIPTION_ALREADY_EXISTS with the id.
	other := strings.Replace(body, `"licensed_seats":10`, `"licensed_seats":11`, 1)
	w4 := do(r, req{"POST", path, other, hdr(bffXFCC, "charge:"+tenantA)})
	if w4.Code != 409 || errorCode(w4) != "SUBSCRIPTION_ALREADY_EXISTS" || !strings.Contains(w4.Body.String(), someID) {
		t.Fatalf("reuse: %d %s", w4.Code, w4.Body.String())
	}
}

// BIL-AUTH-3: B-I1 admits only the Signup BFF's mesh principal, the nearest
// hop of X-Forwarded-Client-Cert. No certificate, another service account
// or a forged earlier element naming the BFF is 403 INSUFFICIENT_ROLE
// before the use case runs.
func TestBIL_AUTH_3_OnlyTheSignupBFFPrincipal(t *testing.T) {
	r, _, _ := newAPI(t, true)
	calls := 0
	path := "/api/v1/internal/subscriptions/authorize-and-charge"
	body := `{"tenant_id":"` + tenantA + `","plan_code":"pro","licensed_seats":10,"payment_method_ref":"pm_1"}`
	for name, xfcc := range map[string]string{
		"no certificate":         "",
		"another service":        strings.Replace(bffXFCC, "ns/bff/sa/bff-signup", "ns/bff/sa/bff-public", 1),
		"forged earlier element": `URI=spiffe://cluster.local/ns/bff/sa/bff-signup,By=spiffe://cluster.local/ns/billing/sa/commercial-billing;URI=spiffe://cluster.local/ns/evil/sa/attacker`,
	} {
		h := map[string]string{"x-tenant-id": tenantA, headerIdempotencyKey: "charge:" + tenantA}
		if xfcc != "" {
			h[headerXFCC] = xfcc
		}
		r2, rec, _ := newAPI(t, true)
		rec.charge = func() (app.ChargeResult, error) { calls++; return app.ChargeResult{}, nil }
		if w := do(r2, req{"POST", path, body, h}); w.Code != 403 || errorCode(w) != "INSUFFICIENT_ROLE" {
			t.Errorf("%s: %d %s", name, w.Code, errorCode(w))
		}
	}
	if calls != 0 {
		t.Fatalf("refused callers reached the use case %d times", calls)
	}
	// Control: the BFF itself is admitted.
	h := map[string]string{"x-tenant-id": tenantA, headerIdempotencyKey: "charge:" + tenantA, headerXFCC: bffXFCC}
	if w := do(r, req{"POST", path, body, h}); w.Code != 201 {
		t.Fatalf("bff-signup: %d %s", w.Code, w.Body.String())
	}
}

// BIL-AUTH-4: commercial-system (by principal id or by role) is refused
// inbound on every tenant, internal and operator route, before the use case.
func TestBIL_AUTH_4_SystemCallerRejectedOnEveryRoute(t *testing.T) {
	r, rec, _ := newAPI(t, true)
	charged := 0
	rec.charge = func() (app.ChargeResult, error) { charged++; return app.ChargeResult{}, nil }
	for _, rt := range Routes() {
		roles := domain.RoleTenantOwner
		if strings.HasPrefix(rt.ID, "B-O") {
			roles = "platform_operator,pci_owner_reviewer"
		}
		for name, h := range map[string]map[string]string{
			"principal": identity(tenantA, roles),
			"role":      identity(tenantA, roles+","+domain.RoleCommercialSystem),
		} {
			if name == "principal" {
				h["x-user-id"] = domain.CommercialSystemPrincipal
			}
			if rt.ID == "B-I1" {
				// B-I1 identifies its caller by mesh principal only (no CD-19
				// identity is read there): commercial-system arrives as
				// Billing's own service account and is refused.
				h[headerXFCC] = strings.Replace(bffXFCC, "ns/bff/sa/bff-signup", "ns/billing/sa/commercial-billing", 1)
				h[headerIdempotencyKey] = "charge:" + tenantA
				w := do(r, req{rt.Method, rt.Path, `{"tenant_id":"` + tenantA + `","plan_code":"pro","licensed_seats":10,"payment_method_ref":"pm_1"}`, h})
				if w.Code != 403 || errorCode(w) != "INSUFFICIENT_ROLE" {
					t.Errorf("%s by %s: %d %s", rt.ID, name, w.Code, errorCode(w))
				}
				// rev 0.73: also refused behind the BFF's own principal.
				h[headerXFCC] = bffXFCC
				w = do(r, req{rt.Method, rt.Path, `{"tenant_id":"` + tenantA + `","plan_code":"pro","licensed_seats":10,"payment_method_ref":"pm_1"}`, h})
				if w.Code != 403 || errorCode(w) != "INSUFFICIENT_ROLE" {
					t.Errorf("%s by %s behind the BFF principal: %d %s", rt.ID, name, w.Code, errorCode(w))
				}
				continue
			}
			if w := call(r, rt, h, true); w.Code != 403 || errorCode(w) != "INSUFFICIENT_ROLE" {
				t.Errorf("%s by %s: %d %s", rt.ID, name, w.Code, errorCode(w))
			}
		}
	}
	if rec.checkout != 0 || charged != 0 {
		t.Fatalf("commercial-system reached a use case: checkout=%d charge=%d", rec.checkout, charged)
	}
}

// BIL-AUTH-2/6, B-O17's pci_owner_reviewer, operator idempotency.
func TestBIL_AUTH_2_OperatorGuards(t *testing.T) {
	path := "/api/v1/operator/billing/provider-certifications/" + someID + "/review"
	body := validBodies["B-O17"]
	op := identity(tenantA, "platform_operator")
	op[headerIdempotencyKey] = "review-0001"

	closed, _, _ := newAPI(t, false) // operator path not live, not dev
	if w := do(closed, req{"POST", path, body, identity(tenantA, "platform_operator,pci_owner_reviewer")}); w.Code != 403 {
		t.Errorf("gate closed: %d", w.Code)
	}
	r, _, _ := newAPI(t, true)
	if w := do(r, req{"GET", "/api/v1/operator/tenants/" + tenantA + "/subscription", "", identity(tenantA, "tenant_owner")}); w.Code != 403 || errorCode(w) != "INSUFFICIENT_ROLE" {
		t.Errorf("tenant role on operator route: %d", w.Code)
	}
	if w := do(r, req{"POST", path, body, op}); w.Code != 403 || errorCode(w) != "INSUFFICIENT_ROLE" {
		t.Errorf("B-O17 without pci_owner_reviewer: %d", w.Code)
	}
	op["x-tenant-roles"] = "platform_operator,pci_owner_reviewer"
	if w := do(r, req{"POST", path, body, op}); w.Code != 501 {
		t.Errorf("B-O17 reviewer: %d %s", w.Code, w.Body.String())
	}
	// What AuthZ Enrichment actually sends on an operator route (AE-50): the
	// nil-UUID tenant when the path has none, and every platform-realm role,
	// Keycloak defaults included. Every operator route still reaches the use
	// case (the path tenant or platform_tenant is used, not the header).
	const nilTenant = "00000000-0000-0000-0000-000000000000"
	kc := "default-roles-bcbp-platform,offline_access,uma_authorization,platform_operator,pci_owner_reviewer"
	for _, rt := range Routes() {
		if !strings.HasPrefix(rt.ID, "B-O") {
			continue
		}
		if w := call(r, rt, identity(nilTenant, kc), true); w.Code != 501 {
			t.Errorf("%s with AuthZ's nil tenant and Keycloak roles: %d %s", rt.ID, w.Code, w.Body.String())
		}
	}
	// Every operator route: platform_operator reaches the use case; a
	// mutation without Idempotency-Key is 400; a short reason is 400.
	for _, rt := range Routes() {
		if !strings.HasPrefix(rt.ID, "B-O") {
			continue
		}
		roles := "platform_operator"
		if rt.ID == "B-O17" {
			roles += ",pci_owner_reviewer"
		}
		if w := call(r, rt, identity(tenantA, roles), true); w.Code != 501 {
			t.Errorf("%s: %d %s", rt.ID, w.Code, w.Body.String())
		}
		if rt.Method != http.MethodGet {
			if w := call(r, rt, identity(tenantA, roles), false); w.Code != 400 {
				t.Errorf("%s without key: %d", rt.ID, w.Code)
			}
		}
	}
	short := identity(tenantA, "platform_operator")
	short[headerIdempotencyKey] = "pause-00001"
	if w := do(r, req{"PUT", "/api/v1/operator/tenants/" + tenantA + "/collection-pause", `{"paused":true,"reason":"short"}`, short}); w.Code != 400 {
		t.Errorf("short reason: %d", w.Code)
	}
	// B-O18 (rev 0.75, BIL-ADD-5): the 10-pack / 200000 ceiling and the
	// mandatory reason are 400 VALIDATION_ERROR before the use case.
	capKey := identity(tenantA, "platform_operator")
	capKey[headerIdempotencyKey] = "cap-00000001"
	for _, body := range []string{
		`{"max_packs":11,"max_amount_minor":5000,"reason":"` + reason10 + `"}`,
		`{"max_packs":0,"max_amount_minor":5000,"reason":"` + reason10 + `"}`,
		`{"max_packs":5,"max_amount_minor":200001,"reason":"` + reason10 + `"}`,
		`{"max_packs":5,"max_amount_minor":0,"reason":"` + reason10 + `"}`,
		`{"max_packs":5,"max_amount_minor":50000,"reason":"short"}`,
		`{"max_packs":5,"max_amount_minor":50000}`,
	} {
		if w := do(r, req{"PUT", "/api/v1/operator/tenants/" + tenantA + "/trial-addon-cap", body, capKey}); w.Code != 400 || errorCode(w) != "VALIDATION_ERROR" {
			t.Errorf("B-O18 %s: %d %s", body, w.Code, errorCode(w))
		}
	}
}

func TestPrincipalFromXFCC(t *testing.T) {
	for h, want := range map[string]string{
		bffXFCC: "cluster.local/ns/bff/sa/bff-signup",
		`URI=spiffe://cluster.local/ns/a/sa/b,By=x;URI="spiffe://cluster.local/ns/bff/sa/bff-signup"`: "cluster.local/ns/bff/sa/bff-signup",
		"": "", "Hash=abc": "",
	} {
		if got := principalFromXFCC(h); got != want {
			t.Errorf("%q: %q", h, got)
		}
	}
}

// §12: a route behind a disabled feature flag is not registered (404).
func TestDisabledRoutes(t *testing.T) {
	rec := &recorder{charge: func() (app.ChargeResult, error) { return app.ChargeResult{}, nil }}
	api := &APIConfig{Tenant: rec, Internal: rec, Operator: rec, Idem: &Idempotency{Repo: newMemIdem(), Tx: passTx{}, TTL: time.Hour, Lock: time.Second},
		OperatorPathLive: true, BFFCallerPrincipal: "cluster.local/ns/bff/sa/bff-signup", Disabled: map[string]bool{"B-I1": true, "B-P19": true, "B-O18": true}}
	r := NewRouter(RouterConfig{GinConfig: testGin(), Routes: []func(gin.IRouter){api.Register}})
	// B-O18 (rev 0.75): FEATURE_TRIAL_CAP_OVERRIDE off → not registered.
	op := identity(tenantA, "platform_operator")
	op[headerIdempotencyKey] = "cap-0000001"
	if w := do(r, req{"PUT", "/api/v1/operator/tenants/" + tenantA + "/trial-addon-cap", validBodies["B-O18"], op}); w.Code != 404 {
		t.Errorf("B-O18: %d", w.Code)
	}
	// Another operator route is still registered.
	if w := do(r, req{"GET", "/api/v1/operator/tenants/" + tenantA + "/subscription", "", op}); w.Code != 501 {
		t.Errorf("B-O7: %d", w.Code)
	}
	if w := do(r, req{"POST", "/api/v1/internal/subscriptions/authorize-and-charge", `{}`, nil}); w.Code != 404 {
		t.Errorf("B-I1: %d", w.Code)
	}
	if w := do(r, req{"GET", "/api/v1/tenants/" + tenantA + "/token-packs", "", identity(tenantA, "tenant_owner")}); w.Code != 404 {
		t.Errorf("B-P19: %d", w.Code)
	}
}
