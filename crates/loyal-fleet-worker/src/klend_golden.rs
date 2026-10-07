//! Byte-parity source for the Go same-mint route builder
//! (go/workers/testdata/klend/same-mint-route.golden.json). Compiled only with
//! the `klend-golden` feature; it calls the production `build_route_execution_plan`
//! against an in-memory RPC so the record is this worker's exact instruction order.
use super::*;

fn position(value: &Value, vault: Pubkey) -> Result<ChainPositionSummary, Box<dyn Error>> {
    let text = |key: &str| -> Result<String, Box<dyn Error>> {
        Ok(value[key]
            .as_str()
            .ok_or(format!("position.{key} missing"))?
            .to_owned())
    };
    let optional = |key: &str| {
        value[key]
            .as_str()
            .filter(|s| !s.is_empty())
            .map(str::to_owned)
    };
    let reserves = |key: &str| -> Vec<String> {
        value[key]
            .as_array()
            .into_iter()
            .flatten()
            .filter_map(|v| v.as_str().map(str::to_owned))
            .collect()
    };
    let market = Pubkey::from_str(&text("market")?)?;
    let obligation = derive_kamino_vanilla_obligation(vault, market);
    let farm = optional("reserveFarmState");
    let farm_user = farm
        .as_deref()
        .map(|farm| {
            Pubkey::from_str(farm)
                .map(|farm| derive_kamino_obligation_farm_user_state(farm, obligation).to_string())
        })
        .transpose()?;
    Ok(ChainPositionSummary {
        reserve: text("reserve")?,
        market: market.to_string(),
        liquidity_mint: text("liquidityMint")?,
        liquidity_token_program: text("liquidityTokenProgram")?,
        reserve_liquidity_supply: text("liquiditySupply")?,
        collateral_mint: text("collateralMint")?,
        reserve_collateral_supply: text("collateralSupply")?,
        collateral_farm: farm,
        collateral_farm_user_state: farm_user,
        collateral_farm_user_state_exists: value["farmUserExists"].as_bool().unwrap_or(false),
        pyth_oracle: optional("pythOracle"),
        switchboard_price_oracle: optional("switchboardPriceOracle"),
        switchboard_twap_oracle: optional("switchboardTwapOracle"),
        scope_prices: optional("scopePrices"),
        obligation: obligation.to_string(),
        obligation_exists: value["obligationExists"]
            .as_bool()
            .ok_or("position.obligationExists missing")?,
        obligation_deposit_reserves: reserves("obligationDepositReserves"),
        obligation_borrow_reserves: reserves("obligationBorrowReserves"),
        amount_raw: value["amountRaw"].as_u64().unwrap_or(0),
        redeemable_liquidity_amount_raw: value["redeemableAmountRaw"].as_u64().unwrap_or(0),
        vault_liquidity_ata: text("vaultLiquidityAta")?,
        vault_liquidity_token_account_exists: true,
        vault_liquidity_amount_raw: 0,
    })
}

fn policy(value: &Value, key: &str) -> Result<Option<(String, Vec<u8>)>, Box<dyn Error>> {
    let Some(account) = value[key].as_str().filter(|s| !s.is_empty()) else {
        return Ok(None);
    };
    let data = BASE64_STANDARD.decode(
        value[format!("{key}Data")]
            .as_str()
            .ok_or("policy data missing")?,
    )?;
    Ok(Some((account.to_owned(), data)))
}

