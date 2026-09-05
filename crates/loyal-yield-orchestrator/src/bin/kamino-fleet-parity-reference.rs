#![recursion_limit = "512"]
//! Deterministic planner and same-mint wire parity oracle. Uses the established
//! capacity-aware planner, canonical identity, official KLend/Squads builders,
//! and Solana v0 compiler. Does not claim RPC or durable lifecycle execution.
#[path = "loyal-klend-proxy.rs"]
mod klend_proxy;

use chrono::{DateTime, Duration, Utc};
use loyal_actions::{
    compile_squads_inner_instruction, execute_program_interaction_policy_instruction,
};
use loyal_yield_orchestrator::{
    fleet_orchestration::{
        plan_capacity_aware_wave, rebalance_opportunity_idempotency_key, CapacityBand,
        EconomicPolicy, OpportunityInput, RebalanceOpportunityInput,
        RebalanceOpportunityOperationClass, TargetCapacityCurve, WaveLimits,
    },
    SnapshotId, VaultId,
};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_sdk::{
    hash::Hash,
    instruction::{AccountMeta, Instruction as SolanaInstruction},
    message::{v0, AddressLookupTableAccount, VersionedMessage},
    pubkey::Pubkey,
    signature::Signature,
    transaction::VersionedTransaction,
};
use std::{collections::HashSet, env, error::Error, fs, str::FromStr};

const MINT: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const ROUTE_FP: &str = "44beb1514255da5f9da4a768b75bda05ca048c5ff1233c0332012c04d88aa75a";

#[derive(Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct Account {
    address: String,
    signer: bool,
    writable: bool,
}
#[derive(Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct Instruction {
    step: String,
    program: String,
    accounts: Vec<Account>,
    #[serde(skip_serializing)]
    data_hex: String,
}
#[derive(Clone, Deserialize, Serialize)]
struct Route {
    public: Vec<Instruction>,
    protected: Vec<Instruction>,
}
#[derive(Deserialize)]
struct ProxyOutput {
    route: Route,
}

