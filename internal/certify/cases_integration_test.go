//go:build integration

package certify

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// In test mode the cases assert provider behaviour. These scenarios replay
// that branch against stripe-mock with the provider's answers scripted, so
// each assertion is shown to pass on a conforming provider and to fail,
// with its message, on a non-conforming one.

// A provider that cannot create subscriptions or list events fails every
// case that needs one; only the stored-data scan passes.
func TestCasesFailWithoutAProvider(t *testing.T) {
	f := newFixture(t)
	s := f.scenario(wiring{})
	s.inner.failOn("CreateSubscription", 0).failOn("ListSince", 0)
	results := Run(context.Background(), s.e, Cases(port.ProrationNative))
	for _, r := range results {
		pass := r.Case == "no_card_data"
		if r.Passed != pass {
			t.Errorf("%s/%s passed=%v: %s", r.Capability, r.Case, r.Passed, r.Error)
		}
		// caseDecline reports the signup error itself when the signup
		// fails (it used to panic writing into addon's nil evidence).
		if r.Case == "decline" {
			if strings.Contains(r.Error, "panic") || !strings.Contains(r.Error, "injected CreateSubscription failure") {
				t.Errorf("decline: %s", r.Error)
			}
			if r.Evidence["error"] == nil {
				t.Errorf("decline evidence has no error: %v", r.Evidence)
			}
			continue
		}
		if !pass && !strings.Contains(r.Error, "injected") {
			t.Errorf("%s/%s: %s", r.Capability, r.Case, r.Error)
		}
		if r.Evidence["target"] != string(TargetTestMode) {
			t.Errorf("%s/%s evidence %v", r.Capability, r.Case, r.Evidence)
		}
	}
}

// An empty event and invoice listing fails the cases that need an event.
func TestCasesWithAnEmptyListing(t *testing.T) {
	f := newFixture(t)
	s := f.scenario(wiring{})
	s.inner.list = func(int) (port.ReconPage, bool) { return port.ReconPage{}, true }
	for name, c := range map[string]func(context.Context, *Env) (Evidence, error){
		"signature": caseSignatureVerified, "tolerance": caseToleranceEnforced, "redelivered": caseRedelivered,
		"same_id": caseSameIDOnRedelivery, "dedupe": caseLedgerDedupe,
	} {
		check(t, name, c, s.e, "listed no event")
	}
	check(t, "listing", caseInvoiceListing, s.e, "returned no invoice")
}

func TestRenewalHelper(t *testing.T) {
	f := newFixture(t)
	s := f.scenario(wiring{})
	ctx := context.Background()
	if _, err := renewal(ctx, s.e, "00000000-0000-4000-8000-000000000000"); err == nil || err.Error() != "no current period" {
		t.Errorf("unknown subscription: %v", err)
	}
	sub, err := subscribe(ctx, s.e, domain.PlanPro, 2)
	if err != nil {
		t.Fatal(err)
	}
	s.clock.failAfter = 1
	if _, err := renewal(ctx, s.e, sub.Sub); err == nil || !strings.Contains(err.Error(), "clock") {
		t.Errorf("clock failure: %v", err)
	}
	s.clock.failAfter = 0
	s.inner.failOn("ListSince", 0)
	if _, err := renewal(ctx, s.e, sub.Sub); err == nil || !strings.Contains(err.Error(), "injected ListSince") {
		t.Errorf("delivery failure: %v", err)
	}
	// The suite's own statements failing stops the signup.
	s.brokenSuper(t)
	if _, err := subscribe(ctx, s.e, domain.PlanPro, 2); err == nil {
		t.Error("signup passed without its database")
	}
}

