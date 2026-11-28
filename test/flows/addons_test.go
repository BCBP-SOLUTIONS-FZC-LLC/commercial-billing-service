//go:build integration

package flows_test

import (
	"context"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// converted returns an active tenant whose IAM lifecycle is active (as
// after TenantConverted).
func (e *env) converted(t *testing.T) (string, string) {
	t.Helper()
	tenant, sub := e.activeSubscription(t, "pro", 5)
	setLifecycle(t, tenant, "active", nil)
	return tenant, sub
}

// §8.7 / BIL-ADD-1, BIL-ADD-2: catalog, eligibility, purchase paid now →
// TenantQuotaAddOnChanged {purchase, delta, billing_ref, purchased_at},
// event_id stored; cap; terms; unlimited quota; trial.
func TestTokenAddonPurchase(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, _ := e.converted(t)

	cat, err := e.svc.ListTokenPacks(ctx, owner, tenant)
	if err != nil || !cat.Eligible || len(cat.Items) != 2 || cat.MonthlyCapRemainingTokens == nil || *cat.MonthlyCapRemainingTokens != 20_000_000 {
		t.Fatalf("catalog %+v %v", cat, err)
	}
	req := app.AddonPurchaseRequest{PackCode: "tokens_5m", PackQuantity: 2, AcceptTerms: true}
	if _, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-add-nt01"), tenant, app.AddonPurchaseRequest{PackCode: "tokens_5m", PackQuantity: 1}); !domain.HasCode(err, domain.ErrTermsNotAccepted) {
		t.Fatalf("terms: %v", err)
	}
	if _, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-add-na01"), tenant, app.AddonPurchaseRequest{PackCode: "tokens_9m", PackQuantity: 1, AcceptTerms: true}); !domain.HasCode(err, domain.ErrAddonNotAvailable) {
		t.Fatalf("pack: %v", err)
	}
	callsBefore := e.ent.calls
	res, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-add-0001"), tenant, req)
	if err != nil || res.Status != "confirmed" || *res.TokensTotal != 10_000_000 || res.Amount.AmountMinor != 39800 || res.Activation != "sent" {
		t.Fatalf("purchase %+v %v", res, err)
	}
	if e.ent.calls != callsBefore+1 {
		t.Fatal("purchase did not read Entitlements fresh")
	}
	if len(e.stripe.called("ChangeQuantity")) != 0 || len(e.stripe.called("ChargeOneOff price_tokens_5m x2 39800 addon:"+res.PurchaseID)) != 1 {
		t.Fatalf("provider: %v", e.stripe.calls)
	}
	ev := scalar[string](t, `SELECT (payload->'data'->>'delta')||'/'||(payload->'data'->>'reason')||'/'||(payload->'data'->>'billing_ref')||'/'||
		(payload->'data'->>'purchased_at' IS NOT NULL)::text||'/'||(payload->>'id' = (SELECT event_id::text FROM quota_addon_purchases WHERE purchase_id = $2))::text
		FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantQuotaAddOnChanged'`, tenant, res.PurchaseID)
	if ev != "10000000/purchase/"+res.PurchaseID+"/true/true" {
		t.Fatalf("event %s", ev)
	}
	// Cap: 10M confirmed of 20M → another 15M is refused.
	if _, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-add-cap1"), tenant, app.AddonPurchaseRequest{PackCode: "tokens_5m", PackQuantity: 3, AcceptTerms: true}); !domain.HasCode(err, domain.ErrAddonCapReached) {
		t.Fatalf("cap: %v", err)
	}
	// Unlimited quota → 409 unlimited_quota; Entitlements down → 503.
	e.ent.limit = nil
	var de *domain.DomainError
	if _, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-add-ul01"), tenant, req); !asDomain(err, &de) || de.Extensions["ineligible_reason"] != "unlimited_quota" {
		t.Fatalf("unlimited: %v", err)
	}
	e.ent.err = domain.NewError(domain.ErrEntitlementsUnavailable, "down")
	if _, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-add-dn01"), tenant, req); !domain.HasCode(err, domain.ErrEntitlementsUnavailable) {
		t.Fatalf("down: %v", err)
	}

	// A trial tenant (IAM lifecycle trial) is never sold an add-on.
	e.ent.err, e.ent.limit = nil, ptrTo(int64(1))
	trial, _ := e.activeSubscription(t, "pro", 1)
	if _, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-add-tr01"), trial, req); !asDomain(err, &de) || de.Extensions["ineligible_reason"] != "trial" {
		t.Fatalf("trial: %v", err)
	}
	if c, _ := e.svc.ListTokenPacks(ctx, owner, trial); c.Eligible || *c.IneligibleReason != "trial" {
		t.Fatalf("trial catalog %+v", c)
	}
}

