package http

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// stubUseCases answers the few use cases these tests reach.
type stubUseCases struct {
	app.Unimplemented
	direction   string
	changeErr   error
	changeCalls int
	listCalls   int
}

func (s *stubUseCases) ChangePlan(context.Context, app.Caller, string, app.PlanChangeRequest) (app.PlanChangeResult, error) {
	s.changeCalls++
	if s.changeErr != nil {
		return app.PlanChangeResult{}, s.changeErr
	}
	return app.PlanChangeResult{ChangeID: someID, Direction: s.direction, Status: "scheduled"}, nil
}

func (s *stubUseCases) InvoiceDownloadURL(context.Context, app.Caller, string, string) (string, error) {
	return "https://files.example/in_1.pdf", nil
}

func (s *stubUseCases) CreateSetupSession(context.Context, app.Caller, string) (app.HostedSession, error) {
	return app.HostedSession{URL: "https://checkout.example/setup/cs_1", ExpiresAt: time.Date(2026, 11, 27, 11, 0, 0, 0, time.UTC)}, nil
}

func (s *stubUseCases) CancelPlanChange(context.Context, app.Caller, string) error { return nil }

func (s *stubUseCases) ListInvoices(context.Context, app.Caller, string, app.InvoiceFilter) (app.List[app.InvoiceView], error) {
	s.listCalls++
	return app.List[app.InvoiceView]{Items: []app.InvoiceView{}}, nil
}

func newStubAPI(t *testing.T, s *stubUseCases, idem *Idempotency, onOp func(string, string)) *Router {
	t.Helper()
	if idem == nil {
		idem = &Idempotency{Repo: newMemIdem(), Tx: passTx{}, TTL: time.Hour, Lock: 30 * time.Second}
	}
	api := &APIConfig{GinConfig: testGin(), Tenant: s, Internal: s, Operator: s, Idem: idem, OperatorPathLive: true,
		BFFCallerPrincipal: "cluster.local/ns/bff/sa/bff-signup", OnOperatorAction: onOp}
	return NewRouter(RouterConfig{GinConfig: testGin(), Routes: []func(gin.IRouter){api.Register}})
}

func ownerWithKey(key string) map[string]string {
	h := identity(tenantA, "tenant_owner")
	if key != "" {
		h[headerIdempotencyKey] = key
	}
	return h
}

// Every UUID path parameter is checked before the use case: a malformed one
// is 400 VALIDATION_ERROR on every route that carries it.
func TestPathParametersMustBeUUIDs(t *testing.T) {
	r := newStubAPI(t, &stubUseCases{}, nil, nil)
	params := []string{":purchase_id", ":invoice_id", ":run_id", ":saga_id", ":cert_id"}
	checked := 0
	for _, rt := range Routes() {
		roles := "tenant_owner"
		if strings.HasPrefix(rt.ID, "B-O") {
			roles = "platform_operator,pci_owner_reviewer"
		}
		targets := params
		if strings.HasPrefix(rt.Path, operatorPrefix+"/tenants/:id") {
			targets = append([]string{":id"}, params...)
		}
		for _, p := range targets {
			if !strings.Contains(rt.Path, p) {
				continue
			}
			path := concretePath(strings.Replace(rt.Path, p, "not-a-uuid", 1))
			h := identity(tenantA, roles)
			h[headerIdempotencyKey] = "key-" + strings.ReplaceAll(rt.ID, "-", "") + "-uuid"
			w := do(r, req{rt.Method, path, validBodies[rt.ID], h})
			if w.Code != http.StatusBadRequest || errorCode(w) != "VALIDATION_ERROR" {
				t.Errorf("%s %s with bad %s: %d %s", rt.ID, rt.Path, p, w.Code, w.Body.String())
			}
			checked++
		}
	}
	if checked < 15 {
		t.Errorf("only %d parameterised routes checked", checked)
	}
}