func TestRecurringSubscriptionCases(t *testing.T) {
	f := newFixture(t)
	t.Run("create", func(t *testing.T) {
		check(t, "conforming", caseCreate, f.scenario(wiring{}).e, "")
		s := f.scenario(wiring{caseOuter: true})
		s.outer.failOn("FetchCurrent", 0)
		check(t, "fetch fails", caseCreate, s.e, "injected FetchCurrent")
		s = f.scenario(wiring{caseOuter: true})
		s.outer.fetch = func(ref domain.ProviderObjectRef) (domain.ProviderObjectState, error, bool) {
			return domain.ProviderObjectState{Ref: ref, Status: "incomplete"}, nil, true
		}
		check(t, "not active", caseCreate, s.e, "provider subscription status: got incomplete, want active")
	})

	t.Run("increase_quantity", func(t *testing.T) {
		s := f.scenario(wiring{svcOuter: true})
		s.outer.failOn("PreviewQuantityChange", 0)
		check(t, "preview fails", caseIncreaseQuantity, s.e, "B-P6")
		s = f.scenario(wiring{svcOuter: true})
		s.outer.failOn("ChangeQuantity", 0)
		check(t, "charge fails", caseIncreaseQuantity, s.e, "B-P7")
		s = f.scenario(wiring{svcOuter: true})
		s.outer.change = &port.ChangeResult{Pending: true, Outcome: port.PaymentOutcome{Status: port.PaymentPending}}
		check(t, "charge pending", caseIncreaseQuantity, s.e, "purchase status")
	})

	t.Run("change_plan_now", func(t *testing.T) {
		s := f.scenario(wiring{svcOuter: true})
		s.outer.failOn("PreviewPriceChange", 0)
		check(t, "preview fails", caseChangePlanNow, s.e, "B-P3")
		s = f.scenario(wiring{svcOuter: true})
		s.outer.failOn("ChangePrice", 0)
		check(t, "change fails", caseChangePlanNow, s.e, "B-P4")
	})

	t.Run("change_plan_period_end", func(t *testing.T) {
		s := f.scenario(wiring{svcOuter: true})
		s.outer.failOn("ScheduleAtPeriodEnd", 0)
		check(t, "schedule fails", caseChangePlanPeriodEnd, s.e, "B-P4 downgrade")
		s = f.scenario(wiring{svcOuter: true})
		s.outer.schedule = &port.ScheduleRef{}
		check(t, "no schedule id", caseChangePlanPeriodEnd, s.e, "no provider schedule")
		// The provider applies the schedule at the renewal.
		s = f.scenario(wiring{})
		s.clock.onAdvance = func(ctx context.Context) {
			_ = s.e.DB.Exec(ctx, `UPDATE subscriptions SET plan = 'pro' WHERE plan = 'enterprise'`)
		}
		check(t, "applied at renewal", caseChangePlanPeriodEnd, s.e, "")
		s = f.scenario(wiring{})
		check(t, "not applied", caseChangePlanPeriodEnd, s.e, "plan after the renewal: got enterprise, want pro")
		s = f.scenario(wiring{})
		s.clock.failAfter = 1
		check(t, "renewal fails", caseChangePlanPeriodEnd, s.e, "clock")
	})

	t.Run("schedule_lower_quantity", func(t *testing.T) {
		s := f.scenario(wiring{})
		s.clock.failAfter = 1
		check(t, "clock fails", caseScheduleLowerQuantity, s.e, "clock")
		s = f.scenario(wiring{svcOuter: true})
		s.outer.failOn("ScheduleAtPeriodEnd", 0)
		check(t, "no provider schedule", caseScheduleLowerQuantity, s.e, "want applying with 6 renewal seats")
		s = f.scenario(wiring{})
		s.clock.failAfter = 2
		check(t, "renewal fails", caseScheduleLowerQuantity, s.e, "clock")
		s = f.scenario(wiring{})
		check(t, "not applied", caseScheduleLowerQuantity, s.e, "quantity after the renewal")
		s = f.scenario(wiring{})
		s.schedules.insertErr = true
		check(t, "reduction refused", caseScheduleLowerQuantity, s.e, "B-P10")
		s = f.scenario(wiring{})
		s.schedules.dueOK = 0
		check(t, "job fails", caseScheduleLowerQuantity, s.e, "seat-renewal: injected")
		s = f.scenario(wiring{})
		s.schedules.dueOK = 1
		check(t, "job fails after the renewal", caseScheduleLowerQuantity, s.e, "injected schedule read")
	})

	t.Run("renew", func(t *testing.T) {
		s := f.scenario(wiring{})
		check(t, "period not rolled", caseRenew, s.e, "period not rolled")
		s = f.scenario(wiring{})
		s.clock.onAdvance = func(ctx context.Context) {
			_ = s.e.DB.Exec(ctx, `UPDATE subscriptions SET current_period_start = current_period_end,
				current_period_end = current_period_end + interval '1 month' WHERE current_period_end < $1`, s.e.Clock.Now())
		}
		check(t, "rolled", caseRenew, s.e, "")
		s = f.scenario(wiring{})
		s.clock.failAfter = 1
		check(t, "clock fails", caseRenew, s.e, "clock")
	})

	t.Run("cancel_at_period_end", func(t *testing.T) {
		s := f.scenario(wiring{svcOuter: true})
		s.outer.failOn("SetCancelAtPeriodEnd", 0)
		check(t, "provider refuses", caseCancelAtPeriodEnd, s.e, "B-P12")
		// A provider whose cancellation does not stick leaves auto_renew on.
		s = f.scenario(wiring{})
		s.sql(t, `CREATE FUNCTION certify_keep_renewing() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.auto_renew := true; RETURN NEW; END $$`)
		s.sql(t, `CREATE TRIGGER certify_keep_renewing BEFORE UPDATE ON subscriptions FOR EACH ROW EXECUTE FUNCTION certify_keep_renewing()`)
		check(t, "still renewing", caseCancelAtPeriodEnd, s.e, "auto_renew still true")
		s.sql(t, `DROP TRIGGER certify_keep_renewing ON subscriptions`)
		s.sql(t, `DROP FUNCTION certify_keep_renewing()`)
		s = f.scenario(wiring{})
		check(t, "not ended", caseCancelAtPeriodEnd, s.e, "status after the period end")
		s = f.scenario(wiring{})
		s.clock.failAfter = 1
		check(t, "renewal fails", caseCancelAtPeriodEnd, s.e, "clock")
	})

	t.Run("cancel_now", func(t *testing.T) {
		s := f.scenario(wiring{caseOuter: true})
		s.outer.failOn("CancelNow", 0)
		check(t, "provider refuses", caseCancelNow, s.e, "injected CancelNow")
		s = f.scenario(wiring{deliverOuter: true})
		s.outer.failOn("ListSince", 0)
		check(t, "delivery fails", caseCancelNow, s.e, "injected ListSince")
		s = f.scenario(wiring{})
		check(t, "not cancelled", caseCancelNow, s.e, "status after cancel now")
	})
}

