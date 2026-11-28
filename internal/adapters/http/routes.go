package http

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// The §5.3 catalogue: path, method, idempotency rule and handler of every
// endpoint id. Permissions come from domain.TenantRoutePermission.

const (
	mGet, mPost, mPut, mDel = http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete
	tp                      = tenantPrefix
	op                      = operatorPrefix
)

func route(id, method, path string, idem idemMode, h handlerFactory, extra ...gin.HandlerFunc) routeDef {
	return routeDef{Route: Route{ID: id, Method: method, Path: path, Idem: idem}, handler: h, extra: extra}
}

var tenantRoutes = []routeDef{
	route("B-P1", mGet, tp+"/subscription", idemNone, noBody(200, func(a *APIConfig, c *gin.Context, w app.Caller) (app.SubscriptionView, error) {
		return a.Tenant.GetSubscription(c.Request.Context(), w, c.Param("id"))
	})),
	route("B-P2", mPost, tp+"/subscription/checkout", idemRequired, withBody(201, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.CheckoutRequest) (app.CheckoutResult, error) {
		return a.Tenant.StartCheckout(c.Request.Context(), w, c.Param("id"), r)
	})),
	route("B-P3", mPost, tp+"/subscription/plan-change/preview", idemNone, withBody(200, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.PlanChangePreviewRequest) (app.PlanChangePreview, error) {
		return a.Tenant.PreviewPlanChange(c.Request.Context(), w, c.Param("id"), r)
	})),
	route("B-P4", mPost, tp+"/subscription/plan-change", idemRequired, changePlan),
	route("B-P5", mDel, tp+"/subscription/plan-change", idemOptional, noContent(func(a *APIConfig, c *gin.Context, w app.Caller) error {
		return a.Tenant.CancelPlanChange(c.Request.Context(), w, c.Param("id"))
	})),
	route("B-P6", mPost, tp+"/subscription/seats/preview", idemNone, withBody(200, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.SeatPreviewRequest) (app.SeatPreview, error) {
		return a.Tenant.PreviewSeats(c.Request.Context(), w, c.Param("id"), r)
	})),
	route("B-P7", mPost, tp+"/subscription/seats/purchases", idemRequired, withBody(202, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.SeatPurchaseRequest) (app.PurchaseView, error) {
		return a.Tenant.PurchaseSeats(c.Request.Context(), w, c.Param("id"), r)
	})),
	route("B-P8", mGet, tp+"/subscription/seats/purchases/:purchase_id", idemNone, noBody(200, func(a *APIConfig, c *gin.Context, w app.Caller) (app.PurchaseView, error) {
		id, err := param(c, "purchase_id")
		if err != nil {
			return app.PurchaseView{}, err
		}
		return a.Tenant.GetSeatPurchase(c.Request.Context(), w, c.Param("id"), id)
	})),
	route("B-P9", mGet, tp+"/subscription/seats/purchases", idemNone, noBody(200, func(a *APIConfig, c *gin.Context, w app.Caller) (app.List[app.PurchaseView], error) {
		var p app.Page
		if err := bindQuery(c, &p); err != nil {
			return app.List[app.PurchaseView]{}, err
		}
		return a.Tenant.ListSeatPurchases(c.Request.Context(), w, c.Param("id"), p)
	})),
	route("B-P10", mPut, tp+"/subscription/seats/scheduled-reduction", idemRequired, withBody(200, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.ReductionRequest) (app.ScheduleView, error) {
		return a.Tenant.ScheduleReduction(c.Request.Context(), w, c.Param("id"), r)
	})),
	route("B-P11", mDel, tp+"/subscription/seats/scheduled-reduction", idemOptional, noContent(func(a *APIConfig, c *gin.Context, w app.Caller) error {
		return a.Tenant.CancelReduction(c.Request.Context(), w, c.Param("id"))
	})),
	route("B-P12", mPost, tp+"/subscription/cancel", idemOptional, withBody(200, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.CancelRequest) (app.SubscriptionView, error) {
		return a.Tenant.CancelSubscription(c.Request.Context(), w, c.Param("id"), r)
	})),
	route("B-P13", mPost, tp+"/subscription/resume", idemOptional, withBody(200, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.ResumeRequest) (app.SubscriptionView, error) {
		return a.Tenant.ResumeSubscription(c.Request.Context(), w, c.Param("id"), r)
	})),
	route("B-P14", mPost, tp+"/subscription/reactivate", idemRequired, withBody(201, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.ReactivateRequest) (app.ReactivationResult, error) {
		return a.Tenant.Reactivate(c.Request.Context(), w, c.Param("id"), r)
	})),
	route("B-P15", mGet, tp+"/payment-methods", idemNone, noBody(200, func(a *APIConfig, c *gin.Context, w app.Caller) (app.PaymentMethodList, error) {
		return a.Tenant.ListPaymentMethods(c.Request.Context(), w, c.Param("id"))
	})),
	route("B-P16", mPost, tp+"/payment-methods/setup-session", idemRequired, hostedSession("setup_url", func(a *APIConfig, c *gin.Context, w app.Caller) (app.HostedSession, error) {
		return a.Tenant.CreateSetupSession(c.Request.Context(), w, c.Param("id"))
	})),
	route("B-P17", mGet, tp+"/invoices", idemNone, noBody(200, func(a *APIConfig, c *gin.Context, w app.Caller) (app.List[app.InvoiceView], error) {
		var f app.InvoiceFilter
		if err := bindQuery(c, &f); err != nil {
			return app.List[app.InvoiceView]{}, err
		}
		if f.Status == "draft" { // drafts are operator-only (B-P17)
			return app.List[app.InvoiceView]{Items: []app.InvoiceView{}}, nil
		}
		return a.Tenant.ListInvoices(c.Request.Context(), w, c.Param("id"), f)
	})),
	route("B-P18", mGet, tp+"/invoices/:invoice_id/download", idemNone, downloadInvoice),
	route("B-P19", mGet, tp+"/token-packs", idemNone, noBody(200, func(a *APIConfig, c *gin.Context, w app.Caller) (app.TokenPackCatalog, error) {
		return a.Tenant.ListTokenPacks(c.Request.Context(), w, c.Param("id"))
	})),
	route("B-P20", mPost, tp+"/token-addons/purchases", idemRequired, withBody(202, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.AddonPurchaseRequest) (app.PurchaseView, error) {
		return a.Tenant.PurchaseTokenAddon(c.Request.Context(), w, c.Param("id"), r)
	})),
	route("B-P21", mGet, tp+"/token-addons/purchases/:purchase_id", idemNone, noBody(200, func(a *APIConfig, c *gin.Context, w app.Caller) (app.PurchaseView, error) {
		id, err := param(c, "purchase_id")
		if err != nil {
			return app.PurchaseView{}, err
		}
		return a.Tenant.GetAddonPurchase(c.Request.Context(), w, c.Param("id"), id)
	})),
	route("B-P22", mGet, tp+"/token-addons/purchases", idemNone, noBody(200, func(a *APIConfig, c *gin.Context, w app.Caller) (app.List[app.PurchaseView], error) {
		var p app.Page
		if err := bindQuery(c, &p); err != nil {
			return app.List[app.PurchaseView]{}, err
		}
		return a.Tenant.ListAddonPurchases(c.Request.Context(), w, c.Param("id"), p)
	})),
	route("B-P23", mPost, tp+"/token-addons/purchases/:purchase_id/payment-session", idemRequired, hostedSession("payment_url", func(a *APIConfig, c *gin.Context, w app.Caller) (app.HostedSession, error) {
		id, err := param(c, "purchase_id")
		if err != nil {
			return app.HostedSession{}, err
		}
		return a.Tenant.AddonPaymentSession(c.Request.Context(), w, c.Param("id"), id)
	})),
	route("B-P24", mGet, tp+"/subscription/history", idemNone, noBody(200, func(a *APIConfig, c *gin.Context, w app.Caller) (app.List[app.HistoryView], error) {
		var p app.Page
		if err := bindQuery(c, &p); err != nil {
			return app.List[app.HistoryView]{}, err
		}
		return a.Tenant.SubscriptionHistory(c.Request.Context(), w, c.Param("id"), p)
	})),
	route("B-P25", mGet, tp+"/billing-contact", idemNone, noBody(200, func(a *APIConfig, c *gin.Context, w app.Caller) (app.BillingContact, error) {
		return a.Tenant.GetBillingContact(c.Request.Context(), w, c.Param("id"))
	})),
	route("B-P26", mPut, tp+"/billing-contact", idemOptional, withBody(200, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.BillingContact) (app.BillingContact, error) {
		return a.Tenant.UpdateBillingContact(c.Request.Context(), w, c.Param("id"), r)
	})),
}

