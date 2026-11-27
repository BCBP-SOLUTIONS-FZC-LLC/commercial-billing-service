// Package metrics registers Billing's metrics under the Enterprise Platform
// Observability Standard (LLD §11.2) and implements port.Metrics.
//
//   - Tier 1 platform_*: only registry entries, through gincommon's shared
//     constructors (platform_dependency_request_seconds here; the HTTP,
//     messaging, outbox and pool series come from the platform libraries).
//   - Tier 2 commercial_*: none. No Commercial domain metric is ratified in
//     the Platform Observability Registry, and services may not invent
//     shared names (standard rule 11); candidates are proposed in
//     docs/observability/registry-proposals.md.
//   - Tier 3 commercial_billing_*: everything Billing owns, built with
//     gincommon's validated constructors (prefix, suffixes, label names).
//
// Every collector is registered on gincommon's registry and every scrape
// and push reads gincommon.MetricsGatherer; domain, service and environment
// are gincommon's central const labels. Collectors are gincommon's own
// types (gincommon.CounterVec, …), so no package names Prometheus types
// (BIL-OPS-10, depguard).
package metrics

import (
	"errors"
	"strconv"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
)

// Names (§11.2). Kept as constants so alerts, tests and the inventory
// agree with the code.
const (
	PurchaseToEventSeconds    = "commercial_billing_purchase_confirmation_to_event_seconds"
	EventsEmittedTotal        = "commercial_billing_events_emitted_total"
	OperatorActionsTotal      = "commercial_billing_operator_actions_total"
	CrossTenantJobRunsTotal   = "commercial_billing_cross_tenant_job_runs_total"
	WebhooksTotal             = "commercial_billing_webhooks_total"
	WebhookSignatureFailures  = "commercial_billing_webhook_signature_failures_total"
	WebhookProcessingSeconds  = "commercial_billing_webhook_processing_seconds"
	WebhookBacklog            = "commercial_billing_webhook_backlog"
	SagaDurationSeconds       = "commercial_billing_conversion_saga_duration_seconds"
	SagasInFlight             = "commercial_billing_sagas_in_flight"
	SagasOverdue              = "commercial_billing_sagas_overdue"
	RPConvertAttemptsTotal    = "commercial_billing_rp_convert_attempts_total"
	Subscriptions             = "commercial_billing_subscriptions"
	DowngradesBlockedTotal    = "commercial_billing_downgrades_blocked_total"
	RepricingScheduledTotal   = "commercial_billing_repricing_scheduled_total"
	SeatChangesTotal          = "commercial_billing_seat_changes_total"
	SeatProjectionMismatches  = "commercial_billing_seat_projection_mismatches_total"
	ProrationDeferredTotal    = "commercial_billing_proration_deferred_total"
	ProrationDeferredAmount   = "commercial_billing_proration_deferred_amount_minor_total"
	AddonPurchasesTotal       = "commercial_billing_addon_purchases_total"
	PaymentFailuresTotal      = "commercial_billing_payment_failures_total"
	DunningTransitionsTotal   = "commercial_billing_dunning_transitions_total"
	UnmatchedConversionsTotal = "commercial_billing_unmatched_conversions_total"
	ReconciliationFindings    = "commercial_billing_reconciliation_findings"
	CollectionPaused          = "commercial_billing_collection_paused"
	InvoicesHeldForReview     = "commercial_billing_invoices_held_for_review"
	InvoiceDispositionsTotal  = "commercial_billing_invoice_dispositions_total"
	ScheduledEmissionsPending = "commercial_billing_scheduled_emissions_pending"
	ProviderCertified         = "commercial_billing_provider_certified"
	DependencyRequestSeconds  = "platform_dependency_request_seconds"
)

// Buckets: purchase → event targets 60 s (§11.1); webhook processing < 10 s
// p95; sagas page at 30 min.
var (
	purchaseBuckets = []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300}
	webhookBuckets  = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
	sagaBuckets     = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600}
)

