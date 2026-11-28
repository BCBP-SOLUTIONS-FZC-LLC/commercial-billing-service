package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// ── B-O4 ───────────────────────────────────────────────────────────────────

// ListInvoicesForOperator is B-O4: as B-P17 including drafts, void and
// uncollectible, with refunds.
func (s *Service) ListInvoicesForOperator(ctx context.Context, _ Caller, tenantID string, f InvoiceFilter) (List[OperatorInvoiceView], error) {
	limit := f.Limit
	if limit == 0 {
		limit = 20
	}
	out := List[OperatorInvoiceView]{Items: []OperatorInvoiceView{}}
	err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		rows, err := s.Invoices.List(ctx, tenantID, f.Status, f.Kind, f.Cursor, true, limit+1)
		if err != nil {
			return err
		}
		for i, r := range rows {
			if i == limit {
				out.NextCursor = ptr(rows[i-1].InvoiceID)
				break
			}
			v := OperatorInvoiceView{InvoiceView: invoiceView(r), Links: []InvoiceLink{}, Refunds: []RefundView{}}
			refunds, err := s.Money.RefundsForInvoice(ctx, r.InvoiceID)
			if err != nil {
				return err
			}
			for _, rf := range refunds {
				v.Refunds = append(v.Refunds, RefundView{RefundID: rf.RefundID, Status: rf.Status, Category: rf.Category,
					Amount: MoneyFrom(domain.Money{AmountMinor: rf.AmountMinor, Currency: rf.Currency})})
			}
			out.Items = append(out.Items, v)
		}
		return nil
	})
	return out, err
}

// ── B-O7 ───────────────────────────────────────────────────────────────────

// GetSubscriptionDetail is B-O7: everything an operator needs; ledger rows
// never carry payloads.
func (s *Service) GetSubscriptionDetail(ctx context.Context, c Caller, tenantID string) (OperatorSubscriptionView, error) {
	var out OperatorSubscriptionView
	err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		acct, err := s.Accounts.Get(ctx, tenantID)
		if err != nil {
			return err
		}
		sub, found, err := s.Subs.Latest(ctx, tenantID)
		if err != nil {
			return err
		}
		if !found {
			return domain.NewError(domain.ErrSubscriptionNotFound, "the tenant has no subscription")
		}
		if out.SubscriptionView, err = s.subscriptionView(ctx, c, sub, &acct); err != nil {
			return err
		}
		out.Account, _ = json.Marshal(map[string]any{"provider": acct.Provider, "currency": acct.Currency,
			"billing_contact_name": acct.BillingContactName, "billing_contact_email": acct.BillingContactEmail, "company_name": acct.CompanyName,
			"country": acct.Country, "iam_lifecycle": acct.IAMLifecycle, "iam_suspension_source": acct.IAMSuspensionSource,
			"closed_at": acct.ClosedAt, "default_pm_set": acct.DefaultPMSet})
		if saga, ok, err := s.Sagas.BySubscription(ctx, sub.BillingSubscriptionID); err != nil {
			return err
		} else if ok {
			out.Saga, _ = json.Marshal(saga)
		}
		out.PlanChanges, out.SeatChangeSchedules = []PlanChangeView{}, []ScheduleView{}
		if ch, ok, err := s.Changes.Open(ctx, tenantID); err != nil {
			return err
		} else if ok {
			out.PlanChanges = append(out.PlanChanges, PlanChangeView{ChangeID: ch.ChangeID, Direction: string(ch.Direction),
				FromPlan: string(ch.FromPlan), ToPlan: string(ch.ToPlan), Status: string(ch.Status), EffectiveAt: ch.EffectiveAt, Blockers: []Blocker{}})
		}
		if sc, ok, err := s.Schedules.Latest(ctx, sub.BillingSubscriptionID); err != nil {
			return err
		} else if ok {
			out.SeatChangeSchedules = append(out.SeatChangeSchedules, ScheduleView{ScheduleID: sc.ScheduleID, TargetSeats: sc.TargetSeats,
				EffectiveAt: sc.EffectiveAt, Status: string(sc.Status)})
		}
		out.SeatPurchases, out.QuotaAddonPurchases = []PurchaseView{}, []PurchaseView{}
		seats, err := s.SeatPurchases.List(ctx, tenantID, "", 50)
		if err != nil {
			return err
		}
		for _, p := range seats {
			out.SeatPurchases = append(out.SeatPurchases, seatView(p))
		}
		addons, err := s.Addons.List(ctx, tenantID, "", 50)
		if err != nil {
			return err
		}
		for _, p := range addons {
			out.QuotaAddonPurchases = append(out.QuotaAddonPurchases, addonView(p))
		}
		disputes, err := s.Money.Disputes(ctx, tenantID)
		if err != nil {
			return err
		}
		out.Disputes = make([]json.RawMessage, 0, len(disputes))
		for _, d := range disputes {
			b, _ := json.Marshal(d)
			out.Disputes = append(out.Disputes, b)
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	out.Ledger = []LedgerRowView{}
	rows, err := s.Ledger.Excerpt(ctx, tenantID, 50)
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		out.Ledger = append(out.Ledger, LedgerRowView{ProviderEventID: r.ProviderEventID, EventType: r.EventType, Status: r.Status,
			Outcome: r.Outcome, ReceivedAt: r.ReceivedAt})
	}
	return out, nil
}

