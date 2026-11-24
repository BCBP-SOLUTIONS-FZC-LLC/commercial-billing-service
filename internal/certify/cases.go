package certify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// Cases returns the cert-1.0 suite for a provider declaring proration mode
// prorationMode: every required case of domain.CertSuite, the proration
// case of that mode, and the optional (recorded) groups.
func Cases(prorationMode port.ProrationMode) []Case {
	impl := map[string]func(context.Context, *Env) (Evidence, error){
		"recurring_subscriptions/create":                  caseCreate,
		"recurring_subscriptions/increase_quantity":       caseIncreaseQuantity,
		"recurring_subscriptions/change_plan_now":         caseChangePlanNow,
		"recurring_subscriptions/change_plan_period_end":  caseChangePlanPeriodEnd,
		"recurring_subscriptions/schedule_lower_quantity": caseScheduleLowerQuantity,
		"recurring_subscriptions/renew":                   caseRenew,
		"recurring_subscriptions/cancel_at_period_end":    caseCancelAtPeriodEnd,
		"recurring_subscriptions/cancel_now":              caseCancelNow,
		"webhook_delivery/signature_verified":             caseSignatureVerified,
		"webhook_delivery/tolerance_enforced":             caseToleranceEnforced,
		"webhook_delivery/redelivered":                    caseRedelivered,
		"webhook_delivery/out_of_order_converges":         caseOutOfOrder,
		"stable_event_ids/same_id_on_redelivery":          caseSameIDOnRedelivery,
		"stable_event_ids/ledger_dedupe":                  caseLedgerDedupe,
		"invoice_retrieval/by_reference":                  caseInvoiceByReference,
		"invoice_retrieval/pdf_link":                      caseInvoicePDF,
		"invoice_retrieval/listing_paginates":             caseInvoiceListing,
		"payment_method_update/hosted_page":               caseHostedPMPage,
		"payment_method_update/used_for_renewal":          casePMUsedForRenewal,
		"payment_method_update/no_card_data":              caseNoCardData,
		"refunds/full":                                    caseRefundFull,
		"refunds/partial":                                 caseRefundPartial,
		"refunds/event_recorded":                          caseRefundEvent,
		"retry_semantics/retried":                         caseRetried,
		"retry_semantics/exhausted_past_due_cancelled":    caseRetriesExhausted,
		"retry_semantics/later_success_active":            caseLaterSuccess,
		"one_off_charges/off_session":                     caseOffSession,
		"one_off_charges/three_d_secure":                  caseThreeDS,
		"one_off_charges/decline":                         caseDecline,
		"proration/preview_equals_charge":                 caseProrationNative,
		"proration/calculator_charged":                    caseProrationFallback,
		"collection_pause/pause_and_resume":               caseCollectionPause,
	}
	var out []Case
	suite := domain.CertSuite[SuiteVersion]
	for _, group := range []string{"recurring_subscriptions", "webhook_delivery", "stable_event_ids", "invoice_retrieval",
		"payment_method_update", "refunds", "retry_semantics", "one_off_charges"} {
		for _, id := range suite[group] {
			out = append(out, Case{Capability: group, ID: id, Required: true, Run: orMissing(impl[group+"/"+id])})
		}
	}
	pc := "preview_equals_charge"
	if prorationMode == port.ProrationFallback {
		pc = "calculator_charged"
	}
	out = append(out, Case{Capability: "proration", ID: pc, Required: true, Run: impl["proration/"+pc]},
		Case{Capability: "collection_pause", ID: "pause_and_resume", Run: impl["collection_pause/pause_and_resume"]})
	return out
}

