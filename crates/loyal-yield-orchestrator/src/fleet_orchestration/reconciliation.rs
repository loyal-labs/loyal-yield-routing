//! Pure post-effect reconciliation semantics for confirmed route submissions.
//!
//! A submission only reaches reconciliation after
//! `classify_authoritative_signature_status` observed its exact signature at
//! confirmed commitment without a transaction error, so execution is already
//! proven when these predicates run. Reconciliation binds a post-execution
//! observation to the decision; it does not re-prove that the route executed.
//!
//! Predicates therefore gate on observation freshness and account identity
//! only. Balance arithmetic cannot gate a terminal transition: the vault ATA
//! keeps moving after the deposit lands (inflight user deposits, withdrawals,
//! protocol rounding), so any predicate demanding an exact post-balance is
//! unsatisfiable as soon as one lamport of unrelated movement arrives. Residual
//! balances stay in the decision as evidence instead.

use std::collections::BTreeSet;
use std::sync::Mutex;

/// Route identity and planned amounts carried by a confirmed idle-vault
/// deposit, taken from the opportunity that produced the submission.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct IdleDepositRouteContract<'a> {
    /// Slot at which the route's signature reached confirmed commitment.
    pub confirmed_slot: i64,
    /// Liquidity mint the route deposited.
    pub liquidity_mint: &'a str,
    /// Vault-owned idle ATA the route drained.
    pub idle_token_account: &'a str,
    /// Liquidity amount the route was built to deposit.
    pub deposited_amount_raw: i64,
    /// Idle balance observed before the route was signed, when the plan
    /// recorded one. Evidence only; it never gates the transition.
    pub baseline_idle_amount_raw: Option<i64>,
}

/// Vault state read at or after the confirmed slot, from either the durable
/// current-state projections or a fresh `minContextSlot`-fenced chain preview.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct IdleDepositPostEffectObservation<'a> {
    /// Slot every account in the observation was read at.
    pub observed_slot: i64,
    /// Liquidity mint of the target reserve as observed.
    pub target_liquidity_mint: &'a str,
    /// Vault liquidity ATA bound to the observed target reserve.
    pub vault_liquidity_ata: &'a str,
    /// Liquidity left idle in the vault ATA at `observed_slot`.
    pub idle_amount_raw: i64,
}

/// Which identity field failed to bind the observation to the route.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum IdleDepositIdentityField {
    LiquidityMint,
    IdleTokenAccount,
}

impl IdleDepositIdentityField {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::LiquidityMint => "liquidity_mint",
            Self::IdleTokenAccount => "idle_token_account",
        }
    }
}

/// Residual idle liquidity recorded alongside a reconciled deposit.
///
/// `unexplained_surplus_raw` is the amount the ATA holds beyond what the plan
/// predicted. It is normal and expected: liquidity that arrived between the
/// planner's observation and execution stays behind, and protocol rounding can
/// leave a unit or two. It is reported so operators can see the residual, and
/// deliberately does not gate reconciliation.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct IdleDepositResidualEvidence {
    pub idle_amount_raw: i64,
    pub planned_residual_raw: i64,
    pub unexplained_surplus_raw: i64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum IdleDepositPostEffectDecision {
    /// The observation is post-execution and identity-bound: close the
    /// decision against it.
    Reconcile(IdleDepositResidualEvidence),
    /// The observation predates the confirmed slot, so it cannot describe
    /// post-deposit state. Read again; never terminal.
    ObservationPredatesConfirmation {
        observed_slot: i64,
        confirmed_slot: i64,
    },
    /// The observation describes different accounts than the route did.
    /// Retrying cannot fix this; it needs an operator.
    IdentityMismatch { field: IdleDepositIdentityField },
}

/// Decides whether an observation may close out a confirmed idle-vault
/// deposit.
///
/// Freshness plus identity is the whole contract. The caller has already
/// fenced the read at `min_context_slot >= confirmed_slot`, so the slot check
/// is a defensive restatement of that fence rather than the primary guarantee.
pub fn classify_idle_deposit_post_effect(
    contract: IdleDepositRouteContract<'_>,
    observation: IdleDepositPostEffectObservation<'_>,
) -> IdleDepositPostEffectDecision {
    if observation.target_liquidity_mint != contract.liquidity_mint {
        return IdleDepositPostEffectDecision::IdentityMismatch {
            field: IdleDepositIdentityField::LiquidityMint,
        };
    }
    if observation.vault_liquidity_ata != contract.idle_token_account {
        return IdleDepositPostEffectDecision::IdentityMismatch {
            field: IdleDepositIdentityField::IdleTokenAccount,
        };
    }
    if observation.observed_slot < contract.confirmed_slot {
        return IdleDepositPostEffectDecision::ObservationPredatesConfirmation {
            observed_slot: observation.observed_slot,
            confirmed_slot: contract.confirmed_slot,
        };
    }
    IdleDepositPostEffectDecision::Reconcile(idle_deposit_residual_evidence(
        contract,
        observation.idle_amount_raw,
    ))
}

