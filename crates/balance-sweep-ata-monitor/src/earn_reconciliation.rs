use std::{
    collections::{BTreeMap, BTreeSet},
    fs,
    future::Future,
    path::Path,
    pin::Pin,
    str::FromStr,
    sync::Arc,
};

use anyhow::{bail, Context, Result};
use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use klend_interface::{
    from_account_data,
    state::{Obligation, Reserve},
    KLEND_PROGRAM_ID,
};
use loyal_actions::{
    decode_squads_policy_create_actions, derive_associated_token_account,
    derive_kamino_vanilla_obligation, earn_stablecoin, earn_stablecoins, SquadsSettingsActionView,
    SQUADS_SMART_ACCOUNT_PROGRAM_ID,
};
use loyal_yield_store::{
    EarnCleanupMutation, EarnDepositMutation, EarnDirectMutation, EarnDirectReconciliationInput,
    EarnDirectReconciliationOutcome, EarnIdleTokenMutation, EarnPolicyOnlyMutation,
    EarnReserveMutation, OrchestratorStore, PolicyMatchInput,
};
use num_bigint::BigUint;
use num_traits::{ToPrimitive, Zero};
use serde::Deserialize;
use serde_json::{json, Value};
use solana_account_decoder::UiAccountEncoding;
use solana_client::{
    rpc_client::RpcClient,
    rpc_config::{RpcAccountInfoConfig, RpcProgramAccountsConfig, RpcTransactionConfig},
    rpc_filter::{Memcmp, RpcFilterType},
};
use solana_program::program_pack::Pack;
use solana_sdk::{
    commitment_config::CommitmentConfig,
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
    signature::Signature,
    transaction::VersionedTransaction,
};
use solana_transaction_status_client_types::UiTransactionEncoding;

use crate::smart_account::{EarnVaultWatch, NormalizedEarnUpdate, SubscriptionWatchSet};

pub trait EarnChainReader: Send + Sync {
    fn mutation_for<'a>(
        &'a self,
        update: &'a NormalizedEarnUpdate,
        vault: &'a EarnVaultWatch,
    ) -> Pin<Box<dyn Future<Output = Result<EarnDirectMutation>> + Send + 'a>>;
}

pub struct RpcEarnChainReader {
    rpc: Arc<RpcClient>,
    store: OrchestratorStore,
}

impl RpcEarnChainReader {
    pub fn new(rpc_url: impl Into<String>, store: OrchestratorStore) -> Self {
        Self {
            rpc: Arc::new(RpcClient::new_with_commitment(
                rpc_url.into(),
                CommitmentConfig::confirmed(),
            )),
            store,
        }
    }
}

impl EarnChainReader for RpcEarnChainReader {
    fn mutation_for<'a>(
        &'a self,
        update: &'a NormalizedEarnUpdate,
        vault: &'a EarnVaultWatch,
    ) -> Pin<Box<dyn Future<Output = Result<EarnDirectMutation>> + Send + 'a>> {
        Box::pin(async move {
            let context = self
                .store
                .load_earn_reconciliation_context(&vault.settings, vault.vault_index, &vault.vault)
                .await?;
            let rpc = Arc::clone(&self.rpc);
            let update = update.clone();
            let vault = vault.clone();
            tokio::task::spawn_blocking(move || {
                resolve_rpc_mutation(rpc.as_ref(), &update, &vault, context)
            })
            .await
            .context("Earn RPC proof task panicked")?
        })
    }
}

fn resolve_rpc_mutation(
    rpc: &RpcClient,
    update: &NormalizedEarnUpdate,
    vault: &EarnVaultWatch,
    context: loyal_yield_store::EarnReconciliationContext,
) -> Result<EarnDirectMutation> {
    if let Some(withdrawal) = context.full_withdrawal.as_ref() {
        let proof = read_cleanup_proof(rpc, vault, withdrawal.confirmed_slot)?;
        if !proof.balances_zero {
            return Ok(EarnDirectMutation::Noop);
        }
        let (cleanup_signature, confirmed_slot) = if proof.policies_closed {
            let is_policy_deletion = update.event_kind == "account_deleted"
                && update.account_pubkey.as_deref().is_some_and(|pubkey| {
                    vault
                        .accounts
                        .iter()
                        .any(|account| account.role == "policy" && account.pubkey == pubkey)
                });
            if !is_policy_deletion {
                // Policy closure has its own account-deletion frame carrying
                // the close transaction. A later idle/obligation frame must
                // not replace that evidence with an unrelated signature.
                return Ok(EarnDirectMutation::Noop);
            }
            (
                update
                    .signature
                    .clone()
                    .context("closed policy update has no transaction signature")?,
                update.slot,
            )
        } else {
            (withdrawal.signature.clone(), withdrawal.confirmed_slot)
        };
        return Ok(EarnDirectMutation::Cleanup(EarnCleanupMutation {
            settings: vault.settings.clone(),
            vault_index: vault.vault_index,
            vault_pubkey: vault.vault.clone(),
            cleanup_signature,
            confirmed_slot,
            observed_at: None,
        }));
    }

    if let Some(onboarding) = context.onboarding.as_ref() {
        if onboarding.status == "route_policy_confirmed" {
            if let Some(policy) = read_policy_only_proof(
                rpc,
                update,
                vault,
                onboarding,
                context.route_policy.as_ref(),
            )? {
                return Ok(EarnDirectMutation::PolicyOnly(policy));
            }
        }
    }

    let Some(route_policy) = context.route_policy else {
        return Ok(EarnDirectMutation::Noop);
    };
    let Some(onboarding) = context.onboarding.as_ref() else {
        return Ok(EarnDirectMutation::Noop);
    };
    if onboarding.status == "complete" || onboarding.deposit_signature.is_some() {
        return Ok(EarnDirectMutation::Noop);
    }
    read_deposit_proof(
        rpc,
        update,
        vault,
        route_policy,
        context.setup_policy,
        onboarding,
    )
}

