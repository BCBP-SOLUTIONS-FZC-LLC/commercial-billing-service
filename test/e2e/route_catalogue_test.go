//go:build integration

package e2e_test

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	httpadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/http"
)

var pathParam = regexp.MustCompile(`:[a-z_]+`)

// concrete fills every path parameter with a fresh UUID (provider names with
// "stripe").
func concrete(path string) string {
	return pathParam.ReplaceAllStringFunc(path, func(p string) string {
		if p == ":provider" {
			return "stripe"
		}
		return uuid.NewString()
	})
}

// E2E-ROUTES-1 — every endpoint ID of the §5.3 catalogue is mounted on the
// production router with its method: 26 tenant routes, B-I1 and 18 operator
// routes (B-W1 is mounted by the webhook adapter), 45 IDs, none duplicated.
// A request without credentials must be refused by a guard (401 / 403), never
// fall through to gin's 404 / 405 — which would mean the route is missing or
// mounted under another method.
func TestE2E_EveryCatalogueRouteIsMounted(t *testing.T) {
	s := newServer(t, defaultOpts())
	routes := httpadapter.Routes()
	if len(routes) != 45 {
		t.Fatalf("%d routes in the catalogue, want 45 (26 tenant + B-I1 + 18 operator)", len(routes))
	}
	seen := map[string]bool{}
	for _, rt := range routes {
		if seen[rt.ID] {
			t.Errorf("endpoint ID %s registered twice", rt.ID)
		}
		seen[rt.ID] = true
		body := any(nil)
		if rt.Method != http.MethodGet && rt.Method != http.MethodDelete {
			body = map[string]any{}
		}
		r := s.call(t, rt.Method, concrete(rt.Path), nil, body)
		if r.status == http.StatusNotFound || r.status == http.StatusMethodNotAllowed {
			t.Errorf("%s %s %s answered %d without credentials: route not mounted: %s", rt.ID, rt.Method, rt.Path, r.status, r.body)
			continue
		}
		if r.status != http.StatusUnauthorized && r.status != http.StatusForbidden && r.status != http.StatusBadRequest {
			t.Errorf("%s %s %s answered %d without credentials, want 401/403/400", rt.ID, rt.Method, rt.Path, r.status)
		}
	}
	for _, prefix := range []string{"B-P", "B-O"} {
		n := 0
		for id := range seen {
			if strings.HasPrefix(id, prefix) {
				n++
			}
		}
		want := map[string]int{"B-P": 26, "B-O": 18}[prefix]
		if n != want {
			t.Errorf("%d %s* routes, want %d", n, prefix, want)
		}
	}
}

// E2E-ROUTES-2 — B-W1 is mounted for a provider that sends callbacks and
// answers 404 for one that doesn't (manual) or is unknown.
func TestE2E_WebhookRouteMounted(t *testing.T) {
	s := newServer(t, defaultOpts())
	expectProblem(t, s.post(t, "/api/v1/webhooks/manual", nil, `{}`), http.StatusNotFound, "")
	expectProblem(t, s.post(t, "/api/v1/webhooks/acme", nil, `{}`), http.StatusNotFound, "")
	if r := s.post(t, "/api/v1/webhooks/stripe", nil, `{}`); r.status == http.StatusNotFound {
		t.Fatalf("B-W1 for stripe not mounted: %s", r.body)
	}
}

// E2E-ROUTES-3 — unmatched paths and methods are gincommon problems, not
// gin's text bodies.
func TestE2E_UnmatchedRoutesAreProblems(t *testing.T) {
	s := newServer(t, defaultOpts())
	expectProblem(t, s.get(t, "/api/v1/no-such-route", nil), http.StatusNotFound, "")
	expectProblem(t, s.call(t, http.MethodPatch, tp(newTenant(), "/subscription"), nil, `{}`), http.StatusMethodNotAllowed, "")
}
