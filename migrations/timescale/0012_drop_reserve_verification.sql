-- The observer writes Kamino reserves from its confirmed stream alone and the
-- planners read reserve_current_states. The confirmed re-read layer of
-- 0005-0007 has no writer and no reader; reserve_current_states stays.
DROP VIEW IF EXISTS kamino.latest_verified_reserve_updates;
DROP TABLE IF EXISTS kamino.reserve_confirmed_verifications;
DROP TABLE IF EXISTS kamino.reserve_confirmed_observation_floors;
DROP SEQUENCE IF EXISTS kamino.reserve_confirmed_observation_id_seq;
DROP FUNCTION IF EXISTS kamino.confirmed_verification_slot_tolerance();