// changePlan is B-P4: 202 for an upgrade, 200 for a scheduled downgrade.
var changePlan = handlerFactory{req: typeOf[app.PlanChangeRequest](), res: typeOf[app.PlanChangeResult](), status: http.StatusAccepted, make: changePlanHandler}

func changePlanHandler(a *APIConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		var r app.PlanChangeRequest
		if err := decodeJSON(c, &r, true); err != nil {
			writeError(c, err)
			return
		}
		res, err := a.Tenant.ChangePlan(c.Request.Context(), caller(c), c.Param("id"), r)
		if err != nil {
			writeError(c, err)
			return
		}
		status := http.StatusAccepted
		if res.Direction == "downgrade" {
			status = http.StatusOK
		}
		c.JSON(status, res)
	}
}

// downloadInvoice is B-P18: 302 to the short-lived provider link.
var downloadInvoice = handlerFactory{status: http.StatusFound, make: downloadInvoiceHandler}

func downloadInvoiceHandler(a *APIConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := param(c, "invoice_id")
		if err != nil {
			writeError(c, err)
			return
		}
		url, err := a.Tenant.InvoiceDownloadURL(c.Request.Context(), caller(c), c.Param("id"), id)
		if err != nil {
			writeError(c, err)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.Redirect(http.StatusFound, url)
	}
}

