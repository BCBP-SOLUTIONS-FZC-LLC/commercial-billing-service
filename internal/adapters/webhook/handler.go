// Package webhook is the B-W1 handler (LLD §5.4.3, §10.3): read the raw
// body, verify it through the provider adapter, resolve the tenant from
// Billing's customer mapping, store the scrubbed event in the ledger, answer
// 200. Processing is asynchronous (§8.15). The route binds no tenant and
// trusts no header but the provider signature (BIL-AUTH-5).
package webhook

import (
	"context"
	"io"
	"net/http"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/problem"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// Handler serves POST /api/v1/webhooks/:provider.
type Handler struct {
	Providers    port.ProviderLookup
	Ledger       port.LedgerRepo   // system pool
	Customers    port.SystemReader // system pool (BIL-PAY-3)
	MaxBodyBytes int64             // WEBHOOK_MAX_BODY_BYTES
	// OnSignatureFailure increments
	// commercial_billing_webhook_signature_failures_total{provider} (BIL-PAY-2).
	OnSignatureFailure func(provider string)
	// OnDuplicate counts a redelivery already in the ledger
	// (commercial_billing_webhooks_total{outcome="duplicate"}).
	OnDuplicate func(provider, kind string)
	Log         port.Logger
}

// Route is the B-W1 path.
const Route = "/api/v1/webhooks/:provider"

// Register mounts B-W1 on r with no authentication middleware.
func (h *Handler) Register(r gin.IRouter) { r.POST(Route, h.receive) }

func (h *Handler) receive(c *gin.Context) {
	name := c.Param("provider")
	ctx, end := app.StartSpan(c.Request.Context(), "billing.webhook.receive", app.AttrProvider(name))
	defer end(nil)
	c.Request = c.Request.WithContext(ctx)
	provider, ok := h.Providers.Loaded(name)
	if !ok || !provider.Capabilities().WebhookRedelivery {
		// Unknown provider, or one that sends no callbacks (manual): 404.
		gincommon.AbortWithProblem(c, problem.NewNotFoundError("no webhook endpoint for this provider"))
		return
	}
	limit := h.MaxBodyBytes
	if limit <= 0 {
		limit = 262144
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, limit+1))
	if err != nil {
		gincommon.AbortWithProblem(c, gincommon.BindingError(err))
		return
	}
	if int64(len(body)) > limit {
		gincommon.AbortWithProblem(c, domain.NewError(domain.ErrPayloadTooLarge, "webhook body exceeds the limit"))
		return
	}
	ctx = c.Request.Context()
	ev, err := provider.VerifyAndNormalise(ctx, c.Request.Header, body)
	if err != nil {
		// BIL-PAY-2: refused, nothing stored. The reason is logged, never returned.
		if h.OnSignatureFailure != nil {
			h.OnSignatureFailure(name)
		}
		if h.Log != nil {
			h.Log.Warn("webhook refused", map[string]any{"provider": name, "reason": err.Error()})
		}
		gincommon.AbortWithProblem(c, domain.NewError(domain.ErrInvalidSignature, "invalid webhook signature"))
		return
	}
	insertCtx, endInsert := app.StartSpan(ctx, "billing.ledger.insert", app.AttrEventID(ev.ProviderEventID))
	err = h.store(insertCtx, ev)
	endInsert(err)
	if err != nil {
		if h.Log != nil {
			h.Log.Error("webhook not stored; provider will redeliver", map[string]any{"provider": name, "provider_event_id": ev.ProviderEventID, "error": err.Error()})
		}
		c.Header("Retry-After", "30")
		gincommon.AbortWithProblem(c, domain.NewError(domain.ErrDBUnavailable, "temporarily unavailable"))
		return
	}
	// New or duplicate: both 200 (the ledger primary key makes a redelivery a no-op).
	c.JSON(http.StatusOK, gin.H{})
}

// store resolves the tenant (BIL-PAY-3) and inserts the ledger row (BIL-PAY-1).
func (h *Handler) store(ctx context.Context, ev domain.PaymentEvent) error {
	var tenantID *string
	if ev.CustomerRef != "" {
		t, found, err := h.Customers.TenantForCustomer(ctx, ev.Provider, ev.CustomerRef)
		if err != nil {
			return err
		}
		if found {
			tenantID = &t
		}
	}
	var objRef *string
	if ev.Object.ID != "" {
		id := ev.Object.ID
		objRef = &id
	}
	inserted, err := h.Ledger.Insert(ctx, domain.LedgerEntry{
		Provider: ev.Provider, ProviderEventID: ev.ProviderEventID, EventType: ev.EventType,
		NormalisedKind: string(ev.Kind), TenantID: tenantID, ProviderObjectRef: objRef, Livemode: ev.Livemode,
		ProviderCreatedAt: ev.ProviderCreatedAt, Payload: ev.Payload,
	})
	if err == nil && !inserted && h.OnDuplicate != nil {
		h.OnDuplicate(ev.Provider, string(ev.Kind))
	}
	return err
}