/// Builds one same-mint route exactly as the fleet executor does. The request
/// is the Go golden request; POLICY_KEYPAIR must already hold its signer.
pub fn same_mint_route(request: &Value) -> Result<Value, Box<dyn Error>> {
    let vault_pubkey = Pubkey::from_str(request["vault"].as_str().ok_or("vault missing")?)?;
    let source = position(&request["source"], vault_pubkey)?;
    let target = position(&request["target"], vault_pubkey)?;
    let (route_policy, route_data) =
        policy(request, "routePolicy")?.ok_or("route policy missing")?;
    let setup = policy(request, "setupPolicy")?;
    let mint = source.liquidity_mint.clone();
    let vault = SelectedVault {
        id: VaultId(1),
        settings: request["settings"]
            .as_str()
            .ok_or("settings missing")?
            .to_owned(),
        authority: route_policy.clone(),
        policy_seed: 1,
        vault_index: i16::from(u8::try_from(
            request["vaultIndex"].as_u64().ok_or("vaultIndex missing")?,
        )?),
        vault_pubkey: vault_pubkey.to_string(),
        policy_account: route_policy.clone(),
        setup_policy_account: setup.as_ref().map(|(account, _)| account.clone()),
        setup_policy_seed: setup.as_ref().map(|_| 2),
        delegated_signers: Vec::new(),
        threshold: 1,
        route_modes: vec![SAME_MINT_ROUTE_MODE.to_owned()],
        stable_mints: vec![mint.clone()],
        kamino_markets: vec![source.market.clone(), target.market.clone()],
        kamino_liquidity_mints: vec![mint.clone()],
        swap_lanes: Value::Array(Vec::new()),
    };
    let preflight = PolicyAccountPreflight {
        policy_account: route_policy,
        source_market: source.market.clone(),
        target_market: target.market.clone(),
        decoded: decode_squads_policy_account(&route_data)?,
    };
    let amount = request["amountRaw"].as_u64().ok_or("amountRaw missing")?;
    let input = SameMintRebalanceInput {
        vault_id: Some(VaultId(1)),
        settings: Some(vault.settings.clone()),
        vault_index: Some(vault.vault_index),
        source_reserve: source.reserve.clone(),
        target_reserve: target.reserve.clone(),
        liquidity_mint: mint,
        amount_raw: i64::try_from(amount)?,
        route_amount_semantics: "liquidity_amount".to_owned(),
        source_amount_semantics: Some("kamino_obligation_collateral_deposited_amount".to_owned()),
        source_collateral_amount_raw: Some(i64::try_from(source.amount_raw)?),
        redeemable_source_liquidity_amount_raw: Some(i64::try_from(amount)?),
        idle_vault_liquidity_amount_raw: None,
        expected_source_snapshot_id: SnapshotId(1),
        source_apy_bps: 0,
        target_apy_bps: 0,
        estimated_edge_bps: 0,
        estimated_cost_lamports: 0,
        dry_run: true,
    };
    let reserve_move = ReserveMove {
        source_reserve: source.reserve.clone(),
        target_reserve: target.reserve.clone(),
    };
    let fee_payer = Pubkey::from_str(request["feePayer"].as_str().ok_or("feePayer missing")?)?;
    let selection = RouteFeePayerSelection {
        pubkey: fee_payer,
        kind: RouteFeePayerKind::Policy,
        reason: "golden".to_owned(),
        mature_route: false,
        observed_balance_lamports: None,
        observed_balance_slot: None,
        observed_balance_at: None,
        shard: None,
    };
    // Responses in the order build_route_execution_plan reads them.
    let lamports = |key: &str| request[key].as_u64().unwrap_or(0);
    let context = |value: Value| json!({"context": {"slot": 1000}, "value": value});
    let mut mocks = MocksMap::default();
    let farm_setup = [&source, &target]
        .iter()
        .any(|p| p.collateral_farm.is_some() && !p.collateral_farm_user_state_exists);
    if farm_setup {
        mocks.insert(
            RpcRequest::GetBalance,
            context(json!(lamports("payerLamports"))),
        );
    }
    if let Some((_, data)) = &setup {
        mocks.insert(
            RpcRequest::GetAccountInfo,
            context(json!({"data": [BASE64_STANDARD.encode(data), "base64"], "executable": false, "lamports": 1_000_000, "owner": SQUADS_SMART_ACCOUNT_PROGRAM_ID.to_string(), "rentEpoch": 0, "space": data.len()})),
        );
    }
    mocks.insert(
        RpcRequest::GetMinimumBalanceForRentExemption,
        json!(lamports("rentLamports")),
    );
    mocks.insert(
        RpcRequest::GetBalance,
        context(json!(lamports("vaultLamports"))),
    );
    mocks.insert(
        RpcRequest::GetBalance,
        context(json!(lamports("payerLamports"))),
    );
    let rpc = RpcClient::new_mock_with_mocks_map("klend-golden", mocks);
    let preview = ChainReconcilePreview {
        observed_slot: 1000,
        vault_user_metadata: user_metadata(&KLEND_PROGRAM_ID, &vault_pubkey)
            .0
            .to_string(),
        vault_user_metadata_exists: true,
        positions: vec![source, target],
        idle_token_balances: Vec::new(),
        rpc_account_reads: FleetRpcAccountReadEvidence::default(),
    };
    let plan = build_route_execution_plan(
        Some(&rpc),
        &vault,
        &preview,
        &reserve_move,
        &input,
        Some(&preflight),
        &selection,
    )?;
    let encode = |ix: &Instruction| {
        json!({
            "program": ix.program_id.to_string(),
            "accounts": ix.accounts.iter().map(|a| json!({"address": a.pubkey.to_string(), "signer": a.is_signer, "writable": a.is_writable})).collect::<Vec<_>>(),
            "data": BASE64_STANDARD.encode(&ix.data),
        })
    };
    Ok(json!({
        "instructions": plan.pre_instructions.iter().chain(plan.instructions.iter()).map(encode).collect::<Vec<_>>(),
        "routeSteps": plan.preview.route_steps,
    }))
}
