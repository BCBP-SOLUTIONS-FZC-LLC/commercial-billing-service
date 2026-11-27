package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// declineReasons is B-I1's decline_reason enum (§5.4.2); anything else is
// "other".
var declineReasons = map[string]string{
	"card_declined": "card_declined", "generic_decline": "card_declined", "do_not_honor": "card_declined",
	"insufficient_funds": "insufficient_funds", "expired_card": "expired_card", "incorrect_cvc": "incorrect_cvc",
	"processing_error": "processing_error", "authentication_required": "authentication_failed",
	"authentication_failed": "authentication_failed", "payment_intent_authentication_failure": "authentication_failed",
}

func declineReason(code string) string {
	if r, ok := declineReasons[strings.ToLower(code)]; ok {
		return r
	}
	return "other"
}

// AuthorizeAndCharge is B-I1 (§5.4.2, §8.3): create the account and an
// incomplete subscription, then customer → default payment method →
// subscription (default_incomplete) → off-session confirmation, each with
// a key derived from the subscription id (BIL-IDEM-2). Paid → active and
// the direct_signup saga in awaiting_tenant_converted; 3-D Secure → 402
// with the client secret, and the BFF's retry with the same key re-reads
// the payment; declined → provider subscription cancelled, ended.
func (s *Service) AuthorizeAndCharge(ctx context.Context, c Caller, r AuthorizeChargeRequest) (ChargeResult, error) {
	ctx, end := StartSpan(ctx, "billing.signup.charge", AttrTenant(r.TenantID))
	out, err := s.authorizeAndCharge(ctx, c, r)
	end(err)
	return out, err
}

