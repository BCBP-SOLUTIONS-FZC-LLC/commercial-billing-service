//go:build integration

package commerce_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// Trial token add-ons (LLD rev 0.75, §8.7 "Trial tenants", BIL-ADD-1 (b),
// BIL-ADD-5, B-O18).

var tenantAdmin = app.Caller{UserID: "5c2f9b63-1111-4222-8333-944455556666", Roles: []string{domain.RoleTenantAdmin}}

func trialReq(pack string, qty int) app.AddonPurchaseRequest {
	return app.AddonPurchaseRequest{PackCode: pack, PackQuantity: qty, AcceptTerms: true}
}

// trialCapExt is the 409 ADDON_CAP_REACHED trial_cap extension.
func trialCapExt(t *testing.T, err error) map[string]any {
	t.Helper()
	isCode(t, "trial cap", err, domain.ErrAddonCapReached)
	m, ok := ext(err, "trial_cap").(map[string]any)
	if !ok {
		t.Fatalf("trial_cap extension %v", ext(err, "trial_cap"))
	}
	return m
}

func capRow(t *testing.T, tenant string) string {
	t.Helper()
	return scalar[string](t, `SELECT coalesce((SELECT max_packs||'/'||max_amount_minor||'/'||record_version FROM trial_addon_caps WHERE tenant_id = $1), 'none')`, tenant)
}

// A trial owner's first pack: the account is created (USD, trial, the
// provider customer outside the transaction with the trial key), the
// purchase is a trial purchase paid on the hosted page; an admin is
// owner_only; the cap counts a pending purchase; a failed one frees it.
func TestTrialAddonFirstPurchase(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.ent.elig = "eligible"
	tenant := newTenant()

	cat, err := e.svc.ListTokenPacks(ctx, tenantAdmin, tenant)
	if err != nil || cat.Eligible || *cat.IneligibleReason != "owner_only" || cat.TrialCap == nil || len(cat.Items) != 2 {
		t.Fatalf("admin catalog %+v %v", cat, err)
	}
	e.now = e.now.Add(61 * time.Second) // past the B-P19 display cache
	if cat, err = e.svc.ListTokenPacks(ctx, owner, tenant); err != nil || !cat.Eligible || cat.IneligibleReason != nil {
		t.Fatalf("owner catalog %+v %v", cat, err)
	}

	_, err = e.svc.PurchaseTokenAddon(ctx, withKey(tenantAdmin, "k-tr-adm1"), tenant, trialReq("tokens_1m", 1))
	isCode(t, "admin", err, domain.ErrAddonNotEligible)
	if ext(err, "ineligible_reason") != "owner_only" || len(e.p.called("EnsureCustomer")) != 0 ||
		count(t, `SELECT count(*) FROM billing_accounts WHERE tenant_id = $1`, tenant) != 0 {
		t.Fatalf("admin refused after side effects: %v %v", ext(err, "ineligible_reason"), e.p.called("EnsureCustomer"))
	}
	// A pack not in DEFAULT_CURRENCY for the provider is not on sale.
	_, err = e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-tr-na1"), tenant, trialReq("tokens_9m", 1))
	isCode(t, "unknown pack", err, domain.ErrAddonNotAvailable)

	// No default payment method: the provider answers with the invoice and
	// an error → requires_action payment_session (B-P23).
	e.p.on("ChargeOneOff", answer{err: errProvider})
	res, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-tr-001"), tenant, trialReq("tokens_1m", 1))
	if err != nil || res.Status != "requires_action" || res.ClientAction == nil || res.ClientAction.Type != "payment_session" {
		t.Fatalf("first purchase %+v %v", res, err)
	}
	if got := scalar[string](t, `SELECT provider||'/'||currency||'/'||iam_lifecycle::text||'/'||provider_customer_id||'/'||default_pm_set::text
		FROM billing_accounts WHERE tenant_id = $1`, tenant); got != "stripe/USD/trial/cus_"+tenant[:8]+"/false" {
		t.Fatalf("account %s", got)
	}
	if n := len(e.p.called("EnsureCustomer! USD bsub:trial:" + tenant + ":customer")); n != 1 {
		t.Fatalf("customer %v", e.p.called("EnsureCustomer"))
	}
	if got := scalar[string](t, `SELECT trial_purchase::text||'/'||status::text||'/'||amount_minor||'/'||currency FROM quota_addon_purchases WHERE purchase_id = $1`,
		res.PurchaseID); got != "true/requires_action/4900/USD" {
		t.Fatalf("purchase %s", got)
	}
	if n := len(e.p.called("ChargeOneOff price_tokens_1m x1 4900 addon:" + res.PurchaseID)); n != 1 {
		t.Fatalf("charge %v", e.p.called("ChargeOneOff"))
	}

	// The pending purchase counts toward the default cap (1 pack / 5000).
	_, err = e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-tr-002"), tenant, trialReq("tokens_1m", 1))
	if m := trialCapExt(t, err); m["max_packs"] != 1 || m["max_amount_minor"] != int64(5000) || m["used_packs"] != 1 || m["used_amount_minor"] != int64(4900) {
		t.Fatalf("trial_cap %v", m)
	}
	if cat, err := e.svc.ListTokenPacks(ctx, owner, tenant); err != nil ||
		*cat.TrialCap != (app.TrialCapView{MaxPacks: 1, MaxAmountMinor: 5000, UsedPacks: 1, UsedAmountMinor: 4900, Currency: "USD"}) {
		t.Fatalf("catalog cap %+v %v", cat.TrialCap, err)
	}
	// A failed, expired or cancelled purchase does not count.
	exec(t, `UPDATE quota_addon_purchases SET status = 'failed', client_action = NULL WHERE purchase_id = $1`, res.PurchaseID)
	res2, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-tr-003"), tenant, trialReq("tokens_1m", 1))
	if err != nil || res2.Status != "confirmed" {
		t.Fatalf("after the failed one %+v %v", res2, err)
	}
	if n := len(e.p.called("EnsureCustomer!")); n != 1 {
		t.Fatalf("customer created again: %v", e.p.called("EnsureCustomer!"))
	}
	if addonEvents(t, res2.PurchaseID) != 1 {
		t.Fatal("confirmation event")
	}
}