struct CleanupProof {
    balances_zero: bool,
    policies_closed: bool,
}

fn read_cleanup_proof(
    rpc: &RpcClient,
    vault: &EarnVaultWatch,
    min_context_slot: u64,
) -> Result<CleanupProof> {
    let addresses = vault
        .accounts
        .iter()
        .map(|account| Pubkey::from_str(&account.pubkey))
        .collect::<std::result::Result<Vec<_>, _>>()?;
    let response = rpc.get_multiple_accounts_with_config(
        &addresses,
        RpcAccountInfoConfig {
            encoding: Some(UiAccountEncoding::Base64),
            commitment: Some(CommitmentConfig::confirmed()),
            min_context_slot: Some(min_context_slot),
            ..RpcAccountInfoConfig::default()
        },
    )?;
    if response.context.slot < min_context_slot {
        bail!(
            "cleanup proof context slot {} is below minimum {min_context_slot}",
            response.context.slot
        );
    }
    let vault_pubkey = Pubkey::from_str(&vault.vault)?;
    let mut balances_zero = true;
    let mut saw_policy = false;
    let mut policy_count = 0_usize;
    for ((binding, address), account) in vault
        .accounts
        .iter()
        .zip(addresses.iter())
        .zip(response.value.iter())
    {
        match binding.role.as_str() {
            "policy" => {
                policy_count += 1;
                if let Some(account) = account {
                    if account.owner != SQUADS_SMART_ACCOUNT_PROGRAM_ID {
                        bail!(
                            "policy account {address} has unexpected owner {}",
                            account.owner
                        );
                    }
                    saw_policy = true;
                }
            }
            "idle_token" => {
                if let Some(account) = account {
                    let (_, owner, amount) = decode_token_account(account)?;
                    if owner != vault_pubkey {
                        bail!("idle account {address} belongs to {owner}, expected {vault_pubkey}");
                    }
                    let known_product_idle =
                        earn_stablecoin(decode_token_account(account)?.0).is_some();
                    if amount > 0 && (!known_product_idle || amount >= 10_000) {
                        balances_zero = false;
                    }
                }
            }
            "obligation" => {
                if let Some(account) = account {
                    if account.owner != KLEND_PROGRAM_ID {
                        bail!(
                            "obligation {address} has unexpected owner {}",
                            account.owner
                        );
                    }
                    let obligation = from_account_data::<Obligation>(&account.data)
                        .context("decode Kamino obligation")?;
                    if obligation.owner != vault_pubkey {
                        bail!(
                            "obligation {address} belongs to {}, expected {vault_pubkey}",
                            obligation.owner
                        );
                    }
                    if obligation
                        .deposits
                        .iter()
                        .any(|deposit| deposit.deposited_amount > 0)
                        || obligation
                            .borrows
                            .iter()
                            .any(|borrow| borrow.borrow_reserve != Pubkey::default())
                    {
                        balances_zero = false;
                    }
                }
            }
            _ => {}
        }
    }
    if policy_count == 0 {
        bail!("cleanup proof has no policy bindings");
    }
    if vault_has_blocking_token_inventory(rpc, vault_pubkey, min_context_slot)? {
        balances_zero = false;
    }
    Ok(CleanupProof {
        balances_zero,
        policies_closed: !saw_policy,
    })
}

fn vault_has_blocking_token_inventory(
    rpc: &RpcClient,
    vault: Pubkey,
    min_context_slot: u64,
) -> Result<bool> {
    let product_idle_accounts = earn_stablecoins()
        .iter()
        .map(|asset| {
            (
                derive_associated_token_account(vault, asset.mint, asset.token_program),
                asset.mint,
                asset.token_program,
            )
        })
        .collect::<BTreeSet<_>>();
    for token_program in [spl_token::id(), spl_token_2022::id()] {
        let accounts = rpc.get_program_accounts_with_config(
            &token_program,
            RpcProgramAccountsConfig {
                filters: Some(vec![RpcFilterType::Memcmp(Memcmp::new_base58_encoded(
                    32,
                    vault.as_ref(),
                ))]),
                account_config: RpcAccountInfoConfig {
                    encoding: Some(UiAccountEncoding::Base64),
                    commitment: Some(CommitmentConfig::confirmed()),
                    min_context_slot: Some(min_context_slot),
                    ..RpcAccountInfoConfig::default()
                },
                with_context: Some(false),
                sort_results: Some(true),
            },
        )?;
        for (address, account) in accounts {
            let (mint, owner, amount) = decode_token_account(&account)?;
            if owner != vault {
                bail!("token inventory query returned account {address} for owner {owner}");
            }
            if amount == 0 {
                continue;
            }
            let is_product_idle = product_idle_accounts.contains(&(address, mint, token_program));
            if !is_product_idle || amount >= 10_000 {
                return Ok(true);
            }
        }
    }
    Ok(false)
}

