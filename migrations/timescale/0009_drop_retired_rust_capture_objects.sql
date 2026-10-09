-- Retired capture objects. The Go observer writes and the ATA projector reads
-- only loyal_prod. The loyal schema stays: it holds this ledger
-- (loyal.timescale_schema_migrations).

-- 0002/0003 single-stream ATA capture, superseded by 0004's loyal_prod.
DROP VIEW IF EXISTS loyal.latest_balance_sweep_wallet_ata_observations;
DROP TABLE IF EXISTS loyal.balance_sweep_wallet_ata_observation_dedupe;
DROP TABLE IF EXISTS loyal.balance_sweep_wallet_ata_observations;
DROP SEQUENCE IF EXISTS loyal.balance_sweep_wallet_ata_observation_event_id_seq;

-- 0004's staging stream had no Go writer or reader.
DROP VIEW IF EXISTS loyal_staging.latest_balance_sweep_wallet_ata_observations;
DROP TABLE IF EXISTS loyal_staging.balance_sweep_wallet_ata_observation_dedupe;
DROP TABLE IF EXISTS loyal_staging.balance_sweep_wallet_ata_observations;
DROP SEQUENCE IF EXISTS loyal_staging.balance_sweep_wallet_ata_observation_event_id_seq;
DROP SCHEMA IF EXISTS loyal_staging;

-- The retired Rust Kamino runner's own migration ledger.
DROP TABLE IF EXISTS kamino.schema_migrations;

-- 0001's one-minute rollup has no reader. It is a continuous aggregate where
-- TimescaleDB could create one and 0001's plain compatibility view elsewhere;
-- each needs its own DROP.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM timescaledb_information.continuous_aggregates
        WHERE view_schema = 'kamino'
          AND view_name = 'reserve_updates_1m'
    ) THEN
        DROP MATERIALIZED VIEW kamino.reserve_updates_1m;
    ELSE
        DROP VIEW IF EXISTS kamino.reserve_updates_1m;
    END IF;
END $$;
