//go:build integration

package e2e_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/test/testkit"
)

// E2E-SEC-1 — tenant isolation end to end. The path tenant must equal
// x-tenant-id before any query (BIL-API-2), and RLS scopes every read to the
// bound tenant: tenant B's owner sees no trace of tenant A's subscription
// even though both live in the same tables (BIL-RLS-1, BIL-RLS-2).
func TestE2E_TenantIsolation(t *testing.T) {
	s := newServer(t, defaultOpts())
	tenantA, _ := s.signup(t, "pro", 5)
	tenantB := newTenant()

	// A's owner can read A.
	expect(t, s.get(t, tp(tenantA, "/subscription"), owner(tenantA)), http.StatusOK)
	// B's owner asking for A's path with B's header: refused before any query.
	expectProblem(t, s.get(t, tp(tenantA, "/subscription"), owner(tenantB)), 403, "INSUFFICIENT_ROLE")
	// B's owner on B's own path sees a trial tenant with no subscription
	// (LLD §5.4 B-P1: 200 {"status":"none","trial":true}), never A's row.
	b := s.get(t, tp(tenantB, "/subscription"), owner(tenantB))
	expect(t, b, http.StatusOK)
	if b.str(t, "status") != "none" || strings.Contains(string(b.body), tenantA) || b.json(t)["billing_subscription_id"] != nil {
		t.Fatalf("tenant B sees %s", b.body)
	}
	// A purchase id from A is not found under B (RLS), not forbidden.
	expectProblem(t, s.get(t, tp(tenantB, "/subscription/seats/purchases/"+newTenant()), owner(tenantB)), 404, "PURCHASE_NOT_FOUND")
	// Same for invoices and the billing contact.
	expectProblem(t, s.get(t, tp(tenantA, "/billing-contact"), owner(tenantB)), 403, "INSUFFICIENT_ROLE")
	// A body tenant_id is never trusted for authorization (BIL-API-1): tenant
	// routes reject the unknown field outright.
	expectProblem(t, s.call(t, http.MethodPut, tp(tenantB, "/billing-contact"), owner(tenantB),
		map[string]any{"record_version": 1, "tenant_id": tenantA}), 400, "VALIDATION_ERROR")
}

// E2E-SEC-2 — the role → permission map (Finance-approved, BIL-AUTH-7):
// tenant_admin may view and manage seats but not change the plan, cancel or
// manage payment methods; member, tender_admin and platform_operator hold no
// tenant permission (BIL-AUTH-2).
func TestE2E_RolePermissions(t *testing.T) {
	s := newServer(t, defaultOpts())
	tenant, _ := s.signup(t, "pro", 5)
	expect(t, s.get(t, tp(tenant, "/subscription"), admin(tenant)), http.StatusOK)
	p := expectProblem(t, s.post(t, tp(tenant, "/subscription/cancel"), admin(tenant),
		map[string]any{"record_version": 1, "reason": "too_expensive"}), 403, "INSUFFICIENT_PERMISSION")
	if p["required_permission"] != "commercial.subscription.cancel" {
		t.Errorf("required_permission %v", p["required_permission"])
	}
	expectProblem(t, s.get(t, tp(tenant, "/payment-methods"), member(tenant)), 403, "INSUFFICIENT_PERMISSION")
	expectProblem(t, s.get(t, tp(tenant, "/subscription"), as(memberID, tenant, "tender_admin")), 403, "INSUFFICIENT_PERMISSION")
	expectProblem(t, s.get(t, tp(tenant, "/subscription"), operator(tenant)), 403, "")
	// Roles are a union: owner wins when listed with member.
	expect(t, s.get(t, tp(tenant, "/subscription"), as(ownerID, tenant, "member,tenant_owner")), http.StatusOK)
}

