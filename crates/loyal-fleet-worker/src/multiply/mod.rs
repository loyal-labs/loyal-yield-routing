pub mod builder;
pub mod config;
pub mod executor;
pub mod observe;
pub mod planner;
pub mod policy;
pub mod view;

use builder::build_operation;
use chrono::{Duration as ChronoDuration, Utc};
use executor::{execute_operation, reconcile_operation, wait_confirmed, ExecutionContext};
use loyal_yield_store::{
    fleet_orchestration::{
        project_frontend, DepositEvidence, ExpectedEffects, MultiplyAction, MultiplyOperation,
        MultiplyOperationStatus, MultiplyPosition, ObligationBefore, RouteGoal, StrategyKey,
        TokenAmountBefore, TokenDelta, MULTIPLY_ENGINE_VERSION,
    },
    MultiplyPositionSnapshotInput, MultiplyRouteLease, NeonSqlClient, OrchestratorError,
};
use planner::{next_action, PlannerDecision};
use serde::Serialize;
use sha2::{Digest, Sha256};
use solana_client::nonblocking::rpc_client::RpcClient;
use solana_client::rpc_client::GetConfirmedSignaturesForAddress2Config;
use solana_client::rpc_config::{RpcSendTransactionConfig, RpcTransactionConfig};
use solana_sdk::{
    commitment_config::CommitmentConfig,
    pubkey::Pubkey,
    signature::{Keypair, Signature},
    transaction::VersionedTransaction,
};
use solana_transaction_status_client_types::{
    option_serializer::OptionSerializer, UiTransactionEncoding, UiTransactionTokenBalance,
};
use std::{error::Error, str::FromStr, time::Duration};
use tokio::sync::watch;

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct TickResult {
    pub route_key: Option<String>,
    pub condition: String,
    pub operation_id: Option<String>,
    pub signature: Option<String>,
}

struct ConfirmedTokenTransfer {
    signature: Signature,
    transaction: VersionedTransaction,
    slot: u64,
    source_pre: u64,
    source_post: u64,
    destination_pre: u64,
    destination_post: u64,
}

pub struct WorkerRuntime {
    pub store: NeonSqlClient,
    pub rpc: RpcClient,
    pub fee_payer: Keypair,
    pub delegate: Keypair,
    pub worker_id: String,
}

impl WorkerRuntime {
    pub async fn bootstrap_ready_route(&self) -> Result<Option<String>, Box<dyn Error>> {
        let Some(policy) = self.store.load_unbootstrapped_earn_max_policy_set().await? else {
            return Ok(None);
        };
        let settings = Pubkey::from_str(&policy.settings)?;
        let topology = config::derive_earn_max_topology_with_policy_seed_base(
            settings,
            policy.policy_seed_base,
        )?;
        if policy.vault_index != topology.vault_index || policy.vault != topology.vault.to_string()
        {
            return Err("ready policy projection drifted from deterministic topology".into());
        }
        let route_key = format!("earn-max:{}:{}", settings, topology.vault_index);
        let state = loyal_yield_store::fleet_orchestration::MultiplyRouteState::new(
            route_key.clone(),
            settings.to_string(),
            topology.vault_index,
            topology.vault.to_string(),
            policy.policy_seed_base,
            loyal_yield_store::fleet_orchestration::TokenBalance {
                account: topology.claim_custody.to_string(),
                mint: config::USDC_MINT.to_owned(),
                token_program: config::TOKEN.to_owned(),
                amount_raw: 0,
            },
            policy.observed_slot,
            Utc::now(),
        )?;
        if self
            .store
            .create_multiply_route_state(&route_key, &state)
            .await?
        {
            Ok(Some(route_key))
        } else {
            Ok(None)
        }
    }

    pub async fn admit_next_confirmed_deposit(&self) -> Result<Option<TickResult>, Box<dyn Error>> {
        let Some(route) = self.store.load_unadmitted_multiply_route_state().await? else {
            return Ok(None);
        };
        let topology = config::topology_for_route(&route.state)?;
        let Some((signature, wallet_account)) = self
            .find_confirmed_deposit(topology.claim_custody, route.state.observed_slot)
            .await?
        else {
            return Ok(None);
        };
        let result = self
            .admit_confirmed_deposit(
                &route.route_key,
                format!("chain:{signature}"),
                signature,
                wallet_account,
                StrategyKey::SyrupUsdcUsdc,
            )
            .await?;
        Ok(Some(result))
    }

    async fn find_confirmed_deposit(
        &self,
        vault_account: Pubkey,
        minimum_slot: u64,
    ) -> Result<Option<(Signature, Pubkey)>, Box<dyn Error>> {
        let signatures = self
            .rpc
            .get_signatures_for_address_with_config(
                &vault_account,
                GetConfirmedSignaturesForAddress2Config {
                    before: None,
                    until: None,
                    limit: Some(32),
                    commitment: Some(CommitmentConfig::confirmed()),
                },
            )
            .await?;
        for status in signatures {
            if status.slot <= minimum_slot || status.err.is_some() {
                continue;
            }
            let signature = Signature::from_str(&status.signature)?;
            let transaction = self
                .rpc
                .get_transaction_with_config(
                    &signature,
                    RpcTransactionConfig {
                        encoding: Some(UiTransactionEncoding::Base64),
                        commitment: Some(CommitmentConfig::confirmed()),
                        max_supported_transaction_version: Some(0),
                    },
                )
                .await?;
            let decoded = transaction
                .transaction
                .transaction
                .decode()
                .ok_or("deposit transaction bytes did not decode")?;
            let Some(meta) = transaction.transaction.meta.as_ref() else {
                continue;
            };
            if meta.err.is_some() {
                continue;
            }
            let keys = transaction_account_keys(&decoded, meta)?;
            let vault_index = account_index(&keys, vault_account)?;
            let Ok(vault_pre) =
                token_amount(&meta.pre_token_balances, vault_index, config::USDC_MINT)
            else {
                continue;
            };
            let Ok(vault_post) =
                token_amount(&meta.post_token_balances, vault_index, config::USDC_MINT)
            else {
                continue;
            };
            let Some(amount) = vault_post
                .checked_sub(vault_pre)
                .filter(|amount| *amount > 0)
            else {
                continue;
            };
            let pre_balances = match &meta.pre_token_balances {
                OptionSerializer::Some(values) => values,
                OptionSerializer::None | OptionSerializer::Skip => continue,
            };
            for pre in pre_balances {
                if pre.account_index == vault_index || pre.mint != config::USDC_MINT {
                    continue;
                }
                let Ok(post) = token_amount(
                    &meta.post_token_balances,
                    pre.account_index,
                    config::USDC_MINT,
                ) else {
                    continue;
                };
                let before = pre.ui_token_amount.amount.parse::<u64>()?;
                if before.checked_sub(post) == Some(amount) {
                    let source = *keys
                        .get(usize::from(pre.account_index))
                        .ok_or("deposit source account index is outside the transaction")?;
                    return Ok(Some((signature, source)));
                }
            }
        }
        Ok(None)
    }

