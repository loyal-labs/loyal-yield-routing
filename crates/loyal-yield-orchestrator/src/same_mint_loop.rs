use std::{fmt, future::Future, pin::Pin};

use crate::{
    DecisionAdvance, DecisionId, OrchestratorError, OrchestratorStore, PlanOutcome,
    PlanOutcomeStatus, PlannerConfig, RebalanceDecision, ReserveScore, VaultId,
};
use serde::{Deserialize, Serialize};
use solana_sdk::instruction::Instruction;

pub type SameMintLoopFuture<'a, T> =
    Pin<Box<dyn Future<Output = Result<T, SameMintRouteLoopError>> + Send + 'a>>;

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SameMintReserveApy {
    pub reserve: String,
    pub liquidity_mint: String,
    pub supply_apy_bps: i64,
    pub borrow_apy_bps: Option<i64>,
}

impl SameMintReserveApy {
    fn as_reserve_score(&self) -> ReserveScore {
        ReserveScore {
            reserve: self.reserve.clone(),
            supply_apy_bps: self.supply_apy_bps,
            borrow_apy_bps: self.borrow_apy_bps,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SameMintRoutingLoopConfig {
    pub planner: PlannerConfig,
    pub batch_size: usize,
    pub submit_batches: bool,
}

impl Default for SameMintRoutingLoopConfig {
    fn default() -> Self {
        Self {
            planner: PlannerConfig::default(),
            batch_size: 8,
            submit_batches: true,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SameMintRouteQuoteRequest {
    pub decision_id: DecisionId,
    pub vault_id: VaultId,
    pub source_reserve: String,
    pub target_reserve: String,
    pub liquidity_mint: String,
    pub redeem_amount_raw: u64,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SameMintRouteQuote {
    pub redeem_amount_raw: u64,
    pub deposit_amount_raw: u64,
    pub route_instructions: Vec<Instruction>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SameMintRouteExecution {
    pub decision_id: DecisionId,
    pub vault_id: VaultId,
    pub source_reserve: String,
    pub target_reserve: String,
    pub liquidity_mint: String,
    pub quote: SameMintRouteQuote,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SameMintBatchSimulation {
    pub preflight_chain_slot: Option<i64>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SameMintBatchSubmission {
    pub signature: String,
    pub submitted_slot: Option<i64>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct SameMintRoutingLoopReport {
    pub target: Option<SameMintReserveApy>,
    pub candidate_vaults: Vec<VaultId>,
    pub planned_decisions: Vec<DecisionId>,
    pub skipped_vaults: Vec<VaultId>,
    pub quoted_decisions: Vec<DecisionId>,
    pub simulated_decisions: Vec<DecisionId>,
    pub submitted_decisions: Vec<DecisionId>,
    pub failed_decisions: Vec<DecisionId>,
}

impl SameMintRoutingLoopReport {
    fn no_target() -> Self {
        Self {
            target: None,
            candidate_vaults: Vec::new(),
            planned_decisions: Vec::new(),
            skipped_vaults: Vec::new(),
            quoted_decisions: Vec::new(),
            simulated_decisions: Vec::new(),
            submitted_decisions: Vec::new(),
            failed_decisions: Vec::new(),
        }
    }
}

pub trait SameMintRouteStore {
    fn same_mint_candidate_vaults<'a>(
        &'a self,
        target_reserve: &'a str,
        liquidity_mint: &'a str,
    ) -> SameMintLoopFuture<'a, Vec<VaultId>>;

    fn plan_same_mint_rebalance<'a>(
        &'a self,
        vault_id: VaultId,
        reserve_scores: Vec<ReserveScore>,
        config: PlannerConfig,
    ) -> SameMintLoopFuture<'a, PlanOutcome>;

    fn advance_decision<'a>(
        &'a self,
        decision_id: DecisionId,
        advance: DecisionAdvance,
    ) -> SameMintLoopFuture<'a, RebalanceDecision>;
}

impl SameMintRouteStore for OrchestratorStore {
    fn same_mint_candidate_vaults<'a>(
        &'a self,
        target_reserve: &'a str,
        liquidity_mint: &'a str,
    ) -> SameMintLoopFuture<'a, Vec<VaultId>> {
        Box::pin(async move {
            OrchestratorStore::same_mint_candidate_vaults(self, target_reserve, liquidity_mint)
                .await
                .map_err(SameMintRouteLoopError::from)
        })
    }

    fn plan_same_mint_rebalance<'a>(
        &'a self,
        vault_id: VaultId,
        reserve_scores: Vec<ReserveScore>,
        config: PlannerConfig,
    ) -> SameMintLoopFuture<'a, PlanOutcome> {
        Box::pin(async move {
            OrchestratorStore::plan_same_mint_rebalance(self, vault_id, reserve_scores, config)
                .await
                .map_err(SameMintRouteLoopError::from)
        })
    }

    fn advance_decision<'a>(
        &'a self,
        decision_id: DecisionId,
        advance: DecisionAdvance,
    ) -> SameMintLoopFuture<'a, RebalanceDecision> {
        Box::pin(async move {
            OrchestratorStore::advance_decision(self, decision_id, advance)
                .await
                .map_err(SameMintRouteLoopError::from)
        })
    }
}

pub trait SameMintRouteExecutor {
    fn quote_same_mint_route<'a>(
        &'a self,
        request: SameMintRouteQuoteRequest,
    ) -> SameMintLoopFuture<'a, SameMintRouteQuote>;

    fn simulate_same_mint_batch<'a>(
        &'a self,
        routes: &'a [SameMintRouteExecution],
    ) -> SameMintLoopFuture<'a, SameMintBatchSimulation>;

