//go:build integration

package commerce_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

func quantity(t *testing.T, sub string) int {
	t.Helper()
	return scalar[int](t, `SELECT quantity FROM subscription_items WHERE billing_subscription_id = $1`, sub)
}

// closeOpenSeatPurchases fails purchases left open by a rolled-back commit
// (the purchase-expiry job would, §8.5).
func closeOpenSeatPurchases(t *testing.T, tenant string) {
	t.Helper()
	exec(t, `UPDATE seat_purchases SET status = 'failed' WHERE tenant_id = $1::uuid AND status IN ('pending_payment','requires_action')`, tenant)
}

// B-P6 / B-P7 refusals (§5.4.1, §8.5): seat ceiling, provider unloaded, no
// current period, the fallback calculator outside the period, provider and
// storage failures, unknown and expired previews.
func TestSeatPreviewRefusals(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 10)
	_, err := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: domain.MaxLicensedSeats})
	isCode(t, "ceiling", err, domain.ErrValidation)
	e.reg.hidden["stripe"] = true
	_, err = e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 1})
	isCode(t, "provider unloaded", err, domain.ErrProviderNotEnabled)
	delete(e.reg.hidden, "stripe")
	start := scalar[time.Time](t, `SELECT current_period_start FROM subscriptions WHERE billing_subscription_id = $1`, sub)
	exec(t, `UPDATE subscriptions SET current_period_start = NULL WHERE billing_subscription_id = $1::uuid`, sub)
	_, err = e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 1})
	isCode(t, "no period", err, domain.ErrSubscriptionNotActive)
	exec(t, `UPDATE subscriptions SET current_period_start = $2 WHERE billing_subscription_id = $1::uuid`, sub, start)
	// §8.5.3 calculator: an instant before the period start is refused.
	e.p.fallback, e.now = true, start.Add(-time.Hour)
	_, err = e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 1})
	isCode(t, "instant outside the period", err, domain.ErrValidation)
	e.p.fallback, e.now = false, time.Now().UTC().Truncate(time.Microsecond)
	e.p.on("PreviewQuantityChange", answer{err: errProvider})
	_, err = e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 1})
	isCode(t, "provider preview", err, domain.ErrProviderUnavailable)
	e.fail("Previews.Save", 0)
	_, err = e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 1})
	isInjected(t, "preview save", err)

	_, err = e.svc.PurchaseSeats(ctx, withKey(owner, "k-sp-none-1"), newTenant(), app.SeatPurchaseRequest{AddSeats: 1, PreviewID: newTenant()})
	isCode(t, "no subscription", err, domain.ErrSubscriptionRequired)
	pv, err := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 2})
	if err != nil {
		t.Fatal(err)
	}
	buy := app.SeatPurchaseRequest{AddSeats: 2, PreviewID: pv.PreviewID}
	e.reg.hidden["stripe"] = true
	_, err = e.svc.PurchaseSeats(ctx, withKey(owner, "k-sp-prov-1"), tenant, buy)
	isCode(t, "purchase provider unloaded", err, domain.ErrProviderNotEnabled)
	delete(e.reg.hidden, "stripe")
	e.fail("Previews.Load", 0)
	_, err = e.svc.PurchaseSeats(ctx, withKey(owner, "k-sp-load-1"), tenant, buy)
	isInjected(t, "preview load", err)
	e.now = e.now.Add(11 * time.Minute) // SEAT_PREVIEW_TTL is 10 minutes
	_, err = e.svc.PurchaseSeats(ctx, withKey(owner, "k-sp-exp-01"), tenant, buy)
	isCode(t, "expired preview", err, domain.ErrPreviewExpired)
	if n := count(t, `SELECT count(*) FROM seat_purchases WHERE tenant_id = $1`, tenant); n != 0 || len(e.p.called("ChangeQuantity")) != 0 {
		t.Fatalf("refusals reached storage or the provider: %d %v", n, e.p.called("ChangeQuantity"))
	}
}

