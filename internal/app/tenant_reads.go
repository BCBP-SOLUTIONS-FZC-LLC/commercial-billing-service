package app

import (
	"context"
	"strconv"
	"strings"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// account reads the tenant's billing account (404 SUBSCRIPTION_NOT_FOUND
// for a tenant without one).
func (s *Service) account(ctx context.Context, tenantID string) (domain.BillingAccount, error) {
	var a domain.BillingAccount
	err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		a, err = s.Accounts.Get(ctx, tenantID)
		return err
	})
	return a, err
}

// ── B-P15 / B-P16 ──────────────────────────────────────────────────────────

// ListPaymentMethods is B-P15: read live, nothing stored (BIL-PCI-2).
func (s *Service) ListPaymentMethods(ctx context.Context, c Caller, tenantID string) (PaymentMethodList, error) {
	out := PaymentMethodList{Items: []PaymentMethodView{}, CanManage: domain.HasPermission(c.Roles, domain.PermPaymentMethodManage)}
	acct, err := s.account(ctx, tenantID)
	if err != nil || acct.Provider == manualProvider {
		return out, err
	}
	prov, err := s.provider(acct.Provider)
	if err != nil {
		return out, err
	}
	pms, err := prov.ListPaymentMethods(ctx, port.CustomerRef{ProviderCustomerID: deref(acct.ProviderCustomerID)})
	if err != nil {
		return out, providerErr(err)
	}
	for _, pm := range pms {
		out.Items = append(out.Items, PaymentMethodView{ID: displayRef(pm.ProviderRef), Brand: pm.Brand, Last4: pm.Last4,
			ExpMonth: pm.ExpMonth, ExpYear: pm.ExpYear, IsDefault: pm.IsDefault})
	}
	return out, nil
}

// displayRef truncates a provider reference for display ("pm_…1234").
func displayRef(ref string) string {
	if len(ref) <= 8 {
		return ref
	}
	return ref[:3] + "…" + ref[len(ref)-4:]
}

// CreateSetupSession is B-P16: a provider-hosted page to add or replace the
// default payment method (BIL-API-5: the URL comes from the adapter only).
func (s *Service) CreateSetupSession(ctx context.Context, c Caller, tenantID string) (HostedSession, error) {
	acct, err := s.account(ctx, tenantID)
	if err != nil {
		return HostedSession{}, err
	}
	prov, err := s.provider(acct.Provider)
	if err != nil {
		return HostedSession{}, err
	}
	if !prov.Capabilities().HostedPaymentMethodUpdate {
		return HostedSession{}, domain.NewError(domain.ErrProviderOperationUnsupported, "the provider has no hosted payment-method page")
	}
	now := s.now()
	session := domain.CheckoutSession{SessionID: newID(), TenantID: tenantID, Purpose: domain.PurposePMSetup, RequestedBy: actorOf(c),
		ExpiresAt: now.Add(s.Settings.CheckoutTTL)}
	session.ProviderSessionID = domain.PendingSessionPrefix + session.SessionID
	if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error { return s.Checkouts.Insert(ctx, session) }); err != nil {
		return HostedSession{}, err
	}
	base := strings.TrimRight(s.Settings.TenantAppBaseURL, "/") + "/billing"
	hosted, err := prov.CreatePaymentMethodSetup(ctx, port.SetupInput{Customer: port.CustomerRef{ProviderCustomerID: deref(acct.ProviderCustomerID)},
		SuccessURL: base + "?payment_method=updated", CancelURL: base + "?payment_method=cancelled", ExpiresAt: session.ExpiresAt},
		"pmsetup:"+session.SessionID)
	if err != nil {
		return HostedSession{}, providerErr(err)
	}
	if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		return s.Checkouts.SetProviderSession(ctx, session.SessionID, hosted.ProviderSessionID, hosted.ExpiresAt)
	}); err != nil {
		return HostedSession{}, err
	}
	return HostedSession{URL: hosted.URL, ExpiresAt: hosted.ExpiresAt}, nil
}

// PaymentMethodSet records a completed setup session (§7.4
// payment_method_set); applyDefaultPM then makes the new method the
// provider default and sets default_pm_set.
type PaymentMethodSet struct{ S *Service }

