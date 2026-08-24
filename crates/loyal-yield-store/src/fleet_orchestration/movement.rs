use super::{
    capacity::{
        target_capacity_reservation_from_row, TargetCapacityProjection,
        TargetCapacityReservationInput, TargetCapacityReservationRecord,
    },
    queue::{
        canonical_conflict_account_keys, canonical_writable_account_keys,
        reserve_fee_only_route_payer_spend, signed_route_submission_from_row,
        RebalanceOpportunityLease, SignedRouteSubmissionInput, SignedRouteSubmissionLease,
        SignedRouteSubmissionRecord,
    },
};
use crate::{DecisionId, NeonSqlClient, OrchestratorError, SnapshotId, VaultId};
use chrono::{DateTime, Duration, Utc};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use sha2::{Digest, Sha256};
use sqlx::{PgConnection, Row};

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CrossMintMovementLeg {
    Withdraw,
    Swap,
    Deposit,
}

impl CrossMintMovementLeg {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Withdraw => "withdraw",
            Self::Swap => "swap",
            Self::Deposit => "deposit",
        }
    }

    fn parse(value: &str) -> Result<Self, OrchestratorError> {
        match value {
            "withdraw" => Ok(Self::Withdraw),
            "swap" => Ok(Self::Swap),
            "deposit" => Ok(Self::Deposit),
            other => Err(OrchestratorError::StoreInvariant(format!(
                "unknown cross-mint movement leg {other:?}"
            ))),
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CrossMintLegPurpose {
    OptimizeYield,
    RecoverSource,
    FallbackTarget,
}

impl CrossMintLegPurpose {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::OptimizeYield => "optimize_yield",
            Self::RecoverSource => "recover_source",
            Self::FallbackTarget => "fallback_target",
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CrossMintCustodyPhase {
    SourceReserve,
    SourceIdle,
    TargetIdle,
    TargetReserve,
    ClosedByUser,
    ManualIntervention,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CrossMintTerminalOutcome {
    CompletedTarget,
    RecoveredSource,
    CancelledBeforeWithdraw,
    ClosedByUser,
    ManualIntervention,
}

impl CrossMintTerminalOutcome {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::CompletedTarget => "completed_target",
            Self::RecoveredSource => "recovered_source",
            Self::CancelledBeforeWithdraw => "cancelled_before_withdraw",
            Self::ClosedByUser => "closed_by_user",
            Self::ManualIntervention => "manual_intervention",
        }
    }

    fn parse(value: &str) -> Result<Self, OrchestratorError> {
        match value {
            "completed_target" => Ok(Self::CompletedTarget),
            "recovered_source" => Ok(Self::RecoveredSource),
            "cancelled_before_withdraw" => Ok(Self::CancelledBeforeWithdraw),
            "closed_by_user" => Ok(Self::ClosedByUser),
            "manual_intervention" => Ok(Self::ManualIntervention),
            other => Err(OrchestratorError::StoreInvariant(format!(
                "unknown cross-mint terminal outcome {other:?}"
            ))),
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CrossMintMovementGates {
    pub cluster: String,
    pub start_new_movements: bool,
    pub continue_or_recover_existing: bool,
    pub generation: i64,
    pub updated_at: Option<DateTime<Utc>>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CrossMintMovementRecord {
    pub decision_id: DecisionId,
    pub opportunity_id: i64,
    pub cluster: String,
    pub vault_id: VaultId,
    pub source_snapshot_id: Option<SnapshotId>,
    pub source_reserve: String,
    pub intended_target_reserve: String,
    pub active_target_reserve: String,
    pub source_mint: String,
    pub target_mint: String,
    pub planned_amount_raw: i64,
    /// Immutable evidence admitted in the same transaction that created the
    /// movement. This proves the exact policy bindings and Jupiter build were
    /// fresh before custody could leave the source reserve.
    pub preflight_certification: Value,
    pub custody_mint: String,
    pub custody_amount_raw: i64,
    pub custody_account: String,
    pub custody_observed_balance_raw: Option<i64>,
    pub custody_reconciled_slot: Option<i64>,
    pub custody_version: i64,
    pub phase: CrossMintCustodyPhase,
    pub terminal_outcome: Option<CrossMintTerminalOutcome>,
    pub terminal_evidence: Option<Value>,
    pub terminal_reason: Option<String>,
    pub terminal_observed_slot: Option<i64>,
    pub continuation_available_at: Option<DateTime<Utc>>,
    pub continuation_fencing_token: i64,
    pub continuation_attempt_count: i32,
}

#[derive(Clone, Debug)]
pub struct CrossMintContinuationLease {
    pub movement: CrossMintMovementRecord,
    pub owner: String,
    pub fencing_token: i64,
    pub control_generation: i64,
    pub expires_at: DateTime<Utc>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TokenBalanceDelta {
    pub mint: String,
    pub token_account: String,
    pub amount_raw: i64,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TokenBalanceAnchor {
    pub mint: String,
    pub token_account: String,
    pub amount_raw: i64,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct KaminoPositionAnchor {
    pub reserve: String,
    pub market: String,
    pub obligation: String,
    pub obligation_exists: bool,
    pub deposited_collateral_amount_raw: i64,
    /// Smallest liquidity amount that the finalized reserve exchange rate can
    /// turn into one raw collateral unit. This is only required on a
    /// post-deposit readback when Kamino leaves rounding dust idle.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub minimum_deposit_amount_raw: Option<i64>,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CrossMintBalanceAnchors {
    pub debit: Option<TokenBalanceAnchor>,
    pub credit: Option<TokenBalanceAnchor>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub kamino_position: Option<KaminoPositionAnchor>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CrossMintExpectedEffect {
    pub debit: Option<TokenBalanceDelta>,
    pub credit_mint: Option<String>,
    pub credit_token_account: Option<String>,
    pub minimum_credit_amount_raw: Option<i64>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CrossMintReconciledEffect {
    pub debit: Option<TokenBalanceDelta>,
    pub credit: Option<TokenBalanceDelta>,
}

#[derive(Clone, Debug)]
pub struct CrossMintLegPublicationInput {
    pub leg: CrossMintMovementLeg,
    pub purpose: CrossMintLegPurpose,
    pub generation: i64,
    pub policy_account: String,
    pub expected_effect: CrossMintExpectedEffect,
    pub expected_balance_anchors: CrossMintBalanceAnchors,
    pub submission: SignedRouteSubmissionInput,
}

#[derive(Clone, Debug)]
pub struct CrossMintLegReconciliationInput {
    pub finalized_slot: i64,
    pub effect: CrossMintReconciledEffect,
    pub reconciled_balance_anchors: CrossMintBalanceAnchors,
}

#[derive(Clone, Debug)]
pub struct CrossMintFallbackCapacityInput {
    pub target: TargetCapacityProjection,
}

#[derive(Clone, Debug)]
pub struct CrossMintMovementCloseInput {
    pub outcome: CrossMintTerminalOutcome,
    pub observed_slot: i64,
    pub reason: String,
    pub evidence: Value,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", deny_unknown_fields)]
pub struct CrossMintEarnPolicyBinding {
    pub policy_account: String,
    pub observed_slot: u64,
    pub observed_signature: String,
    pub source_commitment: String,
    pub constraint_index: u8,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", deny_unknown_fields)]
pub struct CrossMintSwapPolicyBinding {
    pub policy_account: String,
    pub source_shard: String,
    pub enrollment_generation: i64,
    pub observed_slot: u64,
    pub observed_signature: String,
    pub source_commitment: String,
    pub max_slippage_bps: u16,
    pub daily_source_mint_spending_cap: u64,
    pub manifest_fingerprint: String,
}

/// The single immutable policy contract shared by planner, store, and worker.
/// Fields fixed by the V1 generalized policy ABI are deliberately absent: the
/// two Jupiter dialect indexes and token programs are derived at chain readback
/// instead of being copied through JSON and trusted as independent facts.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", deny_unknown_fields)]
pub struct CrossMintPolicyBindings {
    pub settings: String,
    pub vault_index: u8,
    pub vault_pubkey: String,
    pub delegated_signer: String,
    pub withdraw: CrossMintEarnPolicyBinding,
    pub swap: CrossMintSwapPolicyBinding,
    pub deposit: CrossMintEarnPolicyBinding,
}

impl CrossMintPolicyBindings {
    pub fn from_execution_plan(execution_plan: &Value) -> Result<Self, OrchestratorError> {
        let value = execution_plan.get("policy_bindings").ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "cross-mint execution plan lacks immutable policy bindings".to_owned(),
            )
        })?;
        let bindings: Self = serde_json::from_value(value.clone()).map_err(|error| {
            OrchestratorError::StoreInvariant(format!(
                "cross-mint execution plan has invalid policy bindings: {error}"
            ))
        })?;
        bindings.validate()?;
        Ok(bindings)
    }

    pub fn validate(&self) -> Result<(), OrchestratorError> {
        let finalized = [&self.withdraw, &self.deposit]
            .into_iter()
            .all(|binding| binding.source_commitment == "finalized");
        if self.settings.trim().is_empty()
            || self.vault_pubkey.trim().is_empty()
            || self.delegated_signer.trim().is_empty()
            || self.withdraw.policy_account.trim().is_empty()
            || self.swap.policy_account.trim().is_empty()
            || self.deposit.policy_account.trim().is_empty()
            || self.withdraw.policy_account == self.swap.policy_account
            || self.deposit.policy_account == self.swap.policy_account
            || self.withdraw.observed_slot == 0
            || self.swap.observed_slot == 0
            || self.deposit.observed_slot == 0
            || self.withdraw.constraint_index != 0
            || self.deposit.constraint_index != 1
            || !finalized
            || !matches!(
                self.swap.source_commitment.as_str(),
                "confirmed" | "finalized"
            )
            || !matches!(self.swap.source_shard.as_str(), "classic" | "token_2022")
            || self.swap.enrollment_generation <= 0
            || self.swap.max_slippage_bps == 0
            || self.swap.max_slippage_bps > 10_000
            || self.swap.daily_source_mint_spending_cap == 0
            || self.swap.manifest_fingerprint.trim().is_empty()
        {
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint policy bindings violate the generalized V1 contract".to_owned(),
            ));
        }
        Ok(())
    }
}

#[derive(Clone, Debug)]
pub struct CrossMintMovementActivationInput {
    pub capacity: TargetCapacityReservationInput,
    /// Exact compiled fee of the already-built withdrawal that will be
    /// published as generation one after activation.
    pub initial_withdraw_compiled_fee_lamports: i64,
    /// Fresh finalized policy readback plus exact-pair Jupiter build evidence.
    /// The store persists this atomically with movement activation.
    pub preflight_certification: Value,
    /// Exact finalized withdraw, swap, and deposit policy rows bound by the
    /// immutable plan. Withdraw and deposit may intentionally be different
    /// policy shards when their mints use different token programs.
    pub policy_bindings: CrossMintPolicyBindings,
}

impl NeonSqlClient {
    /// Missing control rows are fail-closed for new withdrawals and fail-open
    /// for recovery, so a rollout cannot strand already-withdrawn custody.
    pub async fn cross_mint_movement_gates(
        &self,
        cluster: &str,
    ) -> Result<CrossMintMovementGates, OrchestratorError> {
        if cluster.trim().is_empty() {
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint movement gates require a cluster".to_owned(),
            ));
        }
        let row = sqlx::query(
            r#"
            SELECT start_new_movements, continue_or_recover_existing,
                   generation, updated_at
            FROM loyal_yield.cross_mint_movement_controls
            WHERE cluster = $1
            "#,
        )
        .bind(cluster)
        .fetch_optional(self.pool())
        .await?;
        Ok(match row {
            Some(row) => CrossMintMovementGates {
                cluster: cluster.to_owned(),
                start_new_movements: row.try_get("start_new_movements")?,
                continue_or_recover_existing: row.try_get("continue_or_recover_existing")?,
                generation: row.try_get("generation")?,
                updated_at: Some(row.try_get("updated_at")?),
            },
            None => CrossMintMovementGates {
                cluster: cluster.to_owned(),
                start_new_movements: false,
                continue_or_recover_existing: true,
                generation: 0,
                updated_at: None,
            },
        })
    }

    pub async fn cross_mint_movement(
        &self,
        decision_id: DecisionId,
    ) -> Result<CrossMintMovementRecord, OrchestratorError> {
        let row = sqlx::query(
            r#"
            SELECT decision.*, opportunity.id AS opportunity_id,
                   opportunity.cluster
            FROM loyal_yield.rebalance_decisions decision
            JOIN loyal_yield.rebalance_opportunities opportunity
              ON opportunity.decision_id = decision.id
            WHERE decision.id = $1
              AND decision.movement_route = 'cross_mint_jupiter'
            "#,
        )
        .bind(decision_id.as_i64())
        .fetch_one(self.pool())
        .await?;
        cross_mint_movement_from_row(&row)
    }

    /// Atomically creates the one active movement from a fenced immutable
    /// opportunity and reserves target capacity before any withdrawal is
    /// signed. A crash after commit is recovered by the continuation claimant.
    pub async fn activate_cross_mint_movement(
        &self,
        opportunity_lease: &RebalanceOpportunityLease,
        input: CrossMintMovementActivationInput,
    ) -> Result<CrossMintMovementRecord, OrchestratorError> {
        if input.initial_withdraw_compiled_fee_lamports <= 0 {
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint activation requires the exact positive withdrawal fee".to_owned(),
            ));
        }
        if !input.preflight_certification.is_object()
            || input
                .preflight_certification
                .as_object()
                .is_some_and(|value| value.is_empty())
        {
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint activation requires nonempty object preflight certification".to_owned(),
            ));
        }
        let bindings = &input.policy_bindings;
        bindings.validate()?;
        let withdraw_observed_slot =
            i64::try_from(bindings.withdraw.observed_slot).map_err(|_| {
                OrchestratorError::StoreInvariant(
                    "cross-mint withdraw policy slot does not fit PostgreSQL BIGINT".to_owned(),
                )
            })?;
        let deposit_observed_slot =
            i64::try_from(bindings.deposit.observed_slot).map_err(|_| {
                OrchestratorError::StoreInvariant(
                    "cross-mint deposit policy slot does not fit PostgreSQL BIGINT".to_owned(),
                )
            })?;
        let swap_observed_slot = i64::try_from(bindings.swap.observed_slot).map_err(|_| {
            OrchestratorError::StoreInvariant(
                "cross-mint swap policy slot does not fit PostgreSQL BIGINT".to_owned(),
            )
        })?;
        let swap_daily_source_mint_spending_cap =
            i64::try_from(bindings.swap.daily_source_mint_spending_cap).map_err(|_| {
                OrchestratorError::StoreInvariant(
                    "cross-mint daily source-mint cap does not fit PostgreSQL BIGINT".to_owned(),
                )
            })?;
        let mut tx = self.pool().begin().await?;

        if let Some(decision_id) = sqlx::query_scalar::<_, i64>(
            r#"
            SELECT decision_id
            FROM loyal_yield.rebalance_opportunities
            WHERE id = $1 AND decision_id IS NOT NULL
            "#,
        )
        .bind(opportunity_lease.opportunity.id)
        .fetch_optional(&mut *tx)
        .await?
        {
            let movement =
                cross_mint_movement_in_connection(&mut tx, DecisionId(decision_id)).await?;
            tx.commit().await?;
            return Ok(movement);
        }

        lock_cross_mint_control_key(&mut tx, &opportunity_lease.opportunity.cluster).await?;
        let control = sqlx::query(
            r#"
            SELECT start_new_movements, generation
            FROM loyal_yield.cross_mint_movement_controls
            WHERE cluster = $1
            FOR SHARE
            "#,
        )
        .bind(&opportunity_lease.opportunity.cluster)
        .fetch_optional(&mut *tx)
        .await?;
        let Some(control) = control else {
            return Err(OrchestratorError::StoreInvariant(
                "starting new cross-mint movements is disabled".to_owned(),
            ));
        };
        if !control.try_get::<bool, _>("start_new_movements")? {
            return Err(OrchestratorError::StoreInvariant(
                "starting new cross-mint movements is disabled".to_owned(),
            ));
        }
        let activation_control_generation: i64 = control.try_get("generation")?;
        let opportunity = sqlx::query(
            r#"
            SELECT opportunity.*
            FROM loyal_yield.rebalance_opportunities opportunity
            JOIN loyal_yield.optimizer_epochs epoch
              ON epoch.id = opportunity.optimizer_epoch_id
             AND epoch.cluster = opportunity.cluster
            JOIN loyal_yield.managed_vaults vault
              ON vault.id = opportunity.vault_id
             AND vault.active
             AND vault.settings = $4
             AND vault.vault_index = $5
             AND vault.vault_pubkey = $6
            JOIN loyal_yield.route_policies withdraw_policy
              ON withdraw_policy.active
             AND withdraw_policy.finalized_eligible
             AND withdraw_policy.source_commitment = 'finalized'
             AND withdraw_policy.cluster = opportunity.cluster
             AND withdraw_policy.settings = $4
             AND withdraw_policy.vault_index = $5
             AND withdraw_policy.vault_pubkey = $6
             AND withdraw_policy.delegated_signers = ARRAY[$7]::TEXT[]
             AND withdraw_policy.threshold = 1
             AND withdraw_policy.policy_account = $8
             AND withdraw_policy.last_seen_slot >= $9
             AND 'same_mint_kamino' = ANY(withdraw_policy.route_modes)
             AND opportunity.source_liquidity_mint = ANY(withdraw_policy.stable_mints)
             AND opportunity.source_liquidity_mint = ANY(withdraw_policy.kamino_liquidity_mints)
            JOIN loyal_yield.route_policies deposit_policy
              ON deposit_policy.active
             AND deposit_policy.finalized_eligible
             AND deposit_policy.source_commitment = 'finalized'
             AND deposit_policy.cluster = opportunity.cluster
             AND deposit_policy.settings = $4
             AND deposit_policy.authority = withdraw_policy.authority
             AND deposit_policy.vault_index = $5
             AND deposit_policy.vault_pubkey = $6
             AND deposit_policy.delegated_signers = ARRAY[$7]::TEXT[]
             AND deposit_policy.threshold = 1
             AND deposit_policy.policy_account = $10
             AND deposit_policy.last_seen_slot >= $11
             AND 'same_mint_kamino' = ANY(deposit_policy.route_modes)
             AND opportunity.target_liquidity_mint = ANY(deposit_policy.stable_mints)
             AND opportunity.target_liquidity_mint = ANY(deposit_policy.kamino_liquidity_mints)
            JOIN loyal_yield.cross_mint_swap_policies swap_policy
              ON swap_policy.cluster = opportunity.cluster
             AND swap_policy.settings = $4
             AND swap_policy.authority = withdraw_policy.authority
             AND swap_policy.vault_index = $5
             AND swap_policy.vault_pubkey = $6
             AND swap_policy.delegated_signer = $7
             AND swap_policy.policy_account = $12
             AND swap_policy.last_seen_slot >= $13
             AND swap_policy.source_shard = $14
             AND swap_policy.max_slippage_bps = $15
             AND swap_policy.daily_source_mint_spending_cap = $16
             AND swap_policy.manifest_fingerprint = $17
             AND swap_policy.active
             AND swap_policy.start_eligible
             AND swap_policy.source_commitment IN ('confirmed', 'finalized')
             AND swap_policy.last_mutation IN ('create', 'update')
            JOIN loyal_yield.cross_mint_vault_opt_ins opt_in
              ON opt_in.enabled = TRUE
             AND opt_in.cluster = swap_policy.cluster
             AND opt_in.settings = swap_policy.settings
             AND opt_in.vault_index = swap_policy.vault_index
             AND opt_in.vault_pubkey = swap_policy.vault_pubkey
             AND opt_in.generation = $18
             AND (
                 (
                     swap_policy.source_shard = 'classic'
                     AND opt_in.classic_policy_account = swap_policy.policy_account
                     AND opt_in.classic_policy_seed = swap_policy.policy_seed
                 )
                 OR (
                     swap_policy.source_shard = 'token_2022'
                     AND opt_in.token_2022_policy_account = swap_policy.policy_account
                     AND opt_in.token_2022_policy_seed = swap_policy.policy_seed
                 )
             )
            JOIN loyal_yield.cross_mint_swap_policies sibling_policy
              ON sibling_policy.cluster = swap_policy.cluster
             AND sibling_policy.settings = swap_policy.settings
             AND sibling_policy.authority = swap_policy.authority
             AND sibling_policy.vault_index = swap_policy.vault_index
             AND sibling_policy.vault_pubkey = swap_policy.vault_pubkey
             AND sibling_policy.delegated_signer = swap_policy.delegated_signer
             AND sibling_policy.policy_account <> swap_policy.policy_account
             AND sibling_policy.source_shard <> swap_policy.source_shard
             AND sibling_policy.active
             AND sibling_policy.start_eligible
             AND sibling_policy.source_commitment IN ('confirmed', 'finalized')
             AND sibling_policy.last_mutation IN ('create', 'update')
             AND sibling_policy.max_slippage_bps = swap_policy.max_slippage_bps
             AND sibling_policy.daily_source_mint_spending_cap =
                 swap_policy.daily_source_mint_spending_cap
             AND 2 = (
                 SELECT count(DISTINCT sibling.source_shard)
                 FROM loyal_yield.cross_mint_swap_policies sibling
                 WHERE sibling.cluster = swap_policy.cluster
                   AND sibling.settings = swap_policy.settings
                   AND sibling.authority = swap_policy.authority
                   AND sibling.vault_index = swap_policy.vault_index
                   AND sibling.vault_pubkey = swap_policy.vault_pubkey
                   AND sibling.delegated_signer = swap_policy.delegated_signer
                   AND sibling.active
                   AND sibling.start_eligible
                   AND sibling.source_commitment IN ('confirmed', 'finalized')
                   AND sibling.last_mutation IN ('create', 'update')
                   AND sibling.max_slippage_bps = swap_policy.max_slippage_bps
                   AND sibling.daily_source_mint_spending_cap =
                       swap_policy.daily_source_mint_spending_cap
             )
            WHERE opportunity.id = $1
              AND opportunity.opportunity_state = 'leased'
              AND opportunity.lease_kind = 'execute'
              AND opportunity.lease_owner = $2
              AND opportunity.fencing_token = $3
              AND opportunity.lease_expires_at > now()
              AND opportunity.source_reserve IS NOT NULL
              AND opportunity.source_liquidity_mint
                    <> opportunity.target_liquidity_mint
              AND opportunity.execution_plan ->> 'kind' = 'cross_mint_jupiter'
            FOR UPDATE OF opportunity, vault, withdraw_policy, deposit_policy,
                swap_policy, sibling_policy, opt_in
            "#,
        )
        .bind(opportunity_lease.opportunity.id)
        .bind(&opportunity_lease.owner)
        .bind(opportunity_lease.fencing_token)
        .bind(&bindings.settings)
        .bind(i16::from(bindings.vault_index))
        .bind(&bindings.vault_pubkey)
        .bind(&bindings.delegated_signer)
        .bind(&bindings.withdraw.policy_account)
        .bind(withdraw_observed_slot)
        .bind(&bindings.deposit.policy_account)
        .bind(deposit_observed_slot)
        .bind(&bindings.swap.policy_account)
        .bind(swap_observed_slot)
        .bind(&bindings.swap.source_shard)
        .bind(i32::from(bindings.swap.max_slippage_bps))
        .bind(swap_daily_source_mint_spending_cap)
        .bind(&bindings.swap.manifest_fingerprint)
        .bind(bindings.swap.enrollment_generation)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "cross-mint reserve-source opportunity lost an opted-in finalized policy binding, is mismatched, or was fenced".to_owned(),
            )
        })?;
        let vault_id: i64 = opportunity.try_get("vault_id")?;
        let active_decision_exists: bool = sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1 FROM loyal_yield.rebalance_decisions
                WHERE vault_id = $1
                  AND status IN ('planned', 'simulating', 'ready', 'submitted', 'confirming')
            )
            "#,
        )
        .bind(vault_id)
        .fetch_one(&mut *tx)
        .await?;
        if active_decision_exists {
            return Err(OrchestratorError::StoreInvariant(
                "vault acquired an active decision before cross-mint activation".to_owned(),
            ));
        }

        let reservation = NeonSqlClient::reserve_target_capacity_in_connection(
            &mut tx,
            opportunity_lease,
            &input.capacity,
            input.initial_withdraw_compiled_fee_lamports,
        )
        .await?;
        let source_reserve: String = opportunity.try_get("source_reserve")?;
        let target_reserve: String = opportunity.try_get("target_reserve")?;
        let source_mint: String = opportunity.try_get("source_liquidity_mint")?;
        let target_mint: String = opportunity.try_get("target_liquidity_mint")?;
        let amount_raw: i64 = opportunity.try_get("amount_raw")?;
        let source_snapshot_id: Option<i64> = opportunity.try_get("source_snapshot_id")?;
        let source_apy_bps: i64 = opportunity.try_get("source_apy_bps")?;
        let target_apy_bps: i64 = opportunity.try_get("target_apy_bps")?;
        let estimated_edge_bps: i64 = opportunity.try_get("estimated_edge_bps")?;
        let estimated_cost_lamports: i64 = opportunity.try_get("estimated_cost_lamports")?;
        let execution_plan: Value = opportunity.try_get("execution_plan")?;
        let mut hasher = Sha256::new();
        hasher.update(b"cross-mint-movement-v1");
        hasher.update(opportunity_lease.opportunity.id.to_le_bytes());
        hasher.update(
            opportunity_lease
                .opportunity
                .attempt_generation
                .to_le_bytes(),
        );
        let idempotency_key = format!("{:x}", hasher.finalize());
        let decision_id: i64 = sqlx::query_scalar(
            r#"
            INSERT INTO loyal_yield.rebalance_decisions
                (vault_id, source_snapshot_id, status, source_reserve,
                 target_reserve, liquidity_mint, source_liquidity_mint,
                 target_liquidity_mint, amount_raw, source_apy_bps,
                 target_apy_bps, estimated_edge_bps,
                 estimated_cost_lamports, decision_reason, execution_plan,
                 idempotency_key, movement_route, active_target_reserve,
                 custody_mint, custody_amount_raw, custody_account,
                 custody_version, continuation_available_at,
                 cross_mint_activation_control_generation,
                 cross_mint_preflight_certification)
            VALUES
                ($1, $2, 'confirming'::loyal_yield.decision_status,
                 $3, $4, NULL, $5, $6, $7, $8, $9, $10, $11,
                 'target_supply_apy_exceeds_source'::loyal_yield.decision_reason,
                 $12, $13, 'cross_mint_jupiter', $4, $5, $7, $3, 0, now(), $14,
                 $15)
            RETURNING id
            "#,
        )
        .bind(vault_id)
        .bind(source_snapshot_id)
        .bind(&source_reserve)
        .bind(&target_reserve)
        .bind(&source_mint)
        .bind(&target_mint)
        .bind(amount_raw)
        .bind(source_apy_bps)
        .bind(target_apy_bps)
        .bind(estimated_edge_bps)
        .bind(estimated_cost_lamports)
        .bind(execution_plan)
        .bind(idempotency_key)
        .bind(activation_control_generation)
        .bind(input.preflight_certification)
        .fetch_one(&mut *tx)
        .await?;
        let linked = sqlx::query(
            r#"
            UPDATE loyal_yield.rebalance_opportunities
            SET opportunity_state = 'decision_created',
                decision_id = $2,
                lease_kind = NULL,
                lease_owner = NULL,
                lease_expires_at = NULL,
                terminal_reason = NULL,
                updated_at = now()
            WHERE id = $1
              AND opportunity_state = 'leased'
              AND lease_kind = 'execute'
              AND lease_owner = $3
              AND fencing_token = $4
              AND lease_expires_at > now()
            RETURNING id
            "#,
        )
        .bind(opportunity_lease.opportunity.id)
        .bind(decision_id)
        .bind(&opportunity_lease.owner)
        .bind(opportunity_lease.fencing_token)
        .fetch_optional(&mut *tx)
        .await?;
        if linked.is_none() {
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint opportunity lost its activation fence".to_owned(),
            ));
        }
        let attached = sqlx::query(
            r#"
            UPDATE loyal_yield.target_capacity_reservations
            SET decision_id = $2,
                state_version = state_version + 1,
                updated_at = now()
            WHERE id = $1
              AND decision_id IS NULL
              AND signed_submission_id IS NULL
              AND reservation_state = 'active'
            "#,
        )
        .bind(reservation.id)
        .bind(decision_id)
        .execute(&mut *tx)
        .await?;
        if attached.rows_affected() != 1 {
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint activation did not attach movement-owned capacity".to_owned(),
            ));
        }
        let movement = cross_mint_movement_in_connection(&mut tx, DecisionId(decision_id)).await?;
        tx.commit().await?;
        Ok(movement)
    }

    pub async fn claim_cross_mint_continuation(
        &self,
        cluster: &str,
        owner: &str,
        lease_seconds: i64,
    ) -> Result<Option<CrossMintContinuationLease>, OrchestratorError> {
        if cluster.trim().is_empty()
            || owner.trim().is_empty()
            || !(10..=300).contains(&lease_seconds)
        {
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint continuation claim requires identity and a 10-300 second lease"
                    .to_owned(),
            ));
        }
        let mut tx = self.pool().begin().await?;
        lock_cross_mint_control_key(&mut tx, cluster).await?;
        let control = sqlx::query(
            r#"
            SELECT continue_or_recover_existing, generation
            FROM loyal_yield.cross_mint_movement_controls
            WHERE cluster = $1
            FOR SHARE
            "#,
        )
        .bind(cluster)
        .fetch_optional(&mut *tx)
        .await?;
        let (continue_enabled, control_generation) = match control {
            Some(row) => (
                row.try_get::<bool, _>("continue_or_recover_existing")?,
                row.try_get::<i64, _>("generation")?,
            ),
            None => (true, 0),
        };
        if !continue_enabled {
            tx.commit().await?;
            return Ok(None);
        }

        let row = sqlx::query(
            r#"
            WITH candidate AS (
                SELECT decision.id
                FROM loyal_yield.rebalance_decisions decision
                JOIN loyal_yield.rebalance_opportunities opportunity
                  ON opportunity.decision_id = decision.id
                 AND opportunity.opportunity_state = 'decision_created'
                 AND opportunity.cluster = $1
                WHERE decision.movement_route = 'cross_mint_jupiter'
                  AND decision.status = 'confirming'::loyal_yield.decision_status
                  AND decision.terminal_outcome IS NULL
                  AND decision.continuation_available_at <= now()
                  AND (
                      decision.continuation_lease_expires_at IS NULL
                      OR decision.continuation_lease_expires_at <= now()
                  )
                  AND NOT EXISTS (
                      SELECT 1
                      FROM loyal_yield.signed_route_submissions submission
                      WHERE submission.decision_id = decision.id
                        AND submission.submission_state NOT IN (
                            'reconciled', 'expired', 'failed'
                        )
                  )
                ORDER BY decision.continuation_available_at,
                         decision.created_at, decision.id
                FOR UPDATE OF decision SKIP LOCKED
                LIMIT 1
            )
            UPDATE loyal_yield.rebalance_decisions decision
            SET continuation_lease_owner = $2,
                continuation_lease_expires_at = now()
                    + make_interval(secs => $3::INTEGER),
                continuation_fencing_token = continuation_fencing_token + 1,
                continuation_attempt_count = continuation_attempt_count + 1,
                continuation_control_generation = $4,
                updated_at = now()
            FROM candidate
            WHERE decision.id = candidate.id
            RETURNING decision.id,
                      decision.continuation_fencing_token,
                      decision.continuation_lease_expires_at
            "#,
        )
        .bind(cluster)
        .bind(owner)
        .bind(i32::try_from(lease_seconds).map_err(|_| {
            OrchestratorError::StoreInvariant(
                "cross-mint continuation lease does not fit PostgreSQL INTEGER".to_owned(),
            )
        })?)
        .bind(control_generation)
        .fetch_optional(&mut *tx)
        .await?;
        let Some(row) = row else {
            tx.commit().await?;
            return Ok(None);
        };
        let decision_id = DecisionId(row.try_get("id")?);
        let fencing_token: i64 = row.try_get("continuation_fencing_token")?;
        let expires_at: DateTime<Utc> = row.try_get("continuation_lease_expires_at")?;
        let movement = cross_mint_movement_in_connection(&mut tx, decision_id).await?;
        tx.commit().await?;
        Ok(Some(CrossMintContinuationLease {
            movement,
            owner: owner.to_owned(),
            fencing_token,
            control_generation,
            expires_at,
        }))
    }

    /// Appends exact signed bytes for one movement leg. No optimizer-epoch
    /// lease is reused: the active movement's continuation fence is authority.
    pub async fn append_cross_mint_leg(
        &self,
        lease: &CrossMintContinuationLease,
        input: CrossMintLegPublicationInput,
    ) -> Result<SignedRouteSubmissionRecord, OrchestratorError> {
        validate_publication_input(lease, &input)?;
        let mut tx = self.pool().begin().await?;
        let gates = lock_cross_mint_publication_gates(&mut tx, lease).await?;
        let movement = lock_cross_mint_movement_lease(&mut tx, lease).await?;
        if input.leg == CrossMintMovementLeg::Withdraw
            && input.generation == 1
            && movement.phase == CrossMintCustodyPhase::SourceReserve
        {
            let activation_generation: Option<i64> = sqlx::query_scalar(
                "SELECT cross_mint_activation_control_generation FROM loyal_yield.rebalance_decisions WHERE id = $1",
            )
            .bind(movement.decision_id.as_i64())
            .fetch_one(&mut *tx)
            .await?;
            if !gates.start_new_movements || activation_generation != Some(gates.generation) {
                return Err(OrchestratorError::StoreInvariant(
                    "initial cross-mint withdrawal lost its activation control generation"
                        .to_owned(),
                ));
            }
            validate_initial_cross_mint_policy_bindings(&mut tx, &movement, &input).await?;
        }
        validate_next_leg(&mut tx, &movement, &input).await?;

        let signed_hash = format!("{:x}", Sha256::digest(&input.submission.signed_transaction));
        if !signed_hash.eq_ignore_ascii_case(&input.submission.signed_transaction_hash) {
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint signed transaction hash does not match exact bytes".to_owned(),
            ));
        }
        let writable = canonical_writable_account_keys(&input.submission.writable_account_keys)?;
        let conflicts = canonical_conflict_account_keys(&input.submission.conflict_account_keys)?;
        if !writable.contains(&input.submission.fee_payer) {
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint fee payer is absent from writable evidence".to_owned(),
            ));
        }

        let existing_fees: i64 = sqlx::query_scalar(
            r#"
            SELECT COALESCE(sum(compiled_fee_lamports), 0)::BIGINT
            FROM loyal_yield.signed_route_submissions
            WHERE decision_id = $1
            "#,
        )
        .bind(movement.decision_id.as_i64())
        .fetch_one(&mut *tx)
        .await?;
        let opportunity_fee_cap: i64 = sqlx::query_scalar(
            "SELECT estimated_cost_lamports FROM loyal_yield.rebalance_opportunities WHERE id = $1",
        )
        .bind(movement.opportunity_id)
        .fetch_one(&mut *tx)
        .await?;
        if existing_fees
            .checked_add(input.submission.compiled_fee_lamports)
            .is_none_or(|total| total > opportunity_fee_cap)
        {
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint leg fees exceed the movement's immutable total fee cap".to_owned(),
            ));
        }

        let conflict_expiry = Utc::now() + Duration::minutes(10);
        for key in &conflicts {
            let acquired = sqlx::query_scalar::<_, String>(
                r#"
                INSERT INTO loyal_yield.route_account_conflict_leases AS conflict
                    (cluster, writable_account_key, opportunity_id, lease_owner,
                     fencing_token, expires_at)
                VALUES ($1, $2, $3, $4, $5, $6)
                ON CONFLICT (cluster, writable_account_key) DO UPDATE
                SET opportunity_id = EXCLUDED.opportunity_id,
                    lease_owner = EXCLUDED.lease_owner,
                    fencing_token = EXCLUDED.fencing_token,
                    expires_at = EXCLUDED.expires_at,
                    submission_id = NULL,
                    updated_at = now()
                WHERE conflict.submission_id IS NULL
                  AND (
                      conflict.expires_at <= now()
                      OR (
                          conflict.opportunity_id = EXCLUDED.opportunity_id
                          AND conflict.lease_owner = EXCLUDED.lease_owner
                          AND conflict.fencing_token = EXCLUDED.fencing_token
                      )
                  )
                RETURNING writable_account_key
                "#,
            )
            .bind(&movement.cluster)
            .bind(key)
            .bind(movement.opportunity_id)
            .bind(&lease.owner)
            .bind(lease.fencing_token)
            .bind(conflict_expiry)
            .fetch_optional(&mut *tx)
            .await?;
            if acquired.is_none() {
                return Err(OrchestratorError::StoreInvariant(format!(
                    "cross-mint conflict key {key:?} is owned by another movement"
                )));
            }
        }

        let expected_effect = serde_json::to_value(&input.expected_effect).map_err(|error| {
            OrchestratorError::StoreInvariant(format!(
                "cross-mint expected effect did not serialize: {error}"
            ))
        })?;
        let expected_balance_anchors = serde_json::to_value(&input.expected_balance_anchors)
            .map_err(|error| {
                OrchestratorError::StoreInvariant(format!(
                    "cross-mint expected balance anchors did not serialize: {error}"
                ))
            })?;
        let row = sqlx::query(
            r#"
            INSERT INTO loyal_yield.signed_route_submissions
                (cluster, semantic_key, opportunity_id, decision_id,
                 signed_transaction, signed_transaction_hash, message_hash,
                 transaction_signature, recent_blockhash,
                 last_valid_block_height, source_snapshot_id,
                 optimizer_epoch_id, alt_requirements_fingerprint,
                 alt_selection_fingerprint, alt_mutation_epochs, fee_payer,
                 fee_payer_kind, compiled_fee_lamports,
                 writable_account_keys, conflict_account_keys,
                 executor_owner, executor_fencing_token,
                 movement_leg, leg_purpose, leg_generation,
                 required_commitment, policy_account, expected_effect,
                 expected_balance_anchors)
            VALUES
                ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
                 $11, $12, $13, $14, $15, $16, $17, $18, $19, $20,
                 $21, $22, $23, $24, $25, 'finalized', $26, $27, $28)
            RETURNING *
            "#,
        )
        .bind(&movement.cluster)
        .bind(&input.submission.semantic_key)
        .bind(movement.opportunity_id)
        .bind(movement.decision_id.as_i64())
        .bind(&input.submission.signed_transaction)
        .bind(&signed_hash)
        .bind(&input.submission.message_hash)
        .bind(&input.submission.transaction_signature)
        .bind(&input.submission.recent_blockhash)
        .bind(input.submission.last_valid_block_height)
        .bind(input.submission.source_snapshot_id.map(SnapshotId::as_i64))
        .bind(input.submission.optimizer_epoch_id)
        .bind(&input.submission.alt_requirements_fingerprint)
        .bind(&input.submission.alt_selection_fingerprint)
        .bind(&input.submission.alt_mutation_epochs)
        .bind(&input.submission.fee_payer)
        .bind(input.submission.fee_payer_kind.as_str())
        .bind(input.submission.compiled_fee_lamports)
        .bind(&writable)
        .bind(&conflicts)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(input.leg.as_str())
        .bind(input.purpose.as_str())
        .bind(input.generation)
        .bind(&input.policy_account)
        .bind(expected_effect)
        .bind(expected_balance_anchors)
        .fetch_one(&mut *tx)
        .await?;
        let submission = signed_route_submission_from_row(&row)?;

        reserve_fee_only_route_payer_spend(&mut tx, &input.submission, submission.id).await?;
        let attached = sqlx::query(
            r#"
            UPDATE loyal_yield.route_account_conflict_leases
            SET submission_id = $5,
                expires_at = GREATEST(expires_at, now() + interval '10 minutes'),
                updated_at = now()
            WHERE cluster = $1
              AND opportunity_id = $2
              AND lease_owner = $3
              AND fencing_token = $4
              AND writable_account_key = ANY($6)
              AND submission_id IS NULL
            "#,
        )
        .bind(&movement.cluster)
        .bind(movement.opportunity_id)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(submission.id)
        .bind(&conflicts)
        .execute(&mut *tx)
        .await?;
        if attached.rows_affected() != conflicts.len() as u64 {
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint leg did not assume its exact conflict set".to_owned(),
            ));
        }
        sqlx::query(
            r#"
            UPDATE loyal_yield.rebalance_decisions
            SET continuation_available_at = NULL,
                continuation_lease_owner = NULL,
                continuation_lease_expires_at = NULL,
                signature = NULL,
                confirmed_slot = NULL,
                updated_at = now()
            WHERE id = $1
              AND continuation_lease_owner = $2
              AND continuation_fencing_token = $3
              AND continuation_lease_expires_at > now()
            "#,
        )
        .bind(movement.decision_id.as_i64())
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .execute(&mut *tx)
        .await?;
        tx.commit().await?;
        Ok(submission)
    }

    /// Atomically writes the finalized effect receipt, advances attributed
    /// custody, and only then exposes continuation or terminal completion.
    pub async fn reconcile_cross_mint_leg(
        &self,
        lease: &SignedRouteSubmissionLease,
        input: CrossMintLegReconciliationInput,
    ) -> Result<CrossMintMovementRecord, OrchestratorError> {
        if input.finalized_slot < 0 {
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint reconciliation requires a finalized slot".to_owned(),
            ));
        }
        validate_reconciled_effect(&input.effect)?;
        let mut tx = self.pool().begin().await?;
        let submission = sqlx::query(
            r#"
            SELECT *
            FROM loyal_yield.signed_route_submissions
            WHERE id = $1
              AND decision_id IS NOT NULL
              AND movement_leg <> 'route'
              AND required_commitment = 'finalized'
              AND submission_state = 'reconciliation_pending'
              AND reconciled_effect IS NULL
              AND finalized_slot = $4
              AND confirmation_lease_owner = $2
              AND confirmation_fencing_token = $3
              AND confirmation_lease_expires_at > now()
            FOR UPDATE
            "#,
        )
        .bind(lease.submission.id)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(input.finalized_slot)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "cross-mint reconciliation is stale, already written, or fenced".to_owned(),
            )
        })?;
        let decision_id = DecisionId(submission.try_get::<i64, _>("decision_id")?);
        let confirmed_slot: i64 = submission.try_get("confirmed_slot")?;
        if input.finalized_slot < confirmed_slot {
            return Err(OrchestratorError::StoreInvariant(
                "finalized slot precedes the observed signature slot".to_owned(),
            ));
        }
        let movement = lock_cross_mint_movement(&mut tx, decision_id).await?;
        let leg = CrossMintMovementLeg::parse(submission.try_get("movement_leg")?)?;
        let purpose = submission.try_get::<String, _>("leg_purpose")?;
        let expected: CrossMintExpectedEffect = serde_json::from_value(
            submission.try_get::<Value, _>("expected_effect")?,
        )
        .map_err(|error| {
            OrchestratorError::StoreInvariant(format!(
                "stored cross-mint expected effect is invalid: {error}"
            ))
        })?;
        let expected_balance_anchors: CrossMintBalanceAnchors =
            serde_json::from_value(submission.try_get::<Value, _>("expected_balance_anchors")?)
                .map_err(|error| {
                    OrchestratorError::StoreInvariant(format!(
                        "stored cross-mint balance anchors are invalid: {error}"
                    ))
                })?;
        validate_balance_anchors(&input.reconciled_balance_anchors)?;
        validate_effect_against_movement(
            &movement,
            leg,
            &purpose,
            &expected,
            &expected_balance_anchors,
            &input.effect,
            &input.reconciled_balance_anchors,
        )?;

        let next_custody = next_custody(
            &movement,
            leg,
            &purpose,
            &input.effect,
            &input.reconciled_balance_anchors,
            input.finalized_slot,
        )?;
        let terminal_outcome = next_custody
            .terminal_outcome
            .map(CrossMintTerminalOutcome::as_str);
        let updated = sqlx::query(
            r#"
            UPDATE loyal_yield.rebalance_decisions
            SET custody_mint = $2,
                custody_amount_raw = $3,
                custody_account = $4,
                custody_observed_balance_raw = $5,
                custody_reconciled_slot = $6,
                custody_version = custody_version + 1,
                continuation_available_at = NULL,
                continuation_lease_owner = NULL,
                continuation_lease_expires_at = NULL,
                terminal_outcome = $7,
                terminal_evidence = $10,
                terminal_reason = $11,
                terminal_observed_slot = $12,
                status = CASE
                    WHEN $7 IS NULL THEN 'confirming'::loyal_yield.decision_status
                    ELSE 'confirmed'::loyal_yield.decision_status
                END,
                signature = CASE WHEN $7 IS NULL THEN signature ELSE $8 END,
                confirmed_slot = CASE WHEN $7 IS NULL THEN confirmed_slot ELSE $6 END,
                updated_at = now()
            WHERE id = $1
              AND movement_route = 'cross_mint_jupiter'
              AND status = 'confirming'::loyal_yield.decision_status
              AND terminal_outcome IS NULL
              AND custody_version = $9
            RETURNING id
            "#,
        )
        .bind(decision_id.as_i64())
        .bind(&next_custody.mint)
        .bind(next_custody.amount_raw)
        .bind(&next_custody.account)
        .bind(next_custody.observed_balance_raw)
        .bind(input.finalized_slot)
        .bind(terminal_outcome)
        .bind(submission.try_get::<String, _>("transaction_signature")?)
        .bind(movement.custody_version)
        .bind(next_custody.terminal_evidence)
        .bind(next_custody.terminal_reason)
        .bind(next_custody.terminal_observed_slot)
        .fetch_optional(&mut *tx)
        .await?;
        if updated.is_none() {
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint custody changed during reconciliation".to_owned(),
            ));
        }

        let effect_json = serde_json::to_value(&input.effect).map_err(|error| {
            OrchestratorError::StoreInvariant(format!(
                "cross-mint reconciled effect did not serialize: {error}"
            ))
        })?;
        let reconciled_balance_anchors = serde_json::to_value(&input.reconciled_balance_anchors)
            .map_err(|error| {
                OrchestratorError::StoreInvariant(format!(
                    "cross-mint reconciled balance anchors did not serialize: {error}"
                ))
            })?;
        let debit = input.effect.debit.as_ref();
        let credit = input.effect.credit.as_ref();
        let reconciled = sqlx::query(
            r#"
            UPDATE loyal_yield.signed_route_submissions
            SET submission_state = 'reconciled',
                reconciled_slot = $4,
                reconciled_at = now(),
                reconciled_effect = $5,
                reconciled_balance_anchors = $6,
                effect_debit_mint = $7,
                effect_debit_account = $8,
                effect_debit_amount_raw = $9,
                effect_credit_mint = $10,
                effect_credit_account = $11,
                effect_credit_amount_raw = $12,
                confirmation_lease_owner = NULL,
                confirmation_lease_expires_at = NULL,
                error_detail = NULL,
                updated_at = now()
            WHERE id = $1
              AND submission_state = 'reconciliation_pending'
              AND finalized_slot = $4
              AND finalized_at IS NOT NULL
              AND reconciled_effect IS NULL
              AND confirmation_lease_owner = $2
              AND confirmation_fencing_token = $3
              AND confirmation_lease_expires_at > now()
            RETURNING id
            "#,
        )
        .bind(lease.submission.id)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(input.finalized_slot)
        .bind(effect_json)
        .bind(reconciled_balance_anchors)
        .bind(debit.map(|delta| delta.mint.as_str()))
        .bind(debit.map(|delta| delta.token_account.as_str()))
        .bind(debit.map(|delta| delta.amount_raw))
        .bind(credit.map(|delta| delta.mint.as_str()))
        .bind(credit.map(|delta| delta.token_account.as_str()))
        .bind(credit.map(|delta| delta.amount_raw))
        .fetch_optional(&mut *tx)
        .await?;
        if reconciled.is_none() {
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint effect receipt lost its reconciliation fence".to_owned(),
            ));
        }
        let movement = cross_mint_movement_in_connection(&mut tx, decision_id).await?;
        tx.commit().await?;
        Ok(movement)
    }

    /// Atomically moves the still-live capacity claim to another reserve of
    /// the target mint. Sunk swap economics are not re-applied here.
    pub async fn rebind_cross_mint_fallback_capacity(
        &self,
        lease: &CrossMintContinuationLease,
        input: CrossMintFallbackCapacityInput,
    ) -> Result<TargetCapacityReservationRecord, OrchestratorError> {
        let target = &input.target.observation;
        if target.cluster != lease.movement.cluster
            || target.liquidity_mint != lease.movement.target_mint
            || target.target_reserve.trim().is_empty()
            || target.observed_supply_usd_micros < 0
            || target.observed_slot < 0
            || target.maximum_inflight_usd_micros <= 0
            || input.target.telemetry_version < 0
        {
            return Err(OrchestratorError::StoreInvariant(
                "fallback capacity must be fresh capacity for the movement target mint".to_owned(),
            ));
        }
        let mut tx = self.pool().begin().await?;
        let movement = lock_cross_mint_movement_lease(&mut tx, lease).await?;
        if movement.phase != CrossMintCustodyPhase::TargetIdle {
            return Err(OrchestratorError::StoreInvariant(
                "target fallback is only valid while target-mint custody is idle".to_owned(),
            ));
        }
        let (current_reserve, current_generation): (String, i64) = sqlx::query_as(
            "SELECT target_reserve, reservation_generation FROM loyal_yield.target_capacity_reservations WHERE decision_id = $1",
        )
        .bind(movement.decision_id.as_i64())
        .fetch_one(&mut *tx)
        .await?;
        let mut reserves = vec![current_reserve.as_str(), target.target_reserve.as_str()];
        reserves.sort_unstable();
        reserves.dedup();
        let mut fallback_frontier = None;
        for reserve in reserves {
            let frontier = sqlx::query(
                r#"
                SELECT observed_supply_usd_micros, observed_slot,
                       maximum_inflight_usd_micros, telemetry_version
                FROM loyal_yield.target_capacity_frontiers
                WHERE cluster = $1 AND target_reserve = $2 AND liquidity_mint = $3
                FOR UPDATE
                "#,
            )
            .bind(&movement.cluster)
            .bind(reserve)
            .bind(&movement.target_mint)
            .fetch_one(&mut *tx)
            .await?;
            if reserve == target.target_reserve {
                fallback_frontier = Some(frontier);
            }
        }
        let fallback_frontier = fallback_frontier.ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "fallback target capacity frontier was not locked".to_owned(),
            )
        })?;
        let locked_reservation: (String, i64) = sqlx::query_as(
            "SELECT target_reserve, reservation_generation FROM loyal_yield.target_capacity_reservations WHERE decision_id = $1 AND reservation_state = 'active' FOR UPDATE",
        )
        .bind(movement.decision_id.as_i64())
        .fetch_one(&mut *tx)
        .await?;
        if locked_reservation != (current_reserve.clone(), current_generation) {
            return Err(OrchestratorError::StoreInvariant(
                "fallback capacity changed before deterministic frontier locking; retry".to_owned(),
            ));
        }
        if fallback_frontier.try_get::<i64, _>("observed_supply_usd_micros")?
            != target.observed_supply_usd_micros
            || fallback_frontier.try_get::<i64, _>("observed_slot")? != target.observed_slot
            || fallback_frontier.try_get::<i64, _>("maximum_inflight_usd_micros")?
                != target.maximum_inflight_usd_micros
            || fallback_frontier.try_get::<i64, _>("telemetry_version")?
                != input.target.telemetry_version
        {
            return Err(OrchestratorError::StoreInvariant(
                "fallback target capacity telemetry changed; retry from a fresh projection"
                    .to_owned(),
            ));
        }
        let committed: i64 = sqlx::query_scalar(
            r#"
            SELECT COALESCE(sum(principal_usd_micros), 0)::BIGINT
            FROM loyal_yield.target_capacity_reservations
            WHERE cluster = $1 AND target_reserve = $2 AND liquidity_mint = $3
              AND reservation_state <> 'released'
              AND decision_id <> $4
            "#,
        )
        .bind(&movement.cluster)
        .bind(&target.target_reserve)
        .bind(&movement.target_mint)
        .bind(movement.decision_id.as_i64())
        .fetch_one(&mut *tx)
        .await?;
        let principal: i64 = sqlx::query_scalar(
            "SELECT principal_usd_micros FROM loyal_yield.target_capacity_reservations WHERE decision_id = $1",
        )
        .bind(movement.decision_id.as_i64())
        .fetch_one(&mut *tx)
        .await?;
        if committed
            .checked_add(principal)
            .is_none_or(|total| total > target.maximum_inflight_usd_micros)
        {
            return Err(OrchestratorError::StoreInvariant(
                "fallback target capacity is exhausted".to_owned(),
            ));
        }
        let generation: i64 = sqlx::query_scalar(
            r#"
            UPDATE loyal_yield.target_capacity_frontiers
            SET reservation_generation = GREATEST(
                    reservation_generation,
                    $4
                ) + 1,
                updated_at = now()
            WHERE cluster = $1 AND target_reserve = $2 AND liquidity_mint = $3
            RETURNING reservation_generation
            "#,
        )
        .bind(&movement.cluster)
        .bind(&target.target_reserve)
        .bind(&movement.target_mint)
        .bind(current_generation)
        .fetch_one(&mut *tx)
        .await?;
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.target_capacity_reservations
            SET target_reserve = $2,
                admitted_observed_supply_usd_micros = $3,
                admitted_observed_slot = $4,
                admitted_maximum_inflight_usd_micros = $5,
                admitted_telemetry_version = $6,
                reservation_generation = $7,
                state_version = state_version + 1,
                updated_at = now()
            WHERE decision_id = $1
              AND reservation_state = 'active'
            RETURNING *
            "#,
        )
        .bind(movement.decision_id.as_i64())
        .bind(&target.target_reserve)
        .bind(target.observed_supply_usd_micros)
        .bind(target.observed_slot)
        .bind(target.maximum_inflight_usd_micros)
        .bind(input.target.telemetry_version)
        .bind(generation)
        .fetch_one(&mut *tx)
        .await?;
        sqlx::query(
            r#"
            UPDATE loyal_yield.rebalance_decisions
            SET active_target_reserve = $2,
                continuation_lease_owner = NULL,
                continuation_lease_expires_at = NULL,
                continuation_available_at = now(),
                updated_at = now()
            WHERE id = $1
              AND continuation_lease_owner = $3
              AND continuation_fencing_token = $4
              AND continuation_lease_expires_at > now()
            "#,
        )
        .bind(movement.decision_id.as_i64())
        .bind(&target.target_reserve)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .execute(&mut *tx)
        .await?;
        let reservation = target_capacity_reservation_from_row(&row)?;
        tx.commit().await?;
        Ok(reservation)
    }

    /// Closes a movement only when no transaction effect is unresolved. This
    /// records external user/policy evidence and never fabricates a deposit
    /// receipt or rewrites the last reconciled custody projection.
    pub async fn close_cross_mint_movement(
        &self,
        lease: &CrossMintContinuationLease,
        input: CrossMintMovementCloseInput,
    ) -> Result<CrossMintMovementRecord, OrchestratorError> {
        if !matches!(
            input.outcome,
            CrossMintTerminalOutcome::CancelledBeforeWithdraw
                | CrossMintTerminalOutcome::ClosedByUser
                | CrossMintTerminalOutcome::ManualIntervention
        ) || input.observed_slot < 0
            || input.reason.trim().is_empty()
            || input.reason.len() > 512
            || !input.evidence.is_object()
            || input
                .evidence
                .as_object()
                .is_none_or(serde_json::Map::is_empty)
        {
            return Err(OrchestratorError::StoreInvariant(
                "movement close requires a close-only outcome, bounded reason, slot, and explicit evidence"
                    .to_owned(),
            ));
        }
        let mut tx = self.pool().begin().await?;
        let movement = lock_cross_mint_movement_lease(&mut tx, lease).await?;
        let submission_count: i64 = sqlx::query_scalar(
            "SELECT count(*)::BIGINT FROM loyal_yield.signed_route_submissions WHERE decision_id = $1",
        )
        .bind(movement.decision_id.as_i64())
        .fetch_one(&mut *tx)
        .await?;
        if input.outcome == CrossMintTerminalOutcome::CancelledBeforeWithdraw
            && (movement.phase != CrossMintCustodyPhase::SourceReserve
                || movement.custody_version != 0
                || submission_count != 0
                || input.evidence.get("kind").and_then(Value::as_str)
                    != Some("start_authority_revoked_before_withdraw"))
        {
            return Err(OrchestratorError::StoreInvariant(
                "pre-withdraw cancellation requires untouched source custody, no signed submission, and explicit revocation evidence"
                    .to_owned(),
            ));
        }
        let unresolved: bool = sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1
                FROM loyal_yield.signed_route_submissions
                WHERE decision_id = $1
                  AND submission_state NOT IN ('reconciled', 'expired', 'failed')
            )
            "#,
        )
        .bind(movement.decision_id.as_i64())
        .fetch_one(&mut *tx)
        .await?;
        if unresolved {
            return Err(OrchestratorError::StoreInvariant(
                "movement with an ambiguous or nonterminal effect cannot be closed".to_owned(),
            ));
        }

        let reservation_snapshot = sqlx::query(
            r#"
            SELECT id, cluster, target_reserve, liquidity_mint
            FROM loyal_yield.target_capacity_reservations
            WHERE decision_id = $1 AND reservation_state <> 'released'
            "#,
        )
        .bind(movement.decision_id.as_i64())
        .fetch_one(&mut *tx)
        .await?;
        sqlx::query(
            r#"
            SELECT 1 FROM loyal_yield.target_capacity_frontiers
            WHERE cluster = $1 AND target_reserve = $2 AND liquidity_mint = $3
            FOR UPDATE
            "#,
        )
        .bind(reservation_snapshot.try_get::<String, _>("cluster")?)
        .bind(reservation_snapshot.try_get::<String, _>("target_reserve")?)
        .bind(reservation_snapshot.try_get::<String, _>("liquidity_mint")?)
        .fetch_one(&mut *tx)
        .await?;
        let reservation = sqlx::query(
            r#"
            SELECT id, cluster, target_reserve, liquidity_mint
            FROM loyal_yield.target_capacity_reservations
            WHERE decision_id = $1 AND reservation_state <> 'released'
            FOR UPDATE
            "#,
        )
        .bind(movement.decision_id.as_i64())
        .fetch_one(&mut *tx)
        .await?;
        if reservation.try_get::<String, _>("cluster")?
            != reservation_snapshot.try_get::<String, _>("cluster")?
            || reservation.try_get::<String, _>("target_reserve")?
                != reservation_snapshot.try_get::<String, _>("target_reserve")?
            || reservation.try_get::<String, _>("liquidity_mint")?
                != reservation_snapshot.try_get::<String, _>("liquidity_mint")?
        {
            return Err(OrchestratorError::StoreInvariant(
                "movement capacity changed before deterministic frontier locking; retry".to_owned(),
            ));
        }
        let updated = sqlx::query(
            r#"
            UPDATE loyal_yield.rebalance_decisions
            SET status = 'abandoned'::loyal_yield.decision_status,
                terminal_outcome = $4,
                terminal_evidence = $5,
                terminal_reason = $6,
                terminal_observed_slot = $7,
                abandon_reason = $6,
                continuation_available_at = NULL,
                continuation_lease_owner = NULL,
                continuation_lease_expires_at = NULL,
                updated_at = now()
            WHERE id = $1
              AND movement_route = 'cross_mint_jupiter'
              AND status = 'confirming'::loyal_yield.decision_status
              AND terminal_outcome IS NULL
              AND continuation_lease_owner = $2
              AND continuation_fencing_token = $3
              AND continuation_lease_expires_at > now()
            RETURNING id
            "#,
        )
        .bind(movement.decision_id.as_i64())
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(input.outcome.as_str())
        .bind(input.evidence)
        .bind(input.reason.trim())
        .bind(input.observed_slot)
        .fetch_optional(&mut *tx)
        .await?;
        if updated.is_none() {
            return Err(OrchestratorError::StoreInvariant(
                "movement close lost its continuation fence".to_owned(),
            ));
        }
        let completed = sqlx::query(
            r#"
            UPDATE loyal_yield.rebalance_opportunities
            SET opportunity_state = 'completed', terminal_reason = $2,
                updated_at = now()
            WHERE id = $1 AND opportunity_state = 'decision_created'
              AND decision_id = $3
            "#,
        )
        .bind(movement.opportunity_id)
        .bind(input.outcome.as_str())
        .bind(movement.decision_id.as_i64())
        .execute(&mut *tx)
        .await?;
        if completed.rows_affected() != 1 {
            return Err(OrchestratorError::StoreInvariant(
                "movement close did not terminalize exactly one opportunity".to_owned(),
            ));
        }
        let released = sqlx::query(
            r#"
            UPDATE loyal_yield.target_capacity_reservations
            SET reservation_state = 'released', released_at = now(),
                release_reason = $2, state_version = state_version + 1,
                updated_at = now()
            WHERE id = $1 AND reservation_state <> 'released'
            "#,
        )
        .bind(reservation.try_get::<i64, _>("id")?)
        .bind(input.outcome.as_str())
        .execute(&mut *tx)
        .await?;
        if released.rows_affected() != 1 {
            return Err(OrchestratorError::StoreInvariant(
                "movement close did not release exactly one capacity reservation".to_owned(),
            ));
        }
        let movement = cross_mint_movement_in_connection(&mut tx, movement.decision_id).await?;
        tx.commit().await?;
        Ok(movement)
    }
}