// hostedSession answers 201 {<urlField>: …, expires_at} (B-P16, B-P23). The
// URL comes from the provider adapter only (BIL-API-5).
func hostedSession(urlField string, fn func(a *APIConfig, c *gin.Context, w app.Caller) (app.HostedSession, error)) handlerFactory {
	res := reflect.StructOf([]reflect.StructField{
		{Name: "URL", Type: reflect.TypeFor[string](), Tag: reflect.StructTag(`json:"` + urlField + `"`)},
		{Name: "ExpiresAt", Type: reflect.TypeFor[time.Time](), Tag: `json:"expires_at"`},
	})
	return handlerFactory{req: typeOf[struct{}](), res: res, status: http.StatusCreated, make: func(a *APIConfig) gin.HandlerFunc {
		return func(c *gin.Context) {
			if err := decodeJSON(c, &struct{}{}, true); err != nil {
				writeError(c, err)
				return
			}
			s, err := fn(a, c, caller(c))
			if err != nil {
				writeError(c, err)
				return
			}
			c.JSON(http.StatusCreated, gin.H{urlField: s.URL, "expires_at": s.ExpiresAt})
		}
	}}
}

// internalRoute is B-I1.
var internalRoute = route("B-I1", mPost, "/api/v1/internal/subscriptions/authorize-and-charge", idemRequired,
	handlerFactory{req: typeOf[app.AuthorizeChargeRequest](), res: typeOf[app.ChargeResult](), status: http.StatusCreated, make: authorizeAndCharge})

