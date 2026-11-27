package postgres

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// These tests run the repositories on a scripted transaction (fakeTx), to
// prove what each does with a failure Postgres reports mid-statement: a
// query error, a row that fails to scan, no rows, a constraint violation.
// The happy paths run against Postgres in test/postgres.

var errBoom = errors.New("boom")

// fakeTx answers Query and QueryRow from per-call scripts; a nil script
// answers errBoom. (Statements run through Exec, and SQLSTATE handling,
// are exercised against Postgres in test/postgres.)
type fakeTx struct {
	pgcommon.Tx
	query  []func() (pgcommon.Rows, error)
	row    []pgcommon.Row
	nq, nr int
}

func (f *fakeTx) Query(context.Context, string, ...any) (pgcommon.Rows, error) {
	defer func() { f.nq++ }()
	if f.nq < len(f.query) && f.query[f.nq] != nil {
		return f.query[f.nq]()
	}
	return nil, errBoom
}

func (f *fakeTx) QueryRow(context.Context, string, ...any) pgcommon.Row {
	defer func() { f.nr++ }()
	if f.nr < len(f.row) && f.row[f.nr] != nil {
		return f.row[f.nr]
	}
	return fakeRow{err: errBoom}
}

// fakeRow scans vals positionally, or fails with err.
type fakeRow struct {
	vals []any
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return assign(r.vals, dest)
}

func assign(vals, dest []any) error {
	for i, d := range dest {
		if i >= len(vals) || vals[i] == nil {
			continue
		}
		reflect.ValueOf(d).Elem().Set(reflect.ValueOf(vals[i]))
	}
	return nil
}

// fakeRows yields rows of vals; Scan fails with scanErr when set.
type fakeRows struct {
	pgcommon.Rows
	vals    [][]any
	i       int
	scanErr error
}

func (r *fakeRows) Next() bool { r.i++; return r.i <= len(r.vals) }
func (r *fakeRows) Close()     {}
func (r *fakeRows) Err() error { return nil }
func (r *fakeRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	return assign(r.vals[r.i-1], dest)
}

func rows(vals ...[]any) func() (pgcommon.Rows, error) {
	return func() (pgcommon.Rows, error) { return &fakeRows{vals: vals}, nil }
}

func badRows() func() (pgcommon.Rows, error) {
	return func() (pgcommon.Rows, error) { return &fakeRows{vals: [][]any{{"x"}}, scanErr: errBoom}, nil }
}

var noRows = fakeRow{err: pgcommon.ErrNoRows}

func in(f *fakeTx) context.Context { return withTx(context.Background(), f) }

func wantBoom(t *testing.T, name string, err error) {
	t.Helper()
	if !errors.Is(err, errBoom) {
		t.Errorf("%s: err %v, want the statement's error", name, err)
	}
}

func wantCode(t *testing.T, name string, err error, code domain.ErrorCode) {
	t.Helper()
	if !domain.HasCode(err, code) {
		t.Errorf("%s: err %v, want %s", name, err, code)
	}
}

const tid = "3fa85f64-5717-4562-b3fc-2c963f66afa6"

