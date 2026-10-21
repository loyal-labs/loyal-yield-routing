-- The Backyard capital-recovery guard no longer exempts a manual_recovery
-- row carrying a `manualResolution` disposition, and the journal readers no
-- longer hide the strategy-one rows that 0080 labelled `journalAssociation`.
-- This migration removes what those two exemptions covered.
--
-- 1. The superseded incident row of 0078 (disposition
--    superseded_by_strategy_reset, matched with the same checks the guard
--    used) is deleted. Without the exemption it would stop capital on its
--    route for good. Only a row that satisfied the exemption in full is
--    touched.
-- 2. The strategy-one rows of the four Prime route steps are deleted; no
--    reconciled row of those actions exists past the strategy reset slot.
--    reconciled and failed rows only: a manual_recovery row is never deleted
--    here.
-- 3. A REPORT_NAV row left in manual_recovery with the unclassified marker
--    moves nothing and blocks nothing (a report cannot move capital, and the
--    guard lists only capital actions). It stays manual_recovery, its
--    receipt having never been proven to the failure settlement, and gets an
--    explicit disposition instead of a silent edit. updated_at is untouched:
--    the decision epoch orders by it.
--
-- Apply before the binary that stops reading both exemptions: that binary
-- would stop capital on the superseded row until it is gone. The previous
-- binary treats a deleted row as absent.
DELETE FROM loyal_yield.multiply_operations
WHERE engine_version = 'backyard_rwa_v1'
  AND status = 'manual_recovery'
  AND expected_effects->'manualResolution'->>'schema' = 'backyard-manual-resolution/v1'
  AND expected_effects->'manualResolution'->>'disposition' = 'superseded_by_strategy_reset'
  AND expected_effects->'manualResolution'->>'operationId' = operation_id
  AND expected_effects->'manualResolution'->>'routeKey' = route_key
  AND expected_effects->'manualResolution'->>'action' = action
  AND expected_effects->'manualResolution'->>'signature' = transaction_signature
  AND expected_effects->'manualResolution'->>'confirmedSlot' = confirmed_slot::text
  AND expected_effects->'manualResolution'->>'evidenceSha256' ~ '^[0-9a-f]{64}$'
  AND length(expected_effects->'manualResolution'->>'evidencePath') > 0;

DELETE FROM loyal_yield.multiply_operations
WHERE engine_version = 'backyard_rwa_v1'
  AND route_key = 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh'
  AND status IN ('reconciled', 'failed')
  AND action IN ('SWAP_USDC_TO_PRIME_STEP', 'SWAP_PRIME_TO_USDC_STEP', 'OPEN_PRIME_USDC_STEP', 'DELEVER_PRIME_USDC_STEP')
  AND confirmed_slot BETWEEN 1 AND 444157954
  AND expected_effects->>'journalStrategyConfig' = '9hDH4acTDrSjg9d5n8c1g53jMTonaDAUesp1diCWuuhj'
  AND expected_effects->'journalAssociation'->>'schema' = 'backyard-strategy-association/v1'
  AND expected_effects->'journalAssociation'->>'evidenceSha256' = '3319db2f57baf743be2471d72a1e9d34168a81ebc0378071f7a7f28e1d968a64';

UPDATE loyal_yield.multiply_operations
SET expected_effects = jsonb_set(expected_effects, '{manualResolution}', jsonb_build_object(
    'schema', 'backyard-manual-resolution/v1',
    'disposition', 'report_only_left_in_manual_recovery',
    'operationId', operation_id, 'routeKey', route_key, 'action', action,
    'signature', transaction_signature, 'confirmedSlot', confirmed_slot), true)
WHERE engine_version = 'backyard_rwa_v1'
  AND status = 'manual_recovery'
  AND action = 'REPORT_NAV'
  AND recovery_reason = 'confirmed_transaction_error'
  AND NOT (expected_effects ? 'manualResolution');