// E2E-SEC-3 — commercial-system is Billing's outbound identity; inbound it
// is refused on every route group (BIL-AUTH-4).
//
// Each case also holds a role that would otherwise be admitted (tenant_owner,
// platform_operator), so only the BIL-AUTH-4 guard can refuse it: either the
// sentinel user id or the commercial-system role is enough.
func TestE2E_CommercialSystemRejectedInbound(t *testing.T) {
	s := newServer(t, defaultOpts())
	tenant, _ := s.signup(t, "pro", 5)
	expect(t, s.get(t, tp(tenant, "/subscription"), owner(tenant)), http.StatusOK) // control: the owner is admitted
	cases := map[string]headers{
		"sentinel user id, owner role":       as(systemID, tenant, "tenant_owner"),
		"commercial-system role among roles": as(ownerID, tenant, "tenant_owner,commercial-system"),
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			expectProblem(t, s.get(t, tp(tenant, "/subscription"), h), 403, "INSUFFICIENT_ROLE")
		})
	}
	op := "/api/v1/operator/tenants/" + tenant + "/subscription"
	expect(t, s.get(t, op, operator(tenant)), http.StatusOK) // control: the operator is admitted (dev-like)
	expectProblem(t, s.get(t, op, as(systemID, tenant, "platform_operator")), 403, "INSUFFICIENT_ROLE")
}

// E2E-SEC-4 — operator routes: platform_operator only, and closed outside
// dev/test until IAM's operator path is live (BIL-AUTH-2, BIL-AUTH-6).
func TestE2E_OperatorGate(t *testing.T) {
	tenant := newTenant()
	path := "/api/v1/operator/tenants/" + tenant + "/subscription"

	open := newServer(t, defaultOpts())
	expectProblem(t, open.get(t, path, owner(tenant)), 403, "")
	if r := open.get(t, path, operator(tenant)); r.status == http.StatusForbidden {
		t.Fatalf("dev-like deployment refused the operator: %s", r.body)
	}

	closed := newServer(t, opts{devLike: false, operatorPathLive: false})
	expectProblem(t, closed.get(t, path, operator(tenant)), 403, "INSUFFICIENT_ROLE")

	live := newServer(t, opts{devLike: false, operatorPathLive: true})
	if r := live.get(t, path, operator(tenant)); r.status == http.StatusForbidden {
		t.Fatalf("OPERATOR_PATH_LIVE=true still refused the operator: %s", r.body)
	}
}

// E2E-SEC-5 — B-I1 admits only the Signup BFF's mesh principal from
// X-Forwarded-Client-Cert (BIL-AUTH-3); the key is exactly
// charge:<tenant_id> and the body tenant equals x-tenant-id (§5.4.2).
func TestE2E_DirectSignupCallerAndKey(t *testing.T) {
	s := newServer(t, defaultOpts())
	tenant := newTenant()
	path := "/api/v1/internal/subscriptions/authorize-and-charge"
	body := chargeBody(tenant, "pro", 5)

	noCert := bff(tenant)
	delete(noCert, "X-Forwarded-Client-Cert")
	expectProblem(t, s.post(t, path, noCert, body), 403, "INSUFFICIENT_ROLE")

	otherSA := bff(tenant).with("X-Forwarded-Client-Cert", "By=spiffe://cluster.local/ns/billing/sa/commercial-billing;URI=spiffe://cluster.local/ns/bff/sa/bff-public")
	expectProblem(t, s.post(t, path, otherSA, body), 403, "INSUFFICIENT_ROLE")

	// The principal is the nearest hop (last XFCC element): a forged first
	// element naming the BFF does not help an untrusted nearest hop.
	forged := bff(tenant).with("X-Forwarded-Client-Cert",
		"URI=spiffe://"+bffPrincipal+",By=spiffe://cluster.local/ns/billing/sa/commercial-billing;URI=spiffe://cluster.local/ns/evil/sa/attacker")
	expectProblem(t, s.post(t, path, forged, body), 403, "INSUFFICIENT_ROLE")

	expectProblem(t, s.post(t, path, bff(tenant).with("Idempotency-Key", "charge:"+newTenant()), body), 400, "")
	expectProblem(t, s.post(t, path, bff(tenant).with("x-tenant-id", newTenant()), body), 400, "")

	if n := len(s.fakes.Stripe.Calls("CreateSubscription")); n != 0 {
		t.Fatalf("refused requests reached the provider %d times", n)
	}
}

