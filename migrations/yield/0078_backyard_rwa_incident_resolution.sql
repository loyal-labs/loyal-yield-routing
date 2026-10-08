-- Preserve the original failed reconciliation, wire, status and timestamps.
-- The strategy-two reset independently superseded this exact strategy-one
-- accounting incident. This is a disposition, not a successful reconciliation.
-- Evidence: docs/evidence/backyard-rwa-strategy2/phase1-postconditions.json
-- SHA256: 775ed5199a802027095347865333969427138858ab4a293ab26c5cef74f353cc
DO $$
BEGIN
 IF EXISTS (SELECT 1 FROM loyal_yield.multiply_operations
   WHERE operation_id='fe45a0369bf950da3ea311a4c493377cf9720a92c359c0bfbe739a3d9f699cbe'
   AND NOT (route_key='rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh'
     AND status='manual_recovery' AND action='VOLTR_RESTORE_IDLE'
     AND transaction_signature='46UBvSw1zjtZyDVUVaissm9SEXsKFKnYCQYKd23njb1NS1Ktkzsup5ic9XA55FxyTCpkoYuuM8hhn4MioGU2X7Wz'
     AND confirmed_slot=444157954 AND recovery_reason='exact_effect_reconciliation_failed')) THEN
   RAISE EXCEPTION 'strategy-one incident identity differs from reviewed reset evidence';
 END IF;
 UPDATE loyal_yield.multiply_operations
 SET expected_effects=jsonb_set(expected_effects,'{manualResolution}',jsonb_build_object(
   'schema','backyard-manual-resolution/v1',
   'disposition','superseded_by_strategy_reset',
   'operationId',operation_id,'routeKey',route_key,'action',action,
   'signature',transaction_signature,'confirmedSlot',confirmed_slot,
   'evidencePath','docs/evidence/backyard-rwa-strategy2/phase1-postconditions.json',
   'evidenceSha256','775ed5199a802027095347865333969427138858ab4a293ab26c5cef74f353cc',
   'resetCompletedAt','2026-09-09T22:20:00Z'),true)
 WHERE operation_id='fe45a0369bf950da3ea311a4c493377cf9720a92c359c0bfbe739a3d9f699cbe'
   AND route_key='rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh'
   AND status='manual_recovery' AND action='VOLTR_RESTORE_IDLE'
   AND transaction_signature='46UBvSw1zjtZyDVUVaissm9SEXsKFKnYCQYKd23njb1NS1Ktkzsup5ic9XA55FxyTCpkoYuuM8hhn4MioGU2X7Wz'
   AND confirmed_slot=444157954 AND recovery_reason='exact_effect_reconciliation_failed';
END $$;