// List routes validate their query (§10.4).
func TestListQueriesValidated(t *testing.T) {
	r := newStubAPI(t, &stubUseCases{}, nil, nil)
	for _, path := range []string{
		"/api/v1/tenants/" + tenantA + "/subscription/seats/purchases?limit=abc",
		"/api/v1/tenants/" + tenantA + "/invoices?limit=abc",
		"/api/v1/tenants/" + tenantA + "/token-addons/purchases?limit=101",
		"/api/v1/tenants/" + tenantA + "/subscription/history?limit=-1",
	} {
		if w := do(r, req{"GET", path, "", identity(tenantA, "tenant_owner")}); w.Code != 400 || errorCode(w) != "VALIDATION_ERROR" {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	if w := do(r, req{"GET", "/api/v1/operator/tenants/" + tenantA + "/invoices?status=bogus", "", identity(tenantA, "platform_operator")}); w.Code != 400 {
		t.Errorf("B-O4 bad status: %d", w.Code)
	}
}

// B-P17: drafts are operator-only, so status=draft answers an empty list
// without asking the use case.
func TestB_P17DraftsHidden(t *testing.T) {
	s := &stubUseCases{}
	r := newStubAPI(t, s, nil, nil)
	w := do(r, req{"GET", "/api/v1/tenants/" + tenantA + "/invoices?status=draft", "", identity(tenantA, "tenant_owner")})
	if w.Code != 200 || s.listCalls != 0 || !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatalf("draft: %d %s calls=%d", w.Code, w.Body.String(), s.listCalls)
	}
	if w := do(r, req{"GET", "/api/v1/tenants/" + tenantA + "/invoices?status=paid", "", identity(tenantA, "tenant_owner")}); w.Code != 200 || s.listCalls != 1 {
		t.Fatalf("paid: %d calls=%d", w.Code, s.listCalls)
	}
}

// B-P4: 200 for a scheduled downgrade, 202 for an upgrade; a bad body is 400.
func TestB_P4StatusByDirection(t *testing.T) {
	s := &stubUseCases{direction: "downgrade"}
	r := newStubAPI(t, s, nil, nil)
	path := "/api/v1/tenants/" + tenantA + "/subscription/plan-change"
	if w := do(r, req{"POST", path, validBodies["B-P4"], ownerWithKey("change-0001")}); w.Code != 200 {
		t.Errorf("downgrade: %d %s", w.Code, w.Body.String())
	}
	s.direction = "upgrade"
	if w := do(r, req{"POST", path, validBodies["B-P4"], ownerWithKey("change-0002")}); w.Code != 202 {
		t.Errorf("upgrade: %d", w.Code)
	}
	if w := do(r, req{"POST", path, `{"target_plan":"enterprise","extra":1}`, ownerWithKey("change-0003")}); w.Code != 400 || s.changeCalls != 2 {
		t.Errorf("unknown field: %d calls=%d", w.Code, s.changeCalls)
	}
}

// B-P18: 302 to the provider link, never cached.
func TestB_P18Redirect(t *testing.T) {
	r := newStubAPI(t, &stubUseCases{}, nil, nil)
	w := do(r, req{"GET", "/api/v1/tenants/" + tenantA + "/invoices/" + someID + "/download", "", identity(tenantA, "tenant_owner")})
	if w.Code != http.StatusFound || w.Header().Get("Location") != "https://files.example/in_1.pdf" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("download: %d %v", w.Code, w.Header())
	}
}

