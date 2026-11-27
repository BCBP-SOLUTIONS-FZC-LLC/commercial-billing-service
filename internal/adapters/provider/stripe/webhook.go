package stripe

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// SignatureHeader is Stripe's webhook signature header.
const SignatureHeader = "Stripe-Signature"

// VerifyAndNormalise verifies a callback and normalises it (§5.4.3, §10.3):
// HMAC-SHA256 over "<t>.<raw body>" against every configured secret (two
// during rotation), constant-time compare, timestamp within the tolerance,
// livemode equal to the environment. Any failure is port.ErrInvalidSignature
// and nothing is stored (BIL-PAY-2). The returned payload is allow-listed:
// no card data, no billing details, no email (BIL-PCI-3).
func (a *Adapter) VerifyAndNormalise(_ context.Context, h http.Header, body []byte) (domain.PaymentEvent, error) {
	if err := a.verify(h.Get(SignatureHeader), body); err != nil {
		return domain.PaymentEvent{}, err
	}
	var ev struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Created  int64  `json:"created"`
		Livemode bool   `json:"livemode"`
		Data     struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ev); err != nil || ev.ID == "" || ev.Type == "" {
		return domain.PaymentEvent{}, fmt.Errorf("%w: malformed event", port.ErrInvalidSignature)
	}
	if ev.Livemode != a.cfg.Livemode {
		return domain.PaymentEvent{}, fmt.Errorf("%w: livemode %v in a livemode=%v environment", port.ErrInvalidSignature, ev.Livemode, a.cfg.Livemode)
	}
	return normalise(ev.ID, ev.Type, ev.Created, ev.Livemode, ev.Data.Object)
}

// normalise builds the PaymentEvent and the allow-listed payload
// (BIL-PCI-3) from a verified event, or from one listed by reconciliation.
func normalise(id, typ string, created int64, livemode bool, object json.RawMessage) (domain.PaymentEvent, error) {
	ev := struct {
		ID, Type string
		Created  int64
		Livemode bool
	}{id, typ, created, livemode}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(object, &obj); err != nil {
		return domain.PaymentEvent{}, fmt.Errorf("%w: malformed event object", port.ErrInvalidSignature)
	}
	kind, objKind := normaliseKind(ev.Type, str(obj["mode"]))
	out := domain.PaymentEvent{
		Provider:          Name,
		ProviderEventID:   ev.ID,
		EventType:         ev.Type,
		Kind:              kind,
		Livemode:          ev.Livemode,
		ProviderCreatedAt: time.Unix(ev.Created, 0).UTC(),
		CustomerRef:       idOf(obj["customer"]),
		Object:            domain.ProviderObjectRef{Kind: objKind, ID: str(obj["id"])},
		Metadata:          metadata(obj["metadata"]),
	}
	if objKind == domain.ObjectCustomer {
		out.CustomerRef = out.Object.ID
	}
	scrubbed, err := json.Marshal(map[string]any{
		"id": ev.ID, "type": ev.Type, "created": ev.Created, "livemode": ev.Livemode,
		"object": allowList(obj),
	})
	if err != nil {
		return domain.PaymentEvent{}, err
	}
	out.Payload = scrubbed
	return out, nil
}

