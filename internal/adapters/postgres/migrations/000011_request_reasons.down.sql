ALTER TABLE reconciliation_runs DROP COLUMN IF EXISTS reason;
ALTER TABLE subscription_status_history
  DROP COLUMN IF EXISTS cancel_request_comment,
  DROP COLUMN IF EXISTS cancel_request_reason;