func (s *Service) authorizeAndCharge(ctx context.Context, c Caller, r AuthorizeChargeRequest) (ChargeResult, error) {
	tenantID := strings.ToLower(r.TenantID)
	plan := domain.PlanCode(r.PlanCode)
	interval := domain.BillingInterval(r.Interval)
	if interval == "" {
		interval = domain.IntervalMonth
	}
	currency := strings.ToUpper(r.Currency)
	if currency == "" {
		currency = s.Settings.DefaultCurrency
	}
	prov, err := s.Providers.ForNewAccount(s.Settings.NewAccountProvider)
	if err != nil {
		return ChargeResult{}, err
	}
	if !prov.Capabilities().OffSessionCharges && !prov.Capabilities().RecurringSubscriptions {
		return ChargeResult{}, domain.NewError(domain.ErrProviderOperationUnsupported, "the provider cannot charge off-session")
	}

	// Read the tenant's state: a retry after 3-D Secure continues the same
	// incomplete subscription; anything live is 409 with its id.
	var (
		acct    *domain.BillingAccount
		pending *domain.Subscription
	)
	if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if a, err := s.Accounts.Get(ctx, tenantID); err == nil {
			acct = &a
		} else if !domain.HasCode(err, domain.ErrSubscriptionNotFound) {
			return err
		}
		live, found, err := s.Subs.Live(ctx, tenantID)
		if err != nil || !found {
			return err
		}
		if live.Status != domain.StatusIncomplete || live.Origin != domain.OriginDirectSignup {
			return alreadyExists(live.BillingSubscriptionID)
		}
		pending = &live
		return nil
	}); err != nil {
		return ChargeResult{}, err
	}

	subID := newID()
	if pending != nil {
		if s.now().Sub(pending.CreatedAt) > s.Settings.SCAAbandonAfter {
			return ChargeResult{}, s.expireCharge(ctx, prov, *pending)
		}
		item, err := s.itemOf(ctx, tenantID, pending.BillingSubscriptionID)
		if err != nil {
			return ChargeResult{}, err
		}
		if pending.Plan != plan || pending.Interval != interval || item.Quantity != r.LicensedSeats {
			return ChargeResult{}, alreadyExists(pending.BillingSubscriptionID)
		}
		subID = pending.BillingSubscriptionID
	}

	// Customer before the account row (chk_ba_customer), outside any
	// transaction (BIL-CONC-2).
	custRef := ""
	if acct != nil {
		custRef = deref(acct.ProviderCustomerID)
		if acct.Currency != currency {
			return ChargeResult{}, domain.NewError(domain.ErrCurrencyMismatch, "currency differs from the account's")
		}
	} else {
		in := port.CustomerInput{TenantID: tenantID, Currency: currency, Company: r.CompanyName, Country: r.Country}
		if r.BillingContact != nil {
			in.Name, in.Email = r.BillingContact.Name, r.BillingContact.Email
		}
		cust, err := prov.EnsureCustomer(ctx, in, "bsub:"+subID+":customer")
		if err != nil {
			return ChargeResult{}, providerErr(err)
		}
		custRef = cust.ProviderCustomerID
	}

	// Transaction 1: account, incomplete subscription, seat item.
	var price domain.PlanPrice
	err = s.locked(ctx, tenantID, func(ctx context.Context) error {
		if acct == nil {
			// Re-read under the tenant lock (see StartCheckout): a concurrent
			// signup may have created the account since the read above.
			existing, found, err := s.accountIfAny(ctx, tenantID)
			if err != nil {
				return err
			}
			if found {
				if existing.Currency != currency {
					return domain.NewError(domain.ErrCurrencyMismatch, "currency differs from the account's")
				}
				custRef = deref(existing.ProviderCustomerID)
				acct = &existing
			}
		}
		if acct == nil {
			a := domain.BillingAccount{TenantID: tenantID, Provider: prov.Name(), ProviderCustomerID: &custRef, Currency: currency,
				IAMLifecycle: domain.LifecycleTrial, CompanyName: nonEmpty(r.CompanyName), Country: nonEmpty(r.Country),
				DefaultPMSet: true} // the signup card becomes the customer default (AttachDefaultPaymentMethod)
			if r.BillingContact != nil {
				a.BillingContactName, a.BillingContactEmail = nonEmpty(r.BillingContact.Name), nonEmpty(r.BillingContact.Email)
			}
			if err := s.Accounts.Create(ctx, a); err != nil {
				return accountCreateErr(err)
			}
		}
		var ok bool
		var err error
		price, ok, err = s.Prices.PriceAt(ctx, plan, interval, currency, prov.Name(), s.now())
		if err != nil {
			return err
		}
		if !ok || price.ProviderPriceID == nil {
			return domain.NewError(domain.ErrPriceNotAvailable, "no price for this plan, interval and currency")
		}
		if pending != nil {
			return nil
		}
		if live, found, err := s.Subs.Live(ctx, tenantID); err != nil {
			return err
		} else if found {
			return alreadyExists(live.BillingSubscriptionID)
		}
		sub := domain.Subscription{BillingSubscriptionID: subID, TenantID: tenantID, Provider: prov.Name(), Origin: domain.OriginDirectSignup,
			Plan: plan, Interval: interval, Status: domain.StatusIncomplete, AutoRenew: true}
		// Subs.Live was checked above under the tenant lock, so a unique
		// violation here means a writer that does not take the lock; the
		// repository maps it to 409 SUBSCRIPTION_ALREADY_EXISTS. Nothing more
		// can be read: the failed insert has aborted the transaction.
		if err := s.Subs.Create(ctx, sub, domain.SubscriptionItem{PriceID: price.PriceID, Quantity: r.LicensedSeats}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return ChargeResult{}, err
	}

	// Provider calls, each idempotent on the subscription id.
	cust := port.CustomerRef{ProviderCustomerID: custRef}
	providerSub := ""
	if pending != nil {
		providerSub = deref(pending.ProviderSubscriptionID)
	}
	if providerSub == "" {
		if err := prov.AttachDefaultPaymentMethod(ctx, cust, r.PaymentMethodRef, "bsub:"+subID+":pm"); err != nil {
			return ChargeResult{}, s.chargeFailed(ctx, prov, tenantID, subID, "", err)
		}
		res, err := prov.CreateSubscription(ctx, port.SubscriptionInput{Customer: cust, PriceRef: *price.ProviderPriceID, Quantity: r.LicensedSeats,
			Currency: currency, DefaultPaymentMethod: r.PaymentMethodRef,
			Metadata: map[string]string{"tenant_id": tenantID, "billing_subscription_id": subID, "purpose": "direct_signup"}}, "bsub:"+subID+":create")
		if err != nil {
			return ChargeResult{}, s.chargeFailed(ctx, prov, tenantID, subID, "", err)
		}
		providerSub = res.Ref.ProviderSubscriptionID
		if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
			_, err := s.Subs.Update(ctx, subID, 0, func(x *domain.Subscription) { x.ProviderSubscriptionID = &providerSub })
			return err
		}); err != nil {
			return ChargeResult{}, err
		}
	}
	ref := port.SubscriptionRef{ProviderSubscriptionID: providerSub}
	outcome, err := prov.ConfirmInitialPayment(ctx, ref, "bsub:"+subID+":confirm")
	if err == nil {
		switch outcome.Status {
		case port.PaymentPaid:
		case port.PaymentRequiresAction:
			err = port.ErrActionRequired{ClientSecret: outcome.ClientSecret}
		case port.PaymentDeclined:
			err = port.ErrDeclined{Code: outcome.DeclineCode}
		case port.PaymentPending:
			err = port.ErrProviderUnavailable // not settled yet: the BFF retries with the same key
		}
	}
	if err != nil {
		return ChargeResult{}, s.chargeFailed(ctx, prov, tenantID, subID, providerSub, err)
	}

	// Paid: the period and invoice from the provider, then transaction 2.
	state, err := prov.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: domain.ObjectSubscription, ID: providerSub})
	if err != nil {
		return ChargeResult{}, providerErr(err) // the retry with the same key applies it
	}
	start, _ := state.Fields["current_period_start"].(time.Time)
	end, _ := state.Fields["current_period_end"].(time.Time)
	var out ChargeResult
	err = s.locked(ctx, tenantID, func(ctx context.Context) error {
		sub, err := s.Subs.Get(ctx, subID)
		if err != nil {
			return err
		}
		if sub.Status == domain.StatusIncomplete {
			now := s.now()
			from := sub.Status
			if sub, err = s.Subs.Transition(ctx, subID, 0, domain.StatusActive, func(x *domain.Subscription) {
				x.PaymentConfirmedAt = &now
				if !start.IsZero() {
					x.CurrentPeriodStart, x.CurrentPeriodEnd = &start, &end
				}
			}); err != nil {
				return err
			}
			if err := s.history(ctx, sub, &from, "api:B-I1", actorOf(c), nil, &[2]*int{nil, &r.LicensedSeats}, nil); err != nil {
				return err
			}
			if _, found, err := s.Sagas.BySubscription(ctx, subID); err != nil {
				return err
			} else if !found {
				if err := s.Sagas.Insert(ctx, domain.ConversionSaga{SagaID: newID(), TenantID: tenantID, BillingSubscriptionID: subID,
					Kind: domain.SagaDirectSignup, State: domain.SagaAwaitingTenantConverted, PaymentConfirmedAt: now}); err != nil {
					return err
				}
			}
		}
		out = ChargeResult{BillingSubscriptionID: subID, Status: string(sub.Status), PlanCode: string(sub.Plan), LicensedSeats: r.LicensedSeats,
			Interval: string(sub.Interval), Provider: sub.Provider}
		if sub.CurrentPeriodEnd != nil {
			out.CurrentPeriodEnd = *sub.CurrentPeriodEnd
		}
		return nil
	})
	return out, err
}