// ── B-O5 / B-O6 and the run worker (§8.13) ────────────────────────────────

// StartReconciliation is B-O5: queue a run (one per provider, ≤ 35 days).
func (s *Service) StartReconciliation(ctx context.Context, c Caller, r ReconciliationRequest) (ReconciliationRunView, error) {
	if r.WindowEnd.Sub(r.WindowStart) > 35*24*time.Hour {
		return ReconciliationRunView{}, domain.NewError(domain.ErrValidation, "the window spans at most 35 days")
	}
	run := port.ReconRun{RunID: newID(), Provider: r.Provider, WindowStart: r.WindowStart.UTC(), WindowEnd: r.WindowEnd.UTC(),
		AutoRepair: r.AutoRepair, StartedBy: c.UserID}
	if err := s.Recon.Queue(ctx, run); err != nil {
		return ReconciliationRunView{}, err
	}
	return ReconciliationRunView{RunID: run.RunID, Status: "queued"}, nil
}

// GetReconciliationRun is B-O6.
func (s *Service) GetReconciliationRun(ctx context.Context, _ Caller, runID string) (ReconciliationRunView, error) {
	run, findings, found, err := s.Recon.Get(ctx, runID)
	if err != nil {
		return ReconciliationRunView{}, err
	}
	if !found {
		return ReconciliationRunView{}, domain.NewError(domain.ErrRunNotFound, "reconciliation run not found")
	}
	out := ReconciliationRunView{RunID: run.RunID, Status: run.Status, Counts: run.Counts, Findings: []ReconciliationFinding{}}
	for _, f := range findings {
		v := ReconciliationFinding{TenantID: f.TenantID, Kind: f.Kind, BillingValue: f.BillingValue, ProviderValue: f.ProviderValue, AutoRepaired: f.AutoRepaired}
		if f.RepairNote != nil {
			v.RepairNote = *f.RepairNote
		}
		out.Findings = append(out.Findings, v)
	}
	return out, nil
}

// providerStatus maps a provider subscription status to Billing's.
func providerStatus(st string) domain.SubscriptionStatus {
	switch st {
	case "active", "trialing":
		return domain.StatusActive
	case "past_due", "unpaid":
		return domain.StatusPastDue
	case "canceled":
		return domain.StatusCancelled
	case "incomplete":
		return domain.StatusIncomplete
	}
	return domain.StatusEnded // incomplete_expired
}

