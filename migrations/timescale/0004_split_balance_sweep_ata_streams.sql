CREATE EXTENSION IF NOT EXISTS timescaledb;

CREATE SCHEMA IF NOT EXISTS loyal_prod;
CREATE SCHEMA IF NOT EXISTS loyal_staging;

CREATE SEQUENCE IF NOT EXISTS loyal_prod.balance_sweep_wallet_ata_observation_event_id_seq;
CREATE SEQUENCE IF NOT EXISTS loyal_staging.balance_sweep_wallet_ata_observation_event_id_seq;

CREATE TABLE IF NOT EXISTS loyal_prod.balance_sweep_wallet_ata_observations (
    event_id BIGINT NOT NULL DEFAULT nextval('loyal_prod.balance_sweep_wallet_ata_observation_event_id_seq'),
    cluster TEXT NOT NULL,
    target_id BIGINT NOT NULL,
    wallet TEXT NOT NULL,
    wallet_usdc_ata TEXT NOT NULL,
    vault_pubkey TEXT NOT NULL,
    vault_usdc_ata TEXT NOT NULL,
    amount_raw BIGINT NOT NULL,
    owner TEXT,
    mint TEXT NOT NULL,
    slot BIGINT NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    source TEXT NOT NULL,
    source_commitment TEXT NOT NULL,
    account_data_hash TEXT NOT NULL,
    raw_account_data_base64 TEXT NOT NULL DEFAULT '',
    raw_evidence JSONB NOT NULL,
    received_at TIMESTAMPTZ NOT NULL,
    inserted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    txn_signature TEXT
);

SELECT create_hypertable(
    'loyal_prod.balance_sweep_wallet_ata_observations',
    'observed_at',
    if_not_exists => TRUE,
    chunk_time_interval => INTERVAL '1 day'
);

CREATE INDEX IF NOT EXISTS balance_sweep_wallet_ata_observations_event_id_idx
    ON loyal_prod.balance_sweep_wallet_ata_observations (event_id);
CREATE INDEX IF NOT EXISTS balance_sweep_wallet_ata_observations_wallet_event_idx
    ON loyal_prod.balance_sweep_wallet_ata_observations (wallet_usdc_ata, event_id DESC);
CREATE INDEX IF NOT EXISTS balance_sweep_wallet_ata_observations_target_event_idx
    ON loyal_prod.balance_sweep_wallet_ata_observations (target_id, event_id DESC);
CREATE INDEX IF NOT EXISTS balance_sweep_wallet_ata_observations_slot_desc_idx
    ON loyal_prod.balance_sweep_wallet_ata_observations (slot DESC);
CREATE INDEX IF NOT EXISTS balance_sweep_wallet_ata_observations_time_desc_idx
    ON loyal_prod.balance_sweep_wallet_ata_observations (observed_at DESC);
CREATE INDEX IF NOT EXISTS balance_sweep_wallet_ata_observations_raw_evidence_gin_idx
    ON loyal_prod.balance_sweep_wallet_ata_observations USING GIN (raw_evidence jsonb_path_ops);
CREATE INDEX IF NOT EXISTS balance_sweep_wallet_ata_observations_txn_signature_idx
    ON loyal_prod.balance_sweep_wallet_ata_observations (txn_signature)
    WHERE txn_signature IS NOT NULL;

CREATE TABLE IF NOT EXISTS loyal_prod.balance_sweep_wallet_ata_observation_dedupe (
    dedupe_key TEXT PRIMARY KEY,
    event_id BIGINT NOT NULL,
    source_commitment TEXT NOT NULL,
    wallet_usdc_ata TEXT NOT NULL,
    slot BIGINT NOT NULL,
    account_data_hash TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source_commitment, wallet_usdc_ata, slot, account_data_hash)
);

CREATE INDEX IF NOT EXISTS balance_sweep_wallet_ata_observation_dedupe_event_id_idx
    ON loyal_prod.balance_sweep_wallet_ata_observation_dedupe (event_id);
CREATE INDEX IF NOT EXISTS balance_sweep_wallet_ata_observation_dedupe_ata_slot_idx
    ON loyal_prod.balance_sweep_wallet_ata_observation_dedupe (wallet_usdc_ata, slot);

CREATE OR REPLACE VIEW loyal_prod.latest_balance_sweep_wallet_ata_observations AS
SELECT DISTINCT ON (wallet_usdc_ata)
    event_id,
    cluster,
    target_id,
    wallet,
    wallet_usdc_ata,
    vault_pubkey,
    vault_usdc_ata,
    amount_raw,
    owner,
    mint,
    slot,
    observed_at,
    source,
    source_commitment,
    account_data_hash,
    raw_account_data_base64,
    raw_evidence,
    received_at,
    inserted_at,
    txn_signature
