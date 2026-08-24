use std::{
    collections::{BTreeMap, BTreeSet},
    fs,
    future::Future,
    path::Path,
    pin::Pin,
    str::FromStr,
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc,
    },
    time::Duration,
};

use anyhow::{bail, Context, Result};
use chrono::{Duration as ChronoDuration, Utc};
use helius_laserstream::grpc::SubscribeUpdateTransactionInfo;
use klend_interface::{
    from_account_data,
    state::{Obligation, Reserve},
    KLEND_PROGRAM_ID,
};
use loyal_actions::{
    derive_associated_token_account, derive_squads_vault, earn_stablecoin, earn_stablecoins,
    SQUADS_SMART_ACCOUNT_PROGRAM_ID, SUBSCRIPTIONS_CREATE_RECURRING_DELEGATION,
    SUBSCRIPTIONS_INIT_AUTHORITY, SUBSCRIPTIONS_PROGRAM_ID, USDC_MINT,
};
use loyal_squads_policy_monitor::{PolicyMonitor, PostgresPolicyMatchSink};
use loyal_yield_store::{
    fleet_orchestration::{
        DepositEvidence, ExpectedEffects, MultiplyAction, MultiplyOperation,
        MultiplyOperationStatus, MultiplyPosition, RouteGoal, TokenAmountBefore, TokenBalance,
        TokenDelta, WithdrawalStatus, MULTIPLY_ENGINE_VERSION,
    },
    AutodepositChainObservation, AutodepositRecurringDelegationObserved,
    AutodepositTargetSnapshotContext, EarnCleanupMutation, EarnDepositMutation, EarnDirectMutation,
    EarnIdleTokenMutation, EarnMaxIntent, EarnMaxIntentProjectionInput, EarnPolicyOnlyMutation,
    EarnReconciliationEnqueueInput, EarnReconciliationEnqueueOutcome, EarnReconciliationVaultInput,
    EarnRefundMutation, EarnReserveMutation, EarnWithdrawalMutation, OrchestratorStore,
    PolicyMatchInput,
};
use num_bigint::BigUint;
use num_traits::{ToPrimitive, Zero};
use serde::Deserialize;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_account_decoder::UiAccountEncoding;
use solana_client::{
    rpc_client::RpcClient,
    rpc_config::{RpcAccountInfoConfig, RpcTokenAccountsFilter, RpcTransactionConfig},
    rpc_request::RpcRequest,
    rpc_response::{Response as RpcResponse, RpcKeyedAccount},
};
use solana_program::program_pack::Pack;
use solana_sdk::{
    commitment_config::CommitmentConfig,
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
    signature::Signature,
    transaction::TransactionError,
    transaction::VersionedTransaction,
};
use solana_transaction_status_client_types::{
    option_serializer::OptionSerializer, UiInstruction, UiParsedInstruction, UiTransactionEncoding,
    UiTransactionStatusMeta, UiTransactionTokenBalance,
};
use tokio::{
    sync::{Mutex, Notify},
    time,
};

use crate::{
    emit_earn_reconciliation_consumer_failed, emit_earn_reconciliation_health_snapshot_failed,
    emit_earn_reconciliation_job_failed,
    monitor_observability::EarnMonitorMetrics,
    smart_account::{
        EarnVaultWatch, NormalizedEarnUpdate, SubscriptionWatchSet, EARN_POLICY_ACCOUNTS,
        EARN_SMART_ACCOUNTS, EARN_WALLETS,
    },
};

const EARN_RECONCILIATION_HEALTH_SAMPLE_INTERVAL: Duration = Duration::from_secs(60);
const EARN_MAX_MEMO_PROGRAM: &str = "MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr";

#[derive(Debug, Clone)]
pub struct EarnPolicyTransaction {
    pub signature: String,
    pub slot: u64,
    pub signers: Vec<Pubkey>,
    pub instructions: Vec<Instruction>,
    pub earn_max_memos: Vec<EarnMaxMemoInstruction>,
}

#[derive(Debug, Clone)]
pub struct EarnMaxMemoInstruction {
    pub source_instruction_index: u16,
    pub accounts: Vec<Pubkey>,
    pub data: Vec<u8>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
enum EarnMaxCashFlowMemo {
    Deposit {
        settings: Pubkey,
        amount_raw: u64,
    },
    Claim {
        settings: Pubkey,
        request_id: String,
        amount_raw: u64,
        destination: Pubkey,
    },
}

struct ConfirmedCashFlowTransfer {
    signature: Signature,
    slot: u64,
    source: Pubkey,
    destination: Pubkey,
    source_pre: u64,
    source_post: u64,
    destination_pre: u64,
    destination_post: u64,
}

#[derive(Debug, Clone)]
pub enum EarnPolicyTransactionRead {
    NoStateChange,
    Transaction(EarnPolicyTransaction),
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum PolicyTransactionDisposition {
    Decode,
    NoStateChange,
}

fn policy_transaction_disposition(
    error: Option<&TransactionError>,
) -> PolicyTransactionDisposition {
    if error.is_some() {
        PolicyTransactionDisposition::NoStateChange
    } else {
        PolicyTransactionDisposition::Decode
    }
}

pub trait EarnChainReader: Send + Sync {
    fn mutation_for<'a>(
        &'a self,
        update: &'a NormalizedEarnUpdate,
        vault: &'a EarnVaultWatch,
    ) -> Pin<Box<dyn Future<Output = Result<EarnDirectMutation>> + Send + 'a>>;

    fn policy_transaction_for<'a>(
        &'a self,
        _update: &'a NormalizedEarnUpdate,
    ) -> Pin<Box<dyn Future<Output = Result<Option<EarnPolicyTransactionRead>>> + Send + 'a>> {
        Box::pin(async { Ok(None) })
    }
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
                CommitmentConfig::finalized(),
            )),
            store,
        }
    }

    async fn autodeposit_snapshot(
        &self,
        target: AutodepositTargetSnapshotContext,
        minimum_slot: u64,
    ) -> Result<AutodepositChainObservation> {
        let rpc = Arc::clone(&self.rpc);
        tokio::task::spawn_blocking(move || {
            read_autodeposit_snapshot(rpc.as_ref(), &target, minimum_slot)
        })
        .await
        .context("Autodeposit RPC snapshot task panicked")?
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

    fn policy_transaction_for<'a>(
        &'a self,
        update: &'a NormalizedEarnUpdate,
    ) -> Pin<Box<dyn Future<Output = Result<Option<EarnPolicyTransactionRead>>> + Send + 'a>> {
        Box::pin(async move {
            let Some(signature) = update.signature.clone() else {
                return Ok(None);
            };
            let rpc = Arc::clone(&self.rpc);
            let slot = update.slot;
            tokio::task::spawn_blocking(move || {
                read_squads_policy_transaction(
                    rpc.as_ref(),
                    &signature,
                    slot,
                    CommitmentConfig::confirmed(),
                )
                .map(Some)
            })
            .await
            .context("Autoswap RPC transaction proof task panicked")?
        })
    }
}

pub async fn read_confirmed_squads_policy_transaction(
    rpc: Arc<RpcClient>,
    signature: String,
    expected_slot: u64,
) -> Result<EarnPolicyTransactionRead> {
    tokio::task::spawn_blocking(move || {
        read_squads_policy_transaction(
            rpc.as_ref(),
            &signature,
            expected_slot,
            CommitmentConfig::confirmed(),
        )
    })
    .await
    .context("confirmed Squads policy transaction proof task panicked")?
}

pub(crate) fn decode_laserstream_squads_policy_transaction(
    transaction: SubscribeUpdateTransactionInfo,
    slot: u64,
) -> Result<EarnPolicyTransactionRead> {
    let signature = Signature::try_from(transaction.signature.as_slice())
        .context("LaserStream policy transaction signature")?
        .to_string();
    let meta = transaction
        .meta
        .context("LaserStream policy transaction metadata was missing")?;
    if meta.err.is_some() {
        return Ok(EarnPolicyTransactionRead::NoStateChange);
    }
    let message = transaction
        .transaction
        .and_then(|transaction| transaction.message)
        .context("LaserStream policy transaction message was missing")?;
    let header = message
        .header
        .context("LaserStream policy transaction header was missing")?;
    let static_len = message.account_keys.len();
    let mut account_keys = message
        .account_keys
        .iter()
        .map(|bytes| Pubkey::try_from(bytes.as_slice()).context("LaserStream account key"))
        .collect::<Result<Vec<_>>>()?;
    let loaded_writable_len = meta.loaded_writable_addresses.len();
    account_keys.extend(
        meta.loaded_writable_addresses
            .iter()
            .chain(&meta.loaded_readonly_addresses)
            .map(|bytes| Pubkey::try_from(bytes.as_slice()).context("LaserStream loaded address"))
            .collect::<Result<Vec<_>>>()?,
    );
    let required_signers =
        usize::try_from(header.num_required_signatures).context("LaserStream signer count")?;
    let readonly_signers = usize::try_from(header.num_readonly_signed_accounts)
        .context("LaserStream readonly signer count")?;
    let readonly_unsigned = usize::try_from(header.num_readonly_unsigned_accounts)
        .context("LaserStream readonly unsigned count")?;

    let account_meta = |index: usize| -> Option<AccountMeta> {
        let pubkey = account_keys.get(index).copied()?;
        let is_signer = index < required_signers;
        let is_writable = if is_signer {
            index < required_signers.saturating_sub(readonly_signers)
        } else if index < static_len {
            index < static_len.saturating_sub(readonly_unsigned)
        } else {
            index < static_len.saturating_add(loaded_writable_len)
        };
        Some(AccountMeta {
            pubkey,
            is_signer,
            is_writable,
        })
    };

    let mut instructions = Vec::new();
    let mut earn_max_memos = Vec::new();
    for (outer_index, compiled) in message.instructions.into_iter().enumerate() {
        let Some(program_id) = usize::try_from(compiled.program_id_index)
            .ok()
            .and_then(|index| account_keys.get(index))
            .copied()
        else {
            continue;
        };
        if program_id.to_string() == EARN_MAX_MEMO_PROGRAM {
            earn_max_memos.push(EarnMaxMemoInstruction {
                source_instruction_index: u16::try_from(outer_index)
                    .ok()
                    .and_then(|index| index.checked_mul(256))
                    .context("Earn MAX outer memo instruction index overflow")?,
                accounts: compiled
                    .accounts
                    .iter()
                    .filter_map(|index| account_keys.get(usize::from(*index)).copied())
                    .collect(),
                data: compiled.data,
            });
            continue;
        }
        if program_id != SQUADS_SMART_ACCOUNT_PROGRAM_ID && program_id != SUBSCRIPTIONS_PROGRAM_ID {
            continue;
        }
        instructions.push(Instruction {
            program_id,
            accounts: compiled
                .accounts
                .iter()
                .filter_map(|index| account_meta(usize::from(*index)))
                .collect(),
            data: compiled.data,
        });
    }

    for group in meta.inner_instructions {
        for (inner_index, instruction) in group.instructions.into_iter().enumerate() {
            let Some(program_id) = usize::try_from(instruction.program_id_index)
                .ok()
                .and_then(|index| account_keys.get(index))
                .copied()
            else {
                continue;
            };
            let accounts = instruction
                .accounts
                .iter()
                .filter_map(|index| account_meta(usize::from(*index)))
                .collect::<Vec<_>>();
            if program_id == SQUADS_SMART_ACCOUNT_PROGRAM_ID
                || program_id == SUBSCRIPTIONS_PROGRAM_ID
            {
                instructions.push(Instruction {
                    program_id,
                    accounts: accounts.clone(),
                    data: instruction.data.clone(),
                });
            }
            if program_id.to_string() != EARN_MAX_MEMO_PROGRAM {
                continue;
            }
            let source_instruction_index = u16::try_from(group.index)
                .ok()
                .and_then(|index| index.checked_mul(256))
                .and_then(|value| value.checked_add(u16::try_from(inner_index + 1).ok()?))
                .context("Earn MAX memo instruction index overflow")?;
            earn_max_memos.push(EarnMaxMemoInstruction {
                source_instruction_index,
                accounts: accounts.into_iter().map(|account| account.pubkey).collect(),
                data: instruction.data,
            });
        }
    }
    Ok(EarnPolicyTransactionRead::Transaction(
        EarnPolicyTransaction {
            signature,
            slot,
            signers: account_keys
                .iter()
                .take(required_signers)
                .copied()
                .collect(),
            instructions,
            earn_max_memos,
        },
    ))
}

