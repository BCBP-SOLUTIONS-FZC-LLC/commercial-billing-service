package app

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// NotImplementedError marks a route whose use case lands in a later build
// phase (implementation prompt, Phase 5). The HTTP adapter answers 501. It
// is a build-time marker, never part of the published §17 catalogue.
type NotImplementedError struct {
	Route string
	Phase string
}

func (e *NotImplementedError) Error() string {
	return fmt.Sprintf("%s is implemented in phase %s", e.Route, e.Phase)
}

func notImplemented(route, phase string) error {
	return &NotImplementedError{Route: route, Phase: phase}
}

// Unimplemented answers every API method with NotImplementedError. Services
// embed it and override what they implement, so each route reports the
// phase that delivers it until then.
type Unimplemented struct{}

var (
	_ TenantAPI   = Unimplemented{}
	_ InternalAPI = Unimplemented{}
	_ OperatorAPI = Unimplemented{}
)

// GetSubscription is B-P1.
func (Unimplemented) GetSubscription(_ context.Context, _ Caller, _ string) (SubscriptionView, error) {
	return SubscriptionView{}, notImplemented("B-P1", "6")
}

// StartCheckout is B-P2.
func (Unimplemented) StartCheckout(_ context.Context, _ Caller, _ string, _ CheckoutRequest) (CheckoutResult, error) {
	return CheckoutResult{}, notImplemented("B-P2", "6")
}

// PreviewPlanChange is B-P3.
func (Unimplemented) PreviewPlanChange(_ context.Context, _ Caller, _ string, _ PlanChangePreviewRequest) (PlanChangePreview, error) {
	return PlanChangePreview{}, notImplemented("B-P3", "6")
}

// ChangePlan is B-P4.
func (Unimplemented) ChangePlan(_ context.Context, _ Caller, _ string, _ PlanChangeRequest) (PlanChangeResult, error) {
	return PlanChangeResult{}, notImplemented("B-P4", "6")
}

// CancelPlanChange is B-P5.
func (Unimplemented) CancelPlanChange(_ context.Context, _ Caller, _ string) error {
	return notImplemented("B-P5", "6")
}

// PreviewSeats is B-P6.
func (Unimplemented) PreviewSeats(_ context.Context, _ Caller, _ string, _ SeatPreviewRequest) (SeatPreview, error) {
	return SeatPreview{}, notImplemented("B-P6", "6")
}

// PurchaseSeats is B-P7.
func (Unimplemented) PurchaseSeats(_ context.Context, _ Caller, _ string, _ SeatPurchaseRequest) (PurchaseView, error) {
	return PurchaseView{}, notImplemented("B-P7", "6")
}

// GetSeatPurchase is B-P8.
func (Unimplemented) GetSeatPurchase(_ context.Context, _ Caller, _, _ string) (PurchaseView, error) {
	return PurchaseView{}, notImplemented("B-P8", "6")
}

// ListSeatPurchases is B-P9.
func (Unimplemented) ListSeatPurchases(_ context.Context, _ Caller, _ string, _ Page) (List[PurchaseView], error) {
	return List[PurchaseView]{}, notImplemented("B-P9", "6")
}

// ScheduleReduction is B-P10.
func (Unimplemented) ScheduleReduction(_ context.Context, _ Caller, _ string, _ ReductionRequest) (ScheduleView, error) {
	return ScheduleView{}, notImplemented("B-P10", "6")
}

// CancelReduction is B-P11.
func (Unimplemented) CancelReduction(_ context.Context, _ Caller, _ string) error {
	return notImplemented("B-P11", "6")
}

// CancelSubscription is B-P12.
func (Unimplemented) CancelSubscription(_ context.Context, _ Caller, _ string, _ CancelRequest) (SubscriptionView, error) {
	return SubscriptionView{}, notImplemented("B-P12", "6")
}