func authorizeAndCharge(a *APIConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		var r app.AuthorizeChargeRequest
		if err := decodeJSON(c, &r, false); err != nil { // unknown fields tolerated
			writeError(c, err)
			return
		}
		// BIL-AUTH-4 (rev 0.73): B-I1 reads no CD-19 identity, so the
		// system-caller check reads the raw headers: commercial-system is
		// refused even behind the BFF's principal.
		if c.GetHeader("x-user-id") == domain.CommercialSystemPrincipal ||
			slices.Contains(strings.Split(strings.ReplaceAll(c.GetHeader("x-tenant-roles"), " ", ""), ","), domain.RoleCommercialSystem) {
			writeError(c, domain.NewError(domain.ErrInsufficientRole, "commercial-system is not accepted inbound"))
			return
		}
		who := caller(c)
		if !strings.EqualFold(r.TenantID, who.TenantID) { // BIL-API-1
			writeError(c, domain.NewError(domain.ErrTenantMismatch, "tenant_id must equal x-tenant-id"))
			return
		}
		res, err := a.Internal.AuthorizeAndCharge(c.Request.Context(), who, r)
		if err != nil {
			if domain.HasCode(err, domain.ErrPaymentActionRequired) {
				keepInFlight(c)
			}
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, res)
	}
}

