-- 000009_saga_reversal_request: the B-O8 request a 'reverting' saga carries
-- (§4.2.8, §8.2, rev 0.63), so the saga worker can complete the reversal
-- after B-O8 returned 503, and a repeat after the 7-day window is not refused.
ALTER TABLE conversion_sagas
  ADD COLUMN reversal_requested_at timestamptz,
  ADD COLUMN reversal_requested_by uuid,            -- operator sub of the B-O8 call
  ADD COLUMN reversal_reason       text,
  ADD COLUMN reversal_refund       boolean,
  ADD COLUMN reversal_dispute_id   uuid,
  -- RP-4 answered 200: never call RP-4 again for this request and never
  -- drop it; only the Billing side (cancel, refund, final commit) is left.
  ADD COLUMN reversal_rp_done_at   timestamptz,
  ADD CONSTRAINT chk_saga_reverting CHECK ((state = 'reverting') <=
    (reversal_requested_at IS NOT NULL AND reversal_requested_by IS NOT NULL
     AND reversal_reason IS NOT NULL AND reversal_refund IS NOT NULL));

-- The saga worker's due scan now covers RP-4 retries too.
DROP INDEX IF EXISTS idx_saga_due;
CREATE INDEX idx_saga_due ON conversion_sagas (next_attempt_at)
  WHERE state IN ('rp_convert_pending', 'reverting');