// An ended trial is refused by the fresh Entitlements read (trial_inactive,
// P-5f), Entitlements unreachable is 503, unlimited is unlimited_quota; a
// tenant whose subscription is live (paid, IAM still trial) is not a trial
// tenant.
func TestTrialAddonRefusals(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant := newTenant()
	buy := func(key string) error {
		_, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, key), tenant, trialReq("tokens_1m", 1))
		return err
	}
	for _, c := range []struct{ elig, want string }{{"trial_inactive", "trial_inactive"}, {"", "trial_inactive"}, {"trial", "trial_inactive"},
		{"unlimited_quota", "unlimited_quota"}} {
		e.ent.elig = c.elig
		err := buy("k-tr-ref-" + c.elig)
		isCode(t, c.elig, err, domain.ErrAddonNotEligible)
		if ext(err, "ineligible_reason") != c.want {
			t.Errorf("Entitlements %q → %v, want %s", c.elig, ext(err, "ineligible_reason"), c.want)
		}
	}
	e.ent.elig, e.ent.limit = "eligible", nil
	err := buy("k-tr-ref-ul")
	if isCode(t, "unlimited", err, domain.ErrAddonNotEligible); ext(err, "ineligible_reason") != "unlimited_quota" {
		t.Fatalf("unlimited %v", ext(err, "ineligible_reason"))
	}
	e.ent.err = domain.NewError(domain.ErrEntitlementsUnavailable, "down")
	isCode(t, "entitlements down", buy("k-tr-ref-dn"), domain.ErrEntitlementsUnavailable)
	if count(t, `SELECT count(*) FROM billing_accounts WHERE tenant_id = $1`, tenant) != 0 || len(e.p.called("EnsureCustomer")) != 0 {
		t.Fatal("refusals created an account or a customer")
	}
	e.now = e.now.Add(61 * time.Second) // past the B-P19 display cache
	e.ent.err, e.ent.elig = nil, "trial_inactive"
	if cat, err := e.svc.ListTokenPacks(ctx, owner, tenant); err != nil || cat.Eligible || *cat.IneligibleReason != "trial_inactive" {
		t.Fatalf("ended trial catalog %+v %v", cat, err)
	}

	// Paid at once (B-I1), TenantConverted not yet received: not a trial.
	paid, _ := e.activeSubscription(t, "pro", 2)
	e.ent.elig = "eligible"
	_, err = e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-tr-ref-paid"), paid, trialReq("tokens_1m", 1))
	if isCode(t, "live subscription", err, domain.ErrAddonNotEligible); ext(err, "ineligible_reason") != "not_active" {
		t.Fatalf("live subscription %v", ext(err, "ineligible_reason"))
	}
	if cat, err := e.svc.ListTokenPacks(ctx, owner, paid); err != nil || cat.TrialCap != nil {
		t.Fatalf("paid tenant catalog %+v %v", cat, err)
	}
}

