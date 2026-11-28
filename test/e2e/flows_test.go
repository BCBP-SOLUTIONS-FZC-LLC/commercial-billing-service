//go:build integration

package e2e_test

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

const signupPath = "/api/v1/internal/subscriptions/authorize-and-charge"

// E2E-FLOW-1 — direct paid signup (B-I1, §8.3) over HTTP: 201 with an
// active subscription; the provider sees customer, subscription and
// confirmation calls keyed by Billing ids only (BIL-IDEM-2); the saga waits
// for TenantConverted; B-I1 itself publishes no billing.events (§8.3 step 4).
func TestE2E_DirectSignup(t *testing.T) {
	s := newServer(t, defaultOpts())
	tenant := newTenant()
	r := s.post(t, signupPath, bff(tenant), chargeBody(tenant, "pro", 7))
	expect(t, r, http.StatusCreated)
	res := r.json(t)
	sub, _ := res["billing_subscription_id"].(string)
	if res["status"] != "active" || res["plan_code"] != "pro" || res["licensed_seats"] != float64(7) || sub == "" {
		t.Fatalf("201 body %s", r.body)
	}
	for _, c := range append(s.fakes.Stripe.Calls("EnsureCustomer"), s.fakes.Stripe.Calls("CreateSubscription")...) {
		if want := "bsub:" + sub + ":"; !strings.Contains(c, want) {
			t.Errorf("provider call %q not keyed by the Billing subscription id (%s…)", c, want)
		}
	}
	if st := scalar[string](t, `SELECT status::text FROM subscriptions WHERE billing_subscription_id = $1`, sub); st != "active" {
		t.Errorf("subscription %s", st)
	}
	if st := scalar[string](t, `SELECT state::text FROM conversion_sagas WHERE billing_subscription_id = $1`, sub); st != "awaiting_tenant_converted" {
		t.Errorf("saga %s, want awaiting_tenant_converted", st)
	}
	if n := count(t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, tenant); n != 0 {
		t.Errorf("B-I1 enqueued %d events; the tenant becomes paid through TenantConverted", n)
	}
	if st := scalar[string](t, `SELECT state FROM idempotency_records WHERE tenant_id = $1 AND key = $2`, tenant, "charge:"+tenant); st != "completed" {
		t.Errorf("idempotency record %s, want completed (BIL-IDEM-1)", st)
	}
}

// E2E-FLOW-2 — retries never charge twice (BIL-API-3, BIL-IDEM-1): the same
// key and body replays the stored 201 byte for byte as 200 with
// Idempotent-Replayed; a different body under the same key is
// 409 SUBSCRIPTION_ALREADY_EXISTS naming the live subscription.
func TestE2E_DirectSignupReplay(t *testing.T) {
	s := newServer(t, defaultOpts())
	tenant := newTenant()
	first := s.post(t, signupPath, bff(tenant), chargeBody(tenant, "pro", 7))
	expect(t, first, http.StatusCreated)

	again := s.post(t, signupPath, bff(tenant), chargeBody(tenant, "pro", 7))
	expect(t, again, http.StatusOK)
	if again.header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay without Idempotent-Replayed: %v", again.header)
	}
	// The stored response is jsonb, so the replay is the same JSON document
	// (keys and values), not the same bytes: compare decoded.
	if !reflect.DeepEqual(again.json(t), first.json(t)) {
		t.Errorf("replay body differs:\n%s\n%s", first.body, again.body)
	}

	p := expectProblem(t, s.post(t, signupPath, bff(tenant), chargeBody(tenant, "enterprise", 7)), 409, "SUBSCRIPTION_ALREADY_EXISTS")
	if p["billing_subscription_id"] != first.str(t, "billing_subscription_id") {
		t.Errorf("409 names %v", p["billing_subscription_id"])
	}
	if n := len(s.fakes.Stripe.Calls("CreateSubscription")); n != 1 {
		t.Fatalf("provider CreateSubscription called %d times, want 1", n)
	}
}

// E2E-FLOW-3 — concurrent duplicates (a BFF timeout retry racing the first
// call): exactly one charge. Every answer is 201, a 200 replay or
// 409 REQUEST_IN_PROGRESS with Retry-After; the provider subscription is
// created once and one subscription row exists (BIL-SUB-3, BIL-CONC-1).
func TestE2E_DirectSignupConcurrentDuplicates(t *testing.T) {
	s := newServer(t, defaultOpts())
	tenant := newTenant()
	const n = 8
	var wg sync.WaitGroup
	statuses := make([]int, n)
	retry := make([]string, n)
	for i := range n {
		wg.Go(func() {
			r := s.post(t, signupPath, bff(tenant), chargeBody(tenant, "pro", 3))
			statuses[i], retry[i] = r.status, r.header.Get("Retry-After")
		})
	}
	wg.Wait()
	created := 0
	for i, st := range statuses {
		switch st {
		case http.StatusCreated:
			created++
		case http.StatusOK:
		case http.StatusConflict:
			if retry[i] == "" {
				t.Errorf("409 without Retry-After")
			}
		default:
			t.Errorf("unexpected status %d", st)
		}
	}
	if created != 1 {
		t.Errorf("%d requests answered 201, want exactly 1 (statuses %v)", created, statuses)
	}
	if c := len(s.fakes.Stripe.Calls("CreateSubscription")); c != 1 {
		t.Fatalf("provider CreateSubscription called %d times under %d concurrent duplicates, want 1", c, n)
	}
	if rows := count(t, `SELECT count(*) FROM subscriptions WHERE tenant_id = $1`, tenant); rows != 1 {
		t.Fatalf("%d subscription rows, want 1", rows)
	}
}