// B-P16 answers 201 with setup_url from the provider adapter (BIL-API-5).
func TestB_P16HostedSession(t *testing.T) {
	r := newStubAPI(t, &stubUseCases{}, nil, nil)
	w := do(r, req{"POST", "/api/v1/tenants/" + tenantA + "/payment-methods/setup-session", `{}`, ownerWithKey("setup-0002")})
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"setup_url":"https://checkout.example/setup/cs_1"`) ||
		!strings.Contains(w.Body.String(), `"expires_at":"2026-11-27T11:00:00Z"`) {
		t.Fatalf("setup session: %d %s", w.Code, w.Body.String())
	}
}

// An empty body decodes as {} (B-P16 takes no fields).
func TestEmptyBodyIsEmptyObject(t *testing.T) {
	r := newStubAPI(t, &stubUseCases{}, nil, nil)
	if w := do(r, req{"POST", "/api/v1/tenants/" + tenantA + "/payment-methods/setup-session", "", ownerWithKey("setup-0003")}); w.Code != 201 {
		t.Fatalf("empty body: %d %s", w.Code, w.Body.String())
	}
}

// B-P16 takes an empty object only; B-I1 rejects a body that is not JSON.
func TestBodyDecodeErrors(t *testing.T) {
	r := newStubAPI(t, &stubUseCases{}, nil, nil)
	if w := do(r, req{"POST", "/api/v1/tenants/" + tenantA + "/payment-methods/setup-session", `{"return_url":"x"}`, ownerWithKey("setup-0001")}); w.Code != 400 || errorCode(w) != "VALIDATION_ERROR" {
		t.Errorf("B-P16 body: %d %s", w.Code, w.Body.String())
	}
	h := map[string]string{"x-tenant-id": tenantA, headerXFCC: bffXFCC, headerIdempotencyKey: "charge:" + tenantA}
	if w := do(r, req{"POST", "/api/v1/internal/subscriptions/authorize-and-charge", `{"tenant_id":`, h}); w.Code != 400 || errorCode(w) != "VALIDATION_ERROR" {
		t.Errorf("B-I1 body: %d %s", w.Code, w.Body.String())
	}
	// Two JSON values are not one object.
	if w := do(r, req{"POST", "/api/v1/tenants/" + tenantA + "/subscription/plan-change/preview", `{"target_plan":"pro"}{}`, ownerWithKey("")}); w.Code != 400 {
		t.Errorf("trailing value: %d", w.Code)
	}
	// Over the 64 KiB API limit on a route without a key, with no
	// Content-Length (chunked): the read itself fails, 413.
	big := `{"target_plan":"` + strings.Repeat("x", 70<<10) + `"}`
	w := httptest.NewRecorder()
	hr := httptest.NewRequest("POST", "/api/v1/tenants/"+tenantA+"/subscription/plan-change/preview", io.MultiReader(strings.NewReader(big)))
	hr.ContentLength = -1
	hr.Header.Set("Content-Type", "application/json")
	for k, v := range ownerWithKey("") {
		hr.Header.Set(k, v)
	}
	r.Handler().ServeHTTP(w, hr)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized: %d %s", w.Code, w.Body.String())
	}
}

// B-P5 answers 204; its replay is 204 with no body and the replay header.
func TestNoContentAndEmptyReplay(t *testing.T) {
	r := newStubAPI(t, &stubUseCases{}, nil, nil)
	path := "/api/v1/tenants/" + tenantA + "/subscription/plan-change"
	first := do(r, req{"DELETE", path, "", ownerWithKey("cancel-change-1")})
	second := do(r, req{"DELETE", path, "", ownerWithKey("cancel-change-1")})
	if first.Code != 204 || second.Code != 204 || second.Header().Get(headerReplayed) != "true" || second.Body.Len() != 0 {
		t.Fatalf("204 replay: %d %d %q %q", first.Code, second.Code, second.Header().Get(headerReplayed), second.Body.String())
	}
}

// A stored business error (409 that is not in-progress) is replayed as its
// problem document.
func TestStoredProblemReplayed(t *testing.T) {
	s := &stubUseCases{changeErr: domain.NewError(domain.ErrSubscriptionAlreadyExists, "exists")}
	r := newStubAPI(t, s, nil, nil)
	path := "/api/v1/tenants/" + tenantA + "/subscription/plan-change"
	first := do(r, req{"POST", path, validBodies["B-P4"], ownerWithKey("change-409-1")})
	second := do(r, req{"POST", path, validBodies["B-P4"], ownerWithKey("change-409-1")})
	if first.Code != 409 || second.Code != 409 || second.Header().Get(headerReplayed) != "true" || s.changeCalls != 1 ||
		!strings.HasPrefix(second.Header().Get("Content-Type"), "application/problem+json") || errorCode(second) != "SUBSCRIPTION_ALREADY_EXISTS" {
		t.Fatalf("problem replay: %d %d %s calls=%d", first.Code, second.Code, second.Body.String(), s.changeCalls)
	}
}

// B-I1 reuse with a stored body that names no subscription is still 409,
// without the id.
func TestSubscriptionExistsWithoutID(t *testing.T) {
	err := subscriptionExists([]byte(`{"type":"x"}`))
	var de *domain.DomainError
	if !errors.As(err, &de) || de.Code != domain.ErrSubscriptionAlreadyExists || de.Extensions["billing_subscription_id"] != nil {
		t.Fatalf("err %+v", err)
	}
}

// commercial_billing_operator_actions_total: every operator call, by
// outcome (success, failure, error).
func TestOperatorActionsCounted(t *testing.T) {
	var got []string
	r := newStubAPI(t, &stubUseCases{}, nil, func(a, o string) { got = append(got, a+":"+o) })
	op := identity(tenantA, "platform_operator")
	_ = do(r, req{"GET", "/api/v1/operator/tenants/" + tenantA + "/subscription", "", op})  // 501
	_ = do(r, req{"GET", "/api/v1/operator/tenants/not-a-uuid/subscription", "", op})       // 400
	_ = do(r, req{"GET", "/api/v1/operator/tenants/" + tenantA + "/subscription", "", nil}) // 401
	_ = do(r, req{"GET", "/api/v1/operator/billing/reconciliation-runs/" + someID, "", op}) // 501
	want := []string{"B-O7:error", "B-O7:failure", "B-O7:failure", "B-O6:error"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("counted %v, want %v", got, want)
	}
	// A path outside the catalogue is "unknown"; a 2xx is success.
	var one []string
	h := countOperatorAction(map[string]string{}, func(a, o string) { one = append(one, a+":"+o) })
	e := gin.New()
	e.GET("/x", h, func(c *gin.Context) { c.Status(http.StatusOK) })
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	if len(one) != 1 || one[0] != "unknown:success" {
		t.Errorf("unknown route: %v", one)
	}
	// Without a counter the middleware only passes through.
	e = gin.New()
	e.GET("/y", countOperatorAction(nil, nil), func(c *gin.Context) { c.Status(http.StatusTeapot) })
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", "/y", nil))
	if w.Code != http.StatusTeapot {
		t.Errorf("pass-through: %d", w.Code)
	}
}

func TestRequirePermissionPanicsOnUndeclaredRoute(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("no panic for a route without a permission")
		}
	}()
	requirePermission("B-P99")
}

// ── Idempotency middleware on its own ─────────────────────────────────────

type failingTx struct{}

func (failingTx) RunInTenantTx(context.Context, string, func(context.Context) error) error {
	return domain.NewError(domain.ErrDBUnavailable, "db down")
}

type completeFails struct{ *memIdem }

func (completeFails) Complete(context.Context, string, string, string, int, []byte) error {
	return errors.New("write failed")
}

func idemEngine(m *Idempotency, h gin.HandlerFunc) *gin.Engine {
	e := gin.New()
	e.POST("/x", m.handle(idemRoute{scope: "X", mode: idemRequired, tenant: func(*gin.Context) string { return tenantA }}), h)
	return e
}

func post(e *gin.Engine, key, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/x", strings.NewReader(body))
	r.Header.Set(headerIdempotencyKey, key)
	e.ServeHTTP(w, r)
	return w
}

// A database failure on Begin is the request's error; the handler never runs.
func TestIdempotencyBeginFailure(t *testing.T) {
	ran := false
	e := idemEngine(&Idempotency{Repo: newMemIdem(), Tx: failingTx{}, TTL: time.Hour, Lock: time.Minute},
		func(c *gin.Context) { ran = true; c.Status(201) })
	if w := post(e, "key-begin-0001", `{}`); w.Code != http.StatusServiceUnavailable || ran {
		t.Fatalf("begin failure: %d ran=%v", w.Code, ran)
	}
}

// An unreadable body is refused before any record is written.
func TestIdempotencyUnreadableBody(t *testing.T) {
	repo := newMemIdem()
	e := gin.New()
	e.POST("/x", func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4)
		c.Next()
	}, (&Idempotency{Repo: repo, Tx: passTx{}, TTL: time.Hour, Lock: time.Minute}).handle(idemRoute{scope: "X", mode: idemRequired, tenant: func(*gin.Context) string { return tenantA }}),
		func(c *gin.Context) { c.Status(201) })
	if w := post(e, "key-body-0001", `{"a":"long enough"}`); w.Code < 400 || len(repo.recs) != 0 {
		t.Fatalf("unreadable body: %d records=%d", w.Code, len(repo.recs))
	}
}

// The record expires TTL after the injected clock; a non-JSON response
// (written with WriteString) is stored without a body; a failed Complete is
// logged, the response already sent.
func TestIdempotencyClockAndPlainResponse(t *testing.T) {
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	repo := newMemIdem()
	m := &Idempotency{Repo: repo, Tx: passTx{}, TTL: 72 * time.Hour, Lock: time.Minute, Now: func() time.Time { return now }}
	e := idemEngine(m, func(c *gin.Context) {
		c.Status(http.StatusCreated)
		_, _ = io.WriteString(c.Writer, "plain")
	})
	if w := post(e, "key-plain-0001", `{}`); w.Code != 201 || w.Body.String() != "plain" {
		t.Fatalf("first: %d %q", w.Code, w.Body.String())
	}
	rec := repo.recs[k(tenantA, "X", "key-plain-0001")]
	if rec == nil || !rec.ExpiresAt.Equal(now.Add(72*time.Hour)) || rec.State != domain.IdempotencyCompleted || rec.ResponseBody != nil {
		t.Fatalf("record %+v", rec)
	}

	failing := &Idempotency{Repo: completeFails{newMemIdem()}, Tx: passTx{}, TTL: time.Hour, Lock: time.Minute}
	e = idemEngine(failing, func(c *gin.Context) { c.JSON(http.StatusCreated, gin.H{"ok": true}) })
	w := post(e, "key-complete-01", `{}`)
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("complete failure must not change the response: %d %s", w.Code, w.Body.String())
	}
}

// ── Router and OpenAPI helpers ────────────────────────────────────────────

func TestRouterDefaultsAndNilCheckers(t *testing.T) {
	cfg := testGin()
	cfg.RequestTimeout = 0
	cfg.Logger = nil
	r := NewRouter(RouterConfig{GinConfig: cfg, Readiness: []Check{{Name: "none"}}, Liveness: []Check{{Name: "none"}}})
	for _, p := range []string{"/healthz", "/readyz"} {
		if code, _ := get(t, r, p); code != 200 {
			t.Errorf("%s: %d", p, code)
		}
	}
}

type queryNoTag struct {
	Limit    int `form:"limit" binding:"min=x"`
	Internal string
}

type objectEdges struct {
	Skipped string `json:"-"`
	NoName  string
	Any     any     `json:"any"`
	Ratio   float64 `json:"ratio"`
	Flag    uint8   `json:"flag"`
	Count   int     `json:"count" binding:"min=abc"`
	Name    string  `json:"name" binding:"required"`
}

func TestSchemaGenEdgeCases(t *testing.T) {
	g := &schemaGen{defs: obj{}}
	q := g.queryParams(reflect.TypeFor[queryNoTag]())
	if len(q) != 1 {
		t.Fatalf("query params %v", q)
	}
	s := g.object(reflect.TypeFor[objectEdges](), "objectEdges")
	props := s["properties"].(obj)
	if _, ok := props["Skipped"]; ok || len(props) != 5 {
		t.Errorf("properties %v", props)
	}
	if props["ratio"].(obj)["type"] != "number" || len(props["flag"].(obj)) != 0 {
		t.Errorf("ratio %v flag %v", props["ratio"], props["flag"])
	}
	if len(props["any"].(obj)) != 0 {
		t.Errorf("interface schema %v", props["any"])
	}
	if _, ok := props["count"].(obj)["minimum"]; ok {
		t.Errorf("non-numeric min applied: %v", props["count"])
	}
	if req, _ := s["required"].([]string); len(req) != 1 || req[0] != "name" {
		t.Errorf("required %v", s["required"])
	}
}