func TestWebhookCases(t *testing.T) {
	f := newFixture(t)
	t.Run("signature", func(t *testing.T) {
		s := f.scenario(wiring{})
		s.e.Deliver.Handler = &scripted{codes: []int{http.StatusOK}}
		check(t, "unsigned accepted", caseSignatureVerified, s.e, "bad signature: got 200, want 400")
	})
	t.Run("redelivered", func(t *testing.T) {
		s := f.scenario(wiring{})
		s.e.Deliver.Handler = &scripted{codes: []int{http.StatusBadRequest}}
		check(t, "not refused", caseRedelivered, s.e, "delivery while the ledger is down: got 400")
		s = f.scenario(wiring{})
		s.e.Deliver.Handler = &scripted{codes: []int{http.StatusServiceUnavailable, http.StatusBadRequest}}
		check(t, "redelivery refused", caseRedelivered, s.e, "redelivery: got 400")
		s = f.scenario(wiring{})
		s.brokenSuper(t)
		check(t, "database down", caseRedelivered, s.e, "closed")
	})
	t.Run("same_id", func(t *testing.T) {
		s := f.scenario(wiring{caseOuter: true})
		s.outer.list = func(int) (port.ReconPage, bool) {
			return port.ReconPage{Items: []domain.ProviderObjectState{{Ref: domain.ProviderObjectRef{ID: "evt_same"}}}}, true
		}
		s.proxy.okFor["evt_same"] = 1
		check(t, "redelivery read fails", caseSameIDOnRedelivery, s.e, "500")
	})
	t.Run("out_of_order", func(t *testing.T) {
		s := f.scenario(wiring{deliverOuter: true})
		s.outer.failOn("ListSince", 0)
		check(t, "delivery fails", caseOutOfOrder, s.e, "injected ListSince")
		s = f.scenario(wiring{caseOuter: true})
		s.outer.failOn("SetCancelAtPeriodEnd", 0)
		check(t, "cancel refused", caseOutOfOrder, s.e, "injected SetCancelAtPeriodEnd")
		s = f.scenario(wiring{caseOuter: true})
		s.outer.failOn("SetCancelAtPeriodEnd", 1)
		check(t, "resume refused", caseOutOfOrder, s.e, "injected SetCancelAtPeriodEnd")
		s = f.scenario(wiring{caseOuter: true})
		s.outer.failOn("ListSince", 0)
		check(t, "listing fails", caseOutOfOrder, s.e, "injected ListSince")
		fresh := func(ids ...string) func(int) (port.ReconPage, bool) {
			return func(int) (port.ReconPage, bool) {
				var items []domain.ProviderObjectState
				for _, id := range ids {
					items = append(items, domain.ProviderObjectState{Ref: domain.ProviderObjectRef{ID: id}})
				}
				return port.ReconPage{Items: items}, true
			}
		}
		s = f.scenario(wiring{caseOuter: true})
		s.outer.list = fresh("evt_ooo_fail")
		s.proxy.okFor["evt_ooo_fail"] = 0
		check(t, "event read fails", caseOutOfOrder, s.e, "500")
		s = f.scenario(wiring{caseOuter: true})
		s.outer.list = fresh("evt_ooo_2", "evt_ooo_1")
		s.ledger.claimOK = 1 // the signup's delivery drains once; the reordered one fails
		check(t, "drain fails", caseOutOfOrder, s.e, "injected claim")
		s = f.scenario(wiring{caseOuter: true})
		s.outer.list = fresh("evt_ooo_4", "evt_ooo_3")
		ev := check(t, "converges", caseOutOfOrder, s.e, "")
		if ids, _ := ev["delivered_newest_first"].([]string); strings.Join(ids, ",") != "evt_ooo_4,evt_ooo_3" {
			t.Errorf("delivered %v", ev["delivered_newest_first"])
		}
	})
}

