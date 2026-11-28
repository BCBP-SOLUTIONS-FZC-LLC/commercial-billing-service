package app

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// errConversionUnwinding defers a change while the tenant's conversion is
// being undone (§8.2 step 4, §9.3.1, rev 0.66): the transaction rolls back
// and the webhook, job or emission runs again later. Once B-O8 or B-O9 has
// finished, the subscription is ended and the retry finds nothing to do; if
// the reversal is dropped before RP-4's 200, the retry applies as usual.
var errConversionUnwinding = domain.NewError(domain.ErrRequestInProgress,
	"the tenant's conversion is being reversed or refunded; retried after it")

// coreBound reports whether Org & Membership (Core) applies the event on
// billing-orgm-q, where it would race the conversion revert reset
// (TrialReactivated{reason: conversion_revert}, BL-OQ-27 a).
func coreBound(p domain.EventPayload) bool {
	switch p.(type) {
	case domain.TenantPlanChanged, domain.TenantSeatsChanged, domain.TenantPaymentPastDue,
		domain.TenantSubscriptionCancelled, domain.TenantReactivated:
		return true
	}
	return false
}

// conversionUnwinding reports whether the tenant's live subscription has a
// saga that B-O8 is reverting or B-O9 is refunding. It runs in the caller's
// transaction (the saga row is locked).
func conversionUnwinding(ctx context.Context, subs port.SubscriptionRepo, sagas port.SagaRepo, tenantID string) (bool, error) {
	if sagas == nil {
		return false, nil
	}
	sub, found, err := subs.Live(ctx, tenantID)
	if err != nil || !found {
		return false, err
	}
	saga, found, err := sagas.BySubscription(ctx, sub.BillingSubscriptionID)
	if err != nil || !found {
		return false, err
	}
	return saga.State == domain.SagaReverting || saga.State == domain.SagaRefundPending, nil
}

// emit enqueues a billing.events event; a Core-bound one is deferred while
// the tenant's conversion is being undone (errConversionUnwinding).
func (s *Service) emit(ctx context.Context, p domain.EventPayload, m port.EventMeta) error {
	if coreBound(p) {
		busy, err := conversionUnwinding(ctx, s.Subs, s.Sagas, p.Tenant())
		if err != nil {
			return err
		}
		if busy {
			return errConversionUnwinding
		}
	}
	_, err := s.Events.Enqueue(ctx, p, m)
	return err
}