func orMissing(f func(context.Context, *Env) (Evidence, error)) func(context.Context, *Env) (Evidence, error) {
	if f != nil {
		return f
	}
	return func(context.Context, *Env) (Evidence, error) {
		return nil, errors.New("case not implemented in this binary")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────

var bffCaller = app.Caller{Principal: "cluster.local/ns/bff/sa/bff-signup"}

func owner(tenant string) app.Caller {
	return app.Caller{UserID: "00000000-0000-4000-8000-00000000c3f7", TenantID: tenant, Roles: []string{domain.RoleTenantOwner},
		IdempotencyKey: "certify-" + uuid.NewString()}
}

var operator = app.Caller{UserID: "00000000-0000-4000-8000-00000000c3f8", Roles: []string{domain.RolePlatformOperator}}

func opKey() app.Caller { c := operator; c.IdempotencyKey = "certify-" + uuid.NewString(); return c }

type subscriber struct{ Tenant, Sub, ProviderSub, Customer string }

// subscribe signs a new tenant up through B-I1 (off-session charge with the
// visa test card) and makes it an active, converted tenant.
func subscribe(ctx context.Context, e *Env, plan domain.PlanCode, seats int) (subscriber, error) {
	tenant := uuid.NewString()
	e.Core.SetUsage(1, seats)
	res, err := e.Svc.AuthorizeAndCharge(ctx, bffCaller, app.AuthorizeChargeRequest{TenantID: tenant, PlanCode: string(plan),
		LicensedSeats: seats, PaymentMethodRef: "pm_card_visa", Country: "AE"})
	if err != nil {
		return subscriber{}, fmt.Errorf("B-I1: %w", err)
	}
	s := subscriber{Tenant: tenant, Sub: res.BillingSubscriptionID}
	if err := e.DB.Exec(ctx, `UPDATE billing_accounts SET iam_lifecycle = 'active' WHERE tenant_id = $1`, tenant); err != nil {
		return s, err
	}
	s.ProviderSub, _ = Scalar[string](ctx, e.DB, `SELECT coalesce(provider_subscription_id,'') FROM subscriptions WHERE billing_subscription_id = $1`, s.Sub)
	s.Customer, _ = Scalar[string](ctx, e.DB, `SELECT coalesce(provider_customer_id,'') FROM billing_accounts WHERE tenant_id = $1`, tenant)
	if res.Status != string(domain.StatusActive) {
		return s, fmt.Errorf("B-I1 status %s, want active", res.Status)
	}
	return s, nil
}

func version(ctx context.Context, e *Env, sub string) int64 {
	v, _ := Scalar[int64](ctx, e.DB, `SELECT record_version FROM subscriptions WHERE billing_subscription_id = $1`, sub)
	return v
}

func q[T any](ctx context.Context, e *Env, sql string, args ...any) T {
	v, _ := Scalar[T](ctx, e.DB, sql, args...)
	return v
}

// expect fails unless got == want (test mode: provider behaviour asserted).
func expect[T comparable](what string, got, want T) error {
	if got != want {
		return fmt.Errorf("%s: got %v, want %v", what, got, want)
	}
	return nil
}

// stateful runs f only when the target keeps provider state.
func stateful(e *Env, f func() error) error {
	if !e.Stateful() {
		return nil
	}
	return f()
}

// renewal advances the clock past the subscription's period end and
// delivers the provider's events.
func renewal(ctx context.Context, e *Env, sub string) (time.Time, error) {
	end := q[time.Time](ctx, e, `SELECT current_period_end FROM subscriptions WHERE billing_subscription_id = $1`, sub)
	if end.IsZero() {
		return end, errors.New("no current period")
	}
	if err := e.Clock.Advance(ctx, end.Add(2*time.Hour)); err != nil {
		return end, err
	}
	_, err := e.Deliver.DeliverNew(ctx)
	return end, err
}

// ── recurring_subscriptions ──────────────────────────────────────────────

func caseCreate(ctx context.Context, e *Env) (Evidence, error) {
	s, err := subscribe(ctx, e, domain.PlanPro, 3)
	ev := Evidence{"tenant_id": s.Tenant, "billing_subscription_id": s.Sub, "provider_subscription": s.ProviderSub, "customer": s.Customer}
	if err != nil {
		return ev, err
	}
	return ev, stateful(e, func() error {
		cur, err := e.Provider.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: domain.ObjectSubscription, ID: s.ProviderSub})
		if err != nil {
			return err
		}
		ev["provider_status"] = cur.Status
		return expect("provider subscription status", cur.Status, "active")
	})
}

func caseIncreaseQuantity(ctx context.Context, e *Env) (Evidence, error) {
	s, err := subscribe(ctx, e, domain.PlanPro, 3)
	if err != nil {
		return nil, err
	}
	pv, err := e.Svc.PreviewSeats(ctx, owner(s.Tenant), s.Tenant, app.SeatPreviewRequest{AddSeats: 2})
	if err != nil {
		return nil, fmt.Errorf("B-P6: %w", err)
	}
	p, err := e.Svc.PurchaseSeats(ctx, owner(s.Tenant), s.Tenant, app.SeatPurchaseRequest{AddSeats: 2, PreviewID: pv.PreviewID})
	ev := Evidence{"purchase_id": p.PurchaseID, "status": p.Status, "previewed_minor": pv.AmountDueNow.AmountMinor}
	if err != nil {
		return ev, fmt.Errorf("B-P7: %w", err)
	}
	// Paid before applied: the quantity changes only on confirmation (BIL-SUB-5).
	qty := q[int](ctx, e, `SELECT quantity FROM subscription_items WHERE billing_subscription_id = $1`, s.Sub)
	ev["quantity"] = qty
	if err := expect("purchase status", p.Status, string(domain.PurchaseConfirmed)); err != nil {
		return ev, err
	}
	return ev, expect("seat item quantity", qty, 5)
}

func upgradeOf(ctx context.Context, e *Env) (subscriber, app.PlanChangeResult, app.PlanChangePreview, error) {
	s, err := subscribe(ctx, e, domain.PlanPro, 3)
	if err != nil {
		return s, app.PlanChangeResult{}, app.PlanChangePreview{}, err
	}
	pv, err := e.Svc.PreviewPlanChange(ctx, owner(s.Tenant), s.Tenant, app.PlanChangePreviewRequest{TargetPlan: string(domain.PlanEnterprise)})
	if err != nil {
		return s, app.PlanChangeResult{}, pv, fmt.Errorf("B-P3: %w", err)
	}
	res, err := e.Svc.ChangePlan(ctx, owner(s.Tenant), s.Tenant, app.PlanChangeRequest{TargetPlan: string(domain.PlanEnterprise),
		RecordVersion: version(ctx, e, s.Sub), PreviewID: pv.PreviewID})
	if err != nil {
		return s, res, pv, fmt.Errorf("B-P4: %w", err)
	}
	return s, res, pv, nil
}