    fn submit_same_mint_batch<'a>(
        &'a self,
        routes: &'a [SameMintRouteExecution],
    ) -> SameMintLoopFuture<'a, SameMintBatchSubmission>;
}

pub async fn run_same_mint_yield_routing_loop<S, E>(
    store: &S,
    executor: &E,
    reserve_apys: Vec<SameMintReserveApy>,
    config: SameMintRoutingLoopConfig,
) -> Result<SameMintRoutingLoopReport, SameMintRouteLoopError>
where
    S: SameMintRouteStore + Sync,
    E: SameMintRouteExecutor + Sync,
{
    let Some(target) = max_apy_reserve(&reserve_apys).cloned() else {
        return Ok(SameMintRoutingLoopReport::no_target());
    };
    let reserve_scores = reserve_apys
        .iter()
        .map(SameMintReserveApy::as_reserve_score)
        .collect::<Vec<_>>();
    let mut report = SameMintRoutingLoopReport {
        target: Some(target.clone()),
        candidate_vaults: store
            .same_mint_candidate_vaults(&target.reserve, &target.liquidity_mint)
            .await?,
        planned_decisions: Vec::new(),
        skipped_vaults: Vec::new(),
        quoted_decisions: Vec::new(),
        simulated_decisions: Vec::new(),
        submitted_decisions: Vec::new(),
        failed_decisions: Vec::new(),
    };

    let mut executable_routes = Vec::new();
    for vault_id in report.candidate_vaults.clone() {
        let outcome = store
            .plan_same_mint_rebalance(vault_id, reserve_scores.clone(), config.planner)
            .await?;
        let decision = match outcome.status {
            PlanOutcomeStatus::Planned(decision) => decision,
            PlanOutcomeStatus::Skipped { .. } => {
                report.skipped_vaults.push(vault_id);
                continue;
            }
        };
        report.planned_decisions.push(decision.id);
        store
            .advance_decision(decision.id, DecisionAdvance::StartSimulation)
            .await?;

        match quote_decision(executor, &decision).await {
            Ok(route) => {
                report.quoted_decisions.push(route.decision_id);
                executable_routes.push(route);
            }
            Err(error) => {
                mark_failed(store, decision.id, format!("quote failed: {error}")).await?;
                report.failed_decisions.push(decision.id);
            }
        }
    }

    if executable_routes.is_empty() {
        return Ok(report);
    }

    let batch_size = config.batch_size.max(1);
    for batch in executable_routes.chunks(batch_size) {
        match executor.simulate_same_mint_batch(batch).await {
            Ok(simulation) => {
                for route in batch {
                    store
                        .advance_decision(
                            route.decision_id,
                            DecisionAdvance::SimulationReady {
                                preflight_chain_slot: simulation.preflight_chain_slot,
                            },
                        )
                        .await?;
                    report.simulated_decisions.push(route.decision_id);
                }
            }
            Err(error) => {
                for route in batch {
                    mark_failed(
                        store,
                        route.decision_id,
                        format!("simulation failed: {error}"),
                    )
                    .await?;
                    report.failed_decisions.push(route.decision_id);
                }
                continue;
            }
        }

        if !config.submit_batches {
            continue;
        }

        match executor.submit_same_mint_batch(batch).await {
            Ok(submission) => {
                for route in batch {
                    store
                        .advance_decision(
                            route.decision_id,
                            DecisionAdvance::Submit {
                                signature: submission.signature.clone(),
                                slot: submission.submitted_slot,
                            },
                        )
                        .await?;
                    report.submitted_decisions.push(route.decision_id);
                }
            }
            Err(error) => {
                for route in batch {
                    mark_failed(
                        store,
                        route.decision_id,
                        format!("submission failed: {error}"),
                    )
                    .await?;
                    report.failed_decisions.push(route.decision_id);
                }
            }
        }
    }

    Ok(report)
}

