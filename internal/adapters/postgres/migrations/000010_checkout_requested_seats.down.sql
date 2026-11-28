-- Fails if a checkout row holds more than 1000 seats; such rows must be
-- expired or removed first.
ALTER TABLE checkout_sessions DROP CONSTRAINT IF EXISTS checkout_sessions_requested_seats_check;
ALTER TABLE checkout_sessions ADD CONSTRAINT checkout_sessions_requested_seats_check
  CHECK (requested_seats IS NULL OR requested_seats BETWEEN 1 AND 1000);