fn read_policy_only_proof(
    rpc: &RpcClient,
    update: &NormalizedEarnUpdate,
    vault: &EarnVaultWatch,
    onboarding: &loyal_yield_store::EarnOnboardingContext,
    recorded_route: Option<&PolicyMatchInput>,
) -> Result<Option<EarnPolicyOnlyMutation>> {
    let Some(setup_account) = onboarding.setup_policy_account.as_deref() else {
        return Ok(None);
    };
    // Route and setup policies are commonly created by different
    // transactions. The route notification cannot prove the setup stage and
    // must remain a no-op; the setup account's own notification performs the
    // convergence once both accounts exist.
    if update.account_pubkey.as_deref() != Some(setup_account) {
        return Ok(None);
    }
    let addresses = [
        Pubkey::from_str(&onboarding.route_policy_account)?,
        Pubkey::from_str(setup_account)?,
    ];
    let response = rpc.get_multiple_accounts_with_config(
        &addresses,
        RpcAccountInfoConfig {
            encoding: Some(UiAccountEncoding::Base64),
            commitment: Some(CommitmentConfig::confirmed()),
            min_context_slot: Some(update.slot),
            ..RpcAccountInfoConfig::default()
        },
    )?;
    if response.value.iter().any(Option::is_none) {
        return Ok(None);
    }
    if response
        .value
        .iter()
        .flatten()
        .any(|account| account.owner != SQUADS_SMART_ACCOUNT_PROGRAM_ID)
    {
        bail!("policy-only proof observed a non-Squads policy owner");
    }
    let update_signature = update
        .signature
        .as_deref()
        .context("policy-only update is missing its transaction signature")?;
    let actions = read_squads_policy_actions(rpc, update_signature, update.slot)?;
    let settings = Pubkey::from_str(&vault.settings)?;
    let wallet = Pubkey::from_str(&vault.wallet)?;
    let setup_account_key = Pubkey::from_str(setup_account)?;
    let delegated_signer = Pubkey::from_str(&onboarding.delegated_signer)?;
    let setup_seed = onboarding
        .setup_policy_seed
        .context("policy-only onboarding has no setup policy seed")?;
    let setup_action = actions
        .iter()
        .find(|action| action.policy_account == setup_account_key)
        .context("setup transaction did not create the expected policy")?;
    validate_setup_policy_action(
        setup_action,
        settings,
        wallet,
        delegated_signer,
        setup_seed,
        vault.vault_index,
        onboarding,
    )?;

    let route_policy = recorded_route
        .cloned()
        .context("route policy is not canonical before setup reconciliation")?;
    if route_policy.policy_account != onboarding.route_policy_account
        || route_policy.policy_seed != onboarding.route_policy_seed
        || route_policy.settings != vault.settings
        || route_policy.authority != vault.wallet
    {
        bail!("recorded route policy does not match onboarding identity");
    }
    let setup_policy = PolicyMatchInput {
        signature: onboarding
            .setup_policy_signature
            .clone()
            .unwrap_or_else(|| update_signature.to_owned()),
        slot: onboarding
            .setup_policy_confirmed_slot
            .unwrap_or(update.slot),
        cluster: vault.environment.clone(),
        source_commitment: "confirmed".to_owned(),
        settings: vault.settings.clone(),
        authority: vault.wallet.clone(),
        policy_seed: setup_seed,
        policy_account: setup_account.to_owned(),
        vault_index: vault.vault_index,
        vault_pubkey: vault.vault.clone(),
        delegated_signers: vec![onboarding.delegated_signer.clone()],
        threshold: 1,
        route_modes: vec!["kamino_init_obligation".to_owned()],
        stable_mints: vec![onboarding.liquidity_mint.clone()],
        kamino_markets: onboarding.market.iter().cloned().collect(),
        kamino_liquidity_mints: vec![onboarding.liquidity_mint.clone()],
        universe_preset: None,
        risk_profile: None,
        swap_lanes: Value::Array(Vec::new()),
    };
    Ok(Some(EarnPolicyOnlyMutation {
        route_policy,
        setup_policy,
    }))
}

fn validate_setup_policy_action(
    action: &SquadsSettingsActionView,
    settings: Pubkey,
    wallet: Pubkey,
    delegated_signer: Pubkey,
    expected_seed: u64,
    vault_index: u8,
    onboarding: &loyal_yield_store::EarnOnboardingContext,
) -> Result<()> {
    if action.settings != settings
        || action.authority != wallet
        || action.policy_seed != expected_seed
        || action.payload.vault_index != vault_index
        || action.threshold != 1
        || action.delegated_signers != vec![delegated_signer]
    {
        bail!("decoded setup policy identity does not match onboarding");
    }
    let market = onboarding
        .market
        .as_deref()
        .map(Pubkey::from_str)
        .transpose()?
        .context("policy-only onboarding has no market")?;
    if !action.payload.pubkey_table.contains(&market) {
        bail!("decoded setup policy does not contain the onboarding market");
    }
    Ok(())
}

