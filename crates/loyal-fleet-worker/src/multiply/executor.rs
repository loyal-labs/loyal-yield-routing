use super::{
    builder::BuiltOperation,
    config::{EarnMaxTopology, MAINNET_GENESIS_HASH},
    observe::{position_balance, ObservedRoute},
    planner::ActionPlan,
};
use chrono::Utc;
use loyal_yield_store::{
    fleet_orchestration::{
        MultiplyOperation, MultiplyPosition, MultiplyRouteState, RouteGoal, WithdrawalStatus,
    },
    MultiplyRouteLease, NeonSqlClient, OrchestratorError, SignedOperation,
};
use sha2::{Digest, Sha256};
use solana_client::{
    nonblocking::rpc_client::RpcClient,
    rpc_config::{RpcSendTransactionConfig, RpcSimulateTransactionConfig},
};
use solana_sdk::{
    address_lookup_table::{state::AddressLookupTable, AddressLookupTableAccount},
    commitment_config::CommitmentConfig,
    compute_budget::ComputeBudgetInstruction,
    message::{v0, Message, VersionedMessage},
    pubkey::Pubkey,
    signature::{Keypair, Signature, Signer},
    transaction::VersionedTransaction,
};
use std::{error::Error, str::FromStr, time::Duration};

const MAX_TRANSACTION_FEE_LAMPORTS: u64 = 20_000;

pub struct ExecutionContext<'a> {
    pub store: &'a NeonSqlClient,
    pub rpc: &'a RpcClient,
    pub fee_payer: &'a Keypair,
    pub delegate: &'a Keypair,
}

#[derive(Clone, Debug)]
pub struct PolicyEvidence {
    pub account: Pubkey,
    pub data_sha256: String,
    pub constraint_indexes: Vec<u8>,
}

pub async fn ensure_exact_policy(
    rpc: &RpcClient,
    authority: Pubkey,
    delegate: Pubkey,
    plan: &ActionPlan,
    built: &BuiltOperation,
    topology: EarnMaxTopology,
) -> Result<PolicyEvidence, Box<dyn Error>> {
    let policy = topology
        .strategy(
            plan.strategy_key
                .ok_or("delegate operation has no strategy")?,
        )
        .policy(plan.action)
        .ok_or("strategy has no policy for action")?;
    let account = policy.account;
    let response = rpc
        .get_account_with_commitment(&account, CommitmentConfig::confirmed())
        .await?;
    let value = response
        .value
        .ok_or("exact ProgramInteraction policy is absent")?;
    if !value.executable && value.data.is_empty() {
        return Err("policy account is empty".into());
    }
    let constraint_indexes = if let Some(key) = plan.strategy_key {
        let config = topology.strategy(key);
        let family = super::policy::family_for_action(plan.action)?;
        let update = super::policy::canonical_policy_update(
            topology,
            config,
            family,
            topology.settings,
            authority,
            delegate,
        )?;
        let expected = super::policy::canonical_policy_payload(&update)?;
        if !super::policy::current_policy_matches(&value.data, policy, delegate, &expected)? {
            return Err(format!(
                "stable production policy {} is not installed",
                policy.account
            )
            .into());
        }
        super::policy::constraint_indexes(config, plan.action, &built.instructions)?
    } else {
        let pins = built
            .instructions
            .iter()
            .map(|instruction| {
                (0..instruction.accounts.len())
                    .map(u8::try_from)
                    .collect::<Result<Vec<_>, _>>()
            })
            .collect::<Result<Vec<_>, _>>()?;
        let update = loyal_actions::update_exact_program_interaction_policy_instruction(
            topology.settings,
            authority,
            account,
            delegate,
            0,
            &built.instructions,
            &pins,
        )?;
        let actions = loyal_actions::decode_squads_policy_create_actions(&update)?;
        let [expected] = actions.as_slice() else {
            return Err("exact claim policy update did not decode once".into());
        };
        let current = loyal_actions::decode_program_interaction_policy_account(&value.data)?
            .ok_or("claim policy is not a canonical hookless ProgramInteraction policy")?;
        if current.policy_seed != policy.seed
            || current.policy_account != account
            || current.delegated_signer != delegate
            || current.threshold != 1
            || current.payload != expected.payload
        {
            return Err("claim destination is not bound by the installed exact policy".into());
        }
        (0..built.instructions.len())
            .map(u8::try_from)
            .collect::<Result<Vec<_>, _>>()?
    };
    Ok(PolicyEvidence {
        account,
        data_sha256: hex_hash(&value.data),
        constraint_indexes,
    })
}