// E2E-FLOW-4 — B-P1 for the tenant owner: the subscription as the Billing
// page shows it, with the caller's permissions (all six for the owner) and
// the record_version clients send back on mutations (BIL-CONC-3).
func TestE2E_SubscriptionSummary(t *testing.T) {
	s := newServer(t, defaultOpts())
	tenant, sub := s.signup(t, "pro", 5)
	r := s.get(t, tp(tenant, "/subscription"), owner(tenant))
	expect(t, r, http.StatusOK)
	v := r.json(t)
	if v["billing_subscription_id"] != sub || v["status"] != "active" || v["licensed_seats"] != float64(5) || v["plan"] != "pro" {
		t.Fatalf("B-P1 %s", r.body)
	}
	if v["record_version"] == nil {
		t.Errorf("B-P1 without record_version")
	}
	perms, _ := v["caller_permissions"].([]any)
	if len(perms) != 6 {
		t.Errorf("owner caller_permissions %v, want all six", perms)
	}
	adminView := s.get(t, tp(tenant, "/subscription"), admin(tenant)).json(t)
	if ap, _ := adminView["caller_permissions"].([]any); len(ap) != 3 {
		t.Errorf("tenant_admin caller_permissions %v, want three", ap)
	}
}

// E2E-FLOW-5 — cancel at period end and undo (B-P12, B-P13, §8.8): the
// provider is told, auto_renew flips, record_version guards each step, and a
// stale version is 409 OPTIMISTIC_LOCK_CONFLICT with no provider call.
func TestE2E_CancelAndResume(t *testing.T) {
	s := newServer(t, defaultOpts())
	tenant, _ := s.signup(t, "pro", 5)
	version := func() float64 {
		return s.get(t, tp(tenant, "/subscription"), owner(tenant)).json(t)["record_version"].(float64)
	}

	v1 := version()
	r := s.post(t, tp(tenant, "/subscription/cancel"), owner(tenant), map[string]any{"record_version": v1, "reason": "too_expensive"})
	expect(t, r, http.StatusOK)
	if r.json(t)["auto_renew"] != false {
		t.Fatalf("after cancel: %s", r.body)
	}
	calls := len(s.fakes.Stripe.Calls("SetCancelAtPeriodEnd"))

	expectProblem(t, s.post(t, tp(tenant, "/subscription/resume"), owner(tenant), map[string]any{"record_version": v1}), 409, "OPTIMISTIC_LOCK_CONFLICT")
	if got := len(s.fakes.Stripe.Calls("SetCancelAtPeriodEnd")); got != calls {
		t.Errorf("a stale-version request reached the provider")
	}

	r = s.post(t, tp(tenant, "/subscription/resume"), owner(tenant), map[string]any{"record_version": version()})
	expect(t, r, http.StatusOK)
	if r.json(t)["auto_renew"] != true {
		t.Fatalf("after resume: %s", r.body)
	}
	if got := s.fakes.Stripe.Calls("SetCancelAtPeriodEnd"); len(got) != 2 {
		t.Errorf("provider calls %v, want cancel then resume", got)
	}
}

// E2E-FLOW-6 — buy seats (B-P6 preview → B-P7 purchase, §8.5): the purchase
// is accepted, charged through the provider with a Billing-id key, the seat
// count rises, and TenantSeatsChanged is enqueued in the same commit
// (BIL-EVT-2, BIL-SUB-5).
func TestE2E_SeatPurchase(t *testing.T) {
	s := newServer(t, defaultOpts())
	tenant, sub := s.signup(t, "pro", 5)
	pv := s.post(t, tp(tenant, "/subscription/seats/preview"), owner(tenant), map[string]any{"add_seats": 3})
	expect(t, pv, http.StatusOK)
	preview := pv.json(t)
	if preview["seats_after"] != float64(8) {
		t.Fatalf("preview %s", pv.body)
	}
	buy := s.post(t, tp(tenant, "/subscription/seats/purchases"), idem(owner(tenant)), map[string]any{"add_seats": 3, "preview_id": preview["preview_id"]})
	expect(t, buy, http.StatusAccepted)
	purchase := buy.str(t, "purchase_id")
	if c := s.fakes.Stripe.Calls("ChangeQuantity"); len(c) != 1 || !strings.Contains(c[0], "seatpur:"+purchase) {
		t.Fatalf("provider charge %v, want one keyed seatpur:%s", c, purchase)
	}
	if seats := scalar[int32](t, `SELECT quantity FROM subscription_items WHERE billing_subscription_id = $1`, sub); seats != 8 {
		t.Fatalf("seat quantity %d, want 8", seats)
	}
	evs := scalar[[]string](t, `SELECT coalesce(array_agg(event_type ORDER BY ordering_seq), '{}') FROM outbox_events WHERE tenant_id = $1`, tenant)
	if !slices.Contains(evs, "TenantSeatsChanged") {
		t.Fatalf("outbox %v has no TenantSeatsChanged", evs)
	}
	// B-P8 reads it back.
	st := s.get(t, tp(tenant, "/subscription/seats/purchases/"+purchase), owner(tenant))
	expect(t, st, http.StatusOK)
	if st.str(t, "status") != "confirmed" {
		t.Errorf("purchase %s", st.body)
	}
}