func caseChangePlanNow(ctx context.Context, e *Env) (Evidence, error) {
	s, res, _, err := upgradeOf(ctx, e)
	ev := Evidence{"tenant_id": s.Tenant, "change_id": res.ChangeID, "status": res.Status}
	if err != nil {
		return ev, err
	}
	plan := q[string](ctx, e, `SELECT plan::text FROM subscriptions WHERE billing_subscription_id = $1`, s.Sub)
	ev["plan"] = plan
	return ev, expect("plan after a paid upgrade", plan, string(domain.PlanEnterprise))
}

func caseChangePlanPeriodEnd(ctx context.Context, e *Env) (Evidence, error) {
	s, err := subscribe(ctx, e, domain.PlanEnterprise, 3)
	if err != nil {
		return nil, err
	}
	e.Core.Check = port.PlanChangeCheck{Allowed: true}
	res, err := e.Svc.ChangePlan(ctx, owner(s.Tenant), s.Tenant, app.PlanChangeRequest{TargetPlan: string(domain.PlanPro), RecordVersion: version(ctx, e, s.Sub)})
	ev := Evidence{"tenant_id": s.Tenant, "change_id": res.ChangeID, "status": res.Status}
	if err != nil {
		return ev, fmt.Errorf("B-P4 downgrade: %w", err)
	}
	sched := q[string](ctx, e, `SELECT coalesce(provider_schedule_id,'') FROM plan_change_requests WHERE change_id = $1`, res.ChangeID)
	ev["provider_schedule"] = sched
	if err := expect("downgrade status", res.Status, string(domain.ChangeScheduled)); err != nil {
		return ev, err
	}
	if sched == "" {
		return ev, errors.New("no provider schedule recorded")
	}
	return ev, stateful(e, func() error {
		if _, err := renewal(ctx, e, s.Sub); err != nil {
			return err
		}
		return expect("plan after the renewal", q[string](ctx, e, `SELECT plan::text FROM subscriptions WHERE billing_subscription_id = $1`, s.Sub), string(domain.PlanPro))
	})
}

func caseScheduleLowerQuantity(ctx context.Context, e *Env) (Evidence, error) {
	s, err := subscribe(ctx, e, domain.PlanPro, 10)
	if err != nil {
		return nil, err
	}
	e.Core.SetUsage(4, 10)
	if _, err := e.Svc.ScheduleReduction(ctx, owner(s.Tenant), s.Tenant, app.ReductionRequest{TargetSeats: 6}); err != nil {
		return nil, fmt.Errorf("B-P10: %w", err)
	}
	end := q[time.Time](ctx, e, `SELECT current_period_end FROM subscriptions WHERE billing_subscription_id = $1`, s.Sub)
	if err := e.Clock.Advance(ctx, end.Add(-3*time.Hour)); err != nil {
		return nil, err
	}
	if _, err := e.Svc.RunSeatRenewal(ctx); err != nil {
		return nil, fmt.Errorf("seat-renewal: %w", err)
	}
	ev := Evidence{"tenant_id": s.Tenant, "schedule": q[string](ctx, e, `SELECT status||'/'||coalesce(renewal_seats::text,'')||'/'||coalesce(provider_schedule_id,'')
		FROM seat_change_schedules WHERE billing_subscription_id = $1`, s.Sub)}
	if !strings.HasPrefix(ev["schedule"].(string), "applying/6/") {
		return ev, fmt.Errorf("schedule %v, want applying with 6 renewal seats and a provider schedule", ev["schedule"])
	}
	return ev, stateful(e, func() error {
		if _, err := renewal(ctx, e, s.Sub); err != nil {
			return err
		}
		if _, err := e.Svc.RunSeatRenewal(ctx); err != nil {
			return err
		}
		return expect("quantity after the renewal", q[int](ctx, e, `SELECT quantity FROM subscription_items WHERE billing_subscription_id = $1`, s.Sub), 6)
	})
}

func caseRenew(ctx context.Context, e *Env) (Evidence, error) {
	s, err := subscribe(ctx, e, domain.PlanPro, 2)
	if err != nil {
		return nil, err
	}
	ev := Evidence{"tenant_id": s.Tenant}
	if !e.Stateful() {
		// stripe-mock cannot renew: the cycle handler is driven with the
		// mock's invoice for the mapping only.
		_, err := e.Provider.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: "in_certify"})
		return ev, err
	}
	end, err := renewal(ctx, e, s.Sub)
	if err != nil {
		return ev, err
	}
	start := q[time.Time](ctx, e, `SELECT current_period_start FROM subscriptions WHERE billing_subscription_id = $1`, s.Sub)
	ev["period_start"], ev["invoices_paid"] = start, q[int](ctx, e, `SELECT count(*) FROM invoices WHERE tenant_id = $1 AND status = 'paid'`, s.Tenant)
	if start.Before(end.Add(-time.Minute)) {
		return ev, fmt.Errorf("period not rolled: start %s, previous end %s", start, end)
	}
	return ev, expect("status after renewal", q[string](ctx, e, `SELECT status::text FROM subscriptions WHERE billing_subscription_id = $1`, s.Sub), "active")
}