var operatorRoutes = []routeDef{
	route("B-O1", mPost, op+"/tenants/:id/conversions", idemRequired, withBody(202, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.StubConversionRequest) (app.StubConversionResult, error) {
		t, err := param(c, "id")
		if err != nil {
			return app.StubConversionResult{}, err
		}
		return a.Operator.StubConversion(c.Request.Context(), w, t, r)
	})),
	route("B-O2", mPost, op+"/tenants/:id/adjustments", idemRequired, withBody(201, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.AdjustmentRequest) (app.AdjustmentResult, error) {
		t, err := param(c, "id")
		if err != nil {
			return app.AdjustmentResult{}, err
		}
		return a.Operator.IssueAdjustment(c.Request.Context(), w, t, r)
	})),
	route("B-O3", mPost, op+"/tenants/:id/refunds", idemRequired, withBody(201, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.RefundRequest) (app.RefundResult, error) {
		t, err := param(c, "id")
		if err != nil {
			return app.RefundResult{}, err
		}
		return a.Operator.IssueRefund(c.Request.Context(), w, t, r)
	})),
	route("B-O4", mGet, op+"/tenants/:id/invoices", idemNone, noBody(200, func(a *APIConfig, c *gin.Context, w app.Caller) (app.List[app.OperatorInvoiceView], error) {
		t, err := param(c, "id")
		if err != nil {
			return app.List[app.OperatorInvoiceView]{}, err
		}
		var f app.InvoiceFilter
		if err := bindQuery(c, &f); err != nil {
			return app.List[app.OperatorInvoiceView]{}, err
		}
		return a.Operator.ListInvoicesForOperator(c.Request.Context(), w, t, f)
	})),
	route("B-O5", mPost, op+"/billing/reconciliation-runs", idemRequired, withBody(202, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.ReconciliationRequest) (app.ReconciliationRunView, error) {
		return a.Operator.StartReconciliation(c.Request.Context(), w, r)
	})),
	route("B-O6", mGet, op+"/billing/reconciliation-runs/:run_id", idemNone, noBody(200, func(a *APIConfig, c *gin.Context, w app.Caller) (app.ReconciliationRunView, error) {
		id, err := param(c, "run_id")
		if err != nil {
			return app.ReconciliationRunView{}, err
		}
		return a.Operator.GetReconciliationRun(c.Request.Context(), w, id)
	})),
	route("B-O7", mGet, op+"/tenants/:id/subscription", idemNone, noBody(200, func(a *APIConfig, c *gin.Context, w app.Caller) (app.OperatorSubscriptionView, error) {
		t, err := param(c, "id")
		if err != nil {
			return app.OperatorSubscriptionView{}, err
		}
		return a.Operator.GetSubscriptionDetail(c.Request.Context(), w, t)
	})),
	route("B-O8", mPost, op+"/tenants/:id/conversion-reversals", idemRequired, withBody(202, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.ReversalRequest) (app.ReversalResult, error) {
		t, err := param(c, "id")
		if err != nil {
			return app.ReversalResult{}, err
		}
		return a.Operator.RevertConversion(c.Request.Context(), w, t, r)
	})),
	route("B-O9", mPost, op+"/tenants/:id/conversion-sagas/:saga_id/actions", idemRequired, withBody(200, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.SagaActionRequest) (app.SagaView, error) {
		t, err := param(c, "id")
		if err != nil {
			return app.SagaView{}, err
		}
		s, err := param(c, "saga_id")
		if err != nil {
			return app.SagaView{}, err
		}
		return a.Operator.SagaAction(c.Request.Context(), w, t, s, r)
	})),
	route("B-O10", mPut, op+"/tenants/:id/collection-pause", idemRequired, withBody(200, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.CollectionPauseRequest) (app.SubscriptionView, error) {
		t, err := param(c, "id")
		if err != nil {
			return app.SubscriptionView{}, err
		}
		return a.Operator.SetCollectionPause(c.Request.Context(), w, t, r)
	})),
	route("B-O11", mPost, op+"/billing/plan-prices", idemRequired, withBody(201, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.PlanPriceRequest) (app.PlanPriceView, error) {
		return a.Operator.AddPlanPrice(c.Request.Context(), w, r)
	})),
	route("B-O12", mPost, op+"/billing/token-packs", idemRequired, withBody(201, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.TokenPackRequest) (any, error) {
		return a.Operator.UpsertTokenPack(c.Request.Context(), w, r)
	})),
	route("B-O13", mPost, op+"/billing/webhook-events/:provider/:provider_event_id/replay", idemRequired, withBody(202, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.OperatorReason) (struct{}, error) {
		return struct{}{}, a.Operator.ReplayWebhookEvent(c.Request.Context(), w, c.Param("provider"), c.Param("provider_event_id"), r)
	})),
	route("B-O14", mPost, op+"/billing/provider-certifications", idemRequired, withBody(201, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.CertificationRequest) (app.CertificationView, error) {
		return a.Operator.SubmitCertification(c.Request.Context(), w, r)
	})),
	route("B-O15", mPost, op+"/billing/provider-certifications/:cert_id/revoke", idemRequired, withBody(200, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.OperatorReason) (app.CertificationView, error) {
		id, err := param(c, "cert_id")
		if err != nil {
			return app.CertificationView{}, err
		}
		return a.Operator.RevokeCertification(c.Request.Context(), w, id, r)
	})),
	route("B-O16", mPost, op+"/tenants/:id/invoices/:invoice_id/disposition", idemRequired, withBody(200, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.DispositionRequest) (app.DispositionResult, error) {
		t, err := param(c, "id")
		if err != nil {
			return app.DispositionResult{}, err
		}
		inv, err := param(c, "invoice_id")
		if err != nil {
			return app.DispositionResult{}, err
		}
		return a.Operator.DisposeHeldInvoice(c.Request.Context(), w, t, inv, r)
	})),
	route("B-O17", mPost, op+"/billing/provider-certifications/:cert_id/review", idemRequired, withBody(200, true, func(a *APIConfig, c *gin.Context, w app.Caller, r app.ReviewRequest) (app.CertificationView, error) {
		id, err := param(c, "cert_id")
		if err != nil {
			return app.CertificationView{}, err
		}
		return a.Operator.ReviewCertification(c.Request.Context(), w, id, r)
	}), requirePCIReviewer()),
}
