//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

func strp(s string) *string { return &s }

func codeOf(err error) domain.ErrorCode {
	var de *domain.DomainError
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

// newAccountAndSubscription creates an account and an incomplete
// subscription through the repositories, inside one tenant transaction.
func newAccountAndSubscription(t *testing.T) (tenant, subID string) {
	t.Helper()
	ctx := context.Background()
	tx := pgadapter.NewTxRunner(appPool)
	accounts, subs, prices := pgadapter.NewAccountRepository(appPool), pgadapter.NewSubscriptionRepository(appPool), pgadapter.NewPriceRepository(appPool)
	tenant, subID = newID(), newID()
	err := tx.RunInTenantTx(ctx, tenant, func(ctx context.Context) error {
		if err := accounts.Create(ctx, domain.BillingAccount{TenantID: tenant, Provider: "manual", Currency: "USD", IAMLifecycle: domain.LifecycleTrial}); err != nil {
			return err
		}
		price, ok, err := prices.PriceAt(ctx, domain.PlanPro, domain.IntervalMonth, "USD", "manual", time.Now())
		if err != nil || !ok {
			t.Fatalf("seed price: ok=%v err=%v", ok, err)
		}
		return subs.Create(ctx,
			domain.Subscription{BillingSubscriptionID: subID, TenantID: tenant, Provider: "manual", Origin: domain.OriginOperatorStub, Plan: domain.PlanPro, Interval: domain.IntervalMonth, AutoRenew: true},
			domain.SubscriptionItem{PriceID: price.PriceID, Quantity: 10})
	})
	if err != nil {
		t.Fatalf("create account and subscription: %v", err)
	}
	return tenant, subID
}

func TestAccountRepository(t *testing.T) {
	ctx := context.Background()
	tx, accounts := pgadapter.NewTxRunner(appPool), pgadapter.NewAccountRepository(appPool)
	tenant, _ := newAccountAndSubscription(t)

	var a domain.BillingAccount
	if err := tx.RunInTenantTx(ctx, tenant, func(ctx context.Context) error {
		var err error
		a, err = accounts.Get(ctx, tenant)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if a.TenantID != tenant || a.Currency != "USD" || a.IAMLifecycle != domain.LifecycleTrial || a.RecordVersion != 1 {
		t.Fatalf("unexpected account %+v", a)
	}
	// Optimistic locking: the right version updates and bumps; a stale one conflicts.
	err := tx.RunInTenantTx(ctx, tenant, func(ctx context.Context) error {
		u, err := accounts.UpdateContact(ctx, tenant, 1, strp("Ada"), strp("ada@example.com"), strp("Acme"), strp("AE"), nil)
		if err == nil && u.RecordVersion != 2 {
			t.Errorf("version after update = %d, want 2", u.RecordVersion)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	err = tx.RunInTenantTx(ctx, tenant, func(ctx context.Context) error {
		_, err := accounts.UpdateContact(ctx, tenant, 1, strp("Eve"), nil, nil, nil, nil)
		return err
	})
	var de *domain.DomainError
	if !errors.As(err, &de) || de.Code != domain.ErrOptimisticLockConflict || de.Extensions["record_version"] != int64(2) {
		t.Fatalf("stale write: got %v", err)
	}
	// Another tenant cannot see the account (RLS through the repository).
	err = tx.RunInTenantTx(ctx, newID(), func(ctx context.Context) error {
		_, err := accounts.Get(ctx, tenant)
		return err
	})
	if codeOf(err) != domain.ErrSubscriptionNotFound {
		t.Fatalf("other tenant read: got %v, want SUBSCRIPTION_NOT_FOUND", err)
	}
}

// BIL-SUB-4 through the repository: only §8.14 moves, with optimistic locking.
func TestSubscriptionRepositoryTransitions(t *testing.T) {
	ctx := context.Background()
	tx, subs := pgadapter.NewTxRunner(appPool), pgadapter.NewSubscriptionRepository(appPool)
	tenant, subID := newAccountAndSubscription(t)
	now := time.Now().UTC().Truncate(time.Second)
	end := now.Add(30 * 24 * time.Hour)

	run := func(fn func(ctx context.Context) error) error { return tx.RunInTenantTx(ctx, tenant, fn) }

	var live domain.Subscription
	if err := run(func(ctx context.Context) error {
		var ok bool
		var err error
		live, ok, err = subs.Live(ctx, tenant)
		if !ok && err == nil {
			t.Fatal("live subscription not found")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if live.Status != domain.StatusIncomplete {
		t.Fatalf("new subscription status %s", live.Status)
	}
	// incomplete → past_due is not in §8.14.
	err := run(func(ctx context.Context) error {
		_, err := subs.Transition(ctx, subID, live.RecordVersion, domain.StatusPastDue, nil)
		return err
	})
	if codeOf(err) != domain.ErrInternal {
		t.Fatalf("forbidden move: got %v", err)
	}
	// incomplete → active with the period and history in one transaction.
	err = run(func(ctx context.Context) error {
		s, err := subs.Transition(ctx, subID, live.RecordVersion, domain.StatusActive, func(s *domain.Subscription) {
			s.CurrentPeriodStart, s.CurrentPeriodEnd, s.PaymentConfirmedAt = &now, &end, &now
		})
		if err != nil {
			return err
		}
		from := domain.StatusIncomplete
		live = s
		return subs.AppendHistory(ctx, domain.StatusHistoryEntry{TenantID: tenant, BillingSubscriptionID: subID,
			FromStatus: &from, ToStatus: domain.StatusActive, Cause: "operator:B-O1", ActorID: domain.CommercialSystemPrincipal})
	})
	if err != nil {
		t.Fatal(err)
	}
	if live.Status != domain.StatusActive || live.RecordVersion != 2 || live.CurrentPeriodEnd == nil {
		t.Fatalf("after activation: %+v", live)
	}
	// A stale version conflicts.
	err = run(func(ctx context.Context) error {
		_, err := subs.Transition(ctx, subID, 1, domain.StatusCancelled, nil)
		return err
	})
	if codeOf(err) != domain.ErrOptimisticLockConflict {
		t.Fatalf("stale transition: got %v", err)
	}
	// active → cancelled needs cancelled_at (chk_sub_cancelled).
	err = run(func(ctx context.Context) error {
		r := domain.CancelCustomer
		_, err := subs.Transition(ctx, subID, live.RecordVersion, domain.StatusCancelled, func(s *domain.Subscription) {
			s.CancelledAt, s.CancelReason, s.AutoRenew = &now, &r, false
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var hist []domain.StatusHistoryEntry
	var item domain.SubscriptionItem
	if err := run(func(ctx context.Context) error {
		var err error
		if hist, err = subs.History(ctx, subID, 10); err != nil {
			return err
		}
		item, err = subs.Item(ctx, subID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].ToStatus != domain.StatusActive || hist[0].Cause != "operator:B-O1" {
		t.Fatalf("history %+v", hist)
	}
	if item.Quantity != 10 || item.Kind != "seats" {
		t.Fatalf("item %+v", item)
	}
}

// BIL-SUB-3 through the repository.
func TestSubscriptionRepositoryOneLive(t *testing.T) {
	ctx := context.Background()
	tx, subs, prices := pgadapter.NewTxRunner(appPool), pgadapter.NewSubscriptionRepository(appPool), pgadapter.NewPriceRepository(appPool)
	tenant, _ := newAccountAndSubscription(t)
	err := tx.RunInTenantTx(ctx, tenant, func(ctx context.Context) error {
		p, _, _ := prices.PriceAt(ctx, domain.PlanPro, domain.IntervalMonth, "USD", "manual", time.Now())
		return subs.Create(ctx, domain.Subscription{BillingSubscriptionID: newID(), TenantID: tenant, Provider: "manual",
			Origin: domain.OriginDirectSignup, Plan: domain.PlanPro, Interval: domain.IntervalMonth}, domain.SubscriptionItem{PriceID: p.PriceID, Quantity: 1})
	})
	if codeOf(err) != domain.ErrSubscriptionAlreadyExists {
		t.Fatalf("second live subscription: got %v", err)
	}
}

func TestPriceAndCertificationRepositories(t *testing.T) {
	ctx := context.Background()
	prices, certs := pgadapter.NewPriceRepository(appPool), pgadapter.NewCertificationRepository(appPool)
	p, ok, err := prices.PriceAt(ctx, domain.PlanEnterprise, domain.IntervalYear, "USD", "manual", time.Now())
	if err != nil || !ok || p.UnitAmountMinor != 182580 {
		t.Fatalf("enterprise annual seed: %+v ok=%v err=%v", p, ok, err)
	}
	if _, ok, _ := prices.PriceAt(ctx, domain.PlanPro, domain.IntervalMonth, "USD", "manual", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)); ok {
		t.Fatal("no price before the seed's effective_from")
	}
	if _, ok, _ := prices.PriceAt(ctx, domain.PlanPro, domain.IntervalMonth, "AED", "manual", time.Now()); ok {
		t.Fatal("no AED row in the seed")
	}
	c, ok, err := certs.Enabling(ctx, "manual", "1.0.0+manual", "cert-1.0")
	if err != nil || !ok || !c.Status.Enables() {
		t.Fatalf("manual exempt: %+v ok=%v err=%v", c, ok, err)
	}
	if _, ok, _ := certs.Enabling(ctx, "stripe", "1.0.0+stripeapi.x", "cert-1.0"); ok {
		t.Fatal("uncertified stripe must not be enabled (BIL-PROV-1)")
	}
	if _, ok, _ := certs.Enabling(ctx, "manual", "1.0.0+manual", "cert-2.0"); ok {
		t.Fatal("another suite version must not enable")
	}
}

// BIL-PAY-1 storage: one row per provider event; redelivery inserts nothing.
func TestLedgerRepository(t *testing.T) {
	ctx := context.Background()
	ledger, system := pgadapter.NewLedgerRepository(systemPool), pgadapter.NewSystemRepository(systemPool)
	s := seedTenant(t)
	e := domain.LedgerEntry{Provider: "stripe", ProviderEventID: "evt_" + newID(), EventType: "invoice.paid", NormalisedKind: "invoice_paid",
		Livemode: false, ProviderCreatedAt: time.Now(), Payload: []byte(`{"id":"in_1"}`)}
	if ins, err := ledger.Insert(ctx, e); err != nil || !ins {
		t.Fatalf("first insert: %v %v", ins, err)
	}
	if ins, err := ledger.Insert(ctx, e); err != nil || ins {
		t.Fatalf("redelivery must insert nothing: %v %v", ins, err)
	}
	got, err := ledger.Get(ctx, e.Provider, e.ProviderEventID)
	if err != nil || got.Status != domain.LedgerReceived || got.TenantID != nil {
		t.Fatalf("ledger row %+v %v", got, err)
	}
	// Tenant resolution from the provider customer (BIL-PAY-3).
	tenant, found, err := system.TenantForCustomer(ctx, "stripe", "cus_"+s.Tenant)
	if err != nil || !found || tenant != s.Tenant {
		t.Fatalf("customer → tenant: %q %v %v", tenant, found, err)
	}
	if _, found, _ := system.TenantForCustomer(ctx, "stripe", "cus_unknown"); found {
		t.Fatal("unknown customer must not resolve")
	}
	// The app pool cannot use the ledger (no grant).
	if _, err := pgadapter.NewLedgerRepository(appPool).Insert(ctx, e); errCode(err) != "42501" {
		t.Fatalf("billing_app inserting into the ledger: %v", err)
	}
}

// §9.2 records: begin, replay lookup, complete, release.
func TestIdempotencyRepository(t *testing.T) {
	ctx := context.Background()
	tx, idem := pgadapter.NewTxRunner(appPool), pgadapter.NewIdempotencyRepository(appPool)
	tenant, _ := newAccountAndSubscription(t)
	rec := domain.IdempotencyRecord{TenantID: tenant, Scope: "B-P7", Key: "key-" + newID()[:8], RequestHash: []byte{1, 2, 3},
		ExpiresAt: time.Now().Add(72 * time.Hour)}
	run := func(fn func(ctx context.Context) error) {
		t.Helper()
		if err := tx.RunInTenantTx(ctx, tenant, fn); err != nil {
			t.Fatal(err)
		}
	}
	run(func(ctx context.Context) error {
		existing, err := idem.Begin(ctx, rec)
		if existing != nil {
			t.Fatal("first Begin must insert")
		}
		return err
	})
	run(func(ctx context.Context) error {
		existing, err := idem.Begin(ctx, rec)
		if existing == nil || existing.State != domain.IdempotencyInFlight {
			t.Fatalf("second Begin must return the in-flight record, got %+v", existing)
		}
		return err
	})
	run(func(ctx context.Context) error {
		return idem.Complete(ctx, tenant, rec.Scope, rec.Key, 202, []byte(`{"status":"confirmed"}`))
	})
	run(func(ctx context.Context) error {
		existing, err := idem.Begin(ctx, rec)
		if existing == nil || existing.State != domain.IdempotencyCompleted || existing.StatusCode == nil || *existing.StatusCode != 202 {
			t.Fatalf("completed record for replay, got %+v", existing)
		}
		return err
	})
	// Release only removes in-flight records.
	other := rec
	other.Key = "key-" + newID()[:8]
	run(func(ctx context.Context) error { _, err := idem.Begin(ctx, other); return err })
	run(func(ctx context.Context) error { return idem.Release(ctx, tenant, other.Scope, other.Key) })
	run(func(ctx context.Context) error {
		existing, err := idem.Begin(ctx, other)
		if existing != nil {
			t.Fatal("released key must be free again")
		}
		return err
	})
}

func TestTxRunnerRequiresTenant(t *testing.T) {
	err := pgadapter.NewTxRunner(appPool).RunInTenantTx(context.Background(), "", func(context.Context) error { return nil })
	if codeOf(err) != domain.ErrInternal {
		t.Fatalf("empty tenant: %v", err)
	}
}

func TestTxRunnerRollsBackOnError(t *testing.T) {
	ctx := context.Background()
	tx, accounts := pgadapter.NewTxRunner(appPool), pgadapter.NewAccountRepository(appPool)
	tenant := newID()
	boom := errors.New("boom")
	err := tx.RunInTenantTx(ctx, tenant, func(ctx context.Context) error {
		if err := accounts.Create(ctx, domain.BillingAccount{TenantID: tenant, Provider: "manual", Currency: "USD"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	err = tx.RunInTenantTx(ctx, tenant, func(ctx context.Context) error { _, err := accounts.Get(ctx, tenant); return err })
	if codeOf(err) != domain.ErrSubscriptionNotFound {
		t.Fatalf("rolled-back account must not exist: %v", err)
	}
}
