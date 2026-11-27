package stripe

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// §8.7 / BIL-PAY-7: the one-off invoice is created with auto_advance off,
// so a draft whose item failed is never finalised by Stripe on its own (an
// empty draft would become a paid 0 invoice). Such a draft is deleted at
// once and no invoice is returned; when the delete fails too, its id is
// returned so purchase-expiry deletes it.
func TestBIL_PAY_7_ChargeOneOffItemFailureDeletesDraft(t *testing.T) {
	f, a := newFakeStripe(t)
	f.on("POST", "/v1/invoices", 200, invoice("in_9", "draft", nil))
	f.on("POST", "/v1/invoiceitems", 500, unavailable)
	f.on("DELETE", "/v1/invoices/in_9", 200, map[string]any{"id": "in_9", "object": "invoice", "deleted": true})
	res, err := a.ChargeOneOff(context.Background(), port.OneOffCharge{Customer: cus, Amount: usd5}, "k")
	if !errors.Is(err, port.ErrProviderUnavailable) || res.Invoice.ProviderInvoiceID != "" {
		t.Fatalf("got %+v %v", res, err)
	}
	wantForm(t, f.only("POST", "/v1/invoices"), "auto_advance", "false")
	if n := len(f.sent("DELETE", "/v1/invoices/in_9")); n != 1 {
		t.Fatalf("draft deletes %d", n)
	}

	f2, a2 := newFakeStripe(t)
	f2.on("POST", "/v1/invoices", 200, invoice("in_9", "draft", nil))
	f2.on("POST", "/v1/invoiceitems", 500, unavailable)
	f2.on("DELETE", "/v1/invoices/in_9", 500, unavailable)
	res, err = a2.ChargeOneOff(context.Background(), port.OneOffCharge{Customer: cus, Amount: usd5}, "k")
	if !errors.Is(err, port.ErrProviderUnavailable) || res.Invoice.ProviderInvoiceID != "in_9" {
		t.Fatalf("delete failed: got %+v %v", res, err)
	}
}

// DeleteDraftInvoice: a draft is deleted; one already gone is nil; a
// provider error is returned.
func TestDeleteDraftInvoice(t *testing.T) {
	ctx := context.Background()
	f, a := newFakeStripe(t)
	f.on("DELETE", "/v1/invoices/in_1", 200, map[string]any{"id": "in_1", "object": "invoice", "deleted": true})
	f.on("DELETE", "/v1/invoices/in_gone", 404, apiErr("invalid_request_error", "resource_missing"))
	f.on("DELETE", "/v1/invoices/in_down", 500, unavailable)
	if err := a.DeleteDraftInvoice(ctx, port.InvoiceRef{ProviderInvoiceID: "in_1"}); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteDraftInvoice(ctx, port.InvoiceRef{ProviderInvoiceID: "in_gone"}); err != nil {
		t.Fatalf("gone %v", err)
	}
	if err := a.DeleteDraftInvoice(ctx, port.InvoiceRef{ProviderInvoiceID: "in_down"}); !errors.Is(err, port.ErrProviderUnavailable) {
		t.Fatalf("down %v", err)
	}
}

// BIL-PAY-6: a proration lookup matches only an invoice with a charged
// line for the change: the seats after (quantity) or the target price.
// Another purchase's invoice for the same amount (another quantity or
// price, or no charged line) is not matched.
func TestBIL_PAY_6_FindInvoiceMatchesChargedLine(t *testing.T) {
	ctx := context.Background()
	since := fixedNow.Add(-time.Hour)
	lines := func(ls ...map[string]any) map[string]any {
		return map[string]any{"object": "list", "data": ls, "url": "/v1/invoices/x/lines"}
	}
	line := func(amount, qty int, price string) map[string]any {
		return map[string]any{"id": "il", "object": "line_item", "amount": amount, "quantity": qty,
			"pricing": map[string]any{"type": "price_details", "price_details": map[string]any{"price": price}}}
	}
	pror := func(id string, ls map[string]any) map[string]any {
		return invoice(id, "paid", map[string]any{"billing_reason": "subscription_update", "amount_due": 4600, "total": 4600, "lines": ls})
	}
	f, a := newFakeStripe(t)
	f.on("GET", "/v1/invoices", 200, list("/v1/invoices",
		pror("in_p1", lines(line(-23000, 10, "price_pro"), line(27600, 12, "price_pro"))),
		pror("in_none", lines()),
		pror("in_p2", lines(line(-27600, 12, "price_pro"), line(32200, 14, "price_pro"))),
		pror("in_up", lines(line(-27600, 12, "price_pro"), line(32200, 12, "price_ent"))),
	))
	usd := &domain.Money{AmountMinor: 4600, Currency: "USD"}
	for _, c := range []struct {
		q    port.InvoiceQuery
		want string
	}{
		{port.InvoiceQuery{Subscription: subRef, Amount: usd, Quantity: 14, CreatedSince: since}, "in_p2"},
		{port.InvoiceQuery{Subscription: subRef, Amount: usd, Quantity: 12, CreatedSince: since}, "in_p1"},
		{port.InvoiceQuery{Subscription: subRef, Amount: usd, PriceRef: "price_ent", CreatedSince: since}, "in_up"},
		{port.InvoiceQuery{Subscription: subRef, Amount: usd, Quantity: 16, CreatedSince: since}, ""},
		{port.InvoiceQuery{Subscription: subRef, Amount: usd, PriceRef: "price_biz", CreatedSince: since}, ""},
	} {
		ref, found, err := a.FindInvoice(ctx, c.q)
		if err != nil || found != (c.want != "") || ref.ProviderInvoiceID != c.want {
			t.Fatalf("%+v: %+v %v %v, want %q", c.q, ref, found, err, c.want)
		}
	}
}

