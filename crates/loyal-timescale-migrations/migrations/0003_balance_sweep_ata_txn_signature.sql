ALTER TABLE loyal.balance_sweep_wallet_ata_observations
    ADD COLUMN IF NOT EXISTS txn_signature TEXT;

CREATE INDEX IF NOT EXISTS balance_sweep_wallet_ata_observations_txn_signature_idx
    ON loyal.balance_sweep_wallet_ata_observations (txn_signature)
    WHERE txn_signature IS NOT NULL;

CREATE OR REPLACE VIEW loyal.latest_balance_sweep_wallet_ata_observations AS
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
FROM loyal.balance_sweep_wallet_ata_observations
ORDER BY wallet_usdc_ata, event_id DESC;