fn validate_publication_input(
    lease: &CrossMintContinuationLease,
    input: &CrossMintLegPublicationInput,
) -> Result<(), OrchestratorError> {
    let submission = &input.submission;
    if input.generation <= 0
        || input.policy_account.trim().is_empty()
        || submission.cluster != lease.movement.cluster
        || submission.opportunity_id != lease.movement.opportunity_id
        || submission.decision_id != Some(lease.movement.decision_id)
        || submission.executor_owner != lease.owner
        || submission.executor_fencing_token != lease.fencing_token
        || submission.semantic_key.trim().is_empty()
        || submission.signed_transaction.is_empty()
        || submission.signed_transaction_hash.trim().is_empty()
        || submission.message_hash.trim().is_empty()
        || submission.transaction_signature.trim().is_empty()
        || submission.recent_blockhash.trim().is_empty()
        || submission.last_valid_block_height < 0
        || submission.optimizer_epoch_id <= 0
        || submission.compiled_fee_lamports < 0
        || !submission.alt_mutation_epochs.is_object()
    {
        return Err(OrchestratorError::StoreInvariant(
            "cross-mint leg requires exact movement, wire, policy, and continuation evidence"
                .to_owned(),
        ));
    }
    validate_expected_effect(&input.expected_effect)?;
    validate_expected_balance_anchors(&input.expected_effect, &input.expected_balance_anchors)
}