// §8.5 outcomes of the provider call (BIL-CONC-2: intent first, provider
// outside the transaction): declined and requires_action answered in the
// result, a provider error leaves the purchase pending, each confirm write
// failing rolls the confirmation back (no seats, no event), a callback that
// confirmed first wins, and a purchase without a client key gets one.
func TestSeatPurchaseOutcomes(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 10)
	buy := func(c app.Caller, add int) (app.PurchaseView, error) {
		t.Helper()
		pv, err := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: add})
		if err != nil {
			t.Fatal(err)
		}
		return e.svc.PurchaseSeats(ctx, c, tenant, app.SeatPurchaseRequest{AddSeats: add, PreviewID: pv.PreviewID})
	}
	row := func(key string) string {
		return scalar[string](t, `SELECT status::text||'/'||coalesce(provider_invoice_id,'')||'/'||coalesce(client_action->>'client_secret','')
			FROM seat_purchases WHERE tenant_id = $1 AND idempotency_key = $2`, tenant, key)
	}

	e.fail("SeatPurchases.Open", 0)
	_, err := buy(owner, 1)
	isInjected(t, "open purchase read", err)

	e.p.on("ChangeQuantity", answer{outcome: port.PaymentOutcome{Status: port.PaymentDeclined, DeclineCode: "expired_card"}})
	_, err = buy(withKey(owner, "k-seat-dcl1"), 1)
	isCode(t, "declined", err, domain.ErrPaymentDeclined)
	dcl := scalar[string](t, `SELECT purchase_id::text FROM seat_purchases WHERE idempotency_key = 'k-seat-dcl1'`)
	if ext(err, "decline_reason") != "expired_card" || row("k-seat-dcl1") != "failed/in_seatpur:"+dcl+"/" {
		t.Fatalf("declined %v %s", ext(err, "decline_reason"), row("k-seat-dcl1"))
	}
	// §8.5.1: the open proration invoice is voided (the pending update lapses).
	if n := len(e.p.called("VoidInvoice in_seatpur:" + dcl + " seatpur:" + dcl + ":void")); n != 1 {
		t.Fatalf("void %v", e.p.called("VoidInvoice"))
	}

	e.p.on("ChangeQuantity", answer{outcome: port.PaymentOutcome{Status: port.PaymentRequiresAction, ClientSecret: "pi_seat"}})
	res, err := buy(withKey(owner, "k-seat-3ds1"), 1)
	if err != nil || res.Status != "requires_action" || res.ClientAction == nil || res.ClientAction.ClientSecret != "pi_seat" {
		t.Fatalf("3ds %+v %v", res, err)
	}
	if got := row("k-seat-3ds1"); got != "requires_action/in_seatpur:"+res.PurchaseID+"/pi_seat" {
		t.Fatalf("3ds row %s", got)
	}
	closeOpenSeatPurchases(t, tenant)

	e.p.on("ChangeQuantity", answer{err: errProvider})
	_, err = buy(withKey(owner, "k-seat-err1"), 1)
	isCode(t, "provider error", err, domain.ErrProviderUnavailable)
	if got := row("k-seat-err1"); got != "pending_payment//" {
		t.Fatalf("provider error row %s", got)
	}
	closeOpenSeatPurchases(t, tenant)

	e.fail("SeatPurchases.Get", 0)
	_, err = buy(withKey(owner, "k-seat-get1"), 1)
	isInjected(t, "purchase re-read", err)
	closeOpenSeatPurchases(t, tenant)
	e.p.on("ChangeQuantity", answer{outcome: port.PaymentOutcome{Status: port.PaymentPending}})
	e.fail("SeatPurchases.Update", 0)
	_, err = buy(withKey(owner, "k-seat-upd1"), 1)
	isInjected(t, "pending record", err)
	closeOpenSeatPurchases(t, tenant)

	// Subs.Item skips 2: B-P6 and B-P7 read the item before the confirmation does.
	for _, f := range []struct {
		name string
		skip int
	}{{"Subs.Item", 2}, {"Subs.SetItem", 0}, {"SeatPurchases.Update", 0}, {"Subs.Get", 0}, {"Subs.AppendHistory", 0}} {
		e.fail(f.name, f.skip)
		_, err := buy(withKey(owner, "k-seat-"+f.name), 1)
		isInjected(t, f.name, err)
		if quantity(t, sub) != 10 || outbox(t, tenant, "TenantSeatsChanged") != 0 {
			t.Fatalf("%s: confirmation not rolled back", f.name)
		}
		closeOpenSeatPurchases(t, tenant)
	}

	// The invoice.paid callback confirms while the request is still with the
	// provider: the request returns the confirmed purchase, seats added once.
	e.p.on("ChangeQuantity", answer{outcome: port.PaymentOutcome{Status: port.PaymentPaid}, before: func(key string) {
		id, inv := strings.TrimPrefix(key, "seatpur:"), "in_"+key
		exec(t, `UPDATE seat_purchases SET provider_invoice_id = $2 WHERE purchase_id = $1::uuid`, id, inv)
		cur := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: inv}, Status: "paid",
			Fields: paidFields(t, "seat_purchases", id, "subscription", "sub_"+sub, "billing_reason", "subscription_update")}
		if out, err := e.handle(tenant, app.SeatInvoicePaid{S: e.svc}, callback("evt_race_"+id, "invoice.paid", domain.KindInvoicePaid), cur); err != nil || out != "applied:seat_purchase" {
			t.Errorf("racing callback %s %v", out, err)
		}
	}})
	res, err = buy(withKey(owner, "k-seat-race"), 3)
	if err != nil || res.Status != "confirmed" || *res.SeatsAfter != 13 {
		t.Fatalf("race %+v %v", res, err)
	}
	if quantity(t, sub) != 13 || outbox(t, tenant, "TenantSeatsChanged") != 1 {
		t.Fatalf("race: quantity %d events %d", quantity(t, sub), outbox(t, tenant, "TenantSeatsChanged"))
	}

	res, err = buy(owner, 2)
	if err != nil || res.Status != "confirmed" {
		t.Fatalf("no key %+v %v", res, err)
	}
	if got := scalar[string](t, `SELECT idempotency_key FROM seat_purchases WHERE purchase_id = $1`, res.PurchaseID); got != "seatpur:"+res.PurchaseID {
		t.Fatalf("generated key %s", got)
	}
	if quantity(t, sub) != 15 {
		t.Fatalf("quantity %d", quantity(t, sub))
	}

	// BIL-CONC-2 (rev 0.71): cancelled while ChangeQuantity runs (renewal
	// top-up or offboarding) and the provider answers paid: not reopened,
	// no seats added; the payment is parked for a refund (SEV-2).
	e.p.on("ChangeQuantity", answer{outcome: port.PaymentOutcome{Status: port.PaymentPaid}, before: func(key string) {
		exec(t, `UPDATE seat_purchases SET status = 'cancelled' WHERE purchase_id = $1::uuid`, strings.TrimPrefix(key, "seatpur:"))
	}})
	res, err = buy(withKey(owner, "k-seat-cancel"), 2)
	if err != nil || res.Status != "cancelled" || quantity(t, sub) != 15 {
		t.Fatalf("cancelled during the change %+v %v, quantity %d", res, err, quantity(t, sub))
	}
	if !e.log.has("ERROR", "BillingPaymentParked", "paid_after_cancel", res.PurchaseID) {
		t.Fatalf("payment after cancel not parked: %v", e.log.lines)
	}
}

