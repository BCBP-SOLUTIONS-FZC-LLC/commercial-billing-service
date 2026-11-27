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

func addonEvents(t *testing.T, purchaseID string) int {
	t.Helper()
	return count(t, `SELECT count(*) FROM outbox_events WHERE event_type = 'TenantQuotaAddOnChanged' AND payload->'data'->>'billing_ref' = $1`, purchaseID)
}

// B-P19 (§6, BIL-ADD-1): a tenant without an account is a trial; storage
// failures surface; the Entitlements display cache serves repeat reads for
// 60 s, after which a W-1 ineligibility reason from Entitlements wins; a
// purchase always reads Entitlements fresh.
func TestAddonCatalogPaths(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	cat, err := e.svc.ListTokenPacks(ctx, owner, newTenant())
	if err != nil || cat.Eligible || cat.IneligibleReason == nil || *cat.IneligibleReason != "trial" || len(cat.Items) != 0 || e.ent.calls != 0 {
		t.Fatalf("no account %+v %v (entitlements calls %d)", cat, err, e.ent.calls)
	}
	tenant, _ := e.converted(t, "pro", 5)
	for _, f := range []string{"Accounts.Get", "Subs.Live", "Addons.ConfirmedTokensSince"} {
		e.fail(f, 0)
		_, err := e.svc.ListTokenPacks(ctx, owner, tenant)
		isInjected(t, f, err)
	}
	e.now = e.now.Add(2 * time.Minute) // past the cache entry the failed read stored
	calls := e.ent.calls
	cat, err = e.svc.ListTokenPacks(ctx, owner, tenant)
	if err != nil || !cat.Eligible || len(cat.Items) != 2 || *cat.MonthlyCapRemainingTokens != 20_000_000 || e.ent.calls != calls+1 {
		t.Fatalf("catalog %+v %v", cat, err)
	}
	e.ent.elig = "tenant_suspended"
	if cat, err = e.svc.ListTokenPacks(ctx, owner, tenant); err != nil || !cat.Eligible || e.ent.calls != calls+1 {
		t.Fatalf("cached read %+v %v (calls %d)", cat, err, e.ent.calls)
	}
	_, err = e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-add-w1-01"), tenant, app.AddonPurchaseRequest{PackCode: "tokens_1m", PackQuantity: 1, AcceptTerms: true})
	isCode(t, "fresh read on purchase", err, domain.ErrAddonNotEligible)
	if ext(err, "ineligible_reason") != "tenant_suspended" {
		t.Fatalf("reason %v", ext(err, "ineligible_reason"))
	}
	e.now = e.now.Add(61 * time.Second)
	cat, err = e.svc.ListTokenPacks(ctx, owner, tenant)
	if err != nil || cat.Eligible || cat.IneligibleReason == nil || *cat.IneligibleReason != "tenant_suspended" {
		t.Fatalf("after the TTL %+v %v", cat, err)
	}
}