fn read_squads_policy_actions(
    rpc: &RpcClient,
    signature: &str,
    expected_slot: u64,
) -> Result<Vec<SquadsSettingsActionView>> {
    let parsed_signature =
        Signature::from_str(signature).context("invalid transaction signature")?;
    let transaction = rpc.get_transaction_with_config(
        &parsed_signature,
        RpcTransactionConfig {
            encoding: Some(UiTransactionEncoding::Base64),
            commitment: Some(CommitmentConfig::confirmed()),
            max_supported_transaction_version: Some(0),
        },
    )?;
    let value = serde_json::to_value(transaction).context("serialize policy transaction")?;
    require_transaction_slot(&value, signature, expected_slot)?;
    if value
        .pointer("/transaction/meta/err")
        .is_some_and(|error| !error.is_null())
    {
        bail!("policy transaction {signature} failed on chain");
    }
    let payload = value
        .pointer("/transaction/transaction")
        .and_then(|transaction| transaction.as_array().and_then(|items| items.first()))
        .and_then(Value::as_str)
        .context("policy transaction has no base64 payload")?;
    let bytes = BASE64_STANDARD
        .decode(payload)
        .context("decode policy transaction base64")?;
    let transaction: VersionedTransaction =
        bincode::deserialize(&bytes).context("decode policy versioned transaction")?;
    let mut account_keys = transaction.message.static_account_keys().to_vec();
    if let Some(loaded) = value.pointer("/transaction/meta/loadedAddresses") {
        for key in ["writable", "readonly"] {
            for address in loaded
                .get(key)
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
            {
                account_keys.push(Pubkey::from_str(
                    address.as_str().context("loaded address is not a string")?,
                )?);
            }
        }
    }
    let mut actions = Vec::new();
    for compiled in transaction.message.instructions() {
        let Some(program_id) = account_keys
            .get(compiled.program_id_index as usize)
            .copied()
        else {
            continue;
        };
        if program_id != SQUADS_SMART_ACCOUNT_PROGRAM_ID {
            continue;
        }
        let accounts = compiled
            .accounts
            .iter()
            .filter_map(|index| {
                let index = usize::from(*index);
                account_keys.get(index).copied().map(|pubkey| AccountMeta {
                    pubkey,
                    is_signer: transaction.message.is_signer(index),
                    is_writable: transaction.message.is_maybe_writable(index, None),
                })
            })
            .collect();
        actions.extend(
            decode_squads_policy_create_actions(&Instruction {
                program_id,
                accounts,
                data: compiled.data.clone(),
            })
            .context("decode Squads policy creation")?,
        );
    }
    Ok(actions)
}

fn read_deposit_proof(
    rpc: &RpcClient,
    update: &NormalizedEarnUpdate,
    vault: &EarnVaultWatch,
    route_policy: PolicyMatchInput,
    setup_policy: Option<PolicyMatchInput>,
    onboarding: &loyal_yield_store::EarnOnboardingContext,
) -> Result<EarnDirectMutation> {
    let signature = update
        .signature
        .as_deref()
        .context("deposit candidate is missing its transaction signature")?;
    let transaction = read_transaction_json(rpc, signature)?;
    let transaction_slot = transaction
        .get("slot")
        .and_then(Value::as_u64)
        .context("confirmed transaction has no slot")?;
    if transaction_slot != update.slot {
        bail!(
            "transaction {signature} landed at slot {transaction_slot}, expected account-update slot {}",
            update.slot
        );
    }
    let accounts = transaction_accounts(&transaction);
    if !accounts.contains(&onboarding.target_reserve) {
        return Ok(EarnDirectMutation::Noop);
    }
    let amount = transaction_owner_debit(
        &transaction,
        &onboarding.liquidity_mint,
        [&vault.wallet, &vault.vault],
    )?;
    if amount == 0 {
        return Ok(EarnDirectMutation::Noop);
    }
    let (reserve_amount, observed_slot) =
        read_current_reserve_amount(rpc, vault, onboarding, update.slot)?;
    if reserve_amount == 0 {
        return Ok(EarnDirectMutation::Noop);
    }

    let mut idle_state = Vec::new();
    for binding in vault
        .accounts
        .iter()
        .filter(|binding| binding.role == "idle_token")
    {
        let address = Pubkey::from_str(&binding.pubkey)?;
        let response = rpc.get_account_with_config(
            &address,
            RpcAccountInfoConfig {
                encoding: Some(UiAccountEncoding::Base64),
                commitment: Some(CommitmentConfig::confirmed()),
                min_context_slot: Some(update.slot),
                ..RpcAccountInfoConfig::default()
            },
        )?;
        let Some(account) = response.value else {
            continue;
        };
        let (mint, owner, balance) = decode_token_account(&account)?;
        if owner != Pubkey::from_str(&vault.vault)? {
            bail!(
                "watched idle token account {} has unexpected owner {owner}",
                binding.pubkey
            );
        }
        idle_state.push(EarnIdleTokenMutation {
            mint: mint.to_string(),
            amount_raw: balance,
            owner: owner.to_string(),
            token_account: binding.pubkey.clone(),
            observed_slot: response.context.slot,
            observed_at: None,
            source_commitment: "confirmed".to_owned(),
        });
    }
    Ok(EarnDirectMutation::Deposit(EarnDepositMutation {
        route_policy,
        setup_policy,
        deposit_signature: signature.to_owned(),
        deposit_slot: transaction_slot,
        observed_slot,
        deposit_mint: onboarding.liquidity_mint.clone(),
        principal_amount_raw: amount,
        target_reserve: onboarding.target_reserve.clone(),
        market: onboarding.market.clone(),
        liquidity_mint: onboarding.liquidity_mint.clone(),
        target_supply_apy_bps: None,
        wallet: vault.wallet.clone(),
        smart_account_address: vault.vault.clone(),
        reserve_state: vec![EarnReserveMutation {
            reserve: onboarding.target_reserve.clone(),
            market: onboarding.market.clone(),
            liquidity_mint: onboarding.liquidity_mint.clone(),
            amount_raw: reserve_amount,
            has_value: true,
            supply_apy_bps: None,
            borrow_apy_bps: None,
            planning_metadata: json!({
                "kind": "earn_laserstream_transaction_proof",
                "signature": signature,
                "amount_semantics": "principal_transaction_debit",
            }),
        }],
        idle_state,
        observed_at: None,
    }))
}