// chargeFailed turns a provider failure into B-I1's answer. A decline
// cancels the provider subscription and ends Billing's (signup_failed); 3-D
// Secure answers 402 with the client secret and expiry; anything else is
// mapped (503 PROVIDER_UNAVAILABLE: retry with the same key).
func (s *Service) chargeFailed(ctx context.Context, prov port.PaymentProvider, tenantID, subID, providerSub string, cause error) error {
	var action port.ErrActionRequired
	if errors.As(cause, &action) {
		var created time.Time
		_ = s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
			sub, err := s.Subs.Get(ctx, subID)
			created = sub.CreatedAt
			return err
		})
		return domain.NewError(domain.ErrPaymentActionRequired, "customer authentication required").
			With("action", map[string]string{"type": "3ds", "client_secret": action.ClientSecret}).
			With("billing_subscription_id", subID).With("expires_at", created.Add(s.Settings.SCAAbandonAfter).UTC().Format(time.RFC3339))
	}
	var declined port.ErrDeclined
	if !errors.As(cause, &declined) {
		return providerErr(cause)
	}
	if providerSub != "" {
		if err := prov.CancelNow(ctx, port.SubscriptionRef{ProviderSubscriptionID: providerSub}, "cancel:"+subID+":1"); err != nil {
			return providerErr(err) // retry: the decline is re-read and the cancel repeated
		}
	}
	if err := s.locked(ctx, tenantID, func(ctx context.Context) error {
		sub, err := s.Subs.Get(ctx, subID)
		if err != nil || sub.Status != domain.StatusIncomplete {
			return err
		}
		return s.endSignup(ctx, sub, "api:B-I1")
	}); err != nil {
		return err
	}
	return domain.NewError(domain.ErrPaymentDeclined, "payment declined").With("decline_reason", declineReason(declined.Code))
}