// §8.7 outcomes (BIL-ADD-2): the intent is recorded before the charge;
// declines and 3-D Secure (as an error or in the outcome); a provider error
// leaves the purchase pending; the confirmation (event, then the event id on
// the purchase) is all or nothing; a callback that confirmed first wins; a
// purchase without a client key gets one.
func TestAddonPurchaseOutcomes(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, _ := e.converted(t, "pro", 5)
	buy := func(c app.Caller) (app.PurchaseView, error) {
		return e.svc.PurchaseTokenAddon(ctx, c, tenant, app.AddonPurchaseRequest{PackCode: "tokens_1m", PackQuantity: 1, AcceptTerms: true})
	}
	row := func(key string) string {
		return scalar[string](t, `SELECT status::text||'/'||coalesce(client_action->>'client_secret','')||'/'||(event_id IS NOT NULL)::text
			FROM quota_addon_purchases WHERE tenant_id = $1 AND idempotency_key = $2`, tenant, key)
	}
	e.fail("Accounts.Get", 0)
	_, err := buy(withKey(owner, "k-add-acc01"))
	isInjected(t, "account read", err)
	e.fail("Addons.ConfirmedTokensSince", 0)
	_, err = buy(withKey(owner, "k-add-cap01"))
	isInjected(t, "cap read", err)
	if n := count(t, `SELECT count(*) FROM quota_addon_purchases WHERE tenant_id = $1`, tenant); n != 0 {
		t.Fatalf("rows before the intent %d", n)
	}
	e.reg.hidden["stripe"] = true
	_, err = buy(withKey(owner, "k-add-prv01"))
	isCode(t, "provider unloaded", err, domain.ErrProviderNotEnabled)
	delete(e.reg.hidden, "stripe")
	if got := row("k-add-prv01"); got != "pending_payment//false" {
		t.Fatalf("intent %s", got)
	}

	e.p.on("ChargeOneOff", answer{err: port.ErrDeclined{Code: "card_declined"}},
		answer{outcome: port.PaymentOutcome{Status: port.PaymentDeclined, DeclineCode: "processing_error"}})
	_, err = buy(withKey(owner, "k-add-dcl01"))
	isCode(t, "declined error", err, domain.ErrPaymentDeclined)
	if ext(err, "decline_reason") != "card_declined" || row("k-add-dcl01") != "failed//false" {
		t.Fatalf("declined %v %s", ext(err, "decline_reason"), row("k-add-dcl01"))
	}
	_, err = buy(withKey(owner, "k-add-dcl02"))
	isCode(t, "declined outcome", err, domain.ErrPaymentDeclined)
	if ext(err, "decline_reason") != "processing_error" || row("k-add-dcl02") != "failed//false" {
		t.Fatalf("declined %v %s", ext(err, "decline_reason"), row("k-add-dcl02"))
	}

	e.p.on("ChargeOneOff", answer{err: port.ErrActionRequired{ClientSecret: "pi_ae"}},
		answer{outcome: port.PaymentOutcome{Status: port.PaymentRequiresAction, ClientSecret: "pi_ao"}})
	for _, c := range []struct{ key, secret string }{{"k-add-3ds01", "pi_ae"}, {"k-add-3ds02", "pi_ao"}} {
		res, err := buy(withKey(owner, c.key))
		if err != nil || res.Status != "requires_action" || res.ClientAction == nil || res.ClientAction.ClientSecret != c.secret ||
			row(c.key) != "requires_action/"+c.secret+"/false" {
			t.Fatalf("3ds %s %+v %v %s", c.key, res, err, row(c.key))
		}
	}

	// The account has a default payment method: a provider error is not a
	// hosted-invoice case; the purchase stays pending for the callback.
	e.p.on("ChargeOneOff", answer{err: errProvider})
	_, err = buy(withKey(owner, "k-add-err01"))
	isCode(t, "provider error", err, domain.ErrProviderUnavailable)
	if got := row("k-add-err01"); got != "pending_payment//false" {
		t.Fatalf("provider error row %s", got)
	}

	e.fail("Addons.Get", 0)
	_, err = buy(withKey(owner, "k-add-get01"))
	isInjected(t, "purchase re-read", err)
	for _, f := range []string{"Events.Enqueue", "Addons.Update"} {
		e.fail(f, 0)
		_, err := buy(withKey(owner, "k-add-"+f))
		isInjected(t, f, err)
		if got := row("k-add-" + f); got != "pending_payment//false" {
			t.Fatalf("%s: confirmation not rolled back (%s)", f, got)
		}
	}
	e.p.on("ChargeOneOff", answer{outcome: port.PaymentOutcome{Status: port.PaymentRequiresAction, ClientSecret: "pi_x"}})
	e.fail("Addons.Update", 0)
	_, err = buy(withKey(owner, "k-add-upd01"))
	isInjected(t, "3ds record", err)
	if n := count(t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantQuotaAddOnChanged'`, tenant); n != 0 {
		t.Fatalf("events after failures %d", n)
	}

	// invoice.paid confirms while the charge request is still in flight.
	e.p.on("ChargeOneOff", answer{before: func(key string) {
		id, inv := strings.TrimPrefix(key, "addon:"), "in_"+key
		exec(t, `UPDATE quota_addon_purchases SET provider_invoice_id = $2 WHERE purchase_id = $1::uuid`, id, inv)
		cur := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: inv}, Status: "paid", Fields: map[string]any{}}
		if out, err := e.handle(tenant, app.AddonInvoicePaid{S: e.svc}, callback("evt_add_race_"+id, "invoice.paid", domain.KindInvoicePaid), cur); err != nil || out != "applied:addon_purchase" {
			t.Errorf("racing callback %s %v", out, err)
		}
	}})
	res, err := buy(withKey(owner, "k-add-race1"))
	if err != nil || res.Status != "confirmed" || res.Activation != "sent" || addonEvents(t, res.PurchaseID) != 1 {
		t.Fatalf("race %+v %v", res, err)
	}

	res, err = buy(owner)
	if err != nil || res.Status != "confirmed" || addonEvents(t, res.PurchaseID) != 1 {
		t.Fatalf("no key %+v %v", res, err)
	}
	if got := scalar[string](t, `SELECT idempotency_key FROM quota_addon_purchases WHERE purchase_id = $1`, res.PurchaseID); got != "addon:"+res.PurchaseID {
		t.Fatalf("generated key %s", got)
	}
	if n := len(e.p.called("ChargeOneOff price_tokens_1m x1 4900 addon:" + res.PurchaseID)); n != 1 {
		t.Fatalf("charge %v", e.p.called("ChargeOneOff"))
	}
}

