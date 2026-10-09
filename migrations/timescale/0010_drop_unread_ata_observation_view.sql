-- 0004's latest-observation view has no reader: the ATA projector reads
-- loyal_prod.balance_sweep_wallet_ata_observations directly.
DROP VIEW IF EXISTS loyal_prod.latest_balance_sweep_wallet_ata_observations;
