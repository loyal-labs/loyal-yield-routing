use std::{env, fs};

use futures_util::future::BoxFuture;
use loyal_actions::{
    build_canonical_subscription_sweep_policy_create, create_all_in_one_market_mint_yield_route_action,
    create_init_obligation_yield_route_action, create_jupiter_cross_mint_policy_action,
    create_preset_all_in_one_yield_route_action, create_same_mint_market_mint_yield_route_action,
    create_three_step_yield_route_actions, derive_squads_vault,
    jupiter::{JupiterCrossMintPolicySpec, JupiterCrossMintSourceShard},
    remove_policies_instruction, remove_policy_instruction,
    update_all_in_one_market_mint_yield_route_action, update_exact_program_interaction_policy_instruction,
    JupiterSwapContract, KaminoStableRiskProfile, LoyalActionContext, SwapLane, YieldRouteActionSeeds,
    YieldRouteUniverse, YieldRouteUniversePreset, JUPITER_SWAP_DISCRIMINATOR, JUPITER_V6_PROGRAM_ID,
    KAMINO_ETHENA_MARKET, KAMINO_FIGURE_MARKET, KAMINO_MAIN_MARKET, KAMINO_MAPLE_MARKET,
    KAMINO_ONRE_MARKET, USDC_MINT,
};
use loyal_fleet_worker::multiply::{
    config::derive_earn_max_topology_with_policy_seed_base,
    policy::{canonical_policy_create, canonical_policy_update, PolicyFamily},
};
use loyal_squads_policy_monitor::{
    BalanceSweepExecutionEvent, Cluster, Commitment, MonitorConfig, MonitorError, PolicyMatchSink,
    PolicyMonitor, PolicyMonitorEvent,
};
use loyal_yield_store::{
    fleet_orchestration::{MultiplyOperation, MultiplyRouteState, StrategyKey},
    sqlx::{self, postgres::PgPoolOptions, Row},
    BalanceSweepPolicyMatchInput, CrossMintSwapPolicyManifestInput, EarnDirectMutation,
    EarnReconciliationEnqueueInput, EarnReconciliationVaultInput, OrchestratorStore,
    PolicyMatchInput, PolicyRemovalInput,
};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_sdk::{instruction::Instruction, pubkey::Pubkey};

fn key(tag: &str) -> Pubkey {
    Pubkey::new_from_array(Sha256::digest(tag.as_bytes()).into())
}

#[derive(Clone, Default)]
struct Collect(std::sync::Arc<std::sync::Mutex<Vec<Value>>>);

impl PolicyMatchSink for Collect {
    fn emit(&mut self, event: PolicyMonitorEvent) -> BoxFuture<'_, Result<(), MonitorError>> {
        let value = match event {
            PolicyMonitorEvent::YieldRoute(e) => json!({"kind": "route", "input": PolicyMatchInput::from(e)}),
            PolicyMonitorEvent::YieldSetup(e) => json!({"kind": "setup", "input": PolicyMatchInput::from(e)}),
            PolicyMonitorEvent::BalanceSweep(e) => json!({"kind": "sweep", "input": BalanceSweepPolicyMatchInput::from(e)}),
            PolicyMonitorEvent::CrossMintSwapPolicyManifest(e) => json!({"kind": "cross_mint", "input": CrossMintSwapPolicyManifestInput::from(e)}),
            PolicyMonitorEvent::PolicyRemoval(e) => json!({"kind": "removal", "input": PolicyRemovalInput::from(e)}),
        };
        self.0.lock().unwrap().push(value);
        Box::pin(async { Ok(()) })
    }

    fn emit_execution(&mut self, _: BalanceSweepExecutionEvent) -> BoxFuture<'_, Result<(), MonitorError>> {
        Box::pin(async { Ok(()) })
    }
}

fn instruction_json(instruction: &Instruction) -> Value {
    json!({
        "program_id": instruction.program_id.to_string(),
        "accounts": instruction.accounts.iter().map(|a| json!({"pubkey": a.pubkey.to_string(), "is_signer": a.is_signer, "is_writable": a.is_writable})).collect::<Vec<_>>(),
        "data": hex::encode(&instruction.data),
    })
}

fn context(name: &str, account_index: u8) -> LoyalActionContext {
    let settings = key(&format!("{name}-settings"));
    LoyalActionContext {
        settings,
        authority: key(&format!("{name}-authority")),
        delegated_signer: key("delegate"),
        account_index,
        vault: derive_squads_vault(&settings, account_index).0,
    }
}

fn jupiter_lane() -> SwapLane {
    SwapLane::Jupiter(JupiterSwapContract { program_id: JUPITER_V6_PROGRAM_ID, exact_in_discriminator: JUPITER_SWAP_DISCRIMINATOR, max_slippage_bps: 100 })
}