fn read_transaction_json(rpc: &RpcClient, signature: &str) -> Result<Value> {
    let signature = Signature::from_str(signature).context("invalid transaction signature")?;
    let transaction = rpc.get_transaction_with_config(
        &signature,
        RpcTransactionConfig {
            encoding: Some(UiTransactionEncoding::JsonParsed),
            commitment: Some(CommitmentConfig::confirmed()),
            max_supported_transaction_version: Some(0),
        },
    )?;
    let transaction =
        serde_json::to_value(transaction).context("serialize confirmed transaction")?;
    if transaction
        .pointer("/transaction/meta/err")
        .is_some_and(|error| !error.is_null())
    {
        bail!("transaction {signature} failed on chain");
    }
    Ok(transaction)
}

fn read_current_reserve_amount(
    rpc: &RpcClient,
    vault: &EarnVaultWatch,
    onboarding: &loyal_yield_store::EarnOnboardingContext,
    min_context_slot: u64,
) -> Result<(u64, u64)> {
    let vault_pubkey = Pubkey::from_str(&vault.vault)?;
    let market = Pubkey::from_str(
        onboarding
            .market
            .as_deref()
            .context("deposit onboarding has no Kamino market")?,
    )?;
    let reserve = Pubkey::from_str(&onboarding.target_reserve)?;
    let expected_mint = Pubkey::from_str(&onboarding.liquidity_mint)?;
    let obligation = derive_kamino_vanilla_obligation(vault_pubkey, market);
    let response = rpc.get_multiple_accounts_with_config(
        &[obligation, reserve],
        RpcAccountInfoConfig {
            encoding: Some(UiAccountEncoding::Base64),
            commitment: Some(CommitmentConfig::confirmed()),
            min_context_slot: Some(min_context_slot),
            ..RpcAccountInfoConfig::default()
        },
    )?;
    if response.context.slot < min_context_slot {
        bail!(
            "deposit proof context slot {} is below minimum {min_context_slot}",
            response.context.slot
        );
    }
    let obligation_account = response.value[0]
        .as_ref()
        .context("deposit obligation account is absent")?;
    let reserve_account = response.value[1]
        .as_ref()
        .context("deposit reserve account is absent")?;
    if obligation_account.owner != KLEND_PROGRAM_ID || reserve_account.owner != KLEND_PROGRAM_ID {
        bail!("deposit proof observed a non-Kamino obligation or reserve");
    }
    let obligation_state = from_account_data::<Obligation>(&obligation_account.data)
        .context("decode deposit obligation")?;
    if obligation_state.owner != vault_pubkey || obligation_state.lending_market != market {
        bail!("deposit obligation identity does not match the watched vault/market");
    }
    let collateral_amount = obligation_state
        .deposits
        .iter()
        .find(|deposit| deposit.deposit_reserve == reserve)
        .map(|deposit| deposit.deposited_amount)
        .unwrap_or_default();
    let reserve_state =
        from_account_data::<Reserve>(&reserve_account.data).context("decode deposit reserve")?;
    if reserve_state.lending_market != market
        || reserve_state.liquidity.mint_pubkey != expected_mint
    {
        bail!("deposit reserve identity does not match onboarding");
    }
    let redeemable = collateral_to_redeemable_liquidity(
        reserve_state.collateral.mint_total_supply,
        reserve_total_liquidity_scaled(&reserve_state)?,
        collateral_amount,
    )?;
    Ok((redeemable, response.context.slot))
}

fn reserve_total_liquidity_scaled(reserve: &Reserve) -> Result<BigUint> {
    let scale = BigUint::from(1_u128 << 60);
    let mut total = BigUint::from(reserve.liquidity.total_available_amount) * &scale;
    total += BigUint::from(u128::from(reserve.liquidity.borrowed_amount_sf));
    for (amount, label) in [
        (
            u128::from(reserve.liquidity.accumulated_protocol_fees_sf),
            "accumulated protocol fees",
        ),
        (
            u128::from(reserve.liquidity.accumulated_referrer_fees_sf),
            "accumulated referrer fees",
        ),
        (
            u128::from(reserve.liquidity.pending_referrer_fees_sf),
            "pending referrer fees",
        ),
    ] {
        let amount = BigUint::from(amount);
        if total < amount {
            bail!("reserve total liquidity underflow subtracting {label}");
        }
        total -= amount;
    }
    Ok(total)
}