    pub async fn admit_next_confirmed_claim(&self) -> Result<Option<TickResult>, Box<dyn Error>> {
        let Some(route) = self.store.load_claimable_multiply_route_state().await? else {
            return Ok(None);
        };
        let topology = config::topology_for_route(&route.state)?;
        let withdrawal = route
            .state
            .withdrawal
            .as_ref()
            .ok_or("claimable route omitted withdrawal")?;
        let destination = Pubkey::from_str(&withdrawal.destination_account)?;
        let signatures = self
            .rpc
            .get_signatures_for_address_with_config(
                &topology.claim_custody,
                GetConfirmedSignaturesForAddress2Config {
                    before: None,
                    until: None,
                    limit: Some(32),
                    commitment: Some(CommitmentConfig::confirmed()),
                },
            )
            .await?;
        let mut transfer = None;
        for status in signatures {
            if status.slot < route.state.observed_slot || status.err.is_some() {
                continue;
            }
            let signature = Signature::from_str(&status.signature)?;
            if let Some(candidate) = self
                .confirmed_token_transfer(
                    signature,
                    topology.claim_custody,
                    destination,
                    config::USDC_MINT,
                )
                .await?
            {
                if candidate.source_pre.saturating_sub(candidate.source_post)
                    == withdrawal.amount_raw
                {
                    transfer = Some(candidate);
                    break;
                }
            }
        }
        let Some(transfer) = transfer else {
            return Ok(None);
        };
        let mut lease = self
            .store
            .lease_multiply_route_state(
                &route.route_key,
                &self.worker_id,
                Utc::now() + ChronoDuration::seconds(30),
            )
            .await?
            .ok_or("claimable route is already leased")?;
        let stored = self
            .store
            .load_multiply_route_state(&route.route_key)
            .await?
            .ok_or("claimable route disappeared")?;
        let mut next = stored.state;
        let withdrawal = next
            .withdrawal
            .as_mut()
            .ok_or("claimable route omitted withdrawal")?;
        if withdrawal.status != loyal_yield_store::fleet_orchestration::WithdrawalStatus::Claimable
            || withdrawal.amount_raw != transfer.source_pre.saturating_sub(transfer.source_post)
        {
            let _ = self.store.release_multiply_route_lease(&lease).await;
            return Err("claimable route changed before claim admission".into());
        }
        withdrawal.status = loyal_yield_store::fleet_orchestration::WithdrawalStatus::Claimed;
        withdrawal.claim_signature = Some(transfer.signature.to_string());
        next.generation += 1;
        next.goal = RouteGoal::Claimed;
        next.position = MultiplyPosition::Idle {
            claim: loyal_yield_store::fleet_orchestration::TokenBalance {
                account: topology.claim_custody.to_string(),
                mint: config::USDC_MINT.to_owned(),
                token_program: config::TOKEN.to_owned(),
                amount_raw: transfer.source_post,
            },
        };
        next.observed_slot = transfer.slot;
        next.observed_at = Utc::now();
        next.frontend = project_frontend(&next);
        let amount = transfer.source_pre - transfer.source_post;
        let amount_delta = i64::try_from(amount).map_err(|_| "claim amount exceeds i64")?;
        let wire = bincode::serialize(&transfer.transaction)?;
        let message = bincode::serialize(&transfer.transaction.message)?;
        let evidence = serde_json::json!({
            "signature": transfer.signature.to_string(),
            "slot": transfer.slot,
            "sourcePre": transfer.source_pre,
            "sourcePost": transfer.source_post,
            "destinationPre": transfer.destination_pre,
            "destinationPost": transfer.destination_post,
        });
        let operation = MultiplyOperation {
            operation_id: format!("claim-{}", &hash_bytes(transfer.signature.as_ref())[..32]),
            route_key: next.route_key.clone(),
            cycle: next.cycle,
            engine_version: MULTIPLY_ENGINE_VERSION.to_owned(),
            action: MultiplyAction::Claim,
            strategy_key: None,
            status: MultiplyOperationStatus::Reconciled,
            idempotency_key: format!("{MULTIPLY_ENGINE_VERSION}:claim:{}", transfer.signature),
            expected_effects: ExpectedEffects {
                token_amounts_before: vec![
                    TokenAmountBefore {
                        account: topology.claim_custody.to_string(),
                        mint: config::USDC_MINT.to_owned(),
                        amount_raw: transfer.source_pre,
                    },
                    TokenAmountBefore {
                        account: destination.to_string(),
                        mint: config::USDC_MINT.to_owned(),
                        amount_raw: transfer.destination_pre,
                    },
                ],
                token_deltas: vec![
                    TokenDelta {
                        account: topology.claim_custody.to_string(),
                        mint: config::USDC_MINT.to_owned(),
                        raw_delta: -amount_delta,
                    },
                    TokenDelta {
                        account: destination.to_string(),
                        mint: config::USDC_MINT.to_owned(),
                        raw_delta: amount_delta,
                    },
                ],
                obligation_before: None,
                obligation_delta: None,
            },
            policy_account: None,
            policy_data_sha256: None,
            message_sha256: Some(hash_bytes(&message)),
            signed_wire: None,
            signed_wire_sha256: Some(hash_bytes(&wire)),
            transaction_signature: Some(transfer.signature.to_string()),
            recent_blockhash: Some(transfer.transaction.message.recent_blockhash().to_string()),
            last_valid_block_height: None,
            broadcast_intent_at: None,
            confirmed_slot: Some(transfer.slot),
            reconciliation_sha256: Some(hash_bytes(&serde_json::to_vec(&evidence)?)),
            created_at: Utc::now(),
            updated_at: Utc::now(),
        };
        if !self
            .store
            .admit_external_multiply_operation(&mut lease, &next, &operation)
            .await?
        {
            return Err(
                "claim transaction was already admitted or its route lease was lost".into(),
            );
        }
        if !self.store.release_multiply_route_lease(&lease).await? {
            return Err("claim admission lost its lease before release".into());
        }
        Ok(Some(TickResult {
            route_key: Some(next.route_key),
            condition: "confirmed_claim_admitted".to_owned(),
            operation_id: Some(operation.operation_id),
            signature: operation.transaction_signature,
        }))
    }