pub async fn execute_operation(
    context: &ExecutionContext<'_>,
    lease: &mut MultiplyRouteLease,
    route: &MultiplyRouteState,
    operation: &MultiplyOperation,
    plan: &ActionPlan,
    built: BuiltOperation,
    before: &ObservedRoute,
    topology: EarnMaxTopology,
) -> Result<MultiplyRouteState, Box<dyn Error>> {
    verify_mainnet(context.rpc).await?;
    let policy = ensure_exact_policy(
        context.rpc,
        context.fee_payer.pubkey(),
        context.delegate.pubkey(),
        plan,
        &built,
        topology,
    )
    .await?;
    let (transaction, last_valid_block_height) =
        build_transaction(context, &policy, &built).await?;
    let wire = bincode::serialize(&transaction)?;
    if wire.len() > 1_232 {
        return Err(format!("operation packet is {} bytes", wire.len()).into());
    }
    simulate(context.rpc, &transaction, before.slot).await?;
    let signature = transaction
        .signatures
        .first()
        .ok_or("signed transaction has no signature")?
        .to_string();
    let blockhash = transaction.message.recent_blockhash().to_string();
    let signed = SignedOperation::new(wire, signature.clone(), blockhash, last_valid_block_height)?;
    persist_signed_operation(
        context.store,
        lease,
        operation,
        &policy,
        &transaction,
        &signed,
    )
    .await?;
    if !context
        .store
        .mark_multiply_broadcast_intent(lease, &operation.operation_id, Utc::now())
        .await?
    {
        return Err("lost operation before broadcast intent".into());
    }
    context
        .rpc
        .send_transaction_with_config(
            &transaction,
            RpcSendTransactionConfig {
                skip_preflight: true,
                preflight_commitment: Some(CommitmentConfig::confirmed().commitment),
                max_retries: Some(0),
                min_context_slot: Some(before.slot),
                encoding: None,
            },
        )
        .await?;
    let confirmed_slot = wait_confirmed(context.rpc, &Signature::from_str(&signature)?).await?;
    if !context
        .store
        .mark_multiply_confirmed(lease, &operation.operation_id, confirmed_slot)
        .await?
    {
        return Err("lost operation before confirmed persistence".into());
    }
    let persisted = context
        .store
        .load_multiply_operation(&operation.operation_id)
        .await?
        .ok_or("confirmed operation disappeared")?;
    reconcile_operation(context, lease, route, &persisted, confirmed_slot, topology).await
}

pub async fn persist_signed_operation(
    store: &NeonSqlClient,
    lease: &MultiplyRouteLease,
    operation: &MultiplyOperation,
    policy: &PolicyEvidence,
    transaction: &VersionedTransaction,
    signed: &SignedOperation,
) -> Result<(), OrchestratorError> {
    let message_sha256 = hex_hash(&transaction.message.serialize());
    if store
        .persist_signed_operation(
            lease,
            &operation.operation_id,
            &policy.account.to_string(),
            &policy.data_sha256,
            &message_sha256,
            signed,
        )
        .await?
    {
        Ok(())
    } else {
        Err(OrchestratorError::StoreInvariant(
            "lost prepared operation before signed-byte persistence".to_owned(),
        ))
    }
}

