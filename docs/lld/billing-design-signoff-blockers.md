# Billing LLD — Design Sign-off Blockers by Team

| Field | Value |
|---|---|
| Document | `commercial-lld-billing-service.md` rev 0.18 (base HLD v0.64) |
| Progress | **4 of 9 closed** — S-1, S-2 and S-3 approved by Finance; S-4 approved by Finance and Security (all 2026-11-23) |
| From | Commercial Team (Billing) |
| Date | 2026-11-23 |
| Purpose | Lists the decisions other teams must make before the Billing LLD can be **signed off as a design** |
| Final approvers | Commercial Team (CD-4) for the whole document; PCI owner group for §4, §5.4.4 and §10 |

## Read this first

The Billing LLD tracks three kinds of open item. Only the first kind is in this document.

| Kind | Meaning | Blocks design sign-off? | Where tracked |
|---|---|---|---|
| **Design blocker** (this document) | The LLD already writes a behaviour, rule or schema into the design, marked *Proposed*. The owning team has not yet agreed to it. If they say no, the design text, code or schema changes | **Yes** | Here; LLD §16.1 and §16.2 |
| Go-live gate | Another team must build, configure or deploy something. Billing's design already handles both "not yet" and "done", usually with a feature flag or a `403` / `503` | No | `billing-open-items-by-team.md`, `billing-open-items-entitlements.md` |
| Data or value | A number or price list (currency, prices, pack sizes). Loaded as data; no design change | No | `billing-open-items-by-team.md` |

**What each team needs to do:** for each item addressed to you, reply **Approve** or **Reject with your alternative**. Every item already has a written default, so a reply is enough. No meeting is needed unless you reject.

## Summary

| # | Team | Item | Question in one line | Billing's proposal (current default) | If rejected |
|---|---|---|---|---|---|
| S-1 | Finance | BL-OQ-6 | How are mid-period seat purchases charged when the payment provider can't calculate proration? | ✅ **Approved 2026-11-23, unchanged**, with three record-keeping additions (see S-1). Applied in LLD rev 0.15 | — |
| S-2 | Finance | BL-OQ-1 (CD-24) | Do we keep charging a tenant that an operator suspended? | ✅ **Approved 2026-11-23**, rules 1–4, auto-pause on. Adds new operator route B-O16 (see S-2). Applied in LLD rev 0.16 | — |
| S-3 | Finance | BL-OQ-13 (BIZ-6) | Which roles can do which billing actions? | ✅ **Approved 2026-11-23, unchanged**, plus an overlap rule (see S-3). Applied in LLD rev 0.17 | — |
| S-4 | Finance + Security | BL-OQ-7 (CD-10) | When is a trial-to-paid conversion reversed for fraud? | ✅ **Approved 2026-11-23 by Finance and Security, unchanged** (see S-4). Recorded in LLD rev 0.18 | — |
| S-5 | Finance + Legal | BL-OQ-11 (CD-14) | How long must financial records be kept, per jurisdiction? | Keep everything; no financial deletion job; operational prunes only | Billing adds a retention/deletion job (new design work) |
| S-6 | Product + Finance | BL-OQ-16 | When a price changes, when do existing subscribers pay it? | At their next renewal | Price rows need a grandfathering period; §4.2.2 / B-O11 change |
| S-7 | PCI owner group | BL-OQ-21 + review | Who may mark a payment provider as certified? Plus PCI review of §4, §5.4.4, §10 | A Billing engineer submits, a PCI owner group member reviews; enforced in the database | `provider_certifications` constraint and B-O14 change |
| S-8 | Commercial TL + Entitlements | BL-OQ-23 (c) | Does Entitlements publish any add-on balance event? | No; Audit gets those changes only through AL-5 | Audit Log needs a new LLD decision; event contracts change |
| S-9 | Commercial HLD owner | D-1, D-5, D-8, D-9 | Will the HLD be corrected where the LLD knowingly differs? | LLD follows the source system or TL decision; HLD notes requested | Until fixed, the signed LLD contradicts the signed HLD |

---

## Finance

### S-1 — Proration rule when the provider has no proration engine (BL-OQ-6, CD-54)

**Background.** When a tenant buys extra seats mid-period, they pay only for the rest of the period. Stripe calculates this itself. The LLD also has to support future providers that can't, so Billing has its own calculator (LLD §8.5.3). It runs only for a provider certified with `Proration = fallback`, which Stripe is not.

**Billing's proposal — three rules:**

