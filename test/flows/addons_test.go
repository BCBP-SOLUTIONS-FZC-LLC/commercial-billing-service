//go:build integration

package flows_test

import (
	"context"
	"testing"

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
	if out, err := e.handle(t, tenant, paidChain(e.svc), callback("evt_add", "invoice.paid", domain.KindInvoicePaid), cur); err != nil || out != "applied:addon_purchase" {
		t.Fatalf("paid: %s %v", out, err)
	}
	if v, _ := e.svc.GetAddonPurchase(ctx, owner, tenant, res.PurchaseID); v.Status != "confirmed" {
		t.Fatalf("status %s", v.Status)
	}
	if _, err := e.svc.AddonPaymentSession(ctx, owner, tenant, res.PurchaseID); !domain.HasCode(err, domain.ErrPurchaseNotAwaitingPayment) {
		t.Fatalf("after payment: %v", err)
	}
	if l, err := e.svc.ListAddonPurchases(ctx, owner, tenant, app.Page{}); err != nil || len(l.Items) != 1 {
		t.Fatalf("list %+v %v", l, err)
	}
}