// Metrics holds every §11.2 collector Billing owns.
type Metrics struct {
	purchaseToEvent, webhookSeconds, sagaSeconds                      *gincommon.HistogramVec
	eventsEmitted, operatorActions, jobRuns                           *gincommon.CounterVec
	webhooks, signatureFailures, rpAttempts, downgradesBlocked        *gincommon.CounterVec
	repricing, seatChanges, deferred, deferredAmount, addonPurchases  *gincommon.CounterVec
	paymentFailures, dunning, dispositions                            *gincommon.CounterVec
	seatMismatches, unmatched                                         gincommon.Counter
	backlog, inFlight, subscriptions, findings, paused, held, pending *gincommon.GaugeVec
	certified                                                         *gincommon.GaugeVec
	overdue                                                           gincommon.Gauge
	dependency                                                        *gincommon.HistogramVec // shared platform_dependency_request_seconds
}

var _ port.Metrics = (*Metrics)(nil)

// New registers the collectors on reg (gincommon.MetricsRegisterer() in
// the server; a fresh registry in jobs and tests). gincommon's metrics
// identity must be initialised first (ObservabilityMiddlewares).
func New(reg gincommon.Registerer) (*Metrics, error) {
	m := &Metrics{}
	var errs []error
	cv := func(name, help string, labels ...string) *gincommon.CounterVec {
		c, err := gincommon.NewCounterVec(reg, name, help, labels...)
		errs = append(errs, err)
		return c
	}
	gv := func(name, help string, labels ...string) *gincommon.GaugeVec {
		g, err := gincommon.NewGaugeVec(reg, name, help, labels...)
		errs = append(errs, err)
		return g
	}
	hv := func(name, help string, buckets []float64, labels ...string) *gincommon.HistogramVec {
		h, err := gincommon.NewHistogramVec(reg, name, help, buckets, labels...)
		errs = append(errs, err)
		return h
	}
	m.purchaseToEvent = hv(PurchaseToEventSeconds, "Payment confirmed to the billing.events event committed, by purchase kind (Billing's share of the 60 s targets).",
		purchaseBuckets, "kind")
	m.eventsEmitted = cv(EventsEmittedTotal, "billing.events events enqueued, by event type.", "event_type")
	m.operatorActions = cv(OperatorActionsTotal, "Operator route calls, by action (route id) and outcome.", "action", "outcome")
	m.jobRuns = cv(CrossTenantJobRunsTotal, "Cross-tenant job runs (CronJobs and reconciliation), by job name and result (success | partial | failure).", "job_name", "result")

	m.webhooks = cv(WebhooksTotal, "Provider callbacks, by provider, normalised type and result.", "provider", "type", "result")
	m.signatureFailures = cv(WebhookSignatureFailures, "Callbacks refused for a bad signature or livemode (BIL-PAY-2).", "provider")
	m.webhookSeconds = hv(WebhookProcessingSeconds, "Callback received to processed.", webhookBuckets, "provider", "kind")
	m.backlog = gv(WebhookBacklog, "Ledger rows waiting, by status (exporter, 30 s).", "status")
	m.sagaSeconds = hv(SagaDurationSeconds, "Payment confirmed to TenantConverted, by saga kind.", sagaBuckets, "kind")
	m.inFlight = gv(SagasInFlight, "Open conversion sagas, by saga state.", "saga_state")
	m.rpAttempts = cv(RPConvertAttemptsTotal, "RP-3 calls, by result.", "result")
	m.subscriptions = gv(Subscriptions, "Subscriptions, by status, plan and provider (exporter).", "status", "plan", "provider")
	m.downgradesBlocked = cv(DowngradesBlockedTotal, "Downgrades blocked by I-18, by blocker.", "blocker")
	m.repricing = cv(RepricingScheduledTotal, "Renewal price moves scheduled (§8.16), by direction.", "direction")
	m.seatChanges = cv(SeatChangesTotal, "Seat changes, by direction and result.", "direction", "result")
	m.deferred = cv(ProrationDeferredTotal, "Seat purchases added free until renewal (§8.5.3), by currency and reason.", "currency", "reason")
	m.deferredAmount = cv(ProrationDeferredAmount, "Sum of deferred_amount_minor not charged, by currency.", "currency")
	m.addonPurchases = cv(AddonPurchasesTotal, "Token pack purchases, by pack and result.", "pack_code", "result")
	m.paymentFailures = cv(PaymentFailuresTotal, "Payment declines, by mapped reason.", "reason")
	m.dunning = cv(DunningTransitionsTotal, "Lapse-flow transitions, by target status.", "to")
	m.findings = gv(ReconciliationFindings, "Findings of the last reconciliation run, by kind and repaired.", "kind", "repaired")
	m.paused = gv(CollectionPaused, "Subscriptions with collection paused (CD-24), by origin.", "by")
	m.held = gv(InvoicesHeldForReview, "Drafts held during a collection pause with no B-O16 decision, by currency.", "currency")
	m.dispositions = cv(InvoiceDispositionsTotal, "B-O16 decisions, by action.", "action")
	m.pending = gv(ScheduledEmissionsPending, "Pending delayed emissions (BIL-EVT-6), by builder.", "builder")
	m.certified = gv(ProviderCertified, "1 when the loaded adapter version has a valid certification (BIL-PROV-1).", "provider", "adapter_version")
	var err error
	m.seatMismatches, err = gincommon.NewCounter(reg, SeatProjectionMismatches, "Seat projection mismatches found (§8.6.4).")
	errs = append(errs, err)
	m.unmatched, err = gincommon.NewCounter(reg, UnmatchedConversionsTotal, "TenantConverted events with no saga.")
	errs = append(errs, err)
	m.overdue, err = gincommon.NewGauge(reg, SagasOverdue, "Sagas not confirmed 30 minutes after payment (alert source).")
	errs = append(errs, err)
	// Tier 1 shared histogram. platform-events registers the same registry
	// entry for SNS/SQS calls and reuses an identical collector already
	// registered, so New must run before the event bus is built (cmd/server,
	// cmd/jobs); then both share one series.
	m.dependency, err = gincommon.NewSharedHistogramVec(reg, DependencyRequestSeconds)
	errs = append(errs, err)
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return m, nil
}

