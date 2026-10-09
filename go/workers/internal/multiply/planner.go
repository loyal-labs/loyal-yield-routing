package multiply

// The only route planner, ported from
// 91694cd9^:crates/loyal-fleet-worker/src/multiply/planner.rs. It selects one literal
// transaction from confirmed custody and obligation state; it never predicts a
// multi-transaction graph.

import (
	"math/big"
)

const (
	targetLTVToleranceBPS = 10
	minJupiterSwapRaw     = uint64(50_000)
)

// PlannedAmount mirrors planner::PlannedAmount.
type PlannedAmount struct {
	Mode    string // "exact" | "all" | "max_safe" | "to_target_ltv"
	Exactly uint64 // set when Mode == "exact"
}

var (
	AmountAll         = PlannedAmount{Mode: "all"}
	AmountMaxSafe     = PlannedAmount{Mode: "max_safe"}
	AmountToTargetLTV = PlannedAmount{Mode: "to_target_ltv"}
)

func AmountExact(value uint64) PlannedAmount { return PlannedAmount{Mode: "exact", Exactly: value} }

// ActionPlan is one selected literal action.
type ActionPlan struct {
	Action             MultiplyAction
	StrategyKey        StrategyKey // "" when user-side
	Amount             PlannedAmount
	DestinationAccount string
}

// PlannerDecision mirrors planner::PlannerDecision.
type PlannerDecision struct {
	Kind      string // "execute" | "resume" | "complete"
	Plan      *ActionPlan
	ResumeID  string
	Condition string // deterministic unresolved custody reason
}

func ExecuteDecision(plan ActionPlan) PlannerDecision {
	return PlannerDecision{Kind: "execute", Plan: &plan}
}

func CompleteDecision() PlannerDecision { return PlannerDecision{Kind: "complete"} }

// NextAction mirrors next_action.
func NextAction(route *RouteState, observed *ObservedRoute, topology *EarnMaxTopology) PlannerDecision {
	if route == nil {
		return PlannerDecision{Kind: "invalid_observation"}
	}
	if route.CurrentOperationID != nil {
		return PlannerDecision{Kind: "resume", ResumeID: *route.CurrentOperationID}
	}
	if observed == nil || topology == nil {
		return PlannerDecision{Kind: "invalid_observation"}
	}
	// Only an explicit absence from a complete bank snapshot is empty.
	// A partial caller observation must not become completion or permission.
	if !observed.ActiveStrategyIsCoherent() {
		return PlannerDecision{Kind: "invalid_observation"}
	}
	for _, config := range topology.StrategyCatalog() {
		if observed.Position(config.Key) == nil || observed.CollateralCustody(config.Key) == nil || observed.DebtCustody(config.Key) == nil {
			return PlannerDecision{Kind: "invalid_observation"}
		}
	}
	active := StrategyKey("")
	for _, position := range observed.Strategies {
		if position == nil {
			return PlannerDecision{Kind: "invalid_observation"}
		}
		if position.CollateralDepositedRaw > 0 || position.DebtRaw > 0 {
			active = position.StrategyKey
			break
		}
	}
	switch route.Goal {
	case GoalIdle, GoalClaimed, GoalManualRecovery:
		return CompleteDecision()
	case GoalWithdraw:
		return planDown(observed, active)
	case GoalDeploy:
		return planUp(observed, SyrupUsdcUsdc, topology)
	}
	return CompleteDecision()
}