    async fn confirmed_token_transfer(
        &self,
        signature: Signature,
        source: Pubkey,
        destination: Pubkey,
        mint: &str,
    ) -> Result<Option<ConfirmedTokenTransfer>, Box<dyn Error>> {
        let transaction = self
            .rpc
            .get_transaction_with_config(
                &signature,
                RpcTransactionConfig {
                    encoding: Some(UiTransactionEncoding::Base64),
                    commitment: Some(CommitmentConfig::confirmed()),
                    max_supported_transaction_version: Some(0),
                },
            )
            .await?;
        let decoded = transaction
            .transaction
            .transaction
            .decode()
            .ok_or("claim transaction bytes did not decode")?;
        let Some(meta) = transaction.transaction.meta.as_ref() else {
            return Ok(None);
        };
        if meta.err.is_some() {
            return Ok(None);
        }
        let keys = transaction_account_keys(&decoded, meta)?;
        let Ok(source_index) = account_index(&keys, source) else {
            return Ok(None);
        };
        let Ok(destination_index) = account_index(&keys, destination) else {
            return Ok(None);
        };
        let source_pre = token_amount(&meta.pre_token_balances, source_index, mint)?;
        let source_post = token_amount(&meta.post_token_balances, source_index, mint)?;
        let destination_pre =
            token_amount(&meta.pre_token_balances, destination_index, mint).unwrap_or(0);
        let destination_post = token_amount(&meta.post_token_balances, destination_index, mint)?;
        let source_delta = source_pre.saturating_sub(source_post);
        if source_delta == 0 || destination_post.saturating_sub(destination_pre) != source_delta {
            return Ok(None);
        }
        Ok(Some(ConfirmedTokenTransfer {
            signature,
            transaction: decoded,
            slot: transaction.slot,
            source_pre,
            source_post,
            destination_pre,
            destination_post,
        }))
    }

    pub async fn admit_confirmed_deposit(
        &self,
        route_key: &str,
        request_id: String,
        signature: Signature,
        wallet_account: Pubkey,
        target: StrategyKey,
    ) -> Result<TickResult, Box<dyn Error>> {
        let route = self
            .store
            .load_multiply_route_state(route_key)
            .await?
            .ok_or("route not found")?;
        let topology = config::topology_for_route(&route.state)?;
        let transaction = self
            .rpc
            .get_transaction_with_config(
                &signature,
                RpcTransactionConfig {
                    encoding: Some(UiTransactionEncoding::Base64),
                    commitment: Some(CommitmentConfig::confirmed()),
                    max_supported_transaction_version: Some(0),
                },
            )
            .await?;
        let decoded = transaction
            .transaction
            .transaction
            .decode()
            .ok_or("deposit transaction bytes did not decode")?;
        if decoded.signatures.first() != Some(&signature) {
            return Err("deposit transaction signature drifted".into());
        }
        let meta = transaction
            .transaction
            .meta
            .as_ref()
            .ok_or("deposit transaction omitted metadata")?;
        if let Some(error) = &meta.err {
            return Err(format!("deposit transaction failed: {error:?}").into());
        }
        let keys = transaction_account_keys(&decoded, meta)?;
        let wallet_index = account_index(&keys, wallet_account)?;
        let vault_index = account_index(&keys, topology.claim_custody)?;
        let wallet_pre = token_amount(&meta.pre_token_balances, wallet_index, config::USDC_MINT)?;
        let wallet_post = token_amount(&meta.post_token_balances, wallet_index, config::USDC_MINT)?;
        let vault_pre = token_amount(&meta.pre_token_balances, vault_index, config::USDC_MINT)?;
        let vault_post = token_amount(&meta.post_token_balances, vault_index, config::USDC_MINT)?;
        let amount = wallet_pre
            .checked_sub(wallet_post)
            .ok_or("deposit wallet did not debit")?;
        if amount == 0 || vault_post.checked_sub(vault_pre) != Some(amount) {
            return Err("deposit did not produce equal wallet and vault deltas".into());
        }
        let mut lease = self
            .store
            .lease_multiply_route_state(
                route_key,
                &self.worker_id,
                Utc::now() + ChronoDuration::seconds(30),
            )
            .await?
            .ok_or("route is already leased")?;
        let stored = self
            .store
            .load_multiply_route_state(route_key)
            .await?
            .ok_or("route not found")?;
        let now = Utc::now();
        let mut state = stored.state;
        state.position = MultiplyPosition::Idle {
            claim: loyal_yield_store::fleet_orchestration::TokenBalance {
                account: topology.claim_custody.to_string(),
                mint: config::USDC_MINT.to_owned(),
                token_program: config::TOKEN.to_owned(),
                amount_raw: vault_post,
            },
        };
        state.observed_slot = transaction.slot;
        state.observed_at = now;
        let evidence = DepositEvidence {
            request_id,
            transaction_signature: signature.to_string(),
            wallet_account: wallet_account.to_string(),
            wallet_pre_amount_raw: wallet_pre,
            wallet_post_amount_raw: wallet_post,
            vault_pre_amount_raw: vault_pre,
            vault_post_amount_raw: vault_post,
            amount_raw: amount,
            observed_slot: transaction.slot,
            observed_at: now,
        };
        state = state.admit_deposit(evidence.clone(), target)?;
        let wire = bincode::serialize(&decoded)?;
        let message = bincode::serialize(&decoded.message)?;
        let reconciliation_sha256 = hash_bytes(&serde_json::to_vec(&evidence)?);
        let amount_delta = i64::try_from(amount).map_err(|_| "deposit amount exceeds i64")?;
        let operation = MultiplyOperation {
            operation_id: format!("deposit-{}", &hash_bytes(signature.as_ref())[..32]),
            route_key: route_key.to_owned(),
            cycle: state.cycle,
            engine_version: MULTIPLY_ENGINE_VERSION.to_owned(),
            action: MultiplyAction::DepositClaimAsset,
            strategy_key: Some(target),
            status: MultiplyOperationStatus::Reconciled,
            idempotency_key: format!("{MULTIPLY_ENGINE_VERSION}:deposit:{signature}"),
            expected_effects: ExpectedEffects {
                token_amounts_before: vec![
                    TokenAmountBefore {
                        account: wallet_account.to_string(),
                        mint: config::USDC_MINT.to_owned(),
                        amount_raw: wallet_pre,
                    },
                    TokenAmountBefore {
                        account: topology.claim_custody.to_string(),
                        mint: config::USDC_MINT.to_owned(),
                        amount_raw: vault_pre,
                    },
                ],
                token_deltas: vec![
                    TokenDelta {
                        account: wallet_account.to_string(),
                        mint: config::USDC_MINT.to_owned(),
                        raw_delta: -amount_delta,
                    },
                    TokenDelta {
                        account: topology.claim_custody.to_string(),
                        mint: config::USDC_MINT.to_owned(),
                        raw_delta: amount_delta,
                    },
                ],
                obligation_before: None,
                obligation_delta: None,
            },
            policy_account: None,
            policy_data_sha256: None,
            message_sha256: Some(hash_bytes(&message)),
            signed_wire: None,
            signed_wire_sha256: Some(hash_bytes(&wire)),
            transaction_signature: Some(signature.to_string()),
            recent_blockhash: Some(decoded.message.recent_blockhash().to_string()),
            last_valid_block_height: None,
            broadcast_intent_at: None,
            confirmed_slot: Some(transaction.slot),
            reconciliation_sha256: Some(reconciliation_sha256),
            created_at: now,
            updated_at: now,
        };
        if !self
            .store
            .admit_external_multiply_operation(&mut lease, &state, &operation)
            .await?
        {
            return Err(
                "deposit transaction was already admitted or its route lease was lost".into(),
            );
        }
        if !self.store.release_multiply_route_lease(&lease).await? {
            return Err("deposit admission lost its lease before release".into());
        }
        Ok(TickResult {
            route_key: Some(route_key.to_owned()),
            condition: "confirmed_deposit_admitted".to_owned(),
            operation_id: None,
            signature: Some(signature.to_string()),
        })
    }

