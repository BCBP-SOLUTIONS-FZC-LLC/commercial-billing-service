// Package httpx builds the outbound HTTP client every service client uses
// (LLD §3, §11.3): otelhttp transport (trace propagation and client spans)
// and the per-dependency timeout from §12.
package httpx

import (
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// New returns a client with the dependency's timeout. name labels spans.
func New(name string, timeout time.Duration) *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.MaxIdleConnsPerHost = 16
	base.IdleConnTimeout = 90 * time.Second
	return &http.Client{
		Timeout: timeout,
		Transport: otelhttp.NewTransport(base, otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return name + " " + r.Method
		})),
	}
}