// No default payment method → requires_action payment_session; B-P23 gives
// the hosted invoice page; the paid invoice confirms the purchase.
func TestTokenAddonPaymentSession(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, _ := e.converted(t)
	mustNoErr(t, sql(ctx, `UPDATE billing_accounts SET default_pm_set = false WHERE tenant_id = $1::uuid`, tenant))
	e.stripe.change = []confirmAnswer{{err: errNoPM}}
	res, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-add-ps01"), tenant, app.AddonPurchaseRequest{PackCode: "tokens_1m", PackQuantity: 1, AcceptTerms: true})
	if err != nil || res.Status != "requires_action" || res.ClientAction == nil || res.ClientAction.Type != "payment_session" {
		t.Fatalf("payment session %+v %v", res, err)
	}
	s, err := e.svc.AddonPaymentSession(ctx, owner, tenant, res.PurchaseID)
	if err != nil || s.URL != "https://invoice.example/in_addon:"+res.PurchaseID {
		t.Fatalf("B-P23 %+v %v", s, err)
	}
	cur := invoiceState("in_addon:"+res.PurchaseID, "", "paid", "manual", e.now, 1)
	delete(cur.Fields, "subscription")
	cur.Fields["amount_paid_minor"] = scalar[int64](t, `SELECT amount_minor FROM quota_addon_purchases WHERE purchase_id = $1`, res.PurchaseID)
	if out, err := e.handle(t, tenant, paidChain(e.svc), callback("evt_add", "invoice.paid", domain.KindInvoicePaid), cur); err != nil || out != "applied:addon_purchase" {
		t.Fatalf("paid: %s %v", out, err)
	}
	if v, _ := e.svc.GetAddonPurchase(ctx, owner, tenant, res.PurchaseID); v.Status != "confirmed" {
		t.Fatalf("status %s", v.Status)
	}
	// F-11 (rev 0.71, §4.2.14): once mirrored (invoice.finalized / updated),
	// the invoice is linked to the purchase it paid for.
	if _, err := e.handle(t, tenant, app.InvoiceUpdated{S: e.svc}, callback("evt_add_upd", "invoice.updated", domain.KindInvoiceUpdated), cur); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, `SELECT l.ref_type||'/'||l.ref_id::text FROM invoice_links l JOIN invoices i USING (invoice_id)
		WHERE i.provider_invoice_id = $1`, "in_addon:"+res.PurchaseID); got != "quota_addon_purchase/"+res.PurchaseID {
		t.Fatalf("invoice link %s", got)
	}
	if _, err := e.svc.AddonPaymentSession(ctx, owner, tenant, res.PurchaseID); !domain.HasCode(err, domain.ErrPurchaseNotAwaitingPayment) {
		t.Fatalf("after payment: %v", err)
	}
	if l, err := e.svc.ListAddonPurchases(ctx, owner, tenant, app.Page{}); err != nil || len(l.Items) != 1 {
		t.Fatalf("list %+v %v", l, err)
	}
}

// F-9 (rev 0.71, §8.8.4): offboarding closes an open token add-on purchase
// too. Its invoice is voided and the purchase cancelled, so purchase-expiry
// never settles it later and no TenantQuotaAddOnChanged follows the
// offboarding.
func TestOffboardingCancelsOpenAddonPurchase(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, _ := e.converted(t)
	mustNoErr(t, sql(ctx, `UPDATE billing_accounts SET default_pm_set = false WHERE tenant_id = $1::uuid`, tenant))
	e.stripe.change = []confirmAnswer{{err: errNoPM}}
	res, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-add-off1"), tenant, app.AddonPurchaseRequest{PackCode: "tokens_1m", PackQuantity: 1, AcceptTerms: true})
	if err != nil || res.Status != "requires_action" {
		t.Fatalf("purchase %+v %v", res, err)
	}
	setLifecycle(t, tenant, "offboarded", nil)
	if _, err := e.svc.RunLifecycleFollowUps(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, `SELECT status::text FROM quota_addon_purchases WHERE purchase_id = $1`, res.PurchaseID); got != "cancelled" {
		t.Fatalf("add-on purchase after offboarding: %s", got)
	}
	if n := len(e.stripe.called("VoidInvoice in_addon:" + res.PurchaseID)); n != 1 {
		t.Fatalf("add-on invoice not voided: %v", e.stripe.called("VoidInvoice"))
	}
	e.now = e.now.Add(48 * time.Hour)
	if _, err := e.svc.ExpireStale(ctx); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantQuotaAddOnChanged'`, tenant); n != 0 {
		t.Fatalf("%d TenantQuotaAddOnChanged after offboarding", n)
	}
}

// F-11 (rev 0.71, §5.4.1 SubscriptionView): B-P1 shows the scheduled seat
// reduction and the open purchases (here a token add-on awaiting payment),
// never with its client_action (BIL-API-4: client_secret only to the
// caller that started the payment).
func TestBIL_API_4_SubscriptionViewShowsWorkInFlight(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, _ := e.converted(t)
	if v, err := e.svc.GetSubscription(ctx, owner, tenant); err != nil || v.ScheduledReduction != nil || v.PendingPlanChange != nil || len(v.OpenPurchases) != 0 {
		t.Fatalf("idle view %+v %v", v, err)
	}
	if _, err := e.svc.ScheduleReduction(ctx, owner, tenant, app.ReductionRequest{TargetSeats: 1}); err != nil {
		t.Fatal(err)
	}
	e.stripe.change = []confirmAnswer{{err: errNoPM}}
	mustNoErr(t, sql(ctx, `UPDATE billing_accounts SET default_pm_set = false WHERE tenant_id = $1::uuid`, tenant))
	res, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-add-view1"), tenant, app.AddonPurchaseRequest{PackCode: "tokens_1m", PackQuantity: 1, AcceptTerms: true})
	if err != nil || res.Status != "requires_action" {
		t.Fatalf("purchase %+v %v", res, err)
	}
	v, err := e.svc.GetSubscription(ctx, owner, tenant)
	if err != nil || v.ScheduledReduction == nil || v.ScheduledReduction.TargetSeats != 1 {
		t.Fatalf("scheduled reduction %+v %v", v.ScheduledReduction, err)
	}
	if len(v.OpenPurchases) != 1 || v.OpenPurchases[0].PurchaseID != res.PurchaseID || v.OpenPurchases[0].Kind != "token_addon" ||
		v.OpenPurchases[0].ClientAction != nil {
		t.Fatalf("open purchases %+v", v.OpenPurchases)
	}
}
