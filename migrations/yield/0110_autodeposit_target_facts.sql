-- Every Autodeposit target is a mainnet target: no row has a NULL cluster,
-- and the workers only ever ran against mainnet. A NULL or devnet cluster was
-- a case every reader had to refuse.
SET LOCAL lock_timeout = '5s';

ALTER TABLE loyal_yield.balance_sweep_targets
    DROP CONSTRAINT balance_sweep_targets_cluster_check,
    ALTER COLUMN cluster SET NOT NULL,
    ADD CONSTRAINT balance_sweep_targets_cluster_check CHECK (cluster = 'mainnet-beta');

-- No attempt was ever recorded unknown or ambiguous; the Go settlement
-- protocol records only confirmed, failed and expired observations.
ALTER TABLE loyal_yield.balance_sweep_transaction_attempts
    DROP CONSTRAINT balance_sweep_transaction_attempts_state_check,
    ADD CONSTRAINT balance_sweep_transaction_attempts_state_check CHECK (
        attempt_state IN ('prepared', 'submitted', 'confirmed', 'failed', 'expired')
    );
