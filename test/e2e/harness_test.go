//go:build integration

// Package e2e_test runs Billing end to end over HTTP: the production router
// (internal/adapters/http.NewRouter with gincommon's middleware chain), the
// tenant, internal, operator and webhook route groups wired exactly as
// cmd/server wires them, the real app.Service and repositories, on Postgres
// 17 through the production roles (billing_app under RLS, billing_system).
// Only the outbound world is faked (payment provider, Core, Realm
// Provisioner, Entitlements — test/testkit).
//
// Requests carry the headers the gateway, the mesh sidecar and the provider
// set in production: x-user-id / x-tenant-id / x-tenant-roles, the
// X-Forwarded-Client-Cert of B-I1's caller, and the provider signature.
//
// The suites complement the package tests (internal/adapters/http, fakes
// only) and test/flows (use cases without HTTP): here every request crosses
// authentication, authorization, idempotency, binding, the use case, RLS,
// the outbox and the RFC 9457 error boundary in one pass.
//
// Run: make test-e2e (or make test-integration). Needs Docker.
package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	httpadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/http"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/webhook"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/test/testkit"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/logger"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

var db *testkit.DB

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	d, stop, err := testkit.Start(ctx, "../..")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}
	db = d
	code := m.Run()
	stop()
	os.Exit(code)
}

// ── Server ──────────────────────────────────────────────────────────────

// opts are the deployment switches a test varies.
type opts struct {
	operatorPathLive bool
	devLike          bool
	disabled         map[string]bool
}

// defaultOpts is a dev-like deployment: operator routes open, every flag on.
func defaultOpts() opts { return opts{devLike: true} }

// server is one running Billing with its fakes.
type server struct {
	*httptest.Server
	fakes *testkit.Fakes
	svc   *app.Service
}

const bffPrincipal = "cluster.local/ns/bff/sa/bff-signup"

// newServer starts Billing over httptest with o, sharing the suite's database.
func newServer(t *testing.T, o opts) *server {
	t.Helper()
	fakes := testkit.NewFakes()
	svc, err := testkit.NewService(db, fakes)
	if err != nil {
		t.Fatal(err)
	}
	log, err := logger.NewLogger("dev")
	if err != nil {
		t.Fatal(err)
	}
	ginCfg := gincommon.Config{Logger: log, ServiceName: "commercial-billing", BuildVersion: "e2e", Domain: "commercial",
		Environment: "test", RequestTimeout: 30 * time.Second, ProblemTypeBase: "https://errors.bcbpsolutions.com"}
	api := &httpadapter.APIConfig{
		GinConfig: ginCfg, Tenant: svc, Internal: svc, Operator: svc,
		Idem: &httpadapter.Idempotency{Repo: pgadapter.NewIdempotencyRepository(db.App), Tx: pgadapter.NewTxRunner(db.App),
			TTL: 72 * time.Hour, Lock: 30 * time.Second},
		OperatorPathLive: o.operatorPathLive, DevLike: o.devLike, BFFCallerPrincipal: bffPrincipal, Disabled: o.disabled,
	}
	hook := &webhook.Handler{Providers: fakes.Registry(), Ledger: pgadapter.NewLedgerRepository(db.System),
		Customers: pgadapter.NewSystemRepository(db.System), MaxBodyBytes: 4096}
	health := func(p *pgcommon.Pool) httpadapter.Pinger {
		return httpadapter.PingerFunc(func(ctx context.Context) error {
			if !p.Health(ctx).Healthy {
				return fmt.Errorf("pool %s not healthy", p.Name())
			}
			return nil
		})
	}
	router := httpadapter.NewRouter(httpadapter.RouterConfig{
		GinConfig: ginCfg,
		Readiness: []httpadapter.Check{{Name: "database", Pinger: health(db.App)}, {Name: "sys_database", Pinger: health(db.System)}},
		Routes:    []func(gin.IRouter){hook.Register, api.Register, httpadapter.DocsRoutes([]byte("asyncapi: 3.0.0\ninfo:\n  title: billing-events\n"))},
	})
	s := &server{Server: httptest.NewServer(router.Handler()), fakes: fakes, svc: svc}
	t.Cleanup(s.Close)
	return s
}

// ── Identities ──────────────────────────────────────────────────────────

const (
	ownerID    = "4b1e8a52-1111-4222-8333-944455556666"
	adminID    = "5c2f9b63-1111-4222-8333-944455556666"
	memberID   = "6d3a0c74-1111-4222-8333-944455556666"
	operatorID = "9a9a9a9a-1111-4222-8333-944455556666"
	// systemID is commercial-system's sentinel UUID (BIL-AUTH-4).
	systemID = "00000000-0000-0000-0000-0000000000c1"
)

