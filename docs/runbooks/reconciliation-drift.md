# Reconciliation drift

**Alert:** `BillingReconciliationDrift` (SEV-3): findings of the last reconciliation run not repaired for 24 h. LLD §20.3, §8.13.

Read the run with B-O6 `GET /api/v1/operator/billing/reconciliation-runs/:run_id` (findings, `auto_repaired`, `repair_note`).

| Finding | Action |
|---|---|
| `missing_ledger_event` (not auto-repaired) | Re-run with `auto_repair=true` (B-O5), or use the provider's resend |
| `status_mismatch` | Auto-repair re-reads a subscription the provider ended. Otherwise a replay (B-O13) or the next callback applies it; if Billing is right (e.g. a provider dashboard edit), correct the provider and re-run |
| `seats_mismatch` / `plan_mismatch` | Check for unpaid pending updates or manual dashboard edits; align the provider to Billing's record (Billing is the record of the subscription, CD-56); re-run |
| `invoice_missing` / `invoice_mismatch` | Auto-repair mirrors it; if amounts truly differ, Finance review |
| `orphan_provider_object` | Cancel or link after review; never auto-delete |
| `unknown_customer` | Objects created outside Billing; close them in the provider |

A failed repair is noted on the finding (`repair_note`) and never fails the run.