// A failed query is returned by every list/scan repository method.
func TestQueryErrorsPropagate(t *testing.T) {
	now := time.Now()
	for name, call := range map[string]func(ctx context.Context) error{
		"ActivePacks": func(ctx context.Context) error {
			_, err := NewAddonRepository(nil).ActivePacks(ctx, "USD", "stripe", now)
			return err
		},
		"AddonList": func(ctx context.Context) error { _, err := NewAddonRepository(nil).List(ctx, tid, "", 10); return err },
		"DueTenants": func(ctx context.Context) error {
			_, err := NewEmissionRepository(nil, nil).DueTenants(ctx, now, 10)
			return err
		},
		"ClaimDue": func(ctx context.Context) error {
			_, err := NewEmissionRepository(nil, nil).ClaimDue(ctx, tid, now)
			return err
		},
		"SagaDue":   func(ctx context.Context) error { _, err := NewSagaRepository(nil, nil).Due(ctx, now, 10); return err },
		"FollowUps": func(ctx context.Context) error { _, err := NewFollowUpRepository(nil).Due(ctx, true, 10); return err },
		"InvoiceList": func(ctx context.Context) error {
			_, err := NewInvoiceRepository(nil).List(ctx, tid, "", "", "", false, 10)
			return err
		},
		"OverdueSagas": func(ctx context.Context) error {
			_, _, err := NewJobRepository(nil, nil).OverdueSagas(ctx, now)
			return err
		},
		"StaleWork": func(ctx context.Context) error {
			_, err := NewJobRepository(nil, nil).StaleWork(ctx, now, now, now, 10)
			return err
		},
		"LiveSubscriptions": func(ctx context.Context) error {
			_, err := NewJobRepository(nil, nil).LiveSubscriptions(ctx, "", 10)
			return err
		},
		"PIICandidates": func(ctx context.Context) error {
			_, err := NewJobRepository(nil, nil).PIICandidates(ctx, now, now, 10)
			return err
		},
		"ExpiredIdem": func(ctx context.Context) error {
			_, err := NewJobRepository(nil, nil).ExpiredIdempotencyTenants(ctx, now, 10)
			return err
		},
		"Snapshot": func(ctx context.Context) error {
			_, err := NewSnapshotRepository(nil).MetricsSnapshot(ctx, now)
			return err
		},
		"Refunds": func(ctx context.Context) error {
			_, err := NewMoneyRepository(nil).RefundsForInvoice(ctx, tid)
			return err
		},
		"Disputes": func(ctx context.Context) error { _, err := NewMoneyRepository(nil).Disputes(ctx, tid); return err },
		"DueDowngrades": func(ctx context.Context) error {
			_, err := NewPlanChangeRepository(nil, nil).DueDowngrades(ctx, now, 10)
			return err
		},
		"BillingSubs": func(ctx context.Context) error {
			_, err := NewReconRepository(nil).BillingSubscriptions(ctx, "stripe")
			return err
		},
		"KnownIDs": func(ctx context.Context) error {
			_, err := NewReconRepository(nil).KnownIDs(ctx, "stripe", "invoices", nil)
			return err
		},
		"UnknownCust": func(ctx context.Context) error {
			_, err := NewReconRepository(nil).UnknownCustomerEvents(ctx, "stripe", now, now)
			return err
		},
		"SeatPurchases": func(ctx context.Context) error {
			_, err := NewSeatPurchaseRepository(nil).List(ctx, tid, "", 10)
			return err
		},
		"SchedulesDue": func(ctx context.Context) error {
			_, err := NewScheduleRepository(nil, nil).Due(ctx, now, 10)
			return err
		},
		"History": func(ctx context.Context) error {
			_, err := NewSubscriptionRepository(nil).History(ctx, tid, 5)
			return err
		},
		"LedgerClaim": func(ctx context.Context) error {
			_, err := NewLedgerRepository(nil).Claim(ctx, 10, time.Minute)
			return err
		},
		"LedgerExcerpt": func(ctx context.Context) error { _, err := NewLedgerRepository(nil).Excerpt(ctx, tid, 10); return err },
	} {
		wantBoom(t, name, call(in(&fakeTx{})))
	}
}

// A row that fails to scan is returned, never skipped.
func TestScanErrorsPropagate(t *testing.T) {
	now := time.Now()
	wantBoom(t, "OverdueSagas collect", func() error {
		_, _, err := NewJobRepository(nil, nil).OverdueSagas(in(&fakeTx{query: []func() (pgcommon.Rows, error){badRows()}}), now)
		return err
	}())
	wantBoom(t, "BillingSubscriptions", func() error {
		_, err := NewReconRepository(nil).BillingSubscriptions(in(&fakeTx{query: []func() (pgcommon.Rows, error){badRows()}}), "stripe")
		return err
	}())
	wantBoom(t, "History", func() error {
		_, err := NewSubscriptionRepository(nil).History(in(&fakeTx{query: []func() (pgcommon.Rows, error){badRows()}}), tid, 0)
		return err
	}())
	wantBoom(t, "LedgerClaim", func() error {
		_, err := NewLedgerRepository(nil).Claim(in(&fakeTx{query: []func() (pgcommon.Rows, error){badRows()}}), 1, time.Minute)
		return err
	}())
	wantBoom(t, "collect", func() error {
		_, err := collect(&fakeRows{vals: [][]any{{"a"}}, scanErr: errBoom}, scanOne[string])
		return err
	}())

	// MetricsSnapshot: a group whose row fails; then the subscription rows.
	snap := func(f *fakeTx) error { _, err := NewSnapshotRepository(nil).MetricsSnapshot(in(f), now); return err }
	wantBoom(t, "snapshot group scan", snap(&fakeTx{query: []func() (pgcommon.Rows, error){badRows()}}))
	ok5 := []func() (pgcommon.Rows, error){rows(), rows(), rows(), rows(), rows()}
	wantBoom(t, "snapshot overdue", snap(&fakeTx{query: ok5}))
	wantBoom(t, "snapshot subscriptions query", snap(&fakeTx{query: ok5, row: []pgcommon.Row{fakeRow{vals: []any{3}}}}))
	wantBoom(t, "snapshot subscriptions scan", snap(&fakeTx{query: append(ok5, badRows()), row: []pgcommon.Row{fakeRow{vals: []any{3}}}}))
}