fn max_apy_reserve(reserves: &[SameMintReserveApy]) -> Option<&SameMintReserveApy> {
    reserves.iter().max_by(|left, right| {
        left.supply_apy_bps
            .cmp(&right.supply_apy_bps)
            .then_with(|| right.reserve.cmp(&left.reserve))
            .then_with(|| right.liquidity_mint.cmp(&left.liquidity_mint))
    })
}

async fn quote_decision<E>(
    executor: &E,
    decision: &RebalanceDecision,
) -> Result<SameMintRouteExecution, SameMintRouteLoopError>
where
    E: SameMintRouteExecutor + Sync,
{
    let source_reserve =
        required_decision_string(decision.source_reserve.as_ref(), "source_reserve")?;
    let target_reserve =
        required_decision_string(decision.target_reserve.as_ref(), "target_reserve")?;
    let liquidity_mint =
        required_decision_string(decision.liquidity_mint.as_ref(), "liquidity_mint")?;
    let redeem_amount_raw = decision
        .amount_raw
        .ok_or(SameMintRouteLoopError::MissingDecisionField("amount_raw"))?;
    let redeem_amount_raw = u64::try_from(redeem_amount_raw)
        .map_err(|_| SameMintRouteLoopError::InvalidDecisionAmount(redeem_amount_raw))?;

    let quote = executor
        .quote_same_mint_route(SameMintRouteQuoteRequest {
            decision_id: decision.id,
            vault_id: decision.vault_id,
            source_reserve: source_reserve.clone(),
            target_reserve: target_reserve.clone(),
            liquidity_mint: liquidity_mint.clone(),
            redeem_amount_raw,
        })
        .await?;

    Ok(SameMintRouteExecution {
        decision_id: decision.id,
        vault_id: decision.vault_id,
        source_reserve,
        target_reserve,
        liquidity_mint,
        quote,
    })
}

async fn mark_failed<S>(
    store: &S,
    decision_id: DecisionId,
    reason: String,
) -> Result<(), SameMintRouteLoopError>
where
    S: SameMintRouteStore + Sync,
{
    store
        .advance_decision(decision_id, DecisionAdvance::Fail { reason })
        .await?;
    Ok(())
}

fn required_decision_string(
    value: Option<&String>,
    field: &'static str,
) -> Result<String, SameMintRouteLoopError> {
    value
        .cloned()
        .ok_or(SameMintRouteLoopError::MissingDecisionField(field))
}

#[derive(Debug)]
pub enum SameMintRouteLoopError {
    Orchestrator(OrchestratorError),
    MissingDecisionField(&'static str),
    InvalidDecisionAmount(i64),
    Quote(String),
    Simulation(String),
    Submission(String),
}

impl SameMintRouteLoopError {
    pub fn quote(message: impl Into<String>) -> Self {
        Self::Quote(message.into())
    }

    pub fn simulation(message: impl Into<String>) -> Self {
        Self::Simulation(message.into())
    }