    pub async fn request_withdrawal(
        &self,
        route_key: &str,
        request_id: String,
        destination_account: String,
        amount_raw: u64,
    ) -> Result<TickResult, Box<dyn Error>> {
        let mut lease = self
            .store
            .lease_multiply_route_state(
                route_key,
                &self.worker_id,
                Utc::now() + ChronoDuration::seconds(30),
            )
            .await?
            .ok_or("route is already leased")?;
        let stored = self
            .store
            .load_multiply_route_state(route_key)
            .await?
            .ok_or("route not found")?;
        if stored
            .state
            .withdrawal_matches(&request_id, &destination_account, amount_raw)
        {
            if !self.store.release_multiply_route_lease(&lease).await? {
                return Err("idempotent withdrawal request lost its lease before release".into());
            }
            return Ok(TickResult {
                route_key: Some(route_key.to_owned()),
                condition: "withdrawal_already_requested".to_owned(),
                operation_id: stored.state.current_operation_id,
                signature: stored
                    .state
                    .withdrawal
                    .and_then(|withdrawal| withdrawal.claim_signature),
            });
        }
        let state = stored.state.request_withdrawal(
            request_id,
            destination_account,
            amount_raw,
            Utc::now(),
        )?;
        if !self
            .store
            .save_multiply_route_state(&mut lease, &state)
            .await?
        {
            return Err("withdrawal request CAS lost its lease".into());
        }
        if !self.store.release_multiply_route_lease(&lease).await? {
            return Err("withdrawal request lost its lease before release".into());
        }
        Ok(TickResult {
            route_key: Some(route_key.to_owned()),
            condition: "withdrawal_requested".to_owned(),
            operation_id: None,
            signature: None,
        })
    }

    pub async fn request_move(
        &self,
        route_key: &str,
        target: StrategyKey,
    ) -> Result<TickResult, Box<dyn Error>> {
        let mut lease = self
            .store
            .lease_multiply_route_state(
                route_key,
                &self.worker_id,
                Utc::now() + ChronoDuration::seconds(30),
            )
            .await?
            .ok_or("route is already leased")?;
        let stored = self
            .store
            .load_multiply_route_state(route_key)
            .await?
            .ok_or("route not found")?;
        let state = stored.state.request_move(target)?;
        if !self
            .store
            .save_multiply_route_state(&mut lease, &state)
            .await?
        {
            return Err("move request CAS lost its lease".into());
        }
        if !self.store.release_multiply_route_lease(&lease).await? {
            return Err("move request lost its lease before release".into());
        }
        Ok(TickResult {
            route_key: Some(route_key.to_owned()),
            condition: "move_requested".to_owned(),
            operation_id: None,
            signature: None,
        })
    }

    pub async fn tick(&self, route_key: Option<&str>) -> Result<TickResult, Box<dyn Error>> {
        let expiry = Utc::now() + ChronoDuration::seconds(30);
        let mut lease = match route_key {
            Some(key) => {
                self.store
                    .lease_multiply_route_state(key, &self.worker_id, expiry)
                    .await?
            }
            None => {
                self.store
                    .lease_next_multiply_route_state(&self.worker_id, expiry)
                    .await?
            }
        };
        let Some(mut lease) = lease.take() else {
            return Ok(TickResult {
                route_key: route_key.map(ToOwned::to_owned),
                condition: "no_route_available".to_owned(),
                operation_id: None,
                signature: None,
            });
        };
        let result = self.tick_leased(&mut lease).await;
        let release = self.store.release_multiply_route_lease(&lease).await;
        match (result, release) {
            (Ok(value), Ok(true)) => Ok(value),
            (Ok(_), Ok(false)) => Err("route lease was lost before release".into()),
            (Ok(_), Err(error)) => Err(error.into()),
            (Err(error), Ok(_)) => Err(error),
            (Err(error), Err(_)) => Err(error),
        }
    }

