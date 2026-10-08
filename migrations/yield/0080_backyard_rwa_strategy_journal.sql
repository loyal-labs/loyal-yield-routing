-- Retain old operation statuses, signatures, wires, timestamps and effects.
-- Finalized strategy-two receipt creation: 446086069 / 4XcP5vPdaWdeWPzrQ3n5q694meYqXSvRJx1BVPNC45fy5U6bpN2p3Pc8g1NLPGT6gwt88VrqvfV4SqJWCUfFgQSj.
-- Evidence: docs/evidence/voltr-selector-2026-09-16/strategy-reset-history.json
-- SHA256: 3319db2f57baf743be2471d72a1e9d34168a81ebc0378071f7a7f28e1d968a64
-- The reviewed journal ends at slot 444157954, before the independent reset.
-- This associates accounting history; it does not resolve failed transactions.
DO $$
DECLARE
 route constant text := 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh';
 retired constant text := '9hDH4acTDrSjg9d5n8c1g53jMTonaDAUesp1diCWuuhj';
 current_config constant text := 'DCpR24Eb6xCWxDyaZvCBTkadkxCB2vkqJN1EfYNWtLxY';
BEGIN
 -- Serialize against lease acquisition, decisions and reconciliations.
 PERFORM 1 FROM loyal_yield.multiply_route_states WHERE route_key=route FOR UPDATE;
 IF EXISTS (SELECT 1 FROM loyal_yield.multiply_route_states WHERE route_key=route
   AND lease_owner IS NOT NULL AND lease_expires_at > clock_timestamp()) THEN
   RAISE EXCEPTION 'strategy association requires a stopped worker with no active lease';
 END IF;
 IF EXISTS (SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key=route
   AND status IN ('prepared','signed_persisted','broadcast_intent','confirmed','reconciliation_pending','decided','built','simulated','signed','submitted','reconciling')) THEN
   RAISE EXCEPTION 'strategy association cannot bypass a nonterminal operation';
 END IF;
 IF EXISTS (SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key=route
   AND engine_version='backyard_rwa_v1' AND confirmed_slot IS NOT NULL
   AND ((confirmed_slot > 444157954 AND (expected_effects->>'journalStrategyConfig') IS DISTINCT FROM current_config)
     OR (confirmed_slot <= 444157954 AND (
       confirmed_slot <= 0 OR transaction_signature IS NULL OR transaction_signature=''
       OR jsonb_typeof(expected_effects) IS DISTINCT FROM 'object'
       OR (expected_effects->>'journalStrategyConfig' IS NOT NULL AND expected_effects->>'journalStrategyConfig' <> retired))))) THEN
   RAISE EXCEPTION 'journal identity lies outside reviewed strategy reset evidence';
 END IF;
 -- Freeze the complete audited inventory, not merely a broad slot cutoff.
 -- Canonical tuple order/encoding is documented with the retained inventory.
 IF EXISTS (SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key=route
   AND engine_version='backyard_rwa_v1' AND confirmed_slot BETWEEN 1 AND 444157954) AND
   (SELECT count(*) <> 1717 OR encode(sha256(convert_to(string_agg(
     concat_ws('|',operation_id,route_key,engine_version,action,status,confirmed_slot::text,transaction_signature),
     E'\n' ORDER BY operation_id COLLATE "C"),'UTF8')),'hex') <> '391298ab914ab798685f3c8d1345fb4a06acc02606b0df9cb75a2aebfcbe046a'
     FROM loyal_yield.multiply_operations WHERE route_key=route
     AND engine_version='backyard_rwa_v1' AND confirmed_slot BETWEEN 1 AND 444157954) THEN
   RAISE EXCEPTION 'strategy-one journal differs from the exact audited inventory';
 END IF;
 UPDATE loyal_yield.multiply_operations SET expected_effects = expected_effects || jsonb_build_object(
   'journalStrategyConfig',retired,
   'journalAssociation',jsonb_build_object(
     'schema','backyard-strategy-association/v1',
     'operationId',operation_id,'routeKey',route_key,
     'confirmedSlot',confirmed_slot,'signature',transaction_signature,'action',action,'status',status,
     'currentStrategyConfig',current_config,'bootstrapSlot',446086069,
     'evidencePath','docs/evidence/voltr-selector-2026-09-16/strategy-reset-history.json',
     'evidenceSha256','3319db2f57baf743be2471d72a1e9d34168a81ebc0378071f7a7f28e1d968a64'))
 WHERE route_key=route AND engine_version='backyard_rwa_v1'
   AND confirmed_slot BETWEEN 1 AND 444157954
   AND status IN ('reconciled','failed','manual_recovery');
END $$;