// KnownIDs marks each returned id.
func TestKnownIDsMarksReturnedIDs(t *testing.T) {
	got, err := NewReconRepository(nil).KnownIDs(in(&fakeTx{query: []func() (pgcommon.Rows, error){rows([]any{"evt_1"}, []any{"evt_2"})}}), "stripe", "events", []string{"evt_1", "evt_2", "evt_3"})
	if err != nil || !got["evt_1"] || !got["evt_2"] || got["evt_3"] {
		t.Fatalf("known %v %v", got, err)
	}
}

// No rows: each lookup answers "not found" in its own terms.
func TestNoRowsAnswers(t *testing.T) {
	nr := func() *fakeTx { return &fakeTx{row: []pgcommon.Row{noRows, noRows}} }
	now := time.Now()
	if _, found, err := NewAdminRepository(nil).Certification(in(nr()), tid); found || err != nil {
		t.Errorf("Certification: %v %v", found, err)
	}
	_, err := NewAdminRepository(nil).ReviewCertification(in(nr()), tid, domain.CertificationStatus("passed"), tid, "r", now)
	wantCode(t, "ReviewCertification", err, domain.ErrCertificationNotPending)
	_, err = NewAdminRepository(nil).RevokeCertification(in(nr()), tid, tid, "r", now)
	wantCode(t, "RevokeCertification", err, domain.ErrCertificationNotActive)
	if _, found, err := NewInvoiceRepository(nil).Get(in(nr()), tid); found || err != nil {
		t.Errorf("Invoice Get: %v %v", found, err)
	}
	if _, found, err := NewInvoiceRepository(nil).ByProvider(in(nr()), "stripe", "in_1"); found || err != nil {
		t.Errorf("Invoice ByProvider: %v %v", found, err)
	}
	if _, found, err := NewInvoiceRepository(nil).ConversionInvoice(in(nr()), tid); found || err != nil {
		t.Errorf("ConversionInvoice: %v %v", found, err)
	}
	if id, err := NewInvoiceRepository(nil).OldestOpen(in(nr()), tid, "stripe"); id != "" || err != nil {
		t.Errorf("OldestOpen: %q %v", id, err)
	}
	if _, found, err := NewMoneyRepository(nil).RefundByProvider(in(nr()), "re_1"); found || err != nil {
		t.Errorf("RefundByProvider: %v %v", found, err)
	}
	if _, found, err := NewMoneyRepository(nil).Dispute(in(nr()), tid); found || err != nil {
		t.Errorf("Dispute: %v %v", found, err)
	}
	if _, _, found, err := NewReconRepository(nil).Get(in(nr()), tid); found || err != nil {
		t.Errorf("Recon Get: %v %v", found, err)
	}
	_, err = NewSubscriptionRepository(nil).Get(in(nr()), tid)
	wantCode(t, "Subscription Get", err, domain.ErrSubscriptionNotFound)
	_, err = NewSubscriptionRepository(nil).Item(in(nr()), tid)
	wantCode(t, "Subscription Item", err, domain.ErrSubscriptionNotFound)
	_, err = NewSubscriptionRepository(nil).Transition(in(nr()), tid, 1, domain.StatusActive, func(*domain.Subscription) {})
	wantCode(t, "Transition", err, domain.ErrSubscriptionNotFound)
	_, err = NewSubscriptionRepository(nil).Update(in(nr()), tid, 1, func(*domain.Subscription) {})
	wantCode(t, "Update", err, domain.ErrSubscriptionNotFound)
	if _, found, err := NewSubscriptionRepository(nil).ByProvider(in(nr()), "stripe", "sub_1"); found || err != nil {
		t.Errorf("ByProvider: %v %v", found, err)
	}
	_, err = NewPriceRepository(nil).Get(in(nr()), tid)
	wantCode(t, "Price Get", err, domain.ErrPriceNotAvailable)
	_, err = NewLedgerRepository(nil).Get(in(nr()), "stripe", "evt_1")
	wantCode(t, "Ledger Get", err, domain.ErrLedgerEventNotFound)
	// UpdateContact: version mismatch on an account that vanished.
	_, err = NewAccountRepository(nil).UpdateContact(in(nr()), tid, 1, nil, nil, nil, nil, nil)
	wantCode(t, "UpdateContact", err, domain.ErrSubscriptionNotFound)
}

