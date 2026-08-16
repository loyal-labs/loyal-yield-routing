use loyal_actions::jupiter::{
    JupiterCrossMintPolicySeeds, JupiterCrossMintSourceShard, JupiterV2Dialect,
    SOLANA_PACKET_DATA_SIZE,
};
use loyal_actions::{
    create_jupiter_cross_mint_policy_set, create_same_mint_market_mint_yield_route_action,
    derive_squads_vault, detect_jupiter_cross_mint_policy_action, earn_stablecoin,
    earn_stablecoin_pairs, earn_stablecoins, LoyalActionContext, YieldRouteActionSetup,
    YieldRouteUniverse, KAMINO_ETHENA_MARKET, KAMINO_FIGURE_MARKET, KAMINO_MAIN_MARKET,
    KAMINO_MAPLE_MARKET, KAMINO_ONRE_MARKET,
};
use loyal_yield_router::timescale::{
    SupportedReserveCatalogRow, SupportedReserveMarketSnapshotQuery, TimescaleRouterClient,
    TimescaleRouterClientConfig,
};
use serde_json::json;
use sha2::{Digest, Sha256};
use solana_sdk::{
    hash::Hash,
    message::Message,
    pubkey::Pubkey,
    signature::{Keypair, Signer},
    transaction::Transaction,
};
use std::{
    collections::{BTreeMap, BTreeSet},
    env,
    error::Error,
    str::FromStr,
};

const LIVE_GATE: &str = "CROSS_MINT_SAFE_TOPOLOGY_VERIFY";
const DAILY_SWAP_CAP_RAW: u64 = 1_000_000_000_000;
const MAX_SLIPPAGE_BPS: u16 = 50;

#[test]
#[ignore = "requires CROSS_MINT_SAFE_TOPOLOGY_VERIFY=1 and TIMESCALEDB_URL"]
fn every_current_safe_reserve_cross_mint_topology_maps_to_generalized_shards() {
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("build topology verifier runtime")
        .block_on(verify_current_safe_topology())
        .expect("current Safe reserve topology must be fully authorized");
}