// expireCharge is 410 CHARGE_EXPIRED: 3-D Secure not completed within
// SCA_ABANDON_AFTER; the provider subscription is cancelled and Billing's
// ended (§8.3).
func (s *Service) expireCharge(ctx context.Context, prov port.PaymentProvider, sub domain.Subscription) error {
	if p := deref(sub.ProviderSubscriptionID); p != "" {
		if err := prov.CancelNow(ctx, port.SubscriptionRef{ProviderSubscriptionID: p}, "cancel:"+sub.BillingSubscriptionID+":1"); err != nil {
			return providerErr(err)
		}
	}
	if err := s.locked(ctx, sub.TenantID, func(ctx context.Context) error {
		cur, err := s.Subs.Get(ctx, sub.BillingSubscriptionID)
		if err != nil || cur.Status != domain.StatusIncomplete {
			return err
		}
		return s.endSignup(ctx, cur, "api:B-I1")
	}); err != nil {
		return err
	}
	return domain.NewError(domain.ErrChargeExpired, "3-D Secure was not completed in time; start the signup again")
}

// endSignup closes a failed direct-signup subscription (signup_failed).
func (s *Service) endSignup(ctx context.Context, sub domain.Subscription, cause string) error {
	now := s.now()
	from := sub.Status
	reason := domain.CancelSignupFailed
	ended, err := s.Subs.Transition(ctx, sub.BillingSubscriptionID, 0, domain.StatusEnded, func(x *domain.Subscription) {
		x.EndedAt, x.CancelReason = &now, &reason
	})
	if err != nil {
		return err
	}
	return s.history(ctx, ended, &from, cause, domain.CommercialSystemPrincipal, nil, nil, nil)
}

func (s *Service) itemOf(ctx context.Context, tenantID, subID string) (domain.SubscriptionItem, error) {
	var it domain.SubscriptionItem
	err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		it, err = s.Subs.Item(ctx, subID)
		return err
	})
	return it, err
}

func alreadyExists(subID string) error {
	return domain.NewError(domain.ErrSubscriptionAlreadyExists, "a subscription already exists for this tenant").With("billing_subscription_id", subID)
}

func nonEmpty(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
