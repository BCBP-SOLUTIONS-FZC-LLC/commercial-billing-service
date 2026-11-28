//go:build integration

package e2e_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// E2E-INFRA-1 — /healthz is liveness (no dependency), /readyz checks both
// pools (billing_app and the BYPASSRLS billing_system, LLD §13.1).
func TestE2E_Probes(t *testing.T) {
	s := newServer(t, defaultOpts())
	r := s.get(t, "/healthz", nil)
	expect(t, r, http.StatusOK)
	if r.str(t, "status") != "ok" {
		t.Errorf("/healthz body %s", r.body)
	}
	expect(t, s.get(t, "/readyz", nil), http.StatusOK)
}

// E2E-INFRA-2 — the API documents are served without identity headers and
// carry no tenant data: the OpenAPI document lists every route group, the
// AsyncAPI document is served as YAML (§7.6).
func TestE2E_DocumentsServed(t *testing.T) {
	s := newServer(t, defaultOpts())
	r := s.get(t, "/openapi.json", nil)
	expect(t, r, http.StatusOK)
	var doc struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(r.body, &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1") {
		t.Errorf("openapi version %q", doc.OpenAPI)
	}
	for _, p := range []string{
		"/api/v1/tenants/{id}/subscription",
		"/api/v1/internal/subscriptions/authorize-and-charge",
		"/api/v1/operator/billing/provider-certifications/{cert_id}/review",
	} {
		if _, ok := doc.Paths[p]; !ok {
			t.Errorf("OpenAPI document has no %s", p)
		}
	}
	a := s.get(t, "/asyncapi", nil)
	expect(t, a, http.StatusOK)
	if !strings.Contains(a.header.Get("Content-Type"), "yaml") {
		t.Errorf("/asyncapi Content-Type %q", a.header.Get("Content-Type"))
	}
}

// E2E-INFRA-3 — a route behind a feature flag that is off is not
// registered: it answers gincommon's 404 problem, not 403 or 501 (§12).
func TestE2E_FeatureFlagOffUnregistersRoutes(t *testing.T) {
	s := newServer(t, opts{devLike: true, disabled: map[string]bool{"B-I1": true, "B-P19": true, "B-P20": true, "B-O18": true}})
	tenant := newTenant()
	expectProblem(t, s.post(t, "/api/v1/internal/subscriptions/authorize-and-charge", bff(tenant), chargeBody(tenant, "pro", 5)),
		http.StatusNotFound, "")
	expectProblem(t, s.get(t, tp(tenant, "/token-packs"), owner(tenant)), http.StatusNotFound, "")
	// B-O18 with FEATURE_TRIAL_CAP_OVERRIDE off (rev 0.75).
	expectProblem(t, s.call(t, http.MethodPut, "/api/v1/operator/tenants/"+tenant+"/trial-addon-cap", idem(operator(tenant)),
		`{"max_packs":5,"max_amount_minor":50000,"reason":"Enterprise PoC approved by Sales"}`), http.StatusNotFound, "")
	// A route not behind those flags is still there.
	if r := s.get(t, tp(tenant, "/subscription"), owner(tenant)); r.status == http.StatusNotFound && !strings.Contains(string(r.body), "SUBSCRIPTION_NOT_FOUND") {
		t.Fatalf("B-P1 unregistered: %s", r.body)
	}
}
