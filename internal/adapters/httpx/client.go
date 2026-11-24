// Package httpx builds the outbound HTTP client every service client uses
// (LLD §3, §11.3): otelhttp transport (trace propagation and client spans),
// platform_dependency_request_seconds{dependency, operation, outcome}
// (§11.2 Tier 1) and the per-dependency timeout from §12.
package httpx

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
)

// DependencyMetric is the platform registry histogram name.
const DependencyMetric = "platform_dependency_request_seconds"

// New returns a client with the dependency's timeout. name labels spans and
// the dependency metric (stripe, core, realm_provisioner, entitlements).
func New(name string, timeout time.Duration) *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.MaxIdleConnsPerHost = 16
	base.IdleConnTimeout = 90 * time.Second
	return &http.Client{
		Timeout: timeout,
		Transport: otelhttp.NewTransport(timed{name: name, next: base}, otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return name + " " + Operation(r.Method, r.URL.Path)
		})),
	}
}

// timed records the call's wall time by outcome.
type timed struct {
	name string
	next http.RoundTripper
}

func (t timed) RoundTrip(r *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.next.RoundTrip(r)
	outcome := "success"
	switch {
	case err != nil || resp.StatusCode >= 500:
		outcome = "error"
	case resp.StatusCode >= 400:
		outcome = "failure"
	}
	if h := dependencyHistogram(); h != nil {
		h.WithLabelValues(t.name, Operation(r.Method, r.URL.Path), outcome).Observe(time.Since(start).Seconds())
	}
	return resp, err
}

var (
	histOnce sync.Once
	hist     *prometheus.HistogramVec
)

// dependencyHistogram registers (or reuses, when platform-events already
// did) the shared histogram once gincommon's metrics are initialised.
func dependencyHistogram() *prometheus.HistogramVec {
	histOnce.Do(func() {
		h, err := gincommon.NewSharedHistogramVec(nil, DependencyMetric)
		var are prometheus.AlreadyRegisteredError
		switch {
		case err == nil:
			hist = h
		case errors.As(err, &are):
			hist, _ = are.ExistingCollector.(*prometheus.HistogramVec)
		}
	})
	return hist
}

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