pub(crate) async fn project_earn_max_memos(
    store: &OrchestratorStore,
    transaction: &EarnPolicyTransaction,
) -> Result<usize> {
    let mut applied = 0;
    for memo in &transaction.earn_max_memos {
        let Some(intent) = parse_earn_max_intent(&memo.data)? else {
            continue;
        };
        let matches = memo
            .accounts
            .iter()
            .flat_map(|vault| {
                transaction
                    .instructions
                    .iter()
                    .flat_map(move |instruction| {
                        instruction.accounts.iter().filter_map(move |account| {
                            (derive_squads_vault(&account.pubkey, 0).0 == *vault)
                                .then_some((account.pubkey, *vault))
                        })
                    })
            })
            .collect::<BTreeSet<_>>();
        let matches = matches.into_iter().collect::<Vec<_>>();
        let [(settings, _vault)] = matches.as_slice() else {
            continue;
        };
        store
            .project_earn_max_intent(EarnMaxIntentProjectionInput {
                settings: settings.to_string(),
                vault_index: 0,
                signature: transaction.signature.clone(),
                instruction_index: memo.source_instruction_index,
                slot: transaction.slot,
                observed_at: Utc::now(),
                intent,
            })
            .await?;
        applied += 1;
    }
    Ok(applied)
}

pub(crate) async fn project_earn_max_cash_flows(
    store: &OrchestratorStore,
    rpc: Arc<RpcClient>,
    transaction: &EarnPolicyTransaction,
) -> Result<usize> {
    let mut applied = 0;
    for memo in &transaction.earn_max_memos {
        let Some(cash_flow) = parse_earn_max_cash_flow(&memo.data)? else {
            continue;
        };
        let (source, destination) = match &cash_flow {
            EarnMaxCashFlowMemo::Deposit { settings, .. } => {
                let [wallet, memo_settings, custody, source, routing_program] =
                    memo.accounts.as_slice()
                else {
                    bail!("Earn MAX deposit memo account shape drifted");
                };
                if memo_settings != settings
                    || *routing_program != SQUADS_SMART_ACCOUNT_PROGRAM_ID
                    || !transaction.signers.contains(wallet)
                {
                    bail!("Earn MAX deposit memo is not transaction-signer bound");
                }
                let expected_custody = derive_associated_token_account(
                    derive_squads_vault(settings, 0).0,
                    USDC_MINT,
                    spl_token::ID,
                );
                if *custody != expected_custody {
                    bail!("Earn MAX deposit memo custody drifted");
                }
                (*source, *custody)
            }
            EarnMaxCashFlowMemo::Claim {
                settings,
                destination,
                ..
            } => {
                let [vault, memo_settings, custody, memo_destination] = memo.accounts.as_slice()
                else {
                    bail!("Earn MAX claim memo account shape drifted");
                };
                let expected_vault = derive_squads_vault(settings, 0).0;
                let expected_custody =
                    derive_associated_token_account(expected_vault, USDC_MINT, spl_token::ID);
                if memo_settings != settings
                    || *vault != expected_vault
                    || *custody != expected_custody
                    || memo_destination != destination
                    || transaction.instructions.is_empty()
                {
                    bail!("Earn MAX claim memo topology drifted");
                }
                (*custody, *destination)
            }
        };
        let transfer = read_confirmed_earn_max_transfer(
            Arc::clone(&rpc),
            transaction.signature.clone(),
            transaction.slot,
            source,
            destination,
        )
        .await?;
        let amount = transfer.source_pre.saturating_sub(transfer.source_post);
        let expected_amount = match &cash_flow {
            EarnMaxCashFlowMemo::Deposit { amount_raw, .. }
            | EarnMaxCashFlowMemo::Claim { amount_raw, .. } => *amount_raw,
        };
        if amount != expected_amount
            || transfer
                .destination_post
                .saturating_sub(transfer.destination_pre)
                != amount
        {
            bail!("Earn MAX cash-flow memo amount did not match confirmed token deltas");
        }
        if project_earn_max_cash_flow(store, memo, cash_flow, transfer).await? {
            applied += 1;
        }
    }
    Ok(applied)
}

async fn project_earn_max_cash_flow(
    store: &OrchestratorStore,
    memo: &EarnMaxMemoInstruction,
    cash_flow: EarnMaxCashFlowMemo,
    transfer: ConfirmedCashFlowTransfer,
) -> Result<bool> {
    let settings = match &cash_flow {
        EarnMaxCashFlowMemo::Deposit { settings, .. }
        | EarnMaxCashFlowMemo::Claim { settings, .. } => *settings,
    };
    let route_key = format!("earn-max:{settings}:0");
    let mut lease = store
        .lease_multiply_route_state(
            &route_key,
            "earn-max-laserstream",
            Utc::now() + ChronoDuration::seconds(30),
        )
        .await?
        .context("Earn MAX cash-flow route is actively leased; replay after release")?;
    let stored = store
        .load_multiply_route_state(&route_key)
        .await?
        .context("Earn MAX cash-flow route is not projected")?;
    let mut state = stored.state;
    let amount = transfer.source_pre - transfer.source_post;
    let amount_delta = i64::try_from(amount).context("Earn MAX cash flow exceeds i64")?;
    let now = Utc::now();
    let (action, strategy_key, expected_effects) = match cash_flow {
        EarnMaxCashFlowMemo::Deposit { .. } => {
            if matches!(state.position, MultiplyPosition::Idle { .. }) {
                state.position = MultiplyPosition::Idle {
                    claim: TokenBalance {
                        account: transfer.destination.to_string(),
                        mint: USDC_MINT.to_string(),
                        token_program: spl_token::ID.to_string(),
                        amount_raw: transfer.destination_post,
                    },
                };
            }
            state.observed_slot = transfer.slot;
            state.observed_at = now;
            let evidence = DepositEvidence {
                request_id: format!(
                    "chain:{}:{}",
                    transfer.signature, memo.source_instruction_index
                ),
                transaction_signature: transfer.signature.to_string(),
                wallet_account: transfer.source.to_string(),
                wallet_pre_amount_raw: transfer.source_pre,
                wallet_post_amount_raw: transfer.source_post,
                vault_pre_amount_raw: transfer.destination_pre,
                vault_post_amount_raw: transfer.destination_post,
                amount_raw: amount,
                observed_slot: transfer.slot,
                observed_at: now,
            };
            state = state.admit_deposit(evidence)?;
            (
                MultiplyAction::DepositClaimAsset,
                None,
                ExpectedEffects {
                    token_amounts_before: vec![
                        TokenAmountBefore {
                            account: transfer.source.to_string(),
                            mint: USDC_MINT.to_string(),
                            amount_raw: transfer.source_pre,
                        },
                        TokenAmountBefore {
                            account: transfer.destination.to_string(),
                            mint: USDC_MINT.to_string(),
                            amount_raw: transfer.destination_pre,
                        },
                    ],
                    token_deltas: vec![
                        TokenDelta {
                            account: transfer.source.to_string(),
                            mint: USDC_MINT.to_string(),
                            raw_delta: -amount_delta,
                        },
                        TokenDelta {
                            account: transfer.destination.to_string(),
                            mint: USDC_MINT.to_string(),
                            raw_delta: amount_delta,
                        },
                    ],
                    obligation_before: None,
                    obligation_delta: None,
                },
            )
        }
        EarnMaxCashFlowMemo::Claim {
            request_id,
            destination,
            ..
        } => {
            let withdrawal = state
                .withdrawal
                .as_mut()
                .context("Earn MAX claim route omitted withdrawal")?;
            if withdrawal.status != WithdrawalStatus::Claimable
                || withdrawal.request_id != request_id
                || withdrawal.destination_account != destination.to_string()
                || withdrawal.amount_raw.min(transfer.source_pre) != amount
            {
                bail!("Earn MAX claim did not match the claimable withdrawal");
            }
            withdrawal.status = WithdrawalStatus::Claimed;
            withdrawal.claim_signature = Some(transfer.signature.to_string());
            state.generation += 1;
            state.goal = if transfer.source_post > 0 {
                RouteGoal::Deploy
            } else {
                RouteGoal::Claimed
            };
            state.position = MultiplyPosition::Idle {
                claim: TokenBalance {
                    account: transfer.source.to_string(),
                    mint: USDC_MINT.to_string(),
                    token_program: spl_token::ID.to_string(),
                    amount_raw: transfer.source_post,
                },
            };
            state.observed_slot = transfer.slot;
            state.observed_at = now;
            (
                MultiplyAction::Claim,
                None,
                ExpectedEffects {
                    token_amounts_before: vec![
                        TokenAmountBefore {
                            account: transfer.source.to_string(),
                            mint: USDC_MINT.to_string(),
                            amount_raw: transfer.source_pre,
                        },
                        TokenAmountBefore {
                            account: transfer.destination.to_string(),
                            mint: USDC_MINT.to_string(),
                            amount_raw: transfer.destination_pre,
                        },
                    ],
                    token_deltas: vec![
                        TokenDelta {
                            account: transfer.source.to_string(),
                            mint: USDC_MINT.to_string(),
                            raw_delta: -amount_delta,
                        },
                        TokenDelta {
                            account: transfer.destination.to_string(),
                            mint: USDC_MINT.to_string(),
                            raw_delta: amount_delta,
                        },
                    ],
                    obligation_before: None,
                    obligation_delta: None,
                },
            )
        }
    };
    let evidence = json!({
        "signature": transfer.signature.to_string(),
        "instructionIndex": memo.source_instruction_index,
        "slot": transfer.slot,
        "source": transfer.source.to_string(),
        "sourcePre": transfer.source_pre,
        "sourcePost": transfer.source_post,
        "destination": transfer.destination.to_string(),
        "destinationPre": transfer.destination_pre,
        "destinationPost": transfer.destination_post,
    });
    let evidence_bytes = serde_json::to_vec(&evidence)?;
    let operation = MultiplyOperation {
        operation_id: format!(
            "cash-{}",
            &hex_hash(
                format!("{}:{}", transfer.signature, memo.source_instruction_index).as_bytes()
            )[..32]
        ),
        route_key: route_key.clone(),
        cycle: state.cycle,
        engine_version: MULTIPLY_ENGINE_VERSION.to_owned(),
        action,
        strategy_key,
        status: MultiplyOperationStatus::Reconciled,
        idempotency_key: format!(
            "{MULTIPLY_ENGINE_VERSION}:cash-flow:{}:{}",
            transfer.signature, memo.source_instruction_index
        ),
        expected_effects,
        policy_account: None,
        policy_data_sha256: None,
        message_sha256: None,
        signed_wire: None,
        signed_wire_sha256: None,
        transaction_signature: Some(transfer.signature.to_string()),
        source_instruction_index: Some(memo.source_instruction_index),
        recent_blockhash: None,
        last_valid_block_height: None,
        broadcast_intent_at: None,
        confirmed_slot: Some(transfer.slot),
        reconciliation_sha256: Some(hex_hash(&evidence_bytes)),
        created_at: now,
        updated_at: now,
    };
    let inserted = store
        .admit_external_multiply_operation(&mut lease, &state, &operation)
        .await?;
    if !store.release_multiply_route_lease(&lease).await? {
        bail!("Earn MAX cash-flow projection lost its route lease");
    }
    Ok(inserted)
}

