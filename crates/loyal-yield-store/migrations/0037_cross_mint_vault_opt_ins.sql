CREATE TABLE IF NOT EXISTS loyal_yield.cross_mint_vault_opt_ins (
    cluster TEXT NOT NULL,
    settings TEXT NOT NULL,
    vault_index SMALLINT NOT NULL,
    vault_pubkey TEXT NOT NULL,
    enabled BOOLEAN NOT NULL,
    classic_policy_account TEXT NOT NULL,
    classic_policy_seed BIGINT NOT NULL,
    token_2022_policy_account TEXT NOT NULL,
    token_2022_policy_seed BIGINT NOT NULL,
    max_slippage_bps INTEGER NOT NULL,
    daily_source_mint_spending_cap BIGINT NOT NULL,
    generation BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster, settings, vault_index, vault_pubkey),
    CONSTRAINT cross_mint_vault_opt_ins_identity_check CHECK (
        cluster <> ''
        AND settings <> ''
        AND vault_index >= 0
        AND vault_pubkey <> ''
        AND classic_policy_account <> ''
        AND classic_policy_seed > 0
        AND token_2022_policy_account <> ''
        AND token_2022_policy_seed > 0
        AND classic_policy_account <> token_2022_policy_account
        AND classic_policy_seed <> token_2022_policy_seed
    ),
    CONSTRAINT cross_mint_vault_opt_ins_config_check CHECK (
        max_slippage_bps BETWEEN 1 AND 10000
        AND daily_source_mint_spending_cap > 0
        AND generation > 0
    )
);

CREATE INDEX IF NOT EXISTS cross_mint_vault_opt_ins_enabled_idx
    ON loyal_yield.cross_mint_vault_opt_ins
        (cluster, settings, vault_index, vault_pubkey)
    WHERE enabled;

COMMENT ON TABLE loyal_yield.cross_mint_vault_opt_ins IS
    'Per-vault user intent, immutable cross-mint risk configuration, and exact installed policy identity. Policy catalog rows remain objective finalized on-chain observations.';

COMMENT ON COLUMN loyal_yield.cross_mint_vault_opt_ins.generation IS
    'Monotonic fence incremented exactly once by each real pause or resume transition.';