func planUp(observed *ObservedRoute, target StrategyKey, topology *EarnMaxTopology) PlannerDecision {
	config := topology.Strategies[target]
	position := observed.Position(target)
	if position == nil {
		return PlannerDecision{Kind: "invalid_observation"}
	}
	collateralCustody := observed.CollateralCustody(target)
	if collateralCustody == nil || observed.DebtCustody(target) == nil {
		return PlannerDecision{Kind: "invalid_observation"}
	}
	if collateralCustody != nil && collateralCustody.AmountRaw > 0 {
		return ExecuteDecision(ActionPlan{
			Action: ActionDepositCollateral, StrategyKey: target,
			Amount: AmountExact(collateralCustody.AmountRaw),
		})
	}
	if observed.Claim.AmountRaw > 0 {
		return ExecuteDecision(ActionPlan{
			Action: ActionSwapClaimToCollateral, StrategyKey: target,
			Amount: AmountExact(observed.Claim.AmountRaw),
		})
	}
	if position.CollateralDepositedRaw == 0 {
		return CompleteDecision()
	}
	if debtCustody := observed.DebtCustody(target); debtCustody != nil && debtCustody.AmountRaw > 0 {
		if debtCustody.AmountRaw > minJupiterSwapRaw {
			return ExecuteDecision(ActionPlan{
				Action: ActionSwapDebtToCollateral, StrategyKey: target,
				Amount: AmountExact(debtCustody.AmountRaw),
			})
		}
		amount := AmountExact(debtCustody.AmountRaw)
		if debtCustody.AmountRaw >= position.DebtRaw {
			amount = AmountAll
		}
		return ExecuteDecision(ActionPlan{Action: ActionRepayDebt, StrategyKey: target, Amount: amount})
	}
	if !validMarketValues(position) || position.DebtMintFactor == 0 {
		return PlannerDecision{Kind: "invalid_observation"}
	}
	collateralValue := new(big.Int).Set(position.CollateralValueSF)
	targetDebtValue := saturatingU128(new(big.Int).Mul(collateralValue, new(big.Int).SetUint64(uint64(config.TargetLTVBPS))))
	targetDebtValue.Div(targetDebtValue, big.NewInt(10_000))
	tolerance := saturatingU128(new(big.Int).Mul(collateralValue, big.NewInt(targetLTVToleranceBPS)))
	tolerance.Div(tolerance, big.NewInt(10_000))
	missingDebtValue := saturatingU128(new(big.Int).Sub(targetDebtValue, position.DebtValueSF))
	missingDebtRaw := saturatingU128(new(big.Int).Lsh(big.NewInt(1), 128))
	if position.DebtMarketPriceSF.Sign() != 0 {
		missingDebtRaw = saturatingU128(new(big.Int).Mul(missingDebtValue, new(big.Int).SetUint64(position.DebtMintFactor)))
		missingDebtRaw.Div(missingDebtRaw, position.DebtMarketPriceSF)
	}
	debtValuePlusTolerance := saturatingU128(new(big.Int).Add(position.DebtValueSF, tolerance))
	if debtValuePlusTolerance.Cmp(targetDebtValue) < 0 &&
		missingDebtRaw.Cmp(new(big.Int).SetUint64(minJupiterSwapRaw)) > 0 {
		return ExecuteDecision(ActionPlan{
			Action: ActionBorrowDebt, StrategyKey: target, Amount: AmountToTargetLTV,
		})
	}
	return CompleteDecision()
}

func planDown(observed *ObservedRoute, active StrategyKey) PlannerDecision {
	if active.Valid() {
		position := observed.Position(active)
		if position == nil {
			return PlannerDecision{Kind: "invalid_observation"}
		}
		debtCustody := observed.DebtCustody(active)
		if debtCustody == nil || observed.CollateralCustody(active) == nil {
			return PlannerDecision{Kind: "invalid_observation"}
		}
		debtCustodyRaw := uint64(0)
		if debtCustody != nil {
			debtCustodyRaw = debtCustody.AmountRaw
		}
		if position.DebtRaw > 0 {
			if debtCustodyRaw > 0 {
				amount := AmountExact(debtCustodyRaw)
				if debtCustodyRaw >= position.DebtRaw {
					amount = AmountAll
				}
				return ExecuteDecision(ActionPlan{Action: ActionRepayDebt, StrategyKey: active, Amount: amount})
			}
			if collateral := observed.CollateralCustody(active); collateral != nil && collateral.AmountRaw > 0 {
				return ExecuteDecision(ActionPlan{
					Action: ActionSwapCollateralToDebt, StrategyKey: active,
					Amount: AmountExact(collateral.AmountRaw),
				})
			}
			return ExecuteDecision(ActionPlan{
				Action: ActionWithdrawCollateral, StrategyKey: active, Amount: AmountMaxSafe,
			})
		}
		if position.CollateralDepositedRaw > 0 {
			return ExecuteDecision(ActionPlan{
				Action: ActionWithdrawRemainingCollateral, StrategyKey: active, Amount: AmountAll,
			})
		}
	}
	for _, entry := range observed.CollateralCustodies {
		if entry.Balance.AmountRaw > 0 {
			return ExecuteDecision(ActionPlan{
				Action: ActionSwapCollateralToClaim, StrategyKey: entry.StrategyKey,
				Amount: AmountExact(entry.Balance.AmountRaw),
			})
		}
	}
	// Intentional correction to Rust plan_down: an empty obligation does not
	// make a previously borrowed non-USDC custody balance disappear. Reuse
	// the scoped debt-to-collateral recipe, then the existing collateral-to-
	// claim branch on the next fresh tick. USDC debt aliases claim custody.
	for _, entry := range observed.DebtCustodies {
		if entry.Balance.Account == observed.Claim.Account || entry.Balance.AmountRaw == 0 {
			continue
		}
		if entry.Balance.AmountRaw <= minJupiterSwapRaw {
			return PlannerDecision{Kind: "unresolved_custody", Condition: "withdrawal_residual_debt_dust"}
		}
		return ExecuteDecision(ActionPlan{Action: ActionSwapDebtToCollateral, StrategyKey: entry.StrategyKey, Amount: AmountExact(entry.Balance.AmountRaw)})
	}
	// Claim is a root-signed user transaction prepared by the app. The
	// delegate stops once the requested amount is liquid in claim custody.
	return CompleteDecision()
}