// ResumeSubscription is B-P13.
func (Unimplemented) ResumeSubscription(_ context.Context, _ Caller, _ string, _ ResumeRequest) (SubscriptionView, error) {
	return SubscriptionView{}, notImplemented("B-P13", "6")
}

// Reactivate is B-P14.
func (Unimplemented) Reactivate(_ context.Context, _ Caller, _ string, _ ReactivateRequest) (ReactivationResult, error) {
	return ReactivationResult{}, notImplemented("B-P14", "6")
}

// ListPaymentMethods is B-P15.
func (Unimplemented) ListPaymentMethods(_ context.Context, _ Caller, _ string) (PaymentMethodList, error) {
	return PaymentMethodList{}, notImplemented("B-P15", "6")
}

// CreateSetupSession is B-P16.
func (Unimplemented) CreateSetupSession(_ context.Context, _ Caller, _ string) (HostedSession, error) {
	return HostedSession{}, notImplemented("B-P16", "6")
}

// ListInvoices is B-P17.
func (Unimplemented) ListInvoices(_ context.Context, _ Caller, _ string, _ InvoiceFilter) (List[InvoiceView], error) {
	return List[InvoiceView]{}, notImplemented("B-P17", "6")
}

// InvoiceDownloadURL is B-P18.
func (Unimplemented) InvoiceDownloadURL(_ context.Context, _ Caller, _, _ string) (string, error) {
	return "", notImplemented("B-P18", "6")
}

// ListTokenPacks is B-P19.
func (Unimplemented) ListTokenPacks(_ context.Context, _ Caller, _ string) (TokenPackCatalog, error) {
	return TokenPackCatalog{}, notImplemented("B-P19", "6")
}

// PurchaseTokenAddon is B-P20.
func (Unimplemented) PurchaseTokenAddon(_ context.Context, _ Caller, _ string, _ AddonPurchaseRequest) (PurchaseView, error) {
	return PurchaseView{}, notImplemented("B-P20", "6")
}

// GetAddonPurchase is B-P21.
func (Unimplemented) GetAddonPurchase(_ context.Context, _ Caller, _, _ string) (PurchaseView, error) {
	return PurchaseView{}, notImplemented("B-P21", "6")
}

// ListAddonPurchases is B-P22.
func (Unimplemented) ListAddonPurchases(_ context.Context, _ Caller, _ string, _ Page) (List[PurchaseView], error) {
	return List[PurchaseView]{}, notImplemented("B-P22", "6")
}

// AddonPaymentSession is B-P23.
func (Unimplemented) AddonPaymentSession(_ context.Context, _ Caller, _, _ string) (HostedSession, error) {
	return HostedSession{}, notImplemented("B-P23", "6")
}

// SubscriptionHistory is B-P24.
func (Unimplemented) SubscriptionHistory(_ context.Context, _ Caller, _ string, _ Page) (List[HistoryView], error) {
	return List[HistoryView]{}, notImplemented("B-P24", "6")
}

// GetBillingContact is B-P25.
func (Unimplemented) GetBillingContact(_ context.Context, _ Caller, _ string) (BillingContact, error) {
	return BillingContact{}, notImplemented("B-P25", "6")
}

// UpdateBillingContact is B-P26.
func (Unimplemented) UpdateBillingContact(_ context.Context, _ Caller, _ string, _ BillingContact) (BillingContact, error) {
	return BillingContact{}, notImplemented("B-P26", "6")
}

// StubConversion is B-O1.
func (Unimplemented) StubConversion(_ context.Context, _ Caller, _ string, _ StubConversionRequest) (StubConversionResult, error) {
	return StubConversionResult{}, notImplemented("B-O1", "6")
}

// IssueAdjustment is B-O2.
func (Unimplemented) IssueAdjustment(_ context.Context, _ Caller, _ string, _ AdjustmentRequest) (AdjustmentResult, error) {
	return AdjustmentResult{}, notImplemented("B-O2", "6")
}