pub async fn reconcile_operation(
    context: &ExecutionContext<'_>,
    lease: &mut MultiplyRouteLease,
    route: &MultiplyRouteState,
    operation: &MultiplyOperation,
    confirmed_slot: u64,
    topology: EarnMaxTopology,
) -> Result<MultiplyRouteState, Box<dyn Error>> {
    let extra = operation
        .expected_effects
        .token_deltas
        .iter()
        .filter(|delta| {
            ![
                topology.claim_custody,
                topology.collateral_custody,
                topology.strategy.debt_custody,
            ]
            .iter()
            .any(|account| account.to_string() == delta.account)
        })
        .map(|delta| {
            (
                delta.account.as_str(),
                delta.mint.as_str(),
                super::config::TOKEN,
            )
        })
        .collect::<Vec<_>>();
    let after = super::observe::observe_confirmed_with_extra(context.rpc, topology, &extra).await?;
    if after.slot < confirmed_slot {
        return Err("confirmed reconciliation read is older than the transaction".into());
    }
    let policy_account = Pubkey::from_str(
        operation
            .policy_account
            .as_deref()
            .ok_or("operation omitted its policy account")?,
    )?;
    let policy = context
        .rpc
        .get_account_with_commitment(&policy_account, CommitmentConfig::confirmed())
        .await?;
    if policy.context.slot < confirmed_slot
        || policy
            .value
            .as_ref()
            .map(|account| hex_hash(&account.data))
            .as_deref()
            != operation.policy_data_sha256.as_deref()
    {
        return Err("confirmed policy account drifted from the persisted binding".into());
    }
    verify_expected_effects(operation, &after)?;
    let active = after
        .strategies
        .iter()
        .find(|value| value.collateral_deposited_raw > 0 || value.debt_raw > 0)
        .map(|value| value.strategy_key);
    let position = active.map_or_else(
        || MultiplyPosition::Idle {
            claim: after.claim.clone(),
        },
        |key| position_balance(&after, key, topology),
    );
    let mut next = route.clone();
    next.generation += 1;
    next.position = position;
    next.current_operation_id = None;
    next.observed_slot = after.slot;
    next.observed_at = Utc::now();
    if operation.action == loyal_yield_store::fleet_orchestration::MultiplyAction::Claim {
        if let Some(withdrawal) = &mut next.withdrawal {
            withdrawal.status = WithdrawalStatus::Claimed;
            withdrawal.claim_signature = operation.transaction_signature.clone();
        }
        next.goal = if after.claim.amount_raw > 0 {
            RouteGoal::Deploy
        } else {
            RouteGoal::Claimed
        };
    } else if next.goal == RouteGoal::Withdraw
        && active.is_none()
        && after.collateral_custody.amount_raw == 0
    {
        if let Some(withdrawal) = &mut next.withdrawal {
            withdrawal.status = WithdrawalStatus::Claimable;
            withdrawal.unwind_completed_at = Some(Utc::now());
        }
    }
    let reconciliation_sha256 = hex_hash(&serde_json::to_vec(&serde_json::json!({
        "operationId": operation.operation_id,
        "signature": operation.transaction_signature,
        "confirmedSlot": confirmed_slot,
        "observationSlot": after.slot,
        "position": next.position,
    }))?);
    let signature = operation
        .transaction_signature
        .as_deref()
        .ok_or("operation signature was not persisted")?;
    if !context
        .store
        .reconcile_multiply_operation(
            lease,
            &operation.operation_id,
            signature,
            &reconciliation_sha256,
            confirmed_slot,
            &next,
        )
        .await?
    {
        return Err("lost confirmed operation during reconciliation".into());
    }
    Ok(next)
}

