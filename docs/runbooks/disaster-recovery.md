# Disaster recovery

RTO 30 min, RPO 5 min (HLD §13.1). LLD §20.6.

After a restore to time T:
1. Start Billing with webhooks paused: B-W1 answers `503` (scale the gateway webhook route to zero or block it), so Stripe retries.
2. Run a reconciliation from T − 1 h with `auto_repair=true` (B-O5): it ledgers provider events missed since T.
3. Resume B-W1.
4. `saga-sweeper` and `seat-projection-check` repair downstream drift; run `seat-projection-check` on demand (`kubectl create job --from=cronjob/commercial-billing-seat-projection-check …`).
5. Compare Core's `licensed_seats` for all tenants with Billing's (the projection check does this).