func caseCancelAtPeriodEnd(ctx context.Context, e *Env) (Evidence, error) {
	s, err := subscribe(ctx, e, domain.PlanPro, 2)
	if err != nil {
		return nil, err
	}
	v, err := e.Svc.CancelSubscription(ctx, owner(s.Tenant), s.Tenant, app.CancelRequest{RecordVersion: version(ctx, e, s.Sub), Reason: "too_expensive"})
	if err != nil {
		return nil, fmt.Errorf("B-P12: %w", err)
	}
	renew := q[bool](ctx, e, `SELECT auto_renew FROM subscriptions WHERE billing_subscription_id = $1`, s.Sub)
	ev := Evidence{"tenant_id": s.Tenant, "status": v.Status, "auto_renew": renew}
	if renew {
		return ev, errors.New("auto_renew still true")
	}
	return ev, stateful(e, func() error {
		if _, err := renewal(ctx, e, s.Sub); err != nil {
			return err
		}
		return expect("status after the period end", q[string](ctx, e, `SELECT status::text FROM subscriptions WHERE billing_subscription_id = $1`, s.Sub), "cancelled")
	})
}

func caseCancelNow(ctx context.Context, e *Env) (Evidence, error) {
	s, err := subscribe(ctx, e, domain.PlanPro, 2)
	if err != nil {
		return nil, err
	}
	err = e.Provider.CancelNow(ctx, port.SubscriptionRef{ProviderSubscriptionID: s.ProviderSub}, "cancel:"+s.Sub+":certify")
	ev := Evidence{"tenant_id": s.Tenant, "provider_subscription": s.ProviderSub}
	if err != nil {
		return ev, err
	}
	return ev, stateful(e, func() error {
		if _, err := e.Deliver.DeliverNew(ctx); err != nil {
			return err
		}
		return expect("status after cancel now", q[string](ctx, e, `SELECT status::text FROM subscriptions WHERE billing_subscription_id = $1`, s.Sub), "cancelled")
	})
}

// ── webhook_delivery and stable_event_ids ────────────────────────────────

// anEvent returns one raw provider event of this run.
func anEvent(ctx context.Context, e *Env) (string, []byte, error) {
	page, err := e.Provider.ListSince(ctx, port.ReconEvents, e.Deliver.Since, "")
	if err != nil {
		return "", nil, err
	}
	if len(page.Items) == 0 {
		return "", nil, errors.New("the provider listed no event for this run")
	}
	id := page.Items[0].Ref.ID
	raw, err := e.Deliver.API.RawEvent(ctx, id)
	return id, raw, err
}

func caseSignatureVerified(ctx context.Context, e *Env) (Evidence, error) {
	id, raw, err := anEvent(ctx, e)
	if err != nil {
		return nil, err
	}
	bad := e.Deliver.Post(ctx, raw, time.Now(), Sign("whsec_wrong", time.Now(), raw))
	good := e.Deliver.Post(ctx, raw, time.Now(), "")
	ev := Evidence{"event_id": id, "bad_signature_status": bad, "good_signature_status": good}
	if err := expect("bad signature", bad, http.StatusBadRequest); err != nil {
		return ev, err
	}
	return ev, expect("good signature", good, http.StatusOK)
}

func caseToleranceEnforced(ctx context.Context, e *Env) (Evidence, error) {
	id, raw, err := anEvent(ctx, e)
	if err != nil {
		return nil, err
	}
	code := e.Deliver.Post(ctx, raw, time.Now().Add(-10*time.Minute), "")
	ev := Evidence{"event_id": id, "stale_status": code}
	return ev, expect("stale timestamp", code, http.StatusBadRequest)
}

func ledgerCount(ctx context.Context, e *Env, id string) int {
	return q[int](ctx, e, `SELECT count(*) FROM payment_event_ledger WHERE provider_event_id = $1`, id)
}