async fn build_transaction(
    context: &ExecutionContext<'_>,
    policy: &PolicyEvidence,
    built: &BuiltOperation,
) -> Result<(VersionedTransaction, u64), Box<dyn Error>> {
    let mut table = Vec::new();
    let compiled = built
        .instructions
        .iter()
        .cloned()
        .map(|instruction| loyal_actions::compile_squads_inner_instruction(&mut table, instruction))
        .collect::<Vec<_>>();
    let execute = loyal_actions::execute_program_interaction_policy_instruction(
        policy.account,
        context.delegate.pubkey(),
        0,
        compiled,
        policy.constraint_indexes.clone(),
        table,
    );
    let (blockhash, last_valid_block_height) = context
        .rpc
        .get_latest_blockhash_with_commitment(CommitmentConfig::confirmed())
        .await?;
    let instructions = [
        ComputeBudgetInstruction::set_compute_unit_limit(400_000),
        execute,
    ];
    let message = if built.lookup_tables.is_empty() {
        VersionedMessage::Legacy(Message::new_with_blockhash(
            &instructions,
            Some(&context.fee_payer.pubkey()),
            &blockhash,
        ))
    } else {
        let tables = load_lookup_tables(context.rpc, &built.lookup_tables).await?;
        VersionedMessage::V0(v0::Message::try_compile(
            &context.fee_payer.pubkey(),
            &instructions,
            &tables,
            blockhash,
        )?)
    };
    let fee_lamports = match &message {
        VersionedMessage::Legacy(message) => context.rpc.get_fee_for_message(message).await?,
        VersionedMessage::V0(message) => context.rpc.get_fee_for_message(message).await?,
    };
    if fee_lamports > MAX_TRANSACTION_FEE_LAMPORTS {
        return Err(format!(
            "transaction fee {fee_lamports} exceeds the {MAX_TRANSACTION_FEE_LAMPORTS} lamport worker cap"
        )
        .into());
    }
    let transaction = if context.fee_payer.pubkey() == context.delegate.pubkey() {
        VersionedTransaction::try_new(message, &[context.fee_payer])?
    } else {
        VersionedTransaction::try_new(message, &[context.fee_payer, context.delegate])?
    };
    Ok((transaction, last_valid_block_height))
}

async fn load_lookup_tables(
    rpc: &RpcClient,
    keys: &[Pubkey],
) -> Result<Vec<AddressLookupTableAccount>, Box<dyn Error>> {
    let response = rpc
        .get_multiple_accounts_with_commitment(keys, CommitmentConfig::confirmed())
        .await?;
    keys.iter()
        .zip(response.value)
        .map(|(key, account)| {
            let account = account.ok_or("Jupiter lookup table is absent")?;
            let table = AddressLookupTable::deserialize(&account.data)?;
            Ok(AddressLookupTableAccount {
                key: *key,
                addresses: table.addresses.to_vec(),
            })
        })
        .collect()
}

async fn simulate(
    rpc: &RpcClient,
    transaction: &VersionedTransaction,
    minimum_slot: u64,
) -> Result<(), Box<dyn Error>> {
    let result = rpc
        .simulate_transaction_with_config(
            transaction,
            RpcSimulateTransactionConfig {
                sig_verify: true,
                replace_recent_blockhash: false,
                commitment: Some(CommitmentConfig::confirmed()),
                min_context_slot: Some(minimum_slot),
                encoding: None,
                ..RpcSimulateTransactionConfig::default()
            },
        )
        .await?;
    if let Some(error) = result.value.err {
        return Err(format!(
            "signed simulation failed: {error:?}; logs={:?}",
            result.value.logs
        )
        .into());
    }
    Ok(())
}

pub async fn wait_confirmed(rpc: &RpcClient, signature: &Signature) -> Result<u64, Box<dyn Error>> {
    for _ in 0..60 {
        let status = rpc.get_signature_statuses(&[*signature]).await?;
        if let Some(value) = status.value.into_iter().next().flatten() {
            if let Some(error) = value.err {
                return Err(format!("transaction failed: {error:?}").into());
            }
            if value.confirmation_status.is_some_and(|level| matches!(level,
                solana_transaction_status_client_types::TransactionConfirmationStatus::Confirmed
                    | solana_transaction_status_client_types::TransactionConfirmationStatus::Finalized
            )) {
                let transaction = rpc.get_transaction_with_config(signature, solana_client::rpc_config::RpcTransactionConfig {
                    encoding: None, commitment: Some(CommitmentConfig::confirmed()), max_supported_transaction_version: Some(0),
                }).await?;
                return Ok(transaction.slot);
            }
        }
        tokio::time::sleep(Duration::from_millis(500)).await;
    }
    Err("signature confirmation remained unknown after 30 seconds; do not resend".into())
}