// FindInvoice (§8.5.1, §8.7): one-off invoices are matched on metadata
// purchase_id among the customer's invoices; proration invoices on the
// subscription, billing_reason subscription_update and the amount; only
// invoices created since the bound are listed; none → not found.
func TestFindInvoice(t *testing.T) {
	ctx := context.Background()
	since := fixedNow.Add(-time.Hour)
	f, a := newFakeStripe(t)
	f.on("GET", "/v1/invoices", 200, list("/v1/invoices",
		invoice("in_other", "open", map[string]any{"metadata": map[string]any{"purchase_id": "p-2"}, "billing_reason": "manual", "amount_due": 500}),
		invoice("in_cycle", "paid", map[string]any{"billing_reason": "subscription_cycle", "amount_due": 4600}),
		invoice("in_addon", "paid", map[string]any{"metadata": map[string]any{"purchase_id": "p-1"}, "billing_reason": "manual", "amount_due": 500}),
		invoice("in_pror", "open", map[string]any{"billing_reason": "subscription_update", "amount_due": 4600, "total": 4600}),
	))

	ref, found, err := a.FindInvoice(ctx, port.InvoiceQuery{Customer: cus, PurchaseID: "p-1", CreatedSince: since})
	if err != nil || !found || ref.ProviderInvoiceID != "in_addon" {
		t.Fatalf("by metadata %+v %v %v", ref, found, err)
	}
	r := f.sent("GET", "/v1/invoices")[0]
	wantForm(t, r, "customer", "cus_1", "created[gte]", "1795162443")
	wantNoForm(t, r, "subscription")

	ref, found, err = a.FindInvoice(ctx, port.InvoiceQuery{Subscription: subRef, Amount: &domain.Money{AmountMinor: 4600, Currency: "USD"}, CreatedSince: since})
	if err != nil || !found || ref.ProviderInvoiceID != "in_pror" {
		t.Fatalf("by proration %+v %v %v", ref, found, err)
	}
	wantForm(t, f.sent("GET", "/v1/invoices")[1], "subscription", "sub_1")

	if _, found, err := a.FindInvoice(ctx, port.InvoiceQuery{Customer: cus, PurchaseID: "p-9", CreatedSince: since}); err != nil || found {
		t.Fatalf("absent %v %v", found, err)
	}
	if _, found, err := a.FindInvoice(ctx, port.InvoiceQuery{Subscription: subRef, Amount: &domain.Money{AmountMinor: 1, Currency: "USD"}, CreatedSince: since}); err != nil || found {
		t.Fatalf("other amount %v %v", found, err)
	}
	if _, found, err := a.FindInvoice(ctx, port.InvoiceQuery{}); err != nil || found {
		t.Fatalf("no selector %v %v", found, err)
	}

	f2, a2 := newFakeStripe(t)
	f2.on("GET", "/v1/invoices", 500, unavailable)
	if _, _, err := a2.FindInvoice(ctx, port.InvoiceQuery{Customer: cus, PurchaseID: "p-1", CreatedSince: since}); !errors.Is(err, port.ErrProviderUnavailable) {
		t.Fatalf("provider down %v", err)
	}
}
