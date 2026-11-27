package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	pgdomain "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

type txKey struct{}

// withTx stores the transaction in ctx for repositories.
func withTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

func txFrom(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	return tx, ok
}

// writeRetry retries deadlocks and serialization failures (§9.1).
var writeRetry = pgcommon.RetryOptions{
	MaxAttempts: 3, InitialWait: 10 * time.Millisecond, MaxWait: 500 * time.Millisecond, Multiplier: 2, JitterFraction: 0.25,
}

// TxRunner is the RLS-bound transaction runner on the app pool (billing_app).
type TxRunner struct{ pool *pgcommon.Pool }

var _ port.TxRunner = (*TxRunner)(nil)

// NewTxRunner wraps the app pool. The pool must be built with
// GUCProvider = pgcommon.GUCSetFromContext (AppPoolConfig).
func NewTxRunner(appPool *pgcommon.Pool) *TxRunner { return &TxRunner{pool: appPool} }

// RunInTenantTx binds app.tenant_id for this transaction only (BIL-RLS-2)
// and runs fn. A nested call joins the outer transaction.
func (r *TxRunner) RunInTenantTx(ctx context.Context, tenantID string, fn func(ctx context.Context) error) error {
	if tenantID == "" {
		return domain.NewError(domain.ErrInternal, "tenant transaction without a tenant id")
	}
	if _, ok := txFrom(ctx); ok {
		return fn(ctx)
	}
	ctx = pgcommon.WithGUCSet(ctx, pgdomain.GUCSet{TenantID: tenantID})
	return mapDBError(pgcommon.RunInTxWithRetryOpts(ctx, r.pool, pgx.TxOptions{}, writeRetry, func(ctx context.Context, tx pgx.Tx) error {
		return fn(withTx(ctx, tx))
	}))
}

// querier is what repositories run statements on.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// inTx runs fn on the transaction in ctx, or in a new transaction on pool.
// Tenant repositories are always called inside RunInTenantTx; the fallback
// serves the system pool, which binds no tenant.
func inTx(ctx context.Context, pool *pgcommon.Pool, fn func(q querier) error) error {
	if tx, ok := txFrom(ctx); ok {
		return fn(tx)
	}
	return mapDBError(pgcommon.RunInTx(ctx, pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return fn(tx)
	}))
}

// mapDBError turns availability and contention failures into the §17
// codes, and leaves domain errors and SQL integrity errors unchanged for the
// caller to classify.
func mapDBError(err error) error {
	if err == nil {
		return nil
	}
	var de *domain.DomainError
	if errors.As(err, &de) {
		return err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled),
		pgcommon.IsQueryCanceled(err), pgcommon.IsLockNotAvailable(err):
		return &domain.DomainError{Code: domain.ErrRequestTimeout, Message: "database statement or request timeout", Cause: err}
	case pgcommon.IsDeadlock(err), pgcommon.IsSerializationFailure(err):
		return &domain.DomainError{Code: domain.ErrConcurrentUpdate, Message: "concurrent update; retry the request", Cause: err}
	case isDBUnavailable(err):
		return &domain.DomainError{Code: domain.ErrDBUnavailable, Message: "database unavailable", Cause: err}
	}
	return err
}

func isDBUnavailable(err error) bool {
	var connectErr *pgconn.ConnectError
	return pgcommon.IsConnectionException(err) || pgcommon.IsInsufficientResources(err) ||
		pgcommon.IsOperatorIntervention(err) || pgcommon.IsSystemError(err) || pgcommon.IsPoolClosed(err) ||
		errors.As(err, &connectErr)
}

// sqlState returns the SQLSTATE of err, or "".
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// subscriptionNotFound is the 404 for a missing account, subscription or item:
// a tenant without them has no subscription (§17 SUBSCRIPTION_NOT_FOUND).
func subscriptionNotFound(what, id string) error {
	return domain.NewError(domain.ErrSubscriptionNotFound, fmt.Sprintf("%s %s not found", what, id))
}