// RunReconciliation claims one queued run and compares the provider's
// subscriptions, invoices and events in the window with Billing's records
// (§8.13). With auto_repair it repairs what the §8.13 table allows: a
// missing ledger event is ledgered (the processor then applies it), an
// ended provider subscription is re-read through the processor
// (BIL-PAY-4), a missing invoice is mirrored. The rest are reported.
// Returns 1 when a run was processed.
func (s *Service) RunReconciliation(ctx context.Context, _ int) (int, error) {
	run, ok, err := s.Recon.Claim(ctx, s.Settings.WorkerName)
	if err != nil || !ok {
		return 0, err
	}
	counts, err := s.reconcile(ctx, run)
	status := "completed"
	var errText *string
	if err != nil {
		status, errText = "failed", ptr(err.Error())
	}
	ctx = context.WithoutCancel(ctx)
	outcome := "success"
	if status == "failed" {
		outcome = "failure"
	}
	s.metrics().CrossTenantJobRun("reconciliation", outcome)
	return 1, s.SysTx.RunInSystemTx(ctx, func(ctx context.Context) error {
		if err := s.Recon.Finish(ctx, run.RunID, status, counts, errText); err != nil {
			return err
		}
		return s.platformAudit(ctx, port.AuditEntry{EntryType: "commercial.cross_tenant_job.ran", Action: "reconciliation." + status,
			TargetType: "reconciliation_run", TargetID: run.RunID, SourceEventType: "ReconciliationRunFinished", NaturalID: run.RunID,
			OccurredAt: s.now(), Metadata: map[string]any{"provider": run.Provider, "window_start": run.WindowStart, "window_end": run.WindowEnd,
				"counts": counts, "started_by": run.StartedBy}})
	})
}