async fn verify_current_safe_topology() -> Result<(), Box<dyn Error>> {
    if env::var(LIVE_GATE).as_deref() != Ok("1") {
        return Err(format!(
            "{LIVE_GATE}=1 is required; run the explicit wrapper with the topology gate"
        )
        .into());
    }
    let database_url = env::var("TIMESCALEDB_URL")
        .map_err(|_| "TIMESCALEDB_URL is required for the explicit topology verifier")?;
    let client = TimescaleRouterClient::connect(
        TimescaleRouterClientConfig::new(database_url).with_max_connections(2),
    )
    .await?;
    let canonical_mints = earn_stablecoins()
        .iter()
        .map(|asset| asset.mint.to_string())
        .collect::<Vec<_>>();
    let snapshot = client
        .supported_reserve_market_snapshot(SupportedReserveMarketSnapshotQuery {
            risk_baskets: vec!["safe".to_owned()],
            liquidity_mints: canonical_mints.clone(),
        })
        .await?;
    let reserves = canonical_safe_reserves(snapshot.catalog)?;
    let observed_mints = reserves
        .values()
        .map(|reserve| reserve.liquidity_mint.as_str())
        .collect::<BTreeSet<_>>();
    if canonical_mints
        .iter()
        .any(|mint| !observed_mints.contains(mint.as_str()))
    {
        return Err("current Safe catalog does not cover all six canonical Earn mints".into());
    }

    let authority = Keypair::new();
    let settings = Pubkey::new_unique();
    let context = LoyalActionContext {
        settings,
        authority: authority.pubkey(),
        delegated_signer: authority.pubkey(),
        account_index: 0,
        vault: derive_squads_vault(&settings, 0).0,
    };
    let swap_policies = create_jupiter_cross_mint_policy_set(
        context,
        MAX_SLIPPAGE_BPS,
        DAILY_SWAP_CAP_RAW,
        JupiterCrossMintPolicySeeds {
            classic: 10_000,
            token_2022: 10_001,
        },
    )?;
    let policy_actions = [&swap_policies.classic, &swap_policies.token_2022];
    if policy_actions.len() != 2 {
        return Err("generalized V1 must contain exactly two source-sharded policies".into());
    }
    let mut decoded_shards = BTreeSet::new();
    for action in policy_actions {
        let decoded = detect_jupiter_cross_mint_policy_action(&action.instruction)?
            .ok_or("generalized policy create did not decode as a generalized policy")?;
        if decoded.identity.policy_account() != action.account
            || decoded.settings != settings
            || decoded.authority != authority.pubkey()
            || decoded.vault != context.vault
            || decoded.max_slippage_bps != MAX_SLIPPAGE_BPS
            || decoded.daily_source_mint_spending_cap != DAILY_SWAP_CAP_RAW
            || decoded.dialect_constraint_indexes.len() != 2
            || decoded
                .dialect_constraint_indexes
                .get(&JupiterV2Dialect::RouteV2)
                != Some(&0)
            || decoded
                .dialect_constraint_indexes
                .get(&JupiterV2Dialect::SharedAccountsRouteV2)
                != Some(&1)
        {
            return Err(
                "decoded generalized policy does not match its immutable specification".into(),
            );
        }
        if !decoded_shards.insert(decoded.source_shard) {
            return Err("two generalized policies must have disjoint source shards".into());
        }
    }
    if decoded_shards
        != BTreeSet::from([
            JupiterCrossMintSourceShard::Classic,
            JupiterCrossMintSourceShard::Token2022,
        ])
    {
        return Err("generalized policy set does not contain both canonical source shards".into());
    }

    let classic_packet = signed_packet_size(&authority, &swap_policies.classic.instruction)?;
    let token_2022_packet = signed_packet_size(&authority, &swap_policies.token_2022.instruction)?;
    if classic_packet > SOLANA_PACKET_DATA_SIZE || token_2022_packet > SOLANA_PACKET_DATA_SIZE {
        return Err("generalized source-sharded policy create exceeds the packet limit".into());
    }

    let earn_classic = create_same_mint_market_mint_yield_route_action(
        context,
        policy_universe(spl_token::ID),
        1,
    )?;
    let earn_token_2022 = create_same_mint_market_mint_yield_route_action(
        context,
        policy_universe(loyal_actions::TOKEN_2022_PROGRAM_ID),
        2,
    )?;
    let known_markets = [
        KAMINO_MAIN_MARKET,
        KAMINO_FIGURE_MARKET,
        KAMINO_MAPLE_MARKET,
        KAMINO_ONRE_MARKET,
        KAMINO_ETHENA_MARKET,
    ]
    .into_iter()
    .collect::<BTreeSet<_>>();
    let mut rows = Vec::new();
    let mut covered_pairs = BTreeSet::new();
    for source in reserves.values() {
        for target in reserves.values() {
            if source.liquidity_mint == target.liquidity_mint {
                continue;
            }
            let source_mint = Pubkey::from_str(&source.liquidity_mint)?;
            let target_mint = Pubkey::from_str(&target.liquidity_mint)?;
            let source_market = Pubkey::from_str(&source.market)?;
            let target_market = Pubkey::from_str(&target.market)?;
            if !known_markets.contains(&source_market) || !known_markets.contains(&target_market) {
                return Err(format!(
                    "current Safe topology contains an unmeasured market: {} or {}",
                    source.market, target.market
                )
                .into());
            }
            let source_asset = earn_stablecoin(source_mint)
                .ok_or("Safe source reserve mint is outside canonical Earn registry")?;
            let target_asset = earn_stablecoin(target_mint)
                .ok_or("Safe target reserve mint is outside canonical Earn registry")?;
            let source_earn =
                policy_for_program(&earn_classic, &earn_token_2022, source_asset.token_program);
            let target_earn =
                policy_for_program(&earn_classic, &earn_token_2022, target_asset.token_program);
            prove_earn_binding(source_earn, source_mint, source_market, true)?;
            prove_earn_binding(target_earn, target_mint, target_market, false)?;

            let pair = loyal_actions::EarnStablecoinPair::new(source_mint, target_mint)
                .ok_or("topology produced a self pair")?;
            let swap = swap_policies.action_for_source_mint(source_mint)?;
            let expected_shard = if source_asset.token_program == spl_token::ID {
                JupiterCrossMintSourceShard::Classic
            } else {
                JupiterCrossMintSourceShard::Token2022
            };
            if swap.spec.source_shard != expected_shard {
                return Err("source mint mapped to the wrong generalized policy shard".into());
            }
            let route_v2 = swap.step_for_pair(pair, JupiterV2Dialect::RouteV2)?;
            let shared_route_v2 =
                swap.step_for_pair(pair, JupiterV2Dialect::SharedAccountsRouteV2)?;
            if route_v2.instruction_constraint_index() != 0
                || shared_route_v2.instruction_constraint_index() != 1
                || route_v2.action_account() != swap.account
                || shared_route_v2.action_account() != swap.account
            {
                return Err(
                    "topology pair does not use the fixed generalized dialect indexes".into(),
                );
            }
            let withdraw = source_earn.withdraw_step()?;
            let deposit = target_earn.deposit_step()?;
            if withdraw.instruction_constraint_index() != 0
                || deposit.instruction_constraint_index() != 1
                || withdraw.action_account() == route_v2.action_account()
                || deposit.action_account() == route_v2.action_account()
                || (source_asset.token_program != target_asset.token_program
                    && withdraw.action_account() == deposit.action_account())
            {
                return Err(
                    "Earn withdraw/swap/deposit bindings alias or use the wrong index".into(),
                );
            }
            covered_pairs.insert((source.liquidity_mint.clone(), target.liquidity_mint.clone()));
            rows.push(json!({
                "sourceReserve": source.reserve,
                "sourceMarket": source.market,
                "sourceMint": source.liquidity_mint,
                "swapPolicy": swap.account.to_string(),
                "swapShard": format!("{:?}", swap.spec.source_shard),
                "routeV2ConstraintIndex": route_v2.instruction_constraint_index(),
                "sharedAccountsRouteV2ConstraintIndex": shared_route_v2.instruction_constraint_index(),
                "targetReserve": target.reserve,
                "targetMarket": target.market,
                "targetMint": target.liquidity_mint,
                "withdrawPolicy": withdraw.action_account().to_string(),
                "withdrawConstraintIndex": withdraw.instruction_constraint_index(),
                "depositPolicy": deposit.action_account().to_string(),
                "depositConstraintIndex": deposit.instruction_constraint_index(),
            }));
        }
    }
    let expected_pairs = earn_stablecoin_pairs()
        .into_iter()
        .map(|pair| (pair.input_mint.to_string(), pair.output_mint.to_string()))
        .collect::<BTreeSet<_>>();
    if covered_pairs != expected_pairs || rows.is_empty() {
        return Err("Safe topology did not cover all 30 canonical directed mint pairs".into());
    }
    let encoded = serde_json::to_vec(&rows)?;
    println!(
        "cross_mint_safe_topology PASS captured_at={} reserves={} topologies={} pairs={} generalized_policy_count=2 classic_policy_packet_bytes={} token_2022_policy_packet_bytes={} rows_sha256={:x} sends=0",
        snapshot.captured_at,
        reserves.len(),
        rows.len(),
        covered_pairs.len(),
        classic_packet,
        token_2022_packet,
        Sha256::digest(encoded),
    );
    Ok(())
}