fn safe_universe() -> YieldRouteUniverse {
    YieldRouteUniverse::new(
        vec![USDC_MINT],
        vec![KAMINO_MAIN_MARKET, KAMINO_FIGURE_MARKET, KAMINO_MAPLE_MARKET, KAMINO_ONRE_MARKET, KAMINO_ETHENA_MARKET],
        vec![USDC_MINT],
    )
}

async fn detect() {
    let mut cases: Vec<(String, Instruction)> = Vec::new();
    let mixed = YieldRouteUniverse::new(vec![USDC_MINT, key("stable-b")], vec![key("market-a")], vec![USDC_MINT]);
    let hub = SwapLane::LoyalHub { hub_authorizer: key("hub-authorizer"), max_fee_bps: 50 };
    cases.push(("all_in_one_jupiter_hub".into(), create_all_in_one_market_mint_yield_route_action(context("aio", 0), mixed.clone(), vec![jupiter_lane(), hub]).unwrap().instructions[0].clone()));
    cases.push(("preset_safe_jupiter".into(), create_preset_all_in_one_yield_route_action(context("preset", 0), YieldRouteUniversePreset::KaminoStable(KaminoStableRiskProfile::Safe), vec![jupiter_lane()]).unwrap().instructions[0].clone()));
    cases.push(("preset_medium".into(), create_preset_all_in_one_yield_route_action(context("medium", 0), YieldRouteUniversePreset::KaminoStable(KaminoStableRiskProfile::Medium), vec![]).unwrap().instructions[0].clone()));
    cases.push(("compact_same_mint_safe".into(), create_all_in_one_market_mint_yield_route_action(context("compact", 0), safe_universe(), vec![]).unwrap().instructions[0].clone()));
    cases.push(("same_mint_market_mint".into(), create_same_mint_market_mint_yield_route_action(context("same", 0), safe_universe(), 12).unwrap().instructions[0].clone()));
    cases.push(("init_obligation_setup".into(), create_init_obligation_yield_route_action(context("setup", 0), safe_universe(), 13).unwrap().instructions[0].clone()));
    let three = create_three_step_yield_route_actions(context("three", 0), mixed.clone(), vec![jupiter_lane()], YieldRouteActionSeeds::default()).unwrap();
    for (index, instruction) in three.instructions.iter().enumerate() {
        cases.push((format!("three_step_{index}"), instruction.clone()));
    }
    let update_context = context("update", 0);
    cases.push(("update_all_in_one".into(), update_all_in_one_market_mint_yield_route_action(update_context, safe_universe(), vec![jupiter_lane()], key("update-policy"), 0).unwrap().instructions[0].clone()));
    let sweep_context = context("sweep", 1);
    cases.push(("canonical_subscription_sweep".into(), build_canonical_subscription_sweep_policy_create(sweep_context.settings, sweep_context.authority, key("payer"), key("delegate"), 9, key("wallet"), sweep_context.vault, 500_000).unwrap()));
    for (name, shard) in [("cross_mint_classic", JupiterCrossMintSourceShard::Classic), ("cross_mint_token_2022", JupiterCrossMintSourceShard::Token2022)] {
        let action = create_jupiter_cross_mint_policy_action(context(name, 0), JupiterCrossMintPolicySpec { source_shard: shard, max_slippage_bps: 75, daily_source_mint_spending_cap: 1_000_000_000 }, 21).unwrap();
        cases.push((name.into(), action.instruction));
    }
    let remove_context = context("remove", 0);
    cases.push(("remove_one".into(), remove_policy_instruction(remove_context.settings, remove_context.authority, key("removed-a"))));
    cases.push(("remove_two".into(), remove_policies_instruction(remove_context.settings, remove_context.authority, &[key("removed-a"), key("removed-b")])));
    let foreign = Instruction::new_with_bytes(key("foreign-program"), &[1, 2, 3], vec![solana_sdk::instruction::AccountMeta::new(key("foreign-account"), false)]);
    let incompatible = update_exact_program_interaction_policy_instruction(
        remove_context.settings, remove_context.authority, key("invalid-policy"), key("delegate"), 0, &[foreign], &[vec![0]]).unwrap();
    cases.push(("update_incompatible".into(), incompatible));

    // The App creates the setup policy as one init-obligation constraint.
    let setup_context = context("setup-single", 0);
    let markets = vec![KAMINO_MAIN_MARKET, KAMINO_FIGURE_MARKET];
    let setup_spec = || {
        let mut data = loyal_actions::KAMINO_INIT_OBLIGATION_DISCRIMINATOR.to_vec();
        data.extend([0, 0]);
        loyal_actions::SemanticProgramInteractionConstraint {
            program_id: loyal_actions::KAMINO_LEND_PROGRAM_ID,
            account_pubkeys: vec![
                (0, vec![setup_context.vault]),
                (1, vec![setup_context.vault]),
                (2, markets.iter().map(|market| loyal_actions::derive_kamino_vanilla_obligation(setup_context.vault, *market)).collect()),
                (3, markets.clone()),
                (4, vec![Pubkey::default()]),
                (5, vec![Pubkey::default()]),
                (6, vec![loyal_actions::derive_kamino_user_metadata(setup_context.vault)]),
                (7, vec![solana_sdk::sysvar::rent::id()]),
                (8, vec![solana_sdk::system_program::ID]),
            ],
            account_data: vec![],
            data: vec![loyal_actions::SemanticProgramInteractionDataConstraint::SliceEquals { offset: 0, value: data }],
        }
    };
    cases.push(("setup_single_legacy".into(), loyal_actions::create_deployed_semantic_program_interaction_policy_instruction(
        setup_context.settings, setup_context.authority, key("delegate"), 14, 0, vec![setup_spec()]).unwrap()));
    cases.push(("setup_single_compact".into(), loyal_actions::create_semantic_program_interaction_policy_instruction(
        setup_context.settings, setup_context.authority, key("delegate"), 15, 0, vec![setup_spec()]).unwrap()));

    let mut out = Vec::new();
    for (name, instruction) in cases {
        let sink = Collect::default();
        let mut monitor = PolicyMonitor::new(MonitorConfig { cluster: Cluster::Mainnet, commitment: Commitment::Confirmed, ws_url: String::new() }, sink.clone());
        monitor.process_policy_instructions("golden-signature", 77, vec![instruction.clone()]).await.unwrap();
        let events = sink.0.lock().unwrap().clone();
        out.push(json!({"name": name, "instruction": instruction_json(&instruction), "events": events}));
    }

    let mut earn_max = Vec::new();
    for (settings, base) in [(key("earn-max-a"), 7_u64), (key("earn-max-b"), 31)] {
        let topology = derive_earn_max_topology_with_policy_seed_base(settings, base).unwrap();
        let strategy = topology.strategy(StrategyKey::SyrupUsdcUsdc);
        let mut families = Vec::new();
        for family in [PolicyFamily::Collateral, PolicyFamily::Debt, PolicyFamily::Swap] {
            let create = canonical_policy_create(topology, strategy, family, settings, key("earn-max-root"), key("delegate")).unwrap();
            let update = canonical_policy_update(topology, strategy, family, settings, settings, key("delegate")).unwrap();
            families.push(json!({"family": family.label(), "create": instruction_json(&create), "update": instruction_json(&update)}));
        }
        earn_max.push(json!({"settings": settings.to_string(), "policy_seed_base": base, "delegate": key("delegate").to_string(), "families": families}));
    }
    println!("{}", serde_json::to_string_pretty(&json!({"cases": out, "earn_max": earn_max})).unwrap());
}