1. **Formula.** Charge = unit price × seats added × (seconds left in the period ÷ seconds in the period). It is calculated exactly and rounded half-up once to the currency's minor unit (fils / cents). Month lengths and leap years need no special handling.
   *Example:* 5 seats at 69.00, 18 days left in a 30-day month → 207.00.
2. **Below the provider's minimum charge.** The seats are added free for the rest of the period and billed from renewal, recorded as an adjustment. The alternative is to charge the minimum. Default: `PRORATION_BELOW_MINIMUM = free_until_renewal`.
3. **Renewal top-up fails.** If members were added just before renewal and the top-up charge fails, the tenant **keeps the seats** and the unpaid invoice goes into normal dunning (LLD §8.6.3). Removing the seats would leave the tenant above its seat cap in IAM.

**Decision: ✅ Approved by Finance, 2026-11-23. Rules 1, 2 and 3 approved with no changes.**

Finance's reasons:
- **Rule 1:** time-based proration doesn't depend on the provider. It handles month length, leap years, daylight-saving changes and custom periods, and matches how major billing platforms (Stripe, Chargebee) prorate.
- **Rule 2:** charging a provider minimum (for example 0.50 for a 0.03 proration) over-bills the customer. The exposure is negligible because the seats are billed in full from the next renewal.
- **Rule 3:** removing seats would leave IAM with more users than licensed seats, and automatic removal is complex and potentially destructive. An unpaid renewal is a receivable problem, not an entitlement reduction.

Finance asked for three additions. All are in LLD rev 0.15:

| Finance asked for | Where in the LLD |
|---|---|
| Store the raw unrounded calculation **and** the final rounded amount, for audit and disputes | `seat_purchases` stores the inputs (`unit_amount_minor`, `remaining_seconds`, `period_seconds`), `amount_unrounded_minor` and `amount_minor`. The database enforces it for every calculator-priced purchase (§4.2.10, `BIL-SEAT-5`) |
| Below the minimum: record the seat addition, the deferred amount and `reason = below_provider_minimum`, visible in audit records and metrics | The purchase row has `amount_minor = 0`, `deferred_amount_minor` and `deferral_reason = below_provider_minimum`. `TenantSeatsChanged` carries both fields, so Audit's `tenant.seats_changed` entry records them. New metrics `commercial_billing_proration_deferred_total` and `..._deferred_amount_minor_total` (§8.5.3, §7.3.4, §11.2) |
| State that the fallback calculator is used only for providers certified `Proration = fallback`; providers with their own engine (Stripe) remain the system of record | First paragraph of §8.5.3 |

Rule 3's sequence is now written in §8.6.3: renewal invoice → payment fails → invoice overdue → standard dunning → suspension only under the existing lapse rules. Seats change only if the tenant schedules a reduction.

**One design change follows from this.** The earlier text recorded a below-minimum purchase as an `adjustments` row. That table is for operator credits and debits on the provider balance, so it was the wrong place. The deferral is now recorded on the seat purchase itself.

### S-2 — Invoicing while an operator has suspended the tenant (BL-OQ-1, CD-24)

**Background.** An operator can suspend a tenant for abuse, a security issue or a legal hold. The HLD leaves it to Billing to decide whether the subscription keeps charging during that time (LLD §8.10).

**Billing's proposal:**
- When the suspension arrives, Billing **pauses collection automatically**. Stripe still creates renewal invoices, but keeps them as drafts and does not charge them. No dunning runs, and no purchases or plan changes are allowed (`409 COLLECTION_PAUSED`).
- When the operator reactivates the tenant, collection resumes. Drafts created during the pause are **not** charged automatically; an operator finalises or voids them.
- An operator can override either way with B-O10 and a reason.
- Default: `AUTO_PAUSE_ON_OPERATOR_SUSPENSION=true`.

**Why:** these suspensions are usually disputes or investigations, so billing that time should be a human decision.

**Decision: ✅ Approved by Finance, 2026-11-23. Rules 1–4 approved, with `AUTO_PAUSE_ON_OPERATOR_SUSPENSION=true`.**