fn canonical_safe_reserves(
    catalog: Vec<SupportedReserveCatalogRow>,
) -> Result<BTreeMap<String, SupportedReserveCatalogRow>, Box<dyn Error>> {
    let canonical = earn_stablecoins()
        .iter()
        .map(|asset| asset.mint.to_string())
        .collect::<BTreeSet<_>>();
    let mut reserves = BTreeMap::new();
    for row in catalog {
        if !canonical.contains(&row.liquidity_mint) {
            continue;
        }
        if row.source != "kamino-api" || !row.risk_baskets.iter().any(|risk| risk == "safe") {
            return Err(format!(
                "Safe catalog row {} has unsupported provenance",
                row.reserve
            )
            .into());
        }
        if reserves.insert(row.reserve.clone(), row).is_some() {
            return Err("Safe catalog returned a duplicate reserve identity".into());
        }
    }
    Ok(reserves)
}

fn policy_universe(token_program: Pubkey) -> YieldRouteUniverse {
    let stable_mints = earn_stablecoins()
        .iter()
        .filter(|asset| asset.token_program == token_program)
        .map(|asset| asset.mint)
        .collect::<Vec<_>>();
    YieldRouteUniverse::new(
        stable_mints.clone(),
        vec![
            KAMINO_MAIN_MARKET,
            KAMINO_FIGURE_MARKET,
            KAMINO_MAPLE_MARKET,
            KAMINO_ONRE_MARKET,
            KAMINO_ETHENA_MARKET,
        ],
        stable_mints,
    )
}

fn policy_for_program<'a>(
    classic: &'a YieldRouteActionSetup,
    token_2022: &'a YieldRouteActionSetup,
    token_program: Pubkey,
) -> &'a YieldRouteActionSetup {
    if token_program == spl_token::ID {
        classic
    } else {
        token_2022
    }
}

fn prove_earn_binding(
    policy: &YieldRouteActionSetup,
    mint: Pubkey,
    market: Pubkey,
    withdraw: bool,
) -> Result<(), Box<dyn Error>> {
    if !policy.spec.universe.stable_mints.contains(&mint)
        || !policy.spec.universe.kamino_liquidity_mints.contains(&mint)
        || !policy.spec.universe.kamino_markets.contains(&market)
    {
        return Err("Earn shard does not authorize the reserve market/mint identity".into());
    }
    let step = if withdraw {
        policy.withdraw_step()?
    } else {
        policy.deposit_step()?
    };
    let expected_index = if withdraw { 0 } else { 1 };
    if step.instruction_constraint_index() != expected_index {
        return Err("Earn shard named action uses an unexpected constraint index".into());
    }
    Ok(())
}

fn signed_packet_size(
    authority: &Keypair,
    instruction: &solana_sdk::instruction::Instruction,
) -> Result<usize, Box<dyn Error>> {
    let message = Message::new(std::slice::from_ref(instruction), Some(&authority.pubkey()));
    let transaction = Transaction::new(&[authority], message, Hash::new_unique());
    Ok(bincode::serialize(&transaction)?.len())
}