// B-P21 / B-P22 / B-P23 and AddonInvoicePaid (§8.7): reads, pagination,
// the hosted invoice page for a purchase awaiting payment, and the
// confirmation callback all or nothing.
func TestAddonReadsAndCallback(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, _ := e.converted(t, "pro", 5)
	var ids []string
	for i, st := range []port.PaymentStatus{port.PaymentPaid, port.PaymentRequiresAction} {
		e.p.on("ChargeOneOff", answer{outcome: port.PaymentOutcome{Status: st}})
		res, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-add-rd-"+string(rune('a'+i))), tenant,
			app.AddonPurchaseRequest{PackCode: "tokens_1m", PackQuantity: 1, AcceptTerms: true})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.PurchaseID)
	}
	e.fail("Addons.Get", 0)
	_, err := e.svc.GetAddonPurchase(ctx, owner, tenant, ids[0])
	isInjected(t, "B-P21", err)
	_, err = e.svc.GetAddonPurchase(ctx, owner, tenant, newTenant())
	isCode(t, "B-P21 unknown", err, domain.ErrPurchaseNotFound)
	e.fail("Addons.List", 0)
	_, err = e.svc.ListAddonPurchases(ctx, owner, tenant, app.Page{})
	isInjected(t, "B-P22", err)
	page, err := e.svc.ListAddonPurchases(ctx, owner, tenant, app.Page{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.NextCursor == nil || *page.NextCursor != page.Items[0].PurchaseID {
		t.Fatalf("page 1 %+v %v", page, err)
	}
	page2, err := e.svc.ListAddonPurchases(ctx, owner, tenant, app.Page{Limit: 1, Cursor: *page.NextCursor})
	if err != nil || len(page2.Items) != 1 || page2.Items[0].PurchaseID == page.Items[0].PurchaseID {
		t.Fatalf("page 2 %+v %v", page2, err)
	}

	e.fail("Addons.Get", 0)
	_, err = e.svc.AddonPaymentSession(ctx, owner, tenant, ids[1])
	isInjected(t, "B-P23 read", err)
	_, err = e.svc.AddonPaymentSession(ctx, owner, tenant, newTenant())
	isCode(t, "B-P23 unknown", err, domain.ErrPurchaseNotFound)
	e.reg.hidden["stripe"] = true
	_, err = e.svc.AddonPaymentSession(ctx, owner, tenant, ids[1])
	isCode(t, "B-P23 provider unloaded", err, domain.ErrProviderNotEnabled)
	delete(e.reg.hidden, "stripe")
	e.p.on("PayOpenInvoiceLink", answer{err: errProvider})
	_, err = e.svc.AddonPaymentSession(ctx, owner, tenant, ids[1])
	isCode(t, "B-P23 provider", err, domain.ErrProviderUnavailable)
	if s, err := e.svc.AddonPaymentSession(ctx, owner, tenant, ids[1]); err != nil || s.URL != "https://invoice.example/in_addon:"+ids[1] {
		t.Fatalf("B-P23 %+v %v", s, err)
	}

	h := app.AddonInvoicePaid{S: e.svc}
	ev := callback("evt_add_paid", "invoice.paid", domain.KindInvoicePaid)
	cur := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: "in_addon:" + ids[1]}, Status: "paid", Fields: map[string]any{}}
	other := cur
	other.Ref.ID = "in_unrelated"
	_, err = e.handle(tenant, h, ev, other)
	ignored(t, "unrelated invoice", err, "no_handler")
	for _, f := range []string{"Lock.LockTenant", "Addons.ByProviderInvoice", "Events.Enqueue"} {
		e.fail(f, 0)
		_, err := e.handle(tenant, h, ev, cur)
		isInjected(t, f, err)
	}
	if addonEvents(t, ids[1]) != 0 {
		t.Fatal("event left by a failed callback")
	}
	if out, err := e.handle(tenant, h, ev, cur); err != nil || out != "applied:addon_purchase" {
		t.Fatalf("paid %s %v", out, err)
	}
	_, err = e.handle(tenant, h, ev, cur)
	ignored(t, "repeat", err, "purchase_confirmed")
	if addonEvents(t, ids[1]) != 1 {
		t.Fatal("confirmation event")
	}
}
