//go:build integration

package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

func ptr[T any](v T) *T { return &v }

func auditCount(t *testing.T, entryType string) int {
	t.Helper()
	return scalar[int](t, `SELECT count(*) FROM pending_audit_entries WHERE entry->>'entry_type' = $1`, entryType)
}

// B-O11 / B-O12 (§14.5): the provider price is verified first (422
// PROVIDER_PRICE_MISMATCH, an unloaded provider refused); the manual
// provider has nothing to verify; a failed write leaves no row and no
// AL-5 entry (BIL-AUD-1: one transaction).
func TestPriceBookProviderChecksAndAtomicAudit(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	from := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	pack := app.TokenPackRequest{PackCode: "tokens_jobs_bad", Tokens: 1_000_000, Currency: "USD", AmountMinor: 4900, Provider: "stripe",
		ProviderPriceID: "price_bad", Active: ptr(true), EffectiveFrom: from, Reason: "pack with a mismatching provider price"}
	if _, err := e.svc.UpsertTokenPack(ctx, operator, pack); !domain.HasCode(err, domain.ErrProviderPriceMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	pack.Provider, pack.ProviderPriceID = "adyen", "price_x"
	if _, err := e.svc.UpsertTokenPack(ctx, operator, pack); err == nil {
		t.Fatal("pack for a provider that is not loaded accepted")
	}
	if n := scalar[int](t, `SELECT count(*) FROM token_packs WHERE pack_code = 'tokens_jobs_bad'`); n != 0 {
		t.Fatalf("refused packs stored: %d", n)
	}

	before := auditCount(t, "billing.price_book.changed")
	e.f.fail("Admin.AddTokenPack", 0)
	pack.PackCode, pack.Provider, pack.ProviderPriceID = "tokens_jobs_ok", "stripe", "price_jobs_ok"
	if _, err := e.svc.UpsertTokenPack(ctx, operator, pack); !errors.Is(err, errInjected) {
		t.Fatalf("write failure: %v", err)
	}
	if after := auditCount(t, "billing.price_book.changed"); after != before {
		t.Fatalf("audit entry for a failed change: %d → %d", before, after)
	}
	e.f.clear()

	verified := len(e.prov.called("VerifyPrice"))
	p, err := e.svc.AddPlanPrice(ctx, operator, app.PlanPriceRequest{Plan: "starter", Interval: "month", Currency: "JPY", UnitAmountMinor: 2900,
		Provider: "manual", EffectiveFrom: from, Reason: "test manual JPY row"})
	if err != nil || p.PriceID == "" || p.ProviderPriceID != nil {
		t.Fatalf("manual price %+v %v", p, err)
	}
	if n := len(e.prov.called("VerifyPrice")); n != verified {
		t.Fatalf("manual price sent to a provider: %v", e.prov.called("VerifyPrice"))
	}
}

// B-O13: an operator replay is logged with the operator and reason.
func TestReplayIsLogged(t *testing.T) {
	e := newEnv(t)
	e.exec(`INSERT INTO payment_event_ledger (provider, provider_event_id, event_type, normalised_kind, livemode, provider_created_at,
		payload, status, attempts, outcome) VALUES ('stripe', 'evt_jobs_replay', 'invoice.paid', 'invoice_paid', false, now(), '{}', 'failed', 10, 'failed:x')`)
	if err := e.svc.ReplayWebhookEvent(context.Background(), operator, "stripe", "evt_jobs_replay", app.OperatorReason{Reason: "Fixed handler deployed"}); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, `SELECT status::text FROM payment_event_ledger WHERE provider_event_id = 'evt_jobs_replay'`); got != "received" {
		t.Fatalf("ledger %s", got)
	}
	if !e.log.has("INFO ledger row replayed by an operator") {
		t.Fatal("replay not logged")
	}
}

func fullRun(mode, declared string) app.CertificationRequest {
	var results []app.CertificationResult
	for group, cases := range domain.CertSuite["cert-1.0"] {
		for _, c := range cases {
			results = append(results, app.CertificationResult{Capability: group, Case: c, Passed: true, EvidenceRef: "s3://e/" + c})
		}
	}
	results = append(results, app.CertificationResult{Capability: "proration", Case: "preview_equals_charge", Passed: true, EvidenceRef: "s3://e/p"})
	return app.CertificationRequest{Provider: "stripe", AdapterVersion: "9.9.9+" + newTenant()[:8], SuiteVersion: "cert-1.0", ProrationMode: mode,
		Capabilities: json.RawMessage(`{"Proration":"` + declared + `"}`), Results: results, RunStartedAt: time.Now().Add(-time.Hour),
		RunFinishedAt: time.Now(), Reason: "Phase 1 certification run"}
}