async fn read_confirmed_earn_max_transfer(
    rpc: Arc<RpcClient>,
    signature: String,
    expected_slot: u64,
    source: Pubkey,
    destination: Pubkey,
) -> Result<ConfirmedCashFlowTransfer> {
    tokio::task::spawn_blocking(move || {
        let signature = Signature::from_str(&signature)?;
        let transaction = rpc.get_transaction_with_config(
            &signature,
            RpcTransactionConfig {
                encoding: Some(UiTransactionEncoding::Base64),
                commitment: Some(CommitmentConfig::confirmed()),
                max_supported_transaction_version: Some(0),
            },
        )?;
        if transaction.slot != expected_slot {
            bail!("Earn MAX cash-flow RPC slot drifted from LaserStream");
        }
        let decoded = transaction
            .transaction
            .transaction
            .decode()
            .context("Earn MAX cash-flow transaction bytes did not decode")?;
        let meta = transaction
            .transaction
            .meta
            .as_ref()
            .context("Earn MAX cash-flow metadata was missing")?;
        if meta.err.is_some() {
            bail!("Earn MAX cash-flow transaction failed");
        }
        let keys = transaction_account_keys(&decoded, meta)?;
        let source_index = account_index(&keys, source)?;
        let destination_index = account_index(&keys, destination)?;
        Ok(ConfirmedCashFlowTransfer {
            signature,
            slot: transaction.slot,
            source,
            destination,
            source_pre: token_amount(
                &meta.pre_token_balances,
                source_index,
                &USDC_MINT.to_string(),
            )?,
            source_post: token_amount(
                &meta.post_token_balances,
                source_index,
                &USDC_MINT.to_string(),
            )?,
            destination_pre: token_amount(
                &meta.pre_token_balances,
                destination_index,
                &USDC_MINT.to_string(),
            )
            .unwrap_or(0),
            destination_post: token_amount(
                &meta.post_token_balances,
                destination_index,
                &USDC_MINT.to_string(),
            )?,
        })
    })
    .await
    .context("Earn MAX cash-flow readback task panicked")?
}