| Rule | Finance's decision | Finance's reason |
|---|---|---|
| 1. Pause automatically on suspension | Approve. No automated charge, no dunning; no purchases, seat changes, upgrades, downgrades or add-ons (`409 COLLECTION_PAUSED`) | Avoids charging a tenant whose access may be restricted for reasons outside their control, and avoids reversing invoices if the investigation clears them |
| 2. Drafts need review on reactivation | Approve. Collection resumes, but drafts from the pause are never charged automatically; an operator finalises and collects, or voids | Outcomes differ: abuse → collect; false positive → void; legal hold or security compromise → case by case. Automatic charging carries high dispute risk |
| 3. Operator override (B-O10) | Approve, with a **mandatory reason** | Legitimate exceptions exist: contractual or legal obligations to keep billing, enterprise terms, fraud cases where service stays reserved |
| 4. Default on | Approve `true` | A temporary pause is limited and recoverable; billing a suspended customer risks disputes, write-offs, refunds and escalations |

**Finance's clarifying sentence, now in §8.10:** *"Operator suspension pauses collection activity but does not terminate the subscription, alter entitlements, cancel the contract, or waive amounts that were already due before the suspension."*

**Two design choices made while applying this (agreed with the Commercial Team, 2026-11-23):**

1. **New operator route B-O16 for the draft decision.** Rule 2 needs an explicit, auditable decision per draft. The design had no Billing route for it, which would have left operators deciding in the Stripe dashboard, with no reason recorded and no audit entry. B-O16 `POST /api/v1/operator/tenants/:id/invoices/:invoice_id/disposition` takes `finalize` or `void` and a mandatory reason, and writes an AL-5 entry. Supporting changes: `invoices.held_for_review` plus disposition columns, port method `FinalizeInvoice`, invariant `BIL-SUB-7`, a metric and alert `BillingSuspensionDraftsAwaitingDecision` (drafts still undecided 24 h after reactivation). Finalising is refused while collection is still paused.
2. **Invoices already open at suspension stop retrying.** Stripe's pause only holds *new* invoices. An invoice already `past_due` would keep retrying and could cancel the subscription through dunning, contradicting "no dunning" and "does not terminate the subscription". So while paused, automatic retries on those invoices stop and the dunning clock freezes. They stay open and owed (not waived), and retries restart on resume.

**Follow-up for IAM (new BL-OQ-25, go-live gate, not a design blocker):** add B-O16 to the OP-G4 operator test and add the Audit entry type `billing.invoice.suspension_disposition`.

### S-3 — Billing permissions (BL-OQ-13, BIZ-6)

**Background.** The HLD defines four billing permissions. Some rows of its role matrix ("owner only") had no permission, so the LLD adds two more and maps every action to a permission (LLD §5.2).

| Permission | What it allows | Owner | Tenant admin |
|---|---|---|---|
| `commercial.subscription.manage` | Buy seats, schedule or cancel a seat reduction, buy a token add-on | ✓ | ✓ |
| `commercial.subscription.cancel` | Cancel or resume the subscription | ✓ | |
| `commercial.billing.view` | View plan, subscription, payment methods, purchases, history | ✓ | ✓ |
| `commercial.billing.download_invoice` | View and download invoices | ✓ | ✓ |
| `commercial.subscription.change_plan` **(new)** | Start a paid subscription or convert a trial, change tier, schedule a downgrade, reactivate | ✓ | |
| `commercial.payment_method.manage` **(new)** | Add or update a payment method, edit the billing contact | ✓ | |

Tender admins and members get none of these.

**Decision: ✅ Approved by Finance, 2026-11-23. Six permissions and role mapping approved without changes.**

| Role | Permissions | Finance's reason |
|---|---|---|
| Owner | All six | The legal and commercial authority for the tenant |
| Tenant admin | `subscription.manage`, `billing.view`, `billing.download_invoice` | Operational management and billing visibility only. Cancelling, changing plan and payment methods can create or end commitments, affect revenue or change who pays, so they stay owner-only (least privilege) |
| Tender admin, member | None | Procurement and workflow roles; mixing them with billing authority risks unauthorised commercial actions |

Finance noted that the model gives complete coverage (every billing action maps to exactly one permission), separates operational, commercial and payment authority, and allows custom or delegated finance roles later without redesigning Billing's authorization.

**Finance's overlap rule, now in §5.2:** *"Changing a payment method used for future invoices, updating billing-contact information, and initiating any action that can create a new contractual commitment require `commercial.payment_method.manage` or `commercial.subscription.change_plan` respectively, regardless of any other billing permissions."*

**One interpretation, written into §5.2 so the rule can't conflict with the approved table:** a "new contractual commitment" means starting a paid subscription or converting a trial, changing tier (including scheduling a downgrade) and reactivating. Buying seats or add-ons within the current subscription, and scheduling a seat reduction, stay under `subscription.manage`, which Finance approved for tenant admins. New invariant `BIL-AUTH-7`: the two owner-only permissions are never implied by any other permission. No route or permission changed.