// Dependency records one outbound call (httpx.SetObserver).
func (m *Metrics) Dependency(dependency, operation, outcome string, d time.Duration) {
	if m.dependency != nil {
		m.dependency.WithLabelValues(dependency, operation, outcome).Observe(seconds(d))
	}
}

// PurchaseToEvent implements port.Metrics.
func (m *Metrics) PurchaseToEvent(kind string, d time.Duration) {
	m.purchaseToEvent.WithLabelValues(kind).Observe(seconds(d))
}

// SagaConfirmed implements port.Metrics.
func (m *Metrics) SagaConfirmed(kind string, d time.Duration) {
	m.sagaSeconds.WithLabelValues(kind).Observe(seconds(d))
}

// RPConvertAttempt implements port.Metrics.
func (m *Metrics) RPConvertAttempt(outcome string) { m.rpAttempts.WithLabelValues(outcome).Inc() }

// UnmatchedConversion implements port.Metrics.
func (m *Metrics) UnmatchedConversion() { m.unmatched.Inc() }

// DowngradeBlocked implements port.Metrics.
func (m *Metrics) DowngradeBlocked(blocker string) {
	m.downgradesBlocked.WithLabelValues(blocker).Inc()
}

// RepricingScheduled implements port.Metrics.
func (m *Metrics) RepricingScheduled(direction string) { m.repricing.WithLabelValues(direction).Inc() }

// SeatChange implements port.Metrics.
func (m *Metrics) SeatChange(direction, outcome string) {
	m.seatChanges.WithLabelValues(direction, outcome).Inc()
}

// SeatProjectionMismatch implements port.Metrics.
func (m *Metrics) SeatProjectionMismatch() { m.seatMismatches.Inc() }

