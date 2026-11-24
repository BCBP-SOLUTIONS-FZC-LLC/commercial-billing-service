# Paid purchase not active within 60 s

**Alert:** `CommercialPurchaseNotActive` (SEV-2, HLD §8.6.3, §12.4). Composed p99 over 10 min: Billing's confirmation → event (`commercial_purchase_confirmation_to_event_seconds{kind=seats|token_addon}`) + bus propagation on `billing-orgm-q` / `billing-entitlements-q` (`platform_event_propagation_seconds`) + Usage & Metering render latency, above 60 s.

1. Find the slow leg on the Billing dashboard ("Commercial / Billing" → purchases):
   - **Billing leg high:** the event commits in the same transaction as the confirmation (BIL-OPS-8), so a high value means the confirmation itself was late — the paid callback queued. Check [webhook-backlog.md](webhook-backlog.md) and `platform_outbox_pending_events{service="commercial-billing"}`.
   - **Propagation high:** outbox publishing lag (`platform_outbox_*`), SNS, or the consumer side (Core on `billing-orgm-q`, Entitlements on `billing-entitlements-q`). Page the owning team if their queue depth grows.
   - **Render high:** Usage & Metering — page its on-call.
2. A purchase confirmed but not projected leaves `licensed_seats` behind; `seat-projection-check` repairs seats within 30 min (§8.6.4). Token add-ons are re-delivered by the outbox.
3. Never re-charge: confirmations are idempotent per purchase id (BIL-ADD-2, BIL-SEAT-1).