FROM loyal_prod.balance_sweep_wallet_ata_observations
ORDER BY wallet_usdc_ata, event_id DESC;

CREATE TABLE IF NOT EXISTS loyal_staging.balance_sweep_wallet_ata_observations (
    event_id BIGINT NOT NULL DEFAULT nextval('loyal_staging.balance_sweep_wallet_ata_observation_event_id_seq'),
    cluster TEXT NOT NULL,
    target_id BIGINT NOT NULL,
    wallet TEXT NOT NULL,
    wallet_usdc_ata TEXT NOT NULL,
    vault_pubkey TEXT NOT NULL,
    vault_usdc_ata TEXT NOT NULL,
    amount_raw BIGINT NOT NULL,
    owner TEXT,
    mint TEXT NOT NULL,
    slot BIGINT NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    source TEXT NOT NULL,
    source_commitment TEXT NOT NULL,
    account_data_hash TEXT NOT NULL,
    raw_account_data_base64 TEXT NOT NULL DEFAULT '',
    raw_evidence JSONB NOT NULL,
    received_at TIMESTAMPTZ NOT NULL,
    inserted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    txn_signature TEXT
);

SELECT create_hypertable(
    'loyal_staging.balance_sweep_wallet_ata_observations',
    'observed_at',
    if_not_exists => TRUE,
    chunk_time_interval => INTERVAL '1 day'
);

CREATE INDEX IF NOT EXISTS balance_sweep_wallet_ata_observations_event_id_idx
    ON loyal_staging.balance_sweep_wallet_ata_observations (event_id);
CREATE INDEX IF NOT EXISTS balance_sweep_wallet_ata_observations_wallet_event_idx
    ON loyal_staging.balance_sweep_wallet_ata_observations (wallet_usdc_ata, event_id DESC);
CREATE INDEX IF NOT EXISTS balance_sweep_wallet_ata_observations_target_event_idx
    ON loyal_staging.balance_sweep_wallet_ata_observations (target_id, event_id DESC);
CREATE INDEX IF NOT EXISTS balance_sweep_wallet_ata_observations_slot_desc_idx
    ON loyal_staging.balance_sweep_wallet_ata_observations (slot DESC);
CREATE INDEX IF NOT EXISTS balance_sweep_wallet_ata_observations_time_desc_idx
    ON loyal_staging.balance_sweep_wallet_ata_observations (observed_at DESC);
CREATE INDEX IF NOT EXISTS balance_sweep_wallet_ata_observations_raw_evidence_gin_idx
    ON loyal_staging.balance_sweep_wallet_ata_observations USING GIN (raw_evidence jsonb_path_ops);
CREATE INDEX IF NOT EXISTS balance_sweep_wallet_ata_observations_txn_signature_idx
    ON loyal_staging.balance_sweep_wallet_ata_observations (txn_signature)
    WHERE txn_signature IS NOT NULL;

CREATE TABLE IF NOT EXISTS loyal_staging.balance_sweep_wallet_ata_observation_dedupe (
    dedupe_key TEXT PRIMARY KEY,
    event_id BIGINT NOT NULL,
    source_commitment TEXT NOT NULL,
    wallet_usdc_ata TEXT NOT NULL,
    slot BIGINT NOT NULL,
    account_data_hash TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source_commitment, wallet_usdc_ata, slot, account_data_hash)
);

CREATE INDEX IF NOT EXISTS balance_sweep_wallet_ata_observation_dedupe_event_id_idx
    ON loyal_staging.balance_sweep_wallet_ata_observation_dedupe (event_id);
CREATE INDEX IF NOT EXISTS balance_sweep_wallet_ata_observation_dedupe_ata_slot_idx
    ON loyal_staging.balance_sweep_wallet_ata_observation_dedupe (wallet_usdc_ata, slot);

CREATE OR REPLACE VIEW loyal_staging.latest_balance_sweep_wallet_ata_observations AS
SELECT DISTINCT ON (wallet_usdc_ata)
    event_id,
    cluster,
    target_id,
    wallet,
    wallet_usdc_ata,
    vault_pubkey,
    vault_usdc_ata,
    amount_raw,
    owner,
    mint,
    slot,
    observed_at,
    source,
    source_commitment,
    account_data_hash,
    raw_account_data_base64,
    raw_evidence,
    received_at,
    inserted_at,
    txn_signature
FROM loyal_staging.balance_sweep_wallet_ata_observations
ORDER BY wallet_usdc_ata, event_id DESC;