// Handle implements port.PaymentEventHandler.
func (h PaymentMethodSet) Handle(ctx context.Context, tenantID string, _ domain.PaymentEvent, cur domain.ProviderObjectState) (string, error) {
	s := h.S
	session, found, err := s.Checkouts.ByProviderSession(ctx, cur.Ref.ID)
	if err != nil {
		return "", err
	}
	if !found || session.TenantID != tenantID || session.Purpose != domain.PurposePMSetup {
		return "", &port.IgnoredEvent{Reason: "unknown_session"}
	}
	if err := s.Checkouts.SetStatus(ctx, session.SessionID, domain.SessionCompleted); err != nil {
		return "", err
	}
	// Making it the provider default is a provider call, so the lifecycle
	// follow-up does it outside this transaction (BIL-CONC-2).
	return "applied:payment_method_set", nil
}

// ── B-P17 / B-P18 ──────────────────────────────────────────────────────────

func invoiceView(r port.InvoiceRow) InvoiceView {
	return InvoiceView{InvoiceID: r.InvoiceID, Number: r.Number, Kind: r.Kind, Status: r.Status,
		AmountDue:   MoneyFrom(domain.Money{AmountMinor: r.AmountDueMinor, Currency: r.Currency}),
		AmountPaid:  MoneyFrom(domain.Money{AmountMinor: r.AmountPaidMinor, Currency: r.Currency}),
		PeriodStart: r.PeriodStart, PeriodEnd: r.PeriodEnd, IssuedAt: r.IssuedAt, PaidAt: r.PaidAt, HostedInvoiceURL: r.HostedURL}
}

// ListInvoices is B-P17: drafts are hidden from tenants.
func (s *Service) ListInvoices(ctx context.Context, _ Caller, tenantID string, f InvoiceFilter) (List[InvoiceView], error) {
	limit := f.Limit
	if limit == 0 {
		limit = 20
	}
	out := List[InvoiceView]{Items: []InvoiceView{}}
	err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		rows, err := s.Invoices.List(ctx, tenantID, f.Status, f.Kind, f.Cursor, false, limit+1)
		if err != nil {
			return err
		}
		for i, r := range rows {
			if i == limit {
				out.NextCursor = ptr(rows[i-1].InvoiceID)
				break
			}
			out.Items = append(out.Items, invoiceView(r))
		}
		return nil
	})
	return out, err
}

// InvoiceDownloadURL is B-P18: a short-lived provider PDF link.
func (s *Service) InvoiceDownloadURL(ctx context.Context, _ Caller, tenantID, invoiceID string) (string, error) {
	var inv domain.Invoice
	if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var found bool
		var err error
		inv, found, err = s.Invoices.Get(ctx, invoiceID)
		if err == nil && (!found || inv.TenantID != tenantID) {
			return domain.NewError(domain.ErrInvoiceNotFound, "invoice not found")
		}
		if err == nil && inv.Status == "draft" {
			return domain.NewError(domain.ErrInvoiceNotFinalised, "the invoice is not finalised yet")
		}
		return err
	}); err != nil {
		return "", err
	}
	prov, err := s.provider(inv.Provider)
	if err != nil {
		return "", err
	}
	link, err := prov.InvoicePDFLink(ctx, port.InvoiceRef{ProviderInvoiceID: inv.ProviderInvoiceID})
	if err != nil {
		return "", providerErr(err)
	}
	return link.URL, nil
}

// ── B-P24 ──────────────────────────────────────────────────────────────────

// historyCause turns a stored cause into the tenant-facing label: no
// provider event types or route ids (§5.4.1 B-P24).
func historyCause(cause string) string {
	switch prefix, _, _ := strings.Cut(cause, ":"); prefix {
	case "webhook":
		return "payment"
	case "api":
		return "request"
	case "operator":
		return "support"
	case "iam":
		return "account"
	case "job":
		return "renewal"
	}
	return "system"
}

// SubscriptionHistory is B-P24. Actor is "you", "an administrator" or
// "system"; no operator identity or free text reaches tenants.
func (s *Service) SubscriptionHistory(ctx context.Context, c Caller, tenantID string, pg Page) (List[HistoryView], error) {
	limit := pg.Limit
	if limit == 0 {
		limit = 20
	}
	offset := 0
	if pg.Cursor != "" {
		n, err := strconv.Atoi(pg.Cursor)
		if err != nil || n < 0 {
			return List[HistoryView]{}, domain.NewError(domain.ErrValidation, "invalid cursor")
		}
		offset = n
	}
	out := List[HistoryView]{Items: []HistoryView{}}
	err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		sub, found, err := s.Subs.Latest(ctx, tenantID)
		if err != nil || !found {
			return err
		}
		rows, err := s.Subs.History(ctx, sub.BillingSubscriptionID, offset+limit+1)
		if err != nil {
			return err
		}
		for i := offset; i < len(rows) && i < offset+limit; i++ {
			h := rows[i]
			v := HistoryView{OccurredAt: h.OccurredAt, ToStatus: string(h.ToStatus), FromSeats: h.FromSeats, ToSeats: h.ToSeats,
				Cause: historyCause(h.Cause), Actor: "an administrator"}
			if h.FromStatus != nil {
				v.FromStatus = ptr(string(*h.FromStatus))
			}
			if h.FromPlan != nil {
				v.FromPlan = ptr(string(*h.FromPlan))
			}
			if h.ToPlan != nil {
				v.ToPlan = ptr(string(*h.ToPlan))
			}
			switch h.ActorID {
			case c.UserID:
				v.Actor = "you"
			case domain.CommercialSystemPrincipal:
				v.Actor = "system"
			}
			out.Items = append(out.Items, v)
		}
		if len(rows) > offset+limit {
			out.NextCursor = ptr(strconv.Itoa(offset + limit))
		}
		return nil
	})
	return out, err
}

