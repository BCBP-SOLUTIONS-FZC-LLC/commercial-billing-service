package metrics_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/logger"
)

// scrape registers every §11.2 metric, records one sample of each and
// returns the text exposition.
func scrape(t *testing.T) string {
	t.Helper()
	log, err := logger.NewLogger("dev")
	if err != nil {
		t.Fatal(err)
	}
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{Logger: log, ServiceName: "commercial-billing", Domain: "commercial", Environment: "test", BuildVersion: "test"})
	reg := prometheus.NewRegistry()
	m, err := metrics.New(reg)
	if err != nil {
		t.Fatal(err)
	}
	m.PurchaseToEvent(port.PurchaseKindSeats, time.Second)
	m.SagaConfirmed("trial_conversion", time.Minute)
	m.RPConvertAttempt("ok")
	m.UnmatchedConversion()
	m.DowngradeBlocked("sso_idp_configured")
	m.RepricingScheduled("increase")
	m.SeatChange("purchase", "confirmed")
	m.SeatProjectionMismatch()
	m.ProrationDeferred("USD", "below_provider_minimum", 120)
	m.AddonPurchase("tokens_1m", "confirmed")
	m.PaymentFailure("insufficient_funds")
	m.DunningTransition("past_due")
	m.InvoiceDisposition("void")
	m.WebhookProcessed("stripe", "invoice_paid", "processed", time.Second)
	m.WebhookDuplicate("stripe", "invoice_paid")
	m.SignatureFailure("stripe")
	m.ReconciliationFindings(map[[2]string]int{{"status_mismatch", "true"}: 1})
	m.CrossTenantJobRun("saga-sweeper", "success")
	m.EventEmitted("TenantSeatsChanged")
	m.OperatorAction("B-O3", "success")
	m.ProviderCertified("stripe", "1.0.0+2025-10-29", true)
	m.SetSnapshot(port.MetricsSnapshot{WebhookBacklog: map[string]int{"received": 1}, SagasInFlight: map[string]int{"rp_convert_pending": 1},
		SagasOverdue: 1, Subscriptions: map[[3]string]int{{"active", "pro", "stripe"}: 2}, CollectionPaused: map[string]int{"operator": 1},
		InvoicesHeld: map[string]int{"USD": 1}, EmissionsPending: map[string]int{"seats_resync": 1}})
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	for _, f := range fams {
		if _, err := expfmt.MetricFamilyToText(&b, f); err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}

// Every §11.2 metric is registered with its exact name.
func TestEveryLLDMetricRegistered(t *testing.T) {
	out := scrape(t)
	for _, name := range []string{metrics.PurchaseToEventSeconds, metrics.EventsEmittedTotal, metrics.OperatorActionsTotal, metrics.CrossTenantJobRunsTotal,
		metrics.WebhooksTotal, metrics.WebhookSignatureFailures, metrics.WebhookProcessingSeconds, metrics.WebhookBacklog, metrics.SagaDurationSeconds,
		metrics.SagasInFlight, metrics.SagasOverdue, metrics.RPConvertAttemptsTotal, metrics.Subscriptions, metrics.DowngradesBlockedTotal,
		metrics.RepricingScheduledTotal, metrics.SeatChangesTotal, metrics.SeatProjectionMismatches, metrics.ProrationDeferredTotal,
		metrics.ProrationDeferredAmount, metrics.AddonPurchasesTotal, metrics.PaymentFailuresTotal, metrics.DunningTransitionsTotal,
		metrics.UnmatchedConversionsTotal, metrics.ReconciliationFindings, metrics.CollectionPaused, metrics.InvoicesHeldForReview,
		metrics.InvoiceDispositionsTotal, metrics.ScheduledEmissionsPending, metrics.ProviderCertified} {
		if !strings.Contains(out, "\n"+name) && !strings.HasPrefix(out, name) && !strings.Contains(out, "# TYPE "+name+" ") {
			t.Errorf("%s not exposed", name)
		}
	}
	if !strings.Contains(out, `domain="commercial"`) || !strings.Contains(out, `service="commercial-billing"`) {
		t.Error("const labels missing")
	}
}

// The scrape passes the platform metricslint (naming, labels, registry).
func TestScrapePassesMetricsLint(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go run")
	}
	f := filepath.Join(t.TempDir(), "scrape.txt")
	if err := os.WriteFile(f, []byte(scrape(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/cmd/metricslint", "check", f)
	out, _ := cmd.CombinedOutput()
	var unexpected []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "error ") && !strings.HasPrefix(line, "warning ") {
			continue
		}
		if !knownFinding(line) {
			unexpected = append(unexpected, line)
		}
	}
	if len(unexpected) > 0 {
		t.Fatalf("metricslint findings beyond the known open items:\n%s", strings.Join(unexpected, "\n"))
	}
}

// knownFindings are open governance items, not defects to hide: the Tier 2
// commercial_* names await registry ratification (§11.2 "proposed"). The
// label-vocabulary findings were resolved in rev 0.38 (result, saga_state).
// Remove an entry when it is resolved; the test then guards it.
var knownFindings = []string{
	"[namespace] commercial_cross_tenant_job_runs_total:",
	"[namespace] commercial_events_emitted_total:",
	"[namespace] commercial_operator_actions_total:",
	"[namespace] commercial_purchase_confirmation_to_event_seconds:",
}

func knownFinding(line string) bool {
	for _, k := range knownFindings {
		if strings.Contains(line, k) {
			return true
		}
	}
	return false
}
