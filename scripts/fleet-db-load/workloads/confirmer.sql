\set candidate random(1, :submission_rows)
UPDATE loyal_yield.signed_route_submissions submission
SET last_status_checked_at = clock_timestamp(),
    confirmation_attempt_count = confirmation_attempt_count + 1,
    updated_at = clock_timestamp()
WHERE submission.id = (
    SELECT id
    FROM loyal_yield.signed_route_submissions
    WHERE cluster = 'localnet'
      AND submission_state IN ('submitted', 'confirmed')
      AND id >= :candidate
    ORDER BY confirmation_available_at, created_at, id
    LIMIT 1
);
