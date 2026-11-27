//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// §9.2 one row per key (BIL-OPS-11): a second add-on purchase or adjustment
// with the same (tenant_id, idempotency_key) is port.ErrIdempotencyKeyTaken
// (uq_qap_key, uq_adj_key), not an unmapped database error, and the
// by-key reads return the first row.
func TestBIL_OPS_11_IdempotencyKeyTakenIsAPortError(t *testing.T) {
	ctx := context.Background()
	tx := pgadapter.NewTxRunner(appPool)
	addons, money := pgadapter.NewAddonRepository(appPool), pgadapter.NewMoneyRepository(appPool)
	tenant, _ := newAccountAndSubscription(t)
	actor := newID()
	purchase := func() domain.QuotaAddonPurchase {
		return domain.QuotaAddonPurchase{PurchaseID: newID(), TenantID: tenant, PackCode: "tokens_1m", PackQuantity: 1, TokensTotal: 1_000_000,
			AmountMinor: 4900, Currency: "USD", Status: domain.PurchasePendingPayment, RequestedBy: actor, IdempotencyKey: "k-dup"}
	}
	first := purchase()
	run := func(fn func(ctx context.Context) error) error { return tx.RunInTenantTx(ctx, tenant, fn) }
	if err := run(func(ctx context.Context) error { return addons.Insert(ctx, first) }); err != nil {
		t.Fatal(err)
	}
	if err := run(func(ctx context.Context) error { return addons.Insert(ctx, purchase()) }); !errors.Is(err, port.ErrIdempotencyKeyTaken) {
		t.Fatalf("second purchase: %v, want port.ErrIdempotencyKeyTaken", err)
	}
	if err := run(func(ctx context.Context) error {
		p, found, err := addons.ByIdempotencyKey(ctx, tenant, "k-dup")
		if err != nil || !found || p.PurchaseID != first.PurchaseID {
			t.Errorf("by key %+v %v %v", p, found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	adj := func() domain.Adjustment {
		return domain.Adjustment{AdjustmentID: newID(), TenantID: tenant, Kind: "credit", AmountMinor: 500, Currency: "USD",
			Description: "Service credit", Reason: "SLA credit approved by Finance", ActorID: actor, IdempotencyKey: "k-dup"}
	}
	a1 := adj()
	if err := run(func(ctx context.Context) error { return money.InsertAdjustment(ctx, a1) }); err != nil {
		t.Fatal(err)
	}
	if err := run(func(ctx context.Context) error { return money.InsertAdjustment(ctx, adj()) }); !errors.Is(err, port.ErrIdempotencyKeyTaken) {
		t.Fatalf("second adjustment: %v, want port.ErrIdempotencyKeyTaken", err)
	}
	if err := run(func(ctx context.Context) error {
		a, found, err := money.AdjustmentByKey(ctx, tenant, "k-dup")
		if err != nil || !found || a.AdjustmentID != a1.AdjustmentID || a.CreatedAt.IsZero() || a.Reason != a1.Reason {
			t.Errorf("by key %+v %v %v", a, found, err)
		}
		_, found, err = money.RefundByKey(ctx, tenant, "k-none")
		if err != nil || found {
			t.Errorf("refund by key %v %v", found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
