-- Synthetic wallet-balance events share one BIGINT primary key with positive
-- LaserStream observations. Keep each synthetic producer in a finite,
-- non-overlapping negative range:
--
--   app setup/artifact events (-target_id):  -1 through -999,999,999,999
--   app floor rebaseline sequence:            -1,000,000,000,000 through -1,999,999,999,999
--   LaserStream activation sequence:          -2,000,000,000,000 through -2,999,999,999,999
--
-- The activation sequence originally allocated from -1 downward. Move those
-- retained events and their surplus-lot references before App target IDs can
-- reuse them. The floor sequence originated in Loyal App, so create it for
-- routing-only databases before enforcing the shared range.
CREATE SEQUENCE IF NOT EXISTS loyal_yield.balance_sweep_floor_rebaseline_event_id_seq
    AS BIGINT
    INCREMENT BY -1
    MINVALUE -1999999999999
    MAXVALUE -1000000000000
    START WITH -1000000000000
    CACHE 1;

ALTER SEQUENCE loyal_yield.balance_sweep_floor_rebaseline_event_id_seq
    MINVALUE -1999999999999
    MAXVALUE -1000000000000
    START WITH -1000000000000;

ALTER SEQUENCE loyal_yield.autodeposit_bootstrap_event_id_seq
    MINVALUE -2999999999999
    MAXVALUE -2000000000000
    START WITH -2000000000000
    RESTART WITH -2000000000000;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM loyal_yield.balance_sweep_wallet_balance_events
        WHERE event_id BETWEEN -2999999999999 AND -2000000000000
          AND source <> 'laserstream_autodeposit_activation'
    ) THEN
        RAISE EXCEPTION
            'reserved LaserStream activation event ID range contains another source';
    END IF;
END $$;

-- Continue below any activation rows already moved by an interrupted/manual
-- application. false means the empty range starts exactly at -2 trillion.
SELECT setval(
    'loyal_yield.autodeposit_bootstrap_event_id_seq',
    COALESCE(
        (
            SELECT MIN(event_id)
            FROM loyal_yield.balance_sweep_wallet_balance_events
            WHERE event_id BETWEEN -2999999999999 AND -2000000000000
              AND source = 'laserstream_autodeposit_activation'
        ),
        -2000000000000
    ),
    EXISTS (
        SELECT 1
        FROM loyal_yield.balance_sweep_wallet_balance_events
        WHERE event_id BETWEEN -2999999999999 AND -2000000000000
          AND source = 'laserstream_autodeposit_activation'
    )
);

-- The only event-ID reference must follow retained activation events into the
-- reserved range. Keep ON UPDATE CASCADE as part of the shared ID contract.
ALTER TABLE loyal_yield.balance_sweep_surplus_lots
    DROP CONSTRAINT balance_sweep_surplus_lots_source_event_id_fkey;
ALTER TABLE loyal_yield.balance_sweep_surplus_lots
    ADD CONSTRAINT balance_sweep_surplus_lots_source_event_id_fkey
    FOREIGN KEY (source_event_id)
    REFERENCES loyal_yield.balance_sweep_wallet_balance_events(event_id)
    ON UPDATE CASCADE
    ON DELETE CASCADE;

UPDATE loyal_yield.balance_sweep_wallet_balance_events
SET event_id = nextval('loyal_yield.autodeposit_bootstrap_event_id_seq')
WHERE source = 'laserstream_autodeposit_activation'
  AND event_id < 0
  AND event_id NOT BETWEEN -2999999999999 AND -2000000000000;

COMMENT ON SEQUENCE loyal_yield.balance_sweep_floor_rebaseline_event_id_seq IS
    'Synthetic app floor-rebaseline event IDs: [-1999999999999, -1000000000000].';
COMMENT ON SEQUENCE loyal_yield.autodeposit_bootstrap_event_id_seq IS
    'Synthetic LaserStream activation event IDs: [-2999999999999, -2000000000000].';