    async fn tick_leased(
        &self,
        lease: &mut MultiplyRouteLease,
    ) -> Result<TickResult, Box<dyn Error>> {
        let stored = self
            .store
            .load_multiply_route_state(&lease.route_key)
            .await?
            .ok_or("leased route disappeared")?;
        if stored.version != lease.version || stored.fencing_token != lease.fencing_token {
            return Err("leased route version or fencing token drifted".into());
        }
        let topology = config::topology_for_route(&stored.state)?;
        if let Some(operation) = stored.current_operation {
            return self.recover(lease, &stored.state, operation).await;
        }
        if !self
            .store
            .earn_max_policy_set_ready(
                &stored.settings,
                stored.state.vault_index,
                stored.state.policy_seed_base,
            )
            .await?
        {
            return Ok(TickResult {
                route_key: Some(lease.route_key.clone()),
                condition: "earn_max_policy_set_not_ready".to_owned(),
                operation_id: None,
                signature: None,
            });
        }
        let extra = stored
            .state
            .withdrawal
            .as_ref()
            .map(|withdrawal| {
                vec![(
                    withdrawal.destination_account.as_str(),
                    config::USDC_MINT,
                    config::TOKEN,
                )]
            })
            .unwrap_or_default();
        let observed = observe::observe_confirmed_with_extra(&self.rpc, topology, &extra).await?;
        if !observed.active_strategy_is_coherent() {
            return Ok(TickResult {
                route_key: Some(lease.route_key.clone()),
                condition: "awaiting_coherent_confirmed_observation".to_owned(),
                operation_id: None,
                signature: None,
            });
        }
        self.store
            .record_multiply_position_snapshot(&snapshot_input(&stored.state, &observed))
            .await?;
        match next_action(&stored.state, &observed, topology) {
            PlannerDecision::Complete => {
                let mut next = stored.state;
                if next.goal == RouteGoal::Withdraw
                    && next.withdrawal.as_ref().is_some_and(|withdrawal| {
                        withdrawal.status
                            != loyal_yield_store::fleet_orchestration::WithdrawalStatus::Claimable
                    })
                {
                    next.generation += 1;
                    if let Some(withdrawal) = &mut next.withdrawal {
                        withdrawal.status =
                            loyal_yield_store::fleet_orchestration::WithdrawalStatus::Claimable;
                        withdrawal.unwind_completed_at = Some(Utc::now());
                    }
                    next.frontend = project_frontend(&next);
                    if !self.store.save_multiply_route_state(lease, &next).await? {
                        return Err("claimable transition CAS lost its lease".into());
                    }
                } else if matches!(next.goal, RouteGoal::Deploy | RouteGoal::Move) {
                    next.generation += 1;
                    next.goal = RouteGoal::Idle;
                    next.frontend = project_frontend(&next);
                    if !self.store.save_multiply_route_state(lease, &next).await? {
                        return Err("route completion CAS lost its lease".into());
                    }
                }
                Ok(TickResult {
                    route_key: Some(lease.route_key.clone()),
                    condition: "route_complete".to_owned(),
                    operation_id: None,
                    signature: None,
                })
            }
            PlannerDecision::Resume(_) => unreachable!("current operation was handled above"),
            PlannerDecision::Execute(plan) => {
                let mut built = build_operation(&plan, &observed, topology).await?;
                bind_before(
                    &mut built.expected_effects,
                    &observed,
                    plan.strategy_key,
                    topology,
                )?;
                let now = Utc::now();
                let operation_id = operation_id(
                    &stored.state.route_key,
                    stored.state.cycle,
                    stored.state.generation,
                    plan.action.as_str(),
                );
                let operation = MultiplyOperation {
                    operation_id: operation_id.clone(),
                    route_key: stored.state.route_key.clone(),
                    cycle: stored.state.cycle,
                    engine_version: MULTIPLY_ENGINE_VERSION.to_owned(),
                    action: plan.action,
                    strategy_key: plan.strategy_key,
                    status: MultiplyOperationStatus::Prepared,
                    idempotency_key: format!(
                        "{}:{}:{}:{}",
                        MULTIPLY_ENGINE_VERSION,
                        stored.state.route_key,
                        stored.state.cycle,
                        stored.state.generation
                    ),
                    expected_effects: built.expected_effects.clone(),
                    policy_account: None,
                    policy_data_sha256: None,
                    message_sha256: None,
                    signed_wire: None,
                    signed_wire_sha256: None,
                    transaction_signature: None,
                    recent_blockhash: None,
                    last_valid_block_height: None,
                    broadcast_intent_at: None,
                    confirmed_slot: None,
                    reconciliation_sha256: None,
                    created_at: now,
                    updated_at: now,
                };
                let mut route = stored.state;
                route.generation += 1;
                route.current_operation_id = Some(operation_id.clone());
                route.observed_slot = observed.slot;
                route.observed_at = now;
                route.frontend = project_frontend(&route);
                if !self
                    .store
                    .prepare_multiply_operation(lease, &route, &operation)
                    .await?
                {
                    return Err("prepared operation lost its route lease or idempotency key".into());
                }
                let context = ExecutionContext {
                    store: &self.store,
                    rpc: &self.rpc,
                    fee_payer: &self.fee_payer,
                    delegate: &self.delegate,
                };
                let next = execute_operation(
                    &context, lease, &route, &operation, &plan, built, &observed, topology,
                )
                .await?;
                let completed = self
                    .store
                    .load_multiply_operation(&operation_id)
                    .await?
                    .ok_or("reconciled operation disappeared")?;
                Ok(TickResult {
                    route_key: Some(next.route_key),
                    condition: "operation_reconciled".to_owned(),
                    operation_id: Some(operation_id),
                    signature: completed.transaction_signature,
                })
            }
        }
    }

