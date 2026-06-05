use crate::{
    planner::SameMintReserveTarget, route_builder::associated_token_address,
    ManagedVaultRoutePolicy, OrchestratorError, OrchestratorStore, PositionSnapshot,
    ReconciledReservePosition, ReconciledVaultState,
};
use serde_json::json;
use solana_client::rpc_client::RpcClient;
use solana_sdk::pubkey::Pubkey;
use std::str::FromStr;
use thiserror::Error;

#[derive(Debug, Error)]
pub enum ReconcileError {
    #[error("invalid pubkey in {field}: {value}")]
    InvalidPubkey { field: &'static str, value: String },
    #[error("RPC error: {0}")]
    Rpc(String),
    #[error("token amount {0} is not a valid integer")]
    InvalidTokenAmount(String),
    #[error(transparent)]
    Store(#[from] OrchestratorError),
}

pub struct RpcPositionReconciler<'a> {
    store: &'a OrchestratorStore,
    rpc: &'a RpcClient,
}

impl<'a> RpcPositionReconciler<'a> {
    pub fn new(store: &'a OrchestratorStore, rpc: &'a RpcClient) -> Self {
        Self { store, rpc }
    }

    pub async fn reconcile_vault(
        &self,
        vault_policy: &ManagedVaultRoutePolicy,
        targets: &[SameMintReserveTarget],
    ) -> Result<PositionSnapshot, ReconcileError> {
        let vault = parse_pubkey("vault.vault_pubkey", &vault_policy.vault.vault_pubkey)?;
        let observed_slot = self
            .rpc
            .get_slot()
            .map_err(|error| ReconcileError::Rpc(error.to_string()))?;
        let mut positions = Vec::with_capacity(targets.len());

        for target in targets {
            let token_program = target
                .accounts
                .liquidity_token_program
                .as_deref()
                .map(|value| parse_pubkey("accounts.liquidity_token_program", value))
                .transpose()?
                .unwrap_or(spl_token::ID);
            let collateral_mint = parse_pubkey(
                "accounts.reserve_collateral_mint",
                &target.accounts.reserve_collateral_mint,
            )?;
            let collateral_account =
                associated_token_address(vault, token_program, collateral_mint);
            let amount_raw = token_account_amount(self.rpc, collateral_account)?;

            positions.push(ReconciledReservePosition {
                reserve: target.reserve.clone(),
                market: Some(target.market.clone()),
                liquidity_mint: target.liquidity_mint.clone(),
                amount_raw,
                supply_apy_bps: Some(target.supply_apy_bps),
                borrow_apy_bps: None,
                planning_metadata: json!({
                    "source": "same_mint_rpc_reconciler",
                    "collateralAccount": collateral_account.to_string(),
                    "reserve": target,
                }),
            });
        }

        self.store
            .reconcile_vault(
                vault_policy.vault.id,
                ReconciledVaultState {
                    observed_slot: observed_slot as i64,
                    observed_at: None,
                    chain_slot: Some(observed_slot as i64),
                    lock_attempt_id: None,
                    context: json!({
                        "kind": "same_mint_rpc_reconcile",
                        "vault": vault.to_string(),
                        "targetCount": targets.len(),
                    }),
                    positions,
                },
            )
            .await
            .map_err(ReconcileError::from)
    }
}

fn token_account_amount(rpc: &RpcClient, account: Pubkey) -> Result<u64, ReconcileError> {
    match rpc.get_token_account_balance(&account) {
        Ok(amount) => amount
            .amount
            .parse::<u64>()
            .map_err(|_| ReconcileError::InvalidTokenAmount(amount.amount)),
        Err(error) if looks_like_missing_account(&error.to_string()) => Ok(0),
        Err(error) => Err(ReconcileError::Rpc(error.to_string())),
    }
}

fn looks_like_missing_account(message: &str) -> bool {
    message.contains("could not find account")
        || message.contains("AccountNotFound")
        || message.contains("Invalid param: could not find account")
}

fn parse_pubkey(field: &'static str, value: &str) -> Result<Pubkey, ReconcileError> {
    Pubkey::from_str(value).map_err(|_| ReconcileError::InvalidPubkey {
        field,
        value: value.to_owned(),
    })
}