// B-O14 (§14.7): capabilities must be a JSON object; a proration mode the
// adapter does not declare (or without its passing case) is a missing
// case — fallback is refused outright, native recorded failed.
func TestCertificationProrationRules(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	bad := fullRun("native", "native")
	bad.Capabilities = json.RawMessage(`[1]`)
	if _, err := e.svc.SubmitCertification(ctx, operator, bad); !domain.HasCode(err, domain.ErrValidation) {
		t.Fatalf("capabilities: %v", err)
	}
	var de *domain.DomainError
	if _, err := e.svc.SubmitCertification(ctx, operator, fullRun("fallback", "native")); !errors.As(err, &de) ||
		de.Code != domain.ErrCertificationIncomplete || de.Extensions["id"] != nil {
		t.Fatalf("fallback: %v", err)
	}
	if missing, _ := de.Extensions["missing_cases"].([]string); len(missing) == 0 || missing[len(missing)-1] != "proration/fallback" {
		t.Fatalf("fallback missing %v", de.Extensions["missing_cases"])
	}
	v, err := e.svc.SubmitCertification(ctx, operator, fullRun("native", "fallback"))
	if !errors.As(err, &de) || de.Code != domain.ErrCertificationIncomplete || v.Status != "failed" {
		t.Fatalf("native undeclared: %+v %v", v, err)
	}
	if got := scalar[string](t, `SELECT status::text FROM provider_certifications WHERE id = $1`, v.ID); got != "failed" {
		t.Fatalf("recorded %s", got)
	}
}

// B-O17 / B-O15 (BIL-PROV-4): unknown ids are 404; a failed read or write
// changes nothing and writes no AL-5 entry.
func TestCertificationReviewAndRevokeFailures(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	reviewer := app.Caller{UserID: "7f7f7f7f-1111-4222-8333-944455556666", Roles: []string{"platform_operator", "pci_owner_reviewer"}}
	approve := app.ReviewRequest{Decision: "approve", Reason: "Evidence for all cases checked"}
	revoke := app.OperatorReason{Reason: "API version change shipped without a run"}
	unknown := newTenant()
	if _, err := e.svc.ReviewCertification(ctx, reviewer, unknown, approve); !domain.HasCode(err, domain.ErrCertificationNotFound) {
		t.Fatalf("review unknown: %v", err)
	}
	if _, err := e.svc.RevokeCertification(ctx, operator, unknown, revoke); !domain.HasCode(err, domain.ErrCertificationNotFound) {
		t.Fatalf("revoke unknown: %v", err)
	}
	v, err := e.svc.SubmitCertification(ctx, operator, fullRun("native", "native"))
	if err != nil {
		t.Fatal(err)
	}
	status := func() string {
		return scalar[string](t, `SELECT status::text FROM provider_certifications WHERE id = $1`, v.ID)
	}
	reviewed, revoked := auditCount(t, "billing.provider.certification_reviewed"), auditCount(t, "billing.provider.certification_revoked")

	for _, name := range []string{"Admin.Certification", "Admin.ReviewCertification"} {
		e.f.fail(name, 0)
		if _, err := e.svc.ReviewCertification(ctx, reviewer, v.ID, approve); !errors.Is(err, errInjected) {
			t.Fatalf("%s: %v", name, err)
		}
		e.f.clear()
	}
	if status() != "pending_review" || auditCount(t, "billing.provider.certification_reviewed") != reviewed {
		t.Fatal("changed by a failed review")
	}
	if _, err := e.svc.ReviewCertification(ctx, reviewer, v.ID, approve); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Admin.Certification", "Admin.RevokeCertification"} {
		e.f.fail(name, 0)
		if _, err := e.svc.RevokeCertification(ctx, operator, v.ID, revoke); !errors.Is(err, errInjected) {
			t.Fatalf("%s: %v", name, err)
		}
		e.f.clear()
	}
	if status() != "passed" || auditCount(t, "billing.provider.certification_revoked") != revoked {
		t.Fatal("changed by a failed revocation")
	}
}