// A failing single-row read is returned as is.
func TestRowErrorsPropagate(t *testing.T) {
	f := func(rs ...pgcommon.Row) context.Context { return in(&fakeTx{row: rs}) }
	_, err := NewAccountRepository(nil).UpdateContact(f(noRows), tid, 1, nil, nil, nil, nil, nil)
	wantBoom(t, "UpdateContact current version", err)
	_, _, err = NewInvoiceRepository(nil).ByProvider(f(), "stripe", "in_1")
	wantBoom(t, "Invoice ByProvider", err)
	_, _, err = NewInvoiceRepository(nil).ConversionInvoice(f(), tid)
	wantBoom(t, "ConversionInvoice", err)
	_, err = NewSubscriptionRepository(nil).Transition(f(), tid, 1, domain.StatusActive, func(*domain.Subscription) {})
	wantBoom(t, "Transition", err)
	_, err = NewSubscriptionRepository(nil).Update(f(), tid, 1, func(*domain.Subscription) {})
	wantBoom(t, "Update", err)
	_, _, _, err = NewReconRepository(nil).Get(f(), tid)
	wantBoom(t, "Recon Get run", err)
	_, _, _, err = NewReconRepository(nil).Get(in(&fakeTx{row: []pgcommon.Row{fakeRow{}}}), tid)
	wantBoom(t, "Recon Get findings", err)
}

// A saga state outside the machine is refused before any write.
func TestSagaUpdateRefusesUnknownState(t *testing.T) {
	wantCode(t, "saga state", NewSagaRepository(nil, nil).Update(in(&fakeTx{}), domain.ConversionSaga{State: "bogus"}), domain.ErrInternal)
}

// Mirror: optional fields are stored as NULL when empty; an older provider
// version leaves the row unchanged and returns the stored id.
func TestInvoiceMirrorPaths(t *testing.T) {
	v := port.InvoiceView{Ref: port.InvoiceRef{ProviderInvoiceID: "in_1"}, Number: "INV-1", HostedURL: "https://pay.example/in_1"}
	id, err := NewInvoiceRepository(nil).Mirror(in(&fakeTx{row: []pgcommon.Row{fakeRow{vals: []any{"inv-new"}}}}), tid, "stripe", v, time.Now())
	if err != nil || id != "inv-new" {
		t.Fatalf("upsert: %q %v", id, err)
	}
	id, err = NewInvoiceRepository(nil).Mirror(in(&fakeTx{row: []pgcommon.Row{noRows, fakeRow{vals: []any{"inv-stored"}}}}), tid, "stripe", port.InvoiceView{}, time.Now())
	if err != nil || id != "inv-stored" {
		t.Fatalf("older version: %q %v", id, err)
	}
}

// Nested runners join the transaction already in ctx.
func TestNestedRunnersJoin(t *testing.T) {
	f := &fakeTx{}
	ctx := in(f)
	var seen pgcommon.Tx
	if err := NewTxRunner(nil).RunInTenantTx(ctx, tid, func(ctx context.Context) error { seen, _ = txFrom(ctx); return nil }); err != nil || seen != f {
		t.Errorf("tenant tx: joined=%v %v", seen == f, err)
	}
	seen = nil
	if err := NewSystemTxRunner(nil).RunInSystemTx(ctx, func(ctx context.Context) error { seen, _ = txFrom(ctx); return nil }); err != nil || seen != f {
		t.Errorf("system tx: joined=%v %v", seen == f, err)
	}
}

func TestMapDBErrorCanceled(t *testing.T) {
	wantCode(t, "canceled", mapDBError(context.Canceled), domain.ErrRequestTimeout)
}

// Outside a transaction the tenant lock, audit writer and enqueuer refuse.
func TestOutsideTransactionRefused(t *testing.T) {
	ctx := context.Background()
	wantCode(t, "LockTenant", TenantLocker{}.LockTenant(ctx, tid), domain.ErrInternal)
	wantCode(t, "Audit", (&AuditWriter{}).Write(ctx, port.AuditEntry{TenantID: tid}), domain.ErrInternal)
}

func TestAuditWriterRejectsBadIdentifiers(t *testing.T) {
	ctx := in(&fakeTx{})
	w := &AuditWriter{}
	wantCode(t, "tenant", w.Write(ctx, port.AuditEntry{TenantID: "nope"}), domain.ErrInternal)
	wantCode(t, "actor", w.Write(ctx, port.AuditEntry{TenantID: tid, ActorID: "nope"}), domain.ErrInternal)
	if err := w.Write(ctx, port.AuditEntry{TenantID: tid, Metadata: map[string]any{"f": func() {}}}); err == nil {
		t.Error("unencodable metadata accepted")
	}
}

func TestInboxRunnerRefusals(t *testing.T) {
	if _, err := NewInboxRunner(nil, "tenant-billing"); err == nil {
		t.Error("nil pool accepted")
	}
	r, err := NewInboxRunner(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RunOnce(context.Background(), "nope", "e", "T", tid, func(context.Context) error { return nil }); err == nil {
		t.Error("unknown consumer accepted")
	}
}
