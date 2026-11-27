package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/pkg/requestctx"
)

func init() { gin.SetMode(gin.TestMode) }

func ok() Pinger { return PingerFunc(func(context.Context) error { return nil }) }
func down() Pinger {
	return PingerFunc(func(context.Context) error { return errors.New("dial tcp 10.0.0.1:5432: secret detail") })
}

func get(t *testing.T, r *Router, path string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: invalid JSON %q", path, w.Body.String())
	}
	return w.Code, body
}

func TestReadyzAllHealthy(t *testing.T) {
	r := NewRouter(RouterConfig{GinConfig: testGin(), Readiness: []Check{{"database", ok()}, {"sys_database", ok()}}})
	code, body := get(t, r, "/readyz")
	if code != http.StatusOK || body["status"] != "ready" {
		t.Fatalf("got %d %v", code, body)
	}
}

func TestReadyzDependencyDown(t *testing.T) {
	r := NewRouter(RouterConfig{GinConfig: testGin(), Readiness: []Check{{"database", ok()}, {"sys_database", down()}}})
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "secret detail") || strings.Contains(w.Body.String(), "10.0.0.1") {
		t.Fatalf("readyz leaked dependency error text: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"sys_database":"down"`) {
		t.Fatalf("failed check not named: %s", w.Body.String())
	}
}

func TestHealthzIgnoresDependencies(t *testing.T) {
	r := NewRouter(RouterConfig{GinConfig: testGin(), Readiness: []Check{{"database", down()}}})
	if code, _ := get(t, r, "/healthz"); code != http.StatusOK {
		t.Fatalf("liveness must not check readiness dependencies; got %d", code)
	}
}

func TestHealthzFailsOnDeadLoop(t *testing.T) {
	r := NewRouter(RouterConfig{GinConfig: testGin(), Liveness: []Check{{"audit_runner", down()}}})
	if code, _ := get(t, r, "/healthz"); code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", code)
	}
}

// gincommon's request ID reaches the request context, where httpx.Call
// forwards it as X-Request-ID.
func TestRequestIDOnRequestContext(t *testing.T) {
	var seen string
	r := NewRouter(RouterConfig{GinConfig: testGin(), Liveness: []Check{{"probe", PingerFunc(func(ctx context.Context) error {
		seen = requestctx.RequestID(ctx)
		return nil
	})}}})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-ID", "req-7")
	r.Handler().ServeHTTP(httptest.NewRecorder(), req)
	if seen != "req-7" {
		t.Fatalf("request ID on the context %q", seen)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	r := NewRouter(RouterConfig{GinConfig: testGin()})
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", w.Code)
	}
}
