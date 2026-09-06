use super::*;

// Assertions use finalized execution state, including collateral and idle SPL
// custody. The local mock's exchange rate is exactly 1:1 (no oracle/interest).
pub(super) async fn verify(
    runtime: &SameMintRouteRuntime,
    opportunity: &RebalanceOpportunityRecord,
    same_mint: bool,
) -> Result<Value, Box<dyn Error>> {
    let client = &runtime.client;
    let vault = load_active_vault(
        &runtime.pool,
        &required_plan_string(&opportunity.execution_plan, "settings")?,
        i16::try_from(required_plan_i64(
            &opportunity.execution_plan,
            "vault_index",
        )?)?,
    )
    .await?
    .ok_or("terminal vault missing")?;
    let source = opportunity
        .source_reserve
        .clone()
        .ok_or("terminal source missing")?;
    let preview = load_chain_reconcile_preview_from_rpc(
        &runtime.rpc,
        &vault,
        &[source.clone(), opportunity.target_reserve.clone()],
        Some(1000),
    )?;
    let source_position = chain_position_for_reserve(&preview, &source)?;
    let target = chain_position_for_reserve(&preview, &opportunity.target_reserve)?;
    let expected_deposit = u64::try_from(opportunity.amount_raw)?;
    let expected_residual = if same_mint { 0 } else { 1 };
    if source_position.amount_raw != expected_residual
        || target.amount_raw != expected_deposit
        || source_position.vault_liquidity_amount_raw != 0
        || target.vault_liquidity_amount_raw != 0
    {
        return Err(format!("terminal executed balances differ: source={}, target={}, sourceIdle={}, targetIdle={}, expectedDeposit={expected_deposit}",source_position.amount_raw,target.amount_raw,source_position.vault_liquidity_amount_raw,target.vault_liquidity_amount_raw).into());
    }
    let current_slot = runtime.rpc.get_slot()?;
    let slot: u64 = if current_slot == 1000 {
        runtime.rpc.send(
            solana_client::rpc_request::RpcRequest::Custom {
                method: "advanceSlot",
            },
            json!([1001]),
        )?
    } else {
        current_slot
    };
    if slot != 1001 {
        return Err("telemetry slot did not advance".into());
    }
    let supply = runtime
        .rpc
        .get_token_supply(&Pubkey::from_str(&target.collateral_mint)?)?;
    if supply.decimals != 6 {
        return Err("unexpected collateral decimals".into());
    }
    client
        .observe_target_capacity(TargetCapacityObservation {
            cluster: opportunity.cluster.clone(),
            target_reserve: opportunity.target_reserve.clone(),
            liquidity_mint: opportunity.target_liquidity_mint.clone(),
            observed_supply_usd_micros: supply.amount.parse()?,
            observed_slot: 1001,
            maximum_inflight_usd_micros: 100_000_000_000,
        })
        .await?;
    let terminal:Value=sqlx::query_scalar("SELECT jsonb_build_object('opportunity',to_jsonb(o),'decision',to_jsonb(d),'submissions',(SELECT jsonb_agg(jsonb_build_object('id',s.id,'state',s.submission_state,'signature',s.transaction_signature,'confirmedSlot',s.confirmed_slot,'reconciledSlot',s.reconciled_slot) ORDER BY s.id) FROM loyal_yield.signed_route_submissions s WHERE s.opportunity_id=o.id),'activeCapacity',(SELECT count(*) FROM loyal_yield.target_capacity_reservations r WHERE r.decision_id=d.id AND r.reservation_state<>'released'),'conflicts',(SELECT count(*) FROM loyal_yield.route_account_conflict_leases c WHERE c.opportunity_id=o.id)) FROM loyal_yield.rebalance_opportunities o JOIN loyal_yield.rebalance_decisions d ON d.id=o.decision_id WHERE o.id=$1").bind(opportunity.id).fetch_one(client.pool()).await?;
    eprintln!(
        "connected terminal balances: source={}, target={}, idle=0; terminal={terminal}",
        source_position.amount_raw, target.amount_raw
    );
    if terminal["opportunity"]["opportunity_state"] != "completed"
        || terminal["decision"]["status"] != "confirmed"
        || terminal["activeCapacity"].as_i64() != Some(0)
        || terminal["conflicts"].as_i64() != Some(0)
    {
        return Err("terminal queue/ownership verification failed".into());
    }
    let submissions = terminal["submissions"]
        .as_array()
        .ok_or("terminal submissions missing")?;
    if submissions.len() != if same_mint { 1 } else { 3 }
        || submissions
            .iter()
            .any(|s| s["state"] != "reconciled" || s["reconciledSlot"].as_i64() != Some(1000))
    {
        return Err("terminal signed work incomplete".into());
    }
    if !same_mint
        && (terminal["decision"]["terminal_outcome"] != "completed_target"
            || terminal["decision"]["custody_amount_raw"].as_i64() != Some(0))
    {
        return Err("terminal movement custody unresolved".into());
    }
    if client
        .lease_next_rebalance_opportunity(
            &opportunity.cluster,
            "connected-terminal-duplicate",
            RebalanceOpportunityClaimKind::Execute,
            Utc::now() + ChronoDuration::seconds(2),
        )
        .await?
        .is_some()
    {
        return Err("terminal work became executable again".into());
    }
    Ok(terminal)
}
