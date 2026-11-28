//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// §9.2 on the real schema: in-flight lock, takeover after the lock, unlock
// for a 3-D Secure retry, and a record past its expiry being a new request.
func TestBIL_IDEM_3_IdempotencyReclaimAndUnlock(t *testing.T) {
	ctx := context.Background()
	repo := pgadapter.NewIdempotencyRepository(appPool)
	runner := pgadapter.NewTxRunner(appPool)
	tenant := newID()
	in := func(fn func(ctx context.Context) error) {
		t.Helper()
		if err := runner.RunInTenantTx(ctx, tenant, fn); err != nil {
			t.Fatal(err)
		}
	}
	future := time.Now().Add(30 * time.Second)
	rec := domain.IdempotencyRecord{TenantID: tenant, Scope: "B-I1", Key: "charge:" + tenant, RequestHash: []byte{1, 2, 3},
		LockedUntil: &future, ExpiresAt: time.Now().Add(72 * time.Hour)}

	in(func(ctx context.Context) error {
		existing, err := repo.Begin(ctx, rec)
		if err != nil || existing != nil {
			t.Fatalf("first begin: %v %v", existing, err)
		}
		existing, err = repo.Begin(ctx, rec)
		if err != nil || existing == nil || existing.State != domain.IdempotencyInFlight || string(existing.RequestHash) != "\x01\x02\x03" {
			t.Fatalf("second begin: %+v %v", existing, err)
		}
		// Locked: no takeover.
		if ok, err := repo.Reclaim(ctx, rec); err != nil || ok {
			t.Fatalf("reclaim while locked: %v %v", ok, err)
		}
		// 3-D Secure: unlock, then the retry takes over; a different body cannot.
		if err := repo.Unlock(ctx, tenant, "B-I1", rec.Key); err != nil {
			t.Fatal(err)
		}
		other := rec
		other.RequestHash = []byte{9}
		if ok, _ := repo.Reclaim(ctx, other); ok {
			t.Fatal("other body took over")
		}
		if ok, err := repo.Reclaim(ctx, rec); err != nil || !ok {
			t.Fatalf("retry takeover: %v %v", ok, err)
		}
		if err := repo.Complete(ctx, tenant, "B-I1", rec.Key, 201, []byte(`{"billing_subscription_id":"x"}`)); err != nil {
			t.Fatal(err)
		}
		done, _ := repo.Begin(ctx, rec)
		if done == nil || done.State != domain.IdempotencyCompleted || *done.StatusCode != 201 || string(done.ResponseBody) != `{"billing_subscription_id": "x"}` {
			t.Fatalf("completed: %+v", done)
		}
		return nil
	})
	// Past expires_at: a new request, whatever its body.
	mustExec(t, superPool, `UPDATE idempotency_records SET expires_at = now() - interval '1 second' WHERE tenant_id = $1::uuid`, tenant)
	in(func(ctx context.Context) error {
		fresh := rec
		fresh.RequestHash = []byte{7}
		if ok, err := repo.Reclaim(ctx, fresh); err != nil || !ok {
			t.Fatalf("expired reclaim: %v %v", ok, err)
		}
		return nil
	})
}