fn validate_expected_balance_anchors(
    effect: &CrossMintExpectedEffect,
    anchors: &CrossMintBalanceAnchors,
) -> Result<(), OrchestratorError> {
    validate_balance_anchors(anchors)?;
    if effect.debit.is_some() != anchors.debit.is_some()
        || effect.credit_mint.is_some() != anchors.credit.is_some()
    {
        return Err(OrchestratorError::StoreInvariant(
            "cross-mint signed effect and pre-effect balance anchors have different account sets"
                .to_owned(),
        ));
    }
    if let (Some(debit), Some(anchor)) = (effect.debit.as_ref(), anchors.debit.as_ref()) {
        if anchor.mint != debit.mint
            || anchor.token_account != debit.token_account
            || anchor.amount_raw < debit.amount_raw
        {
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint debit anchor does not identify and fund the signed debit".to_owned(),
            ));
        }
    }
    if let (Some(mint), Some(account), Some(anchor)) = (
        effect.credit_mint.as_deref(),
        effect.credit_token_account.as_deref(),
        anchors.credit.as_ref(),
    ) {
        if anchor.mint != mint || anchor.token_account != account {
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint credit anchor does not identify the signed credit account".to_owned(),
            ));
        }
    }
    Ok(())
}

fn validate_balance_anchors(anchors: &CrossMintBalanceAnchors) -> Result<(), OrchestratorError> {
    for anchor in [anchors.debit.as_ref(), anchors.credit.as_ref()]
        .into_iter()
        .flatten()
    {
        if anchor.mint.trim().is_empty()
            || anchor.token_account.trim().is_empty()
            || anchor.amount_raw < 0
        {
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint balance anchors require canonical nonnegative observations".to_owned(),
            ));
        }
    }
    if let Some(position) = &anchors.kamino_position {
        if position.reserve.trim().is_empty()
            || position.market.trim().is_empty()
            || position.obligation.trim().is_empty()
            || position.deposited_collateral_amount_raw < 0
            || position
                .minimum_deposit_amount_raw
                .is_some_and(|amount| amount <= 0)
            || (!position.obligation_exists && position.deposited_collateral_amount_raw != 0)
        {
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint Kamino position anchors require canonical nonnegative observations"
                    .to_owned(),
            ));
        }
    }
    Ok(())
}

