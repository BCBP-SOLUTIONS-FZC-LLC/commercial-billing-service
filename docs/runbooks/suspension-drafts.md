# Held drafts awaiting a decision

**Alert:** `BillingSuspensionDraftsAwaitingDecision` (SEV-3, Finance / operator queue): drafts held during an operator suspension (`held_for_review`, no `disposition`) are still undecided 24 h after collection resumed. LLD §8.10, BIL-SUB-7.

Drafts held while collection was paused are never finalised or charged automatically after reactivation. An operator decides each one:

1. List the tenant's held drafts: B-O4 `GET /api/v1/operator/tenants/:id/invoices` (filter held). The alert carries no tenant (no tenant labels on metrics), so query `invoices` with `held_for_review AND disposition IS NULL` (admin_readonly) to find them.
2. With Finance, decide per draft: B-O16 `POST /api/v1/operator/tenants/:id/invoices/:invoice_id/disposition` with `action: finalize` (charge it; refused with `409 COLLECTION_PAUSED` while billing is still paused) or `action: void`, and a reason.
3. Each decision writes AL-5 `billing.invoice.suspension_disposition` and counts `commercial_billing_invoice_dispositions_total`.