// B-P8 / B-P9 reads with pagination, and the invoice.paid confirmation of
// a pending purchase (SeatInvoicePaid) all or nothing.
func TestSeatReadsAndCallback(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 10)
	var ids []string
	for i, outcome := range []port.PaymentStatus{port.PaymentPaid, port.PaymentPending} {
		pv, err := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 1})
		if err != nil {
			t.Fatal(err)
		}
		e.p.on("ChangeQuantity", answer{outcome: port.PaymentOutcome{Status: outcome}})
		res, err := e.svc.PurchaseSeats(ctx, withKey(owner, "k-seat-rd-"+string(rune('a'+i))), tenant, app.SeatPurchaseRequest{AddSeats: 1, PreviewID: pv.PreviewID})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.PurchaseID)
	}
	e.fail("SeatPurchases.Get", 0)
	_, err := e.svc.GetSeatPurchase(ctx, owner, tenant, ids[0])
	isInjected(t, "B-P8", err)
	e.fail("SeatPurchases.List", 0)
	_, err = e.svc.ListSeatPurchases(ctx, owner, tenant, app.Page{})
	isInjected(t, "B-P9", err)
	page, err := e.svc.ListSeatPurchases(ctx, owner, tenant, app.Page{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.NextCursor == nil || *page.NextCursor != page.Items[0].PurchaseID {
		t.Fatalf("page 1 %+v %v", page, err)
	}
	page2, err := e.svc.ListSeatPurchases(ctx, owner, tenant, app.Page{Limit: 1, Cursor: *page.NextCursor})
	if err != nil || len(page2.Items) != 1 || page2.Items[0].PurchaseID == page.Items[0].PurchaseID || page2.NextCursor != nil {
		t.Fatalf("page 2 %+v %v", page2, err)
	}

	h := app.SeatInvoicePaid{S: e.svc}
	ev := callback("evt_seat_paid", "invoice.paid", domain.KindInvoicePaid)
	cur := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: "in_seatpur:" + ids[1]}, Status: "paid",
		Fields: paidFields(t, "seat_purchases", ids[1], "subscription", "sub_"+sub, "billing_reason", "subscription_update")}
	other := cur
	other.Ref.ID = "in_unrelated"
	_, err = e.handle(tenant, h, ev, other)
	ignored(t, "unrelated invoice", err, "no_handler")
	for _, f := range []string{"Lock.LockTenant", "SeatPurchases.ByProviderInvoice", "Subs.SetItem"} {
		e.fail(f, 0)
		_, err := e.handle(tenant, h, ev, cur)
		isInjected(t, f, err)
	}
	if quantity(t, sub) != 11 {
		t.Fatalf("quantity after failed callbacks %d", quantity(t, sub))
	}
	if out, err := e.handle(tenant, h, ev, cur); err != nil || out != "applied:seat_purchase" {
		t.Fatalf("paid %s %v", out, err)
	}
	if v, err := e.svc.GetSeatPurchase(ctx, owner, tenant, ids[1]); err != nil || v.Status != "confirmed" || quantity(t, sub) != 12 {
		t.Fatalf("confirmed %+v %v", v, err)
	}
}