// IssueRefund is B-O3.
func (Unimplemented) IssueRefund(_ context.Context, _ Caller, _ string, _ RefundRequest) (RefundResult, error) {
	return RefundResult{}, notImplemented("B-O3", "6")
}

// ListInvoicesForOperator is B-O4.
func (Unimplemented) ListInvoicesForOperator(_ context.Context, _ Caller, _ string, _ InvoiceFilter) (List[OperatorInvoiceView], error) {
	return List[OperatorInvoiceView]{}, notImplemented("B-O4", "6")
}

// StartReconciliation is B-O5.
func (Unimplemented) StartReconciliation(_ context.Context, _ Caller, _ ReconciliationRequest) (ReconciliationRunView, error) {
	return ReconciliationRunView{}, notImplemented("B-O5", "7")
}

// GetReconciliationRun is B-O6.
func (Unimplemented) GetReconciliationRun(_ context.Context, _ Caller, _ string) (ReconciliationRunView, error) {
	return ReconciliationRunView{}, notImplemented("B-O6", "7")
}

// GetSubscriptionDetail is B-O7.
func (Unimplemented) GetSubscriptionDetail(_ context.Context, _ Caller, _ string) (OperatorSubscriptionView, error) {
	return OperatorSubscriptionView{}, notImplemented("B-O7", "6")
}

// RevertConversion is B-O8.
func (Unimplemented) RevertConversion(_ context.Context, _ Caller, _ string, _ ReversalRequest) (ReversalResult, error) {
	return ReversalResult{}, notImplemented("B-O8", "6")
}

// SagaAction is B-O9.
func (Unimplemented) SagaAction(_ context.Context, _ Caller, _, _ string, _ SagaActionRequest) (SagaView, error) {
	return SagaView{}, notImplemented("B-O9", "6")
}

// SetCollectionPause is B-O10.
func (Unimplemented) SetCollectionPause(_ context.Context, _ Caller, _ string, _ CollectionPauseRequest) (SubscriptionView, error) {
	return SubscriptionView{}, notImplemented("B-O10", "6")
}

// AddPlanPrice is B-O11.
func (Unimplemented) AddPlanPrice(_ context.Context, _ Caller, _ PlanPriceRequest) (domain.PlanPrice, error) {
	return domain.PlanPrice{}, notImplemented("B-O11", "6")
}

// UpsertTokenPack is B-O12.
func (Unimplemented) UpsertTokenPack(_ context.Context, _ Caller, _ TokenPackRequest) (json.RawMessage, error) {
	return nil, notImplemented("B-O12", "6")
}

// ReplayWebhookEvent is B-O13.
func (Unimplemented) ReplayWebhookEvent(_ context.Context, _ Caller, _, _ string, _ OperatorReason) error {
	return notImplemented("B-O13", "6")
}

// SubmitCertification is B-O14.
func (Unimplemented) SubmitCertification(_ context.Context, _ Caller, _ CertificationRequest) (CertificationView, error) {
	return CertificationView{}, notImplemented("B-O14", "6")
}

// RevokeCertification is B-O15.
func (Unimplemented) RevokeCertification(_ context.Context, _ Caller, _ string, _ OperatorReason) (CertificationView, error) {
	return CertificationView{}, notImplemented("B-O15", "6")
}

// DisposeHeldInvoice is B-O16.
func (Unimplemented) DisposeHeldInvoice(_ context.Context, _ Caller, _, _ string, _ DispositionRequest) (DispositionResult, error) {
	return DispositionResult{}, notImplemented("B-O16", "6")
}

// ReviewCertification is B-O17.
func (Unimplemented) ReviewCertification(_ context.Context, _ Caller, _ string, _ ReviewRequest) (CertificationView, error) {
	return CertificationView{}, notImplemented("B-O17", "6")
}

// AuthorizeAndCharge is B-I1.
func (Unimplemented) AuthorizeAndCharge(_ context.Context, _ Caller, _ AuthorizeChargeRequest) (ChargeResult, error) {
	return ChargeResult{}, notImplemented("B-I1", "6")
}