    pub fn submission(message: impl Into<String>) -> Self {
        Self::Submission(message.into())
    }
}

impl fmt::Display for SameMintRouteLoopError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Orchestrator(error) => write!(formatter, "{error}"),
            Self::MissingDecisionField(field) => {
                write!(formatter, "planned decision is missing {field}")
            }
            Self::InvalidDecisionAmount(amount) => {
                write!(formatter, "planned decision amount {amount} is invalid")
            }
            Self::Quote(message) => formatter.write_str(message),
            Self::Simulation(message) => formatter.write_str(message),
            Self::Submission(message) => formatter.write_str(message),
        }
    }
}

impl std::error::Error for SameMintRouteLoopError {}

impl From<OrchestratorError> for SameMintRouteLoopError {
    fn from(value: OrchestratorError) -> Self {
        Self::Orchestrator(value)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{DecisionReason, DecisionStatus, SnapshotId};
    use chrono::Utc;
    use std::sync::{Arc, Mutex};

    #[derive(Clone)]
    struct FakeStore {
        inner: Arc<Mutex<FakeStoreState>>,
    }

    #[derive(Default)]
    struct FakeStoreState {
        candidate_vaults: Vec<VaultId>,
        plan_outcomes: Vec<PlanOutcome>,
        advances: Vec<(DecisionId, DecisionAdvance)>,
    }

    impl FakeStore {
        fn new(candidate_vaults: Vec<VaultId>, plan_outcomes: Vec<PlanOutcome>) -> Self {
            Self {
                inner: Arc::new(Mutex::new(FakeStoreState {
                    candidate_vaults,
                    plan_outcomes,
                    advances: Vec::new(),
                })),
            }
        }

        fn advances(&self) -> Vec<(DecisionId, DecisionAdvance)> {
            self.inner.lock().expect("fake store lock").advances.clone()
        }
    }

    impl SameMintRouteStore for FakeStore {
        fn same_mint_candidate_vaults<'a>(
            &'a self,
            _target_reserve: &'a str,
            _liquidity_mint: &'a str,
        ) -> SameMintLoopFuture<'a, Vec<VaultId>> {
            Box::pin(async move {
                Ok(self
                    .inner
                    .lock()
                    .expect("fake store lock")
                    .candidate_vaults
                    .clone())
            })
        }