const TABLES: &[&str] = &[
    "route_policies", "managed_vaults", "balance_sweep_targets", "autodeposit_reconciliation_requests",
    "cross_mint_swap_policies", "cross_mint_vault_opt_ins", "earn_reconciliation_jobs", "earn_chain_mutations",
    "user_yield_positions", "user_yield_position_deposits", "user_yield_position_withdrawals",
    "user_yield_position_holding_events", "vault_position_snapshots", "vault_position_snapshot_positions",
    "vault_reserve_positions_current", "vault_idle_token_balances_current", "earn_chain_refund_events",
    "earn_deposit_onboarding_attempts", "earn_max_policy_sets", "projection_offsets", "laserstream_replay_cursors",
    "multiply_route_states", "multiply_operations", "multiply_position_snapshots",
];

async fn store(script: &str, url: &str) {
    let pool = PgPoolOptions::new().max_connections(2).connect(url).await.unwrap();
    let store = OrchestratorStore::from_pool(pool.clone());
    let steps: Vec<Value> = serde_json::from_str(&fs::read_to_string(script).unwrap()).unwrap();
    let mut results = Vec::new();
    for step in steps {
        let op = step["op"].as_str().unwrap().to_owned();
        let input = step["input"].clone();
        let result: Result<Value, String> = async {
            let err = |e: loyal_yield_store::OrchestratorError| e.to_string();
            match op.as_str() {
                "record_policy_match" => store.record_policy_match(serde_json::from_value(input).unwrap()).await.map(|_| json!(null)).map_err(err),
                "record_setup_policy_match" => store.record_setup_policy_match(serde_json::from_value(input).unwrap()).await.map(|_| json!(null)).map_err(err),
                "record_balance_sweep_policy_match" => store.record_balance_sweep_policy_match(serde_json::from_value(input).unwrap()).await.map(|_| json!(null)).map_err(err),
                "record_cross_mint_swap_policy_manifest" => store.record_cross_mint_swap_policy_manifest(serde_json::from_value(input).unwrap()).await.map(|_| json!(null)).map_err(err),
                "record_policy_removal" => store.record_policy_removal(serde_json::from_value(input).unwrap()).await.map(|_| json!(null)).map_err(err),
                "record_recurring_delegation" => store.record_autodeposit_recurring_delegation(serde_json::from_value(input).unwrap()).await.map(|_| json!(null)).map_err(err),
                "project_earn_max_policy_set" => store.project_earn_max_policy_set(step["consumer"].as_str().unwrap(), serde_json::from_value(input).unwrap()).await.map(|_| json!(null)).map_err(err),
                "project_earn_max_intent" => store.project_earn_max_intent(serde_json::from_value(input).unwrap()).await.map(|inserted| json!(inserted)).map_err(err),
                "job" => {
                    let vault = &step["vault"];
                    store.enqueue_earn_reconciliation_jobs(EarnReconciliationEnqueueInput {
                        consumer_name: step["consumer"].as_str().unwrap().to_owned(),
                        event_key: step["event_key"].as_str().unwrap().to_owned(),
                        durable_slot: step["durable_slot"].as_u64().unwrap(),
                        event_payload: step["event_payload"].clone(),
                        vaults: vec![EarnReconciliationVaultInput {
                            settings: vault["settings"].as_str().unwrap().to_owned(),
                            vault_index: vault["vault_index"].as_u64().unwrap() as u8,
                            vault_pubkey: vault["vault"].as_str().unwrap().to_owned(),
                            vault_payload: vault.clone(),
                        }],
                        autodeposit_target_ids: vec![],
                    }).await.map_err(err)?;
                    let Some(job) = store.claim_earn_reconciliation_job(step["consumer"].as_str().unwrap(), "golden-owner", 120).await.map_err(err)? else {
                        return Ok(json!("idle"));
                    };
                    let mutation: EarnDirectMutation = serde_json::from_value(step["mutation"].clone()).unwrap();
                    store.complete_earn_reconciliation_job(job.id, "golden-owner", &mutation).await.map(|outcome| json!(outcome.applied_mutations)).map_err(err)
                }
                "sql" => {
                    let mut query = sqlx::query(step["statement"].as_str().unwrap());
                    for arg in step["args"].as_array().unwrap() {
                        query = match arg {
                            Value::Number(n) => query.bind(n.as_i64().unwrap()),
                            other => query.bind(other.as_str().unwrap().to_owned()),
                        };
                    }
                    query.execute(&pool).await.map(|_| json!(null)).map_err(|e| e.to_string())
                }
                "admit_external" => {
                    let route: MultiplyRouteState = serde_json::from_value(step["route"].clone()).unwrap();
                    let operation: MultiplyOperation = serde_json::from_value(step["operation"].clone()).unwrap();
                    let mut lease = store.lease_multiply_route_state(step["route_key"].as_str().unwrap(), "golden-owner", chrono::Utc::now() + chrono::Duration::seconds(30)).await.map_err(err)?.ok_or("leased")?;
                    let inserted = store.admit_external_multiply_operation(&mut lease, &route, &operation).await.map_err(err)?;
                    store.release_multiply_route_lease(&lease).await.map_err(err)?;
                    Ok(json!(inserted))
                }
                other => panic!("unknown op {other}"),
            }
        }
        .await;
        results.push(match result {
            Ok(value) => json!({"op": op, "ok": true, "value": value}),
            Err(error) => json!({"op": op, "ok": false, "error": error}),
        });
    }
    let mut tables = serde_json::Map::new();
    for table in TABLES {
        let row = sqlx::query(&dump_sql(table)).fetch_one(&pool).await.unwrap();
        let value: Value = row.get(0);
        tables.insert((*table).to_owned(), value);
    }
    println!("{}", serde_json::to_string_pretty(&json!({"steps": results, "tables": tables})).unwrap());
}

// Shared with the Go test: every row without its wall-clock (*_at) columns,
// ordered by its own canonical text.
fn dump_sql(table: &str) -> String {
    format!("SELECT coalesce(jsonb_agg(r ORDER BY r::text), '[]'::jsonb) FROM (SELECT (SELECT coalesce(jsonb_object_agg(key, value), '{{}}'::jsonb) FROM jsonb_each(to_jsonb(t)) WHERE key NOT LIKE '%\\_at') AS r FROM loyal_yield.{table} t) rows")
}

#[tokio::main]
async fn main() {
    let args = env::args().collect::<Vec<_>>();
    match args.get(1).map(String::as_str) {
        Some("detect") => detect().await,
        Some("store") => store(&args[2], &args[3]).await,
        _ => panic!("usage: detect | store <script> <url>"),
    }
}
