use crate::{
    planner::{SameMintPlannerConfig, SameMintRoutePlanner},
    reconcile::{ReconcileError, RpcPositionReconciler},
    route_builder::{build_same_mint_route_transaction, RouteBuildError},
    rpc::{RpcRouteSubmitter, RpcSubmitError},
    DecisionAdvance, DecisionId, OrchestratorError, OrchestratorStore, PlanOutcomeStatus,
    RebalanceAttemptInput, RebalanceAttemptUpdate,
};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use solana_client::rpc_client::RpcClient;
use solana_sdk::{instruction::Instruction, signature::Keypair, signer::Signer};
use thiserror::Error;

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct SameMintLoopConfig {
    #[serde(default)]
    pub cluster: Option<String>,
    #[serde(default = "default_max_vaults")]
    pub max_vaults: usize,
    #[serde(default = "default_batch_size")]
    pub batch_size: usize,
    #[serde(default = "default_true")]
    pub reconcile_positions: bool,
    #[serde(default = "default_true")]
    pub dry_run: bool,
    #[serde(default)]
    pub submit_txs: bool,
    #[serde(default = "default_true")]
    pub abandon_dry_run_decisions: bool,
    #[serde(default = "default_worker_id")]
    pub worker_id: String,
}

impl Default for SameMintLoopConfig {
    fn default() -> Self {
        Self {
            cluster: None,
            max_vaults: default_max_vaults(),
            batch_size: default_batch_size(),
            reconcile_positions: true,
            dry_run: true,
            submit_txs: false,
            abandon_dry_run_decisions: true,
            worker_id: default_worker_id(),
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct SameMintRouteRunConfig {
    #[serde(flatten)]
    pub loop_config: SameMintLoopConfig,
    #[serde(flatten)]
    pub planner_config: SameMintPlannerConfig,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq, Eq)]
pub struct SameMintLoopReport {
    pub active_vaults: usize,
    pub reconciled_vaults: usize,
    pub planned_decisions: usize,
    pub claimed_decisions: usize,
    pub built_transactions: usize,
    pub simulated: bool,
    pub simulation_ok: Option<bool>,
    pub submitted: bool,
    pub submitted_signature: Option<String>,
    pub confirmed_slot: Option<u64>,
    pub failures: Vec<String>,
}

#[derive(Debug, Error)]
pub enum SameMintLoopError {
    #[error(transparent)]
    Store(#[from] OrchestratorError),
    #[error(transparent)]
    Reconcile(#[from] ReconcileError),
    #[error(transparent)]
    RouteBuild(#[from] RouteBuildError),
    #[error(transparent)]
    Rpc(#[from] RpcSubmitError),
}

pub struct SameMintYieldRoutingLoop<'a> {
    store: &'a OrchestratorStore,
    rpc: &'a RpcClient,
    signer: &'a Keypair,
    planner: SameMintRoutePlanner,
    config: SameMintLoopConfig,
}

impl<'a> SameMintYieldRoutingLoop<'a> {
    pub fn new(
        store: &'a OrchestratorStore,
        rpc: &'a RpcClient,
        signer: &'a Keypair,
        route_config: SameMintRouteRunConfig,
    ) -> Self {
        Self {
            store,
            rpc,
            signer,
            planner: SameMintRoutePlanner::new(route_config.planner_config),
            config: route_config.loop_config,
        }
    }

    pub async fn run_once(&self) -> Result<SameMintLoopReport, SameMintLoopError> {
        let mut report = SameMintLoopReport::default();
        let active_vaults = self
            .store
            .active_vault_route_policies(
                self.config.cluster.as_deref(),
                self.config.max_vaults as i64,
            )
            .await?;
        report.active_vaults = active_vaults.len();

        if self.config.reconcile_positions {
            let reconciler = RpcPositionReconciler::new(self.store, self.rpc);
            for vault_policy in &active_vaults {
                match reconciler
                    .reconcile_vault(vault_policy, &self.planner.config().targets)
                    .await
                {
                    Ok(_) => report.reconciled_vaults += 1,
                    Err(error) => report.failures.push(error.to_string()),
                }
            }
        }

        for vault_policy in &active_vaults {
            let positions = self.store.current_positions(vault_policy.vault.id).await?;
            let Ok(input) = self.planner.plan_vault(vault_policy, &positions) else {
                continue;
            };
            let outcome = self
                .store
                .record_planned_rebalance_decision(vault_policy.vault.id, input)
                .await?;
            if matches!(outcome.status, PlanOutcomeStatus::Planned(_)) {
                report.planned_decisions += 1;
            }
        }

        let decisions = self
            .store
            .claim_same_mint_decisions(self.config.batch_size as i64)
            .await?;
        report.claimed_decisions = decisions.len();
        if decisions.is_empty() {
            return Ok(report);
        }

        let mut batch_instructions = Vec::new();
        let mut attempt_ids = Vec::new();
        for decision in decisions {
            let transaction = match build_same_mint_route_transaction(
                &decision.execution_plan,
                self.signer.pubkey(),
            ) {
                Ok(transaction) => transaction,
                Err(error) => {
                    let attempt = self
                        .store
                        .record_rebalance_attempt(
                            decision.id,
                            RebalanceAttemptInput {
                                status: "failed".to_owned(),
                                worker_id: Some(self.config.worker_id.clone()),
                                dry_run: self.config.dry_run,
                                transaction_plan: json!({
                                    "decisionExecutionPlan": decision.execution_plan,
                                }),
                                simulation_result: Value::Object(Default::default()),
                                submit_result: Value::Object(Default::default()),
                                signature: None,
                                slot: None,
                                error: Some(error.to_string()),
                            },
                        )
                        .await?;
                    self.store
                        .advance_decision(
                            decision.id,
                            DecisionAdvance::Fail {
                                reason: format!(
                                    "same-mint route build failed in attempt {}: {error}",
                                    attempt.attempt_no
                                ),
                            },
                        )
                        .await?;
                    report.failures.push(error.to_string());
                    continue;
                }
            };

            let attempt = self
                .store
                .record_rebalance_attempt(
                    decision.id,
                    RebalanceAttemptInput {
                        status: "simulating".to_owned(),
                        worker_id: Some(self.config.worker_id.clone()),
                        dry_run: self.config.dry_run || !self.config.submit_txs,
                        transaction_plan: transaction.report,
                        simulation_result: Value::Object(Default::default()),
                        submit_result: Value::Object(Default::default()),
                        signature: None,
                        slot: None,
                        error: None,
                    },
                )
                .await?;
            attempt_ids.push((attempt.id, decision.id));
            batch_instructions.push(transaction.instruction);
        }

        report.built_transactions = batch_instructions.len();
        if batch_instructions.is_empty() {
            return Ok(report);
        }

        self.execute_batch(batch_instructions, attempt_ids, &mut report)
            .await?;
        Ok(report)
    }

    async fn execute_batch(
        &self,
        instructions: Vec<Instruction>,
        attempts: Vec<(i64, DecisionId)>,
        report: &mut SameMintLoopReport,
    ) -> Result<(), SameMintLoopError> {
        let submitter = RpcRouteSubmitter::new(self.rpc);
        let simulation = submitter.simulate_instructions(&instructions, self.signer)?;
        report.simulated = true;
        report.simulation_ok = Some(simulation.ok);
        let simulation_error = (!simulation.ok).then(|| "simulation failed".to_owned());

        for (attempt_id, decision_id) in &attempts {
            self.store
                .update_rebalance_attempt(
                    *attempt_id,
                    RebalanceAttemptUpdate {
                        status: if simulation.ok {
                            "simulated".to_owned()
                        } else {
                            "failed".to_owned()
                        },
                        simulation_result: simulation.report.clone(),
                        submit_result: Value::Object(Default::default()),
                        signature: None,
                        slot: Some(simulation.slot as i64),
                        error: simulation_error.clone(),
                    },
                )
                .await?;

            if simulation.ok {
                self.store
                    .advance_decision(*decision_id, DecisionAdvance::SimulationReady)
                    .await?;
            } else {
                self.store
                    .advance_decision(
                        *decision_id,
                        DecisionAdvance::Fail {
                            reason: "simulation failed".to_owned(),
                        },
                    )
                    .await?;
            }
        }

        if !simulation.ok {
            return Ok(());
        }

        if self.config.dry_run || !self.config.submit_txs {
            if self.config.abandon_dry_run_decisions {
                for (_, decision_id) in &attempts {
                    self.store
                        .advance_decision(
                            *decision_id,
                            DecisionAdvance::Abandon {
                                reason: "dry_run_simulation_only".to_owned(),
                            },
                        )
                        .await?;
                }
            }
            return Ok(());
        }

        let submitted = submitter.submit_and_confirm(&instructions, self.signer)?;
        report.submitted = true;
        report.submitted_signature = Some(submitted.signature.to_string());
        report.confirmed_slot = Some(submitted.slot);

        for (attempt_id, decision_id) in &attempts {
            self.store
                .update_rebalance_attempt(
                    *attempt_id,
                    RebalanceAttemptUpdate {
                        status: "confirmed".to_owned(),
                        simulation_result: simulation.report.clone(),
                        submit_result: submitted.report.clone(),
                        signature: Some(submitted.signature.to_string()),
                        slot: Some(submitted.slot as i64),
                        error: None,
                    },
                )
                .await?;
            self.store
                .advance_decision(
                    *decision_id,
                    DecisionAdvance::Submit {
                        signature: submitted.signature.to_string(),
                        slot: Some(submitted.slot as i64),
                    },
                )
                .await?;
            self.store
                .advance_decision(*decision_id, DecisionAdvance::StartConfirmation)
                .await?;
            self.store
                .advance_decision(
                    *decision_id,
                    DecisionAdvance::Confirm {
                        slot: Some(submitted.slot as i64),
                        post_snapshot_id: None,
                    },
                )
                .await?;
        }

        Ok(())
    }
}

fn default_max_vaults() -> usize {
    50
}

fn default_batch_size() -> usize {
    8
}

fn default_true() -> bool {
    true
}

fn default_worker_id() -> String {
    "same-mint-route-runner".to_owned()
}