/// Residual the plan predicted, and how far the observation exceeds it.
pub fn idle_deposit_residual_evidence(
    contract: IdleDepositRouteContract<'_>,
    idle_amount_raw: i64,
) -> IdleDepositResidualEvidence {
    let planned_residual_raw = contract
        .baseline_idle_amount_raw
        .map(|baseline| baseline.saturating_sub(contract.deposited_amount_raw))
        .unwrap_or_default()
        .max(0);
    IdleDepositResidualEvidence {
        idle_amount_raw,
        planned_residual_raw,
        unexplained_surplus_raw: idle_amount_raw.saturating_sub(planned_residual_raw).max(0),
    }
}

/// Attempts served at the minimum delay before backoff engages. A reconciler
/// waiting for the projections to catch up resolves inside this window.
pub const RECONCILIATION_FAST_RETRY_ATTEMPTS: i32 = 12;
pub const RECONCILIATION_MIN_RETRY_SECONDS: i64 = 1;
pub const RECONCILIATION_MAX_RETRY_SECONDS: i64 = 60;
/// Attempt count past which a submission is reported as stalled. At the capped
/// delay this is roughly ten minutes of failure, well beyond any transient RPC
/// fault.
pub const RECONCILIATION_STALL_ATTEMPTS: i32 = 60;

/// Backoff for a reconciliation attempt that could not reach a terminal state.
///
/// Reconciliation retries forever by design — a confirmed money movement must
/// never be stranded — so the schedule, not an attempt cap, is what keeps a
/// permanently failing submission from burning RPC at one request per second
/// indefinitely.
pub fn reconciliation_retry_delay_seconds(attempt_count: i32) -> i64 {
    let over_fast_window = attempt_count.saturating_sub(RECONCILIATION_FAST_RETRY_ATTEMPTS);
    if over_fast_window <= 0 {
        return RECONCILIATION_MIN_RETRY_SECONDS;
    }
    let doublings = u32::try_from(over_fast_window).unwrap_or(u32::MAX).min(16);
    RECONCILIATION_MIN_RETRY_SECONDS
        .saturating_mul(1i64 << doublings)
        .clamp(
            RECONCILIATION_MIN_RETRY_SECONDS,
            RECONCILIATION_MAX_RETRY_SECONDS,
        )
}

/// Whether a submission has failed long enough to deserve an operator signal.
pub fn reconciliation_is_stalled(attempt_count: i32) -> bool {
    attempt_count >= RECONCILIATION_STALL_ATTEMPTS
}

/// Submissions whose stall has already been reported to operators.
///
/// `reconciliation_is_stalled` stays true for every attempt once it trips, so
/// an unlatched signal repeats for as long as the submission is stuck — which
/// for a permanently failing predicate is forever. The latch makes the operator
/// signal one-shot per submission; the submission row's attempt count and error
/// detail remain the live state of record for anything watching progress.
///
/// A worker restart re-arms the latch by construction, which is the intended
/// behaviour: a stall that survives a restart is worth saying again.
#[derive(Debug)]
pub struct ReconciliationStallLatch {
    reported: Mutex<BTreeSet<i64>>,
}

/// Bound on tracked submissions. Reaching it re-arms the latch rather than
/// silencing it: this many distinct stalls is itself an incident, and one
/// repeated signal is the cheaper failure.
pub const MAX_LATCHED_RECONCILIATION_STALLS: usize = 1024;

impl ReconciliationStallLatch {
    pub const fn new() -> Self {
        Self {
            reported: Mutex::new(BTreeSet::new()),
        }
    }

    /// Claims the right to report this submission's stall, returning true to
    /// exactly one caller per submission even under concurrent reconciler
    /// tasks.
    pub fn claim(&self, submission_id: i64) -> bool {
        let Ok(mut reported) = self.reported.lock() else {
            // A poisoned latch must not silence an operator signal.
            return true;
        };
        if reported.contains(&submission_id) {
            return false;
        }
        if reported.len() >= MAX_LATCHED_RECONCILIATION_STALLS {
            reported.clear();
        }
        reported.insert(submission_id);
        true
    }
}

impl Default for ReconciliationStallLatch {
    fn default() -> Self {
        Self::new()
    }
}