type headers map[string]string

func as(user, tenant, roles string) headers {
	return headers{"x-user-id": user, "x-tenant-id": tenant, "x-tenant-roles": roles}
}

func owner(tenant string) headers    { return as(ownerID, tenant, "tenant_owner") }
func admin(tenant string) headers    { return as(adminID, tenant, "tenant_admin") }
func member(tenant string) headers   { return as(memberID, tenant, "member") }
func operator(tenant string) headers { return as(operatorID, tenant, "platform_operator") }

// bff is B-I1's caller: the XFCC the Istio sidecar sets for bff/bff-signup.
func bff(tenant string) headers {
	return headers{"x-tenant-id": tenant, "Idempotency-Key": "charge:" + tenant,
		"X-Forwarded-Client-Cert": "By=spiffe://cluster.local/ns/billing/sa/commercial-billing;Hash=abc;URI=spiffe://" + bffPrincipal}
}

func (h headers) with(k, v string) headers {
	out := headers{}
	for a, b := range h {
		out[a] = b
	}
	out[k] = v
	return out
}

func idem(h headers) headers { return h.with("Idempotency-Key", "e2e-"+uuid.NewString()) }

func newTenant() string { return uuid.NewString() }

// ── Requests ────────────────────────────────────────────────────────────

// reply is one HTTP answer.
type reply struct {
	status int
	header http.Header
	body   []byte
}

// json decodes the body into a map.
func (r reply) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("body is not a JSON object (%d): %s", r.status, r.body)
	}
	return m
}

// str reads a top-level string member.
func (r reply) str(t *testing.T, key string) string {
	t.Helper()
	v, _ := r.json(t)[key].(string)
	return v
}

func (s *server) call(t *testing.T, method, path string, h headers, body any) reply {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, s.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return reply{status: resp.StatusCode, header: resp.Header, body: b}
}

func (s *server) get(t *testing.T, path string, h headers) reply {
	return s.call(t, http.MethodGet, path, h, nil)
}

func (s *server) post(t *testing.T, path string, h headers, body any) reply {
	return s.call(t, http.MethodPost, path, h, body)
}

// expect fails unless r has status want.
func expect(t *testing.T, r reply, want int) {
	t.Helper()
	if r.status != want {
		t.Fatalf("status %d, want %d: %s", r.status, want, r.body)
	}
}

// expectProblem fails unless r is an RFC 9457 problem from gincommon with
// status and errorCode (BIL-API-6), and returns its members.
func expectProblem(t *testing.T, r reply, status int, code string) map[string]any {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d (%s): %s", r.status, status, code, r.body)
	}
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type %q, want application/problem+json: %s", ct, r.body)
	}
	p := r.json(t)
	if got, _ := p["errorCode"].(string); code != "" && got != code {
		t.Fatalf("errorCode %q, want %q: %s", got, code, r.body)
	}
	if typ, _ := p["type"].(string); !strings.HasPrefix(typ, "https://errors.bcbpsolutions.com/") {
		t.Errorf("type %q is not a platform problem type URI", typ)
	}
	if st, _ := p["status"].(float64); int(st) != status {
		t.Errorf("problem status member %v, want %d", p["status"], status)
	}
	if p["title"] == "" || p["title"] == nil {
		t.Errorf("problem without a title: %s", r.body)
	}
	return p
}

// ── Database assertions (superuser) ─────────────────────────────────────

func scalar[T any](t *testing.T, q string, args ...any) T {
	t.Helper()
	v, err := testkit.Scalar[T](context.Background(), db, q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func count(t *testing.T, q string, args ...any) int {
	t.Helper()
	return int(scalar[int64](t, q, args...))
}

// signup runs B-I1 for a new tenant and returns it with its subscription.
func (s *server) signup(t *testing.T, plan string, seats int) (tenant, sub string) {
	t.Helper()
	tenant = newTenant()
	r := s.post(t, "/api/v1/internal/subscriptions/authorize-and-charge", bff(tenant), chargeBody(tenant, plan, seats))
	expect(t, r, http.StatusCreated)
	return tenant, r.str(t, "billing_subscription_id")
}

func chargeBody(tenant, plan string, seats int) map[string]any {
	return map[string]any{"tenant_id": tenant, "plan_code": plan, "licensed_seats": seats, "payment_method_ref": "pm_card_visa"}
}

func tp(tenant, rest string) string { return "/api/v1/tenants/" + tenant + rest }