// §8.6.1 B-P10 / B-P11: refusals, replacing a schedule all or nothing, one
// being applied cannot be replaced, and cancelling one being applied
// releases the provider schedule first (BIL-SEAT-4: no event).
func TestReductionPaths(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	_, err := e.svc.ScheduleReduction(ctx, owner, newTenant(), app.ReductionRequest{TargetSeats: 5})
	isCode(t, "no subscription", err, domain.ErrSubscriptionNotActive)
	isCode(t, "cancel without subscription", e.svc.CancelReduction(ctx, owner, newTenant()), domain.ErrScheduleNotFound)

	tenant, sub := e.activeSubscription(t, "pro", 20)
	reduce := func(n int) (app.ScheduleView, error) {
		return e.svc.ScheduleReduction(ctx, owner, tenant, app.ReductionRequest{TargetSeats: n})
	}
	e.fail("Subs.Live", 0)
	_, err = reduce(10)
	isInjected(t, "live read", err)
	exec(t, `UPDATE subscriptions SET auto_renew = false WHERE billing_subscription_id = $1::uuid`, sub)
	_, err = reduce(10)
	isCode(t, "ending", err, domain.ErrSubscriptionEnding)
	exec(t, `UPDATE subscriptions SET auto_renew = true WHERE billing_subscription_id = $1::uuid`, sub)
	e.fail("Subs.Item", 0)
	_, err = reduce(10)
	isInjected(t, "item read", err)
	e.fail("Schedules.Open", 0)
	_, err = reduce(10)
	isInjected(t, "open schedule read", err)
	first, err := reduce(10)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"Schedules.Update", "Schedules.Insert"} {
		e.fail(f, 0)
		_, err := reduce(8)
		isInjected(t, f, err)
	}
	if got := scalar[string](t, `SELECT string_agg(target_seats||':'||status::text, ',') FROM seat_change_schedules WHERE billing_subscription_id = $1`, sub); got != "10:scheduled" {
		t.Fatalf("schedules after failed replacements %s", got)
	}
	exec(t, `UPDATE seat_change_schedules SET status = 'applying', provider_schedule_id = 'sub_sched_1' WHERE schedule_id = $1::uuid`, first.ScheduleID)
	_, err = reduce(8)
	isCode(t, "being applied", err, domain.ErrScheduleBeingApplied)

	// B-P11 on the schedule being applied.
	e.fail("Subs.Live", 0)
	isInjected(t, "cancel live read", e.svc.CancelReduction(ctx, owner, tenant))
	e.fail("Schedules.Latest", 0)
	isInjected(t, "cancel schedule read", e.svc.CancelReduction(ctx, owner, tenant))
	e.reg.hidden["stripe"] = true
	isCode(t, "cancel provider unloaded", e.svc.CancelReduction(ctx, owner, tenant), domain.ErrProviderNotEnabled)
	delete(e.reg.hidden, "stripe")
	e.p.on("ReleaseSchedule", answer{err: errProvider})
	isCode(t, "release", e.svc.CancelReduction(ctx, owner, tenant), domain.ErrProviderUnavailable)
	e.fail("Schedules.Get", 0)
	isInjected(t, "cancel commit", e.svc.CancelReduction(ctx, owner, tenant))
	if err := e.svc.CancelReduction(ctx, owner, tenant); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, `SELECT status::text FROM seat_change_schedules WHERE schedule_id = $1`, first.ScheduleID); got != "cancelled" {
		t.Fatalf("schedule %s", got)
	}
	if n := len(e.p.called("ReleaseSchedule sub_sched_1 sched:" + first.ScheduleID + ":release")); n != 3 {
		t.Fatalf("release calls %d, want 3 with one key", n)
	}

	second, err := reduce(12)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, `UPDATE seat_change_schedules SET status = 'applied' WHERE schedule_id = $1::uuid`, second.ScheduleID)
	isCode(t, "applied", e.svc.CancelReduction(ctx, owner, tenant), domain.ErrScheduleAlreadyApplied)
	if n := outbox(t, tenant, "TenantSeatsChanged"); n != 0 {
		t.Fatalf("events %d", n)
	}
}