// E2E-SEC-6 — B-W1 trusts nothing but the provider signature (BIL-AUTH-5,
// BIL-PAY-2): a bad signature is 400 INVALID_SIGNATURE and nothing is stored;
// a valid callback is ledgered before anything else (BIL-PAY-1); a
// redelivery is 200 with no second row; the tenant comes from Billing's
// customer mapping, not from the payload (BIL-PAY-3).
func TestE2E_WebhookSignatureAndLedger(t *testing.T) {
	s := newServer(t, defaultOpts())
	tenant, _ := s.signup(t, "pro", 5)
	customer := "cus_" + tenant[:8]
	evID := "evt_" + uuid.NewString()[:12]
	body := `{"id":"` + evID + `","type":"invoice.updated","customer":"` + customer + `"}`
	ledger := func() int {
		return count(t, `SELECT count(*) FROM payment_event_ledger WHERE provider_event_id = $1`, evID)
	}

	expectProblem(t, s.post(t, "/api/v1/webhooks/stripe", nil, body), 400, "INVALID_SIGNATURE")
	expectProblem(t, s.post(t, "/api/v1/webhooks/stripe", headers{testkit.SignatureHeader: "forged"}, body), 400, "INVALID_SIGNATURE")
	if ledger() != 0 {
		t.Fatal("a refused callback was stored")
	}

	signed := headers{testkit.SignatureHeader: testkit.ValidSignature}
	expect(t, s.post(t, "/api/v1/webhooks/stripe", signed, body), http.StatusOK)
	expect(t, s.post(t, "/api/v1/webhooks/stripe", signed, body), http.StatusOK)
	if n := ledger(); n != 1 {
		t.Fatalf("%d ledger rows after a delivery and a redelivery, want 1", n)
	}
	if got := scalar[string](t, `SELECT tenant_id::text FROM payment_event_ledger WHERE provider_event_id = $1`, evID); got != tenant {
		t.Fatalf("ledger tenant %s, want %s (from the customer mapping)", got, tenant)
	}

	// A body over WEBHOOK_MAX_BODY_BYTES is refused unread.
	big := `{"id":"evt_big","type":"x","customer":"` + strings.Repeat("c", 5000) + `"}`
	expectProblem(t, s.post(t, "/api/v1/webhooks/stripe", signed, big), 413, "")
}

// E2E-SEC-7 — card data never comes back: B-P15 returns the provider's
// display fields only (brand, last 4, expiry) and B-P1 holds no provider
// secret (BIL-PCI-2, BIL-API-4).
func TestE2E_NoCardDataInResponses(t *testing.T) {
	s := newServer(t, defaultOpts())
	tenant, _ := s.signup(t, "pro", 5)
	r := s.get(t, tp(tenant, "/payment-methods"), owner(tenant))
	expect(t, r, http.StatusOK)
	body := string(r.body)
	if !strings.Contains(body, "4242") || !strings.Contains(body, "visa") {
		t.Fatalf("payment methods: %s", body)
	}
	for _, forbidden := range []string{"4111111111111111", "cvc", "cvv", "client_secret", "whsec_", "rk_live", "sk_live"} {
		for _, b := range []string{body, string(s.get(t, tp(tenant, "/subscription"), owner(tenant)).body)} {
			if strings.Contains(strings.ToLower(b), forbidden) {
				t.Errorf("response contains %q: %s", forbidden, b)
			}
		}
	}
}
