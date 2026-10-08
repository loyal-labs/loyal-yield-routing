CREATE INDEX CONCURRENTLY IF NOT EXISTS optimizer_epochs_latest_cluster_idx
    ON loyal_yield.optimizer_epochs (cluster, observed_at DESC, id DESC)
    INCLUDE (epoch_key, market_slot, expires_at);
