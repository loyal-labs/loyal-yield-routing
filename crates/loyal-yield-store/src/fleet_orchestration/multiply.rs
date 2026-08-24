//! Persisted contract for the Kamino Multiply production engine.
//!
//! A route row describes product intent and the latest confirmed observation.
//! Every Solana transaction lives in `multiply_operations`; the route stores
//! only the id of the one operation that currently owns execution.

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use thiserror::Error;

pub const MULTIPLY_STATE_SCHEMA_VERSION: u16 = 7;
pub const MULTIPLY_ENGINE_VERSION: &str = "earn_max_v1";
pub const MULTIPLY_DEFAULT_LEASE_SECONDS: i64 = 30;

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum StrategyKey {
    SyrupUsdcUsdc,
    SyrupUsdcPyusd,
}

impl StrategyKey {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::SyrupUsdcUsdc => "syrup_usdc_usdc",
            Self::SyrupUsdcPyusd => "syrup_usdc_pyusd",
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RouteGoal {
    Idle,
    Deploy,
    Move,
    Withdraw,
    Claimed,
    ManualRecovery,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct TokenBalance {
    pub account: String,
    pub mint: String,
    pub token_program: String,
    pub amount_raw: u64,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(
    tag = "kind",
    rename_all = "snake_case",
    rename_all_fields = "camelCase"
)]
pub enum MultiplyPosition {
    Idle {
        claim: TokenBalance,
    },
    Active {
        strategy_key: StrategyKey,
        obligation: String,
        collateral: TokenBalance,
        debt: TokenBalance,
        debt_amount_sf: String,
        health_factor_ppm: u64,
    },
}

impl MultiplyPosition {
    pub fn strategy_key(&self) -> Option<StrategyKey> {
        match self {
            Self::Idle { .. } => None,
            Self::Active { strategy_key, .. } => Some(*strategy_key),
        }
    }

    pub fn observed_accounts(&self) -> impl Iterator<Item = &TokenBalance> {
        let mut balances = [None, None];
        match self {
            Self::Idle { claim } => balances[0] = Some(claim),
            Self::Active {
                collateral, debt, ..
            } => {
                balances[0] = Some(collateral);
                balances[1] = Some(debt);
            }
        }
        balances.into_iter().flatten()
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct DepositEvidence {
    pub request_id: String,
    pub transaction_signature: String,
    pub wallet_account: String,
    pub wallet_pre_amount_raw: u64,
    pub wallet_post_amount_raw: u64,
    pub vault_pre_amount_raw: u64,
    pub vault_post_amount_raw: u64,
    pub amount_raw: u64,
    pub observed_slot: u64,
    pub observed_at: DateTime<Utc>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum WithdrawalStatus {
    Requested,
    Unwinding,
    Claimable,
    Claimed,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct Withdrawal {
    pub request_id: String,
    pub destination_account: String,
    pub amount_raw: u64,
    pub status: WithdrawalStatus,
    pub requested_at: DateTime<Utc>,
    /// Product SLA. A claim may become available sooner when unwind completes.
    pub ready_by: DateTime<Utc>,
    pub unwind_completed_at: Option<DateTime<Utc>>,
    pub claim_signature: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct MultiplyFrontendView {
    pub generation: u64,
    pub status: String,
    pub strategy_key: Option<StrategyKey>,
    pub claim_amount_raw: u64,
    pub collateral_amount_raw: u64,
    pub debt_amount_raw: u64,
    pub withdrawal_status: Option<WithdrawalStatus>,
    pub observed_slot: u64,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct MultiplyRouteState {
    pub schema_version: u16,
    pub engine_version: String,
    pub route_key: String,
    pub settings: String,
    pub vault_index: u8,
    pub vault: String,
    pub policy_seed_base: u64,
    pub generation: u64,
    pub cycle: u64,
    pub goal: RouteGoal,
    pub target_strategy_key: Option<StrategyKey>,
    pub position: MultiplyPosition,
    pub deposit: Option<DepositEvidence>,
    pub withdrawal: Option<Withdrawal>,
    pub current_operation_id: Option<String>,
    pub manual_recovery_reason: Option<String>,
    pub observed_slot: u64,
    pub observed_at: DateTime<Utc>,
    pub frontend: MultiplyFrontendView,
}

impl MultiplyRouteState {
    pub fn new(
        route_key: String,
        settings: String,
        vault_index: u8,
        vault: String,
        policy_seed_base: u64,
        claim: TokenBalance,
        observed_slot: u64,
        observed_at: DateTime<Utc>,
    ) -> Result<Self, MultiplyStateError> {
        let mut state = Self {
            schema_version: MULTIPLY_STATE_SCHEMA_VERSION,
            engine_version: MULTIPLY_ENGINE_VERSION.to_owned(),
            route_key,
            settings,
            vault_index,
            vault,
            policy_seed_base,
            generation: 1,
            cycle: 1,
            goal: RouteGoal::Idle,
            target_strategy_key: None,
            position: MultiplyPosition::Idle { claim },
            deposit: None,
            withdrawal: None,
            current_operation_id: None,
            manual_recovery_reason: None,
            observed_slot,
            observed_at,
            frontend: MultiplyFrontendView {
                generation: 1,
                status: String::new(),
                strategy_key: None,
                claim_amount_raw: 0,
                collateral_amount_raw: 0,
                debt_amount_raw: 0,
                withdrawal_status: None,
                observed_slot,
            },
        };
        state.frontend = project_frontend(&state);
        state.validate_persisted()?;
        Ok(state)
    }

    pub fn withdrawal_matches(
        &self,
        request_id: &str,
        destination_account: &str,
        amount_raw: u64,
    ) -> bool {
        self.withdrawal.as_ref().is_some_and(|withdrawal| {
            withdrawal.request_id == request_id
                && withdrawal.destination_account == destination_account
                && withdrawal.amount_raw == amount_raw
        })
    }

    pub fn validate_persisted(&self) -> Result<(), MultiplyStateError> {
        if self.schema_version != MULTIPLY_STATE_SCHEMA_VERSION
            || self.engine_version != MULTIPLY_ENGINE_VERSION
            || self.route_key.trim().is_empty()
            || self.settings.trim().is_empty()
            || self.vault.trim().is_empty()
            || self.policy_seed_base == 0
            || self.generation == 0
            || self.cycle == 0
            || self.observed_slot == 0
        {
            return Err(MultiplyStateError::InvalidRouteIdentity);
        }
        for balance in self.position.observed_accounts() {
            validate_token_balance(balance)?;
        }
        if self.goal == RouteGoal::ManualRecovery
            && self
                .manual_recovery_reason
                .as_deref()
                .is_none_or(|reason| reason.trim().is_empty())
        {
            return Err(MultiplyStateError::ManualRecoveryReasonMissing);
        }
        if self.goal != RouteGoal::ManualRecovery && self.manual_recovery_reason.is_some() {
            return Err(MultiplyStateError::UnexpectedManualRecoveryReason);
        }
        if matches!(self.goal, RouteGoal::Deploy | RouteGoal::Move)
            && self.target_strategy_key.is_none()
        {
            return Err(MultiplyStateError::TargetStrategyMissing);
        }
        if let Some(withdrawal) = &self.withdrawal {
            if withdrawal.request_id.trim().is_empty()
                || withdrawal.destination_account.trim().is_empty()
                || withdrawal.amount_raw == 0
                || withdrawal.ready_by < withdrawal.requested_at
                || withdrawal.ready_by - withdrawal.requested_at > chrono::Duration::minutes(10)
                || (withdrawal.status == WithdrawalStatus::Claimed
                    && withdrawal
                        .claim_signature
                        .as_deref()
                        .is_none_or(str::is_empty))
            {
                return Err(MultiplyStateError::InvalidWithdrawal);
            }
        }
        if self.frontend != project_frontend(self) {
            return Err(MultiplyStateError::FrontendProjectionDrift);
        }
        Ok(())
    }

    pub fn advance(
        mut self,
        position: MultiplyPosition,
        observed_slot: u64,
        observed_at: DateTime<Utc>,
    ) -> Self {
        self.generation += 1;
        self.position = position;
        self.observed_slot = observed_slot;
        self.observed_at = observed_at;
        self.frontend = project_frontend(&self);
        self
    }

    pub fn admit_deposit(
        mut self,
        evidence: DepositEvidence,
        target: StrategyKey,
    ) -> Result<Self, MultiplyStateError> {
        if evidence.request_id.trim().is_empty()
            || evidence.transaction_signature.trim().is_empty()
            || evidence.wallet_account.trim().is_empty()
            || evidence.amount_raw == 0
            || evidence
                .wallet_pre_amount_raw
                .saturating_sub(evidence.wallet_post_amount_raw)
                != evidence.amount_raw
            || evidence
                .vault_post_amount_raw
                .saturating_sub(evidence.vault_pre_amount_raw)
                != evidence.amount_raw
        {
            return Err(MultiplyStateError::InvalidDeposit);
        }
        self.generation += 1;
        self.cycle += 1;
        self.goal = RouteGoal::Deploy;
        self.target_strategy_key = Some(target);
        self.deposit = Some(evidence);
        self.withdrawal = None;
        self.manual_recovery_reason = None;
        self.frontend = project_frontend(&self);
        Ok(self)
    }

    pub fn request_move(mut self, target: StrategyKey) -> Result<Self, MultiplyStateError> {
        if self.current_operation_id.is_some() || self.position.strategy_key() == Some(target) {
            return Err(MultiplyStateError::InvalidGoalChange);
        }
        self.generation += 1;
        self.goal = RouteGoal::Move;
        self.target_strategy_key = Some(target);
        self.frontend = project_frontend(&self);
        Ok(self)
    }

    pub fn request_withdrawal(
        mut self,
        request_id: String,
        destination_account: String,
        amount_raw: u64,
        requested_at: DateTime<Utc>,
    ) -> Result<Self, MultiplyStateError> {
        let is_same_request =
            self.withdrawal_matches(&request_id, &destination_account, amount_raw);
        if is_same_request {
            return Ok(self);
        }
        let has_pending_withdrawal = self
            .withdrawal
            .as_ref()
            .is_some_and(|withdrawal| withdrawal.status != WithdrawalStatus::Claimed);
        let request_id_reused = self
            .withdrawal
            .as_ref()
            .is_some_and(|withdrawal| withdrawal.request_id == request_id);
        if self.current_operation_id.is_some()
            || has_pending_withdrawal
            || request_id_reused
            || request_id.trim().is_empty()
            || destination_account.trim().is_empty()
            || amount_raw == 0
        {
            return Err(MultiplyStateError::InvalidGoalChange);
        }
        self.generation += 1;
        self.goal = RouteGoal::Withdraw;
        self.withdrawal = Some(Withdrawal {
            request_id,
            destination_account,
            amount_raw,
            status: WithdrawalStatus::Requested,
            requested_at,
            ready_by: requested_at + chrono::Duration::minutes(10),
            unwind_completed_at: None,
            claim_signature: None,
        });
        self.frontend = project_frontend(&self);
        Ok(self)
    }

    pub fn cancel_withdrawal(mut self, request_id: &str) -> Result<Self, MultiplyStateError> {
        let can_cancel = self.current_operation_id.is_none()
            && self.withdrawal.as_ref().is_some_and(|withdrawal| {
                withdrawal.request_id == request_id
                    && withdrawal.status == WithdrawalStatus::Requested
            });
        if !can_cancel {
            return Err(MultiplyStateError::InvalidGoalChange);
        }
        self.generation += 1;
        self.goal = RouteGoal::Idle;
        self.withdrawal = None;
        self.frontend = project_frontend(&self);
        Ok(self)
    }
}

pub fn project_frontend(state: &MultiplyRouteState) -> MultiplyFrontendView {
    let (claim, collateral, debt) = match &state.position {
        MultiplyPosition::Idle { claim } => (claim.amount_raw, 0, 0),
        MultiplyPosition::Active {
            collateral, debt, ..
        } => (0, collateral.amount_raw, debt.amount_raw),
    };
    MultiplyFrontendView {
        generation: state.generation,
        status: match state.goal {
            RouteGoal::Idle => "idle",
            RouteGoal::Deploy => "deploying",
            RouteGoal::Move => "moving",
            RouteGoal::Withdraw => "withdrawing",
            RouteGoal::Claimed => "claimed",
            RouteGoal::ManualRecovery => "manual_recovery",
        }
        .to_owned(),
        strategy_key: state.position.strategy_key(),
        claim_amount_raw: claim,
        collateral_amount_raw: collateral,
        debt_amount_raw: debt,
        withdrawal_status: state.withdrawal.as_ref().map(|value| value.status),
        observed_slot: state.observed_slot,
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MultiplyAction {
    RequestWithdrawal,
    CancelWithdrawal,
    DepositClaimAsset,
    SwapClaimToCollateral,
    DepositCollateral,
    BorrowDebt,
    SwapDebtToCollateral,
    WithdrawCollateral,
    SwapCollateralToDebt,
    RepayDebt,
    WithdrawRemainingCollateral,
    SwapCollateralToClaim,
    Claim,
}

impl MultiplyAction {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::RequestWithdrawal => "request_withdrawal",
            Self::CancelWithdrawal => "cancel_withdrawal",
            Self::DepositClaimAsset => "deposit_claim_asset",
            Self::SwapClaimToCollateral => "swap_claim_to_collateral",
            Self::DepositCollateral => "deposit_collateral",
            Self::BorrowDebt => "borrow_debt",
            Self::SwapDebtToCollateral => "swap_debt_to_collateral",
            Self::WithdrawCollateral => "withdraw_collateral",
            Self::SwapCollateralToDebt => "swap_collateral_to_debt",
            Self::RepayDebt => "repay_debt",
            Self::WithdrawRemainingCollateral => "withdraw_remaining_collateral",
            Self::SwapCollateralToClaim => "swap_collateral_to_claim",
            Self::Claim => "claim",
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MultiplyOperationStatus {
    Prepared,
    SignedPersisted,
    BroadcastIntent,
    Confirmed,
    ReconciliationPending,
    Reconciled,
    Expired,
    ManualRecovery,
}

impl MultiplyOperationStatus {
    pub const fn is_terminal(self) -> bool {
        matches!(
            self,
            Self::Reconciled | Self::Expired | Self::ManualRecovery
        )
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct TokenDelta {
    pub account: String,
    pub mint: String,
    pub raw_delta: i64,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct ObligationDelta {
    pub obligation: String,
    pub collateral_raw_delta: i64,
    pub debt_raw_delta: i64,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct TokenAmountBefore {
    pub account: String,
    pub mint: String,
    pub amount_raw: u64,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct ObligationBefore {
    pub obligation: String,
    pub collateral_raw: u64,
    pub debt_raw: u64,
    pub debt_amount_sf: String,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct ExpectedEffects {
    #[serde(default)]
    pub token_amounts_before: Vec<TokenAmountBefore>,
    pub token_deltas: Vec<TokenDelta>,
    #[serde(default)]
    pub obligation_before: Option<ObligationBefore>,
    pub obligation_delta: Option<ObligationDelta>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct MultiplyOperation {
    pub operation_id: String,
    pub route_key: String,
    pub cycle: u64,
    pub engine_version: String,
    pub action: MultiplyAction,
    pub strategy_key: Option<StrategyKey>,
    pub status: MultiplyOperationStatus,
    pub idempotency_key: String,
    pub expected_effects: ExpectedEffects,
    pub policy_account: Option<String>,
    pub policy_data_sha256: Option<String>,
    pub message_sha256: Option<String>,
    #[serde(skip_serializing, skip_deserializing)]
    pub signed_wire: Option<Vec<u8>>,
    pub signed_wire_sha256: Option<String>,
    pub transaction_signature: Option<String>,
    pub source_instruction_index: Option<u16>,
    pub recent_blockhash: Option<String>,
    pub last_valid_block_height: Option<u64>,
    pub broadcast_intent_at: Option<DateTime<Utc>>,
    pub confirmed_slot: Option<u64>,
    pub reconciliation_sha256: Option<String>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

impl MultiplyOperation {
    pub fn validate(&self) -> Result<(), MultiplyStateError> {
        if self.operation_id.trim().is_empty()
            || self.route_key.trim().is_empty()
            || self.cycle == 0
            || self.engine_version != MULTIPLY_ENGINE_VERSION
            || self.idempotency_key.trim().is_empty()
        {
            return Err(MultiplyStateError::InvalidOperationIdentity);
        }
        for delta in &self.expected_effects.token_deltas {
            if delta.account.trim().is_empty() || delta.mint.trim().is_empty() {
                return Err(MultiplyStateError::InvalidExpectedEffects);
            }
        }
        Ok(())
    }
}

fn validate_token_balance(balance: &TokenBalance) -> Result<(), MultiplyStateError> {
    if balance.account.trim().is_empty()
        || balance.mint.trim().is_empty()
        || balance.token_program.trim().is_empty()
    {
        Err(MultiplyStateError::InvalidTokenBalance)
    } else {
        Ok(())
    }
}

#[derive(Debug, Error, PartialEq, Eq)]
pub enum MultiplyStateError {
    #[error("invalid Multiply route identity or observation")]
    InvalidRouteIdentity,
    #[error("invalid token balance identity")]
    InvalidTokenBalance,
    #[error("target strategy is required for this goal")]
    TargetStrategyMissing,
    #[error("manual recovery requires a reason")]
    ManualRecoveryReasonMissing,
    #[error("manual recovery reason is present outside manual recovery")]
    UnexpectedManualRecoveryReason,
    #[error("withdrawal violates identity, amount, time, or claim evidence")]
    InvalidWithdrawal,
    #[error("frontend view is not the projection of the route state")]
    FrontendProjectionDrift,
    #[error("invalid operation identity")]
    InvalidOperationIdentity,
    #[error("invalid operation expected effects")]
    InvalidExpectedEffects,
    #[error("deposit evidence does not prove equal wallet and vault deltas")]
    InvalidDeposit,
    #[error("route goal change is invalid while another action is active")]
    InvalidGoalChange,
}