// ── B-P25 / B-P26 ──────────────────────────────────────────────────────────

func contactView(a domain.BillingAccount) BillingContact {
	return BillingContact{BillingContactName: a.BillingContactName, BillingContactEmail: a.BillingContactEmail, CompanyName: a.CompanyName,
		Country: a.Country, TaxID: a.TaxID, RecordVersion: a.RecordVersion}
}

// GetBillingContact is B-P25.
func (s *Service) GetBillingContact(ctx context.Context, _ Caller, tenantID string) (BillingContact, error) {
	a, err := s.account(ctx, tenantID)
	return contactView(a), err
}

// UpdateBillingContact is B-P26: the provider customer first (invoices and
// dunning emails go there), then the account with its record_version. No
// bus event (PII stays off the bus).
func (s *Service) UpdateBillingContact(ctx context.Context, _ Caller, tenantID string, r BillingContact) (BillingContact, error) {
	a, err := s.account(ctx, tenantID)
	if err != nil {
		return BillingContact{}, err
	}
	if a.RecordVersion != r.RecordVersion {
		return BillingContact{}, domain.NewError(domain.ErrOptimisticLockConflict, "the billing contact changed since it was read").
			With("record_version", a.RecordVersion)
	}
	if a.Provider != manualProvider {
		prov, err := s.provider(a.Provider)
		if err != nil {
			return BillingContact{}, err
		}
		in := port.CustomerInput{TenantID: tenantID, Name: deref(r.BillingContactName), Email: deref(r.BillingContactEmail),
			Company: deref(r.CompanyName), Country: deref(r.Country), TaxID: deref(r.TaxID)}
		if err := prov.UpdateCustomer(ctx, port.CustomerRef{ProviderCustomerID: deref(a.ProviderCustomerID)}, in,
			"contact:"+tenantID+":"+strconv.FormatInt(r.RecordVersion, 10)); err != nil {
			return BillingContact{}, providerErr(err)
		}
	}
	var out BillingContact
	err = s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		updated, err := s.Accounts.UpdateContact(ctx, tenantID, r.RecordVersion, r.BillingContactName, r.BillingContactEmail,
			r.CompanyName, r.Country, r.TaxID)
		out = contactView(updated)
		return err
	})
	return out, err
}

// applyDefaultPM is the lifecycle follow-up for a completed setup session
// (B-P16): re-read the session for the saved method, make it the customer
// and subscription default on the provider, then record it.
func (s *Service) applyDefaultPM(ctx context.Context, tenantID, sessionID string) error {
	var (
		session domain.CheckoutSession
		acct    domain.BillingAccount
	)
	if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var found bool
		var err error
		if session, found, err = s.Checkouts.GetSession(ctx, sessionID); err != nil || !found {
			return err
		}
		acct, err = s.Accounts.Get(ctx, tenantID)
		return err
	}); err != nil || session.SessionID == "" {
		return err
	}
	prov, err := s.provider(acct.Provider)
	if err != nil {
		return err
	}
	st, err := prov.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: domain.ObjectCheckoutSession, ID: session.ProviderSessionID})
	if err != nil {
		return providerErr(err)
	}
	pm, _ := st.Fields["setup_payment_method"].(string)
	if pm != "" {
		if err := prov.AttachDefaultPaymentMethod(ctx, port.CustomerRef{ProviderCustomerID: deref(acct.ProviderCustomerID)}, pm,
			"pmdefault:"+sessionID); err != nil {
			return providerErr(err)
		}
	}
	return s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.Checkouts.MarkDefaultApplied(ctx, sessionID); err != nil {
			return err
		}
		if pm == "" {
			return nil // nothing saved (abandoned inside the page): only close it
		}
		return s.Accounts.SetDefaultPM(ctx, tenantID, true)
	})
}
