//! Offline diagnostic at the normalized policy-eligible source frontier.
//! Uses this binary's real capacity curves, core wave planner and fee guard.
//! Does not prove SQL/policy admission, freshness or execution compatibility.
use super::*;
use loyal_yield_orchestrator::fleet_orchestration::{
    CommittedSourceOutflow, CommittedTargetInflow, FleetObservationResult, FleetObservationStats,
    ImmutableMarketEpoch, MarketEpochReserve, OpportunityInput,
};
use serde::Deserialize;
use sha2::{Digest, Sha256};

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Reserve {
    mint: String,
    supply: i64,
    apy: i64,
    inflow: i64,
    outflow: i64,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Vault {
    id: i64,
    source: String,
    targets: Vec<String>,
    amount: i64,
    collateral: i64,
    tenant: String,
    #[serde(default, rename = "idleMint")]
    idle_mint: Option<String>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Case {
    name: String,
    reserves: BTreeMap<String, Reserve>,
    vaults: Vec<Vault>,
}
#[derive(Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct Fixture {
    schema_version: u8,
    cases: Vec<Case>,
}

#[test]
#[ignore = "offline diagnostic requires FLEET_DECISION_FIXTURE and FLEET_DECISION_OUTPUT"]
fn produce_shared_input_decisions() -> Result<(), Box<dyn Error>> {
    let raw = std::fs::read(env::var("FLEET_DECISION_FIXTURE")?)?;
    let fixture: Fixture = serde_json::from_slice(&raw)?;
    assert_eq!(fixture.schema_version, 1);
    let now = DateTime::parse_from_rfc3339("2026-01-01T00:00:00Z")?.with_timezone(&Utc);
    let mut cases = Vec::new();
    for case in fixture.cases {
        let mut reserves = Vec::new();
        for (name, r) in &case.reserves {
            // Only curve inputs are consumed here. Evidence placeholders do
            // not pass through market/policy verification and are not proof.
            let reserve: MarketEpochReserve = serde_json::from_value(json!({
                "stateEventId":1,"accountDataHash":"fixture","stateObservedAt":now,"stateSlot":1000,
                "verificationCommitment":"confirmed","reserve":name,"market":format!("market-{name}"),
                "liquidityMint":r.mint,"mintDecimals":6,"marketPriceUsdMicros":1000000,
                "reserveLastUpdateSlot":1000,"economicSlotLag":0,"economicExpiresAt":now+ChronoDuration::minutes(5),
                "reserveLastUpdateStale":false,"reservePriceStatus":1,"marketPriceLastUpdatedTs":0,
                "availableAmountRaw":"0","borrowedAmountRaw":"0","totalSupplyAmountRaw":r.supply.to_string(),
                "utilizationPpm":0,"borrowApyBps":0,"observedAt":now,"slot":1000,
                "supplyApyBps":r.apy,"totalSupplyUsdMicros":r.supply,"targetEligible":true
            }))?;
            reserves.push(reserve);
        }
        let observation = FleetObservationResult {
            market_epoch: ImmutableMarketEpoch {
                optimizer_epoch_id: 7,
                fingerprint: "fixture".into(),
                catalog_fingerprint: "fixture".into(),
                captured_at: now,
                expires_at: now + ChronoDuration::minutes(5),
                catalog_expires_at: now + ChronoDuration::minutes(5),
                catalog_reserve_count: reserves.len(),
                oldest_market_observed_at: Some(now),
                newest_market_observed_at: Some(now),
                minimum_market_slot: Some(1000),
                maximum_market_slot: Some(1000),
                mint_coverage: vec![],
                reserves,
            },
            opportunities: vec![],
            stats: FleetObservationStats::default(),
            committed_target_inflows: case
                .reserves
                .iter()
                .map(|(name, r)| CommittedTargetInflow {
                    target_reserve: name.clone(),
                    principal_usd_micros: r.inflow,
                })
                .collect(),
            committed_source_outflows: case
                .reserves
                .iter()
                .map(|(name, r)| CommittedSourceOutflow {
                    source_reserve: name.clone(),
                    principal_usd_micros: r.outflow,
                })
                .collect(),
        };
        let mut inputs = Vec::new();
        let mut routes = BTreeMap::new();
        let mut vaults: Vec<_> = case.vaults.iter().collect();
        vaults.sort_by_key(|v| {
            (
                v.id,
                v.idle_mint.is_none(),
                v.idle_mint
                    .clone()
                    .unwrap_or_else(|| case.reserves[&v.source].mint.clone()),
                v.source.clone(),
            )
        });
        for vault in vaults {
            let idle = vault.idle_mint.is_some();
            let source = case.reserves.get(&vault.source);
            let source_mint = vault
                .idle_mint
                .as_deref()
                .unwrap_or_else(|| source.expect("reserve source required").mint.as_str());
            let source_apy = if idle {
                0
            } else {
                source.expect("reserve source required").apy
            };
            for target_name in &vault.targets {
                let target = &case.reserves[target_name];
                if target.apy <= source_apy || idle && target.mint != source_mint {
                    continue;
                }
                let cross = source_mint != target.mint;
                // Normalized source adapter: same one-collateral-unit recovery
                // anchor as the observer; source parsing is outside this test.
                let amount = if cross {
                    if vault.collateral <= 1 {
                        continue;
                    }
                    i64::try_from(
                        i128::from(vault.amount) * i128::from(vault.collateral - 1)
                            / i128::from(vault.collateral),
                    )?
                } else {
                    vault.amount
                };
                let id = i64::try_from(inputs.len() + 1)?;
                routes.insert(
                    id,
                    if idle {
                        "idle_vault_deposit"
                    } else if cross {
                        "cross_mint_jupiter"
                    } else {
                        "same_mint"
                    },
                );
                let multiplier = if cross { 3 } else { 1 };
                inputs.push(OpportunityInput {
                    opportunity_id: id,
                    optimizer_epoch_id: 7,
                    vault_id: vault.id,
                    tenant_id: vault.tenant.clone(),
                    source_snapshot_id: vault.id,
                    observed_slot: 1000,
                    mint: source_mint.to_owned(),
                    source_reserve: if idle {
                        format!("idle-vault:vault-{}", vault.id)
                    } else {
                        vault.source.clone()
                    },
                    target_reserve: target_name.clone(),
                    notional_usd_micros: amount,
                    source_net_apy_bps: source_apy,
                    target_net_apy_bps: target.apy,
                    confidence_ppm: 950000,
                    expected_service_millis: 15_000 * multiplier as u64,
                    holding_horizon_seconds: 2_592_000,
                    estimated_execution_cost_usd_micros: if idle {
                        500_000
                    } else {
                        DEFAULT_ESTIMATED_COST_USD_MICROS * multiplier
                    },
                    age_seconds: 0,
                    fairness_credit: 0,
                    writable_conflict_keys: vec![
                        format!("vault:vault-{}", vault.id),
                        "policy:1".into(),
                        format!(
                            "source-reserve:{}",
                            if idle { "idle" } else { &vault.source }
                        ),
                        format!("target-reserve:{target_name}"),
                    ],
                });
            }
        }
        let wave = plan_capacity_aware_wave(
            inputs,
            &EconomicPolicy::default(),
            capacity_curves(&observation),
            &WaveLimits {
                max_opportunities: DEFAULT_WAVE_SIZE,
                max_notional_usd_micros: 1_000_000_000_000_000,
                max_per_tenant: DEFAULT_WAVE_SIZE.clamp(1, 64),
                max_per_writable_conflict_key: 64,
            },
        )
        .map_err(|e| format!("{}: {e:?}", case.name))?;
        let mut selected = Vec::new();
        for chosen in wave.selected {
            let o = chosen.opportunity;
            let e = chosen.economics;
            let route = routes[&o.opportunity_id];
            let fee_policy = RouteFeePolicy::default();
            let Ok(fee) = route_fee_budget(e.net_holding_gain_usd_micros, fee_policy) else {
                continue;
            };
            if route == "cross_mint_jupiter"
                && !cross_mint_fee_envelope_is_covered(fee.cap_lamports, fee_policy)
            {
                continue;
            }
            selected.push(json!({"vaultId":o.vault_id,"source":if route == "idle_vault_deposit" { "" } else { &o.source_reserve },"target":o.target_reserve,"route":route,
                "amount":o.notional_usd_micros,"sourceApy":e.capacity_adjusted_source_net_apy_bps,
                "targetApy":e.capacity_adjusted_target_net_apy_bps,"edge":e.capacity_adjusted_net_edge_bps,
                "netGain":e.net_holding_gain_usd_micros,"priority":e.total_priority,"feeCap":fee.cap_lamports}));
        }
        cases.push(json!({"name":case.name,"selected":selected}));
    }
    std::fs::write(
        env::var("FLEET_DECISION_OUTPUT")?,
        serde_json::to_vec(&json!({"schemaVersion":1,"implementation":"rust",
        "fixtureSha256":format!("{:x}",Sha256::digest(raw)),"cases":cases}))?,
    )?;
    Ok(())
}