fn validate_expected_effect(effect: &CrossMintExpectedEffect) -> Result<(), OrchestratorError> {
    if effect
        .debit
        .as_ref()
        .is_some_and(|delta| !valid_delta(delta))
        || effect
            .credit_mint
            .as_deref()
            .is_some_and(|value| value.trim().is_empty())
        || effect
            .credit_token_account
            .as_deref()
            .is_some_and(|value| value.trim().is_empty())
        || effect
            .minimum_credit_amount_raw
            .is_some_and(|amount| amount <= 0)
        || effect.credit_mint.is_some() != effect.credit_token_account.is_some()
        || effect.credit_mint.is_some() != effect.minimum_credit_amount_raw.is_some()
    {
        return Err(OrchestratorError::StoreInvariant(
            "cross-mint expected effect is incomplete or nonpositive".to_owned(),
        ));
    }
    Ok(())
}

fn validate_reconciled_effect(effect: &CrossMintReconciledEffect) -> Result<(), OrchestratorError> {
    if effect.debit.is_none() && effect.credit.is_none()
        || effect
            .debit
            .as_ref()
            .is_some_and(|delta| !valid_delta(delta))
        || effect
            .credit
            .as_ref()
            .is_some_and(|delta| !valid_delta(delta))
    {
        return Err(OrchestratorError::StoreInvariant(
            "cross-mint reconciled effect requires positive canonical deltas".to_owned(),
        ));
    }
    Ok(())
}