func caseRedelivered(ctx context.Context, e *Env) (Evidence, error) {
	// B-W1 refuses with 503 while the ledger is unavailable (the
	// provider retries); the redelivery is ledgered once.
	id, raw, err := anEvent(ctx, e)
	if err != nil {
		return nil, err
	}
	if err := e.DB.Exec(ctx, `DELETE FROM payment_event_ledger WHERE provider_event_id = $1`, id); err != nil {
		return nil, err
	}
	if err := e.DB.Exec(ctx, `REVOKE INSERT ON payment_event_ledger FROM billing_system`); err != nil {
		return nil, err
	}
	refused := e.Deliver.Post(ctx, raw, time.Now(), "")
	if err := e.DB.Exec(ctx, `GRANT INSERT ON payment_event_ledger TO billing_system`); err != nil {
		return nil, err
	}
	again := e.Deliver.Post(ctx, raw, time.Now(), "")
	ev := Evidence{"event_id": id, "first_status": refused, "redelivery_status": again, "ledger_rows": ledgerCount(ctx, e, id)}
	if err := expect("delivery while the ledger is down", refused, http.StatusServiceUnavailable); err != nil {
		return ev, err
	}
	if err := expect("redelivery", again, http.StatusOK); err != nil {
		return ev, err
	}
	return ev, expect("ledger rows", ev["ledger_rows"].(int), 1)
}

func caseOutOfOrder(ctx context.Context, e *Env) (Evidence, error) {
	// Two events for one subscription delivered newest first converge on
	// the provider's current state (BIL-PAY-4: re-read, don't trust).
	s, err := subscribe(ctx, e, domain.PlanPro, 2)
	if err != nil {
		return nil, err
	}
	if _, err := e.Deliver.DeliverNew(ctx); err != nil { // older events of the signup
		return nil, err
	}
	if err := e.Provider.SetCancelAtPeriodEnd(ctx, port.SubscriptionRef{ProviderSubscriptionID: s.ProviderSub}, true, "cape:"+s.Sub+":1"); err != nil {
		return nil, err
	}
	if err := e.Provider.SetCancelAtPeriodEnd(ctx, port.SubscriptionRef{ProviderSubscriptionID: s.ProviderSub}, false, "cape:"+s.Sub+":2"); err != nil {
		return nil, err
	}
	ev := Evidence{"tenant_id": s.Tenant}
	return ev, stateful(e, func() error {
		page, err := e.Provider.ListSince(ctx, port.ReconEvents, e.Deliver.Since, "")
		if err != nil {
			return err
		}
		var ids []string
		for _, it := range page.Items {
			if !e.Deliver.delivered[it.Ref.ID] {
				ids = append(ids, it.Ref.ID) // newest first, as listed
			}
		}
		for _, id := range ids {
			raw, err := e.Deliver.API.RawEvent(ctx, id)
			if err != nil {
				return err
			}
			e.Deliver.Post(ctx, raw, time.Now(), "")
			e.Deliver.delivered[id] = true
		}
		if err := e.Deliver.Drain(ctx); err != nil {
			return err
		}
		ev["delivered_newest_first"] = ids
		return expect("auto_renew after reordering", q[bool](ctx, e, `SELECT auto_renew FROM subscriptions WHERE billing_subscription_id = $1`, s.Sub), true)
	})
}

func caseSameIDOnRedelivery(ctx context.Context, e *Env) (Evidence, error) {
	id, first, err := anEvent(ctx, e)
	if err != nil {
		return nil, err
	}
	second, err := e.Deliver.API.RawEvent(ctx, id)
	if err != nil {
		return nil, err
	}
	var a, b struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(first, &a)
	_ = json.Unmarshal(second, &b)
	return Evidence{"event_id": id}, expect("event id on redelivery", b.ID, a.ID)
}

func caseLedgerDedupe(ctx context.Context, e *Env) (Evidence, error) {
	id, raw, err := anEvent(ctx, e)
	if err != nil {
		return nil, err
	}
	e.Deliver.Post(ctx, raw, time.Now(), "")
	e.Deliver.Post(ctx, raw, time.Now(), "")
	n := ledgerCount(ctx, e, id)
	return Evidence{"event_id": id, "ledger_rows": n}, expect("ledger rows after two deliveries", n, 1)
}

// ── invoices ─────────────────────────────────────────────────────────────

func firstInvoice(ctx context.Context, e *Env) (subscriber, string, error) {
	s, err := subscribe(ctx, e, domain.PlanPro, 2)
	if err != nil {
		return s, "", err
	}
	if m, ok := e.Provider.(*mockState); ok {
		if err := m.paidCallback(ctx, e, s); err != nil {
			return s, "", err
		}
	} else if _, err := e.Deliver.DeliverNew(ctx); err != nil {
		return s, "", err
	}
	inv := q[string](ctx, e, `SELECT coalesce(max(provider_invoice_id),'') FROM invoices WHERE tenant_id = $1`, s.Tenant)
	if inv == "" {
		return s, "", errors.New("no invoice mirrored for the signup")
	}
	return s, inv, nil
}

func caseInvoiceByReference(ctx context.Context, e *Env) (Evidence, error) {
	_, inv, err := firstInvoice(ctx, e)
	if err != nil {
		return nil, err
	}
	v, err := e.Provider.GetInvoice(ctx, port.InvoiceRef{ProviderInvoiceID: inv})
	ev := Evidence{"invoice": inv, "status": v.Status, "amount_due_minor": v.AmountDue.AmountMinor}
	if err != nil {
		return ev, err
	}
	return ev, stateful(e, func() error { return expect("invoice status", v.Status, "paid") })
}