func (s *Service) reconcile(ctx context.Context, run port.ReconRun) (map[string]int, error) {
	counts := map[string]int{"subscriptions": 0, "invoices": 0, "events": 0, "findings": 0, "repaired": 0}
	prov, err := s.provider(run.Provider)
	if err != nil {
		return counts, err
	}
	byKind := map[[2]string]int{}
	add := func(f port.ReconFinding) error {
		counts["findings"]++
		if f.AutoRepaired {
			counts["repaired"]++
		}
		byKind[[2]string{f.Kind, strconv.FormatBool(f.AutoRepaired)}]++
		return s.Recon.AddFinding(ctx, run.RunID, f)
	}
	defer func() { s.metrics().ReconciliationFindings(byKind) }()
	// repair applies one auto-repair when the run asks for it; a failure is
	// recorded on the finding, never fails the run.
	repair := func(f *port.ReconFinding, fn func() error) {
		if !run.AutoRepair {
			return
		}
		if err := fn(); err != nil {
			f.RepairNote = ptr("repair failed: " + err.Error())
			return
		}
		f.AutoRepaired = true
	}
	billing, err := s.Recon.BillingSubscriptions(ctx, run.Provider)
	if err != nil {
		return counts, err
	}
	pages := func(kind port.ReconKind, each func([]domain.ProviderObjectState) error) error {
		cursor := ""
		for {
			page, err := prov.ListSince(ctx, kind, run.WindowStart, cursor)
			if err != nil {
				return providerErr(err)
			}
			if err := each(page.Items); err != nil {
				return err
			}
			if page.NextCursor == "" {
				return nil
			}
			cursor = page.NextCursor
		}
	}
	err = pages(port.ReconSubscriptions, func(items []domain.ProviderObjectState) error {
		for _, it := range items {
			counts["subscriptions"]++
			b, known := billing[it.Ref.ID]
			if !known {
				if it.Metadata["billing_subscription_id"] != "" {
					if err := add(port.ReconFinding{TenantID: it.Metadata["tenant_id"], Kind: "orphan_provider_object",
						ProviderValue: mustJSON(map[string]string{"subscription": it.Ref.ID, "status": it.Status})}); err != nil {
						return err
					}
				}
				continue
			}
			want := providerStatus(it.Status)
			got := domain.SubscriptionStatus(b[1])
			if want != got && (want != domain.StatusCancelled || got != domain.StatusEnded) {
				f := port.ReconFinding{TenantID: b[0], Kind: "status_mismatch", BillingValue: mustJSON(map[string]string{"status": b[1]}),
					ProviderValue: mustJSON(map[string]string{"status": it.Status})}
				if want == domain.StatusCancelled || want == domain.StatusEnded {
					repair(&f, func() error { return s.ledgerReread(ctx, run, b[0], it, domain.KindSubscriptionEnded) })
				} else if run.AutoRepair {
					f.RepairNote = ptr("not auto-repaired: the next callback or a replay (B-O13) applies it")
				}
				if err := add(f); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return counts, err
	}
	for _, kind := range []port.ReconKind{port.ReconInvoices, port.ReconEvents} {
		err = pages(kind, func(items []domain.ProviderObjectState) error {
			ids := make([]string, 0, len(items))
			for _, it := range items {
				ids = append(ids, it.Ref.ID)
			}
			known, err := s.Recon.KnownIDs(ctx, run.Provider, string(kind), ids)
			if err != nil {
				return err
			}
			for _, it := range items {
				counts[string(kind)]++
				if known[it.Ref.ID] {
					continue
				}
				findingKind := "missing_ledger_event"
				tenantID := ""
				if kind == port.ReconInvoices {
					// BIL-PAY-3: the tenant is Billing's own customer mapping,
					// never the invoice metadata (rev 0.69). An invoice of a
					// customer Billing doesn't map is not Billing's.
					customer, _ := it.Fields["customer"].(string)
					if customer == "" {
						continue
					}
					t, found, err := s.Customers.TenantForCustomer(ctx, run.Provider, customer)
					if err != nil {
						// Recorded, not repaired; the run goes on (the next run retries).
						note := "tenant lookup failed: " + err.Error()
						f := port.ReconFinding{Kind: "invoice_missing", RepairNote: &note,
							ProviderValue: mustJSON(map[string]string{"id": it.Ref.ID, "status": it.Status})}
						if err := s.Recon.AddFinding(ctx, run.RunID, f); err != nil {
							return err
						}
						counts["findings"]++
						continue
					}
					if !found {
						continue
					}
					if m := it.Metadata["tenant_id"]; m != "" && m != t && s.Log != nil {
						s.logger(ctx).Warn("reconciliation: invoice metadata names another tenant; Billing's customer mapping wins", map[string]any{
							"tenant_id": t, "metadata_tenant_id": m, "provider_invoice_id": it.Ref.ID})
					}
					tenantID, findingKind = t, "invoice_missing"
				}
				f := port.ReconFinding{TenantID: tenantID, Kind: findingKind,
					ProviderValue: mustJSON(map[string]string{"id": it.Ref.ID, "status": it.Status})}
				if kind == port.ReconInvoices {
					repair(&f, func() error { return s.mirrorMissing(ctx, prov, run.Provider, f.TenantID, it.Ref) })
				} else {
					repair(&f, func() error { return s.ledgerMissing(ctx, it, &f) })
				}
				if err := add(f); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return counts, err
		}
	}
	unknown, err := s.Recon.UnknownCustomerEvents(ctx, run.Provider, run.WindowStart, run.WindowEnd)
	if err != nil {
		return counts, err
	}
	for _, id := range unknown {
		if err := add(port.ReconFinding{Kind: "unknown_customer", ProviderValue: mustJSON(map[string]string{"event": id})}); err != nil {
			return counts, err
		}
	}
	return counts, nil
}

// ledgerMissing inserts a provider event found by reconciliation into the
// ledger as received; the processor applies it as a delivery (§8.13).
func (s *Service) ledgerMissing(ctx context.Context, it domain.ProviderObjectState, f *port.ReconFinding) error {
	ev, ok := it.Fields["event"].(domain.PaymentEvent)
	if !ok {
		return errors.New("the provider listing did not include the event body")
	}
	var tenantID *string
	if ev.CustomerRef != "" {
		t, found, err := s.Customers.TenantForCustomer(ctx, ev.Provider, ev.CustomerRef)
		if err != nil {
			return err
		}
		if found {
			tenantID, f.TenantID = &t, t
		}
	}
	var objRef *string
	if ev.Object.ID != "" {
		objRef = &ev.Object.ID
	}
	_, err := s.LedgerStore.Insert(ctx, domain.LedgerEntry{Provider: ev.Provider, ProviderEventID: ev.ProviderEventID, EventType: ev.EventType,
		NormalisedKind: string(ev.Kind), TenantID: tenantID, ProviderObjectRef: objRef, Livemode: ev.Livemode,
		ProviderCreatedAt: ev.ProviderCreatedAt, Payload: ev.Payload})
	return err
}

// ledgerReread queues a re-read of a provider object through the normal
// processor (BIL-PAY-4): a ledger row with a reconciliation id whose
// handler fetches the current state and applies it.
func (s *Service) ledgerReread(ctx context.Context, run port.ReconRun, tenantID string, it domain.ProviderObjectState, kind domain.NormalisedKind) error {
	id := "recon:" + run.RunID + ":" + it.Ref.ID
	payload := mustJSON(map[string]any{"id": id, "type": "reconciliation.reread", "object": map[string]string{"id": it.Ref.ID}})
	_, err := s.LedgerStore.Insert(ctx, domain.LedgerEntry{Provider: run.Provider, ProviderEventID: id, EventType: "reconciliation.reread",
		NormalisedKind: string(kind), TenantID: &tenantID, ProviderObjectRef: &it.Ref.ID, ProviderCreatedAt: s.now(), Payload: payload})
	return err
}

// mirrorMissing upserts a provider invoice missing from the mirror.
func (s *Service) mirrorMissing(ctx context.Context, prov port.PaymentProvider, provider, tenantID string, ref domain.ProviderObjectRef) error {
	if tenantID == "" {
		return errors.New("invoice has no tenant_id metadata")
	}
	ref.Kind = domain.ObjectInvoice
	cur, err := prov.FetchCurrent(ctx, ref)
	if err != nil {
		return providerErr(err)
	}
	return s.locked(ctx, tenantID, func(ctx context.Context) error {
		_, _, err := s.mirrorFromState(ctx, tenantID, provider, domain.PaymentEvent{Provider: provider}, cur)
		return err
	})
}

// voidOpenInvoices voids the tenant's open invoices at offboarding where
// the provider allows it (§8.8.4); the rest stay for the provider to mark
// uncollectible.
func (s *Service) voidOpenInvoices(ctx context.Context, prov port.PaymentProvider, tenantID string) {
	var rows []port.InvoiceRow
	_ = s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		rows, err = s.Invoices.List(ctx, tenantID, "open", "", "", false, 100)
		return err
	})
	for _, r := range rows {
		var inv domain.Invoice
		_ = s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
			var err error
			inv, _, err = s.Invoices.Get(ctx, r.InvoiceID)
			return err
		})
		if inv.ProviderInvoiceID == "" {
			continue
		}
		if err := prov.VoidInvoice(ctx, port.InvoiceRef{ProviderInvoiceID: inv.ProviderInvoiceID}, "void:"+inv.InvoiceID+":offboarded"); err != nil && s.Log != nil {
			s.logger(ctx).Warn("open invoice not voided at offboarding; left for the provider", map[string]any{"tenant_id": tenantID,
				"invoice_id": inv.InvoiceID, "error": fmt.Sprint(providerErr(err))})
		}
	}
}
