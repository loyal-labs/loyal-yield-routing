use loyal_actions::autonomous_vaults::BackyardVoltrStrategy;

pub const BACKYARD_VOLTR_PRODUCTION_MANAGER_CAP_RAW: u64 = 50_000_000_000;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum VoltrOperationClass {
    WithdrawalRestoration,
    IdleAllocation,
    YieldOptimization,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum VoltrManagerOperation {
    Deposit,
    Withdraw,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct VoltrPosition {
    pub strategy: BackyardVoltrStrategy,
    pub value_raw: u64,
    pub safely_redeemable_raw: u64,
    pub target_raw: u64,
    pub net_apy_bps: i64,
    pub unwind_cost_bps: u64,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct VoltrControllerSnapshot {
    pub context_slot: u64,
    pub idle_raw: u64,
    pub safety_buffer_raw: u64,
    pub active_receipt_demand_raw: u64,
    pub receipt_set_fingerprint: String,
    pub positions: Vec<VoltrPosition>,
    pub max_manager_amount_raw: u64,
    pub now_unix_seconds: u64,
    pub last_normal_optimization_started_at: Option<u64>,
    pub normal_optimization_interval_seconds: u64,
    pub has_nonterminal_signed_generation: bool,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct VoltrManagerLeg {
    pub operation_class: VoltrOperationClass,
    pub operation: VoltrManagerOperation,
    pub strategy: BackyardVoltrStrategy,
    pub amount_raw: u64,
    pub source_context_slot: u64,
    pub receipt_set_fingerprint: String,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum VoltrNextLeg {
    RecoverExisting,
    Execute(VoltrManagerLeg),
    Noop,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum VoltrControllerError {
    InvalidManagerCap,
    InvalidStrategySet,
    ArithmeticOverflow,
    UnrestorableShortfall { shortfall_raw: u64 },
}

/// Produce at most one manager transaction from one confirmed snapshot.
///
/// Recovery and withdrawal liquidity always precede allocation and yield
/// optimization. The function does not perform I/O and does not pre-plan a
/// sibling leg; a caller must confirm, re-observe, and call it again.
pub fn next_voltr_leg(
    snapshot: &VoltrControllerSnapshot,
) -> Result<VoltrNextLeg, VoltrControllerError> {
    if snapshot.max_manager_amount_raw == 0 {
        return Err(VoltrControllerError::InvalidManagerCap);
    }
    if snapshot.positions.len() != BackyardVoltrStrategy::ALL.len()
        || BackyardVoltrStrategy::ALL.iter().any(|strategy| {
            !snapshot
                .positions
                .iter()
                .any(|position| position.strategy == *strategy)
        })
    {
        return Err(VoltrControllerError::InvalidStrategySet);
    }
    if snapshot.has_nonterminal_signed_generation {
        return Ok(VoltrNextLeg::RecoverExisting);
    }

    let required_idle_raw = snapshot
        .safety_buffer_raw
        .checked_add(snapshot.active_receipt_demand_raw)
        .ok_or(VoltrControllerError::ArithmeticOverflow)?;
    let shortfall_raw = required_idle_raw.saturating_sub(snapshot.idle_raw);
    if shortfall_raw > 0 {
        let desired_leg_raw = shortfall_raw.min(snapshot.max_manager_amount_raw);
        let source = snapshot
            .positions
            .iter()
            .filter(|position| position.safely_redeemable_raw > 0)
            .min_by_key(|position| {
                (
                    position.safely_redeemable_raw < desired_leg_raw,
                    position.net_apy_bps,
                    position.unwind_cost_bps,
                    position.strategy,
                )
            })
            .ok_or(VoltrControllerError::UnrestorableShortfall { shortfall_raw })?;
        let amount_raw = desired_leg_raw.min(source.safely_redeemable_raw);
        return Ok(VoltrNextLeg::Execute(VoltrManagerLeg {
            operation_class: VoltrOperationClass::WithdrawalRestoration,
            operation: VoltrManagerOperation::Withdraw,
            strategy: source.strategy,
            amount_raw,
            source_context_slot: snapshot.context_slot,
            receipt_set_fingerprint: snapshot.receipt_set_fingerprint.clone(),
        }));
    }

    let investable_idle_raw = snapshot.idle_raw.saturating_sub(required_idle_raw);
    if investable_idle_raw > 0 {
        if let Some(target) = target_deficit(&snapshot.positions) {
            let deficit_raw = target.target_raw.saturating_sub(target.value_raw);
            let amount_raw = investable_idle_raw
                .min(snapshot.max_manager_amount_raw)
                .min(deficit_raw);
            if amount_raw > 0 {
                return Ok(VoltrNextLeg::Execute(VoltrManagerLeg {
                    operation_class: VoltrOperationClass::IdleAllocation,
                    operation: VoltrManagerOperation::Deposit,
                    strategy: target.strategy,
                    amount_raw,
                    source_context_slot: snapshot.context_slot,
                    receipt_set_fingerprint: snapshot.receipt_set_fingerprint.clone(),
                }));
            }
        }
    }

    let cooldown_elapsed = snapshot
        .last_normal_optimization_started_at
        .is_none_or(|started_at| {
            snapshot.now_unix_seconds.saturating_sub(started_at)
                >= snapshot.normal_optimization_interval_seconds
        });
    if cooldown_elapsed && snapshot.active_receipt_demand_raw == 0 {
        let target = target_deficit(&snapshot.positions);
        let source = snapshot
            .positions
            .iter()
            .filter(|position| {
                position.value_raw > position.target_raw && position.safely_redeemable_raw > 0
            })
            .min_by_key(|position| {
                (
                    position.net_apy_bps,
                    position.unwind_cost_bps,
                    position.strategy,
                )
            });
        if let (Some(source), Some(target)) = (source, target) {
            if target.net_apy_bps > source.net_apy_bps {
                let amount_raw = source
                    .value_raw
                    .saturating_sub(source.target_raw)
                    .min(source.safely_redeemable_raw)
                    .min(snapshot.max_manager_amount_raw);
                if amount_raw > 0 {
                    return Ok(VoltrNextLeg::Execute(VoltrManagerLeg {
                        operation_class: VoltrOperationClass::YieldOptimization,
                        operation: VoltrManagerOperation::Withdraw,
                        strategy: source.strategy,
                        amount_raw,
                        source_context_slot: snapshot.context_slot,
                        receipt_set_fingerprint: snapshot.receipt_set_fingerprint.clone(),
                    }));
                }
            }
        }
    }
    Ok(VoltrNextLeg::Noop)
}

fn target_deficit(positions: &[VoltrPosition]) -> Option<&VoltrPosition> {
    positions
        .iter()
        .filter(|position| position.target_raw > position.value_raw)
        .max_by_key(|position| {
            (
                position.target_raw.saturating_sub(position.value_raw),
                position.net_apy_bps,
                std::cmp::Reverse(position.strategy),
            )
        })
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BackyardVoltrOneLegOracle {
    pub passed: bool,
    pub amounts_raw: Vec<u64>,
    pub strategies: Vec<BackyardVoltrStrategy>,
}

/// Independent deterministic fixture required by the end-state verifier.
pub fn backyard_voltr_one_leg_oracle() -> BackyardVoltrOneLegOracle {
    let mut snapshot = VoltrControllerSnapshot {
        context_slot: 1,
        idle_raw: 0,
        safety_buffer_raw: 0,
        active_receipt_demand_raw: 120_000_000_000,
        receipt_set_fingerprint: "oracle-receipts".to_owned(),
        positions: vec![
            position(BackyardVoltrStrategy::Main, 80_000_000_000, 200),
            position(BackyardVoltrStrategy::Onre, 0, 400),
            position(BackyardVoltrStrategy::Prime, 80_000_000_000, 300),
            position(BackyardVoltrStrategy::Maple, 0, 500),
        ],
        max_manager_amount_raw: BACKYARD_VOLTR_PRODUCTION_MANAGER_CAP_RAW,
        now_unix_seconds: 10_000,
        last_normal_optimization_started_at: None,
        normal_optimization_interval_seconds: 3_600,
        has_nonterminal_signed_generation: false,
    };
    let mut amounts_raw = Vec::new();
    let mut strategies = Vec::new();
    for _ in 0..3 {
        let Ok(VoltrNextLeg::Execute(leg)) = next_voltr_leg(&snapshot) else {
            return BackyardVoltrOneLegOracle {
                passed: false,
                amounts_raw,
                strategies,
            };
        };
        if leg.operation_class != VoltrOperationClass::WithdrawalRestoration
            || leg.operation != VoltrManagerOperation::Withdraw
        {
            return BackyardVoltrOneLegOracle {
                passed: false,
                amounts_raw,
                strategies,
            };
        }
        amounts_raw.push(leg.amount_raw);
        strategies.push(leg.strategy);
        snapshot.idle_raw = snapshot.idle_raw.saturating_add(leg.amount_raw);
        snapshot.context_slot += 1;
        let position = snapshot
            .positions
            .iter_mut()
            .find(|position| position.strategy == leg.strategy)
            .expect("oracle has every strategy");
        position.value_raw = position.value_raw.saturating_sub(leg.amount_raw);
        position.safely_redeemable_raw = position
            .safely_redeemable_raw
            .saturating_sub(leg.amount_raw);
    }
    let stopped = next_voltr_leg(&snapshot) == Ok(VoltrNextLeg::Noop);
    BackyardVoltrOneLegOracle {
        passed: stopped
            && amounts_raw == vec![50_000_000_000, 50_000_000_000, 20_000_000_000]
            && strategies
                == vec![
                    BackyardVoltrStrategy::Main,
                    BackyardVoltrStrategy::Prime,
                    BackyardVoltrStrategy::Main,
                ],
        amounts_raw,
        strategies,
    }
}

fn position(strategy: BackyardVoltrStrategy, value_raw: u64, net_apy_bps: i64) -> VoltrPosition {
    VoltrPosition {
        strategy,
        value_raw,
        safely_redeemable_raw: value_raw,
        target_raw: value_raw,
        net_apy_bps,
        unwind_cost_bps: 0,
    }
}