// ProrationDeferred implements port.Metrics.
func (m *Metrics) ProrationDeferred(currency, reason string, amountMinor int64) {
	m.deferred.WithLabelValues(currency, reason).Inc()
	m.deferredAmount.WithLabelValues(currency).Add(float64(amountMinor))
}

// AddonPurchase implements port.Metrics.
func (m *Metrics) AddonPurchase(packCode, outcome string) {
	m.addonPurchases.WithLabelValues(packCode, outcome).Inc()
}

// PaymentFailure implements port.Metrics.
func (m *Metrics) PaymentFailure(reason string) { m.paymentFailures.WithLabelValues(reason).Inc() }

// DunningTransition implements port.Metrics.
func (m *Metrics) DunningTransition(to string) { m.dunning.WithLabelValues(to).Inc() }

// InvoiceDisposition implements port.Metrics.
func (m *Metrics) InvoiceDisposition(action string) { m.dispositions.WithLabelValues(action).Inc() }

// WebhookProcessed implements port.Metrics.
func (m *Metrics) WebhookProcessed(provider, kind, outcome string, d time.Duration) {
	m.webhooks.WithLabelValues(provider, kind, outcome).Inc()
	if outcome == "processed" {
		m.webhookSeconds.WithLabelValues(provider, kind).Observe(seconds(d))
	}
}

// WebhookDuplicate counts a redelivered callback (B-W1).
func (m *Metrics) WebhookDuplicate(provider, kind string) {
	m.webhooks.WithLabelValues(provider, kind, "duplicate").Inc()
}

// SignatureFailure counts a refused callback (BIL-PAY-2).
func (m *Metrics) SignatureFailure(provider string) {
	m.signatureFailures.WithLabelValues(provider).Inc()
}

// SagasOverdue implements port.Metrics.
func (m *Metrics) SagasOverdue(n int) { m.overdue.Set(float64(n)) }

// ReconciliationFindings implements port.Metrics: the last run's findings
// replace the previous run's.
func (m *Metrics) ReconciliationFindings(counts map[[2]string]int) {
	m.findings.Reset()
	for k, n := range counts {
		m.findings.WithLabelValues(k[0], k[1]).Set(float64(n))
	}
}

// CrossTenantJobRun implements port.Metrics.
func (m *Metrics) CrossTenantJobRun(job, result string) {
	m.jobRuns.WithLabelValues(job, result).Inc()
}

// EventEmitted counts one billing.events event enqueued.
func (m *Metrics) EventEmitted(eventType string) {
	m.eventsEmitted.WithLabelValues(eventType).Inc()
}

// OperatorAction counts one operator route call.
func (m *Metrics) OperatorAction(action, outcome string) {
	m.operatorActions.WithLabelValues(action, outcome).Inc()
}

// ProviderCertified sets the certification gauge for one loaded adapter.
func (m *Metrics) ProviderCertified(provider, adapterVersion string, ok bool) {
	v := 0.0
	if ok {
		v = 1
	}
	m.certified.WithLabelValues(provider, adapterVersion).Set(v)
}

// SetSnapshot replaces the exporter gauges with a fresh database reading.
func (m *Metrics) SetSnapshot(s port.MetricsSnapshot) {
	setMap(m.backlog, s.WebhookBacklog)
	setMap(m.inFlight, s.SagasInFlight)
	setMap(m.paused, s.CollectionPaused)
	setMap(m.held, s.InvoicesHeld)
	setMap(m.pending, s.EmissionsPending)
	m.overdue.Set(float64(s.SagasOverdue))
	m.subscriptions.Reset()
	for k, n := range s.Subscriptions {
		m.subscriptions.WithLabelValues(k[0], k[1], k[2]).Set(float64(n))
	}
}

func setMap(g *gincommon.GaugeVec, values map[string]int) {
	g.Reset()
	for k, n := range values {
		g.WithLabelValues(k).Set(float64(n))
	}
}

func seconds(d time.Duration) float64 { return max(d.Seconds(), 0) }

// Repaired is the reconciliation_findings repaired label value.
func Repaired(b bool) string { return strconv.FormatBool(b) }