fn collateral_to_redeemable_liquidity(
    collateral_total_supply: u64,
    total_liquidity_scaled: BigUint,
    collateral_amount: u64,
) -> Result<u64> {
    if collateral_amount == 0 {
        return Ok(0);
    }
    if collateral_total_supply == 0 || total_liquidity_scaled.is_zero() {
        return Ok(collateral_amount);
    }
    let scale = BigUint::from(1_u128 << 60);
    let numerator = BigUint::from(collateral_amount) * total_liquidity_scaled;
    let denominator = BigUint::from(collateral_total_supply) * scale;
    (numerator / denominator)
        .to_u64()
        .context("redeemable liquidity amount does not fit u64")
}

fn transaction_accounts(transaction: &Value) -> BTreeSet<String> {
    transaction
        .pointer("/transaction/transaction/message/accountKeys")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter_map(|key| {
            key.as_str()
                .or_else(|| key.get("pubkey").and_then(Value::as_str))
                .map(ToOwned::to_owned)
        })
        .collect()
}

fn require_transaction_slot(transaction: &Value, signature: &str, expected: u64) -> Result<()> {
    let actual = transaction
        .get("slot")
        .and_then(Value::as_u64)
        .context("confirmed transaction has no slot")?;
    if actual != expected {
        bail!("transaction {signature} landed at slot {actual}, expected {expected}");
    }
    Ok(())
}

fn transaction_owner_debit<'a>(
    transaction: &Value,
    mint: &str,
    owners: impl IntoIterator<Item = &'a String>,
) -> Result<u64> {
    let owners = owners
        .into_iter()
        .map(String::as_str)
        .collect::<BTreeSet<_>>();
    let balances = |name: &str| -> Result<BTreeMap<u64, (Option<String>, u64)>> {
        transaction
            .pointer(&format!("/transaction/meta/{name}"))
            .and_then(Value::as_array)
            .cloned()
            .unwrap_or_default()
            .into_iter()
            .filter(|row| row.get("mint").and_then(Value::as_str) == Some(mint))
            .try_fold(BTreeMap::new(), |mut parsed, row| {
                if row.get("mint").and_then(Value::as_str) != Some(mint) {
                    return Ok(parsed);
                }
                let index = row
                    .get("accountIndex")
                    .and_then(Value::as_u64)
                    .context("token balance has no account index")?;
                let amount = row
                    .pointer("/uiTokenAmount/amount")
                    .and_then(Value::as_str)
                    .context("token balance has no raw amount")?
                    .parse::<u64>()?;
                let owner = row
                    .get("owner")
                    .and_then(Value::as_str)
                    .map(ToOwned::to_owned);
                if parsed.insert(index, (owner, amount)).is_some() {
                    bail!("duplicate token balance account index {index} in {name}");
                }
                Ok(parsed)
            })
    };
    let pre = balances("preTokenBalances")?;
    let post = balances("postTokenBalances")?;
    let indexes = pre
        .keys()
        .chain(post.keys())
        .copied()
        .collect::<BTreeSet<_>>();
    let mut by_owner = BTreeMap::<String, (u64, u64)>::new();

    for index in indexes {
        let pre_row = pre.get(&index);
        let post_row = post.get(&index);
        if let Some((pre_owner, pre_amount)) = pre_row {
            if let Some(owner) = pre_owner
                .as_ref()
                .or_else(|| post_row.and_then(|(owner, _)| owner.as_ref()))
                .filter(|owner| owners.contains(owner.as_str()))
            {
                let total = by_owner.entry(owner.clone()).or_default();
                total.0 = total
                    .0
                    .checked_add(*pre_amount)
                    .context("deposit owner pre-balance overflow")?;
            }
        }
        if let Some((post_owner, post_amount)) = post_row {
            if let Some(owner) = post_owner
                .as_ref()
                .or_else(|| pre_row.and_then(|(owner, _)| owner.as_ref()))
                .filter(|owner| owners.contains(owner.as_str()))
            {
                let total = by_owner.entry(owner.clone()).or_default();
                total.1 = total
                    .1
                    .checked_add(*post_amount)
                    .context("deposit owner post-balance overflow")?;
            }
        }
    }

    by_owner.into_values().try_fold(0_u64, |sum, (pre, post)| {
        sum.checked_add(pre.saturating_sub(post))
            .context("deposit owner debit overflow")
    })
}

fn decode_token_account(account: &solana_sdk::account::Account) -> Result<(Pubkey, Pubkey, u64)> {
    if account.owner == spl_token::id() {
        let decoded = spl_token::state::Account::unpack(&account.data)?;
        return Ok((decoded.mint, decoded.owner, decoded.amount));
    }
    if account.owner == spl_token_2022::id() {
        let decoded = spl_token_2022::state::Account::unpack(&account.data)?;
        return Ok((decoded.mint, decoded.owner, decoded.amount));
    }
    bail!("account has unsupported token program {}", account.owner)
}