// D-L2 (rev 0.73, §8.10 BIL-SUB-7): while collection is paused, scheduling
// or cancelling a seat reduction (B-P10, B-P11) and cancelling a scheduled
// downgrade (B-P5) answer 409 COLLECTION_PAUSED.
func TestCollectionPausedRefusesScheduleChanges(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 10)
	exec(t, `UPDATE subscriptions SET collection_paused = true, collection_paused_by = 'operator' WHERE billing_subscription_id = $1::uuid`, sub)
	_, err := e.svc.ScheduleReduction(ctx, owner, tenant, app.ReductionRequest{TargetSeats: 5})
	isCode(t, "B-P10", err, domain.ErrCollectionPaused)
	exec(t, `INSERT INTO seat_change_schedules (schedule_id, tenant_id, billing_subscription_id, target_seats, effective_at, requested_by)
		VALUES (gen_random_uuid(), $1, $2, 5, now() + interval '10 days', '00000000-0000-0000-0000-0000000000c1')`, tenant, sub)
	isCode(t, "B-P11", e.svc.CancelReduction(ctx, owner, tenant), domain.ErrCollectionPaused)
	exec(t, `INSERT INTO plan_change_requests (change_id, tenant_id, billing_subscription_id, from_plan, to_plan, direction, status, requested_by, idempotency_key)
		VALUES (gen_random_uuid(), $1, $2, 'pro', 'starter', 'downgrade', 'scheduled', '00000000-0000-0000-0000-0000000000c1', 'k-paused-dg')`, tenant, sub)
	isCode(t, "B-P5", e.svc.CancelPlanChange(ctx, owner, tenant), domain.ErrCollectionPaused)
}