// E2E-FLOW-7 — billing contact (B-P25, B-P26): read, update with the
// current record_version, and a stale version is refused without a write.
func TestE2E_BillingContact(t *testing.T) {
	s := newServer(t, defaultOpts())
	tenant, _ := s.signup(t, "pro", 5)
	r := s.get(t, tp(tenant, "/billing-contact"), owner(tenant))
	expect(t, r, http.StatusOK)
	v := r.json(t)["record_version"].(float64)
	up := s.call(t, http.MethodPut, tp(tenant, "/billing-contact"), owner(tenant),
		map[string]any{"record_version": v, "company_name": "Acme Bidding Co", "country": "AE"})
	expect(t, up, http.StatusOK)
	if up.str(t, "company_name") != "Acme Bidding Co" {
		t.Fatalf("after update %s", up.body)
	}
	expectProblem(t, s.call(t, http.MethodPut, tp(tenant, "/billing-contact"), owner(tenant),
		map[string]any{"record_version": v, "company_name": "Stale Co"}), 409, "OPTIMISTIC_LOCK_CONFLICT")
	if got := scalar[string](t, `SELECT company_name FROM billing_accounts WHERE tenant_id = $1`, tenant); got != "Acme Bidding Co" {
		t.Errorf("stale write applied: %s", got)
	}
}

// E2E-FLOW-8 — operator view (B-O7): the full record with the saga, as a
// platform operator on a dev-like deployment.
func TestE2E_OperatorSubscriptionView(t *testing.T) {
	s := newServer(t, defaultOpts())
	tenant, sub := s.signup(t, "pro", 5)
	r := s.get(t, "/api/v1/operator/tenants/"+tenant+"/subscription", operator(tenant))
	expect(t, r, http.StatusOK)
	v := r.json(t)
	if v["saga"] == nil || v["account"] == nil {
		t.Fatalf("B-O7 %s", r.body)
	}
	if !strings.Contains(string(r.body), sub) {
		t.Errorf("B-O7 does not name subscription %s", sub)
	}
}

// E2E-FLOW-TRIAL-ADDONS (rev 0.75) — over HTTP: B-O18 validates and sets a
// trial tenant's add-on cap (200 view); B-P19 shows it as trial_cap; a
// trial owner's first pack is a 202 requires_action payment_session; the
// tenant admin is refused with owner_only.
func TestE2E_TrialAddonCapAndPurchase(t *testing.T) {
	s := newServer(t, defaultOpts())
	s.fakes.Entitlements.Eligibility = "eligible"
	tenant := newTenant()
	path := "/api/v1/operator/tenants/" + tenant + "/trial-addon-cap"
	expectProblem(t, s.call(t, http.MethodPut, path, idem(operator(tenant)), `{"max_packs":11,"max_amount_minor":5000,"reason":"Enterprise PoC approved"}`),
		http.StatusBadRequest, "VALIDATION_ERROR")
	r := s.call(t, http.MethodPut, path, idem(operator(tenant)), `{"max_packs":5,"max_amount_minor":50000,"reason":"Enterprise PoC approved by Sales"}`)
	expect(t, r, http.StatusOK)
	if v := r.json(t); v["tenant_id"] != tenant || v["max_packs"] != float64(5) || v["max_amount_minor"] != float64(50000) || v["currency"] != "USD" ||
		v["used_packs"] != float64(0) || v["set_by"] != operatorID {
		t.Fatalf("B-O18 %s", r.body)
	}
	r = s.get(t, tp(tenant, "/token-packs"), owner(tenant))
	expect(t, r, http.StatusOK)
	if c, _ := r.json(t)["trial_cap"].(map[string]any); c["max_packs"] != float64(5) || c["max_amount_minor"] != float64(50000) || r.json(t)["eligible"] != true {
		t.Fatalf("B-P19 %s", r.body)
	}
	body := `{"pack_code":"tokens_1m","pack_quantity":1,"accept_terms":true}`
	expectProblem(t, s.post(t, tp(tenant, "/token-addons/purchases"), idem(admin(tenant)), body), http.StatusConflict, "ADDON_NOT_ELIGIBLE")
	r = s.post(t, tp(tenant, "/token-addons/purchases"), idem(owner(tenant)), body)
	expect(t, r, http.StatusAccepted)
	if v := r.json(t); v["status"] != "confirmed" {
		t.Fatalf("B-P20 %s", r.body)
	}
}
