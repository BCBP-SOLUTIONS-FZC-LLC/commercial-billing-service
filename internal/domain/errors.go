package domain

import (
	"errors"
	"fmt"
	"maps"
)

// DomainError is a classified failure with its frozen errorCode and HTTP
// status (LLD §17). The HTTP adapter renders it as an RFC 9457 problem;
// Extensions become documented extension members.
type DomainError struct {
	Code       ErrorCode
	Message    string
	Extensions map[string]any
	Cause      error
}

func (e *DomainError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap returns the cause.
func (e *DomainError) Unwrap() error { return e.Cause }

// Status returns the HTTP status for the error code.
func (e *DomainError) Status() int { return e.Code.Status() }

// With returns a copy with an extension member set.
func (e *DomainError) With(key string, value any) *DomainError {
	c := *e
	c.Extensions = make(map[string]any, len(e.Extensions)+1)
	maps.Copy(c.Extensions, e.Extensions)
	c.Extensions[key] = value
	return &c
}

// HasCode reports whether err is (or wraps) a DomainError with code.
func HasCode(err error, code ErrorCode) bool {
	var de *DomainError
	return errors.As(err, &de) && de.Code == code
}

// NewError builds a DomainError.
func NewError(code ErrorCode, message string) *DomainError {
	return &DomainError{Code: code, Message: message}
}

// ErrorCode is an UPPER_SNAKE code, frozen once published (CD-31).
type ErrorCode string

// Status returns the HTTP status of the code (§17), 500 for an unknown code.
func (c ErrorCode) Status() int {
	if s, ok := errorStatus[c]; ok {
		return s
	}
	return 500
}

// Known reports whether the code is in the §17 catalogue.
func (c ErrorCode) Known() bool { _, ok := errorStatus[c]; return ok }

// Error codes, LLD §17.
const (
	ErrValidation                   ErrorCode = "VALIDATION_ERROR"
	ErrTenantMismatch               ErrorCode = "TENANT_MISMATCH"
	ErrInvalidSignature             ErrorCode = "INVALID_SIGNATURE"
	ErrPaymentDeclined              ErrorCode = "PAYMENT_DECLINED"
	ErrPaymentActionRequired        ErrorCode = "PAYMENT_ACTION_REQUIRED"
	ErrInsufficientPermission       ErrorCode = "INSUFFICIENT_PERMISSION"
	ErrInsufficientRole             ErrorCode = "INSUFFICIENT_ROLE"
	ErrSubscriptionNotFound         ErrorCode = "SUBSCRIPTION_NOT_FOUND"
	ErrPurchaseNotFound             ErrorCode = "PURCHASE_NOT_FOUND"
	ErrInvoiceNotFound              ErrorCode = "INVOICE_NOT_FOUND"
	ErrSagaNotFound                 ErrorCode = "SAGA_NOT_FOUND"
	ErrScheduleNotFound             ErrorCode = "SCHEDULE_NOT_FOUND"
	ErrPlanChangeNotFound           ErrorCode = "PLAN_CHANGE_NOT_FOUND"
	ErrRunNotFound                  ErrorCode = "RUN_NOT_FOUND"
	ErrLedgerEventNotFound          ErrorCode = "LEDGER_EVENT_NOT_FOUND"
	ErrCertificationNotFound        ErrorCode = "CERTIFICATION_NOT_FOUND"
	ErrSubscriptionAlreadyExists    ErrorCode = "SUBSCRIPTION_ALREADY_EXISTS"
	ErrSubscriptionRequired         ErrorCode = "SUBSCRIPTION_REQUIRED"
	ErrSubscriptionNotActive        ErrorCode = "SUBSCRIPTION_NOT_ACTIVE"
	ErrSubscriptionNotLapsed        ErrorCode = "SUBSCRIPTION_NOT_LAPSED"
	ErrSubscriptionEnding           ErrorCode = "SUBSCRIPTION_ENDING"
	ErrAccountClosed                ErrorCode = "ACCOUNT_CLOSED"
	ErrCollectionPaused             ErrorCode = "COLLECTION_PAUSED"
	ErrInvoiceNotHeld               ErrorCode = "INVOICE_NOT_HELD"
	ErrPlanChangeBlocked            ErrorCode = "PLAN_CHANGE_BLOCKED"
	ErrPlanChangePending            ErrorCode = "PLAN_CHANGE_PENDING"
	ErrSamePlan                     ErrorCode = "SAME_PLAN"
	ErrTenantNotKnownToIAM          ErrorCode = "TENANT_NOT_KNOWN_TO_IAM"
	ErrSeatPurchaseInProgress       ErrorCode = "SEAT_PURCHASE_IN_PROGRESS"
	ErrPreviewStale                 ErrorCode = "PREVIEW_STALE"
	ErrScheduleBeingApplied         ErrorCode = "SCHEDULE_BEING_APPLIED"
	ErrScheduleAlreadyApplied       ErrorCode = "SCHEDULE_ALREADY_APPLIED"
	ErrAddonNotEligible             ErrorCode = "ADDON_NOT_ELIGIBLE"
	ErrAddonNotAvailable            ErrorCode = "ADDON_NOT_AVAILABLE"
	ErrAddonCapReached              ErrorCode = "ADDON_CAP_REACHED"
	ErrPurchaseNotAwaitingPayment   ErrorCode = "PURCHASE_NOT_AWAITING_PAYMENT"
	ErrProviderOperationUnsupported ErrorCode = "PROVIDER_OPERATION_UNSUPPORTED"
	ErrInvoiceNotFinalised          ErrorCode = "INVOICE_NOT_FINALISED"
	ErrOptimisticLockConflict       ErrorCode = "OPTIMISTIC_LOCK_CONFLICT"
	ErrConcurrentUpdate             ErrorCode = "CONCURRENT_UPDATE"
	ErrRequestInProgress            ErrorCode = "REQUEST_IN_PROGRESS"
	ErrRevertWindowExpired          ErrorCode = "REVERT_WINDOW_EXPIRED"
	ErrSagaAlreadyConfirmed         ErrorCode = "SAGA_ALREADY_CONFIRMED"
	ErrReconciliationRunning        ErrorCode = "RECONCILIATION_RUNNING"
	ErrPriceOverlap                 ErrorCode = "PRICE_OVERLAP"
	ErrCertificationExists          ErrorCode = "CERTIFICATION_EXISTS"
	ErrCertificationNotActive       ErrorCode = "CERTIFICATION_NOT_ACTIVE"
	ErrCertificationNotPending      ErrorCode = "CERTIFICATION_NOT_PENDING"
	ErrChargeExpired                ErrorCode = "CHARGE_EXPIRED"
	ErrPreviewExpired               ErrorCode = "PREVIEW_EXPIRED"
	ErrPayloadTooLarge              ErrorCode = "PAYLOAD_TOO_LARGE"
	ErrUnsupportedMediaType         ErrorCode = "UNSUPPORTED_MEDIA_TYPE"
	ErrIdempotencyKeyReused         ErrorCode = "IDEMPOTENCY_KEY_REUSED"
	ErrPriceNotAvailable            ErrorCode = "PRICE_NOT_AVAILABLE"
	ErrSeatsBelowInUse              ErrorCode = "SEATS_BELOW_IN_USE"
	ErrTargetNotBelowCurrent        ErrorCode = "TARGET_NOT_BELOW_CURRENT"
	ErrTermsNotAccepted             ErrorCode = "TERMS_NOT_ACCEPTED"
	ErrCurrencyMismatch             ErrorCode = "CURRENCY_MISMATCH"
	ErrRefundExceedsPaid            ErrorCode = "REFUND_EXCEEDS_PAID"
	ErrAddonRefundCategory          ErrorCode = "ADDON_REFUND_CATEGORY"
	ErrProviderPriceMismatch        ErrorCode = "PROVIDER_PRICE_MISMATCH"
	ErrSuiteVersionMismatch         ErrorCode = "SUITE_VERSION_MISMATCH"
	ErrCertificationIncomplete      ErrorCode = "CERTIFICATION_INCOMPLETE"
	ErrSelfReviewNotAllowed         ErrorCode = "SELF_REVIEW_NOT_ALLOWED"
	ErrInternal                     ErrorCode = "INTERNAL"
	ErrProviderUnavailable          ErrorCode = "PROVIDER_UNAVAILABLE"
	ErrProviderNotEnabled           ErrorCode = "PROVIDER_NOT_ENABLED"
	ErrIAMUnavailable               ErrorCode = "IAM_UNAVAILABLE"
	ErrEntitlementsUnavailable      ErrorCode = "ENTITLEMENTS_UNAVAILABLE"
	ErrDBUnavailable                ErrorCode = "DB_UNAVAILABLE"
	ErrRequestTimeout               ErrorCode = "REQUEST_TIMEOUT"
)

// errorStatus is the §17 table: code → HTTP status.
var errorStatus = map[ErrorCode]int{
	ErrValidation: 400, ErrTenantMismatch: 400, ErrInvalidSignature: 400,
	ErrPaymentDeclined: 402, ErrPaymentActionRequired: 402,
	ErrInsufficientPermission: 403, ErrInsufficientRole: 403,
	ErrSubscriptionNotFound: 404, ErrPurchaseNotFound: 404, ErrInvoiceNotFound: 404, ErrSagaNotFound: 404,
	ErrScheduleNotFound: 404, ErrPlanChangeNotFound: 404, ErrRunNotFound: 404, ErrLedgerEventNotFound: 404,
	ErrCertificationNotFound:     404,
	ErrSubscriptionAlreadyExists: 409, ErrSubscriptionRequired: 409, ErrSubscriptionNotActive: 409,
	ErrSubscriptionNotLapsed: 409, ErrSubscriptionEnding: 409, ErrAccountClosed: 409, ErrCollectionPaused: 409,
	ErrInvoiceNotHeld: 409, ErrPlanChangeBlocked: 409, ErrPlanChangePending: 409, ErrSamePlan: 409,
	ErrTenantNotKnownToIAM: 409, ErrSeatPurchaseInProgress: 409, ErrPreviewStale: 409,
	ErrScheduleBeingApplied: 409, ErrScheduleAlreadyApplied: 409, ErrAddonNotEligible: 409,
	ErrAddonNotAvailable: 409, ErrAddonCapReached: 409, ErrPurchaseNotAwaitingPayment: 409,
	ErrProviderOperationUnsupported: 409, ErrInvoiceNotFinalised: 409, ErrOptimisticLockConflict: 409,
	ErrConcurrentUpdate: 409, ErrRequestInProgress: 409, ErrRevertWindowExpired: 409,
	ErrSagaAlreadyConfirmed: 409, ErrReconciliationRunning: 409, ErrPriceOverlap: 409,
	ErrCertificationExists: 409, ErrCertificationNotActive: 409, ErrCertificationNotPending: 409,
	ErrChargeExpired: 410, ErrPreviewExpired: 410,
	ErrPayloadTooLarge:      413,
	ErrUnsupportedMediaType: 415,
	ErrIdempotencyKeyReused: 422, ErrPriceNotAvailable: 422, ErrSeatsBelowInUse: 422,
	ErrTargetNotBelowCurrent: 422, ErrTermsNotAccepted: 422, ErrCurrencyMismatch: 422,
	ErrRefundExceedsPaid: 422, ErrAddonRefundCategory: 422, ErrProviderPriceMismatch: 422,
	ErrSuiteVersionMismatch: 422, ErrCertificationIncomplete: 422, ErrSelfReviewNotAllowed: 422,
	ErrInternal:            500,
	ErrProviderUnavailable: 503, ErrProviderNotEnabled: 503, ErrIAMUnavailable: 503,
	ErrEntitlementsUnavailable: 503, ErrDBUnavailable: 503, ErrRequestTimeout: 503,
}

// ErrorCatalogue returns a copy of the §17 table, for tests and docs.
func ErrorCatalogue() map[ErrorCode]int {
	out := make(map[ErrorCode]int, len(errorStatus))
	maps.Copy(out, errorStatus)
	return out
}