// The account is created with the purchase under the tenant lock: provider
// and storage failures leave nothing; a concurrent writer's account is
// used; a duplicate insert is a retryable 409; a tenant that stopped being
// a trial between the read and the lock is refused; the cap reads fail
// closed.
func TestTrialAddonAccountAndCapFaults(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.ent.elig = "eligible"
	tenant := newTenant()
	buy := func(key string) (app.PurchaseView, error) {
		return e.svc.PurchaseTokenAddon(ctx, withKey(owner, key), tenant, trialReq("tokens_1m", 1))
	}
	none := func(what string) {
		t.Helper()
		if n := count(t, `SELECT count(*) FROM billing_accounts WHERE tenant_id = $1`, tenant) + count(t,
			`SELECT count(*) FROM quota_addon_purchases WHERE tenant_id = $1`, tenant); n != 0 {
			t.Fatalf("%s left %d rows", what, n)
		}
	}
	e.reg.hidden["stripe"] = true
	_, err := buy("k-tr-f-prov")
	isCode(t, "provider not enabled", err, domain.ErrProviderNotEnabled)
	delete(e.reg.hidden, "stripe")
	e.p.on("EnsureCustomer", answer{err: errProvider})
	_, err = buy("k-tr-f-cust")
	isCode(t, "customer", err, domain.ErrProviderUnavailable)
	// Under the lock (the first read passed): the account insert and the
	// cap reads roll back the account.
	for _, f := range []struct {
		name string
		skip int
	}{{"Accounts.Create", 0}, {"Addons.TrialCap", 1}, {"Addons.TrialUsage", 1}} {
		e.fail(f.name, f.skip)
		_, err := buy("k-tr-f-" + f.name)
		isInjected(t, f.name, err)
		none(f.name)
	}
	// B-P19 and B-P20's first read fail on the cap reads too.
	for _, f := range []string{"Addons.TrialCap", "Addons.TrialUsage"} {
		e.fail(f, 0)
		_, err := e.svc.ListTokenPacks(ctx, owner, tenant)
		isInjected(t, "catalog "+f, err)
	}
	e.f.set("Accounts.Create", 0, port.ErrAccountExists)
	_, err = buy("k-tr-f-exists")
	isCode(t, "account created concurrently", err, domain.ErrConcurrentUpdate)
	none("duplicate account")
	e.fail("Lock.LockTenant", 0)
	_, err = buy("k-tr-f-lock")
	isInjected(t, "lock", err)
	none("lock")

	// Another writer (B-P2) creates the account after the first read: the
	// purchase uses it, never a second one.
	e.p.on("EnsureCustomer", answer{before: func(string) {
		exec(t, `INSERT INTO billing_accounts (tenant_id, provider, provider_customer_id, currency, iam_lifecycle)
			VALUES ($1, 'stripe', 'cus_other', 'USD', 'trial')`, tenant)
	}})
	res, err := buy("k-tr-f-race")
	if err != nil || res.Status != "confirmed" {
		t.Fatalf("race %+v %v", res, err)
	}
	if got := scalar[string](t, `SELECT provider_customer_id FROM billing_accounts WHERE tenant_id = $1`, tenant); got != "cus_other" {
		t.Fatalf("account %s", got)
	}

	// The tenant converted (or a subscription went live) between the read
	// and the lock: refused, nothing inserted.
	other := newTenant()
	exec(t, `INSERT INTO billing_accounts (tenant_id, provider, provider_customer_id, currency, iam_lifecycle)
		VALUES ($1, 'stripe', 'cus_x', 'USD', 'trial')`, other)
	e.ent.hook = func() { exec(t, `UPDATE billing_accounts SET iam_lifecycle = 'active' WHERE tenant_id = $1`, other) }
	_, err = e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-tr-f-conv1"), other, trialReq("tokens_1m", 1))
	if isCode(t, "converted meanwhile", err, domain.ErrAddonNotEligible); ext(err, "ineligible_reason") != "not_active" {
		t.Fatalf("converted meanwhile %v", ext(err, "ineligible_reason"))
	}
	for _, f := range []string{"Accounts.Get", "Subs.Live"} {
		e.fail(f, 1) // the first read passes; the read under the lock fails
		exec(t, `UPDATE billing_accounts SET iam_lifecycle = 'trial' WHERE tenant_id = $1`, other)
		_, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-tr-f-"+f), other, trialReq("tokens_1m", 1))
		isInjected(t, "re-read "+f, err)
	}
	if n := count(t, `SELECT count(*) FROM quota_addon_purchases WHERE tenant_id = $1`, other); n != 0 {
		t.Fatalf("purchases %d", n)
	}
}

