package metrics_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
)

// HPA (§13.1): at zero backlog the status="received" series is still
// exposed, at 0, so prometheus-adapter can scale down; a status that
// disappears from the reading is deleted, not left at its old value; every
// successful reading stamps the last-success time.
func TestSetSnapshotKeepsKnownSeriesAtZero(t *testing.T) {
	m, reg := fresh(t)
	m.SetSnapshot(port.MetricsSnapshot{WebhookBacklog: map[string]int{"received": 250}, EmissionsPending: map[string]int{"seats_resync": 2},
		Subscriptions: map[[3]string]int{{"active", "pro", "stripe"}: 3}})
	if v, ok := value(t, reg, metrics.WebhookBacklog, map[string]string{"status": "received"}); !ok || v != 250 {
		t.Fatalf("received = %v (%v)", v, ok)
	}
	m.SetSnapshot(port.MetricsSnapshot{})
	for _, status := range []string{"received", "failed"} {
		if v, ok := value(t, reg, metrics.WebhookBacklog, map[string]string{"status": status}); !ok || v != 0 {
			t.Errorf("backlog %s = %v (present %v), want 0", status, v, ok)
		}
	}
	if v, ok := value(t, reg, metrics.SagasInFlight, map[string]string{"saga_state": "failed"}); !ok || v != 0 {
		t.Errorf("saga failed = %v (present %v), want 0", v, ok)
	}
	if _, ok := value(t, reg, metrics.ScheduledEmissionsPending, map[string]string{"builder": "seats_resync"}); ok {
		t.Error("a builder gone from the reading is still exposed")
	}
	if _, ok := value(t, reg, metrics.Subscriptions, map[string]string{"status": "active"}); ok {
		t.Error("a subscription group gone from the reading is still exposed")
	}
	if v, ok := value(t, reg, metrics.SnapshotLastSuccess, nil); !ok || time.Since(time.Unix(int64(v), 0)) > time.Minute {
		t.Errorf("last success = %v (%v)", v, ok)
	}
}

// BillingBackgroundLoopStopped, BillingCronJobNotSucceeding and
// BillingSeatProjectionDrift read these gauges.
func TestOperabilityGauges(t *testing.T) {
	m, reg := fresh(t)
	m.LoopRunning("outbox", true)
	m.LoopRunning("sqs:tenant-billing-q", false)
	if v, _ := value(t, reg, metrics.BackgroundLoopRunning, map[string]string{"loop": "sqs:tenant-billing-q"}); v != 0 {
		t.Errorf("stopped loop = %v", v)
	}
	at := time.Unix(1_800_000_000, 0)
	m.CronJobSucceeded("seat-renewal", at)
	if v, _ := value(t, reg, metrics.CronJobLastSuccess, map[string]string{"job_name": "seat-renewal"}); v != float64(at.Unix()) {
		t.Errorf("last success = %v", v)
	}
	m.SeatProjectionRun(3, 1)
	if v, _ := value(t, reg, metrics.SeatProjectionLastRunDrift, map[string]string{"repeat": "true"}); v != 1 {
		t.Errorf("repeat = %v", v)
	}
	if v, _ := value(t, reg, metrics.SeatProjectionLastRunDrift, map[string]string{"repeat": "false"}); v != 2 {
		t.Errorf("first-time = %v", v)
	}
	m.SeatProjectionRun(0, 0) // a clean run clears the alert
	if v, ok := value(t, reg, metrics.SeatProjectionLastRunDrift, map[string]string{"repeat": "true"}); !ok || v != 0 {
		t.Errorf("after a clean run: %v (%v)", v, ok)
	}
}

// A failed CronJob run is POSTed so the Pushgateway keeps the group's
// last-success timestamp; a successful run PUTs (replaces) the group.
func TestPushFailedRunUsesPost(t *testing.T) {
	m := registered(t)
	m.CronJobSucceeded("downgrade-apply", time.Unix(1_800_000_000, 0))
	var method, body string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method, body = r.Method, string(b)
	}))
	defer srv.Close()
	if err := metrics.PushFailedRun(srv.URL, "purchase-expiry"); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || strings.Contains(body, `job_name="downgrade-apply"`) {
		t.Errorf("%s %q", method, body)
	}
	if err := metrics.PushRun(srv.URL, "downgrade-apply"); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPut || !strings.Contains(body, metrics.CronJobLastSuccess+`{`) {
		t.Errorf("%s %q", method, body)
	}
}
