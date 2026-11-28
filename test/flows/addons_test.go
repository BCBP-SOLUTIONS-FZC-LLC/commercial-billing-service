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

	// Paid, TenantConverted not yet received (subscription live, IAM
	// lifecycle still trial): neither a paid-active nor a trial tenant
	// (rev 0.75: "trial" is no longer returned).
	e.ent.err, e.ent.limit = nil, ptrTo(int64(1))
	trial, _ := e.activeSubscription(t, "pro", 1)
	if _, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-add-tr01"), trial, req); !asDomain(err, &de) || de.Extensions["ineligible_reason"] != "not_active" {
		t.Fatalf("trial: %v", err)
	}
	if c, _ := e.svc.ListTokenPacks(ctx, owner, trial); c.Eligible || *c.IneligibleReason != "not_active" || c.TrialCap != nil {
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

// Rev 0.75 (§8.7 "Trial tenants", BIL-ADD-1 (b), BIL-ADD-5) end to end: a
// trial owner's first pack creates the USD account and is paid on the
// hosted page; invoice.paid confirms it (TenantQuotaAddOnChanged); B-O18
// raises the cap; B-P2 still starts the conversion in the account currency
// on the same customer; TenantConverted deletes the override.
func TestTrialTokenAddonFlow(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.ent.elig = "eligible"
	tenant := newTenant()
	admin := app.Caller{UserID: "5c2f9b63-1111-4222-8333-944455556666", Roles: []string{domain.RoleTenantAdmin}}
	req := app.AddonPurchaseRequest{PackCode: "tokens_1m", PackQuantity: 1, AcceptTerms: true}

	cat, err := e.svc.ListTokenPacks(ctx, owner, tenant)
	if err != nil || !cat.Eligible || cat.TrialCap == nil || *cat.TrialCap != (app.TrialCapView{MaxPacks: 1, MaxAmountMinor: 5000, Currency: "USD"}) ||
		len(cat.Items) != 2 || cat.Items[0].Price.Currency != "USD" {
		t.Fatalf("trial catalog %+v %v", cat, err)
	}
	var de *domain.DomainError
	if _, err := e.svc.PurchaseTokenAddon(ctx, withKey(admin, "k-trf-adm"), tenant, req); !asDomain(err, &de) || de.Extensions["ineligible_reason"] != "owner_only" {
		t.Fatalf("admin: %v", err)
	}

	e.stripe.change = []confirmAnswer{{err: errNoPM}}
	res, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-trf-001"), tenant, req)
	if err != nil || res.Status != "requires_action" || res.ClientAction == nil || res.ClientAction.Type != "payment_session" {
		t.Fatalf("first pack %+v %v", res, err)
	}
	if got := scalar[string](t, `SELECT provider||'/'||currency||'/'||iam_lifecycle::text FROM billing_accounts WHERE tenant_id = $1`, tenant); got != "stripe/USD/trial" {
		t.Fatalf("account %s", got)
	}
	if len(e.stripe.called("EnsureCustomer bsub:trial:"+tenant+":customer")) != 1 {
		t.Fatalf("customer %v", e.stripe.called("EnsureCustomer"))
	}
	s, err := e.svc.AddonPaymentSession(ctx, owner, tenant, res.PurchaseID)
	if err != nil || s.URL != "https://invoice.example/in_addon:"+res.PurchaseID {
		t.Fatalf("B-P23 %+v %v", s, err)
	}
	// Pending, it already counts toward the cap.
	if _, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-trf-002"), tenant, req); !asDomain(err, &de) || de.Code != domain.ErrAddonCapReached {
		t.Fatalf("cap: %v", err)
	}
	cur := invoiceState("in_addon:"+res.PurchaseID, "", "paid", "manual", e.now, 1)
	delete(cur.Fields, "subscription")
	cur.Fields["amount_paid_minor"] = int64(4900)
	if out, err := e.handle(t, tenant, paidChain(e.svc), callback("evt_trf_paid", "invoice.paid", domain.KindInvoicePaid), cur); err != nil || out != "applied:addon_purchase" {
		t.Fatalf("paid: %s %v", out, err)
	}
	ev := scalar[string](t, `SELECT (payload->'data'->>'delta')||'/'||(payload->'data'->>'reason')||'/'||(payload->'data'->>'billing_ref')
		FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantQuotaAddOnChanged'`, tenant)
	if ev != "1000000/purchase/"+res.PurchaseID {
		t.Fatalf("event %s", ev)
	}

	// B-O18 raises the cap: a second pack fits.
	v, err := e.svc.SetTrialAddonCap(ctx, withKey(operator, "k-trf-cap"), tenant, app.TrialAddonCapRequest{MaxPacks: 3, MaxAmountMinor: 20000,
		Reason: "Enterprise PoC approved by Sales (deal 4411)"})
	if err != nil || v.UsedPacks != 1 || v.UsedAmountMinor != 4900 || v.MaxPacks != 3 {
		t.Fatalf("B-O18 %+v %v", v, err)
	}
	if res2, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-trf-003"), tenant, req); err != nil || res2.Status != "confirmed" {
		t.Fatalf("second pack %+v %v", res2, err)
	}

	// The account currency is not fixed by the trial purchases: B-P2 uses
	// it (USD) on the same customer, no second customer.
	co, err := e.svc.StartCheckout(ctx, owner, tenant, app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 5})
	if err != nil || co.Price.Currency != "USD" || co.Price.AmountMinor != 5*6900 || len(e.stripe.called("EnsureCustomer")) != 1 {
		t.Fatalf("B-P2 after trial purchases %+v %v (%v)", co, err, e.stripe.called("EnsureCustomer"))
	}
	sub := co.BillingSubscriptionID
	evc, curc := checkoutCompleted(sub)
	if out, err := e.handle(t, tenant, app.CheckoutCompleted{S: e.svc}, evc, curc); err != nil || out != "applied:subscription_linked" {
		t.Fatalf("completed: %s %v", out, err)
	}
	evp, curp := firstInvoicePaid(sub, e.now.Truncate(time.Second), 5)
	if out, err := e.handle(t, tenant, app.FirstInvoicePaid{S: e.svc}, evp, curp); err != nil || out != "applied:conversion_payment" {
		t.Fatalf("conversion paid: %s %v", out, err)
	}
	// A live subscription: B-O18 now refuses (not_trial).
	if _, err := e.svc.SetTrialAddonCap(ctx, withKey(operator, "k-trf-cap2"), tenant, app.TrialAddonCapRequest{MaxPacks: 3, MaxAmountMinor: 20000,
		Reason: "Enterprise PoC approved by Sales (deal 4411)"}); !asDomain(err, &de) || de.Extensions["ineligible_reason"] != "not_trial" {
		t.Fatalf("not_trial: %v", err)
	}
	e.rp.answers = []domain.RP3Answer{{Status: 200}}
	if _, err := e.svc.RunSagas(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, `SELECT count(*) FROM trial_addon_caps WHERE tenant_id = $1`, tenant); n != 1 {
		t.Fatalf("override before TenantConverted: %d", n)
	}
	conv := app.TenantLifecycleEvent{EventID: "0193aaaa-0000-7000-8000-0000000000a5", Type: app.EventTenantConverted, Time: e.now,
		TenantID: tenant, BillingSubscriptionID: sub, Plan: domain.PlanPro}
	if err := pgTx(tenant, func(ctx context.Context) error {
		_, err := e.svc.ConfirmConversion(ctx, conv)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, `SELECT count(*) FROM trial_addon_caps WHERE tenant_id = $1`, tenant); n != 0 {
		t.Fatalf("override after TenantConverted: %d", n)
	}
	if got := scalar[string](t, `SELECT state::text FROM conversion_sagas WHERE billing_subscription_id = $1`, sub); got != "confirmed" {
		t.Fatalf("saga %s", got)
	}
}