func TestInvoiceAndPaymentMethodCases(t *testing.T) {
	f := newFixture(t)
	t.Run("first_invoice", func(t *testing.T) {
		s := f.scenario(wiring{})
		s.ledger.insertErr = true
		check(t, "callback not ledgered", caseInvoiceByReference, s.e, "injected ledger insert")
		s = f.scenario(wiring{caseOuter: true, deliverOuter: true})
		s.outer.failOn("ListSince", 0)
		check(t, "delivery fails", caseInvoiceByReference, s.e, "injected ListSince")
		s = f.scenario(wiring{caseOuter: true})
		s.outer.list = func(int) (port.ReconPage, bool) { return port.ReconPage{}, true }
		check(t, "nothing mirrored", caseInvoicePDF, s.e, "no invoice mirrored")
	})
	t.Run("by_reference", func(t *testing.T) {
		s := f.scenario(wiring{})
		s.inner.failOn("GetInvoice", 0)
		check(t, "read fails", caseInvoiceByReference, s.e, "injected GetInvoice")
		s = f.scenario(wiring{})
		check(t, "fixture invoice", caseInvoiceByReference, s.e, "invoice status")
	})
	t.Run("pdf", func(t *testing.T) {
		s := f.scenario(wiring{})
		s.inner.failOn("InvoicePDFLink", 0)
		check(t, "link fails", caseInvoicePDF, s.e, "injected InvoicePDFLink")
		s = f.scenario(wiring{})
		s.inner.pdf = &port.URLWithExpiry{URL: "http://insecure/pdf"}
		check(t, "not https", caseInvoicePDF, s.e, "no https PDF link")
	})
	t.Run("listing", func(t *testing.T) {
		s := f.scenario(wiring{caseOuter: true})
		s.outer.list = func(n int) (port.ReconPage, bool) {
			item := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{ID: "in_" + string(rune('a'+n))}}
			if n == 1 {
				return port.ReconPage{Items: []domain.ProviderObjectState{item}, NextCursor: "in_a"}, true
			}
			return port.ReconPage{Items: []domain.ProviderObjectState{item}}, true
		}
		ev := check(t, "two pages", caseInvoiceListing, s.e, "")
		if ev["pages"] != 2 || ev["items"] != 2 {
			t.Errorf("evidence %v", ev)
		}
	})
	t.Run("hosted_page", func(t *testing.T) {
		s := f.scenario(wiring{svcOuter: true})
		s.outer.failOn("CreatePaymentMethodSetup", 0)
		check(t, "setup fails", caseHostedPMPage, s.e, "B-P16")
		s = f.scenario(wiring{svcOuter: true})
		s.outer.setup = &port.HostedSession{ProviderSessionID: "seti_1", URL: "http://insecure/setup", ExpiresAt: time.Now().Add(time.Hour)}
		check(t, "not https", caseHostedPMPage, s.e, "BIL-API-5")
	})
	t.Run("used_for_renewal", func(t *testing.T) {
		s := f.scenario(wiring{caseOuter: true})
		s.outer.failOn("AttachDefaultPaymentMethod", 0)
		check(t, "attach fails", casePMUsedForRenewal, s.e, "injected AttachDefaultPaymentMethod")
		s = f.scenario(wiring{})
		s.clock.failAfter = 1
		check(t, "renewal fails", casePMUsedForRenewal, s.e, "clock")
		s = f.scenario(wiring{caseOuter: true})
		s.outer.failOn("ListPaymentMethods", 0)
		check(t, "listing fails", casePMUsedForRenewal, s.e, "injected ListPaymentMethods")
		s = f.scenario(wiring{caseOuter: true})
		s.outer.methods = []port.PaymentMethodView{{Brand: "visa"}}
		check(t, "no default", casePMUsedForRenewal, s.e, "default brand")
		s = f.scenario(wiring{caseOuter: true})
		s.outer.methods = []port.PaymentMethodView{{Brand: "visa"}, {Brand: "mastercard", IsDefault: true}}
		check(t, "renewed on the new card", casePMUsedForRenewal, s.e, "")
	})
	t.Run("no_card_data", func(t *testing.T) {
		s := f.scenario(wiring{})
		for _, tc := range []struct{ payload, want string }{
			{`{"pan":"4000056655665556"}`, "PAN-like"},
			{`{"cvc":"123"}`, "holds cvc"},
		} {
			s.sql(t, `INSERT INTO payment_event_ledger (provider, provider_event_id, event_type, normalised_kind, provider_created_at, payload, status, livemode)
				VALUES ('stripe', 'evt_pan_probe', 'test', 'ignored', now(), $1::jsonb, 'ignored', false)`, tc.payload)
			check(t, tc.payload, caseNoCardData, s.e, tc.want)
			s.sql(t, `DELETE FROM payment_event_ledger WHERE provider_event_id = 'evt_pan_probe'`)
		}
		// The visa test card of the signups is tolerated.
		s.sql(t, `INSERT INTO payment_event_ledger (provider, provider_event_id, event_type, normalised_kind, provider_created_at, payload, status, livemode)
			VALUES ('stripe', 'evt_pan_probe', 'test', 'ignored', now(), '{"card":"4242424242424242"}', 'ignored', false)`)
		check(t, "test card", caseNoCardData, s.e, "")
		s.sql(t, `DELETE FROM payment_event_ledger WHERE provider_event_id = 'evt_pan_probe'`)
		check(t, "clean", caseNoCardData, s.e, "")
	})
}

