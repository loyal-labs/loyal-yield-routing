DROP TABLE IF EXISTS app_user_smart_accounts CASCADE;
DROP TABLE IF EXISTS app_users CASCADE;
DROP SCHEMA IF EXISTS loyal_yield CASCADE;

CREATE TABLE app_users (
    id BIGINT PRIMARY KEY,
    subject_address TEXT NOT NULL
);
CREATE TABLE app_user_smart_accounts (
    user_id BIGINT NOT NULL REFERENCES app_users(id),
    solana_env TEXT NOT NULL,
    settings_pda TEXT NOT NULL,
    state TEXT NOT NULL
);

CREATE SCHEMA loyal_yield;
CREATE TABLE loyal_yield.earn_deposit_onboarding_attempts (
    wallet_address TEXT NOT NULL,
    settings TEXT NOT NULL,
    vault_index SMALLINT NOT NULL,
    vault_pubkey TEXT NOT NULL,
    policy_account TEXT,
    setup_policy_account TEXT,
    market TEXT,
    status TEXT NOT NULL
);
CREATE TABLE loyal_yield.user_yield_positions (
    wallet_address TEXT NOT NULL,
    settings TEXT NOT NULL,
    vault_index SMALLINT NOT NULL,
    vault_pubkey TEXT NOT NULL,
    policy_account TEXT,
    current_market TEXT,
    status TEXT NOT NULL
);
CREATE TABLE loyal_yield.managed_vaults (
    settings TEXT NOT NULL,
    vault_index SMALLINT NOT NULL,
    vault_pubkey TEXT NOT NULL,
    active_policy_id BIGINT,
    setup_policy_id BIGINT,
    active BOOLEAN NOT NULL
);
CREATE TABLE loyal_yield.route_policies (
    id BIGINT PRIMARY KEY,
    settings TEXT NOT NULL,
    authority TEXT NOT NULL,
    vault_index SMALLINT NOT NULL,
    vault_pubkey TEXT NOT NULL,
    policy_account TEXT NOT NULL,
    kamino_markets TEXT[] NOT NULL,
    active BOOLEAN NOT NULL,
    last_seen_slot BIGINT NOT NULL
);
CREATE TABLE loyal_yield.cross_mint_swap_policies (
    cluster TEXT NOT NULL,
    authority TEXT NOT NULL,
    settings TEXT NOT NULL,
    vault_index SMALLINT NOT NULL,
    vault_pubkey TEXT NOT NULL,
    policy_account TEXT NOT NULL,
    source_shard TEXT NOT NULL,
    active BOOLEAN NOT NULL
);
CREATE TABLE loyal_yield.balance_sweep_targets (
    id BIGINT PRIMARY KEY,
    cluster TEXT NOT NULL,
    settings TEXT NOT NULL,
    wallet TEXT NOT NULL,
    wallet_token_ata TEXT NOT NULL,
    vault_index SMALLINT NOT NULL,
    vault_pubkey TEXT NOT NULL,
    vault_token_ata TEXT NOT NULL,
    token_mint TEXT NOT NULL,
    policy_account TEXT NOT NULL,
    subscription_authority TEXT,
    recurring_delegation TEXT,
    desired_active BOOLEAN NOT NULL,
    chain_status TEXT NOT NULL
);
CREATE TABLE loyal_yield.multiply_route_states (
    settings TEXT NOT NULL,
    vault_index SMALLINT NOT NULL,
    vault TEXT NOT NULL,
    state JSONB NOT NULL
);
CREATE TABLE loyal_yield.earn_max_policy_sets (
    settings TEXT NOT NULL,
    vault_index SMALLINT NOT NULL,
    vault TEXT NOT NULL,
    policy_accounts JSONB NOT NULL,
    manifest_version TEXT NOT NULL,
    status TEXT NOT NULL
);

INSERT INTO app_users (id, subject_address)
VALUES (1, 'wallet-a');
INSERT INTO app_user_smart_accounts (user_id, solana_env, settings_pda, state)
VALUES (1, 'mainnet-beta', 'settings-a', 'ready');

INSERT INTO loyal_yield.route_policies
    (id, settings, authority, vault_index, vault_pubkey, policy_account,
     kamino_markets, active, last_seen_slot)
VALUES
    (1, 'settings-a', 'wallet-a', 2, 'managed-vault-a', 'managed-active-policy-a',
     ARRAY['managed-market-a'], TRUE, 91),
    (2, 'settings-a', 'wallet-a', 2, 'managed-vault-a', 'managed-setup-policy-a',
     ARRAY[]::TEXT[], FALSE, 89),
    (3, 'settings-b', 'wallet-b', 2, 'managed-vault-b', 'managed-active-policy-b',
     ARRAY['managed-market-b'], TRUE, 92);
INSERT INTO loyal_yield.managed_vaults
    (settings, vault_index, vault_pubkey, active_policy_id, setup_policy_id, active)
VALUES
    ('settings-a', 2, 'managed-vault-a', 1, 2, TRUE),
    ('settings-b', 2, 'managed-vault-b', 3, NULL, TRUE);

INSERT INTO loyal_yield.cross_mint_swap_policies
    (cluster, authority, settings, vault_index, vault_pubkey, policy_account, source_shard, active)
VALUES
    ('mainnet-beta', 'wallet-a', 'settings-a', 1, 'vault-a', 'cross-policy-a', 'classic', TRUE),
    ('mainnet-beta', 'wallet-b', 'settings-b', 1, 'vault-b', 'cross-policy-b', 'classic', TRUE);

INSERT INTO loyal_yield.multiply_route_states (settings, vault_index, vault, state)
VALUES
    ('settings-a', 0, 'earn-max-vault-a', '{"engineVersion":"earn_max_v2","observedSlot":101}'),
    ('settings-b', 0, 'earn-max-vault-b', '{"engineVersion":"earn_max_v2","observedSlot":102}');
INSERT INTO loyal_yield.earn_max_policy_sets
    (settings, vault_index, vault, policy_accounts, manifest_version, status)
VALUES
    ('settings-a', 0, 'earn-max-vault-a', '[{"account":"earn-max-policy-a","seed":1}]', 'earn-max-v2', 'ready'),
    ('settings-b', 0, 'earn-max-vault-b', '[{"account":"earn-max-policy-b","seed":1}]', 'earn-max-v2', 'ready');