// verify checks the Stripe-Signature header: t=<unix>,v1=<hex>[,v1=…].
func (a *Adapter) verify(header string, body []byte) error {
	var ts int64
	var sigs [][]byte
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts, _ = strconv.ParseInt(v, 10, 64)
		case "v1":
			if b, err := hex.DecodeString(v); err == nil {
				sigs = append(sigs, b)
			}
		}
	}
	if ts == 0 || len(sigs) == 0 {
		return fmt.Errorf("%w: missing timestamp or v1 signature", port.ErrInvalidSignature)
	}
	age := a.now().Sub(time.Unix(ts, 0))
	if age > a.cfg.Tolerance || age < -a.cfg.Tolerance {
		return fmt.Errorf("%w: timestamp outside the %s tolerance", port.ErrInvalidSignature, a.cfg.Tolerance)
	}
	signed := append([]byte(strconv.FormatInt(ts, 10)+"."), body...)
	for _, secret := range a.cfg.WebhookSecrets {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(signed)
		want := mac.Sum(nil)
		for _, s := range sigs {
			if hmac.Equal(want, s) {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: no signature matches a configured secret", port.ErrInvalidSignature)
}

// normaliseKind maps a Stripe event type to the §7.4 kind and the object kind.
func normaliseKind(eventType, mode string) (domain.NormalisedKind, domain.ProviderObjectKind) {
	switch eventType {
	case "checkout.session.completed":
		if mode == "setup" {
			return domain.KindPaymentMethodSet, domain.ObjectCheckoutSession
		}
		return domain.KindCheckoutCompleted, domain.ObjectCheckoutSession // subscription or payment (add-on)
	case "checkout.session.expired":
		return domain.KindCheckoutExpired, domain.ObjectCheckoutSession
	case "invoice.created", "invoice.finalized", "invoice.updated", "invoice.voided", "invoice.marked_uncollectible":
		return domain.KindInvoiceUpdated, domain.ObjectInvoice
	case "invoice.paid":
		return domain.KindInvoicePaid, domain.ObjectInvoice
	case "invoice.payment_failed":
		return domain.KindInvoicePaymentFailed, domain.ObjectInvoice
	case "invoice.payment_action_required":
		return domain.KindPaymentActionRequired, domain.ObjectInvoice
	case "customer.subscription.updated":
		return domain.KindSubscriptionUpdated, domain.ObjectSubscription
	case "customer.subscription.deleted":
		return domain.KindSubscriptionEnded, domain.ObjectSubscription
	case "subscription_schedule.released", "subscription_schedule.completed", "subscription_schedule.canceled":
		return domain.KindScheduleUpdated, domain.ObjectSchedule
	case "charge.refunded", "refund.updated", "refund.failed":
		if eventType == "charge.refunded" {
			return domain.KindRefundUpdated, domain.ProviderObjectKind("charge")
		}
		return domain.KindRefundUpdated, domain.ObjectRefund
	case "charge.dispute.created":
		return domain.KindDisputeOpened, domain.ObjectDispute
	case "charge.dispute.updated":
		return domain.KindDisputeUpdated, domain.ObjectDispute
	case "charge.dispute.closed":
		return domain.KindDisputeClosed, domain.ObjectDispute
	case "radar.early_fraud_warning.created":
		return domain.KindFraudWarning, domain.ObjectEarlyFraudWarning
	case "customer.updated":
		return domain.KindCustomerUpdated, domain.ObjectCustomer
	case "payment_method.attached", "payment_method.detached":
		return domain.KindCustomerUpdated, domain.ObjectPaymentMethod
	}
	return domain.KindIgnored, ""
}

// allowedObjectFields are the only object fields kept in the ledger payload
// (BIL-PCI-3). Anything not listed — payment_method_details, card,
// billing_details, customer_email, address, shipping, … — is dropped.
var allowedObjectFields = map[string]bool{
	"id": true, "object": true, "customer": true, "subscription": true, "status": true, "mode": true,
	"metadata": true, "billing_reason": true, "amount_due": true, "amount_paid": true, "amount_remaining": true,
	"amount": true, "amount_refunded": true, "currency": true, "number": true, "period_start": true, "period_end": true,
	"cancel_at_period_end": true, "canceled_at": true, "ended_at": true, "created": true, "reason": true,
	"charge": true, "payment_intent": true, "invoice": true, "attempt_count": true, "next_payment_attempt": true,
	"livemode": true, "fraud_type": true, "actionable": true, "released_subscription": true, "expires_at": true,
	"payment_status": true, "collection_method": true, "auto_advance": true, "pause_collection": true,
}

// allowedMetadataKeys are the only metadata keys kept (BIL-PCI-3, §4.2.15):
// the ids Billing itself sets. Anything else a dashboard user or another
// integration put in metadata is dropped.
var allowedMetadataKeys = map[string]bool{
	"tenant_id": true, "billing_subscription_id": true, "purpose": true, "purchase_id": true, "change_id": true,
}

func allowList(obj map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(obj))
	for k, v := range obj {
		if !allowedObjectFields[k] {
			continue
		}
		if k == "metadata" {
			if m := metadata(v); len(m) > 0 {
				b, _ := json.Marshal(m)
				out[k] = b
			}
			continue
		}
		if k == "customer" || k == "subscription" || k == "invoice" || k == "charge" || k == "payment_intent" {
			// Expanded objects carry personal data; keep only the id.
			if id := idOf(v); id != "" {
				b, _ := json.Marshal(id)
				out[k] = b
				continue
			}
		}
		out[k] = v
	}
	return out
}

// str decodes a JSON string, or "".
func str(raw json.RawMessage) string {
	var s string
	if len(raw) > 0 && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// idOf returns the id of a field that is either an id string or an expanded object.
func idOf(raw json.RawMessage) string {
	if s := str(raw); s != "" {
		return s
	}
	var o struct {
		ID string `json:"id"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &o) == nil {
		return o.ID
	}
	return ""
}

// metadata returns the allow-listed metadata keys (BIL-PCI-3).
func metadata(raw json.RawMessage) map[string]string {
	all := map[string]json.RawMessage{}
	m := map[string]string{}
	if len(raw) == 0 || json.Unmarshal(raw, &all) != nil {
		return m
	}
	for k, v := range all {
		if s := str(v); s != "" && allowedMetadataKeys[k] {
			m[k] = s
		}
	}
	return m
}