func TestRefundRetryAndChargeCases(t *testing.T) {
	f := newFixture(t)
	t.Run("refunds", func(t *testing.T) {
		// The first invoice is mirrored, then reopened before the refund.
		s := f.scenario(wiring{})
		s.ledger.onFinish = func(ctx context.Context) {
			if err := s.e.DB.Exec(ctx, `UPDATE invoices SET status = 'open' WHERE status = 'paid'`); err != nil {
				t.Error(err)
			}
		}
		check(t, "invoice open", caseRefundFull, s.e, "no paid invoice")
		s = f.scenario(wiring{})
		s.inner.failOn("Refund", 0)
		check(t, "refund fails", caseRefundPartial, s.e, "B-O3")
		s = f.scenario(wiring{deliverOuter: true})
		s.outer.failOn("ListSince", 0)
		check(t, "event not delivered", caseRefundEvent, s.e, "injected ListSince")
		s = f.scenario(wiring{})
		check(t, "refund pending", caseRefundEvent, s.e, "refund status after the refund event")
	})

	pastDue := func(s *scenario) {
		s.clock.onAdvance = func(ctx context.Context) {
			if err := s.e.DB.Exec(ctx, `UPDATE subscriptions SET status = 'past_due', past_due_since = now() WHERE status = 'active'`); err != nil {
				t.Error(err)
			}
		}
	}
	t.Run("retried", func(t *testing.T) {
		s := f.scenario(wiring{caseOuter: true})
		s.outer.failOn("AttachDefaultPaymentMethod", 0)
		check(t, "attach fails", caseRetried, s.e, "injected AttachDefaultPaymentMethod")
		s = f.scenario(wiring{})
		check(t, "still active", caseRetried, s.e, "status after the failed renewal: got active, want past_due")
		s = f.scenario(wiring{caseOuter: true})
		pastDue(s)
		s.outer.fetch = func(ref domain.ProviderObjectRef) (domain.ProviderObjectState, error, bool) {
			return domain.ProviderObjectState{Ref: ref, Status: "open"}, nil, ref.Kind == domain.ObjectInvoice
		}
		check(t, "no retry", caseRetried, s.e, "scheduled no retry")
		s = f.scenario(wiring{caseOuter: true})
		pastDue(s)
		s.outer.fetch = func(ref domain.ProviderObjectRef) (domain.ProviderObjectState, error, bool) {
			return domain.ProviderObjectState{}, errInjected, ref.Kind == domain.ObjectInvoice
		}
		check(t, "invoice read fails", caseRetried, s.e, "injected")
		s = f.scenario(wiring{caseOuter: true})
		pastDue(s)
		s.outer.fetch = func(ref domain.ProviderObjectRef) (domain.ProviderObjectState, error, bool) {
			return domain.ProviderObjectState{Ref: ref, Status: "open", Fields: map[string]any{"next_payment_attempt": time.Now().Add(72 * time.Hour)}}, nil, true
		}
		check(t, "retry scheduled", caseRetried, s.e, "")
		s = f.scenario(wiring{})
		s.clock.failAfter = 1
		check(t, "renewal fails", caseRetried, s.e, "clock")
	})
	t.Run("exhausted", func(t *testing.T) {
		s := f.scenario(wiring{caseOuter: true})
		s.outer.failOn("AttachDefaultPaymentMethod", 0)
		check(t, "attach fails", caseRetriesExhausted, s.e, "injected")
		s = f.scenario(wiring{})
		check(t, "never past due", caseRetriesExhausted, s.e, "past_due first")
		s = f.scenario(wiring{})
		pastDue(s)
		s.clock.failAfter = 2
		check(t, "clock fails", caseRetriesExhausted, s.e, "clock")
		s = f.scenario(wiring{deliverOuter: true})
		pastDue(s)
		s.outer.failOn("ListSince", 1)
		check(t, "delivery fails", caseRetriesExhausted, s.e, "injected ListSince")
		s = f.scenario(wiring{})
		pastDue(s)
		check(t, "not cancelled", caseRetriesExhausted, s.e, "status after retries are exhausted")
	})
	t.Run("later_success", func(t *testing.T) {
		s := f.scenario(wiring{caseOuter: true})
		s.outer.failOn("AttachDefaultPaymentMethod", 0)
		check(t, "attach fails", caseLaterSuccess, s.e, "injected")
		s = f.scenario(wiring{caseOuter: true})
		s.outer.failOn("AttachDefaultPaymentMethod", 1)
		check(t, "fix refused", caseLaterSuccess, s.e, "injected AttachDefaultPaymentMethod")
		s = f.scenario(wiring{})
		s.clock.failAfter = 2
		check(t, "clock fails", caseLaterSuccess, s.e, "clock")
		s = f.scenario(wiring{deliverOuter: true})
		s.outer.failOn("ListSince", 1)
		check(t, "delivery fails", caseLaterSuccess, s.e, "injected ListSince")
		s = f.scenario(wiring{})
		pastDue(s)
		check(t, "still past due", caseLaterSuccess, s.e, "status after a later success: got past_due")
	})
	t.Run("one_off", func(t *testing.T) {
		s := f.scenario(wiring{svcOuter: true})
		s.outer.failOn("ChargeOneOff", 0)
		check(t, "off-session fails", caseOffSession, s.e, "B-P20")
		s = f.scenario(wiring{caseOuter: true})
		s.outer.failOn("AttachDefaultPaymentMethod", 0)
		check(t, "3DS attach fails", caseThreeDS, s.e, "B-P20: injected AttachDefaultPaymentMethod")
		s = f.scenario(wiring{})
		check(t, "3DS paid", caseThreeDS, s.e, "add-on status")
		s = f.scenario(wiring{svcOuter: true})
		s.outer.failOn("ChargeOneOff", 0)
		s.outer.fail["ChargeOneOff"] = failRule{err: port.ErrActionRequired{ClientSecret: "pi_secret_1"}}
		ev := check(t, "3DS action", caseThreeDS, s.e, "")
		if ev["client_action"] != true {
			t.Errorf("evidence %v", ev)
		}
		s = f.scenario(wiring{svcOuter: true})
		s.outer.fail["ChargeOneOff"] = failRule{err: port.ErrActionRequired{}}
		check(t, "3DS without secret", caseThreeDS, s.e, "no 3-D Secure action")
		s = f.scenario(wiring{})
		check(t, "decline paid", caseDecline, s.e, "want 402 PAYMENT_DECLINED")
		s = f.scenario(wiring{})
		s.inner.fail["ChargeOneOff"] = failRule{err: port.ErrDeclined{Code: "card_declined"}}
		check(t, "declined", caseDecline, s.e, "")
		// stripe-mock: the 3-D Secure and decline branches need the provider.
		check(t, "rehearsal 3DS", caseThreeDS, f.scenario(wiring{target: TargetStripeMock}).e, "")
	})
	t.Run("proration", func(t *testing.T) {
		s := f.scenario(wiring{})
		s.clock.failAfter = 1
		check(t, "clock fails", caseProrationNative, s.e, "clock")
		s = f.scenario(wiring{svcOuter: true})
		s.outer.failOn("PreviewQuantityChange", 0)
		check(t, "preview fails", caseProrationNative, s.e, "B-P6")
		s = f.scenario(wiring{svcOuter: true})
		s.outer.failOn("ChangeQuantity", 0)
		check(t, "charge fails", caseProrationNative, s.e, "B-P7")
		s = f.scenario(wiring{svcOuter: true})
		caps := s.ms.Capabilities()
		caps.Proration = port.ProrationFallback
		s.outer.caps = &caps
		check(t, "calculator", caseProrationNative, s.e, "amount source")
		s = f.scenario(wiring{caseOuter: true})
		s.outer.failOn("GetInvoice", 0)
		check(t, "invoice read fails", caseProrationNative, s.e, "injected GetInvoice")
		s = f.scenario(wiring{})
		check(t, "charged as previewed", caseProrationNative, s.e, "")
	})
	t.Run("collection_pause", func(t *testing.T) {
		s := f.scenario(wiring{caseOuter: true})
		s.outer.failOn("PauseCollection", 0)
		check(t, "pause refused", caseCollectionPause, s.e, "injected PauseCollection")
	})
}

var errInjected = errorString("injected invoice read failure")

type errorString string

func (e errorString) Error() string { return string(e) }