### S-4 — Conversion reversal for fraud (BL-OQ-7, CD-10) — Finance + Security

**Background.** If a trial converts to paid with a stolen card, the tenant should go back to the trial realm and the money should be returned. The HLD says the trigger is Billing's to define (LLD §8.2).

**Billing's proposal:**
- **Trigger:** a dispute with reason `fraudulent`, or a Stripe early fraud warning, **on the conversion payment**, **within 7 days** of conversion. Other disputes are handled in the provider dashboard and never reverse a conversion.
- **Alert:** SEV-2 `BillingReversalDecisionNeeded` to the operator.
- **Decision:** an operator approves or declines (B-O8). It is **never automatic** in Phase 1. `AUTO_REVERT_ON_EFW=false` could later allow it for early fraud warnings only.
- **Effect if approved:** IAM moves the tenant back to the trial realm (RP-4), then Billing cancels the subscription and refunds, unless the dispute already returned the money. Everything is audited.
- After 7 days, IAM refuses the reversal and it becomes a manual runbook.

**Decision needed:**
- **Finance:** approve the trigger, the 7-day window and the refund rule.
- **Security:** approve SEV-2 severity and operator-only approval.

**Decision: ✅ Approved by Finance and Security, 2026-11-23, with no changes.**

| Team | Approved | Reason given |
|---|---|---|
| Finance | Trigger: dispute reason `fraudulent` or early fraud warning | The goal is to reverse fraudulent conversions, not ordinary commercial disputes ("product not received", "unsatisfactory", billing complaints), which are customer-service matters |
| Finance | Conversion payment only | A dispute on a later renewal can never revert an established tenant |
| Finance | 7-day window; runbook after that | Deterministic; the tenant is still close to its trial state, and operators never have to debate 30-, 60- or 90-day-old conversions |
| Finance | Refund unless the dispute already returned the funds | No double reimbursement; Stripe stays the source of truth for chargeback outcomes |
| Security | SEV-2 `BillingReversalDecisionNeeded` | Possible payment fraud, abuse and financial loss, but the platform keeps running, data is not necessarily compromised, and there is no platform-wide impact |
| Security | Operator-only approval through B-O8 | Early fraud warnings can be wrong, and a reversal has several consequential effects (realm migration, cancellation, refund, audit) |
| Security | `AUTO_REVERT_ON_EFW=false` in Phase 1 | Automatic handling of EFW-only cases may be considered later, once fraud and false-positive rates are measured |

Nothing in the design changed. LLD rev 0.18 records the approval and reasons in §8.2 and closes BL-OQ-7 and CD-10.

### S-5 — Financial-record retention (BL-OQ-11, CD-14) — Finance + Legal

**Background.** Invoices, refunds, adjustments, disputes and subscription history must be kept for a legal period that depends on jurisdiction (UAE first). The same answer sets how long Workflow history is kept (LLD §15.3).

**Billing's default:** keep all financial records indefinitely. There is **no** financial deletion job, only operational prunes (outbox, idempotency keys). The setting `FINANCIAL_RECORD_RETENTION_YEARS` exists but is unused. Billing does not delete financial records when a tenant is purged.

**Why it blocks:** if the answer requires deletion or minimisation after N years, Billing must add a retention job to the design. That is new design work, not a configuration value.

**Decision needed:** the retention period per jurisdiction, and whether records are deleted or only minimised at the end of it.

---

## Product (with Finance)

### S-6 — Price changes for existing subscribers (BL-OQ-16)

**Background.** Prices live in `plan_prices`. A price change adds a new row and closes the old one (LLD §4.2.2, B-O11).

**Billing's proposal:** existing subscribers move to the new price at their **next renewal**. New subscribers pay it immediately.

**Alternative:** grandfather existing subscribers for longer, for example 12 months or until they change plan. That needs a grandfathering period on price rows and logic to apply it at renewal.

**Decision needed:** approve "next renewal", or define the grandfathering rule.

---

## PCI owner group

### S-7 — Who signs a provider certification, plus PCI review (BL-OQ-21)

**Background.** Before Billing uses a payment provider adapter for new customers, the adapter must pass a certification test suite (LLD §3.5, B-O14). This is a PCI-relevant control.

