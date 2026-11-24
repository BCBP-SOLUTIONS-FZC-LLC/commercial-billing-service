package metrics_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/metrics"
)

// PushRun PUTs this CronJob's commercial_billing_* series from gincommon's
// registry to /metrics/job/<cronjob>.
func TestPushRun(t *testing.T) {
	m := registered(t)
	m.CrossTenantJobRun("seat-renewal", "success")
	m.CrossTenantJobRun("outbox-prune", "success")
	var path, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		path, body = r.Method+" "+r.URL.Path, string(b)
	}))
	defer srv.Close()
	if err := metrics.PushRun(srv.URL, "seat-renewal"); err != nil {
		t.Fatal(err)
	}
	if path != "PUT /metrics/job/seat-renewal" || !strings.Contains(body, metrics.CrossTenantJobRunsTotal) {
		t.Fatalf("%s %q", path, body)
	}
	if strings.Contains(body, "go_goroutines") {
		t.Error("runtime collectors pushed")
	}
	if strings.Contains(body, "outbox-prune") {
		t.Error("another CronJob's series pushed")
	}
	if err := metrics.PushRun("", "seat-renewal"); err != nil {
		t.Fatal("empty URL must be a no-op")
	}
}