// BIL-ADD-5: the cap is counted under the tenant lock with the insert, so
// concurrent first purchases of a trial tenant without an account create
// one account and at most the capped number of purchases; whichever of
// packs and amount is reached first refuses.
func TestBIL_ADD_5_TrialCapUnderTheLock(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.ent.elig = "eligible"
	tenant := newTenant()
	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-tr-conc-"+string(rune('a'+i))), tenant, trialReq("tokens_1m", 1))
		}(i)
	}
	wg.Wait()
	ok, capped := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case domain.HasCode(err, domain.ErrAddonCapReached):
			capped++
		case domain.HasCode(err, domain.ErrConcurrentUpdate): // lost the account insert race: retryable
		default:
			t.Errorf("unexpected %v", err)
		}
	}
	if ok != 1 || count(t, `SELECT count(*) FROM quota_addon_purchases WHERE tenant_id = $1 AND trial_purchase`, tenant) != 1 ||
		count(t, `SELECT count(*) FROM billing_accounts WHERE tenant_id = $1`, tenant) != 1 {
		t.Fatalf("ok %d capped %d errs %v", ok, capped, errs)
	}

	// B-O18: 3 packs / 10000 → the amount is reached first (4900 + 9800).
	if _, err := e.svc.SetTrialAddonCap(ctx, withKey(operator, "k-cap-1"), tenant, app.TrialAddonCapRequest{MaxPacks: 3, MaxAmountMinor: 10000,
		Reason: "Enterprise PoC approved by Sales (deal 4411)"}); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-tr-amt"), tenant, trialReq("tokens_1m", 2))
	if m := trialCapExt(t, err); m["max_packs"] != 3 || m["max_amount_minor"] != int64(10000) || m["used_packs"] != 1 {
		t.Fatalf("by amount %v", m)
	}
	// 2 packs / 200000 → the pack count is reached first.
	if _, err := e.svc.SetTrialAddonCap(ctx, withKey(operator, "k-cap-2"), tenant, app.TrialAddonCapRequest{MaxPacks: 2, MaxAmountMinor: 200000,
		Reason: "Enterprise PoC approved by Sales (deal 4411)"}); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-tr-pk"), tenant, trialReq("tokens_1m", 2))
	if m := trialCapExt(t, err); m["max_packs"] != 2 || m["used_amount_minor"] != int64(4900) {
		t.Fatalf("by packs %v", m)
	}
	if res, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-tr-fit"), tenant, trialReq("tokens_5m", 1)); err != nil || res.Status != "confirmed" {
		t.Fatalf("within the raised cap %+v %v", res, err)
	}
}

