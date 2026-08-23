use super::{config::strategy, observe::ObservedRoute};
use loyal_yield_store::fleet_orchestration::{
    MultiplyAction, MultiplyRouteState, RouteGoal, StrategyKey,
};

const TARGET_LTV_TOLERANCE_BPS: u128 = 10;
const MIN_JUPITER_SWAP_RAW: u128 = 50_000;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PlannedAmount {
    Exact(u64),
    All,
    MaxSafe,
    ToTargetLtv,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ActionPlan {
    pub action: MultiplyAction,
    pub strategy_key: Option<StrategyKey>,
    pub amount: PlannedAmount,
    pub destination_account: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum PlannerDecision {
    Execute(ActionPlan),
    Resume(String),
    Complete,
}

/// The only route planner. It selects one literal transaction from confirmed
/// custody and obligation state; it never predicts a multi-transaction graph.
pub fn next_action(route: &MultiplyRouteState, observed: &ObservedRoute) -> PlannerDecision {
    if let Some(operation_id) = &route.current_operation_id {
        return PlannerDecision::Resume(operation_id.clone());
    }
    let active = observed
        .strategies
        .iter()
        .find(|position| position.collateral_deposited_raw > 0 || position.debt_raw > 0)
        .map(|position| position.strategy_key);
    match route.goal {
        RouteGoal::Idle | RouteGoal::Claimed | RouteGoal::ManualRecovery => {
            PlannerDecision::Complete
        }
        RouteGoal::Withdraw => down(route, observed, active, true),
        RouteGoal::Deploy | RouteGoal::Move => {
            let target = route.target_strategy_key.expect("validated route target");
            if active.is_some_and(|current| current != target) {
                return down(route, observed, active, false);
            }
            up(observed, target)
        }
    }
}

fn up(observed: &ObservedRoute, target: StrategyKey) -> PlannerDecision {
    let config = strategy(target);
    let position = observed.position(target);
    if observed.collateral_custody.amount_raw > 0 {
        return execute(
            MultiplyAction::DepositCollateral,
            Some(target),
            PlannedAmount::Exact(observed.collateral_custody.amount_raw),
        );
    }
    if position.collateral_deposited_raw == 0 {
        if observed.claim.amount_raw == 0 {
            return PlannerDecision::Complete;
        }
        return execute(
            MultiplyAction::SwapClaimToCollateral,
            Some(target),
            PlannedAmount::Exact(observed.claim.amount_raw),
        );
    }
    let debt_custody = observed.debt_custody(target).amount_raw;
    if debt_custody > 0 {
        if u128::from(debt_custody) <= MIN_JUPITER_SWAP_RAW {
            return execute(
                MultiplyAction::RepayDebt,
                Some(target),
                if debt_custody >= position.debt_raw {
                    PlannedAmount::All
                } else {
                    PlannedAmount::Exact(debt_custody)
                },
            );
        }
        return execute(
            MultiplyAction::SwapDebtToCollateral,
            Some(target),
            PlannedAmount::Exact(debt_custody),
        );
    }
    let target_debt_value = position
        .collateral_value_sf
        .saturating_mul(u128::from(config.target_ltv_bps))
        / 10_000;
    let tolerance = position
        .collateral_value_sf
        .saturating_mul(TARGET_LTV_TOLERANCE_BPS)
        / 10_000;
    let missing_debt_value = target_debt_value.saturating_sub(position.debt_value_sf);
    let missing_debt_raw = if position.debt_market_price_sf == 0 {
        u128::MAX
    } else {
        missing_debt_value.saturating_mul(u128::from(position.debt_mint_factor))
            / position.debt_market_price_sf
    };
    if position.debt_value_sf.saturating_add(tolerance) < target_debt_value
        && missing_debt_raw > MIN_JUPITER_SWAP_RAW
    {
        return execute(
            MultiplyAction::BorrowDebt,
            Some(target),
            PlannedAmount::ToTargetLtv,
        );
    }
    PlannerDecision::Complete
}

fn down(
    route: &MultiplyRouteState,
    observed: &ObservedRoute,
    active: Option<StrategyKey>,
    for_withdrawal: bool,
) -> PlannerDecision {
    if let Some(key) = active {
        let position = observed.position(key);
        let debt_custody = observed.debt_custody(key).amount_raw;
        if position.debt_raw > 0 {
            if debt_custody > 0 {
                return execute(
                    MultiplyAction::RepayDebt,
                    Some(key),
                    if debt_custody >= position.debt_raw {
                        PlannedAmount::All
                    } else {
                        PlannedAmount::Exact(debt_custody)
                    },
                );
            }
            if observed.collateral_custody.amount_raw > 0 {
                return execute(
                    MultiplyAction::SwapCollateralToDebt,
                    Some(key),
                    PlannedAmount::Exact(observed.collateral_custody.amount_raw),
                );
            }
            return execute(
                MultiplyAction::WithdrawCollateral,
                Some(key),
                PlannedAmount::MaxSafe,
            );
        }
        if position.collateral_deposited_raw > 0 {
            return execute(
                MultiplyAction::WithdrawRemainingCollateral,
                Some(key),
                PlannedAmount::All,
            );
        }
    }
    if observed.collateral_custody.amount_raw > 0 {
        return execute(
            MultiplyAction::SwapCollateralToClaim,
            route.target_strategy_key,
            PlannedAmount::Exact(observed.collateral_custody.amount_raw),
        );
    }
    if for_withdrawal {
        let amount = route
            .withdrawal
            .as_ref()
            .map_or(observed.claim.amount_raw, |value| {
                value.amount_raw.min(observed.claim.amount_raw)
            });
        if amount > 0 {
            return PlannerDecision::Execute(ActionPlan {
                action: MultiplyAction::Claim,
                strategy_key: None,
                amount: PlannedAmount::Exact(amount),
                destination_account: route
                    .withdrawal
                    .as_ref()
                    .map(|value| value.destination_account.clone()),
            });
        }
    }
    if route.goal == RouteGoal::Move {
        return up(
            observed,
            route.target_strategy_key.expect("validated move target"),
        );
    }
    PlannerDecision::Complete
}

fn execute(
    action: MultiplyAction,
    strategy_key: Option<StrategyKey>,
    amount: PlannedAmount,
) -> PlannerDecision {
    PlannerDecision::Execute(ActionPlan {
        action,
        strategy_key,
        amount,
        destination_account: None,
    })
}
