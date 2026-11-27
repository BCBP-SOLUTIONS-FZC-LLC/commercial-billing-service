//go:build integration

package flows_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// B-O11 closes the current row and adds the new one; overlap → 409; AL-5
// under platform_tenant. B-O12 adds a pack.
func TestPriceBook(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	from := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	p, err := e.svc.AddPlanPrice(ctx, operator, app.PlanPriceRequest{Plan: "starter", Interval: "month", Currency: "AED", UnitAmountMinor: 12900,
		Provider: "stripe", ProviderPriceID: "price_aed_1", EffectiveFrom: from, Reason: "AED launch pricing approved"})
	if err != nil || p.PriceID == "" {
		t.Fatalf("%+v %v", p, err)
	}
	_, err = e.svc.AddPlanPrice(ctx, operator, app.PlanPriceRequest{Plan: "starter", Interval: "month", Currency: "AED", UnitAmountMinor: 13900,
		Provider: "stripe", ProviderPriceID: "price_aed_2", EffectiveFrom: from.AddDate(0, 6, 0), Reason: "AED price increase approved"})
	if err != nil {
		t.Fatal(err)
	}
	if got := scalar[time.Time](t, `SELECT effective_to FROM plan_prices WHERE price_id = $1`, p.PriceID); !got.Equal(from.AddDate(0, 6, 0)) {
		t.Fatalf("previous row not closed: %s", got)
	}
	// A row starting before the current one's start overlaps; the refused
	// change leaves no audit entry (BIL-AUD-1: one transaction).
	before := scalar[int](t, `SELECT count(*) FROM pending_audit_entries WHERE entry->>'entry_type' = 'billing.price_book.changed'`)
	if _, err := e.svc.AddPlanPrice(ctx, operator, app.PlanPriceRequest{Plan: "starter", Interval: "month", Currency: "AED", UnitAmountMinor: 1,
		Provider: "stripe", ProviderPriceID: "x", EffectiveFrom: from.AddDate(0, 3, 0), Reason: "overlapping row for test"}); !domain.HasCode(err, domain.ErrPriceOverlap) {
		t.Fatalf("overlap: %v", err)
	}
	if after := scalar[int](t, `SELECT count(*) FROM pending_audit_entries WHERE entry->>'entry_type' = 'billing.price_book.changed'`); after != before {
		t.Fatalf("audit entry written for a refused change: %d → %d", before, after)
	}
	if n := scalar[int](t, `SELECT count(*) FROM pending_audit_entries WHERE tenant_id = '00000000-0000-0000-0000-0000000000b1'
		AND entry->>'entry_type' = 'billing.price_book.changed'`); n < 2 {
		t.Fatalf("audit %d", n)
	}
	if _, err := e.svc.AddPlanPrice(ctx, operator, app.PlanPriceRequest{Plan: "pro", Interval: "month", Currency: "AED", UnitAmountMinor: 25300,
		Provider: "stripe", ProviderPriceID: "price_bad", EffectiveFrom: from, Reason: "misconfigured price for test"}); !domain.HasCode(err, domain.ErrProviderPriceMismatch) {
		t.Fatalf("price mismatch: %v", err)
	}
	if _, err := e.svc.UpsertTokenPack(ctx, operator, app.TokenPackRequest{PackCode: "tokens_25m", Tokens: 25_000_000, Currency: "USD", AmountMinor: 89900,
		Provider: "stripe", ProviderPriceID: "price_t25", Active: ptr(true), EffectiveFrom: from, Reason: "BIZ-1 pack sizes approved"}); err != nil {
		t.Fatal(err)
	}
}

func certReq(suite string, skip string) app.CertificationRequest {
	var results []app.CertificationResult
	for group, cases := range domain.CertSuite["cert-1.0"] {
		for _, c := range cases {
			if group+"/"+c != skip {
				results = append(results, app.CertificationResult{Capability: group, Case: c, Passed: true, EvidenceRef: "s3://e/" + c})
			}
		}
	}
	results = append(results, app.CertificationResult{Capability: "proration", Case: "preview_equals_charge", Passed: true, EvidenceRef: "s3://e/p"})
	return app.CertificationRequest{Provider: "stripe", AdapterVersion: "9.9.9+" + newTenant()[:8], ProviderAPIVersion: "2026-11-18.endive",
		SuiteVersion: suite, ProrationMode: "native", Capabilities: json.RawMessage(`{"Proration":"native"}`), Results: results,
		ProviderAccountRef: "acct_test", RunStartedAt: time.Now().Add(-time.Hour), RunFinishedAt: time.Now(), Reason: "Phase 1 certification run"}
}

