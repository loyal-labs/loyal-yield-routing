use crate::{
    planner::{SameMintPlannerConfig, YieldRoutePlanner},
    reconcile::{ReconcileError, RpcPositionReconciler},
    route_builder::{build_yield_route_transaction, RouteBuildError},
    rpc::{RpcRouteSubmitter, RpcSubmitError},
    DecisionAdvance, DecisionId, JupiterRouteQuoteProvider, OrchestratorError, OrchestratorStore,
    PlanOutcomeStatus, RebalanceAttemptInput, RebalanceAttemptUpdate,
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

pub type YieldRouteLoopConfig = SameMintLoopConfig;
pub type YieldRouteRunConfig = SameMintRouteRunConfig;

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
    pub submitted_signatures: Vec<String>,
    pub confirmed_slot: Option<u64>,
    pub skips: Vec<String>,
    pub failures: Vec<String>,
}

pub type YieldRouteLoopReport = SameMintLoopReport;

struct RouteExecutionGroup {
    attempt_id: i64,
    decision_id: DecisionId,
    instructions: Vec<Instruction>,
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
    planner: YieldRoutePlanner,
    config: SameMintLoopConfig,
}

pub type YieldRoutingLoop<'a> = SameMintYieldRoutingLoop<'a>;

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
            planner: YieldRoutePlanner::new(route_config.planner_config),
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

        let quote_provider = JupiterRouteQuoteProvider::default();
        for vault_policy in &active_vaults {
            let positions = self.store.current_positions(vault_policy.vault.id).await?;
            let input = match self
                .planner
                .plan_vault(vault_policy, &positions, &quote_provider)
                .await
            {
                Ok(input) => input,
                Err(error) => {
                    report.skips.push(format!(
                        "vault {} skipped planning: {:?}",
                        vault_policy.vault.id, error
                    ));
                    continue;
                }
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
            .claim_yield_route_decisions(self.config.batch_size as i64)
            .await?;
        report.claimed_decisions = decisions.len();
        if decisions.is_empty() {
            return Ok(report);
        }

        let mut execution_groups = Vec::new();
        let mut attempt_ids = Vec::new();
        for decision in decisions {
            let transaction =
                match build_yield_route_transaction(&decision.execution_plan, self.signer.pubkey())
                {
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
                                        "yield route build failed in attempt {}: {error}",
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
            execution_groups.push(RouteExecutionGroup {
                attempt_id: attempt.id,
                decision_id: decision.id,
                instructions: transaction.instructions,
            });
        }

        report.built_transactions = execution_groups
            .iter()
            .map(|group| group.instructions.len())
            .sum();
        if execution_groups.is_empty() {
            return Ok(report);
        }

        if execution_groups
            .iter()
            .all(|group| group.instructions.len() == 1)
        {
            let batch_instructions = execution_groups
                .iter()
                .map(|group| group.instructions[0].clone())
                .collect::<Vec<_>>();
            self.execute_batch(batch_instructions, attempt_ids, &mut report)
                .await?;
        } else {
            self.execute_execution_groups(execution_groups, &mut report)
                .await?;
        }
        Ok(report)
    }

    async fn execute_execution_groups(
        &self,
        groups: Vec<RouteExecutionGroup>,
        report: &mut SameMintLoopReport,
    ) -> Result<(), SameMintLoopError> {
        for group in groups {
            if group.instructions.len() <= 1 {
                self.execute_batch(
                    group.instructions,
                    vec![(group.attempt_id, group.decision_id)],
                    report,
                )
                .await?;
            } else {
                self.execute_sequence(group, report).await?;
            }
        }
        Ok(())
    }

    async fn execute_sequence(
        &self,
        group: RouteExecutionGroup,
        report: &mut SameMintLoopReport,
    ) -> Result<(), SameMintLoopError> {
        let submitter = RpcRouteSubmitter::new(self.rpc);
        let mut simulation_reports = Vec::new();
        let mut submission_reports = Vec::new();
        let mut signatures = Vec::new();
        let mut last_slot = None;
        let dry_run_sequence = self.config.dry_run || !self.config.submit_txs;

        for (index, instruction) in group.instructions.iter().enumerate() {
            let simulation =
                submitter.simulate_instructions(&[instruction.clone()], self.signer)?;
            report.simulated = true;
            simulation_reports.push(json!({
                "step": index,
                "result": simulation.report,
            }));
            if !simulation.ok {
                report.simulation_ok = Some(false);
                let aggregate_simulation = json!({
                    "attempted": true,
                    "ok": false,
                    "mode": "sequential",
                    "failedStep": index,
                    "steps": simulation_reports,
                });
                self.store
                    .update_rebalance_attempt(
                        group.attempt_id,
                        RebalanceAttemptUpdate {
                            status: "failed".to_owned(),
                            simulation_result: aggregate_simulation,
                            submit_result: json!({
                                "attempted": !submission_reports.is_empty(),
                                "mode": "sequential",
                                "steps": submission_reports,
                            }),
                            signature: signatures.last().cloned(),
                            slot: last_slot.map(|slot| slot as i64),
                            error: Some(format!("sequential simulation failed at step {index}")),
                        },
                    )
                    .await?;
                self.store
                    .advance_decision(
                        group.decision_id,
                        DecisionAdvance::Fail {
                            reason: format!("sequential simulation failed at step {index}"),
                        },
                    )
                    .await?;
                return Ok(());
            }

            if dry_run_sequence {
                let skipped_reason =
                    "dry-run sequential execution cannot simulate state after the first step";
                for skipped_index in (index + 1)..group.instructions.len() {
                    simulation_reports.push(json!({
                        "step": skipped_index,
                        "skipped": skipped_reason,
                    }));
                }
                break;
            }

            let submitted = submitter.submit_and_confirm(&[instruction.clone()], self.signer)?;
            report.submitted = true;
            report.submitted_signature = Some(submitted.signature.to_string());
            report
                .submitted_signatures
                .push(submitted.signature.to_string());
            report.confirmed_slot = Some(submitted.slot);
            signatures.push(submitted.signature.to_string());
            last_slot = Some(submitted.slot);
            submission_reports.push(json!({
                "step": index,
                "result": submitted.report,
            }));
        }

        report.simulation_ok = Some(true);
        let aggregate_simulation = json!({
            "attempted": true,
            "ok": true,
            "mode": "sequential",
            "complete": !dry_run_sequence,
            "steps": simulation_reports,
        });

        if dry_run_sequence {
            self.store
                .update_rebalance_attempt(
                    group.attempt_id,
                    RebalanceAttemptUpdate {
                        status: "simulated".to_owned(),
                        simulation_result: aggregate_simulation,
                        submit_result: Value::Object(Default::default()),
                        signature: None,
                        slot: None,
                        error: None,
                    },
                )
                .await?;
            self.store
                .advance_decision(group.decision_id, DecisionAdvance::SimulationReady)
                .await?;
            if self.config.abandon_dry_run_decisions {
                self.store
                    .advance_decision(
                        group.decision_id,
                        DecisionAdvance::Abandon {
                            reason: "dry_run_simulation_only".to_owned(),
                        },
                    )
                    .await?;
            }
            return Ok(());
        }

        let aggregate_submit = json!({
            "attempted": true,
            "mode": "sequential",
            "steps": submission_reports,
            "signatures": signatures,
        });
        self.store
            .update_rebalance_attempt(
                group.attempt_id,
                RebalanceAttemptUpdate {
                    status: "confirmed".to_owned(),
                    simulation_result: aggregate_simulation,
                    submit_result: aggregate_submit,
                    signature: report.submitted_signature.clone(),
                    slot: report.confirmed_slot.map(|slot| slot as i64),
                    error: None,
                },
            )
            .await?;
        self.store
            .advance_decision(
                group.decision_id,
                DecisionAdvance::Submit {
                    signature: report.submitted_signature.clone().unwrap_or_default(),
                    slot: report.confirmed_slot.map(|slot| slot as i64),
                },
            )
            .await?;
        self.store
            .advance_decision(group.decision_id, DecisionAdvance::StartConfirmation)
            .await?;
        self.store
            .advance_decision(
                group.decision_id,
                DecisionAdvance::Confirm {
                    slot: report.confirmed_slot.map(|slot| slot as i64),
                    post_snapshot_id: None,
                },
            )
            .await?;
        Ok(())
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
        report.submitted_signatures = vec![submitted.signature.to_string()];
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
