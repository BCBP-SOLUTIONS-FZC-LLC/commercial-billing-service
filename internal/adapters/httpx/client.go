// Package httpx builds the outbound HTTP client every service client uses
// (LLD §3, §11.3): a client span per call on gincommon's tracer, W3C trace
// context injected with gincommon.InjectTraceContext, the call's duration
// reported to Observe (platform_dependency_request_seconds, registered by
// adapters/metrics on gincommon's registry), and the per-dependency timeout
// from §12. No OpenTelemetry or Prometheus types here.
package httpx

import (
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
)

// Observer records one dependency call (dependency, operation, outcome:
// success | failure | error).
type Observer func(dependency, operation, outcome string, d time.Duration)

var observer atomic.Pointer[Observer]

// SetObserver installs the dependency metric (adapters/metrics), once at startup.
func SetObserver(o Observer) { observer.Store(&o) }

// tracerScope names the client spans' instrumentation scope.
const tracerScope = "commercial-billing/httpx"

// New returns a client with the dependency's timeout. name labels spans and
// the dependency metric (stripe, core, realm_provisioner, entitlements).
func New(name string, timeout time.Duration) *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.MaxIdleConnsPerHost = 16
	base.IdleConnTimeout = 90 * time.Second
	return &http.Client{Timeout: timeout, Transport: instrumented{name: name, next: base}}
}

// instrumented traces, propagates and times each call.
type instrumented struct {
	name string
	next http.RoundTripper
}

func (t instrumented) RoundTrip(r *http.Request) (*http.Response, error) {
	op := Operation(r.Method, r.URL.Path)
	// Looked up per call, so it is always the provider gincommon installed.
	ctx, end := gincommon.NewSpanTracer(tracerScope).StartSpan(r.Context(), t.name+" "+op)
	defer end()
	r = r.Clone(ctx)
	gincommon.InjectTraceContext(ctx, r.Header)
	start := time.Now()
	resp, err := t.next.RoundTrip(r)
	outcome := "success"
	switch {
	case err != nil:
		outcome = "error"
		gincommon.RecordSpanError(ctx, err, "dependency call failed")
	case resp.StatusCode >= 500:
		outcome = "error"
		gincommon.RecordSpanError(ctx, errStatus(resp.StatusCode), "dependency answered 5xx")
	case resp.StatusCode >= 400:
		outcome = "failure"
	}
	if o := observer.Load(); o != nil {
		(*o)(t.name, op, outcome, time.Since(start))
	}
	return resp, err
}

type errStatus int

func (e errStatus) Error() string { return http.StatusText(int(e)) }

// Operation is a bounded label for a call: the method and the path with
// identifiers removed, as dotted segments (POST /v1/subscriptions/sub_1 →
// subscriptions.update; GET /api/v1/internal/tenants/<uuid>/seat-usage →
// tenants.seat-usage.retrieve).
func Operation(method, path string) string {
	var segs []string
	idLast := false
	for _, p := range strings.Split(strings.Trim(path, "/"), "/") {
		switch {
		case p == "" || p == "api" || p == "internal" || isVersion(p):
			continue
		case isID(p):
			idLast = true
			continue
		}
		idLast = false
		segs = append(segs, p)
	}
	verb := map[string]string{http.MethodGet: "list", http.MethodPost: "create", http.MethodDelete: "delete", http.MethodPut: "update", http.MethodPatch: "update"}[method]
	if idLast {
		verb = map[string]string{http.MethodGet: "retrieve", http.MethodPost: "update", http.MethodDelete: "delete", http.MethodPut: "update", http.MethodPatch: "update"}[method]
	}
	if verb == "" {
		verb = strings.ToLower(method)
	}
	op := strings.Join(append(segs, verb), ".")
	if len(op) > 64 {
		op = op[:64]
	}
	return op
}

func isVersion(s string) bool {
	return len(s) >= 2 && s[0] == 'v' && strings.Trim(s[1:], "0123456789") == ""
}

// isID: provider ids (sub_…, cus_…, in_…), UUIDs and numbers.
func isID(s string) bool {
	if strings.Trim(s, "0123456789") == "" {
		return true
	}
	if len(s) == 36 && strings.Count(s, "-") == 4 {
		return true
	}
	if i := strings.IndexByte(s, '_'); i > 0 && i <= 6 && len(s)-i > 6 {
		return strings.ToLower(s[:i]) == s[:i]
	}
	return false
}