// B-O14 → B-O17 → B-O15 (BIL-PROV-4): suite version, incomplete runs
// recorded failed, duplicate run, self-review refused, approval enables,
// revocation.
func TestCertification(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, err := e.svc.SubmitCertification(ctx, operator, certReq("cert-0.9", "")); !domain.HasCode(err, domain.ErrSuiteVersionMismatch) {
		t.Fatalf("suite: %v", err)
	}
	var de *domain.DomainError
	if _, err := e.svc.SubmitCertification(ctx, operator, certReq("cert-1.0", "refunds/partial")); !asDomain(err, &de) ||
		de.Code != domain.ErrCertificationIncomplete || de.Extensions["missing_cases"].([]string)[0] != "refunds/partial" {
		t.Fatalf("incomplete: %v", err)
	}
	req := certReq("cert-1.0", "")
	v, err := e.svc.SubmitCertification(ctx, operator, req)
	if err != nil || v.Status != "pending_review" || v.EnabledForNewAccounts {
		t.Fatalf("submit %+v %v", v, err)
	}
	if _, err := e.svc.SubmitCertification(ctx, operator, req); !domain.HasCode(err, domain.ErrCertificationExists) {
		t.Fatalf("duplicate: %v", err)
	}
	reviewer := app.Caller{UserID: "7f7f7f7f-1111-4222-8333-944455556666", Roles: []string{"platform_operator", "pci_owner_reviewer"}}
	if _, err := e.svc.ReviewCertification(ctx, operator, v.ID, app.ReviewRequest{Decision: "approve", Reason: "self approval attempt"}); !domain.HasCode(err, domain.ErrSelfReviewNotAllowed) {
		t.Fatalf("self review: %v", err)
	}
	ok, err := e.svc.ReviewCertification(ctx, reviewer, v.ID, app.ReviewRequest{Decision: "approve", Reason: "Evidence for all cases checked"})
	if err != nil || ok.Status != "passed" || !ok.EnabledForNewAccounts || *ok.ReviewedBy != reviewer.UserID {
		t.Fatalf("review %+v %v", ok, err)
	}
	if _, err := e.svc.ReviewCertification(ctx, reviewer, v.ID, app.ReviewRequest{Decision: "reject", Reason: "second decision attempt"}); !domain.HasCode(err, domain.ErrCertificationNotPending) {
		t.Fatalf("second review: %v", err)
	}
	r, err := e.svc.RevokeCertification(ctx, operator, v.ID, app.OperatorReason{Reason: "API version change shipped without a run"})
	if err != nil || r.Status != "revoked" || r.EnabledForNewAccounts {
		t.Fatalf("revoke %+v %v", r, err)
	}
	if _, err := e.svc.RevokeCertification(ctx, operator, v.ID, app.OperatorReason{Reason: "again, a long enough reason"}); !domain.HasCode(err, domain.ErrCertificationNotActive) {
		t.Fatalf("second revoke: %v", err)
	}
}

// B-O13 puts a ledger row back to received.
func TestReplay(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	mustNoErr(t, sql(ctx, `INSERT INTO payment_event_ledger (provider, provider_event_id, event_type, normalised_kind, livemode, provider_created_at,
		payload, status, attempts, outcome) VALUES ('stripe', 'evt_replay_1', 'invoice.paid', 'invoice_paid', false, now(), '{}', 'failed', 10, 'failed:x')`))
	if err := e.svc.ReplayWebhookEvent(ctx, operator, "stripe", "evt_replay_1", app.OperatorReason{Reason: "Fixed handler deployed"}); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, `SELECT status::text||'/'||attempts FROM payment_event_ledger WHERE provider_event_id = 'evt_replay_1'`); got != "received/0" {
		t.Fatalf("ledger %s", got)
	}
	if err := e.svc.ReplayWebhookEvent(ctx, operator, "stripe", "evt_none", app.OperatorReason{Reason: "a long enough reason"}); !domain.HasCode(err, domain.ErrLedgerEventNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}