fn valid_delta(delta: &TokenBalanceDelta) -> bool {
    !delta.mint.trim().is_empty() && !delta.token_account.trim().is_empty() && delta.amount_raw > 0
}

async fn validate_next_leg(
    connection: &mut PgConnection,
    movement: &CrossMintMovementRecord,
    input: &CrossMintLegPublicationInput,
) -> Result<(), OrchestratorError> {
    let previous_generation: Option<i64> = sqlx::query_scalar(
        r#"
        SELECT max(leg_generation)
        FROM loyal_yield.signed_route_submissions
        WHERE decision_id = $1 AND movement_leg = $2
        "#,
    )
    .bind(movement.decision_id.as_i64())
    .bind(input.leg.as_str())
    .fetch_one(&mut *connection)
    .await?;
    let expected_generation = previous_generation.unwrap_or(0) + 1;
    if input.generation != expected_generation {
        return Err(OrchestratorError::StoreInvariant(format!(
            "cross-mint {:?} generation must be {expected_generation}",
            input.leg
        )));
    }
    if previous_generation.is_some() {
        let prior_no_effect: bool = sqlx::query_scalar(
            r#"
            SELECT submission.submission_state IN ('expired', 'failed')
                AND EXISTS (
                    SELECT 1
                    FROM loyal_yield.cross_mint_no_effect_receipts receipt
                    WHERE receipt.submission_id = submission.id
                      AND receipt.decision_id = submission.decision_id
                      AND receipt.movement_leg = submission.movement_leg
                      AND receipt.leg_generation = submission.leg_generation
                      AND receipt.transaction_signature =
                          submission.transaction_signature
                )
            FROM loyal_yield.signed_route_submissions submission
            WHERE submission.decision_id = $1
              AND submission.movement_leg = $2
            ORDER BY submission.leg_generation DESC LIMIT 1
            "#,
        )
        .bind(movement.decision_id.as_i64())
        .bind(input.leg.as_str())
        .fetch_one(&mut *connection)
        .await?;
        if !prior_no_effect {
            return Err(OrchestratorError::StoreInvariant(
                "a higher leg generation requires terminal no-effect proof".to_owned(),
            ));
        }
    }
    let allowed = matches!(
        (movement.phase, input.leg, input.purpose),
        (
            CrossMintCustodyPhase::SourceReserve,
            CrossMintMovementLeg::Withdraw,
            CrossMintLegPurpose::OptimizeYield,
        ) | (
            CrossMintCustodyPhase::SourceIdle,
            CrossMintMovementLeg::Swap,
            CrossMintLegPurpose::OptimizeYield,
        ) | (
            CrossMintCustodyPhase::SourceIdle,
            CrossMintMovementLeg::Deposit,
            CrossMintLegPurpose::RecoverSource,
        ) | (
            CrossMintCustodyPhase::TargetIdle,
            CrossMintMovementLeg::Deposit,
            CrossMintLegPurpose::OptimizeYield | CrossMintLegPurpose::FallbackTarget,
        )
    );
    if !allowed {
        return Err(OrchestratorError::StoreInvariant(format!(
            "cross-mint {:?} with purpose {:?} is invalid from {:?}",
            input.leg, input.purpose, movement.phase
        )));
    }
    if matches!(
        input.leg,
        CrossMintMovementLeg::Swap | CrossMintMovementLeg::Deposit
    ) {
        let debit = input.expected_effect.debit.as_ref();
        let anchor = input.expected_balance_anchors.debit.as_ref();
        if debit.is_none_or(|debit| {
            debit.mint != movement.custody_mint
                || debit.token_account != movement.custody_account
                || debit.amount_raw != movement.custody_amount_raw
        }) || anchor.is_none_or(|anchor| {
            anchor.mint != movement.custody_mint
                || anchor.token_account != movement.custody_account
                || Some(anchor.amount_raw) != movement.custody_observed_balance_raw
        }) {
            return Err(OrchestratorError::StoreInvariant(
                "swap and deposit wires must bind the full attributed idle custody".to_owned(),
            ));
        }
    }
    Ok(())
}