    async fn recover(
        &self,
        lease: &mut MultiplyRouteLease,
        route: &loyal_yield_store::fleet_orchestration::MultiplyRouteState,
        operation: MultiplyOperation,
    ) -> Result<TickResult, Box<dyn Error>> {
        let topology = config::topology_for_route(route)?;
        let context = ExecutionContext {
            store: &self.store,
            rpc: &self.rpc,
            fee_payer: &self.fee_payer,
            delegate: &self.delegate,
        };
        match operation.status {
            MultiplyOperationStatus::Prepared => {
                let mut next = route.clone();
                next.generation += 1;
                next.current_operation_id = None;
                next.frontend = project_frontend(&next);
                if !self
                    .store
                    .cancel_prepared_multiply_operation(lease, &operation.operation_id, &next)
                    .await?
                {
                    return Err("prepared operation cancellation lost its lease".into());
                }
                Ok(tick_result(route, &operation, "prepared_operation_rebuilt"))
            }
            MultiplyOperationStatus::SignedPersisted => {
                let transaction = persisted_transaction(&operation)?;
                if !self
                    .store
                    .mark_multiply_broadcast_intent(lease, &operation.operation_id, Utc::now())
                    .await?
                {
                    return Err("signed operation lost before broadcast intent".into());
                }
                let send = self
                    .rpc
                    .send_transaction_with_config(
                        &transaction,
                        RpcSendTransactionConfig {
                            skip_preflight: true,
                            preflight_commitment: Some(CommitmentConfig::confirmed().commitment),
                            max_retries: Some(0),
                            min_context_slot: Some(route.observed_slot),
                            encoding: None,
                        },
                    )
                    .await;
                if send.is_err() {
                    return Ok(tick_result(
                        route,
                        &operation,
                        "broadcast_result_ambiguous_signature_recovery_required",
                    ));
                }
                let signature = required_signature(&operation)?;
                let slot = wait_confirmed(&self.rpc, &signature).await?;
                if !self
                    .store
                    .mark_multiply_confirmed(lease, &operation.operation_id, slot)
                    .await?
                {
                    return Err("broadcast operation lost before confirmation persistence".into());
                }
                let confirmed = self
                    .store
                    .load_multiply_operation(&operation.operation_id)
                    .await?
                    .ok_or("confirmed operation disappeared")?;
                if let Err(error) =
                    reconcile_operation(&context, lease, route, &confirmed, slot, topology).await
                {
                    return self
                        .enter_manual_recovery(
                            lease,
                            route,
                            &confirmed,
                            format!("confirmed reconciliation failed: {error}"),
                        )
                        .await;
                }
                Ok(tick_result(
                    route,
                    &confirmed,
                    "recovered_operation_reconciled",
                ))
            }
            MultiplyOperationStatus::BroadcastIntent => {
                let signature = required_signature(&operation)?;
                let statuses = self
                    .rpc
                    .get_signature_statuses_with_history(&[signature])
                    .await?;
                if let Some(status) = statuses.value.into_iter().next().flatten() {
                    if let Some(error) = status.err {
                        return self
                            .enter_manual_recovery(
                                lease,
                                route,
                                &operation,
                                format!("broadcast transaction failed: {error:?}"),
                            )
                            .await;
                    }
                    if status.confirmation_status.is_some_and(|level| {
                        matches!(
                            level,
                            solana_transaction_status_client_types::TransactionConfirmationStatus::Confirmed
                                | solana_transaction_status_client_types::TransactionConfirmationStatus::Finalized
                        )
                    }) {
                        if !self
                            .store
                            .mark_multiply_confirmed(lease, &operation.operation_id, status.slot)
                            .await?
                        {
                            return Err(
                                "broadcast operation lost before confirmation persistence".into(),
                            );
                        }
                        let confirmed = self
                            .store
                            .load_multiply_operation(&operation.operation_id)
                            .await?
                            .ok_or("confirmed operation disappeared")?;
                        if let Err(error) = reconcile_operation(
                            &context,
                            lease,
                            route,
                            &confirmed,
                            status.slot,
                            topology,
                        )
                        .await
                        {
                            return self
                                .enter_manual_recovery(
                                    lease,
                                    route,
                                    &confirmed,
                                    format!("confirmed reconciliation failed: {error}"),
                                )
                                .await;
                        }
                        return Ok(tick_result(
                            route,
                            &confirmed,
                            "recovered_operation_reconciled",
                        ));
                    }
                }
                let expiry = operation
                    .last_valid_block_height
                    .ok_or("broadcast operation omitted blockhash expiry")?;
                if self.rpc.get_block_height().await? > expiry {
                    let mut next = route.clone();
                    next.generation += 1;
                    next.current_operation_id = None;
                    next.frontend = project_frontend(&next);
                    if !self
                        .store
                        .expire_multiply_operation(lease, &operation.operation_id, &next)
                        .await?
                    {
                        return Err("expired operation lost its lease".into());
                    }
                    return Ok(tick_result(
                        route,
                        &operation,
                        "absent_signature_expired_and_rebuild_ready",
                    ));
                }
                Ok(tick_result(route, &operation, "awaiting_stored_signature"))
            }
            MultiplyOperationStatus::Confirmed | MultiplyOperationStatus::ReconciliationPending => {
                let slot = operation
                    .confirmed_slot
                    .ok_or("confirmed operation omitted its slot")?;
                if let Err(error) =
                    reconcile_operation(&context, lease, route, &operation, slot, topology).await
                {
                    return self
                        .enter_manual_recovery(
                            lease,
                            route,
                            &operation,
                            format!("confirmed reconciliation failed: {error}"),
                        )
                        .await;
                }
                Ok(tick_result(
                    route,
                    &operation,
                    "confirmed_operation_reconciled",
                ))
            }
            MultiplyOperationStatus::Reconciled
            | MultiplyOperationStatus::Expired
            | MultiplyOperationStatus::ManualRecovery => {
                Err("route points at a terminal operation".into())
            }
        }
    }