func caseInvoicePDF(ctx context.Context, e *Env) (Evidence, error) {
	_, inv, err := firstInvoice(ctx, e)
	if err != nil {
		return nil, err
	}
	link, err := e.Provider.InvoicePDFLink(ctx, port.InvoiceRef{ProviderInvoiceID: inv})
	ev := Evidence{"invoice": inv, "has_link": link.URL != ""}
	if err != nil {
		return ev, err
	}
	if !strings.HasPrefix(link.URL, "https://") {
		return ev, errors.New("no https PDF link")
	}
	return ev, nil
}

func caseInvoiceListing(ctx context.Context, e *Env) (Evidence, error) {
	pages, items, cursor := 0, 0, ""
	for range 20 {
		page, err := e.Provider.ListSince(ctx, port.ReconInvoices, e.Deliver.Since, cursor)
		if err != nil {
			return nil, err
		}
		pages++
		items += len(page.Items)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	ev := Evidence{"pages": pages, "items": items}
	if items == 0 {
		return ev, errors.New("the listing returned no invoice")
	}
	return ev, nil
}

// ── payment_method_update ────────────────────────────────────────────────

func caseHostedPMPage(ctx context.Context, e *Env) (Evidence, error) {
	s, err := subscribe(ctx, e, domain.PlanPro, 2)
	if err != nil {
		return nil, err
	}
	h, err := e.Svc.CreateSetupSession(ctx, owner(s.Tenant), s.Tenant)
	ev := Evidence{"tenant_id": s.Tenant, "https": strings.HasPrefix(h.URL, "https://"), "expires_at": h.ExpiresAt}
	if err != nil {
		return ev, fmt.Errorf("B-P16: %w", err)
	}
	if !strings.HasPrefix(h.URL, "https://") {
		return ev, errors.New("setup page is not a provider-hosted https URL (BIL-API-5)")
	}
	return ev, nil
}

func casePMUsedForRenewal(ctx context.Context, e *Env) (Evidence, error) {
	s, err := subscribe(ctx, e, domain.PlanPro, 2)
	if err != nil {
		return nil, err
	}
	err = e.Provider.AttachDefaultPaymentMethod(ctx, port.CustomerRef{ProviderCustomerID: s.Customer}, "pm_card_mastercard", "pmdefault:"+s.Sub)
	ev := Evidence{"tenant_id": s.Tenant}
	if err != nil {
		return ev, err
	}
	return ev, stateful(e, func() error {
		if _, err := renewal(ctx, e, s.Sub); err != nil {
			return err
		}
		methods, err := e.Provider.ListPaymentMethods(ctx, port.CustomerRef{ProviderCustomerID: s.Customer})
		if err != nil {
			return err
		}
		for _, m := range methods {
			if m.IsDefault {
				ev["default_brand"] = m.Brand
			}
		}
		if err := expect("default brand", ev["default_brand"], any("mastercard")); err != nil {
			return err
		}
		return expect("status after renewal", q[string](ctx, e, `SELECT status::text FROM subscriptions WHERE billing_subscription_id = $1`, s.Sub), "active")
	})
}

// cardPattern is a PAN-like run of 13–19 digits.
func looksLikePAN(s string) bool {
	run := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			run++
			if run >= 13 {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

func caseNoCardData(ctx context.Context, e *Env) (Evidence, error) {
	// BIL-PCI-2/3: the ledger payloads and every text column hold no PAN.
	payloads := q[string](ctx, e, `SELECT coalesce(string_agg(payload::text, ' '), '') FROM payment_event_ledger`)
	ev := Evidence{"ledger_bytes_scanned": len(payloads)}
	if looksLikePAN(strings.NewReplacer("1795", "", "4242424242424242", "PAN").Replace(payloads)) || strings.Contains(payloads, "PAN") {
		return ev, errors.New("a PAN-like value is stored in payment_event_ledger")
	}
	for _, field := range []string{"client_secret", "fingerprint", "\"number\"", "cvc"} {
		if strings.Contains(payloads, field) {
			return ev, fmt.Errorf("ledger payload holds %s (BIL-PCI-3)", field)
		}
	}
	return ev, nil
}

// ── refunds ──────────────────────────────────────────────────────────────

func paidInvoice(ctx context.Context, e *Env) (subscriber, string, int64, error) {
	s, _, err := firstInvoice(ctx, e)
	if err != nil {
		return s, "", 0, err
	}
	id := q[string](ctx, e, `SELECT coalesce(max(invoice_id::text),'') FROM invoices WHERE tenant_id = $1 AND status = 'paid'`, s.Tenant)
	paid := q[int64](ctx, e, `SELECT coalesce(max(amount_paid_minor),0) FROM invoices WHERE tenant_id = $1 AND status = 'paid'`, s.Tenant)
	if id == "" {
		return s, "", 0, errors.New("no paid invoice mirrored")
	}
	return s, id, paid, nil
}

func refund(ctx context.Context, e *Env, amount func(paid int64) int64) (Evidence, error) {
	s, inv, paid, err := paidInvoice(ctx, e)
	if err != nil {
		return nil, err
	}
	r, err := e.Svc.IssueRefund(ctx, opKey(), s.Tenant, app.RefundRequest{InvoiceID: inv, Amount: app.Money{AmountMinor: amount(paid), Currency: "USD"},
		Category: "goodwill", Reason: "certification suite refund case"})
	ev := Evidence{"tenant_id": s.Tenant, "invoice_id": inv, "refund_id": r.RefundID, "amount_minor": amount(paid)}
	if err != nil {
		return ev, fmt.Errorf("B-O3: %w", err)
	}
	return ev, nil
}

func caseRefundFull(ctx context.Context, e *Env) (Evidence, error) {
	return refund(ctx, e, func(p int64) int64 { return p })
}

func caseRefundPartial(ctx context.Context, e *Env) (Evidence, error) {
	return refund(ctx, e, func(p int64) int64 { return max(p/3, 1) })
}

func caseRefundEvent(ctx context.Context, e *Env) (Evidence, error) {
	ev, err := refund(ctx, e, func(p int64) int64 { return max(p/4, 1) })
	if err != nil || !e.Stateful() {
		return ev, err
	}
	if _, err := e.Deliver.DeliverNew(ctx); err != nil {
		return ev, err
	}
	st := q[string](ctx, e, `SELECT status FROM refunds WHERE refund_id = $1`, ev["refund_id"])
	ev["refund_status"] = st
	return ev, expect("refund status after the refund event", st, "succeeded")
}

// ── retry_semantics ──────────────────────────────────────────────────────

// failingRenewal subscribes with a good card, then switches the default to
// a card that fails every charge, and renews.
func failingRenewal(ctx context.Context, e *Env) (subscriber, error) {
	s, err := subscribe(ctx, e, domain.PlanPro, 2)
	if err != nil {
		return s, err
	}
	if err := e.Provider.AttachDefaultPaymentMethod(ctx, port.CustomerRef{ProviderCustomerID: s.Customer}, "pm_card_chargeCustomerFail", "pmfail:"+s.Sub); err != nil {
		return s, err
	}
	if !e.Stateful() {
		return s, nil
	}
	_, err = renewal(ctx, e, s.Sub)
	return s, err
}

func status(ctx context.Context, e *Env, sub string) string {
	return q[string](ctx, e, `SELECT status::text FROM subscriptions WHERE billing_subscription_id = $1`, sub)
}

func caseRetried(ctx context.Context, e *Env) (Evidence, error) {
	s, err := failingRenewal(ctx, e)
	ev := Evidence{"tenant_id": s.Tenant}
	if err != nil {
		return ev, err
	}
	return ev, stateful(e, func() error {
		if err := expect("status after the failed renewal", status(ctx, e, s.Sub), "past_due"); err != nil {
			return err
		}
		inv := q[string](ctx, e, `SELECT coalesce(max(provider_invoice_id),'') FROM invoices WHERE tenant_id = $1 AND status = 'open'`, s.Tenant)
		cur, err := e.Provider.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: inv})
		if err != nil {
			return err
		}
		next, ok := cur.Fields["next_payment_attempt"].(time.Time)
		ev["next_payment_attempt"] = next
		if !ok || next.IsZero() {
			return errors.New("the provider scheduled no retry")
		}
		return nil
	})
}

func caseRetriesExhausted(ctx context.Context, e *Env) (Evidence, error) {
	s, err := failingRenewal(ctx, e)
	ev := Evidence{"tenant_id": s.Tenant}
	if err != nil {
		return ev, err
	}
	return ev, stateful(e, func() error {
		if err := expect("past_due first", status(ctx, e, s.Sub), "past_due"); err != nil {
			return err
		}
		if err := e.Clock.Advance(ctx, e.Clock.Now().Add(35*24*time.Hour)); err != nil {
			return err
		}
		if _, err := e.Deliver.DeliverNew(ctx); err != nil {
			return err
		}
		return expect("status after retries are exhausted", status(ctx, e, s.Sub), "cancelled")
	})
}

func caseLaterSuccess(ctx context.Context, e *Env) (Evidence, error) {
	s, err := failingRenewal(ctx, e)
	ev := Evidence{"tenant_id": s.Tenant}
	if err != nil {
		return ev, err
	}
	return ev, stateful(e, func() error {
		if err := e.Provider.AttachDefaultPaymentMethod(ctx, port.CustomerRef{ProviderCustomerID: s.Customer}, "pm_card_visa", "pmfix:"+s.Sub); err != nil {
			return err
		}
		if err := e.Clock.Advance(ctx, e.Clock.Now().Add(4*24*time.Hour)); err != nil { // next Smart Retry
			return err
		}
		if _, err := e.Deliver.DeliverNew(ctx); err != nil {
			return err
		}
		return expect("status after a later success", status(ctx, e, s.Sub), "active")
	})
}

// ── one_off_charges ──────────────────────────────────────────────────────

func addon(ctx context.Context, e *Env, pm string) (Evidence, app.PurchaseView, error) {
	s, err := subscribe(ctx, e, domain.PlanPro, 2)
	if err != nil {
		return nil, app.PurchaseView{}, err
	}
	if pm != "" {
		if err := e.Provider.AttachDefaultPaymentMethod(ctx, port.CustomerRef{ProviderCustomerID: s.Customer}, pm, "pmaddon:"+s.Sub); err != nil {
			return nil, app.PurchaseView{}, err
		}
	}
	p, err := e.Svc.PurchaseTokenAddon(ctx, owner(s.Tenant), s.Tenant, app.AddonPurchaseRequest{PackCode: e.TokenPack, PackQuantity: 1, AcceptTerms: true})
	return Evidence{"tenant_id": s.Tenant, "purchase_id": p.PurchaseID, "status": p.Status}, p, err
}

func caseOffSession(ctx context.Context, e *Env) (Evidence, error) {
	ev, p, err := addon(ctx, e, "")
	if err != nil {
		return ev, fmt.Errorf("B-P20: %w", err)
	}
	return ev, expect("add-on status", p.Status, string(domain.PurchaseConfirmed))
}

func caseThreeDS(ctx context.Context, e *Env) (Evidence, error) {
	ev, p, err := addon(ctx, e, "pm_card_authenticationRequired")
	if !e.Stateful() {
		return ev, nil // the mock answers paid; the 3-D Secure branch needs the provider
	}
	if err != nil {
		return ev, fmt.Errorf("B-P20: %w", err)
	}
	ev["client_action"] = p.ClientAction != nil
	if err := expect("add-on status", p.Status, string(domain.PurchaseRequiresAction)); err != nil {
		return ev, err
	}
	if p.ClientAction == nil || p.ClientAction.ClientSecret == "" {
		return ev, errors.New("no 3-D Secure action returned (PAYMENT_ACTION_REQUIRED)")
	}
	return ev, nil
}

func caseDecline(ctx context.Context, e *Env) (Evidence, error) {
	ev, _, err := addon(ctx, e, "pm_card_chargeDeclined")
	if !e.Stateful() {
		return ev, nil
	}
	ev["error"] = fmt.Sprint(err)
	if !domain.HasCode(err, domain.ErrPaymentDeclined) {
		return ev, fmt.Errorf("want 402 PAYMENT_DECLINED, got %v", err)
	}
	return ev, nil
}

// ── proration ────────────────────────────────────────────────────────────

func caseProrationNative(ctx context.Context, e *Env) (Evidence, error) {
	s, err := subscribe(ctx, e, domain.PlanPro, 3)
	if err != nil {
		return nil, err
	}
	if err := e.Clock.Advance(ctx, e.Clock.Now().Add(10*24*time.Hour)); err != nil { // mid-cycle
		return nil, err
	}
	pv, err := e.Svc.PreviewSeats(ctx, owner(s.Tenant), s.Tenant, app.SeatPreviewRequest{AddSeats: 2})
	if err != nil {
		return nil, fmt.Errorf("B-P6: %w", err)
	}
	p, err := e.Svc.PurchaseSeats(ctx, owner(s.Tenant), s.Tenant, app.SeatPurchaseRequest{AddSeats: 2, PreviewID: pv.PreviewID})
	ev := Evidence{"previewed_minor": pv.AmountDueNow.AmountMinor, "charged_minor": p.Amount.AmountMinor, "amount_source": pv.AmountSource}
	if err != nil {
		return ev, fmt.Errorf("B-P7: %w", err)
	}
	if err := expect("amount source", pv.AmountSource, "provider_preview"); err != nil {
		return ev, err
	}
	return ev, stateful(e, func() error {
		inv := q[string](ctx, e, `SELECT coalesce(provider_invoice_id,'') FROM seat_purchases WHERE purchase_id = $1`, p.PurchaseID)
		v, err := e.Provider.GetInvoice(ctx, port.InvoiceRef{ProviderInvoiceID: inv})
		if err != nil {
			return err
		}
		ev["provider_invoice_minor"] = v.AmountDue.AmountMinor
		return expect("charged equals previewed", v.AmountDue.AmountMinor, pv.AmountDueNow.AmountMinor)
	})
}

func caseProrationFallback(context.Context, *Env) (Evidence, error) {
	return nil, errors.New("fallback proration is certified only for an adapter declaring Proration = fallback")
}

// ── collection_pause (recorded) ──────────────────────────────────────────

func caseCollectionPause(ctx context.Context, e *Env) (Evidence, error) {
	s, err := subscribe(ctx, e, domain.PlanPro, 2)
	if err != nil {
		return nil, err
	}
	ref := port.SubscriptionRef{ProviderSubscriptionID: s.ProviderSub}
	if err := e.Provider.PauseCollection(ctx, ref, true, "pause:"+s.Sub+":1"); err != nil {
		return nil, err
	}
	return Evidence{"tenant_id": s.Tenant}, e.Provider.PauseCollection(ctx, ref, false, "pause:"+s.Sub+":2")
}