fn validate_effect_against_movement(
    movement: &CrossMintMovementRecord,
    leg: CrossMintMovementLeg,
    purpose: &str,
    expected: &CrossMintExpectedEffect,
    expected_anchors: &CrossMintBalanceAnchors,
    actual: &CrossMintReconciledEffect,
    actual_anchors: &CrossMintBalanceAnchors,
) -> Result<(), OrchestratorError> {
    validate_balance_transition(expected, expected_anchors, actual, actual_anchors)?;
    if let Some(expected_debit) = &expected.debit {
        let debit_matches = actual.debit.as_ref().is_some_and(|actual_debit| {
            actual_debit.mint == expected_debit.mint
                && actual_debit.token_account == expected_debit.token_account
                && actual_debit.amount_raw <= expected_debit.amount_raw
                && (leg == CrossMintMovementLeg::Deposit
                    || actual_debit.amount_raw == expected_debit.amount_raw)
        });
        if !debit_matches {
            return Err(OrchestratorError::StoreInvariant(
                if leg == CrossMintMovementLeg::Deposit {
                    "finalized deposit debit exceeds or changes its signed custody bound"
                } else {
                    "finalized debit differs from the signed leg expectation"
                }
                .to_owned(),
            ));
        }
    }
    if let (Some(mint), Some(account), Some(minimum), Some(credit)) = (
        expected.credit_mint.as_deref(),
        expected.credit_token_account.as_deref(),
        expected.minimum_credit_amount_raw,
        actual.credit.as_ref(),
    ) {
        if credit.mint != mint || credit.token_account != account || credit.amount_raw < minimum {
            return Err(OrchestratorError::StoreInvariant(
                "finalized credit differs from the signed leg expectation".to_owned(),
            ));
        }
    } else if expected.credit_mint.is_some() {
        return Err(OrchestratorError::StoreInvariant(
            "finalized leg is missing its expected credit".to_owned(),
        ));
    }
    match leg {
        CrossMintMovementLeg::Withdraw => {
            let credit = actual.credit.as_ref().ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "finalized withdrawal has no source-mint idle credit".to_owned(),
                )
            })?;
            if credit.mint != movement.source_mint {
                return Err(OrchestratorError::StoreInvariant(
                    "withdrawal credited a mint other than the movement source".to_owned(),
                ));
            }
            validate_kamino_position_transition(
                expected_anchors,
                actual_anchors,
                &movement.source_reserve,
                true,
            )?;
        }
        CrossMintMovementLeg::Swap => {
            if expected_anchors.kamino_position.is_some()
                || actual_anchors.kamino_position.is_some()
            {
                return Err(OrchestratorError::StoreInvariant(
                    "swap reconciliation unexpectedly included a Kamino position".to_owned(),
                ));
            }
            let debit = actual.debit.as_ref().ok_or_else(|| {
                OrchestratorError::StoreInvariant("swap has no source debit".to_owned())
            })?;
            let credit = actual.credit.as_ref().ok_or_else(|| {
                OrchestratorError::StoreInvariant("swap has no target credit".to_owned())
            })?;
            if debit.mint != movement.custody_mint
                || debit.token_account != movement.custody_account
                || debit.amount_raw != movement.custody_amount_raw
                || movement.custody_observed_balance_raw
                    != expected_anchors
                        .debit
                        .as_ref()
                        .map(|anchor| anchor.amount_raw)
                || credit.mint != movement.target_mint
            {
                return Err(OrchestratorError::StoreInvariant(
                    "swap deltas do not consume exact attributed source custody and credit target custody"
                        .to_owned(),
                ));
            }
        }
        CrossMintMovementLeg::Deposit => {
            let debit = actual.debit.as_ref().ok_or_else(|| {
                OrchestratorError::StoreInvariant("deposit has no idle custody debit".to_owned())
            })?;
            if debit.mint != movement.custody_mint
                || debit.token_account != movement.custody_account
            {
                return Err(OrchestratorError::StoreInvariant(
                    "deposit debit identity differs from movement-attributed idle custody"
                        .to_owned(),
                ));
            }
            if debit.amount_raw > movement.custody_amount_raw {
                return Err(OrchestratorError::StoreInvariant(
                    "deposit consumed more than movement-attributed idle custody".to_owned(),
                ));
            }
            let expected_observed_balance = expected_anchors
                .debit
                .as_ref()
                .map(|anchor| anchor.amount_raw);
            if movement.custody_observed_balance_raw != expected_observed_balance {
                return Err(OrchestratorError::StoreInvariant(format!(
                    "deposit pre-balance anchor {expected_observed_balance:?} differs from attributed aggregate {:?}",
                    movement.custody_observed_balance_raw,
                )));
            }
            if purpose == CrossMintLegPurpose::RecoverSource.as_str()
                && movement.custody_mint != movement.source_mint
            {
                return Err(OrchestratorError::StoreInvariant(
                    "source recovery cannot deposit target-mint custody".to_owned(),
                ));
            }
            let expected_reserve = if purpose == CrossMintLegPurpose::RecoverSource.as_str() {
                &movement.source_reserve
            } else {
                &movement.active_target_reserve
            };
            validate_kamino_position_transition(
                expected_anchors,
                actual_anchors,
                expected_reserve,
                false,
            )?;
            let residual_amount_raw = movement
                .custody_amount_raw
                .checked_sub(debit.amount_raw)
                .ok_or_else(|| {
                    OrchestratorError::StoreInvariant(
                        "deposit consumed more than attributed custody".to_owned(),
                    )
                })?;
            if residual_amount_raw > 0 {
                let minimum_deposit_amount_raw = actual_anchors
                    .kamino_position
                    .as_ref()
                    .and_then(|position| position.minimum_deposit_amount_raw)
                    .ok_or_else(|| {
                        OrchestratorError::StoreInvariant(
                            "partial Kamino deposit lacks a finalized minimum-deposit proof"
                                .to_owned(),
                        )
                    })?;
                if residual_amount_raw >= minimum_deposit_amount_raw {
                    return Err(OrchestratorError::StoreInvariant(
                        "partial Kamino deposit left custody that can still mint collateral"
                            .to_owned(),
                    ));
                }
            }
        }
    }
    Ok(())
}

