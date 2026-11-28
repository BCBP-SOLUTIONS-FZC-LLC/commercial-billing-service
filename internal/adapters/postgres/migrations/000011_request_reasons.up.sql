-- 000011_request_reasons (rev 0.73): two request reasons that were validated
-- and then dropped are kept.
-- (1) B-P12 keeps the owner's cancellation reason and comment on the
--     status-history row of the cancellation (§4.2.6, D-L5). Tenant text,
--     never card data; kept with the subscription's history.
ALTER TABLE subscription_status_history
  ADD COLUMN cancel_request_reason  text CHECK (cancel_request_reason IN
    ('too_expensive', 'missing_features', 'switching_vendor', 'no_longer_needed', 'other')),
  ADD COLUMN cancel_request_comment text CHECK (length(cancel_request_comment) <= 500);
-- (2) B-O5 keeps the operator's mandatory reason on the run (§4.2.21, C-5);
--     it is also in the run's commercial.cross_tenant_job.ran audit entry.
--     Nullable: runs queued before this migration have none.
ALTER TABLE reconciliation_runs
  ADD COLUMN reason text CHECK (length(reason) BETWEEN 10 AND 1000);