pub async fn reconcile_normalized_earn_update(
    store: &OrchestratorStore,
    consumer_name: &str,
    update: &NormalizedEarnUpdate,
    watch_set: &SubscriptionWatchSet,
    chain: &dyn EarnChainReader,
) -> Result<EarnDirectReconciliationOutcome> {
    let affected = watch_set.affected_vaults(update.account_pubkey.iter().map(String::as_str));
    if affected.is_empty() {
        bail!(
            "Earn LaserStream update at slot {} matched {:?} but no watched vault",
            update.slot,
            update.filters
        );
    }
    let mut mutations = Vec::with_capacity(affected.len());
    for vault in affected {
        mutations.push(chain.mutation_for(update, vault).await?);
    }
    store
        .apply_direct_earn_reconciliation(EarnDirectReconciliationInput {
            consumer_name: consumer_name.to_owned(),
            event_key: update.event_key.clone().unwrap_or_else(|| {
                format!(
                    "{}:{}:{}:{}",
                    update.event_kind,
                    update.slot,
                    update.signature.as_deref().unwrap_or("missing-signature"),
                    update
                        .account_pubkey
                        .as_deref()
                        .unwrap_or("missing-account")
                )
            }),
            durable_slot: update.slot,
            mutations,
        })
        .await
        .map_err(Into::into)
}

#[derive(Debug, Deserialize)]
pub struct FixtureEarnChainReader {
    signatures: BTreeMap<String, FixtureEvidence>,
}

impl FixtureEarnChainReader {
    pub fn from_path(path: impl AsRef<Path>) -> Result<Self> {
        serde_json::from_str(
            &fs::read_to_string(path.as_ref())
                .with_context(|| format!("read chain fixture {}", path.as_ref().display()))?,
        )
        .context("decode chain fixture")
    }
}

#[derive(Debug, Deserialize)]
struct FixtureEvidence {
    kind: String,
    slot: u64,
    #[serde(default)]
    amount_raw: Option<u64>,
    #[serde(default)]
    observed_amount_raw: Option<u64>,
    #[serde(default)]
    observed_slot: Option<u64>,
    #[serde(default)]
    deposit_mint: Option<String>,
    #[serde(default)]
    liquidity_mint: Option<String>,
    #[serde(default)]
    market: Option<String>,
    #[serde(default)]
    target_reserve: Option<String>,
    #[serde(default)]
    idle_token_account: Option<String>,
    #[serde(default)]
    route_policy: Option<FixturePolicy>,
    #[serde(default)]
    setup_policy: Option<FixturePolicy>,
    #[serde(default)]
    delegated_signer: Option<String>,
    #[serde(default)]
    withdrawal_signature: Option<String>,
    #[serde(default)]
    withdrawal_slot: Option<u64>,
    #[serde(default)]
    context_slot: Option<u64>,
    #[serde(default)]
    balances_zero: Option<bool>,
    #[serde(default)]
    policies_closed: Option<bool>,
}

#[derive(Debug, Deserialize)]
struct FixturePolicy {
    policy_account: String,
    policy_seed: u64,
    signature: String,
    confirmed_slot: u64,
}

impl EarnChainReader for FixtureEarnChainReader {
    fn mutation_for<'a>(
        &'a self,
        update: &'a NormalizedEarnUpdate,
        vault: &'a EarnVaultWatch,
    ) -> Pin<Box<dyn Future<Output = Result<EarnDirectMutation>> + Send + 'a>> {
        Box::pin(async move {
            let signature = update
                .signature
                .as_deref()
                .context("fixture Earn update is missing its transaction signature")?;
            let evidence = self
                .signatures
                .get(signature)
                .with_context(|| format!("no fixture chain evidence for {signature}"))?;
            if evidence.slot != update.slot {
                bail!(
                    "fixture evidence slot {} does not match update slot {}",
                    evidence.slot,
                    update.slot
                );
            }
            match evidence.kind.as_str() {
                "noop" => Ok(EarnDirectMutation::Noop),
                "policy_only" => {
                    let route = fixture_policy_match(
                        evidence
                            .route_policy
                            .as_ref()
                            .context("missing route policy")?,
                        vault,
                        evidence,
                        "kamino_deposit",
                    )?;
                    let setup = fixture_policy_match(
                        evidence
                            .setup_policy
                            .as_ref()
                            .context("missing setup policy")?,
                        vault,
                        evidence,
                        "kamino_setup",
                    )?;
                    Ok(EarnDirectMutation::PolicyOnly(EarnPolicyOnlyMutation {
                        route_policy: route,
                        setup_policy: setup,
                    }))
                }
                "deposit" => {
                    let route = fixture_policy_match(
                        evidence
                            .route_policy
                            .as_ref()
                            .context("missing route policy")?,
                        vault,
                        evidence,
                        "kamino_deposit",
                    )?;
                    let amount = evidence.amount_raw.context("missing deposit amount")?;
                    let observed_amount = evidence.observed_amount_raw.unwrap_or(amount);
                    let observed_slot = evidence.observed_slot.unwrap_or(evidence.slot);
                    let liquidity_mint = evidence
                        .liquidity_mint
                        .clone()
                        .context("missing liquidity mint")?;
                    let reserve = evidence
                        .target_reserve
                        .clone()
                        .context("missing target reserve")?;
                    Ok(EarnDirectMutation::Deposit(EarnDepositMutation {
                        route_policy: route,
                        setup_policy: None,
                        deposit_signature: signature.to_owned(),
                        deposit_slot: evidence.slot,
                        observed_slot,
                        deposit_mint: evidence
                            .deposit_mint
                            .clone()
                            .context("missing deposit mint")?,
                        principal_amount_raw: amount,
                        target_reserve: reserve.clone(),
                        market: evidence.market.clone(),
                        liquidity_mint: liquidity_mint.clone(),
                        target_supply_apy_bps: None,
                        wallet: vault.wallet.clone(),
                        smart_account_address: vault.vault.clone(),
                        reserve_state: vec![EarnReserveMutation {
                            reserve,
                            market: evidence.market.clone(),
                            liquidity_mint: liquidity_mint.clone(),
                            amount_raw: observed_amount,
                            has_value: observed_amount > 0,
                            supply_apy_bps: None,
                            borrow_apy_bps: None,
                            planning_metadata: json!({
                                "kind": "fixture_chain_proof",
                                "signature": signature,
                            }),
                        }],
                        idle_state: evidence
                            .idle_token_account
                            .as_ref()
                            .map(|token_account| EarnIdleTokenMutation {
                                mint: liquidity_mint,
                                amount_raw: 0,
                                owner: vault.vault.clone(),
                                token_account: token_account.clone(),
                                observed_slot,
                                observed_at: None,
                                source_commitment: "confirmed".to_owned(),
                            })
                            .into_iter()
                            .collect(),
                        observed_at: None,
                    }))
                }
                "cleanup" => {
                    let context_slot = evidence.context_slot.context("missing context slot")?;
                    if context_slot < update.slot {
                        bail!(
                            "cleanup proof context slot {context_slot} is below minimum {}",
                            update.slot
                        );
                    }
                    if !evidence.balances_zero.unwrap_or(false) {
                        return Ok(EarnDirectMutation::Noop);
                    }
                    let withdrawal_signature = evidence
                        .withdrawal_signature
                        .as_deref()
                        .context("missing withdrawal signature")?;
                    let cleanup_signature = if evidence.policies_closed.unwrap_or(false) {
                        signature
                    } else {
                        withdrawal_signature
                    };
                    Ok(EarnDirectMutation::Cleanup(EarnCleanupMutation {
                        settings: vault.settings.clone(),
                        vault_index: vault.vault_index,
                        vault_pubkey: vault.vault.clone(),
                        cleanup_signature: cleanup_signature.to_owned(),
                        confirmed_slot: if evidence.policies_closed.unwrap_or(false) {
                            evidence.slot
                        } else {
                            evidence
                                .withdrawal_slot
                                .context("missing withdrawal slot")?
                        },
                        observed_at: None,
                    }))
                }
                other => bail!("unsupported fixture evidence kind {other}"),
            }
        })
    }
}