fn validate_kamino_position_transition(
    expected_anchors: &CrossMintBalanceAnchors,
    actual_anchors: &CrossMintBalanceAnchors,
    expected_reserve: &str,
    withdrawal: bool,
) -> Result<(), OrchestratorError> {
    let before = expected_anchors.kamino_position.as_ref().ok_or_else(|| {
        OrchestratorError::StoreInvariant(
            "Kamino leg is missing its pre-transaction position anchor".to_owned(),
        )
    })?;
    let after = actual_anchors.kamino_position.as_ref().ok_or_else(|| {
        OrchestratorError::StoreInvariant(
            "Kamino leg is missing its finalized position readback".to_owned(),
        )
    })?;
    if before.reserve != expected_reserve
        || after.reserve != expected_reserve
        || before.reserve != after.reserve
        || before.market != after.market
        || before.obligation != after.obligation
    {
        return Err(OrchestratorError::StoreInvariant(
            "Kamino position identity changed across finalized reconciliation".to_owned(),
        ));
    }
    let valid_amount_transition = if withdrawal {
        before.obligation_exists
            && before.deposited_collateral_amount_raw > 0
            && before.deposited_collateral_amount_raw > after.deposited_collateral_amount_raw
            && (after.deposited_collateral_amount_raw == 0
                || (after.obligation_exists && after.deposited_collateral_amount_raw == 1))
    } else {
        after.obligation_exists
            && after.deposited_collateral_amount_raw > before.deposited_collateral_amount_raw
    };
    if !valid_amount_transition {
        return Err(OrchestratorError::StoreInvariant(
            if withdrawal {
                "finalized withdrawal did not remove the source Kamino position"
            } else {
                "finalized deposit did not increase the destination Kamino position"
            }
            .to_owned(),
        ));
    }
    Ok(())
}

fn validate_balance_transition(
    expected: &CrossMintExpectedEffect,
    expected_anchors: &CrossMintBalanceAnchors,
    actual: &CrossMintReconciledEffect,
    actual_anchors: &CrossMintBalanceAnchors,
) -> Result<(), OrchestratorError> {
    if expected.debit.is_some() != expected_anchors.debit.is_some()
        || expected.credit_mint.is_some() != expected_anchors.credit.is_some()
        || actual.debit.is_some() != actual_anchors.debit.is_some()
        || actual.credit.is_some() != actual_anchors.credit.is_some()
    {
        return Err(OrchestratorError::StoreInvariant(
            "cross-mint delta and aggregate balance anchors have different account sets".to_owned(),
        ));
    }
    if expected_anchors.kamino_position.is_some() != actual_anchors.kamino_position.is_some() {
        return Err(OrchestratorError::StoreInvariant(
            "cross-mint pre/post Kamino position anchors have different account sets".to_owned(),
        ));
    }
    if let (Some(delta), Some(pre), Some(post)) = (
        actual.debit.as_ref(),
        expected_anchors.debit.as_ref(),
        actual_anchors.debit.as_ref(),
    ) {
        if pre.mint != delta.mint
            || post.mint != delta.mint
            || pre.token_account != delta.token_account
            || post.token_account != delta.token_account
            || pre.amount_raw.checked_sub(delta.amount_raw) != Some(post.amount_raw)
        {
            return Err(OrchestratorError::StoreInvariant(
                "finalized debit is not the exact pre/post aggregate balance delta".to_owned(),
            ));
        }
    }
    if let (Some(delta), Some(pre), Some(post)) = (
        actual.credit.as_ref(),
        expected_anchors.credit.as_ref(),
        actual_anchors.credit.as_ref(),
    ) {
        if pre.mint != delta.mint
            || post.mint != delta.mint
            || pre.token_account != delta.token_account
            || post.token_account != delta.token_account
            || pre.amount_raw.checked_add(delta.amount_raw) != Some(post.amount_raw)
        {
            return Err(OrchestratorError::StoreInvariant(
                "finalized credit is not the exact pre/post aggregate balance delta".to_owned(),
            ));
        }
    }
    Ok(())
}

struct NextCustody {
    mint: String,
    amount_raw: i64,
    account: String,
    observed_balance_raw: Option<i64>,
    terminal_outcome: Option<CrossMintTerminalOutcome>,
    terminal_evidence: Option<Value>,
    terminal_reason: Option<&'static str>,
    terminal_observed_slot: Option<i64>,
}

fn next_custody(
    movement: &CrossMintMovementRecord,
    leg: CrossMintMovementLeg,
    purpose: &str,
    effect: &CrossMintReconciledEffect,
    anchors: &CrossMintBalanceAnchors,
    finalized_slot: i64,
) -> Result<NextCustody, OrchestratorError> {
    match leg {
        CrossMintMovementLeg::Withdraw | CrossMintMovementLeg::Swap => {
            let credit = effect.credit.as_ref().ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "custody-advancing leg lacks finalized credit".to_owned(),
                )
            })?;
            let observed_balance = anchors.credit.as_ref().ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "custody-advancing leg lacks a finalized aggregate balance anchor".to_owned(),
                )
            })?;
            Ok(NextCustody {
                mint: credit.mint.clone(),
                amount_raw: credit.amount_raw,
                account: credit.token_account.clone(),
                observed_balance_raw: Some(observed_balance.amount_raw),
                terminal_outcome: None,
                terminal_evidence: None,
                terminal_reason: None,
                terminal_observed_slot: None,
            })
        }
        CrossMintMovementLeg::Deposit => {
            let debit = effect.debit.as_ref().ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "terminal deposit lacks its finalized idle-custody debit".to_owned(),
                )
            })?;
            let post_balance = anchors.debit.as_ref().ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "terminal deposit lacks its finalized aggregate balance".to_owned(),
                )
            })?;
            let residual_amount_raw = movement
                .custody_amount_raw
                .checked_sub(debit.amount_raw)
                .ok_or_else(|| {
                    OrchestratorError::StoreInvariant(
                        "terminal deposit consumed more than attributed custody".to_owned(),
                    )
                })?;
            let (terminal_outcome, terminal_reserve) =
                if purpose == CrossMintLegPurpose::RecoverSource.as_str() {
                    (
                        CrossMintTerminalOutcome::RecoveredSource,
                        movement.source_reserve.as_str(),
                    )
                } else {
                    (
                        CrossMintTerminalOutcome::CompletedTarget,
                        movement.active_target_reserve.as_str(),
                    )
                };
            if residual_amount_raw == 0 {
                Ok(NextCustody {
                    mint: movement.custody_mint.clone(),
                    amount_raw: 0,
                    account: terminal_reserve.to_owned(),
                    observed_balance_raw: None,
                    terminal_outcome: Some(terminal_outcome),
                    terminal_evidence: None,
                    terminal_reason: None,
                    terminal_observed_slot: None,
                })
            } else {
                let minimum_deposit_amount_raw = anchors
                    .kamino_position
                    .as_ref()
                    .and_then(|position| position.minimum_deposit_amount_raw)
                    .ok_or_else(|| {
                        OrchestratorError::StoreInvariant(
                            "partial Kamino deposit lacks a finalized minimum-deposit proof"
                                .to_owned(),
                        )
                    })?;
                Ok(NextCustody {
                    mint: movement.custody_mint.clone(),
                    amount_raw: residual_amount_raw,
                    account: movement.custody_account.clone(),
                    observed_balance_raw: Some(post_balance.amount_raw),
                    terminal_outcome: Some(terminal_outcome),
                    terminal_evidence: Some(serde_json::json!({
                        "kind": "kamino_unmintable_rounding_dust",
                        "mint": movement.custody_mint,
                        "tokenAccount": movement.custody_account,
                        "requestedAmountRaw": movement.custody_amount_raw,
                        "depositedAmountRaw": debit.amount_raw,
                        "residualAmountRaw": residual_amount_raw,
                        "minimumDepositAmountRaw": minimum_deposit_amount_raw,
                        "finalizedPostBalanceRaw": post_balance.amount_raw,
                        "reserve": terminal_reserve,
                    })),
                    terminal_reason: Some("kamino_unmintable_rounding_dust"),
                    terminal_observed_slot: Some(finalized_slot),
                })
            }
        }
    }
}

fn start_policy_bindings_from_execution_plan(
    execution_plan: &Value,
) -> Result<CrossMintPolicyBindings, OrchestratorError> {
    CrossMintPolicyBindings::from_execution_plan(execution_plan)
}

async fn validate_initial_cross_mint_policy_bindings(
    connection: &mut PgConnection,
    movement: &CrossMintMovementRecord,
    publication: &CrossMintLegPublicationInput,
) -> Result<(), OrchestratorError> {
    let execution_plan: Value = sqlx::query_scalar(
        r#"
        SELECT opportunity.execution_plan
        FROM loyal_yield.rebalance_opportunities opportunity
        WHERE opportunity.decision_id = $1
        "#,
    )
    .bind(movement.decision_id.as_i64())
    .fetch_one(&mut *connection)
    .await?;
    let bindings = start_policy_bindings_from_execution_plan(&execution_plan)?;
    if publication.policy_account != bindings.withdraw.policy_account {
        return Err(OrchestratorError::StoreInvariant(
            "initial cross-mint withdrawal does not use its immutable policy account".to_owned(),
        ));
    }
    let withdraw_observed_slot = i64::try_from(bindings.withdraw.observed_slot).map_err(|_| {
        OrchestratorError::StoreInvariant(
            "cross-mint withdraw policy slot does not fit PostgreSQL BIGINT".to_owned(),
        )
    })?;
    let deposit_observed_slot = i64::try_from(bindings.deposit.observed_slot).map_err(|_| {
        OrchestratorError::StoreInvariant(
            "cross-mint deposit policy slot does not fit PostgreSQL BIGINT".to_owned(),
        )
    })?;
    let swap_observed_slot = i64::try_from(bindings.swap.observed_slot).map_err(|_| {
        OrchestratorError::StoreInvariant(
            "cross-mint swap policy slot does not fit PostgreSQL BIGINT".to_owned(),
        )
    })?;
    let swap_daily_source_mint_spending_cap =
        i64::try_from(bindings.swap.daily_source_mint_spending_cap).map_err(|_| {
            OrchestratorError::StoreInvariant(
                "cross-mint daily source-mint cap does not fit PostgreSQL BIGINT".to_owned(),
            )
        })?;
    let valid = sqlx::query_scalar::<_, i64>(
        r#"
        SELECT withdraw_policy.id
        FROM loyal_yield.route_policies withdraw_policy
        JOIN loyal_yield.route_policies deposit_policy
          ON deposit_policy.authority = withdraw_policy.authority
         AND deposit_policy.cluster = $1
         AND deposit_policy.settings = $2
         AND deposit_policy.vault_index = $3
         AND deposit_policy.vault_pubkey = $4
         AND deposit_policy.delegated_signers = ARRAY[$5]::TEXT[]
         AND deposit_policy.threshold = 1
         AND deposit_policy.policy_account = $8
         AND deposit_policy.last_seen_slot >= $9
         AND deposit_policy.active
         AND deposit_policy.finalized_eligible
         AND deposit_policy.source_commitment = 'finalized'
         AND 'same_mint_kamino' = ANY(deposit_policy.route_modes)
         AND $10 = ANY(deposit_policy.stable_mints)
         AND $10 = ANY(deposit_policy.kamino_liquidity_mints)
        JOIN loyal_yield.cross_mint_swap_policies swap_policy
          ON swap_policy.authority = withdraw_policy.authority
         AND swap_policy.cluster = $1
         AND swap_policy.settings = $2
         AND swap_policy.vault_index = $3
         AND swap_policy.vault_pubkey = $4
         AND swap_policy.delegated_signer = $5
         AND swap_policy.policy_account = $11
         AND swap_policy.last_seen_slot >= $12
         AND swap_policy.source_shard = $13
         AND swap_policy.max_slippage_bps = $14
         AND swap_policy.daily_source_mint_spending_cap = $16
         AND swap_policy.manifest_fingerprint = $17
         AND swap_policy.active
         AND swap_policy.start_eligible
         AND swap_policy.source_commitment IN ('confirmed', 'finalized')
         AND swap_policy.last_mutation IN ('create', 'update')
        JOIN loyal_yield.cross_mint_vault_opt_ins opt_in
          ON opt_in.enabled = TRUE
         AND opt_in.cluster = swap_policy.cluster
         AND opt_in.settings = swap_policy.settings
         AND opt_in.vault_index = swap_policy.vault_index
         AND opt_in.vault_pubkey = swap_policy.vault_pubkey
         AND opt_in.generation = $18
         AND (
             (
                 swap_policy.source_shard = 'classic'
                 AND opt_in.classic_policy_account = swap_policy.policy_account
                 AND opt_in.classic_policy_seed = swap_policy.policy_seed
             )
             OR (
                 swap_policy.source_shard = 'token_2022'
                 AND opt_in.token_2022_policy_account = swap_policy.policy_account
                 AND opt_in.token_2022_policy_seed = swap_policy.policy_seed
             )
         )
        JOIN loyal_yield.cross_mint_swap_policies sibling_policy
          ON sibling_policy.cluster = swap_policy.cluster
         AND sibling_policy.settings = swap_policy.settings
         AND sibling_policy.authority = swap_policy.authority
         AND sibling_policy.vault_index = swap_policy.vault_index
         AND sibling_policy.vault_pubkey = swap_policy.vault_pubkey
         AND sibling_policy.delegated_signer = swap_policy.delegated_signer
         AND sibling_policy.policy_account <> swap_policy.policy_account
         AND sibling_policy.source_shard <> swap_policy.source_shard
         AND sibling_policy.active
         AND sibling_policy.start_eligible
         AND sibling_policy.source_commitment IN ('confirmed', 'finalized')
         AND sibling_policy.last_mutation IN ('create', 'update')
         AND sibling_policy.max_slippage_bps = swap_policy.max_slippage_bps
         AND sibling_policy.daily_source_mint_spending_cap =
             swap_policy.daily_source_mint_spending_cap
         AND 2 = (
             SELECT count(DISTINCT sibling.source_shard)
             FROM loyal_yield.cross_mint_swap_policies sibling
             WHERE sibling.cluster = swap_policy.cluster
               AND sibling.settings = swap_policy.settings
               AND sibling.authority = swap_policy.authority
               AND sibling.vault_index = swap_policy.vault_index
               AND sibling.vault_pubkey = swap_policy.vault_pubkey
               AND sibling.delegated_signer = swap_policy.delegated_signer
               AND sibling.active
               AND sibling.start_eligible
               AND sibling.source_commitment IN ('confirmed', 'finalized')
               AND sibling.last_mutation IN ('create', 'update')
               AND sibling.max_slippage_bps = swap_policy.max_slippage_bps
               AND sibling.daily_source_mint_spending_cap =
                   swap_policy.daily_source_mint_spending_cap
         )
        WHERE withdraw_policy.cluster = $1
          AND withdraw_policy.settings = $2
          AND withdraw_policy.vault_index = $3
          AND withdraw_policy.vault_pubkey = $4
          AND withdraw_policy.delegated_signers = ARRAY[$5]::TEXT[]
          AND withdraw_policy.threshold = 1
          AND withdraw_policy.policy_account = $6
          AND withdraw_policy.last_seen_slot >= $15
          AND withdraw_policy.active
          AND withdraw_policy.finalized_eligible
          AND withdraw_policy.source_commitment = 'finalized'
          AND 'same_mint_kamino' = ANY(withdraw_policy.route_modes)
          AND $7 = ANY(withdraw_policy.stable_mints)
          AND $7 = ANY(withdraw_policy.kamino_liquidity_mints)
        FOR SHARE OF withdraw_policy, deposit_policy, swap_policy, sibling_policy, opt_in
        "#,
    )
    .bind(&movement.cluster)
    .bind(&bindings.settings)
    .bind(i16::from(bindings.vault_index))
    .bind(&bindings.vault_pubkey)
    .bind(&bindings.delegated_signer)
    .bind(&bindings.withdraw.policy_account)
    .bind(&movement.source_mint)
    .bind(&bindings.deposit.policy_account)
    .bind(deposit_observed_slot)
    .bind(&movement.target_mint)
    .bind(&bindings.swap.policy_account)
    .bind(swap_observed_slot)
    .bind(&bindings.swap.source_shard)
    .bind(i32::from(bindings.swap.max_slippage_bps))
    .bind(withdraw_observed_slot)
    .bind(swap_daily_source_mint_spending_cap)
    .bind(&bindings.swap.manifest_fingerprint)
    .bind(bindings.swap.enrollment_generation)
    .fetch_optional(&mut *connection)
    .await?;
    if valid.is_none() {
        return Err(OrchestratorError::StoreInvariant(
            "initial cross-mint withdrawal lost an opted-in finalized policy binding".to_owned(),
        ));
    }
    Ok(())
}

