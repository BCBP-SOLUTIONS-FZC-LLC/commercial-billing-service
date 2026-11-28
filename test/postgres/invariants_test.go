//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// serializationFailure is a real SQLSTATE 40001 from the server.
func serializationFailure(t *testing.T) error {
	t.Helper()
	err := exec(context.Background(), superPool, `DO $$ BEGIN RAISE EXCEPTION 'injected conflict' USING ERRCODE = 'serialization_failure'; END $$`)
	if errCode(err) != "40001" {
		t.Fatalf("want a 40001 error, got %v", err)
	}
	return err
}

// BIL-CONC-4 (§9.1): tenant writes run in RunInTxWithRetryOpts. A
// serialization failure (or deadlock) is retried up to 3 attempts; one that
// clears commits, and one that persists comes back as CONCURRENT_UPDATE
// (409) with nothing committed.
func TestBIL_CONC_4_ContendedWritesRetryThenConcurrentUpdate(t *testing.T) {
	ctx := context.Background()
	tenant, subID := newAccountAndSubscription(t)
	tx := pgadapter.NewTxRunner(appPool)
	subs := pgadapter.NewSubscriptionRepository(appPool)
	conflict := serializationFailure(t)

	// Transient: the second attempt commits.
	attempts := 0
	err := tx.RunInTenantTx(ctx, tenant, func(ctx context.Context) error {
		attempts++
		if attempts == 1 {
			return conflict
		}
		_, err := subs.Update(ctx, subID, 0, func(s *domain.Subscription) { s.AutoRenew = false })
		return err
	})
	if err != nil || attempts != 2 {
		t.Fatalf("transient conflict: %d attempts, %v", attempts, err)
	}

	// Persistent: three attempts, then CONCURRENT_UPDATE, nothing committed.
	attempts = 0
	err = tx.RunInTenantTx(ctx, tenant, func(ctx context.Context) error {
		attempts++
		if _, err := subs.Update(ctx, subID, 0, func(s *domain.Subscription) { s.AutoRenew = true }); err != nil {
			return err
		}
		return conflict
	})
	var de *domain.DomainError
	if attempts != 3 || !errors.As(err, &de) || de.Code != domain.ErrConcurrentUpdate || de.Status() != 409 {
		t.Fatalf("persistent conflict: %d attempts, %v", attempts, err)
	}
	var after domain.Subscription
	if err := tx.RunInTenantTx(ctx, tenant, func(ctx context.Context) error {
		var err error
		after, err = subs.Get(ctx, subID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if after.AutoRenew {
		t.Fatal("a write from a failed attempt was committed")
	}
}

// BIL-RLS-2 (§4.3): the production app pool (pgadapter.OpenPools) binds
// app.tenant_id with set_config(…, true): the tenant is set inside the
// transaction and gone from the same connection afterwards, so a later
// checkout without a tenant sees no rows.
func TestBIL_RLS_2_TenantBindingIsTransactionLocal(t *testing.T) {
	ctx := context.Background()
	a := seedTenant(t)
	// One connection, so the second transaction reuses the first's session.
	t.Setenv("DATABASE_URL", withUser(migrateDSN, "billing_app", "devpassword")+"&pool_max_conns=1")
	pools, err := pgadapter.OpenPools(ctx, pgadapter.PoolOptions{
		System: pgadapter.Role{User: "billing_system", Password: "devpassword", MaxConns: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer pools.Close()
	read := func(ctx context.Context) (pid int, bound string, rows int) {
		t.Helper()
		err := pgcommon.RunInTx(ctx, pools.App, pgcommon.TxOptions{}, func(ctx context.Context, tx pgcommon.Tx) error {
			return tx.QueryRow(ctx, `SELECT pg_backend_pid(), coalesce(current_setting('app.tenant_id', true), ''),
				(SELECT count(*) FROM billing_accounts WHERE tenant_id = $1::uuid)`, a.Tenant).Scan(&pid, &bound, &rows)
		})
		if err != nil {
			t.Fatal(err)
		}
		return pid, bound, rows
	}
	pid1, bound, rows := read(asTenant(ctx, a.Tenant))
	if !strings.EqualFold(bound, a.Tenant) || rows != 1 {
		t.Fatalf("inside the tenant transaction: app.tenant_id %q, %d rows", bound, rows)
	}
	pid2, bound, rows := read(ctx)
	if pid2 != pid1 {
		t.Fatalf("second transaction ran on backend %d, not %d: the check needs the same session", pid2, pid1)
	}
	if bound != "" || rows != 0 {
		t.Fatalf("after commit on the same session: app.tenant_id %q, %d rows visible", bound, rows)
	}
}

// BIL-PCI-2 (§4, HLD §11.1): no table holds a PAN, CVV, expiry, bank
// account number, provider API key or webhook secret, and no payment-method
// display data (brand, last four, expiry month/year: read live from the
// provider). Provider identifiers are opaque text.
func TestBIL_PCI_2_NoCardDataColumns(t *testing.T) {
	ctx := context.Background()
	var cols []string
	err := pgcommon.RunInTx(ctx, superPool, pgcommon.TxOptions{AccessMode: "read only"}, func(ctx context.Context, tx pgcommon.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT table_name||'.'||column_name FROM information_schema.columns
			WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
			  AND column_name ~* '(^|_)(pan|cvv|cvc|cvv2|iban|last_?4|brand|fingerprint)($|_)|card_?num|exp(iry|iration)?_(month|year|date)|^expiry$|account_number|routing_number|api_key|secret'
			ORDER BY 1`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				return err
			}
			cols = append(cols, c)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 0 {
		t.Errorf("columns that could hold card data or a provider secret: %v", cols)
	}
	var nonText []string
	ids := 0
	err = pgcommon.RunInTx(ctx, superPool, pgcommon.TxOptions{AccessMode: "read only"}, func(ctx context.Context, tx pgcommon.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT table_name||'.'||column_name||' '||data_type, data_type = 'text' FROM information_schema.columns
			WHERE table_schema = 'public' AND column_name ~ '^provider_.*_(id|ref)$'
			ORDER BY 1`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c string
			var text bool
			if err := rows.Scan(&c, &text); err != nil {
				return err
			}
			ids++
			if !text {
				nonText = append(nonText, c)
			}
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if ids == 0 {
		t.Fatal("no provider identifier column found: the check ran against the wrong schema")
	}
	if len(nonText) != 0 {
		t.Errorf("provider identifiers not stored as opaque text: %v", nonText)
	}
}
