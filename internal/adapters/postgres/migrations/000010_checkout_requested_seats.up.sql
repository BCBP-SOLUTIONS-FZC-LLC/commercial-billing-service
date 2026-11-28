-- 000010_checkout_requested_seats: a B-P14 reactivation checkout carries the
-- subscription's seat count, which can be up to 100000 (subscription_items),
-- so the checkout row's bound matches it (§4.2.7, rev 0.71, F-13 / D-L1).
-- Requests still accept at most 1000 seats where their routes say so.
ALTER TABLE checkout_sessions DROP CONSTRAINT IF EXISTS checkout_sessions_requested_seats_check;
ALTER TABLE checkout_sessions ADD CONSTRAINT checkout_sessions_requested_seats_check
  CHECK (requested_seats IS NULL OR requested_seats BETWEEN 1 AND 100000);
