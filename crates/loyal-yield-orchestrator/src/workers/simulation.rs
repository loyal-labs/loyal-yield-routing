use serde_json::{json, Value};
use solana_sdk::{instruction::Instruction, pubkey::Pubkey};

use crate::kamino::{same_mint_kamino_compiled_parts, SameMintKaminoRouteAccounts};
use crate::pipeline::{AttemptStatus, DecisionWorkItem};
use crate::policy_execution::{execute_same_mint_policy_route_from_policy, SameMintPolicyRoute};
use crate::{OrchestratorError, RpcSimulationReport};

#[derive(Debug, Default, Clone, Copy)]
pub struct SimulationWorker;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SimulationReport {
    pub slot: Option<i64>,
    pub compute_units: Option<i64>,
    pub logs_hash: Option<String>,
    pub error_code: Option<String>,
    pub error_message: Option<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct SameMintPolicyExecutionRequest {
    pub route: SameMintPolicyRoute,
    pub signer: Pubkey,
    pub vault_index: u8,
    pub accounts: SameMintKaminoRouteAccounts,
    pub amount: u64,
}

impl SimulationWorker {
    pub fn build_same_mint_policy_execution(
        request: SameMintPolicyExecutionRequest,
    ) -> Result<Instruction, OrchestratorError> {
        let (withdraw, deposit) =
            same_mint_kamino_compiled_parts(request.accounts, request.amount)?;
        execute_same_mint_policy_route_from_policy(
            request.route,
            request.signer,
            request.vault_index,
            withdraw,
            deposit,
        )
    }

    pub fn report_from_rpc(report: &RpcSimulationReport) -> SimulationReport {
        SimulationReport {
            slot: i64::try_from(report.slot).ok(),
            compute_units: report
                .units_consumed
                .and_then(|units| i64::try_from(units).ok()),
            logs_hash: report.logs_hash.clone(),
            error_code: report
                .error
                .as_ref()
                .map(|_| "simulation_failed".to_owned()),
            error_message: report.error.clone(),
        }
    }

    pub fn attempt_status(report: &SimulationReport) -> AttemptStatus {
        if report.error_code.is_some() || report.error_message.is_some() {
            AttemptStatus::Failed
        } else {
            AttemptStatus::Ready
        }
    }

    pub fn attempt_payload(decision: &DecisionWorkItem, report: &SimulationReport) -> Value {
        json!({
            "vault_id": decision.vault_id.as_i64(),
            "liquidity_mint": decision.liquidity_mint,
            "source_reserve": decision.source_reserve,
            "target_reserve": decision.target_reserve,
            "amount_raw": decision.amount_raw,
            "simulation_slot": report.slot,
            "compute_units": report.compute_units,
            "logs_hash": report.logs_hash
        })
    }

    pub fn should_reconcile_after_failure(report: &SimulationReport) -> bool {
        matches!(
            report.error_code.as_deref(),
            Some("account_not_found" | "stale_blockhash" | "state_drift")
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::kamino::{KaminoReserveAccounts, SameMintKaminoRouteAccounts};
    use crate::policy_execution::SameMintPolicyRoute;
    use crate::{DecisionId, VaultId};
    use loyal_actions::{
        create_all_in_one_market_mint_yield_route_action, JupiterSwapContract, LoyalActionContext,
        SwapLane, YieldRouteUniverse, JUPITER_DEFAULT_MAX_SLIPPAGE_BPS, JUPITER_SWAP_DISCRIMINATOR,
        JUPITER_V6_PROGRAM_ID,
    };

    fn decision() -> DecisionWorkItem {
        DecisionWorkItem {
            decision_id: DecisionId(1),
            vault_id: VaultId(2),
            cluster: "mainnet".to_owned(),
            liquidity_mint: "USDC".to_owned(),
            source_reserve: "source".to_owned(),
            target_reserve: "target".to_owned(),
            amount_raw: 5,
        }
    }

    fn reserve(liquidity_mint: Pubkey) -> KaminoReserveAccounts {
        KaminoReserveAccounts {
            reserve: Pubkey::new_unique(),
            lending_market: Pubkey::new_unique(),
            lending_market_authority: Pubkey::new_unique(),
            liquidity_mint,
            liquidity_supply: Pubkey::new_unique(),
            collateral_mint: Pubkey::new_unique(),
        }
    }

    #[test]
    fn simulation_worker_marks_error_as_failed() {
        let report = SimulationReport {
            slot: Some(1),
            compute_units: None,
            logs_hash: None,
            error_code: Some("state_drift".to_owned()),
            error_message: Some("source balance changed".to_owned()),
        };

        assert_eq!(
            SimulationWorker::attempt_status(&report),
            AttemptStatus::Failed
        );
        assert!(SimulationWorker::should_reconcile_after_failure(&report));
    }

    #[test]
    fn simulation_worker_payload_carries_route_identity() {
        let report = SimulationReport {
            slot: Some(9),
            compute_units: Some(12_345),
            logs_hash: Some("abc".to_owned()),
            error_code: None,
            error_message: None,
        };
        let payload = SimulationWorker::attempt_payload(&decision(), &report);

        assert_eq!(payload["vault_id"], 2);
        assert_eq!(payload["source_reserve"], "source");
        assert_eq!(payload["compute_units"], 12_345);
    }

    #[test]
    fn simulation_worker_builds_real_same_mint_policy_execution() {
        let mint = Pubkey::new_unique();
        let source = reserve(mint);
        let target = reserve(mint);
        let signer = Pubkey::new_unique();
        let vault = Pubkey::new_unique();
        let setup = create_all_in_one_market_mint_yield_route_action(
            LoyalActionContext {
                settings: Pubkey::new_unique(),
                authority: Pubkey::new_unique(),
                delegated_signer: signer,
                account_index: 0,
                vault,
            },
            YieldRouteUniverse::new(
                vec![mint],
                vec![source.lending_market, target.lending_market],
                vec![mint],
            ),
            vec![SwapLane::Jupiter(JupiterSwapContract {
                program_id: JUPITER_V6_PROGRAM_ID,
                exact_in_discriminator: JUPITER_SWAP_DISCRIMINATOR,
                max_slippage_bps: JUPITER_DEFAULT_MAX_SLIPPAGE_BPS,
            })],
        )
        .unwrap();
        let route = SameMintPolicyRoute::from(setup.same_mint_route().unwrap());

        let ix =
            SimulationWorker::build_same_mint_policy_execution(SameMintPolicyExecutionRequest {
                route,
                signer,
                vault_index: 0,
                accounts: SameMintKaminoRouteAccounts {
                    source_reserve: source,
                    target_reserve: target,
                    vault_owner: vault,
                    vault_liquidity_token_account: Pubkey::new_unique(),
                    source_collateral_token_account: Pubkey::new_unique(),
                    target_collateral_token_account: Pubkey::new_unique(),
                },
                amount: 10,
            })
            .unwrap();

        assert_eq!(ix.accounts[0].pubkey, route.action_account);
        assert_eq!(ix.accounts[2].pubkey, signer);
        assert!(ix.accounts[2].is_signer);
        assert!(ix.accounts.iter().any(|account| account.pubkey == vault));
    }
}