**Billing's proposal:** a Commercial Team (Billing) engineer submits the run, and a **different person** from the PCI owner group reviews it. This matches the rule for production changes to Billing (HLD §15.4). It is enforced twice:
- in the database: a run cannot be `passed` without `reviewed_by` (`chk_pc_passed_reviewed`);
- in the API: `reviewed_by` must differ from the submitter (`422` otherwise).

**Decision needed:**
1. Approve the submitter/reviewer rule.
2. Complete the PCI review of LLD §4 (data model), §5.4.4 (operator routes) and §10 (security). This review is part of sign-off, not a separate step.

---

## Commercial TL (with Entitlements)

### S-8 — Does Entitlements publish an add-on balance event? (BL-OQ-23 c)

**Background.** Our 19 Nov corrections note said Entitlements *could* publish its own balance event (for example `TenantAddOnBalanceChanged`) for goodwill grants, corrections and expiry. IAM then pointed out that Audit records **everything** on Entitlements' event topic. Such an event would be stored as `entitlement.unknown`, raise an alarm and duplicate audit entries Entitlements already writes through AL-5.

**Options:**
1. **No add-on balance event on `entitlement-events`; AL-5 is the only audit path.** Recommended by Billing and IAM. Nothing else changes.
2. Publish one. This needs a named consumer, and Audit Log needs a new LLD decision to skip or map it.

**Why it blocks:** it fixes the event contracts between Billing, Entitlements and Audit that the LLD relies on (§7.3.6, §8.11).

**Decision needed:** confirm option 1. Details: `billing-open-items-entitlements.md` E-3.

---

## Commercial HLD owner

### S-9 — HLD notes for known LLD/HLD differences (D-1, D-5, D-8, D-9)

**Background.** In four places the LLD knowingly differs from the HLD, because it follows the system actually built or a later TL decision. The LLD's rule is that the HLD wins in a conflict, so these differences must be acknowledged in the HLD before both are signed.

| # | HLD says | LLD does | Why |
|---|---|---|---|
| D-1 | `tenant_id` only in the event envelope (§9.4) | Also in `data` | Realm Provisioner's consumer requires `data.tenant_id` |
| D-5 | `platform-gincommon` v1.6.0 (§3.3) | v2 | Same RFC 9457 wire format; aligned with CD-31's target |
| D-8 | Reserved correction reasons on `TenantQuotaAddOnChanged` (§9.4) | `purchase` \| `refund` only | TL decision BL-OQ-20: corrections belong to Entitlements |
| D-9 | CD-52 row numbers the gateway provider OP-G3 and the end-to-end test OP-G5 | OP-G2 and OP-G4 | Follows the AuthZ Enrichment LLD §8.9.1, which is the source |

**Decision needed:** accept the four notes and schedule them into the next HLD revision. The notes can follow sign-off, but the acceptance cannot.

---

## Suggested order

1. **Send this week:** S-1 to S-6 to Finance, Product and Legal as one pack (they share BL-OQ-7, 11 and 16), S-7 to the PCI owner group, S-8 to the TL and S-9 to the HLD owner.
2. **Rejections** become LLD changes in rev 0.15. Approvals change the item's status from *Proposed* to *Closed* in §16.1.
3. **Sign-off** happens when every row below says Approved. Go-live gates stay open and are tracked separately.

## Tracker

| # | Team | Approver | Reply | Date |
|---|---|---|---|---|
| S-1 | Finance | Finance | ✅ Approved, rules 1–3 unchanged; three record-keeping additions applied (LLD rev 0.15) | 2026-11-23 |
| S-2 | Finance | Finance | ✅ Approved, rules 1–4, auto-pause `true`; B-O16 and retry freeze added (LLD rev 0.16) | 2026-11-23 |
| S-3 | Finance | Finance | ✅ Approved unchanged; overlap rule and `BIL-AUTH-7` added (LLD rev 0.17) | 2026-11-23 |
| S-4 | Finance | Finance | ✅ Approved unchanged: trigger, conversion-payment scope, 7-day window, refund rule (LLD rev 0.18) | 2026-11-23 |
| S-4 | Security | Security | ✅ Approved unchanged: SEV-2, operator-only B-O8, `AUTO_REVERT_ON_EFW=false` (LLD rev 0.18) | 2026-11-23 |
| S-5 | Finance | | | |
| S-5 | Legal | | | |
| S-6 | Product | | | |
| S-6 | Finance | | | |
| S-7 | PCI owner group | | | |
| S-8 | Commercial TL | | | |
| S-8 | Entitlements | | | |
| S-9 | Commercial HLD owner | | | |
