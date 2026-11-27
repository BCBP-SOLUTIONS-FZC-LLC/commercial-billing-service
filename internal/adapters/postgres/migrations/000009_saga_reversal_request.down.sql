-- 000009 down: back to 000001's columns and due index. A saga still
-- 'reverting' returns to 'confirmed' (the operator repeats B-O8).
UPDATE conversion_sagas SET state = 'confirmed', next_attempt_at = NULL WHERE state = 'reverting';
DROP INDEX IF EXISTS idx_saga_due;
CREATE INDEX idx_saga_due ON conversion_sagas (next_attempt_at) WHERE state = 'rp_convert_pending';
ALTER TABLE conversion_sagas
  DROP CONSTRAINT IF EXISTS chk_saga_reverting,
  DROP COLUMN IF EXISTS reversal_rp_done_at,
  DROP COLUMN IF EXISTS reversal_dispute_id,
  DROP COLUMN IF EXISTS reversal_refund,
  DROP COLUMN IF EXISTS reversal_reason,
  DROP COLUMN IF EXISTS reversal_requested_by,
  DROP COLUMN IF EXISTS reversal_requested_at;