fn hex_hash(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

fn transaction_account_keys(
    transaction: &VersionedTransaction,
    meta: &UiTransactionStatusMeta,
) -> Result<Vec<Pubkey>> {
    let mut keys = transaction.message.static_account_keys().to_vec();
    match &meta.loaded_addresses {
        OptionSerializer::Some(loaded) => {
            for key in loaded.writable.iter().chain(&loaded.readonly) {
                keys.push(Pubkey::from_str(key)?);
            }
        }
        OptionSerializer::None | OptionSerializer::Skip => {
            if matches!(
                transaction.message,
                solana_sdk::message::VersionedMessage::V0(_)
            ) {
                bail!("Earn MAX cash-flow transaction omitted loaded addresses");
            }
        }
    }
    Ok(keys)
}

fn account_index(keys: &[Pubkey], expected: Pubkey) -> Result<u8> {
    let index = keys
        .iter()
        .position(|key| key == &expected)
        .context("Earn MAX cash-flow token account is absent")?;
    u8::try_from(index).context("Earn MAX cash-flow account index exceeds u8")
}

fn token_amount(
    balances: &OptionSerializer<Vec<UiTransactionTokenBalance>>,
    account_index: u8,
    mint: &str,
) -> Result<u64> {
    let balances = match balances {
        OptionSerializer::Some(values) => values,
        OptionSerializer::None | OptionSerializer::Skip => {
            bail!("Earn MAX cash-flow transaction omitted token balances")
        }
    };
    let mut matches = balances
        .iter()
        .filter(|value| value.account_index == account_index && value.mint == mint);
    let value = matches
        .next()
        .context("Earn MAX cash-flow token balance is absent")?;
    if matches.next().is_some() || value.ui_token_amount.decimals != 6 {
        bail!("Earn MAX cash-flow token balance is ambiguous");
    }
    value
        .ui_token_amount
        .amount
        .parse()
        .context("Earn MAX cash-flow token amount is invalid")
}

fn read_autodeposit_snapshot(
    rpc: &RpcClient,
    target: &AutodepositTargetSnapshotContext,
    minimum_slot: u64,
) -> Result<AutodepositChainObservation> {
    let policy = Pubkey::from_str(&target.policy_account)?;
    let subscription_authority = Pubkey::from_str(&target.subscription_authority)?;
    let recurring_delegation = Pubkey::from_str(&target.recurring_delegation)?;
    let wallet = Pubkey::from_str(&target.wallet)?;
    let wallet_ata = Pubkey::from_str(&target.wallet_token_ata)?;
    let response = rpc.get_multiple_accounts_with_config(
        &[
            policy,
            subscription_authority,
            recurring_delegation,
            wallet_ata,
        ],
        RpcAccountInfoConfig {
            encoding: Some(UiAccountEncoding::Base64),
            commitment: Some(CommitmentConfig::finalized()),
            min_context_slot: Some(minimum_slot),
            ..RpcAccountInfoConfig::default()
        },
    )?;
    let [policy_account, authority_account, delegation_account, token_account] =
        response.value.as_slice()
    else {
        bail!("Autodeposit snapshot did not return four requested accounts");
    };
    let policy_valid = policy_account
        .as_ref()
        .is_some_and(|account| account.owner == SQUADS_SMART_ACCOUNT_PROGRAM_ID);
    let subscription_authority_valid = authority_account
        .as_ref()
        .is_some_and(|account| account.owner == SUBSCRIPTIONS_PROGRAM_ID);
    let recurring_delegation_valid = delegation_account
        .as_ref()
        .is_some_and(|account| account.owner == SUBSCRIPTIONS_PROGRAM_ID);
    let token_delegate_valid = token_account.as_ref().is_some_and(|account| {
        if account.owner != spl_token::ID {
            return false;
        }
        spl_token::state::Account::unpack(&account.data)
            .ok()
            .is_some_and(|token| {
                token.owner == wallet
                    && token.mint == USDC_MINT
                    && token.delegate
                        == solana_program::program_option::COption::Some(subscription_authority)
            })
    });
    let wallet_balance_raw = token_account
        .as_ref()
        .and_then(|account| spl_token::state::Account::unpack(&account.data).ok())
        .map(|token| token.amount)
        .unwrap_or(0);
    Ok(AutodepositChainObservation {
        target_id: target.target_id,
        observation_slot: response.context.slot,
        observation_complete: true,
        policy_valid,
        subscription_authority_valid,
        recurring_delegation_valid,
        token_delegate_valid,
        wallet_balance_raw,
    })
}

fn resolve_rpc_mutation(
    rpc: &RpcClient,
    update: &NormalizedEarnUpdate,
    vault: &EarnVaultWatch,
    context: loyal_yield_store::EarnReconciliationContext,
) -> Result<EarnDirectMutation> {
    let is_policy_deletion = update.event_kind == "account_deleted"
        && update.account_pubkey.as_deref().is_some_and(|pubkey| {
            vault
                .accounts
                .iter()
                .any(|account| account.role == "policy" && account.pubkey == pubkey)
        });
    if is_policy_deletion {
        let signature = update
            .signature
            .as_deref()
            .context("closed policy update has no transaction signature")?;
        let transaction = read_transaction_json(rpc, signature)?;
        let transaction_slot = transaction
            .get("slot")
            .and_then(Value::as_u64)
            .context("finalized policy-close transaction has no slot")?;
        if transaction_slot != update.slot {
            bail!(
                "transaction {signature} landed at slot {transaction_slot}, expected account-update slot {}",
                update.slot
            );
        }
        let proof = read_cleanup_proof(rpc, vault, update.slot)?;
        if transaction_owner_lamport_credit(&transaction, &vault.wallet)? > 0 {
            return Ok(EarnDirectMutation::Refund(EarnRefundMutation {
                cluster: vault.environment.clone(),
                full_cleanup: proof.balances_zero && proof.policies_closed,
                settings: vault.settings.clone(),
                vault_index: vault.vault_index,
                vault_pubkey: vault.vault.clone(),
                wallet: vault.wallet.clone(),
                refund_signature: signature.to_owned(),
                confirmed_slot: transaction_slot,
                refund_kind: "policy".to_owned(),
                observed_at: None,
            }));
        }
        if !proof.balances_zero || !proof.policies_closed {
            return Ok(EarnDirectMutation::Noop);
        }
        return Ok(EarnDirectMutation::Cleanup(EarnCleanupMutation {
            settings: vault.settings.clone(),
            vault_index: vault.vault_index,
            vault_pubkey: vault.vault.clone(),
            cleanup_signature: update
                .signature
                .clone()
                .context("closed policy update has no transaction signature")?,
            confirmed_slot: update.slot,
            observed_at: None,
        }));
    }

    let Some(signature) = update.signature.as_deref() else {
        return Ok(EarnDirectMutation::Noop);
    };
    let transaction = read_transaction_json(rpc, signature)?;
    let transaction_slot = transaction
        .get("slot")
        .and_then(Value::as_u64)
        .context("finalized transaction has no slot")?;
    if transaction_slot != update.slot {
        bail!(
            "transaction {signature} landed at slot {transaction_slot}, expected account-update slot {}",
            update.slot
        );
    }
    let supported_mints = earn_stablecoins()
        .iter()
        .map(|asset| asset.mint.to_string())
        .collect::<Vec<_>>();
    let Some(cash_flow) = classify_transaction_cash_flow(
        &transaction,
        &vault.wallet,
        supported_mints.iter().map(String::as_str),
        update,
        vault,
    )?
    else {
        return Ok(EarnDirectMutation::Noop);
    };
    if cash_flow.kind == CashFlowKind::Refund {
        return Ok(EarnDirectMutation::Refund(EarnRefundMutation {
            cluster: vault.environment.clone(),
            full_cleanup: false,
            settings: vault.settings.clone(),
            vault_index: vault.vault_index,
            vault_pubkey: vault.vault.clone(),
            wallet: vault.wallet.clone(),
            refund_signature: signature.to_owned(),
            confirmed_slot: transaction_slot,
            refund_kind: cash_flow
                .refund_kind
                .unwrap_or_else(|| "account".to_owned()),
            observed_at: None,
        }));
    }
    let Some(route_policy) = context.route_policy else {
        bail!("cash flow {signature} arrived before its finalized route policy projection");
    };
    read_cash_flow_proof(
        rpc,
        update,
        vault,
        route_policy,
        context.setup_policy,
        transaction,
        cash_flow,
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
            commitment: Some(CommitmentConfig::finalized()),
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
        let response =
            get_token_accounts_by_owner_with_config(rpc, vault, token_program, min_context_slot)?;
        if response.context.slot < min_context_slot {
            bail!(
                "token inventory context slot {} is below minimum {min_context_slot}",
                response.context.slot
            );
        }
        for keyed in response.value {
            let address = Pubkey::from_str(&keyed.pubkey)?;
            let account = keyed
                .account
                .decode()
                .with_context(|| format!("decode owner token account {address}"))?;
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

fn get_token_accounts_by_owner_with_config(
    rpc: &RpcClient,
    owner: Pubkey,
    token_program: Pubkey,
    min_context_slot: u64,
) -> Result<RpcResponse<Vec<RpcKeyedAccount>>> {
    let config = RpcAccountInfoConfig {
        encoding: Some(UiAccountEncoding::Base64),
        commitment: Some(CommitmentConfig::finalized()),
        min_context_slot: Some(min_context_slot),
        ..RpcAccountInfoConfig::default()
    };
    rpc.send(
        RpcRequest::GetTokenAccountsByOwner,
        json!([
            owner.to_string(),
            RpcTokenAccountsFilter::ProgramId(token_program.to_string()),
            config
        ]),
    )
    .context("get token accounts by owner")
}

fn read_squads_policy_transaction(
    rpc: &RpcClient,
    signature: &str,
    expected_slot: u64,
    commitment: CommitmentConfig,
) -> Result<EarnPolicyTransactionRead> {
    let parsed_signature =
        Signature::from_str(signature).context("invalid transaction signature")?;
    let transaction = rpc.get_transaction_with_config(
        &parsed_signature,
        RpcTransactionConfig {
            encoding: Some(UiTransactionEncoding::Base64),
            commitment: Some(commitment),
            max_supported_transaction_version: Some(0),
        },
    )?;
    if transaction.slot != expected_slot {
        bail!(
            "transaction {signature} landed at slot {}, expected {expected_slot}",
            transaction.slot
        );
    }
    let meta = transaction
        .transaction
        .meta
        .as_ref()
        .context("policy transaction has no status metadata")?;
    if policy_transaction_disposition(meta.err.as_ref())
        == PolicyTransactionDisposition::NoStateChange
    {
        return Ok(EarnPolicyTransactionRead::NoStateChange);
    }
    let transaction = transaction
        .transaction
        .transaction
        .decode()
        .context("decode policy versioned transaction")?;
    let mut account_keys = transaction.message.static_account_keys().to_vec();
    if let OptionSerializer::Some(loaded) = &meta.loaded_addresses {
        for address in loaded.writable.iter().chain(&loaded.readonly) {
            account_keys.push(Pubkey::from_str(address)?);
        }
    }
    let account_meta = |index: usize| {
        account_keys.get(index).copied().map(|pubkey| AccountMeta {
            pubkey,
            is_signer: transaction.message.is_signer(index),
            is_writable: transaction.message.is_maybe_writable(index, None),
        })
    };
    let mut instructions = Vec::new();
    for compiled in transaction.message.instructions() {
        let Some(program_id) = account_keys
            .get(compiled.program_id_index as usize)
            .copied()
        else {
            continue;
        };
        if program_id != SQUADS_SMART_ACCOUNT_PROGRAM_ID && program_id != SUBSCRIPTIONS_PROGRAM_ID {
            continue;
        }
        let accounts = compiled
            .accounts
            .iter()
            .filter_map(|index| account_meta(usize::from(*index)))
            .collect();
        instructions.push(Instruction {
            program_id,
            accounts,
            data: compiled.data.clone(),
        });
    }
    let mut earn_max_memos = Vec::new();
    if let OptionSerializer::Some(groups) = &meta.inner_instructions {
        for group in groups {
            for (inner_index, instruction) in group.instructions.iter().enumerate() {
                let (program_id, accounts, data) = match instruction {
                    UiInstruction::Compiled(compiled) => {
                        let Some(program_id) = account_keys
                            .get(usize::from(compiled.program_id_index))
                            .copied()
                        else {
                            continue;
                        };
                        let accounts = compiled
                            .accounts
                            .iter()
                            .filter_map(|index| account_meta(usize::from(*index)))
                            .collect::<Vec<_>>();
                        (
                            program_id,
                            accounts,
                            bs58::decode(&compiled.data).into_vec()?,
                        )
                    }
                    UiInstruction::Parsed(UiParsedInstruction::PartiallyDecoded(decoded)) => {
                        let program_id = Pubkey::from_str(&decoded.program_id)?;
                        let accounts = decoded
                            .accounts
                            .iter()
                            .map(|account| Pubkey::from_str(account))
                            .collect::<Result<Vec<_>, _>>()?
                            .into_iter()
                            .map(|pubkey| {
                                account_keys
                                    .iter()
                                    .position(|candidate| *candidate == pubkey)
                                    .and_then(account_meta)
                                    .unwrap_or(AccountMeta::new_readonly(pubkey, false))
                            })
                            .collect();
                        (
                            program_id,
                            accounts,
                            bs58::decode(&decoded.data).into_vec()?,
                        )
                    }
                    UiInstruction::Parsed(UiParsedInstruction::Parsed(_)) => continue,
                };
                if program_id == SQUADS_SMART_ACCOUNT_PROGRAM_ID
                    || program_id == SUBSCRIPTIONS_PROGRAM_ID
                {
                    instructions.push(Instruction {
                        program_id,
                        accounts: accounts.clone(),
                        data: data.clone(),
                    });
                }
                if program_id.to_string() != EARN_MAX_MEMO_PROGRAM {
                    continue;
                }
                let source_instruction_index = u16::from(group.index)
                    .checked_mul(256)
                    .and_then(|value| value.checked_add(u16::try_from(inner_index + 1).ok()?))
                    .context("Earn MAX memo instruction index overflow")?;
                earn_max_memos.push(EarnMaxMemoInstruction {
                    source_instruction_index,
                    accounts: accounts.into_iter().map(|account| account.pubkey).collect(),
                    data,
                });
            }
        }
    }
    Ok(EarnPolicyTransactionRead::Transaction(
        EarnPolicyTransaction {
            signature: signature.to_owned(),
            slot: expected_slot,
            signers: account_keys
                .iter()
                .enumerate()
                .filter_map(|(index, account)| {
                    transaction.message.is_signer(index).then_some(*account)
                })
                .collect(),
            instructions,
            earn_max_memos,
        },
    ))
}

fn parse_earn_max_intent(data: &[u8]) -> Result<Option<EarnMaxIntent>> {
    let Ok(value) = std::str::from_utf8(data) else {
        return Ok(None);
    };
    let fields = value.split(':').collect::<Vec<_>>();
    if fields.get(0..3) != Some(&["loyal", "earn-max", "v2"]) {
        return Ok(None);
    }
    let valid_request_id = |request_id: &str| {
        (8..=64).contains(&request_id.len())
            && request_id
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
    };
    match fields.as_slice() {
        [_, _, _, "withdraw", request_id, amount, destination] if valid_request_id(request_id) => {
            Pubkey::from_str(destination).context("invalid Earn MAX withdrawal destination")?;
            let amount_raw = if *amount == "max" {
                None
            } else {
                let parsed = amount
                    .parse::<u64>()
                    .context("invalid Earn MAX withdrawal amount")?;
                if parsed == 0 {
                    bail!("Earn MAX withdrawal amount is zero");
                }
                Some(parsed)
            };
            Ok(Some(EarnMaxIntent::Withdraw {
                request_id: (*request_id).to_owned(),
                destination_account: (*destination).to_owned(),
                amount_raw,
            }))
        }
        [_, _, _, "cancel", request_id] if valid_request_id(request_id) => {
            Ok(Some(EarnMaxIntent::Cancel {
                request_id: (*request_id).to_owned(),
            }))
        }
        [_, _, _, "deposit", ..] | [_, _, _, "claim", ..] => Ok(None),
        _ => bail!("malformed Earn MAX intent memo"),
    }
}

fn parse_earn_max_cash_flow(data: &[u8]) -> Result<Option<EarnMaxCashFlowMemo>> {
    let Ok(value) = std::str::from_utf8(data) else {
        return Ok(None);
    };
    let fields = value.split(':').collect::<Vec<_>>();
    if fields.get(0..3) != Some(&["loyal", "earn-max", "v2"]) {
        return Ok(None);
    }
    match fields.as_slice() {
        [_, _, _, "deposit", amount, settings] => {
            let amount_raw = amount
                .parse::<u64>()
                .context("invalid Earn MAX deposit amount")?;
            if amount_raw == 0 {
                bail!("Earn MAX deposit amount is zero");
            }
            Ok(Some(EarnMaxCashFlowMemo::Deposit {
                settings: Pubkey::from_str(settings)
                    .context("invalid Earn MAX deposit Settings")?,
                amount_raw,
            }))
        }
        [_, _, _, "claim", request_id, amount, destination, settings]
            if (8..=64).contains(&request_id.len())
                && request_id
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-')) =>
        {
            let amount_raw = amount
                .parse::<u64>()
                .context("invalid Earn MAX claim amount")?;
            if amount_raw == 0 {
                bail!("Earn MAX claim amount is zero");
            }
            Ok(Some(EarnMaxCashFlowMemo::Claim {
                settings: Pubkey::from_str(settings).context("invalid Earn MAX claim Settings")?,
                request_id: (*request_id).to_owned(),
                amount_raw,
                destination: Pubkey::from_str(destination)
                    .context("invalid Earn MAX claim destination")?,
            }))
        }
        [_, _, _, "withdraw", ..] | [_, _, _, "cancel", ..] => Ok(None),
        _ => bail!("malformed Earn MAX cash-flow memo"),
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum CashFlowKind {
    Deposit,
    Withdrawal,
    Refund,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct CashFlowEvidence {
    kind: CashFlowKind,
    mint: String,
    amount_raw: u64,
    refund_kind: Option<String>,
}

#[derive(Debug)]
struct CompleteVaultSnapshot {
    observed_slot: u64,
    reserve_state: Vec<EarnReserveMutation>,
    idle_state: Vec<EarnIdleTokenMutation>,
}

pub(crate) fn classify_transaction_cash_flow<'a>(
    transaction: &Value,
    wallet: &str,
    mints: impl IntoIterator<Item = &'a str>,
    update: &NormalizedEarnUpdate,
    vault: &EarnVaultWatch,
) -> Result<Option<CashFlowEvidence>> {
    let deleted_role = (update.event_kind == "account_deleted")
        .then(|| {
            update.account_pubkey.as_deref().and_then(|pubkey| {
                vault
                    .accounts
                    .iter()
                    .find(|account| account.pubkey == pubkey)
                    .map(|account| account.role.as_str())
            })
        })
        .flatten();
    if matches!(deleted_role, Some("policy" | "idle_token" | "vault"))
        && transaction_owner_lamport_credit(transaction, wallet)? > 0
    {
        return Ok(Some(CashFlowEvidence {
            kind: CashFlowKind::Refund,
            mint: String::new(),
            amount_raw: transaction_owner_lamport_credit(transaction, wallet)?,
            refund_kind: Some(
                match deleted_role {
                    Some("policy") => "policy",
                    Some("idle_token") => "vault_token_account",
                    _ => "vault_account",
                }
                .to_owned(),
            ),
        }));
    }

    if !transaction_has_earn_anchor(transaction, vault) {
        return Ok(None);
    }

    let mut result = None;
    for mint in mints {
        let delta = transaction_owner_token_delta(transaction, mint, wallet)?;
        if delta == 0 {
            continue;
        }
        if result.is_some() {
            bail!("one Earn transaction changed more than one supported wallet mint");
        }
        let (kind, amount_raw) = if delta < 0 {
            (CashFlowKind::Deposit, u64::try_from(-delta)?)
        } else {
            (CashFlowKind::Withdrawal, u64::try_from(delta)?)
        };
        result = Some(CashFlowEvidence {
            kind,
            mint: mint.to_owned(),
            amount_raw,
            refund_kind: None,
        });
    }
    Ok(result)
}

fn transaction_has_earn_anchor(transaction: &Value, vault: &EarnVaultWatch) -> bool {
    let accounts = transaction_accounts(transaction);
    accounts.contains(&vault.settings)
        || accounts.contains(&vault.vault)
        || vault.accounts.iter().any(|account| {
            matches!(
                account.role.as_str(),
                "smart_account" | "policy" | "vault" | "idle_token" | "obligation"
            ) && accounts.contains(&account.pubkey)
        })
}

fn transaction_owner_token_delta(transaction: &Value, mint: &str, owner: &str) -> Result<i128> {
    let balances = |name: &str| -> Result<BTreeMap<u64, (Option<String>, u64)>> {
        transaction
            .pointer(&format!("/meta/{name}"))
            .and_then(Value::as_array)
            .cloned()
            .unwrap_or_default()
            .into_iter()
            .filter(|row| row.get("mint").and_then(Value::as_str) == Some(mint))
            .try_fold(BTreeMap::new(), |mut parsed, row| {
                let index = row
                    .get("accountIndex")
                    .and_then(Value::as_u64)
                    .context("token balance has no account index")?;
                let amount = row
                    .pointer("/uiTokenAmount/amount")
                    .and_then(Value::as_str)
                    .context("token balance has no raw amount")?
                    .parse::<u64>()?;
                let row_owner = row
                    .get("owner")
                    .and_then(Value::as_str)
                    .map(ToOwned::to_owned);
                if parsed.insert(index, (row_owner, amount)).is_some() {
                    bail!("duplicate token balance account index {index} in {name}");
                }
                Ok(parsed)
            })
    };
    let pre = balances("preTokenBalances")?;
    let post = balances("postTokenBalances")?;
    pre.keys()
        .chain(post.keys())
        .copied()
        .collect::<BTreeSet<_>>()
        .into_iter()
        .try_fold(0_i128, |delta, index| {
            let pre_row = pre.get(&index);
            let post_row = post.get(&index);
            let row_owner = post_row
                .and_then(|row| row.0.as_deref())
                .or_else(|| pre_row.and_then(|row| row.0.as_deref()));
            if row_owner != Some(owner) {
                return Ok(delta);
            }
            let pre_amount = pre_row.map(|row| row.1).unwrap_or_default();
            let post_amount = post_row.map(|row| row.1).unwrap_or_default();
            delta
                .checked_add(i128::from(post_amount) - i128::from(pre_amount))
                .context("wallet token delta overflow")
        })
}

fn transaction_owner_lamport_credit(transaction: &Value, owner: &str) -> Result<u64> {
    let keys = transaction
        .pointer("/transaction/message/accountKeys")
        .and_then(Value::as_array)
        .context("transaction has no account keys")?;
    let Some(index) = keys.iter().position(|key| {
        key.as_str()
            .or_else(|| key.get("pubkey").and_then(Value::as_str))
            == Some(owner)
    }) else {
        return Ok(0);
    };
    let pre = transaction
        .pointer("/meta/preBalances")
        .and_then(Value::as_array)
        .and_then(|balances| balances.get(index))
        .and_then(Value::as_u64)
        .unwrap_or_default();
    let post = transaction
        .pointer("/meta/postBalances")
        .and_then(Value::as_array)
        .and_then(|balances| balances.get(index))
        .and_then(Value::as_u64)
        .unwrap_or_default();
    let fee = if index == 0 {
        transaction
            .pointer("/meta/fee")
            .and_then(Value::as_u64)
            .unwrap_or_default()
    } else {
        0
    };
    Ok(post.saturating_add(fee).saturating_sub(pre))
}

fn read_cash_flow_proof(
    rpc: &RpcClient,
    update: &NormalizedEarnUpdate,
    vault: &EarnVaultWatch,
    route_policy: PolicyMatchInput,
    setup_policy: Option<PolicyMatchInput>,
    transaction: Value,
    cash_flow: CashFlowEvidence,
) -> Result<EarnDirectMutation> {
    let signature = update
        .signature
        .as_deref()
        .context("cash-flow update is missing its transaction signature")?;
    let snapshot = read_complete_vault_snapshot(rpc, vault, &route_policy, update.slot)?;
    let transaction_accounts = transaction_accounts(&transaction);
    let target = snapshot.reserve_state.iter().find(|reserve| {
        reserve.liquidity_mint == cash_flow.mint && transaction_accounts.contains(&reserve.reserve)
    });
    let Some(target) = target else {
        return Ok(EarnDirectMutation::Noop);
    };
    let remaining_amount_raw = snapshot
        .reserve_state
        .iter()
        .filter(|reserve| reserve.liquidity_mint == cash_flow.mint)
        .try_fold(0_u64, |sum, reserve| sum.checked_add(reserve.amount_raw))
        .context("Earn reserve balance overflow")?
        .checked_add(
            snapshot
                .idle_state
                .iter()
                .filter(|idle| idle.mint == cash_flow.mint)
                .try_fold(0_u64, |sum, idle| sum.checked_add(idle.amount_raw))
                .context("Earn idle balance overflow")?,
        )
        .context("Earn remaining balance overflow")?;
    match cash_flow.kind {
        CashFlowKind::Deposit => Ok(EarnDirectMutation::Deposit(EarnDepositMutation {
            route_policy,
            setup_policy,
            deposit_signature: signature.to_owned(),
            deposit_slot: update.slot,
            observed_slot: snapshot.observed_slot,
            deposit_mint: cash_flow.mint.clone(),
            principal_amount_raw: cash_flow.amount_raw,
            target_reserve: target.reserve.clone(),
            market: target.market.clone(),
            liquidity_mint: cash_flow.mint,
            target_supply_apy_bps: target.supply_apy_bps,
            wallet: vault.wallet.clone(),
            smart_account_address: vault.vault.clone(),
            reserve_state: snapshot.reserve_state,
            idle_state: snapshot.idle_state,
            observed_at: None,
        })),
        CashFlowKind::Withdrawal => Ok(EarnDirectMutation::Withdrawal(EarnWithdrawalMutation {
            route_policy,
            withdrawal_signature: signature.to_owned(),
            confirmed_slot: update.slot,
            observed_slot: snapshot.observed_slot,
            wallet: vault.wallet.clone(),
            vault_pubkey: vault.vault.clone(),
            target_reserve: target.reserve.clone(),
            market: target.market.clone(),
            liquidity_mint: cash_flow.mint,
            withdrawn_amount_raw: cash_flow.amount_raw,
            remaining_amount_raw,
            reserve_state: snapshot.reserve_state,
            idle_state: snapshot.idle_state,
            observed_at: None,
        })),
        CashFlowKind::Refund => unreachable!("refunds are returned before reserve proof"),
    }
}

fn read_complete_vault_snapshot(
    rpc: &RpcClient,
    vault: &EarnVaultWatch,
    route_policy: &PolicyMatchInput,
    min_context_slot: u64,
) -> Result<CompleteVaultSnapshot> {
    let vault_pubkey = Pubkey::from_str(&vault.vault)?;
    let obligations = vault
        .accounts
        .iter()
        .filter(|account| account.role == "obligation")
        .map(|account| Pubkey::from_str(&account.pubkey))
        .collect::<std::result::Result<Vec<_>, _>>()?;
    let discovery = rpc.get_multiple_accounts_with_config(
        &obligations,
        RpcAccountInfoConfig {
            encoding: Some(UiAccountEncoding::Base64),
            commitment: Some(CommitmentConfig::finalized()),
            min_context_slot: Some(min_context_slot),
            ..RpcAccountInfoConfig::default()
        },
    )?;
    let mut reserves = BTreeSet::new();
    for account in discovery.value.iter().flatten() {
        if account.owner != KLEND_PROGRAM_ID {
            continue;
        }
        let obligation = from_account_data::<Obligation>(&account.data)
            .context("decode discovered Earn obligation")?;
        if obligation.owner != vault_pubkey {
            continue;
        }
        reserves.extend(
            obligation
                .deposits
                .iter()
                .map(|deposit| deposit.deposit_reserve)
                .filter(|reserve| *reserve != Pubkey::default()),
        );
    }
    let idle = vault
        .accounts
        .iter()
        .filter(|account| account.role == "idle_token")
        .map(|account| Pubkey::from_str(&account.pubkey))
        .collect::<std::result::Result<Vec<_>, _>>()?;
    let addresses = obligations
        .iter()
        .copied()
        .chain(reserves.iter().copied())
        .chain(idle.iter().copied())
        .collect::<Vec<_>>();
    let response = rpc.get_multiple_accounts_with_config(
        &addresses,
        RpcAccountInfoConfig {
            encoding: Some(UiAccountEncoding::Base64),
            commitment: Some(CommitmentConfig::finalized()),
            min_context_slot: Some(discovery.context.slot.max(min_context_slot)),
            ..RpcAccountInfoConfig::default()
        },
    )?;
    if response.context.slot < min_context_slot {
        bail!(
            "Earn snapshot context slot {} is below minimum {min_context_slot}",
            response.context.slot
        );
    }
    let obligation_count = obligations.len();
    let reserve_count = reserves.len();
    let reserve_accounts = reserves
        .iter()
        .copied()
        .zip(response.value[obligation_count..obligation_count + reserve_count].iter())
        .filter_map(|(address, account)| account.as_ref().map(|account| (address, account)))
        .map(|(address, account)| {
            if account.owner != KLEND_PROGRAM_ID {
                bail!(
                    "Earn reserve {address} has unexpected owner {}",
                    account.owner
                );
            }
            Ok((
                address,
                from_account_data::<Reserve>(&account.data).context("decode Earn reserve")?,
            ))
        })
        .collect::<Result<BTreeMap<_, _>>>()?;
    let allowed_markets = route_policy
        .kamino_markets
        .iter()
        .map(|market| Pubkey::from_str(market))
        .collect::<std::result::Result<BTreeSet<_>, _>>()?;
    let allowed_mints = route_policy
        .kamino_liquidity_mints
        .iter()
        .map(|mint| Pubkey::from_str(mint))
        .collect::<std::result::Result<BTreeSet<_>, _>>()?;
    let mut reserve_state = Vec::new();
    for account in response.value[..obligation_count].iter().flatten() {
        if account.owner != KLEND_PROGRAM_ID {
            continue;
        }
        let obligation = from_account_data::<Obligation>(&account.data)
            .context("decode canonical Earn obligation")?;
        if obligation.owner != vault_pubkey || !allowed_markets.contains(&obligation.lending_market)
        {
            continue;
        }
        for deposit in obligation
            .deposits
            .iter()
            .filter(|deposit| deposit.deposit_reserve != Pubkey::default())
        {
            let Some(reserve) = reserve_accounts.get(&deposit.deposit_reserve) else {
                continue;
            };
            if reserve.lending_market != obligation.lending_market
                || !allowed_mints.contains(&reserve.liquidity.mint_pubkey)
            {
                continue;
            }
            let amount_raw = collateral_to_redeemable_liquidity(
                reserve.collateral.mint_total_supply,
                reserve_total_liquidity_scaled(reserve)?,
                deposit.deposited_amount,
            )?;
            reserve_state.push(EarnReserveMutation {
                reserve: deposit.deposit_reserve.to_string(),
                market: Some(obligation.lending_market.to_string()),
                liquidity_mint: reserve.liquidity.mint_pubkey.to_string(),
                amount_raw,
                has_value: amount_raw > 0,
                supply_apy_bps: None,
                borrow_apy_bps: None,
                planning_metadata: json!({
                    "kind": "earn_laserstream_complete_snapshot",
                    "slot": response.context.slot,
                }),
            });
        }
    }
    reserve_state.sort_by(|left, right| left.reserve.cmp(&right.reserve));
    reserve_state.dedup_by(|left, right| left.reserve == right.reserve);
    let mut idle_state = Vec::new();
    for (address, account) in idle
        .iter()
        .zip(response.value[obligation_count + reserve_count..].iter())
    {
        let Some(account) = account else {
            continue;
        };
        let (mint, owner, amount_raw) = decode_token_account(account)?;
        if owner != vault_pubkey {
            bail!("Earn idle account {address} belongs to {owner}, expected {vault_pubkey}");
        }
        idle_state.push(EarnIdleTokenMutation {
            mint: mint.to_string(),
            amount_raw,
            owner: owner.to_string(),
            token_account: address.to_string(),
            observed_slot: response.context.slot,
            observed_at: None,
            source_commitment: "finalized".to_owned(),
        });
    }
    Ok(CompleteVaultSnapshot {
        observed_slot: response.context.slot,
        reserve_state,
        idle_state,
    })
}

fn read_transaction_json(rpc: &RpcClient, signature: &str) -> Result<Value> {
    let signature = Signature::from_str(signature).context("invalid transaction signature")?;
    let transaction = rpc.get_transaction_with_config(
        &signature,
        RpcTransactionConfig {
            encoding: Some(UiTransactionEncoding::JsonParsed),
            commitment: Some(CommitmentConfig::finalized()),
            max_supported_transaction_version: Some(0),
        },
    )?;
    let transaction =
        serde_json::to_value(transaction).context("serialize confirmed transaction")?;
    if transaction
        .pointer("/meta/err")
        .is_some_and(|error| !error.is_null())
    {
        bail!("transaction {signature} failed on chain");
    }
    Ok(transaction)
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
        .pointer("/transaction/message/accountKeys")
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

#[cfg(test)]
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
            .pointer(&format!("/meta/{name}"))
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

fn durable_earn_event_key(update: &NormalizedEarnUpdate, affected: &[&EarnVaultWatch]) -> String {
    let policy_discovery_account = update.account_pubkey.as_deref().is_some_and(|pubkey| {
        affected.iter().any(|vault| {
            pubkey == vault.settings
                || pubkey == vault.wallet
                || vault.accounts.iter().any(|account| {
                    account.pubkey == pubkey
                        && (account.role == "smart_account" || account.role == "policy")
                })
        })
    });
    let policy_discovery_filter = update.filters.iter().any(|filter| {
        filter == EARN_SMART_ACCOUNTS || filter == EARN_POLICY_ACCOUNTS || filter == EARN_WALLETS
    });
    if policy_discovery_account && policy_discovery_filter {
        if let Some(signature) = update.signature.as_deref() {
            return format!("policy-discovery:{}:{signature}", update.slot);
        }
    }
    update.event_key.clone().unwrap_or_else(|| {
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
    })
}

pub async fn enqueue_normalized_earn_update(
    store: &OrchestratorStore,
    consumer_name: &str,
    update: &NormalizedEarnUpdate,
    watch_set: &SubscriptionWatchSet,
) -> Result<EarnReconciliationEnqueueOutcome> {
    let affected = watch_set.affected_vaults(update.account_pubkey.iter().map(String::as_str));
    if affected.is_empty() {
        bail!(
            "Earn LaserStream update at slot {} matched {:?} but no watched vault",
            update.slot,
            update.filters
        );
    }
    let mut autodeposit_target_ids = Vec::new();
    if let Some(account_pubkey) = update.account_pubkey.as_deref() {
        for vault in &affected {
            if let Some(target_id) = store
                .load_autodeposit_reconciliation_target_id(
                    &vault.settings,
                    &vault.vault,
                    account_pubkey,
                )
                .await?
            {
                if !autodeposit_target_ids.contains(&target_id) {
                    autodeposit_target_ids.push(target_id);
                }
            }
        }
    }
    let event_key = durable_earn_event_key(update, &affected);
    let event_payload = serde_json::to_value(update)?;
    let vaults = affected
        .into_iter()
        .map(|vault| {
            Ok(EarnReconciliationVaultInput {
                settings: vault.settings.clone(),
                vault_index: vault.vault_index,
                vault_pubkey: vault.vault.clone(),
                vault_payload: serde_json::to_value(vault)?,
            })
        })
        .collect::<Result<Vec<_>>>()?;
    store
        .enqueue_earn_reconciliation_jobs(EarnReconciliationEnqueueInput {
            consumer_name: consumer_name.to_owned(),
            event_key,
            durable_slot: update.slot,
            event_payload,
            vaults,
            autodeposit_target_ids,
        })
        .await
        .map_err(Into::into)
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum EarnReconciliationProcessOutcome {
    Idle,
    Completed {
        job_id: i64,
        applied: usize,
    },
    Deferred {
        job_id: i64,
        attempt_count: i32,
        error: String,
    },
}

pub(crate) fn should_emit_reconciliation_retry_alert(attempt_count: i32) -> bool {
    attempt_count == 1
}

pub async fn reconcile_targeted_policy_vault_update(
    store: &OrchestratorStore,
    chain: &dyn EarnChainReader,
    policy_monitor: Option<&Mutex<PolicyMonitor<PostgresPolicyMatchSink>>>,
    update: &NormalizedEarnUpdate,
    vault: &EarnVaultWatch,
) -> Result<bool> {
    if !update.filters.iter().any(|filter| {
        filter == EARN_SMART_ACCOUNTS || filter == EARN_POLICY_ACCOUNTS || filter == EARN_WALLETS
    }) {
        return Ok(false);
    }
    let Some(account_pubkey) = update.account_pubkey.as_deref() else {
        return Ok(false);
    };
    if account_pubkey != vault.settings
        && account_pubkey != vault.wallet
        && !vault.accounts.iter().any(|account| {
            account.pubkey == account_pubkey
                && (account.role == "smart_account" || account.role == "policy")
        })
    {
        return Ok(false);
    }
    let Some(transaction) = chain.policy_transaction_for(update).await? else {
        return Ok(false);
    };
    let EarnPolicyTransactionRead::Transaction(transaction) = transaction else {
        return Ok(true);
    };
    let Some(policy_monitor) = policy_monitor else {
        return Ok(false);
    };
    let settings = Pubkey::from_str(&vault.settings)?;
    let instructions = transaction
        .instructions
        .iter()
        .filter(|instruction| {
            instruction.program_id == SQUADS_SMART_ACCOUNT_PROGRAM_ID
                && instruction
                    .accounts
                    .iter()
                    .any(|account| account.pubkey == settings)
        })
        .cloned()
        .collect::<Vec<_>>();
    let has_squads_execution = !instructions.is_empty();
    let policy_reconciled = if !has_squads_execution {
        false
    } else {
        policy_monitor
            .lock()
            .await
            .process_policy_instructions(&transaction.signature, transaction.slot, instructions)
            .await
            .map_err(|error| anyhow::anyhow!(error))?;
        true
    };

    let vault_pubkey = Pubkey::from_str(&vault.vault)?;
    let mut intent_reconciled = false;
    if has_squads_execution {
        for memo in &transaction.earn_max_memos {
            if !memo.accounts.contains(&vault_pubkey) {
                continue;
            }
            let Some(intent) = parse_earn_max_intent(&memo.data)? else {
                continue;
            };
            store
                .project_earn_max_intent(EarnMaxIntentProjectionInput {
                    settings: vault.settings.clone(),
                    vault_index: vault.vault_index,
                    signature: transaction.signature.clone(),
                    instruction_index: memo.source_instruction_index,
                    slot: transaction.slot,
                    observed_at: Utc::now(),
                    intent,
                })
                .await?;
            intent_reconciled = true;
        }
    }

    let mut subscription_control_observed = false;
    for instruction in &transaction.instructions {
        if instruction.program_id != SUBSCRIPTIONS_PROGRAM_ID {
            continue;
        }
        if instruction.data.first().copied() == Some(SUBSCRIPTIONS_INIT_AUTHORITY) {
            if instruction
                .accounts
                .first()
                .is_some_and(|account| account.pubkey.to_string() == vault.wallet)
            {
                subscription_control_observed = true;
            }
            continue;
        }
        if instruction.data.first().copied() != Some(SUBSCRIPTIONS_CREATE_RECURRING_DELEGATION)
            || instruction.accounts.len() < 4
            || instruction.data.len() < 41
        {
            continue;
        }
        let wallet = instruction.accounts[0].pubkey;
        let subscription_authority = instruction.accounts[1].pubkey;
        let recurring_delegation = instruction.accounts[2].pubkey;
        let delegatee = instruction.accounts[3].pubkey;
        if wallet.to_string() != vault.wallet || delegatee.to_string() != vault.vault {
            continue;
        }
        let read_u64 = |offset: usize| -> Result<u64> {
            Ok(u64::from_le_bytes(
                instruction.data[offset..offset + 8]
                    .try_into()
                    .context("decode recurring delegation u64")?,
            ))
        };
        let read_i64 = |offset: usize| -> Result<i64> {
            Ok(i64::from_le_bytes(
                instruction.data[offset..offset + 8]
                    .try_into()
                    .context("decode recurring delegation i64")?,
            ))
        };
        store
            .record_autodeposit_recurring_delegation(AutodepositRecurringDelegationObserved {
                wallet: wallet.to_string(),
                vault_pubkey: delegatee.to_string(),
                subscription_authority: subscription_authority.to_string(),
                recurring_delegation: recurring_delegation.to_string(),
                nonce: read_u64(1)?,
                amount_per_period: read_u64(9)?,
                period_length_seconds: read_u64(17)?,
                start_timestamp: read_i64(25)?,
                expiry_timestamp: read_i64(33)?,
                signature: transaction.signature.clone(),
                slot: transaction.slot,
            })
            .await?;
        subscription_control_observed = true;
    }
    Ok(intent_reconciled || policy_reconciled || subscription_control_observed)
}

pub async fn process_next_earn_reconciliation_job(
    store: &OrchestratorStore,
    consumer_name: &str,
    claim_owner: &str,
    chain: &dyn EarnChainReader,
    lease_seconds: i64,
    retry_after_seconds: i64,
) -> Result<EarnReconciliationProcessOutcome> {
    process_next_earn_reconciliation_job_with_policy_monitor(
        store,
        consumer_name,
        claim_owner,
        chain,
        None,
        lease_seconds,
        retry_after_seconds,
    )
    .await
}

pub async fn process_next_earn_reconciliation_job_with_policy_monitor(
    store: &OrchestratorStore,
    consumer_name: &str,
    claim_owner: &str,
    chain: &dyn EarnChainReader,
    policy_monitor: Option<&Mutex<PolicyMonitor<PostgresPolicyMatchSink>>>,
    lease_seconds: i64,
    retry_after_seconds: i64,
) -> Result<EarnReconciliationProcessOutcome> {
    let Some(job) = store
        .claim_earn_reconciliation_job(consumer_name, claim_owner, lease_seconds)
        .await?
    else {
        return Ok(EarnReconciliationProcessOutcome::Idle);
    };
    let retry_after_seconds = retry_after_seconds.saturating_mul(
        1_i64 << u32::try_from(job.attempt_count.saturating_sub(1).min(5)).unwrap_or(5),
    );
    let decoded = serde_json::from_value(job.event_payload.clone())
        .context("decode durable Earn event")
        .and_then(|update| {
            serde_json::from_value(job.vault_payload.clone())
                .context("decode durable Earn vault")
                .map(|vault| (update, vault))
        });
    let (update, vault): (NormalizedEarnUpdate, EarnVaultWatch) = match decoded {
        Ok(decoded) => decoded,
        Err(error) => {
            return defer_earn_reconciliation_job(
                store,
                job.id,
                job.attempt_count,
                claim_owner,
                error,
                retry_after_seconds,
            )
            .await;
        }
    };
    let policy_reconciled =
        match reconcile_targeted_policy_vault_update(store, chain, policy_monitor, &update, &vault)
            .await
        {
            Ok(reconciled) => reconciled,
            Err(error) => {
                return defer_earn_reconciliation_job(
                    store,
                    job.id,
                    job.attempt_count,
                    claim_owner,
                    error,
                    retry_after_seconds,
                )
                .await;
            }
        };
    let mutation = if policy_reconciled {
        Ok(EarnDirectMutation::Noop)
    } else {
        chain.mutation_for(&update, &vault).await
    };
    match mutation {
        Ok(mutation) => match store
            .complete_earn_reconciliation_job(job.id, claim_owner, &mutation)
            .await
        {
            Ok(outcome) => Ok(EarnReconciliationProcessOutcome::Completed {
                job_id: job.id,
                applied: outcome.applied_mutations,
            }),
            Err(error) => {
                defer_earn_reconciliation_job(
                    store,
                    job.id,
                    job.attempt_count,
                    claim_owner,
                    error.into(),
                    retry_after_seconds,
                )
                .await
            }
        },
        Err(error) => {
            defer_earn_reconciliation_job(
                store,
                job.id,
                job.attempt_count,
                claim_owner,
                error,
                retry_after_seconds,
            )
            .await
        }
    }
}

async fn defer_earn_reconciliation_job(
    store: &OrchestratorStore,
    job_id: i64,
    attempt_count: i32,
    claim_owner: &str,
    error: anyhow::Error,
    retry_after_seconds: i64,
) -> Result<EarnReconciliationProcessOutcome> {
    let error = format!("{error:#}");
    store
        .retry_earn_reconciliation_job(job_id, claim_owner, &error, retry_after_seconds)
        .await?;
    Ok(EarnReconciliationProcessOutcome::Deferred {
        job_id,
        attempt_count,
        error,
    })
}

pub async fn run_earn_reconciliation_consumer(
    store: OrchestratorStore,
    consumer_name: String,
    claim_owner: String,
    chain: Arc<dyn EarnChainReader>,
    policy_monitor: Arc<Mutex<PolicyMonitor<PostgresPolicyMatchSink>>>,
    wake: Arc<Notify>,
    running: Arc<AtomicBool>,
    metrics: EarnMonitorMetrics,
) {
    let mut next_health_sample_at = time::Instant::now();
    while running.load(Ordering::Relaxed) {
        let now = time::Instant::now();
        if now >= next_health_sample_at {
            match store
                .load_earn_reconciliation_health_snapshot(&consumer_name)
                .await
            {
                Ok(snapshot) => metrics.record(&snapshot),
                Err(error) => {
                    tracing::error!(
                        error = %error,
                        "failed to load Earn reconciliation health snapshot"
                    );
                    emit_earn_reconciliation_health_snapshot_failed();
                }
            }
            next_health_sample_at = now + EARN_RECONCILIATION_HEALTH_SAMPLE_INTERVAL;
        }

        match process_next_earn_reconciliation_job_with_policy_monitor(
            &store,
            &consumer_name,
            &claim_owner,
            chain.as_ref(),
            Some(policy_monitor.as_ref()),
            120,
            15,
        )
        .await
        {
            Ok(EarnReconciliationProcessOutcome::Completed { job_id, applied }) => {
                tracing::info!(job_id, applied, "completed durable Earn reconciliation job");
            }
            Ok(EarnReconciliationProcessOutcome::Deferred {
                job_id,
                attempt_count,
                error,
            }) => {
                if should_emit_reconciliation_retry_alert(attempt_count) {
                    tracing::error!(
                        job_id,
                        attempt_count,
                        error,
                        "Earn reconciliation proof failed; job retained for retry"
                    );
                    emit_earn_reconciliation_job_failed();
                } else {
                    tracing::warn!(
                        job_id,
                        attempt_count,
                        error,
                        "Earn reconciliation proof is still pending"
                    );
                }
            }
            Ok(EarnReconciliationProcessOutcome::Idle) => {
                tokio::select! {
                    _ = wake.notified() => {}
                    _ = time::sleep(Duration::from_secs(1)) => {}
                }
            }
            Err(error) => {
                tracing::error!(error = %error, "durable Earn reconciliation consumer failed");
                emit_earn_reconciliation_consumer_failed();
                time::sleep(Duration::from_secs(1)).await;
            }
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum AutodepositReconciliationProcessOutcome {
    Idle,
    AwaitingSetup {
        target_id: loyal_yield_store::BalanceSweepTargetId,
        requested_slot: u64,
    },
    Completed {
        target_id: loyal_yield_store::BalanceSweepTargetId,
        requested_slot: u64,
        observed_slot: u64,
        chain_status: String,
        still_pending: bool,
    },
    Deferred {
        target_id: loyal_yield_store::BalanceSweepTargetId,
        attempt_count: i32,
        error: String,
    },
}

pub async fn process_next_autodeposit_reconciliation_request(
    store: &OrchestratorStore,
    claim_owner: &str,
    chain: &RpcEarnChainReader,
    lease_seconds: i64,
    retry_after_seconds: i64,
) -> Result<AutodepositReconciliationProcessOutcome> {
    let Some(request) = store
        .claim_autodeposit_reconciliation_request(claim_owner, lease_seconds)
        .await?
    else {
        return Ok(AutodepositReconciliationProcessOutcome::Idle);
    };
    let retry_after_seconds = retry_after_seconds.saturating_mul(
        1_i64 << u32::try_from(request.attempt_count.saturating_sub(1).min(5)).unwrap_or(5),
    );
    let Some(context) = store
        .load_autodeposit_target_snapshot_context_by_id(request.target_id)
        .await?
    else {
        // Policy discovery can legitimately precede recurring-delegation
        // creation. Keep the durable request dormant until a later account
        // update advances its requested slot instead of treating an
        // incomplete user setup as a failed reconciliation job.
        store
            .await_autodeposit_setup_reconciliation_request(request.target_id, claim_owner, 3_600)
            .await?;
        return Ok(AutodepositReconciliationProcessOutcome::AwaitingSetup {
            target_id: request.target_id,
            requested_slot: request.requested_slot,
        });
    };
    let result = async {
        let observation = chain
            .autodeposit_snapshot(context, request.requested_slot)
            .await?;
        let observed_slot = observation.observation_slot;
        let projection = store
            .reconcile_autodeposit_chain_observation(observation)
            .await?;
        let still_pending = store
            .complete_autodeposit_reconciliation_request(
                request.target_id,
                claim_owner,
                observed_slot,
            )
            .await?;
        Ok::<_, anyhow::Error>((observed_slot, projection.chain_status, still_pending))
    }
    .await;
    match result {
        Ok((observed_slot, chain_status, still_pending)) => {
            Ok(AutodepositReconciliationProcessOutcome::Completed {
                target_id: request.target_id,
                requested_slot: request.requested_slot,
                observed_slot,
                chain_status,
                still_pending,
            })
        }
        Err(error) => {
            let error = format!("{error:#}");
            store
                .retry_autodeposit_reconciliation_request(
                    request.target_id,
                    claim_owner,
                    &error,
                    retry_after_seconds,
                )
                .await?;
            Ok(AutodepositReconciliationProcessOutcome::Deferred {
                target_id: request.target_id,
                attempt_count: request.attempt_count,
                error,
            })
        }
    }
}

pub async fn run_autodeposit_reconciliation_consumer(
    store: OrchestratorStore,
    claim_owner: String,
    chain: Arc<RpcEarnChainReader>,
    wake: Arc<Notify>,
    running: Arc<AtomicBool>,
) {
    while running.load(Ordering::Relaxed) {
        match process_next_autodeposit_reconciliation_request(
            &store,
            &claim_owner,
            chain.as_ref(),
            120,
            15,
        )
        .await
        {
            Ok(AutodepositReconciliationProcessOutcome::Completed {
                target_id,
                requested_slot,
                observed_slot,
                chain_status,
                still_pending,
            }) => {
                tracing::info!(
                    target_id = target_id.as_i64(),
                    requested_slot,
                    observed_slot,
                    chain_status,
                    still_pending,
                    "completed coalesced Autodeposit reconciliation"
                );
            }
            Ok(AutodepositReconciliationProcessOutcome::AwaitingSetup {
                target_id,
                requested_slot,
            }) => {
                tracing::info!(
                    target_id = target_id.as_i64(),
                    requested_slot,
                    "Autodeposit reconciliation is waiting for setup to complete"
                );
            }
            Ok(AutodepositReconciliationProcessOutcome::Deferred {
                target_id,
                attempt_count,
                error,
            }) => {
                if should_emit_reconciliation_retry_alert(attempt_count) {
                    tracing::error!(
                        target_id = target_id.as_i64(),
                        attempt_count,
                        error,
                        "Autodeposit reconciliation failed; request retained for retry"
                    );
                    emit_earn_reconciliation_job_failed();
                } else {
                    tracing::warn!(
                        target_id = target_id.as_i64(),
                        attempt_count,
                        error,
                        "Autodeposit reconciliation is still pending"
                    );
                }
            }
            Ok(AutodepositReconciliationProcessOutcome::Idle) => {
                tokio::select! {
                    _ = wake.notified() => {}
                    _ = time::sleep(Duration::from_secs(1)) => {}
                }
            }
            Err(error) => {
                tracing::error!(error = %error, "Autodeposit reconciliation consumer failed");
                emit_earn_reconciliation_consumer_failed();
                time::sleep(Duration::from_secs(1)).await;
            }
        }
    }
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
    fn policy_transaction_for<'a>(
        &'a self,
        update: &'a NormalizedEarnUpdate,
    ) -> Pin<Box<dyn Future<Output = Result<Option<EarnPolicyTransactionRead>>> + Send + 'a>> {
        Box::pin(async move {
            let Some(signature) = update.signature.as_deref() else {
                return Ok(None);
            };
            let Some(evidence) = self.signatures.get(signature) else {
                return Ok(None);
            };
            if evidence.slot != update.slot {
                bail!(
                    "fixture evidence slot {} does not match update slot {}",
                    evidence.slot,
                    update.slot
                );
            }
            Ok((evidence.kind == "failed_transaction")
                .then_some(EarnPolicyTransactionRead::NoStateChange))
        })
    }

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
                                source_commitment: "finalized".to_owned(),
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
        source_commitment: "finalized".to_owned(),
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
    use solana_sdk::{instruction::InstructionError, transaction::TransactionError};

    #[test]
    fn finalized_failed_policy_transaction_is_noop() {
        let error = TransactionError::InstructionError(2, InstructionError::Custom(15_001));

        assert_eq!(
            policy_transaction_disposition(Some(&error)),
            PolicyTransactionDisposition::NoStateChange
        );
        assert_eq!(
            policy_transaction_disposition(None),
            PolicyTransactionDisposition::Decode
        );
    }

    #[test]
    fn autodeposit_policy_discovery_event_key_coalesces_same_transaction() {
        let vault = EarnVaultWatch {
            environment: "mainnet-beta".to_owned(),
            settings: "settings".to_owned(),
            wallet: "wallet".to_owned(),
            vault: "vault".to_owned(),
            vault_index: 1,
            accounts: vec![crate::smart_account::EarnWatchAccount {
                pubkey: "smart-account".to_owned(),
                role: "smart_account".to_owned(),
            }],
        };
        let update = |account: &str, filter: &str| NormalizedEarnUpdate {
            event_key: None,
            filters: vec![filter.to_owned()],
            event_kind: "account".to_owned(),
            account_pubkey: Some(account.to_owned()),
            slot: 500,
            signature: Some("setup-signature".to_owned()),
        };

        let wallet_key = durable_earn_event_key(&update("wallet", EARN_WALLETS), &[&vault]);
        let settings_key =
            durable_earn_event_key(&update("settings", EARN_SMART_ACCOUNTS), &[&vault]);
        assert_eq!(wallet_key, settings_key);

        let obligation_key = durable_earn_event_key(
            &update("obligation", crate::smart_account::EARN_OBLIGATIONS),
            &[&vault],
        );
        assert_ne!(wallet_key, obligation_key);
    }

    fn token_balance(index: u64, owner: &str, mint: &str, amount: u64) -> Value {
        json!({
            "accountIndex": index,
            "owner": owner,
            "mint": mint,
            "uiTokenAmount": { "amount": amount.to_string() }
        })
    }

    fn cash_flow_transaction(wallet: &str, mint: &str, pre: u64, post: u64) -> Value {
        json!({
            "slot": 1,
            "meta": {
                "preTokenBalances": [token_balance(0, wallet, mint, pre)],
                "postTokenBalances": [token_balance(0, wallet, mint, post)],
                "preBalances": [1_000_000],
                "postBalances": [995_000],
                "fee": 5_000
            },
            "transaction": {
                "message": { "accountKeys": [wallet, "vault-owner"] }
            }
        })
    }

    fn test_vault(role: &str, account: &str) -> EarnVaultWatch {
        EarnVaultWatch {
            environment: "mainnet-beta".to_owned(),
            settings: "settings".to_owned(),
            wallet: "wallet-owner".to_owned(),
            vault: "vault-owner".to_owned(),
            vault_index: 1,
            accounts: vec![crate::smart_account::EarnWatchAccount {
                pubkey: account.to_owned(),
                role: role.to_owned(),
            }],
        }
    }

    fn test_update(kind: &str, account: &str, signature: &str, slot: u64) -> NormalizedEarnUpdate {
        NormalizedEarnUpdate {
            event_key: None,
            filters: vec!["earn".to_owned()],
            event_kind: kind.to_owned(),
            account_pubkey: Some(account.to_owned()),
            slot,
            signature: Some(signature.to_owned()),
        }
    }

    #[test]
    fn initial_deposit_without_onboarding() {
        let vault = test_vault("wallet_token", "wallet-ata");
        let update = test_update("account_updated", "wallet-ata", "deposit-1", 10);
        let evidence = classify_transaction_cash_flow(
            &cash_flow_transaction(&vault.wallet, "mint", 125, 25),
            &vault.wallet,
            ["mint"],
            &update,
            &vault,
        )
        .unwrap()
        .unwrap();
        assert_eq!(evidence.kind, CashFlowKind::Deposit);
        assert_eq!(evidence.amount_raw, 100);
    }

    #[test]
    fn top_up_without_onboarding() {
        let vault = test_vault("wallet_token", "wallet-ata");
        let update = test_update("account_updated", "wallet-ata", "deposit-2", 11);
        let evidence = classify_transaction_cash_flow(
            &cash_flow_transaction(&vault.wallet, "mint", 80, 50),
            &vault.wallet,
            ["mint"],
            &update,
            &vault,
        )
        .unwrap()
        .unwrap();
        assert_eq!(evidence.kind, CashFlowKind::Deposit);
        assert_eq!(evidence.amount_raw, 30);
    }

    #[test]
    fn partial_withdrawal_from_chain() {
        let vault = test_vault("wallet_token", "wallet-ata");
        let update = test_update("account_updated", "wallet-ata", "withdraw-1", 12);
        let evidence = classify_transaction_cash_flow(
            &cash_flow_transaction(&vault.wallet, "mint", 10, 35),
            &vault.wallet,
            ["mint"],
            &update,
            &vault,
        )
        .unwrap()
        .unwrap();
        assert_eq!(evidence.kind, CashFlowKind::Withdrawal);
        assert_eq!(evidence.amount_raw, 25);
    }

    #[test]
    fn multi_step_full_withdrawal_from_chain() {
        let vault = test_vault("wallet_token", "wallet-ata");
        let first = test_update("account_updated", "wallet-ata", "withdraw-a", 20);
        let second = test_update("account_updated", "wallet-ata", "withdraw-b", 21);
        let first_evidence = classify_transaction_cash_flow(
            &cash_flow_transaction(&vault.wallet, "mint", 0, 60),
            &vault.wallet,
            ["mint"],
            &first,
            &vault,
        )
        .unwrap()
        .unwrap();
        let second_evidence = classify_transaction_cash_flow(
            &cash_flow_transaction(&vault.wallet, "mint", 60, 100),
            &vault.wallet,
            ["mint"],
            &second,
            &vault,
        )
        .unwrap()
        .unwrap();
        assert_eq!(first_evidence.amount_raw + second_evidence.amount_raw, 100);
        assert_eq!(second_evidence.kind, CashFlowKind::Withdrawal);
    }

    #[test]
    fn cleanup_without_seed_withdrawal() {
        let proof = CleanupProof {
            balances_zero: true,
            policies_closed: true,
        };
        assert!(proof.balances_zero && proof.policies_closed);
    }

    #[test]
    fn policy_refund_from_chain() {
        let vault = test_vault("policy", "policy-account");
        let update = test_update("account_deleted", "policy-account", "refund-policy", 30);
        let transaction = json!({
            "slot": 30,
            "meta": {
                "preTokenBalances": [], "postTokenBalances": [],
                "preBalances": [1_000_000], "postBalances": [1_995_000], "fee": 5_000
            },
            "transaction": {
                "message": { "accountKeys": [vault.wallet] }
            }
        });
        let evidence =
            classify_transaction_cash_flow(&transaction, &vault.wallet, ["mint"], &update, &vault)
                .unwrap()
                .unwrap();
        assert_eq!(evidence.kind, CashFlowKind::Refund);
        assert_eq!(evidence.refund_kind.as_deref(), Some("policy"));
    }

    #[test]
    fn vault_refund_from_chain() {
        let vault = test_vault("idle_token", "vault-token-account");
        let update = test_update("account_deleted", "vault-token-account", "refund-vault", 31);
        let transaction = json!({
            "slot": 31,
            "meta": {
                "preTokenBalances": [], "postTokenBalances": [],
                "preBalances": [1_000_000], "postBalances": [2_995_000], "fee": 5_000
            },
            "transaction": {
                "message": { "accountKeys": [vault.wallet] }
            }
        });
        let evidence =
            classify_transaction_cash_flow(&transaction, &vault.wallet, ["mint"], &update, &vault)
                .unwrap()
                .unwrap();
        assert_eq!(evidence.refund_kind.as_deref(), Some("vault_token_account"));
    }

    #[test]
    fn replay_is_idempotent() {
        let vault = test_vault("wallet_token", "wallet-ata");
        let update = test_update("account_updated", "wallet-ata", "stable-signature", 40);
        let transaction = cash_flow_transaction(&vault.wallet, "mint", 50, 25);
        let first =
            classify_transaction_cash_flow(&transaction, &vault.wallet, ["mint"], &update, &vault)
                .unwrap();
        let replay =
            classify_transaction_cash_flow(&transaction, &vault.wallet, ["mint"], &update, &vault)
                .unwrap();
        assert_eq!(first, replay);
    }

    #[test]
    fn same_slot_siblings_all_complete() {
        let vault = test_vault("wallet_token", "wallet-ata");
        let first = test_update("account_updated", "account-a", "same-signature", 50);
        let second = test_update("account_updated", "account-b", "same-signature", 50);
        assert_ne!(
            durable_earn_event_key(&first, &[&vault]),
            durable_earn_event_key(&second, &[&vault])
        );
    }

    #[test]
    fn principal_debit_nets_same_owner_token_accounts() {
        let owner = "wallet-owner".to_owned();
        let mint = "deposit-mint";
        let transaction = json!({
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
        });

        assert_eq!(
            transaction_owner_debit(&transaction, mint, [&wallet, &vault]).unwrap(),
            100
        );
    }
}
