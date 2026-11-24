package metrics_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/logger"
)

var (
	once    sync.Once
	shared  *metrics.Metrics
	initErr error
)

// registered registers Billing's metrics on gincommon's registry once per
// test binary, as the server does.
func registered(t *testing.T) *metrics.Metrics {
	t.Helper()
	once.Do(func() {
		log, err := logger.NewLogger("dev")
		if err != nil {
			initErr = err
			return
		}
		_ = gincommon.ObservabilityMiddlewares(gincommon.Config{Logger: log, ServiceName: "commercial-billing", Domain: "commercial", Environment: "test", BuildVersion: "test"})
		shared, initErr = metrics.New(gincommon.MetricsRegisterer(), metrics.Options{Legacy: true})
	})
	if initErr != nil {
		t.Fatal(initErr)
	}
	return shared
}

// scrape records one sample of every §11.2 metric on gincommon's registry
// (legacy names on, as the server during the compatibility period) and
// returns the exposition.
func scrape(t *testing.T) string {
	t.Helper()
	return record(t, registered(t), gincommon.MetricsGatherer())
}

// scrapeWithoutLegacy is the post-sunset scrape: Billing's metrics alone on
// a fresh registry, legacy names off (gincommon's identity initialised).
func scrapeWithoutLegacy(t *testing.T) string {
	t.Helper()
	registered(t)
	reg := prometheus.NewRegistry()
	m, err := metrics.New(reg, metrics.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return record(t, m, reg)
}

func record(t *testing.T, m *metrics.Metrics, g prometheus.Gatherer) string {
	t.Helper()
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
	m.Dependency("stripe", "subscriptions.update", "success", time.Second)
	m.SetSnapshot(port.MetricsSnapshot{WebhookBacklog: map[string]int{"received": 1}, SagasInFlight: map[string]int{"rp_convert_pending": 1},
		SagasOverdue: 1, Subscriptions: map[[3]string]int{{"active", "pro", "stripe"}: 2}, CollectionPaused: map[string]int{"operator": 1},
		InvoicesHeld: map[string]int{"USD": 1}, EmissionsPending: map[string]int{"seats_resync": 1}})
	fams, err := g.Gather()
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

// lint runs the platform metricslint on a scrape and returns its error and
// warning lines.
func lint(t *testing.T, scrape string) []string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "scrape.txt")
	if err := os.WriteFile(f, []byte(scrape), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _ := exec.Command("go", "run", "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/cmd/metricslint", "check", f).CombinedOutput()
	if !strings.Contains(string(out), "metrics checked") {
		t.Fatalf("metricslint did not run:\n%s", out)
	}
	var findings []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "error ") || strings.HasPrefix(line, "warning ") {
			findings = append(findings, line)
		}
	}
	return findings
}

// proposedPlatform are "proposed" warnings for platform-owned registry
// entries (gincommon's build info, the shared dependency histogram): the
// registry's ratification state, not a Billing defect.
var proposedPlatform = []string{"[status] platform_build_info:", "[status] platform_dependency_request_seconds:"}

func matches(line string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.Contains(line, p) {
			return true
		}
	}
	return false
}

// CI gate: Billing's metrics comply with the Enterprise Platform
// Observability Standard — namespace (no unratified commercial_*), naming,
// suffixes, required labels, registry and label vocabulary — with no
// finding but the platform's own "proposed" status warnings.
func TestScrapePassesMetricsLint(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go run")
	}
	var bad []string
	for _, f := range lint(t, scrapeWithoutLegacy(t)) {
		if !matches(f, proposedPlatform) {
			bad = append(bad, f)
		}
	}
	if len(bad) > 0 {
		t.Fatalf("metricslint findings:\n%s", strings.Join(bad, "\n"))
	}
}

// During the compatibility period the only extra findings are the four
// deprecated former Tier 2 names (removed at the sunset).
func TestLegacyNamesAreTheOnlyCompatibilityFindings(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go run")
	}
	legacy := make([]string, 0, len(metrics.Replacements))
	for old := range metrics.Replacements {
		legacy = append(legacy, "[namespace] "+old+":")
	}
	var bad []string
	for _, f := range lint(t, scrape(t)) {
		if !matches(f, proposedPlatform) && !matches(f, legacy) {
			bad = append(bad, f)
		}
	}
	if len(bad) > 0 {
		t.Fatalf("findings beyond the deprecated names:\n%s", strings.Join(bad, "\n"))
	}
}

// Legacy names are emitted in parallel only when switched on, each with
// its Tier 3 replacement.
func TestLegacyParallelEmission(t *testing.T) {
	with, without := scrape(t), scrapeWithoutLegacy(t)
	for old, repl := range metrics.Replacements {
		if !strings.Contains(with, "# TYPE "+old+" ") || !strings.Contains(with, "# TYPE "+repl+" ") {
			t.Errorf("compatibility period: %s and %s must both be exposed", old, repl)
		}
		if strings.Contains(without, old) {
			t.Errorf("%s exposed with legacy off", old)
		}
	}
}
