\set ON_ERROR_STOP on

CREATE SCHEMA IF NOT EXISTS kamino;

CREATE TABLE IF NOT EXISTS kamino.local_verified_reserve_updates (
    event_id BIGINT PRIMARY KEY,
    account_data_hash TEXT NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    slot BIGINT NOT NULL,
    verified_at TIMESTAMPTZ NOT NULL,
    verified_slot BIGINT NOT NULL,
    verification_commitment TEXT NOT NULL,
    verification_source TEXT NOT NULL,
    reserve TEXT NOT NULL UNIQUE,
    market TEXT NOT NULL,
    market_name TEXT NOT NULL,
    liquidity_mint TEXT NOT NULL,
    symbol TEXT NOT NULL,
    mint_decimals INTEGER NOT NULL,
    reserve_last_update_slot BIGINT NOT NULL,
    reserve_last_update_stale BOOLEAN NOT NULL,
    reserve_price_status SMALLINT NOT NULL,
    available_amount DOUBLE PRECISION NOT NULL,
    borrowed_amount DOUBLE PRECISION NOT NULL,
    total_supply_amount DOUBLE PRECISION NOT NULL,
    market_price_usd DOUBLE PRECISION NOT NULL,
    market_price_last_updated_ts BIGINT NOT NULL,
    utilization DOUBLE PRECISION NOT NULL,
    borrow_apy DOUBLE PRECISION NOT NULL,
    supply_apy DOUBLE PRECISION NOT NULL,
    total_supply_usd_estimate DOUBLE PRECISION NOT NULL,
    total_borrow_usd_estimate DOUBLE PRECISION NOT NULL,
    diff_changed BOOLEAN NOT NULL,
    changed_fields TEXT[] NOT NULL,
    diff_summary TEXT NOT NULL
);

CREATE OR REPLACE VIEW kamino.latest_verified_reserve_updates AS
SELECT * FROM kamino.local_verified_reserve_updates;

INSERT INTO kamino.supported_reserves (
    market, liquidity_mint, reserve, market_name, symbol, risk_baskets,
    source, active, fetched_at
) VALUES
    (
        :'main_market', :'usdc_mint', :'main_reserve', 'Main', 'USDC',
        ARRAY['safe'], 'local-mainnet-clone-simulation', TRUE, now()
    ),
    (
        :'prime_market', :'usdc_mint', :'prime_reserve', 'Prime', 'USDC',
        ARRAY['safe'], 'local-mainnet-clone-simulation', TRUE, now()
    )
ON CONFLICT (market, liquidity_mint) DO UPDATE SET
    reserve = EXCLUDED.reserve,
    market_name = EXCLUDED.market_name,
    symbol = EXCLUDED.symbol,
    risk_baskets = EXCLUDED.risk_baskets,
    source = EXCLUDED.source,
    active = TRUE,
    fetched_at = now(),
    updated_at = now();

INSERT INTO kamino.local_verified_reserve_updates (
    event_id, account_data_hash, observed_at, slot, verified_at, verified_slot,
    verification_commitment, verification_source, reserve, market, market_name,
    liquidity_mint, symbol, mint_decimals, reserve_last_update_slot,
    reserve_last_update_stale, reserve_price_status, available_amount,
    borrowed_amount, total_supply_amount, market_price_usd,
    market_price_last_updated_ts, utilization, borrow_apy, supply_apy,
    total_supply_usd_estimate, total_borrow_usd_estimate, diff_changed,
    changed_fields, diff_summary
) VALUES
    (
        1, :'main_hash', now(), :'observed_slot'::BIGINT, now(), :'observed_slot'::BIGINT,
        'confirmed', 'local_fixture', :'main_reserve', :'main_market', 'Main',
        :'usdc_mint', 'USDC', 6, :'observed_slot'::BIGINT, FALSE, 1,
        8000000000000.0, 2000000000000.0, 10000000000000.0, 1.0,
        extract(epoch FROM now())::BIGINT, 0.2, 0.02, 0.01,
        10000000.0, 2000000.0, TRUE, ARRAY['supply_apy'],
        'simulated Main APY input over cloned account identity'
    ),
    (
        2, :'prime_hash', now(), :'observed_slot'::BIGINT, now(), :'observed_slot'::BIGINT,
        'confirmed', 'local_fixture', :'prime_reserve', :'prime_market', 'Prime',
        :'usdc_mint', 'USDC', 6, :'observed_slot'::BIGINT, FALSE, 1,
        8000000000000.0, 2000000000000.0, 10000000000000.0, 1.0,
        extract(epoch FROM now())::BIGINT, 0.2, 0.12, 0.10,
        10000000.0, 2000000.0, TRUE, ARRAY['supply_apy'],
        'simulated Prime APY input over cloned account identity'
    )
ON CONFLICT (event_id) DO UPDATE SET
    account_data_hash = EXCLUDED.account_data_hash,
    observed_at = now(),
    slot = EXCLUDED.slot,
    verified_at = now(),
    verified_slot = EXCLUDED.verified_slot,
    reserve_last_update_slot = EXCLUDED.reserve_last_update_slot,
    market_price_last_updated_ts = extract(epoch FROM now())::BIGINT,
    supply_apy = EXCLUDED.supply_apy;

INSERT INTO loyal_yield.route_policies (
    settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
    delegated_signers, threshold, route_modes, stable_mints, kamino_markets,
    kamino_liquidity_mints, universe_preset, risk_profile, swap_lanes, active,
    last_seen_slot, last_seen_signature
) VALUES (
    :'settings', :'authority', :'route_policy_seed'::BIGINT, :'route_policy',
    :'vault_index'::SMALLINT, :'vault', ARRAY[:'policy'], 1,
    ARRAY['same_mint_kamino'], ARRAY[:'usdc_mint'],
    ARRAY[:'main_market', :'prime_market'], ARRAY[:'usdc_mint'],
    'kamino-safe-usdc', 'safe', '[]'::JSONB, TRUE,
    :'observed_slot'::BIGINT, 'local-fixture-provisional-policy'
)
ON CONFLICT (policy_account) DO UPDATE SET
    authority = EXCLUDED.authority,
    delegated_signers = EXCLUDED.delegated_signers,
    active = TRUE,
    last_seen_at = now(),
    last_seen_slot = EXCLUDED.last_seen_slot,
    last_seen_signature = EXCLUDED.last_seen_signature
RETURNING id AS provisional_policy_id
\gset

INSERT INTO loyal_yield.managed_vaults (
    settings, vault_index, vault_pubkey, active_policy_id, active
) VALUES (
    :'settings', :'vault_index'::SMALLINT, :'vault', :provisional_policy_id, TRUE
)
ON CONFLICT (settings, vault_index, vault_pubkey) DO UPDATE SET
    active_policy_id = EXCLUDED.active_policy_id,
    active = TRUE,
    last_seen_at = now();
