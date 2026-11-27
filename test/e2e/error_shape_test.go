//go:build integration

package e2e_test

import (
	"net/http"
	"strings"
	"testing"
)

// E2E-ERR-1 — every error path answers an RFC 9457 problem produced by
// platform-gincommon (BIL-API-6): application/problem+json, a platform type
// URI, title, status, the §17 errorCode, and traceId / timestamp. One case
// per kind of failure, each crossing a different layer: routing, gincommon's
// RequireAuth, Billing's guards, binding, validation, the use case, the
// database.
func TestE2E_EveryErrorIsAProblem(t *testing.T) {
	s := newServer(t, defaultOpts())
	tenant := newTenant()
	cases := []struct {
		name   string
		do     func() reply
		status int
		code   string
	}{
		{"unknown route", func() reply { return s.get(t, "/api/v1/nope", nil) }, 404, ""},
		{"no identity headers", func() reply { return s.get(t, tp(tenant, "/subscription"), nil) }, 401, ""},
		{"path tenant differs from x-tenant-id (BIL-API-2)", func() reply {
			return s.get(t, tp(tenant, "/subscription"), owner(newTenant()))
		}, 403, "INSUFFICIENT_ROLE"},
		{"missing permission (BIL-AUTH-1)", func() reply { return s.get(t, tp(tenant, "/subscription"), member(tenant)) }, 403, "INSUFFICIENT_PERMISSION"},
		{"unknown purchase (use case, RLS-scoped read)", func() reply {
			return s.get(t, tp(tenant, "/subscription/seats/purchases/"+newTenant()), owner(tenant))
		}, 404, "PURCHASE_NOT_FOUND"},
		{"malformed JSON", func() reply {
			return s.call(t, http.MethodPut, tp(tenant, "/billing-contact"), owner(tenant), `{"record_version":`)
		}, 400, "VALIDATION_ERROR"},
		{"unknown field on a tenant route (§10.4)", func() reply {
			return s.call(t, http.MethodPut, tp(tenant, "/billing-contact"), owner(tenant), `{"record_version":1,"is_admin":true}`)
		}, 400, "VALIDATION_ERROR"},
		{"field validation", func() reply {
			return s.post(t, tp(tenant, "/subscription/cancel"), owner(tenant), map[string]any{"record_version": 1, "reason": "because"})
		}, 400, "VALIDATION_ERROR"},
		{"missing Idempotency-Key on a charging route", func() reply {
			return s.post(t, tp(tenant, "/subscription/seats/purchases"), owner(tenant), map[string]any{"add_seats": 1, "preview_id": newTenant()})
		}, 400, "VALIDATION_ERROR"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := expectProblem(t, c.do(), c.status, c.code)
			if c.status != 404 || c.code != "" { // gin-level 404s carry no tenant context
				if p["traceId"] == nil && p["timestamp"] == nil {
					t.Errorf("problem without traceId/timestamp: %v", p)
				}
			}
		})
	}
}

// E2E-ERR-2 — a 403 for a missing permission names it in the
// required_permission extension member (BIL-AUTH-1), so the UI can explain
// what is missing.
func TestE2E_PermissionProblemNamesThePermission(t *testing.T) {
	s := newServer(t, defaultOpts())
	tenant := newTenant()
	p := expectProblem(t, s.get(t, tp(tenant, "/subscription"), member(tenant)), 403, "INSUFFICIENT_PERMISSION")
	if p["required_permission"] != "commercial.billing.view" {
		t.Errorf("required_permission = %v, want commercial.billing.view", p["required_permission"])
	}
}

// E2E-ERR-3 — validation problems list the field in validationErrors[]
// (gincommon's {field, message} shape, rev 0.34) and never echo the
// submitted value.
func TestE2E_ValidationProblemListsFieldsWithoutValues(t *testing.T) {
	s := newServer(t, defaultOpts())
	tenant := newTenant()
	secret := "4111111111111111" // a card-number-shaped value must never come back
	r := s.call(t, http.MethodPut, tp(tenant, "/billing-contact"), owner(tenant),
		map[string]any{"record_version": 1, "country": secret})
	p := expectProblem(t, r, 400, "VALIDATION_ERROR")
	if _, ok := p["validationErrors"].([]any); !ok {
		t.Errorf("no validationErrors[]: %s", r.body)
	}
	if strings.Contains(string(r.body), secret) {
		t.Errorf("problem echoes the submitted value: %s", r.body)
	}
}

// E2E-ERR-4 — the JSON body limit on tenant routes is 64 KiB (§10.4).
func TestE2E_OversizedBodyRefused(t *testing.T) {
	s := newServer(t, defaultOpts())
	tenant := newTenant()
	big := `{"record_version":1,"company_name":"` + strings.Repeat("a", 70*1024) + `"}`
	r := s.call(t, http.MethodPut, tp(tenant, "/billing-contact"), owner(tenant), big)
	if r.status != http.StatusRequestEntityTooLarge && r.status != http.StatusBadRequest {
		t.Fatalf("70 KiB body answered %d, want 413 (or 400 from the limited reader): %s", r.status, r.body)
	}
	expectProblem(t, r, r.status, "")
}

// E2E-ERR-5 — a body that is not application/json is refused before the
// handler (415, §5.1).
func TestE2E_NonJSONBodyRefused(t *testing.T) {
	s := newServer(t, defaultOpts())
	tenant := newTenant()
	r := s.call(t, http.MethodPut, tp(tenant, "/billing-contact"), owner(tenant).with("Content-Type", "text/plain"), "record_version=1")
	expectProblem(t, r, http.StatusUnsupportedMediaType, "")
}
