package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// problemOf asserts an RFC 9457 response from platform-gincommon and
// returns its members.
func problemOf(t *testing.T, w *httptest.ResponseRecorder, status int) map[string]any {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type %q, want application/problem+json", ct)
	}
	var p map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"type", "title", "status", "traceId"} {
		if _, ok := p[m]; !ok {
			t.Errorf("member %s missing: %v", m, p)
		}
	}
	return p
}

func serve(r *Router, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	r.Handler().ServeHTTP(w, req)
	return w
}

// Every error path is gincommon's RFC 9457 response.
func TestBIL_API_6_EveryErrorIsAProblem(t *testing.T) {
	r := NewRouter(RouterConfig{GinConfig: testGin(), Routes: []func(gin.IRouter){func(g gin.IRouter) {
		g.GET("/domain", func(c *gin.Context) {
			writeError(c, domain.NewError(domain.ErrPlanChangeBlocked, "remove SSO first").With("blockers", []string{"sso_idp_configured"}))
		})
		g.GET("/internal", func(c *gin.Context) { writeError(c, errors.New("db password=secret")) })
		g.GET("/stub", func(c *gin.Context) { writeError(c, &app.NotImplementedError{Route: "B-P99", Phase: "9"}) })
		g.GET("/panic", func(*gin.Context) { panic("boom") })
		g.POST("/strict", func(c *gin.Context) {
			var v struct {
				Plan string `json:"plan" binding:"required"`
			}
			if err := decodeJSON(c, &v, true); err != nil {
				writeError(c, err)
			}
		})
	}}})

	p := problemOf(t, serve(r, http.MethodGet, "/domain", ""), http.StatusConflict)
	if p["errorCode"] != "PLAN_CHANGE_BLOCKED" || p["type"] != "https://errors.bcbpsolutions.com/commercial/plan-change-blocked" ||
		p["detail"] != "remove SSO first" || p["blockers"] == nil {
		t.Errorf("domain problem %v", p)
	}
	w := serve(r, http.MethodGet, "/internal", "")
	if p := problemOf(t, w, http.StatusInternalServerError); strings.Contains(w.Body.String(), "secret") || p["errorCode"] == nil {
		t.Errorf("internal problem leaked or uncoded: %s", w.Body.String())
	}
	problemOf(t, serve(r, http.MethodGet, "/stub", ""), http.StatusNotImplemented)
	problemOf(t, serve(r, http.MethodGet, "/panic", ""), http.StatusInternalServerError)
	if p := problemOf(t, serve(r, http.MethodPost, "/strict", `{"plan":"pro","extra":1}`), http.StatusBadRequest); p["errorCode"] != "VALIDATION_ERROR" {
		t.Errorf("unknown field %v", p)
	}
	if p := problemOf(t, serve(r, http.MethodPost, "/strict", `{}`), http.StatusBadRequest); p["validationErrors"] == nil {
		t.Errorf("missing field %v", p)
	}
	problemOf(t, serve(r, http.MethodGet, "/no-such-route", ""), http.StatusNotFound)
	problemOf(t, serve(r, http.MethodDelete, "/domain", ""), http.StatusMethodNotAllowed)
}

// Probe failures are problems too; success stays the probe's JSON.
func TestProbeFailuresAreProblems(t *testing.T) {
	r := NewRouter(RouterConfig{GinConfig: testGin(), Readiness: []Check{{"database", down()}}, Liveness: []Check{{"audit_runner", down()}}})
	problemOf(t, serve(r, http.MethodGet, "/readyz", ""), http.StatusServiceUnavailable)
	problemOf(t, serve(r, http.MethodGet, "/healthz", ""), http.StatusServiceUnavailable)
}

// §5.1: Retry-After on 409 REQUEST_IN_PROGRESS and 503.
func TestRetryAfter(t *testing.T) {
	r := NewRouter(RouterConfig{GinConfig: testGin(), Routes: []func(gin.IRouter){func(g gin.IRouter) {
		g.GET("/busy", func(c *gin.Context) { writeError(c, domain.NewError(domain.ErrRequestInProgress, "in progress")) })
		g.GET("/down", func(c *gin.Context) { writeError(c, domain.NewError(domain.ErrIAMUnavailable, "down")) })
	}}})
	if w := serve(r, http.MethodGet, "/busy", ""); w.Header().Get("Retry-After") != "2" {
		t.Error("no Retry-After on REQUEST_IN_PROGRESS")
	}
	if w := serve(r, http.MethodGet, "/down", ""); w.Header().Get("Retry-After") != "5" || w.Code != http.StatusServiceUnavailable {
		t.Errorf("503: %d %q", w.Code, w.Header().Get("Retry-After"))
	}
}
