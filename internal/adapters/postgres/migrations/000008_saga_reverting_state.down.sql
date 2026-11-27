-- 000008 down: PostgreSQL cannot drop an enum value. 000009's down moves any
-- 'reverting' saga back to 'confirmed', so the value is left unused.
SELECT 1;