// B-O18 (§5.4.4): validation, not_trial, the upsert under the lock with its
// AL-5 entry (old values 1 / 5000 without a row), faults roll back both.
func TestSetTrialAddonCap(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant := newTenant() // no account: a trial tenant
	reason := "Enterprise PoC approved by Sales (deal 4411)"
	set := func(key string, packs int, amount int64, why string) (app.TrialAddonCapView, error) {
		return e.svc.SetTrialAddonCap(ctx, withKey(operator, key), tenant, app.TrialAddonCapRequest{MaxPacks: packs, MaxAmountMinor: amount, Reason: why})
	}
	for _, c := range []struct {
		packs  int
		amount int64
		why    string
	}{{11, 5000, reason}, {0, 5000, reason}, {5, 200001, reason}, {5, 0, reason}, {5, 5000, "too short"}} {
		_, err := set("k-cap-v", c.packs, c.amount, c.why)
		isCode(t, "validation", err, domain.ErrValidation)
	}
	audits := func() int {
		return count(t, `SELECT count(*) FROM pending_audit_entries WHERE tenant_id = $1 AND entry->>'entry_type' = 'billing.trial_addon_cap.changed'`, tenant)
	}
	for _, f := range []string{"Lock.LockTenant", "Accounts.Get", "Addons.TrialCap", "Addons.UpsertTrialCap", "Addons.TrialUsage", "Audit.Write"} {
		e.fail(f, 0)
		_, err := set("k-cap-f", 5, 50000, reason)
		isInjected(t, f, err)
		if capRow(t, tenant) != "none" || audits() != 0 {
			t.Fatalf("%s: not rolled back (%s, %d audits)", f, capRow(t, tenant), audits())
		}
	}
	e.f.set("Addons.UpsertTrialCap", 0, port.ErrTrialCapChanged)
	_, err := set("k-cap-cc", 5, 50000, reason)
	isCode(t, "concurrent override", err, domain.ErrConcurrentUpdate)

	v, err := set("k-cap-1", 5, 50000, reason)
	if err != nil || v.TenantID != tenant || v.MaxPacks != 5 || v.MaxAmountMinor != 50000 || v.Currency != "USD" || v.UsedPacks != 0 ||
		v.SetBy != operator.UserID || !v.SetAt.Equal(e.now) || capRow(t, tenant) != "5/50000/1" {
		t.Fatalf("override %+v %v (%s)", v, err, capRow(t, tenant))
	}
	got := scalar[string](t, `SELECT (entry->'target'->>'type')||'/'||(entry->'target'->>'id')||'/'||(entry->'actor'->>'id')||'/'||
		(entry->'metadata'->>'old_max_packs')||'/'||(entry->'metadata'->>'new_max_packs')||'/'||(entry->'metadata'->>'old_max_amount_minor')||'/'||
		(entry->'metadata'->>'new_max_amount_minor')||'/'||(entry->'metadata'->>'tenant_id')||'/'||(entry->'metadata'->>'operator_id')||'/'||
		(entry->'metadata'->>'reason') FROM pending_audit_entries WHERE tenant_id = $1 AND entry->>'entry_type' = 'billing.trial_addon_cap.changed'`, tenant)
	if want := "tenant/" + tenant + "/" + operator.UserID + "/1/5/5000/50000/" + tenant + "/" + operator.UserID + "/" + reason; got != want {
		t.Fatalf("AL-5 %s\nwant %s", got, want)
	}
	// Changed again: the old values are the row's.
	if _, err := set("k-cap-2", 2, 9800, reason); err != nil || capRow(t, tenant) != "2/9800/2" || audits() != 2 {
		t.Fatalf("second override %v (%s, %d audits)", err, capRow(t, tenant), audits())
	}
	if n := count(t, `SELECT count(*) FROM pending_audit_entries WHERE tenant_id = $1 AND entry->'metadata'->>'old_max_packs' = '5'
		AND entry->'metadata'->>'old_max_amount_minor' = '50000' AND entry->'metadata'->>'new_max_packs' = '2'`, tenant); n != 1 {
		t.Fatalf("second AL-5 old values: %d", n)
	}

	// not_trial: a live subscription (even with IAM lifecycle still trial)
	// or a converted account.
	paid, _ := e.activeSubscription(t, "pro", 2)
	_, err = e.svc.SetTrialAddonCap(ctx, withKey(operator, "k-cap-nt"), paid, app.TrialAddonCapRequest{MaxPacks: 2, MaxAmountMinor: 9800, Reason: reason})
	if isCode(t, "not trial", err, domain.ErrAddonNotEligible); ext(err, "ineligible_reason") != "not_trial" {
		t.Fatalf("not_trial %v", ext(err, "ineligible_reason"))
	}
	e.fail("Subs.Live", 0)
	_, err = e.svc.SetTrialAddonCap(ctx, withKey(operator, "k-cap-sl"), paid, app.TrialAddonCapRequest{MaxPacks: 2, MaxAmountMinor: 9800, Reason: reason})
	isInjected(t, "Subs.Live", err)
}

// BIL-ADD-5 (review fix): a trial tenant's first purchase above the cap is
// refused before any provider customer or billing account is created, so a
// refused request leaves no orphan customer at the provider.
func TestTrialAddonOverCapCreatesNoCustomer(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.ent.elig = "eligible"
	tenant := newTenant()
	for i, req := range []app.AddonPurchaseRequest{trialReq("tokens_5m", 1), trialReq("tokens_1m", 2)} {
		_, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-tr-over"+string(rune('a'+i))), tenant, req)
		if m := trialCapExt(t, err); m["max_packs"] != 1 || m["used_packs"] != 0 {
			t.Fatalf("cap extension %v", m)
		}
	}
	if n := len(e.p.called("EnsureCustomer")); n != 0 {
		t.Fatalf("provider customers created for refused purchases: %d", n)
	}
	if n := count(t, `SELECT count(*) FROM billing_accounts WHERE tenant_id = $1`, tenant); n != 0 {
		t.Fatalf("account created for a refused purchase")
	}
}