fn digest(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}
fn decode_hex(value: &str) -> Result<Vec<u8>, Box<dyn Error>> {
    if value.len() % 2 != 0 {
        return Err("odd hex".into());
    }
    (0..value.len())
        .step_by(2)
        .map(|i| Ok(u8::from_str_radix(&value[i..i + 2], 16)?))
        .collect()
}
fn solana_instruction(value: &Instruction) -> Result<SolanaInstruction, Box<dyn Error>> {
    Ok(SolanaInstruction {
        program_id: Pubkey::from_str(&value.program)?,
        accounts: value
            .accounts
            .iter()
            .map(|a| {
                let key = Pubkey::from_str(&a.address).expect("fixture pubkey");
                if a.writable {
                    AccountMeta::new(key, a.signer)
                } else {
                    AccountMeta::new_readonly(key, a.signer)
                }
            })
            .collect(),
        data: decode_hex(&value.data_hex)?,
    })
}
fn compile_reference_wire(
    route: &Route,
    request: &Value,
) -> Result<(Vec<u8>, Vec<u8>), Box<dyn Error>> {
    let vault = Pubkey::from_str(request["vault"].as_str().ok_or("vault")?)?;
    let policy = Pubkey::from_str(
        request["source"]["collateralMint"]
            .as_str()
            .ok_or("policy")?,
    )?;
    let protected = route
        .protected
        .iter()
        .enumerate()
        .map(|(index, ix)| {
            let mut transaction_accounts = Vec::new();
            let compiled = compile_squads_inner_instruction(
                &mut transaction_accounts,
                solana_instruction(ix)?,
            );
            Ok(execute_program_interaction_policy_instruction(
                policy,
                vault,
                0,
                vec![compiled],
                vec![u8::try_from(index)?],
                transaction_accounts,
            ))
        })
        .collect::<Result<Vec<_>, Box<dyn Error>>>()?;
    let mut public = route
        .public
        .iter()
        .map(solana_instruction)
        .collect::<Result<Vec<_>, _>>()?;
    let source_refresh = public
        .iter()
        .position(|ix| ix.data == [33, 132, 147, 228, 151, 192, 72, 89])
        .ok_or("source refresh")?;
    let tail = public.split_off(source_refresh + 1);
    public.push(protected[0].clone());
    public.extend(tail);
    public.push(protected[1].clone());
    let mut seen = HashSet::new();
    let mut addresses = Vec::new();
    for ix in route.public.iter().chain(route.protected.iter()) {
        for account in &ix.accounts {
            let key = Pubkey::from_str(&account.address)?;
            if seen.insert(key) {
                addresses.push(key);
            }
        }
    }
    for key in [policy, vault] {
        if seen.insert(key) {
            addresses.push(key);
        }
    }
    let table = AddressLookupTableAccount {
        key: Pubkey::from_str(
            request["target"]["market"]
                .as_str()
                .ok_or("target market")?,
        )?,
        addresses,
    };
    let message = v0::Message::try_compile(
        &vault,
        &public,
        &[table],
        Hash::from_str(request["source"]["market"].as_str().ok_or("blockhash")?)?,
    )?;
    let message_bytes = VersionedMessage::V0(message.clone()).serialize();
    let transaction = VersionedTransaction {
        signatures: vec![Signature::default(); usize::from(message.header.num_required_signatures)],
        message: VersionedMessage::V0(message),
    };
    Ok((message_bytes, bincode::serialize(&transaction)?))
}
fn identity(
    epoch: i64,
    vault: i64,
    snapshot: i64,
    plan: &Value,
    target_apy: i64,
    edge: i64,
    priority: i64,
    annual_gain: i64,
    net_gain: i64,
    expires: DateTime<Utc>,
) -> String {
    let input = RebalanceOpportunityInput {
        cluster: "mainnet-beta".to_owned(),
        vault_id: VaultId(vault),
        source_snapshot_id: Some(SnapshotId(snapshot)),
        optimizer_epoch_id: epoch,
        route_fingerprint: None,
        requirements_fingerprint: None,
        source_reserve: Some("reserve-a".to_owned()),
        target_reserve: "reserve-b".to_owned(),
        liquidity_mint: MINT.to_owned(),
        amount_raw: 9_000_000_000,
        principal_usd_micros: 9_000_000_000,
        source_apy_bps: 81,
        target_apy_bps: target_apy,
        estimated_edge_bps: edge,
        estimated_cost_lamports: 50_000,
        annual_yield_gain_usd_micros: annual_gain,
        expected_net_gain_usd_micros: net_gain,
        economic_priority: priority,
        priority_version: "lost-yield-service-net-reserve-capacity-v3".to_owned(),
        operation_class: RebalanceOpportunityOperationClass::YieldOptimization,
        service_deadline_at: None,
        execution_plan: plan.clone(),
        available_at: expires,
        expires_at: expires,
        provisioning_request_id: None,
    };
    rebalance_opportunity_idempotency_key(&input)
}
fn plan(vault: i64, target: i64, edge: i64, now: DateTime<Utc>) -> Value {
    json!({
        "amount_raw":9000000000i64,"capacity_adjusted_target_apy_bps":target,"confidence_ppm":950000,"conservative_sol_price_usd_micros":1000000000i64,
        "cross_mint_maximum_value_loss_bps":Value::Null,"estimated_edge_bps":edge,"estimated_execution_cost_usd_micros":100000,
        "estimated_execution_costs":{"kind":"same_mint","route_usd_micros":100000},"expected_service_millis":15000,"fee_cap_lamports":50000,
        "fee_gain_fraction_ppm":50000,"fee_tier":"high_value","fresh_executable_jupiter_minimum_output_required":false,"holding_horizon_seconds":2592000,
        "idle_token_account":Value::Null,"idle_vault_liquidity_amount_raw":Value::Null,"kind":"same_mint","liquidity_mint":MINT,"minimum_transaction_fee_lamports":5000,
        "observed_source_apy_bps":81,"observed_target_apy_bps":919,"optimizer_market_slot":1000,"planning_economics_are_executable_quote":false,
        "policy_bindings":Value::Null,"policy_id":0,"redeemable_source_liquidity_amount_raw":9000000000i64,"route_amount_semantics":"redeemable_liquidity_amount",
        "route_kind":"same_mint","settings":"","source_amount_semantics":"redeemable_liquidity_amount","source_apy_bps":81,"source_collateral_amount_raw":9000000000i64,
        "source_kind":"reserve_position","source_liquidity_mint":MINT,"source_observed_at":"0001-01-01T00:00:00Z","source_observed_slot":0,
        "source_recovery_anchor_collateral_raw":Value::Null,"source_reserve":"reserve-a","target_apy_bps":target,"target_liquidity_mint":MINT,
        "target_observed_at":now.to_rfc3339_opts(chrono::SecondsFormat::Secs,true),"target_observed_slot":1000,"target_reserve":"reserve-b","vault_index":0,
        "vault_pubkey":format!("vault-{vault}"),"writable_conflict_keys":[format!("vault:vault-{vault}"),"policy:0","source-reserve:reserve-a","target-reserve:reserve-b"]
    })
}
fn planner_artifact(now: DateTime<Utc>, expires: DateTime<Utc>) -> Result<Value, Box<dyn Error>> {
    let opportunities = (1..=3)
        .map(|vault| OpportunityInput {
            opportunity_id: vault,
            optimizer_epoch_id: 7,
            vault_id: vault,
            tenant_id: "parity".to_owned(),
            source_snapshot_id: 99 + vault,
            observed_slot: 1000,
            mint: MINT.to_owned(),
            source_reserve: "reserve-a".to_owned(),
            target_reserve: "reserve-b".to_owned(),
            notional_usd_micros: 9_000_000_000,
            source_net_apy_bps: 81,
            target_net_apy_bps: 919,
            confidence_ppm: 950_000,
            expected_service_millis: 15_000,
            holding_horizon_seconds: 2_592_000,
            estimated_execution_cost_usd_micros: 100_000,
            age_seconds: 0,
            fairness_credit: 0,
            writable_conflict_keys: vec![
                format!("vault:vault-{vault}"),
                "policy:0".to_owned(),
                "source-reserve:reserve-a".to_owned(),
                "target-reserve:reserve-b".to_owned(),
            ],
        })
        .collect();
    let curve = |target: &str, apy, bands| TargetCapacityCurve {
        target_reserve: target.to_owned(),
        observed_supply_usd_micros: 1_000_000_000_000,
        observed_net_apy_bps: apy,
        already_committed_inflow_usd_micros: 0,
        already_committed_outflow_usd_micros: 0,
        bands,
    };
    let curves = vec![
        curve(
            "reserve-a",
            81,
            vec![CapacityBand {
                cumulative_inflow_usd_micros: 20_000_000_000,
                target_net_apy_bps: 79,
            }],
        ),
        curve(
            "reserve-b",
            919,
            vec![
                CapacityBand {
                    cumulative_inflow_usd_micros: 1_000_000_000,
                    target_net_apy_bps: 918,
                },
                CapacityBand {
                    cumulative_inflow_usd_micros: 5_000_000_000,
                    target_net_apy_bps: 914,
                },
                CapacityBand {
                    cumulative_inflow_usd_micros: 10_000_000_000,
                    target_net_apy_bps: 909,
                },
                CapacityBand {
                    cumulative_inflow_usd_micros: 20_000_000_000,
                    target_net_apy_bps: 900,
                },
            ],
        ),
    ];
    let wave = plan_capacity_aware_wave(
        opportunities,
        &EconomicPolicy::default(),
        curves,
        &WaveLimits::default(),
    )
    .map_err(|error| format!("established Rust planner rejected parity fixture: {error:?}"))?;
    if wave.selected.len() != 2 {
        return Err("established Rust planner did not select the two-move capacity wave".into());
    }
    let mut artifacts = Vec::new();
    for selected in wave.selected {
        let opportunity = selected.opportunity;
        let economics = selected.economics;
        let route_plan = plan(
            opportunity.vault_id,
            economics.capacity_adjusted_target_net_apy_bps,
            economics.capacity_adjusted_net_edge_bps,
            now,
        );
        let annual = economics
            .lost_yield_usd_micros_per_hour
            .checked_mul(8_760)
            .ok_or("annual gain overflow")?;
        artifacts.push(json!({
            "idempotencyKey": identity(7, opportunity.vault_id, opportunity.source_snapshot_id, &route_plan, economics.capacity_adjusted_target_net_apy_bps, economics.capacity_adjusted_net_edge_bps, economics.total_priority, annual, economics.net_holding_gain_usd_micros, expires),
            "executionPlan": route_plan, "sourceApyBps": economics.capacity_adjusted_source_net_apy_bps,
            "targetApyBps": economics.capacity_adjusted_target_net_apy_bps, "estimatedEdgeBps": economics.capacity_adjusted_net_edge_bps
        }));
    }
    Ok(Value::Array(artifacts))
}