        fn plan_same_mint_rebalance<'a>(
            &'a self,
            _vault_id: VaultId,
            _reserve_scores: Vec<ReserveScore>,
            _config: PlannerConfig,
        ) -> SameMintLoopFuture<'a, PlanOutcome> {
            Box::pin(async move {
                Ok(self
                    .inner
                    .lock()
                    .expect("fake store lock")
                    .plan_outcomes
                    .remove(0))
            })
        }

        fn advance_decision<'a>(
            &'a self,
            decision_id: DecisionId,
            advance: DecisionAdvance,
        ) -> SameMintLoopFuture<'a, RebalanceDecision> {
            Box::pin(async move {
                self.inner
                    .lock()
                    .expect("fake store lock")
                    .advances
                    .push((decision_id, advance));
                Ok(decision(decision_id.0, VaultId(99), "source", "target"))
            })
        }
    }

    #[derive(Default)]
    struct FakeExecutor {
        quote_error: Option<String>,
        simulation_error: Option<String>,
        submission_error: Option<String>,
        quoted: Mutex<Vec<SameMintRouteQuoteRequest>>,
        simulated_batches: Mutex<Vec<usize>>,
        submitted_batches: Mutex<Vec<usize>>,
    }

    impl SameMintRouteExecutor for FakeExecutor {
        fn quote_same_mint_route<'a>(
            &'a self,
            request: SameMintRouteQuoteRequest,
        ) -> SameMintLoopFuture<'a, SameMintRouteQuote> {
            Box::pin(async move {
                if let Some(error) = &self.quote_error {
                    return Err(SameMintRouteLoopError::quote(error.clone()));
                }
                self.quoted.lock().expect("quote lock").push(request);
                Ok(SameMintRouteQuote {
                    redeem_amount_raw: 1_000,
                    deposit_amount_raw: 995,
                    route_instructions: Vec::new(),
                })
            })
        }

        fn simulate_same_mint_batch<'a>(
            &'a self,
            routes: &'a [SameMintRouteExecution],
        ) -> SameMintLoopFuture<'a, SameMintBatchSimulation> {
            Box::pin(async move {
                if let Some(error) = &self.simulation_error {
                    return Err(SameMintRouteLoopError::simulation(error.clone()));
                }
                self.simulated_batches
                    .lock()
                    .expect("simulation lock")
                    .push(routes.len());
                Ok(SameMintBatchSimulation {
                    preflight_chain_slot: Some(42),
                })
            })
        }

        fn submit_same_mint_batch<'a>(
            &'a self,
            routes: &'a [SameMintRouteExecution],
        ) -> SameMintLoopFuture<'a, SameMintBatchSubmission> {
            Box::pin(async move {
                if let Some(error) = &self.submission_error {
                    return Err(SameMintRouteLoopError::submission(error.clone()));
                }
                self.submitted_batches
                    .lock()
                    .expect("submission lock")
                    .push(routes.len());
                Ok(SameMintBatchSubmission {
                    signature: "sig-batch".to_owned(),
                    submitted_slot: Some(43),
                })
            })
        }
    }

    fn reserve(reserve: &str, mint: &str, apy: i64) -> SameMintReserveApy {
        SameMintReserveApy {
            reserve: reserve.to_owned(),
            liquidity_mint: mint.to_owned(),
            supply_apy_bps: apy,
            borrow_apy_bps: None,
        }
    }

    fn decision(id: i64, vault_id: VaultId, source: &str, target: &str) -> RebalanceDecision {
        RebalanceDecision {
            id: DecisionId(id),
            vault_id,
            source_snapshot_id: Some(SnapshotId(7)),
            status: DecisionStatus::Planned,
            source_reserve: Some(source.to_owned()),
            target_reserve: Some(target.to_owned()),
            liquidity_mint: Some("USDC".to_owned()),
            amount_raw: Some(1_000),
            source_apy_bps: Some(100),
            target_apy_bps: Some(200),
            estimated_edge_bps: Some(100),
            estimated_cost_lamports: 0,
            decision_reason: DecisionReason::TargetSupplyApyExceedsSource,
            abandon_reason: None,
            signature: None,
            submitted_slot: None,
            confirmed_slot: None,
            preflight_chain_slot: None,
            post_snapshot_id: None,
            created_at: Utc::now(),
            updated_at: Utc::now(),
        }
    }

    #[tokio::test]
    async fn loop_quotes_simulates_submits_and_stores_decision_results() {
        let store = FakeStore::new(
            vec![VaultId(1), VaultId(2)],
            vec![
                PlanOutcome::planned(VaultId(1), decision(11, VaultId(1), "source-a", "target")),
                PlanOutcome::planned(VaultId(2), decision(12, VaultId(2), "source-b", "target")),
            ],
        );
        let executor = FakeExecutor::default();

        let report = run_same_mint_yield_routing_loop(
            &store,
            &executor,
            vec![
                reserve("source-a", "USDC", 100),
                reserve("target", "USDC", 220),
            ],
            SameMintRoutingLoopConfig {
                planner: PlannerConfig::default(),
                batch_size: 2,
                submit_batches: true,
            },
        )
        .await
        .expect("loop succeeds");

        assert_eq!(
            report.target.as_ref().map(|target| target.reserve.as_str()),
            Some("target")
        );
        assert_eq!(report.candidate_vaults, vec![VaultId(1), VaultId(2)]);
        assert_eq!(
            report.planned_decisions,
            vec![DecisionId(11), DecisionId(12)]
        );
        assert_eq!(
            report.quoted_decisions,
            vec![DecisionId(11), DecisionId(12)]
        );
        assert_eq!(
            report.simulated_decisions,
            vec![DecisionId(11), DecisionId(12)]
        );
        assert_eq!(
            report.submitted_decisions,
            vec![DecisionId(11), DecisionId(12)]
        );
        assert!(report.failed_decisions.is_empty());
        assert_eq!(
            executor
                .simulated_batches
                .lock()
                .expect("simulation lock")
                .as_slice(),
            &[2]
        );
        assert_eq!(
            executor
                .submitted_batches
                .lock()
                .expect("submission lock")
                .as_slice(),
            &[2]
        );

        let advances = store.advances();
        assert_eq!(advances.len(), 6);
        assert_eq!(
            advances[0],
            (DecisionId(11), DecisionAdvance::StartSimulation)
        );
        assert_eq!(
            advances[1],
            (DecisionId(12), DecisionAdvance::StartSimulation)
        );
        assert_eq!(
            advances[2],
            (
                DecisionId(11),
                DecisionAdvance::SimulationReady {
                    preflight_chain_slot: Some(42)
                }
            )
        );
        assert_eq!(
            advances[4],
            (
                DecisionId(11),
                DecisionAdvance::Submit {
                    signature: "sig-batch".to_owned(),
                    slot: Some(43)
                }
            )
        );
    }

    #[tokio::test]
    async fn loop_marks_quote_failures_in_store() {
        let store = FakeStore::new(
            vec![VaultId(1)],
            vec![PlanOutcome::planned(
                VaultId(1),
                decision(11, VaultId(1), "source-a", "target"),
            )],
        );
        let executor = FakeExecutor {
            quote_error: Some("quote unavailable".to_owned()),
            ..FakeExecutor::default()
        };

        let report = run_same_mint_yield_routing_loop(
            &store,
            &executor,
            vec![reserve("target", "USDC", 220)],
            SameMintRoutingLoopConfig::default(),
        )
        .await
        .expect("quote failure is stored, not returned");

        assert_eq!(report.failed_decisions, vec![DecisionId(11)]);
        assert!(report.submitted_decisions.is_empty());
        assert_eq!(
            store.advances().last(),
            Some(&(
                DecisionId(11),
                DecisionAdvance::Fail {
                    reason: "quote failed: quote unavailable".to_owned()
                }
            ))
        );
    }

    #[tokio::test]
    async fn loop_marks_simulation_failures_and_does_not_submit() {
        let store = FakeStore::new(
            vec![VaultId(1)],
            vec![PlanOutcome::planned(
                VaultId(1),
                decision(11, VaultId(1), "source-a", "target"),
            )],
        );
        let executor = FakeExecutor {
            simulation_error: Some("compute budget exceeded".to_owned()),
            ..FakeExecutor::default()
        };

        let report = run_same_mint_yield_routing_loop(
            &store,
            &executor,
            vec![reserve("target", "USDC", 220)],
            SameMintRoutingLoopConfig::default(),
        )
        .await
        .expect("simulation failure is stored, not returned");

        assert_eq!(report.quoted_decisions, vec![DecisionId(11)]);
        assert!(report.simulated_decisions.is_empty());
        assert!(report.submitted_decisions.is_empty());
        assert_eq!(report.failed_decisions, vec![DecisionId(11)]);
        assert!(executor
            .submitted_batches
            .lock()
            .expect("submission lock")
            .is_empty());
        assert_eq!(
            store.advances().last(),
            Some(&(
                DecisionId(11),
                DecisionAdvance::Fail {
                    reason: "simulation failed: compute budget exceeded".to_owned()
                }
            ))
        );
    }

    #[tokio::test]
    async fn loop_can_stop_after_simulation_for_dry_run() {
        let store = FakeStore::new(
            vec![VaultId(1)],
            vec![PlanOutcome::planned(
                VaultId(1),
                decision(11, VaultId(1), "source-a", "target"),
            )],
        );
        let executor = FakeExecutor::default();

        let report = run_same_mint_yield_routing_loop(
            &store,
            &executor,
            vec![reserve("target", "USDC", 220)],
            SameMintRoutingLoopConfig {
                planner: PlannerConfig::default(),
                batch_size: 1,
                submit_batches: false,
            },
        )
        .await
        .expect("dry run succeeds");

        assert_eq!(report.quoted_decisions, vec![DecisionId(11)]);
        assert_eq!(report.simulated_decisions, vec![DecisionId(11)]);
        assert!(report.submitted_decisions.is_empty());
        assert!(report.failed_decisions.is_empty());
        assert!(executor
            .submitted_batches
            .lock()
            .expect("submission lock")
            .is_empty());
        assert_eq!(
            store.advances().last(),
            Some(&(
                DecisionId(11),
                DecisionAdvance::SimulationReady {
                    preflight_chain_slot: Some(42)
                }
            ))
        );
    }
}