    async fn enter_manual_recovery(
        &self,
        lease: &mut MultiplyRouteLease,
        route: &loyal_yield_store::fleet_orchestration::MultiplyRouteState,
        operation: &MultiplyOperation,
        reason: String,
    ) -> Result<TickResult, Box<dyn Error>> {
        let mut next = route.clone();
        next.generation += 1;
        next.goal = RouteGoal::ManualRecovery;
        next.current_operation_id = None;
        next.manual_recovery_reason = Some(reason);
        next.frontend = project_frontend(&next);
        if !self
            .store
            .mark_multiply_manual_recovery(lease, &operation.operation_id, &next)
            .await?
        {
            return Err("manual recovery transition lost its lease".into());
        }
        Ok(tick_result(route, operation, "manual_recovery_required"))
    }
}

pub async fn run(runtime: &WorkerRuntime, route_key: Option<&str>) -> Result<(), Box<dyn Error>> {
    let mut shutdown = shutdown_signal()?;
    loop {
        if *shutdown.borrow() {
            println!(
                "{}",
                serde_json::json!({"condition":"multiply_worker_drained"})
            );
            return Ok(());
        }
        if route_key.is_none() {
            match runtime.bootstrap_ready_route().await {
                Ok(Some(route_key)) => println!(
                    "{}",
                    serde_json::json!({"condition":"earn_max_route_bootstrapped","routeKey":route_key})
                ),
                Ok(None) => {}
                Err(error) => eprintln!(
                    "{}",
                    serde_json::json!({"condition":"earn_max_route_bootstrap_failed","error":safe_error(error.as_ref())})
                ),
            }
            match runtime.admit_next_confirmed_deposit().await {
                Ok(Some(result)) => println!("{}", serde_json::to_string(&result)?),
                Ok(None) => {}
                Err(error) => eprintln!(
                    "{}",
                    serde_json::json!({"condition":"earn_max_deposit_observation_failed","error":safe_error(error.as_ref())})
                ),
            }
            match runtime.admit_next_confirmed_claim().await {
                Ok(Some(result)) => println!("{}", serde_json::to_string(&result)?),
                Ok(None) => {}
                Err(error) => eprintln!(
                    "{}",
                    serde_json::json!({"condition":"earn_max_claim_observation_failed","error":safe_error(error.as_ref())})
                ),
            }
        }
        match runtime.tick(route_key).await {
            Ok(result) => println!("{}", serde_json::to_string(&result)?),
            Err(error) => eprintln!(
                "{}",
                serde_json::json!({"condition":"multiply_tick_failed","error":safe_error(error.as_ref())})
            ),
        }
        if *shutdown.borrow() {
            println!(
                "{}",
                serde_json::json!({"condition":"multiply_worker_drained"})
            );
            return Ok(());
        }
        tokio::select! {
            _ = tokio::time::sleep(Duration::from_millis(750)) => {}
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    println!("{}", serde_json::json!({"condition":"multiply_worker_drained"}));
                    return Ok(());
                }
            }
        }
    }
}

fn shutdown_signal() -> Result<watch::Receiver<bool>, Box<dyn Error>> {
    let (sender, receiver) = watch::channel(false);
    #[cfg(unix)]
    {
        let mut terminate =
            tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())?;
        tokio::spawn(async move {
            tokio::select! {
                _ = tokio::signal::ctrl_c() => {}
                _ = terminate.recv() => {}
            }
            let _ = sender.send(true);
        });
    }
    #[cfg(not(unix))]
    tokio::spawn(async move {
        let _ = tokio::signal::ctrl_c().await;
        let _ = sender.send(true);
    });
    Ok(receiver)
}

fn operation_id(route_key: &str, cycle: u64, generation: u64, action: &str) -> String {
    let mut hash = Sha256::new();
    hash.update(MULTIPLY_ENGINE_VERSION);
    hash.update(route_key);
    hash.update(cycle.to_le_bytes());
    hash.update(generation.to_le_bytes());
    hash.update(action);
    format!("mul-{}", &format!("{:x}", hash.finalize())[..32])
}