fn verify_expected_effects(
    operation: &MultiplyOperation,
    after: &ObservedRoute,
) -> Result<(), Box<dyn Error>> {
    for delta in &operation.expected_effects.token_deltas {
        let before = operation
            .expected_effects
            .token_amounts_before
            .iter()
            .find(|value| value.account == delta.account && value.mint == delta.mint)
            .ok_or("operation omitted its pre-transaction token amount")?;
        let post = observed_token_amount(after, &delta.account, &delta.mint)
            .ok_or("post-state omitted an expected custody account")?;
        if delta.raw_delta < 0 {
            let spent = before
                .amount_raw
                .checked_sub(post)
                .ok_or("source custody increased")?;
            let expected = delta.raw_delta.unsigned_abs();
            let action = operation.action;
            let valid = match action {
                loyal_yield_store::fleet_orchestration::MultiplyAction::SwapCollateralToDebt => {
                    spent > 0 && spent <= expected
                }
                loyal_yield_store::fleet_orchestration::MultiplyAction::RepayDebt => {
                    let full_repay = operation
                        .expected_effects
                        .obligation_before
                        .as_ref()
                        .is_some_and(|obligation| expected >= obligation.debt_raw);
                    if full_repay {
                        spent > 0 && spent <= before.amount_raw
                    } else {
                        spent == expected
                    }
                }
                _ => spent == expected,
            };
            if !valid {
                return Err(
                    "confirmed source custody delta violated its exact input or maximum".into(),
                );
            }
        } else if delta.raw_delta > 0 {
            let received = post
                .checked_sub(before.amount_raw)
                .ok_or("destination custody decreased")?;
            if received < delta.raw_delta as u64 {
                return Err("confirmed destination custody delta missed its minimum".into());
            }
        }
    }
    if let (Some(before), Some(delta), Some(key)) = (
        operation.expected_effects.obligation_before.as_ref(),
        operation.expected_effects.obligation_delta.as_ref(),
        operation.strategy_key,
    ) {
        if before.obligation != delta.obligation {
            return Err("obligation pre-state and delta identities differ".into());
        }
        let post = after.position(key);
        let direction_ok = (delta.collateral_raw_delta == 0
            || (delta.collateral_raw_delta > 0
                && post.collateral_deposited_raw > before.collateral_raw)
            || (delta.collateral_raw_delta < 0
                && post.collateral_deposited_raw < before.collateral_raw))
            && (delta.debt_raw_delta == 0
                || (delta.debt_raw_delta > 0 && post.debt_raw > before.debt_raw)
                || (delta.debt_raw_delta < 0 && post.debt_raw < before.debt_raw));
        if !direction_ok {
            return Err("confirmed obligation movement had the wrong direction".into());
        }
        if operation.action == loyal_yield_store::fleet_orchestration::MultiplyAction::RepayDebt
            && delta.debt_raw_delta.unsigned_abs() >= before.debt_raw
            && post.debt_raw != 0
        {
            return Err("repay-all left confirmed obligation debt".into());
        }
    }
    Ok(())
}

fn observed_token_amount(after: &ObservedRoute, account: &str, mint: &str) -> Option<u64> {
    [&after.claim, &after.collateral_custody, &after.debt_custody]
        .into_iter()
        .chain(after.external_custody.iter())
        .find(|value| value.account == account && value.mint == mint)
        .map(|value| value.amount_raw)
}

async fn verify_mainnet(rpc: &RpcClient) -> Result<(), Box<dyn Error>> {
    if rpc.get_genesis_hash().await?.to_string() != MAINNET_GENESIS_HASH {
        Err("RPC is not Solana mainnet-beta".into())
    } else {
        Ok(())
    }
}

fn hex_hash(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}
