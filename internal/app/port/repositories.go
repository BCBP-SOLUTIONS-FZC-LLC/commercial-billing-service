package port

import (
	"context"
	"errors"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// TxRunner runs work in one database transaction (LLD §3.3, §9.1).
//
// RunInTenantTx binds app.tenant_id transaction-locally on the RLS app pool
// (BIL-RLS-2): every repository call made with the ctx it passes sees only
// that tenant's rows. Contended writes retry on deadlock and serialization
// failure; errors come back as *domain.DomainError where they are
// classifiable (DB_UNAVAILABLE, REQUEST_TIMEOUT, CONCURRENT_UPDATE).
type TxRunner interface {
	RunInTenantTx(ctx context.Context, tenantID string, fn func(ctx context.Context) error) error
}

// SystemTxRunner runs work in one billing_system transaction: global
// operator changes together with their AL-5 entry (BIL-AUD-1).
type SystemTxRunner interface {
	RunInSystemTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// SystemReader is the billing_system (BYPASSRLS) side, used only by the
// paths of §4.3 (webhook pre-resolution, cron tenant listing,
// reconciliation, price book and certification writes). Tenant route
// handlers never receive it (BIL-RLS-3).
type SystemReader interface {
	// TenantForCustomer resolves a provider customer to its tenant (BIL-PAY-3).
	TenantForCustomer(ctx context.Context, provider, providerCustomerID string) (tenantID string, found bool, err error)
}

// AccountRepo stores billing_accounts.
// ErrAccountExists is AccountRepo.Create's answer when the tenant already
// has a billing account (a concurrent first checkout or signup created it).
// Repositories map the storage error to it, so the app layer never inspects
// database errors (BIL-OPS-11).
var ErrAccountExists = errors.New("billing account already exists")

// ErrIdempotencyKeyTaken is a unique violation on a row's (tenant_id,
// idempotency_key) (uq_qap_key, uq_adj_key, uq_ref_key): another row holds
// the client key (§9.2 "one row per key").
var ErrIdempotencyKeyTaken = errors.New("a row already holds this idempotency key")

type AccountRepo interface {
	// Create inserts the account; ErrAccountExists when one exists.
	Create(ctx context.Context, a domain.BillingAccount) error
	Get(ctx context.Context, tenantID string) (domain.BillingAccount, error)
	// UpdateContact sets contact fields with optimistic locking on record_version.
	UpdateContact(ctx context.Context, tenantID string, expectedVersion int64, name, email, company, country, taxID *string) (domain.BillingAccount, error)
	SetDefaultPM(ctx context.Context, tenantID string, set bool) error
	// MinimiseContact nulls the billing-contact name and email and sets
	// pii_minimised_at (§15.3); nothing else changes, no row is deleted.
	MinimiseContact(ctx context.Context, tenantID string, at time.Time) error
	// ReturnToTrial records that RP-4 returned the tenant to a trial (B-O8,
	// §8.2 step 3): iam_lifecycle 'trial', no suspension source, and
	// iam_last_event_at advanced to at so that IAM lifecycle events stamped
	// before the reversal are skipped (BIL-EVT-3). An offboarded account is
	// left unchanged.
	ReturnToTrial(ctx context.Context, tenantID string, at time.Time) error
}

// SubscriptionRepo stores subscriptions, their seat item and history.
type SubscriptionRepo interface {
	Create(ctx context.Context, s domain.Subscription, item domain.SubscriptionItem) error
	Get(ctx context.Context, billingSubscriptionID string) (domain.Subscription, error)
	// Live returns the tenant's non-ended subscription (BIL-SUB-3).
	Live(ctx context.Context, tenantID string) (domain.Subscription, bool, error)
	Item(ctx context.Context, billingSubscriptionID string) (domain.SubscriptionItem, error)
	// Transition moves the status along §8.14 (BIL-SUB-4) with optimistic
	// locking; mutate sets the timestamps and fields the move requires.
	Transition(ctx context.Context, billingSubscriptionID string, expectedVersion int64, to domain.SubscriptionStatus,
		mutate func(*domain.Subscription)) (domain.Subscription, error)
	AppendHistory(ctx context.Context, h domain.StatusHistoryEntry) error
	Update(ctx context.Context, billingSubscriptionID string, expectedVersion int64, mutate func(*domain.Subscription)) (domain.Subscription, error)
	ByProvider(ctx context.Context, provider, providerSubscriptionID string) (domain.Subscription, bool, error)
	Latest(ctx context.Context, tenantID string) (domain.Subscription, bool, error)
	SetItem(ctx context.Context, it domain.SubscriptionItem) error
	History(ctx context.Context, billingSubscriptionID string, limit int) ([]domain.StatusHistoryEntry, error)
}

// PriceRepo reads the price book (§4.2.2).
type PriceRepo interface {
	// PriceAt returns the row effective at instant for the key.
	PriceAt(ctx context.Context, plan domain.PlanCode, interval domain.BillingInterval, currency, provider string, at time.Time) (domain.PlanPrice, bool, error)
	Get(ctx context.Context, priceID string) (domain.PlanPrice, error)
}

// CertificationRepo reads provider certifications for the registry (§3.5).
type CertificationRepo interface {
	// Enabling returns the passed or exempt record for the exact adapter and
	// suite version, if any (BIL-PROV-1).
	Enabling(ctx context.Context, provider, adapterVersion, suiteVersion string) (domain.Certification, bool, error)
}

// IdempotencyRepo implements §9.2's records.
type IdempotencyRepo interface {
	// Begin inserts an in_flight record or returns the existing one.
	Begin(ctx context.Context, r domain.IdempotencyRecord) (existing *domain.IdempotencyRecord, err error)
	Complete(ctx context.Context, tenantID, scope, key string, statusCode int, body []byte) error
	Release(ctx context.Context, tenantID, scope, key string) error
	// Reclaim restarts a record whose lock expired with the same request
	// hash, or one past expires_at (a new request, BIL-IDEM-3);
	// reclaimed=false when neither holds (another request owns it).
	Reclaim(ctx context.Context, r domain.IdempotencyRecord) (reclaimed bool, err error)
	// Unlock keeps an in_flight record but lets the next request with the
	// same key run it now (B-I1 after PAYMENT_ACTION_REQUIRED, §5.4.2).
	Unlock(ctx context.Context, tenantID, scope, key string) error
}

// LedgerRepo stores provider callbacks before any domain write (BIL-PAY-1).
type LedgerRepo interface {
	// Insert records a callback; inserted is false for a redelivery.
	Insert(ctx context.Context, e domain.LedgerEntry) (inserted bool, err error)
	Get(ctx context.Context, provider, providerEventID string) (domain.LedgerEntry, error)
	// Claim moves up to limit due rows to processing for claimFor, one
	// tenant's rows in provider_created_at order (§8.15, BIL-PAY-5).
	Claim(ctx context.Context, limit int, claimFor time.Duration) ([]domain.LedgerEntry, error)
	// Finish records the outcome. nextAttempt is set only for a retry.
	Finish(ctx context.Context, provider, providerEventID string, status domain.LedgerStatus, outcome string, nextAttempt *time.Time) error
}

// CheckoutRepo stores hosted checkout sessions (§4.2.7).
type CheckoutRepo interface {
	Insert(ctx context.Context, c domain.CheckoutSession) error
	Open(ctx context.Context, tenantID, purpose string) (domain.CheckoutSession, bool, error)
	ByProviderSession(ctx context.Context, providerSessionID string) (domain.CheckoutSession, bool, error)
	SetProviderSession(ctx context.Context, sessionID, providerSessionID string, expiresAt time.Time) error
	SetStatus(ctx context.Context, sessionID, status string) error
	ExpireAll(ctx context.Context, tenantID string) error
	MarkDefaultApplied(ctx context.Context, sessionID string) error
	GetSession(ctx context.Context, sessionID string) (domain.CheckoutSession, bool, error)
}

// SagaRef names a due saga.
type SagaRef struct{ SagaID, TenantID string }

// SagaRepo stores conversion sagas (§4.2.8).
type SagaRepo interface {
	Insert(ctx context.Context, s domain.ConversionSaga) error
	BySubscription(ctx context.Context, billingSubscriptionID string) (domain.ConversionSaga, bool, error)
	Get(ctx context.Context, sagaID string) (domain.ConversionSaga, bool, error)
	Update(ctx context.Context, s domain.ConversionSaga) error
	Due(ctx context.Context, now time.Time, limit int) ([]SagaRef, error)
}

// InvoiceRepo mirrors provider invoices (§4.2.13, §4.2.14).
type InvoiceRepo interface {
	Mirror(ctx context.Context, tenantID, provider string, v InvoiceView, providerUpdatedAt time.Time) (invoiceID string, err error)
	Link(ctx context.Context, invoiceID, tenantID, refType, refID string) error
	OldestOpen(ctx context.Context, tenantID, billingSubscriptionID string) (providerInvoiceID string, err error)
	Get(ctx context.Context, invoiceID string) (domain.Invoice, bool, error)
	SetHeld(ctx context.Context, invoiceID string) error
	Dispose(ctx context.Context, invoiceID, action, reason, by string, at time.Time, status string) error
	List(ctx context.Context, tenantID, status, kind, afterID string, withDrafts bool, limit int) ([]InvoiceRow, error)
	ByProvider(ctx context.Context, provider, providerInvoiceID string) (domain.Invoice, bool, error)
	ConversionInvoice(ctx context.Context, tenantID string) (domain.Invoice, bool, error)
	AddRefunded(ctx context.Context, invoiceID string, amountMinor int64) error
}

// MoneyRepo stores adjustments, refunds and disputes (§4.2.16–§4.2.18).
type MoneyRepo interface {
	InsertAdjustment(ctx context.Context, a domain.Adjustment) error
	SetAdjustmentRef(ctx context.Context, adjustmentID, providerRef string) error
	// AdjustmentByKey / RefundByKey are the rows a B-O2 / B-O3 key created (locked).
	AdjustmentByKey(ctx context.Context, tenantID, key string) (domain.Adjustment, bool, error)
	InsertRefund(ctx context.Context, r domain.Refund) error
	RefundByKey(ctx context.Context, tenantID, key string) (domain.Refund, bool, error)
	RefundByProvider(ctx context.Context, providerRefundID string) (domain.Refund, bool, error)
	RefundsForInvoice(ctx context.Context, invoiceID string) ([]domain.Refund, error)
	UpdateRefund(ctx context.Context, r domain.Refund) error
	UpsertDispute(ctx context.Context, d domain.Dispute) (domain.Dispute, error)
	Dispute(ctx context.Context, disputeID string) (domain.Dispute, bool, error)
	DecideDispute(ctx context.Context, disputeID, decision, by string) error
	Disputes(ctx context.Context, tenantID string) ([]domain.Dispute, error)
}

// LedgerExcerpt is one ledger row for an operator, without its payload.
type LedgerExcerpt struct {
	ProviderEventID, EventType, Status string
	Outcome                            *string
	ReceivedAt                         time.Time
}

// LedgerReader lists a tenant's ledger rows (system pool, B-O7).
type LedgerReader interface {
	Excerpt(ctx context.Context, tenantID string, limit int) ([]LedgerExcerpt, error)
}

// InvoiceRow is one invoice of a list (B-P17, B-O4).
type InvoiceRow struct {
	InvoiceID, Kind, Status, Currency                    string
	Number, HostedURL                                    *string
	AmountDueMinor, AmountPaidMinor, AmountRefundedMinor int64
	PeriodStart, PeriodEnd, IssuedAt, PaidAt             *time.Time
	HeldForReview                                        bool
}

// FollowUp is one lifecycle follow-up a tenant needs (§8.8.4, §8.10).
type FollowUp struct {
	TenantID, BillingSubscriptionID, Action string // pause | resume | close | default_pm
	SessionID                               string // default_pm: the completed setup session
}

// FollowUpRepo finds tenants whose IAM lifecycle needs a provider action
// (system pool, across tenants).
type FollowUpRepo interface {
	Due(ctx context.Context, autoPause bool, limit int) ([]FollowUp, error)
}

// PlanChangeRepo stores plan change requests (§4.2.9).
type PlanChangeRepo interface {
	Insert(ctx context.Context, c domain.PlanChangeRequest) error
	Open(ctx context.Context, tenantID string) (domain.PlanChangeRequest, bool, error)
	ByProviderInvoice(ctx context.Context, tenantID, providerInvoiceID string) (domain.PlanChangeRequest, bool, error)
	Update(ctx context.Context, c domain.PlanChangeRequest) error
	DueDowngrades(ctx context.Context, until time.Time, limit int) ([]domain.PlanChangeRequest, error)
}

// PreviewRepo keeps short-lived previews (B-P3, B-P6) for any replica.
type PreviewRepo interface {
	Save(ctx context.Context, tenantID, previewID string, body []byte, expiresAt time.Time) error
	Load(ctx context.Context, tenantID, previewID string) (body []byte, expiresAt time.Time, found bool, err error)
}

// SeatPurchaseRepo stores seat purchases (§4.2.10).
type SeatPurchaseRepo interface {
	Insert(ctx context.Context, p domain.SeatPurchase) error
	Get(ctx context.Context, purchaseID string) (domain.SeatPurchase, bool, error)
	Open(ctx context.Context, billingSubscriptionID string) (domain.SeatPurchase, bool, error)
	ByProviderInvoice(ctx context.Context, tenantID, providerInvoiceID string) (domain.SeatPurchase, bool, error)
	List(ctx context.Context, tenantID, afterID string, limit int) ([]domain.SeatPurchase, error)
	Update(ctx context.Context, p domain.SeatPurchase) error
}

// ScheduleRepo stores scheduled seat reductions (§4.2.11).
type ScheduleRepo interface {
	Insert(ctx context.Context, s domain.SeatChangeSchedule) error
	Open(ctx context.Context, billingSubscriptionID string) (domain.SeatChangeSchedule, bool, error)
	Latest(ctx context.Context, billingSubscriptionID string) (domain.SeatChangeSchedule, bool, error)
	Get(ctx context.Context, scheduleID string) (domain.SeatChangeSchedule, bool, error)
	Update(ctx context.Context, s domain.SeatChangeSchedule) error
	Due(ctx context.Context, until time.Time, limit int) ([]domain.SeatChangeSchedule, error)
}

// AddonRepo stores the token pack catalog and add-on purchases (§4.2.3, §4.2.12).
type AddonRepo interface {
	ActivePacks(ctx context.Context, currency, provider string, at time.Time) ([]domain.TokenPack, error)
	Insert(ctx context.Context, p domain.QuotaAddonPurchase) error
	Get(ctx context.Context, purchaseID string) (domain.QuotaAddonPurchase, bool, error)
	ByProviderInvoice(ctx context.Context, tenantID, providerInvoiceID string) (domain.QuotaAddonPurchase, bool, error)
	// ByIdempotencyKey is the purchase a B-P20 key created (locked).
	ByIdempotencyKey(ctx context.Context, tenantID, key string) (domain.QuotaAddonPurchase, bool, error)
	List(ctx context.Context, tenantID, afterID string, limit int) ([]domain.QuotaAddonPurchase, error)
	Update(ctx context.Context, p domain.QuotaAddonPurchase) error
	ConfirmedTokensSince(ctx context.Context, tenantID, packCode string, since time.Time) (int64, error)
}

// CertificationRun is a B-O14 submission as stored.
type CertificationRun struct {
	Provider, AdapterVersion, SuiteVersion, ProrationMode, CertifiedBy string
	ProviderAPIVersion, ProviderAccountRef                             *string
	Status                                                             domain.CertificationStatus
	Capabilities, Results                                              []byte // JSON
	RunStartedAt, RunFinishedAt                                        time.Time
}

// AdminRepo writes global operator data as billing_system (B-O11–B-O15, B-O17).
type AdminRepo interface {
	AddPlanPrice(ctx context.Context, p domain.PlanPrice) (domain.PlanPrice, error)
	AddTokenPack(ctx context.Context, p domain.TokenPack, by, reason string) error
	ReplayLedger(ctx context.Context, provider, providerEventID, note string) (found bool, err error)
	InsertCertification(ctx context.Context, c CertificationRun) (domain.Certification, error)
	Certification(ctx context.Context, id string) (domain.Certification, bool, error)
	ReviewCertification(ctx context.Context, id string, status domain.CertificationStatus, by, reason string, at time.Time) (domain.Certification, error)
	RevokeCertification(ctx context.Context, id, by, reason string, at time.Time) (domain.Certification, error)
}

// ReconRun is a reconciliation run (§4.2.22).
type ReconRun struct {
	RunID, Provider, StartedBy, Status string
	WindowStart, WindowEnd             time.Time
	AutoRepair                         bool
	Counts                             map[string]int
}

// ReconFinding is one finding (§4.2.22).
type ReconFinding struct {
	TenantID, Kind              string
	BillingValue, ProviderValue []byte // JSON
	AutoRepaired                bool
	RepairNote                  *string
}

// ReconRepo stores reconciliation runs and reads Billing's side (system pool).
type ReconRepo interface {
	Queue(ctx context.Context, r ReconRun) error
	Claim(ctx context.Context, worker string) (ReconRun, bool, error)
	AddFinding(ctx context.Context, runID string, f ReconFinding) error
	Finish(ctx context.Context, runID, status string, counts map[string]int, errText *string) error
	Get(ctx context.Context, runID string) (ReconRun, []ReconFinding, bool, error)
	BillingSubscriptions(ctx context.Context, provider string) (map[string][2]string, error)
	KnownIDs(ctx context.Context, provider, kind string, ids []string) (map[string]bool, error)
	UnknownCustomerEvents(ctx context.Context, provider string, from, to time.Time) ([]string, error)
}