async fn lock_cross_mint_movement_lease(
    connection: &mut PgConnection,
    lease: &CrossMintContinuationLease,
) -> Result<CrossMintMovementRecord, OrchestratorError> {
    let valid: Option<i64> = sqlx::query_scalar(
        r#"
        SELECT id FROM loyal_yield.rebalance_decisions
        WHERE id = $1
          AND movement_route = 'cross_mint_jupiter'
          AND status = 'confirming'::loyal_yield.decision_status
          AND terminal_outcome IS NULL
          AND continuation_lease_owner = $2
          AND continuation_fencing_token = $3
          AND continuation_control_generation = $4
          AND continuation_lease_expires_at > now()
        FOR UPDATE
        "#,
    )
    .bind(lease.movement.decision_id.as_i64())
    .bind(&lease.owner)
    .bind(lease.fencing_token)
    .bind(lease.control_generation)
    .fetch_optional(&mut *connection)
    .await?;
    if valid.is_none() {
        return Err(OrchestratorError::StoreInvariant(
            "cross-mint continuation lease is stale, expired, or fenced".to_owned(),
        ));
    }
    lock_cross_mint_movement(connection, lease.movement.decision_id).await
}

struct LockedCrossMintPublicationGates {
    start_new_movements: bool,
    generation: i64,
}

async fn lock_cross_mint_control_key(
    connection: &mut PgConnection,
    cluster: &str,
) -> Result<(), OrchestratorError> {
    sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")
        .bind(format!("loyal-yield-cross-mint-control:{cluster}"))
        .execute(&mut *connection)
        .await?;
    Ok(())
}

async fn lock_cross_mint_publication_gates(
    connection: &mut PgConnection,
    lease: &CrossMintContinuationLease,
) -> Result<LockedCrossMintPublicationGates, OrchestratorError> {
    lock_cross_mint_control_key(connection, &lease.movement.cluster).await?;
    let row = sqlx::query(
        r#"
        SELECT start_new_movements, continue_or_recover_existing, generation
        FROM loyal_yield.cross_mint_movement_controls
        WHERE cluster = $1
        FOR SHARE
        "#,
    )
    .bind(&lease.movement.cluster)
    .fetch_optional(&mut *connection)
    .await?;
    let (start_new_movements, continue_or_recover_existing, generation) = match row {
        Some(row) => (
            row.try_get::<bool, _>("start_new_movements")?,
            row.try_get::<bool, _>("continue_or_recover_existing")?,
            row.try_get::<i64, _>("generation")?,
        ),
        None => (false, true, 0),
    };
    if !continue_or_recover_existing || generation != lease.control_generation {
        return Err(OrchestratorError::StoreInvariant(
            "cross-mint publication lost its continuation control generation".to_owned(),
        ));
    }
    Ok(LockedCrossMintPublicationGates {
        start_new_movements,
        generation,
    })
}

async fn lock_cross_mint_movement(
    connection: &mut PgConnection,
    decision_id: DecisionId,
) -> Result<CrossMintMovementRecord, OrchestratorError> {
    sqlx::query("SELECT id FROM loyal_yield.rebalance_decisions WHERE id = $1 FOR UPDATE")
        .bind(decision_id.as_i64())
        .fetch_one(&mut *connection)
        .await?;
    cross_mint_movement_in_connection(connection, decision_id).await
}

async fn cross_mint_movement_in_connection(
    connection: &mut PgConnection,
    decision_id: DecisionId,
) -> Result<CrossMintMovementRecord, OrchestratorError> {
    let row = sqlx::query(
        r#"
        SELECT decision.*, opportunity.id AS opportunity_id,
               opportunity.cluster
        FROM loyal_yield.rebalance_decisions decision
        JOIN loyal_yield.rebalance_opportunities opportunity
          ON opportunity.decision_id = decision.id
        WHERE decision.id = $1
          AND decision.movement_route = 'cross_mint_jupiter'
        "#,
    )
    .bind(decision_id.as_i64())
    .fetch_one(&mut *connection)
    .await?;
    cross_mint_movement_from_row(&row)
}

fn cross_mint_movement_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<CrossMintMovementRecord, OrchestratorError> {
    let source_reserve: String = row.try_get("source_reserve")?;
    let intended_target_reserve: String = row.try_get("target_reserve")?;
    let active_target_reserve: String = row.try_get("active_target_reserve")?;
    let source_mint: String = row.try_get("source_liquidity_mint")?;
    let target_mint: String = row.try_get("target_liquidity_mint")?;
    let custody_mint: String = row.try_get("custody_mint")?;
    let custody_amount_raw: i64 = row.try_get("custody_amount_raw")?;
    let custody_account: String = row.try_get("custody_account")?;
    let terminal_outcome = row
        .try_get::<Option<String>, _>("terminal_outcome")?
        .as_deref()
        .map(CrossMintTerminalOutcome::parse)
        .transpose()?;
    let phase = match terminal_outcome {
        Some(CrossMintTerminalOutcome::CompletedTarget) => CrossMintCustodyPhase::TargetReserve,
        Some(CrossMintTerminalOutcome::RecoveredSource) => CrossMintCustodyPhase::SourceReserve,
        Some(CrossMintTerminalOutcome::CancelledBeforeWithdraw) => {
            CrossMintCustodyPhase::SourceReserve
        }
        Some(CrossMintTerminalOutcome::ClosedByUser) => CrossMintCustodyPhase::ClosedByUser,
        Some(CrossMintTerminalOutcome::ManualIntervention) => {
            CrossMintCustodyPhase::ManualIntervention
        }
        None if row.try_get::<i64, _>("custody_version")? == 0 => {
            CrossMintCustodyPhase::SourceReserve
        }
        None if custody_mint == source_mint => CrossMintCustodyPhase::SourceIdle,
        None if custody_mint == target_mint => CrossMintCustodyPhase::TargetIdle,
        None => {
            return Err(OrchestratorError::StoreInvariant(
                "cross-mint custody mint is neither source nor target".to_owned(),
            ));
        }
    };
    Ok(CrossMintMovementRecord {
        decision_id: DecisionId(row.try_get("id")?),
        opportunity_id: row.try_get("opportunity_id")?,
        cluster: row.try_get("cluster")?,
        vault_id: VaultId(row.try_get("vault_id")?),
        source_snapshot_id: row
            .try_get::<Option<i64>, _>("source_snapshot_id")?
            .map(SnapshotId),
        source_reserve,
        intended_target_reserve,
        active_target_reserve,
        source_mint,
        target_mint,
        planned_amount_raw: row.try_get("amount_raw")?,
        preflight_certification: row
            .try_get::<Option<Value>, _>("cross_mint_preflight_certification")?
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "cross-mint movement is missing preflight certification".to_owned(),
                )
            })?,
        custody_mint,
        custody_amount_raw,
        custody_account,
        custody_observed_balance_raw: row.try_get("custody_observed_balance_raw")?,
        custody_reconciled_slot: row.try_get("custody_reconciled_slot")?,
        custody_version: row.try_get("custody_version")?,
        phase,
        terminal_outcome,
        terminal_evidence: row.try_get("terminal_evidence")?,
        terminal_reason: row.try_get("terminal_reason")?,
        terminal_observed_slot: row.try_get("terminal_observed_slot")?,
        continuation_available_at: row.try_get("continuation_available_at")?,
        continuation_fencing_token: row.try_get("continuation_fencing_token")?,
        continuation_attempt_count: row.try_get("continuation_attempt_count")?,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn autoswap_confirmed_bindings(commitment: &str) -> CrossMintPolicyBindings {
        let earn = |account: &str, constraint_index| CrossMintEarnPolicyBinding {
            policy_account: account.to_owned(),
            observed_slot: 10,
            observed_signature: format!("{account}-signature"),
            source_commitment: "finalized".to_owned(),
            constraint_index,
        };
        CrossMintPolicyBindings {
            settings: "settings".to_owned(),
            vault_index: 1,
            vault_pubkey: "vault".to_owned(),
            delegated_signer: "delegate".to_owned(),
            withdraw: earn("withdraw", 0),
            swap: CrossMintSwapPolicyBinding {
                policy_account: "swap".to_owned(),
                source_shard: "classic".to_owned(),
                enrollment_generation: 1,
                observed_slot: 11,
                observed_signature: "swap-signature".to_owned(),
                source_commitment: commitment.to_owned(),
                max_slippage_bps: 50,
                daily_source_mint_spending_cap: 1_000_000,
                manifest_fingerprint: "manifest".to_owned(),
            },
            deposit: earn("deposit", 1),
        }
    }

    #[test]
    fn autoswap_confirmed_binding_is_admitted_with_finalized_earn_policies() {
        autoswap_confirmed_bindings("confirmed")
            .validate()
            .expect("confirmed Autoswap binding must be admitted");
        autoswap_confirmed_bindings("finalized")
            .validate()
            .expect("finalized Autoswap binding remains admitted");
        assert!(autoswap_confirmed_bindings("processed").validate().is_err());

        let mut unfinalized_earn = autoswap_confirmed_bindings("confirmed");
        unfinalized_earn.withdraw.source_commitment = "confirmed".to_owned();
        assert!(unfinalized_earn.validate().is_err());
    }
}