fn hash_bytes(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

fn snapshot_input(
    route: &loyal_yield_store::fleet_orchestration::MultiplyRouteState,
    observed: &observe::ObservedRoute,
) -> MultiplyPositionSnapshotInput {
    const FRACTION_ONE_SF: u128 = 1_u128 << 60;
    let active = observed
        .strategies
        .iter()
        .find(|value| value.collateral_deposited_raw > 0 || value.debt_raw > 0);
    let to_usd_micros = |value: u128| value.saturating_mul(1_000_000) / FRACTION_ONE_SF;
    let (
        strategy_key,
        collateral_raw,
        debt_raw,
        collateral_value,
        debt_value,
        leverage_bps,
        ltv_bps,
        health_factor_ppm,
    ) = match active {
        Some(position) => {
            let collateral_value = to_usd_micros(position.collateral_value_sf);
            let debt_value = to_usd_micros(position.debt_value_sf);
            let equity = u128::from(observed.claim.amount_raw)
                .saturating_add(collateral_value)
                .saturating_sub(debt_value);
            (
                Some(position.strategy_key.as_str().to_owned()),
                position.collateral_deposited_raw,
                position.debt_raw,
                collateral_value,
                debt_value,
                (equity > 0).then(|| {
                    u64::try_from(collateral_value.saturating_mul(10_000) / equity)
                        .unwrap_or(u64::MAX)
                }),
                (collateral_value > 0).then(|| {
                    u64::try_from(debt_value.saturating_mul(10_000) / collateral_value)
                        .unwrap_or(u64::MAX)
                }),
                (position.debt_value_sf > 0).then(|| {
                    u64::try_from(
                        position.unhealthy_value_sf.saturating_mul(1_000_000)
                            / position.debt_value_sf,
                    )
                    .unwrap_or(u64::MAX)
                }),
            )
        }
        None => (None, 0, 0, 0, 0, None, None, None),
    };
    let equity = if active.is_some() {
        u128::from(observed.claim.amount_raw)
            .saturating_add(collateral_value)
            .saturating_sub(debt_value)
    } else {
        u128::from(observed.claim.amount_raw)
    };
    let (supply_apy_bps, borrow_apy_bps, forecast_apy_bps) = match active {
        Some(position) => {
            let supply = position.collateral_supply_apy_bps;
            let borrow = position.debt_borrow_apy_bps;
            let forecast = (equity > 0).then(|| {
                let income = i128::try_from(collateral_value)
                    .unwrap_or(i128::MAX)
                    .saturating_mul(i128::from(supply));
                let cost = i128::try_from(debt_value)
                    .unwrap_or(i128::MAX)
                    .saturating_mul(i128::from(borrow));
                let net = income.saturating_sub(cost) / i128::try_from(equity).unwrap_or(i128::MAX);
                i64::try_from(net).unwrap_or(if net.is_negative() {
                    i64::MIN
                } else {
                    i64::MAX
                })
            });
            (Some(supply), Some(borrow), forecast)
        }
        None => (None, None, None),
    };
    MultiplyPositionSnapshotInput {
        route_key: route.route_key.clone(),
        generation: route.generation,
        observed_slot: observed.slot,
        observed_at: Utc::now(),
        strategy_key,
        claim_raw: observed.claim.amount_raw,
        collateral_raw,
        debt_raw,
        equity_usd_micros: Some(equity.to_string()),
        collateral_value_usd_micros: Some(collateral_value.to_string()),
        debt_value_usd_micros: Some(debt_value.to_string()),
        leverage_bps,
        ltv_bps,
        health_factor_ppm,
        supply_apy_bps,
        borrow_apy_bps,
        forecast_apy_bps,
        valuation_source: Some("confirmed_kamino_reserve_curve_500ms".to_owned()),
        valuation_slot: Some(observed.slot),
        valuation_observed_at: Some(Utc::now()),
        coverage_start_at: route
            .deposit
            .as_ref()
            .map(|deposit| deposit.observed_at)
            .or(Some(route.observed_at)),
    }
}

fn bind_before(
    effects: &mut loyal_yield_store::fleet_orchestration::ExpectedEffects,
    observed: &observe::ObservedRoute,
    strategy_key: Option<loyal_yield_store::fleet_orchestration::StrategyKey>,
    topology: config::EarnMaxTopology,
) -> Result<(), Box<dyn Error>> {
    effects.token_amounts_before = effects
        .token_deltas
        .iter()
        .map(|delta| {
            let balance = [
                &observed.claim,
                &observed.collateral_custody,
                &observed.source_debt_custody,
                &observed.target_debt_custody,
            ]
            .into_iter()
            .chain(observed.external_custody.iter())
            .find(|balance| balance.account == delta.account && balance.mint == delta.mint)
            .ok_or("expected token effect is not in the confirmed observation")?;
            Ok(TokenAmountBefore {
                account: balance.account.clone(),
                mint: balance.mint.clone(),
                amount_raw: balance.amount_raw,
            })
        })
        .collect::<Result<Vec<_>, Box<dyn Error>>>()?;
    effects.obligation_before = strategy_key.map(|key| {
        let position = observed.position(key);
        ObligationBefore {
            obligation: topology.strategy(key).obligation.to_string(),
            collateral_raw: position.collateral_deposited_raw,
            debt_raw: position.debt_raw,
            debt_amount_sf: position.debt_amount_sf.clone(),
        }
    });
    Ok(())
}

fn persisted_transaction(
    operation: &MultiplyOperation,
) -> Result<VersionedTransaction, Box<dyn Error>> {
    let wire = operation
        .signed_wire
        .as_deref()
        .ok_or("signed operation omitted persisted wire")?;
    let hash = format!("{:x}", Sha256::digest(wire));
    if operation.signed_wire_sha256.as_deref() != Some(hash.as_str()) {
        return Err("persisted wire hash drifted".into());
    }
    let transaction: VersionedTransaction = bincode::deserialize(wire)?;
    if transaction
        .signatures
        .first()
        .map(ToString::to_string)
        .as_deref()
        != operation.transaction_signature.as_deref()
        || transaction.message.recent_blockhash().to_string()
            != operation.recent_blockhash.as_deref().unwrap_or_default()
    {
        return Err("persisted transaction identity drifted".into());
    }
    Ok(transaction)
}

fn required_signature(operation: &MultiplyOperation) -> Result<Signature, Box<dyn Error>> {
    Signature::from_str(
        operation
            .transaction_signature
            .as_deref()
            .ok_or("operation omitted signature")?,
    )
    .map_err(Into::into)
}

fn tick_result(
    route: &loyal_yield_store::fleet_orchestration::MultiplyRouteState,
    operation: &MultiplyOperation,
    condition: &str,
) -> TickResult {
    TickResult {
        route_key: Some(route.route_key.clone()),
        condition: condition.to_owned(),
        operation_id: Some(operation.operation_id.clone()),
        signature: operation.transaction_signature.clone(),
    }
}

fn transaction_account_keys(
    transaction: &VersionedTransaction,
    meta: &solana_transaction_status_client_types::UiTransactionStatusMeta,
) -> Result<Vec<Pubkey>, Box<dyn Error>> {
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
                return Err("versioned deposit transaction omitted loaded addresses".into());
            }
        }
    }
    Ok(keys)
}

fn account_index(keys: &[Pubkey], expected: Pubkey) -> Result<u8, Box<dyn Error>> {
    keys.iter()
        .position(|key| key == &expected)
        .ok_or_else(|| "expected token account is absent from the deposit transaction".into())
        .and_then(|value| u8::try_from(value).map_err(Into::into))
}

fn token_amount(
    balances: &OptionSerializer<Vec<UiTransactionTokenBalance>>,
    account_index: u8,
    mint: &str,
) -> Result<u64, Box<dyn Error>> {
    let balances = match balances {
        OptionSerializer::Some(values) => values,
        OptionSerializer::None | OptionSerializer::Skip => {
            return Err("deposit transaction omitted token balances".into())
        }
    };
    let mut matches = balances
        .iter()
        .filter(|value| value.account_index == account_index && value.mint == mint);
    let value = matches.next().ok_or("deposit token balance is absent")?;
    if matches.next().is_some() || value.ui_token_amount.decimals != 6 {
        return Err("deposit token balance is ambiguous or has the wrong decimals".into());
    }
    Ok(value.ui_token_amount.amount.parse()?)
}

fn safe_error(error: &dyn Error) -> String {
    let message = error.to_string();
    if message.contains("postgres") || message.contains("http") || message.contains("keypair") {
        "external dependency failed; inspect terminal logs".to_owned()
    } else {
        message
    }
}

pub fn store_error(message: &str) -> OrchestratorError {
    OrchestratorError::StoreInvariant(message.to_owned())
}
