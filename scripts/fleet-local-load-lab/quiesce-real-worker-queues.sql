\set ON_ERROR_STOP on

-- Historical rows remain visible to the exact health view and SQL workload,
-- but the real worker processes must not interpret synthetic route payloads as
-- signable work. No execution signer is mounted in the component lab.
UPDATE loyal_yield.rebalance_opportunities
SET available_at = clock_timestamp() + interval '1 day'
WHERE cluster = 'localnet'
  AND opportunity_state IN ('ready', 'revalidate');

UPDATE loyal_yield.signed_route_submissions
SET confirmation_available_at = clock_timestamp() + interval '1 day'
WHERE cluster = 'localnet'
  AND submission_state IN ('signed', 'submitted', 'confirmed');
