package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFilterExposition(t *testing.T) {
	in := `# HELP go_goroutines Number of goroutines.
# TYPE go_goroutines gauge
go_goroutines 12
# HELP commercial_billing_cross_tenant_job_runs_total Runs.
# TYPE commercial_billing_cross_tenant_job_runs_total counter
commercial_billing_cross_tenant_job_runs_total{domain="commercial",job_name="outbox-prune",result="success"} 1
commercial_billing_cross_tenant_job_runs_total{domain="commercial",job_name="seat-renewal",result="success"} 2
# HELP commercial_billing_webhook_processing_seconds Callback received to processed.
# TYPE commercial_billing_webhook_processing_seconds histogram
commercial_billing_webhook_processing_seconds_bucket{kind="invoice",provider="stripe",le="0.05"} 1
commercial_billing_webhook_processing_seconds_bucket{kind="invoice",provider="stripe",le="+Inf"} 1
commercial_billing_webhook_processing_seconds_sum{kind="invoice",provider="stripe"} 0.01
commercial_billing_webhook_processing_seconds_count{kind="invoice",provider="stripe"} 1
# HELP commercial_billing_operator_actions_total Operator calls.
# TYPE commercial_billing_operator_actions_total counter
commercial_billing_operator_actions_total{action="B-O5",job_name="outbox-prune",outcome="success"} 1
`
	got := string(filterExposition([]byte(in), "seat-renewal"))
	for _, want := range []string{
		`job_name="seat-renewal"`,
		"# TYPE commercial_billing_webhook_processing_seconds histogram",
		`commercial_billing_webhook_processing_seconds_bucket{kind="invoice",provider="stripe",le="+Inf"} 1`,
		"commercial_billing_webhook_processing_seconds_count",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, bad := range []string{"go_goroutines", "outbox-prune", "commercial_billing_operator_actions_total"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q kept:\n%s", bad, got)
		}
	}
}

func TestPushRunReportsGatewayError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad metric", http.StatusBadRequest)
	}))
	defer srv.Close()
	if err := PushRun(srv.URL, "seat-renewal"); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("err = %v, want the gateway's 400", err)
	}
}