fn fixture_policy_match(
    policy: &FixturePolicy,
    vault: &EarnVaultWatch,
    evidence: &FixtureEvidence,
    route_mode: &str,
) -> Result<PolicyMatchInput> {
    let delegated_signer = evidence
        .delegated_signer
        .clone()
        .unwrap_or_else(|| vault.wallet.clone());
    let liquidity_mint = evidence
        .liquidity_mint
        .clone()
        .context("missing policy liquidity mint")?;
    Ok(PolicyMatchInput {
        signature: policy.signature.clone(),
        slot: policy.confirmed_slot,
        cluster: vault.environment.clone(),
        source_commitment: "confirmed".to_owned(),
        settings: vault.settings.clone(),
        authority: vault.wallet.clone(),
        policy_seed: policy.policy_seed,
        policy_account: policy.policy_account.clone(),
        vault_index: vault.vault_index,
        vault_pubkey: vault.vault.clone(),
        delegated_signers: vec![delegated_signer],
        threshold: 1,
        route_modes: vec![route_mode.to_owned()],
        stable_mints: vec![liquidity_mint.clone()],
        kamino_markets: evidence.market.iter().cloned().collect(),
        kamino_liquidity_mints: vec![liquidity_mint],
        universe_preset: None,
        risk_profile: None,
        swap_lanes: Value::Array(Vec::new()),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn token_balance(index: u64, owner: &str, mint: &str, amount: u64) -> Value {
        json!({
            "accountIndex": index,
            "owner": owner,
            "mint": mint,
            "uiTokenAmount": { "amount": amount.to_string() }
        })
    }

    #[test]
    fn principal_debit_nets_same_owner_token_accounts() {
        let owner = "wallet-owner".to_owned();
        let mint = "deposit-mint";
        let transaction = json!({
            "transaction": {
                "meta": {
                    "preTokenBalances": [
                        token_balance(0, &owner, mint, 100),
                        token_balance(1, &owner, mint, 0)
                    ],
                    "postTokenBalances": [
                        token_balance(0, &owner, mint, 0),
                        token_balance(1, &owner, mint, 100)
                    ]
                }
            }
        });

        assert_eq!(
            transaction_owner_debit(&transaction, mint, [&owner]).unwrap(),
            0
        );
    }

    #[test]
    fn principal_debit_keeps_net_outflow_across_allowed_owners() {
        let wallet = "wallet-owner".to_owned();
        let vault = "vault-owner".to_owned();
        let mint = "deposit-mint";
        let transaction = json!({
            "transaction": {
                "meta": {
                    "preTokenBalances": [
                        token_balance(0, &wallet, mint, 200),
                        token_balance(1, &vault, mint, 50),
                        token_balance(2, &vault, mint, 0)
                    ],
                    "postTokenBalances": [
                        token_balance(0, &wallet, mint, 100),
                        token_balance(1, &vault, mint, 0),
                        token_balance(2, &vault, mint, 50)
                    ]
                }
            }
        });

        assert_eq!(
            transaction_owner_debit(&transaction, mint, [&wallet, &vault]).unwrap(),
            100
        );
    }
}