#[tokio::main]
async fn main() {
    if let Err(e) = run().await {
        eprintln!("kamino fleet parity reference: {e}");
        std::process::exit(1)
    }
}
async fn run() -> Result<(), Box<dyn Error>> {
    let contract_path = env::args().nth(1).ok_or("contract path required")?;
    let contract = fs::read(&contract_path)?;
    let contract_digest = digest(&contract);
    if env::var("KAMINO_PARITY_CONTRACT_SHA256")? != contract_digest {
        return Err("contract digest mismatch".into());
    }
    let now = DateTime::parse_from_rfc3339(&env::var("KAMINO_PARITY_CLOCK")?)?.with_timezone(&Utc);
    let route_fixture = fs::read_to_string(
        std::path::Path::new(&contract_path)
            .parent()
            .unwrap()
            .join("kamino-route-v1.json"),
    )?;
    let request: Value = serde_json::from_str(&route_fixture)?;
    let input = serde_json::to_string(
        &json!({"schemaVersion":1,"operation":"buildSameMintRoute","request":request}),
    )?;
    let official = klend_proxy::build_json(&input)?;
    let output: ProxyOutput = serde_json::from_str(&official)?;
    if output.route.public.len() != 4 || output.route.protected.len() != 2 {
        return Err("official KLend route shape drifted".into());
    }
    let computed_route = digest(&serde_json::to_vec(&output.route)?);
    if computed_route != ROUTE_FP {
        return Err(format!("official KLend route fingerprint drifted: {computed_route}").into());
    }
    let (message, wire) = compile_reference_wire(&output.route, &request)?;
    let expires = now + Duration::minutes(5);
    let opportunities = planner_artifact(now, expires)?;
    let hex = |bytes: &[u8]| bytes.iter().map(|b| format!("{b:02x}")).collect::<String>();
    let artifact = json!({
        "schemaVersion": 2,
        "implementation": "rust",
        "scope": "deterministic_planner_and_same_mint_wire",
        "fixture": {"id":"kamino-planner-revalidator-replacement-v1","sha256":contract_digest,"clock":now.to_rfc3339_opts(chrono::SecondsFormat::Secs,true)},
        "opportunities": opportunities,
        "route": {"fingerprint":computed_route,"messageHex":hex(&message),"wireHex":hex(&wire)}
    });
    println!("{}", serde_json::to_string(&artifact)?);
    Ok(())
}
