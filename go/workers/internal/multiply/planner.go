package multiply

import (
	"math/big"
)

const (
	targetLTVToleranceBPS = 10
	minJupiterSwapRaw     = uint64(50_000)
)

type PlannedAmount interface{ plannedAmount() }

type ExactAmount uint64
type allAmount struct{}
type maxSafeAmount struct{}
type toTargetLTVAmount struct{}

func (ExactAmount) plannedAmount()       {}
func (allAmount) plannedAmount()         {}
func (maxSafeAmount) plannedAmount()     {}
func (toTargetLTVAmount) plannedAmount() {}

var (
	AmountAll         PlannedAmount = allAmount{}
	AmountMaxSafe     PlannedAmount = maxSafeAmount{}
	AmountToTargetLTV PlannedAmount = toTargetLTVAmount{}
)

func AmountExact(value uint64) PlannedAmount { return ExactAmount(value) }

type ActionPlan struct {
	Action             MultiplyAction
	StrategyKey        StrategyKey
	Amount             PlannedAmount
	DestinationAccount string
}

type DecisionKind string

const (
	DecisionExecute            DecisionKind = "execute"
	DecisionResume             DecisionKind = "resume"
	DecisionComplete           DecisionKind = "complete"
	DecisionInvalidObservation DecisionKind = "invalid_observation"
	DecisionUnresolvedCustody  DecisionKind = "unresolved_custody"
)

type PlannerDecision struct {
	Kind      DecisionKind
	Plan      *ActionPlan
	ResumeID  string
	Condition string
}

func ExecuteDecision(plan ActionPlan) PlannerDecision {
	return PlannerDecision{Kind: DecisionExecute, Plan: &plan}
}

func CompleteDecision() PlannerDecision { return PlannerDecision{Kind: DecisionComplete} }

func NextAction(route *RouteState, observed *ObservedRoute, topology *EarnMaxTopology) PlannerDecision {
	if route == nil {
		return PlannerDecision{Kind: DecisionInvalidObservation}
	}
	if route.CurrentOperationID != nil {
		return PlannerDecision{Kind: DecisionResume, ResumeID: *route.CurrentOperationID}
	}
	if observed == nil || topology == nil {
		return PlannerDecision{Kind: DecisionInvalidObservation}
	}
	if !observed.ActiveStrategyIsCoherent() {
		return PlannerDecision{Kind: DecisionInvalidObservation}
	}
	for _, config := range topology.StrategyCatalog() {
		if observed.Position(config.Key) == nil || observed.CollateralCustody(config.Key) == nil || observed.DebtCustody(config.Key) == nil {
			return PlannerDecision{Kind: DecisionInvalidObservation}
		}
	}
	active := NoStrategy
	for _, position := range observed.Strategies {
		if position == nil {
			return PlannerDecision{Kind: DecisionInvalidObservation}
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
		return PlannerDecision{Kind: DecisionInvalidObservation}
	}
	collateralCustody := observed.CollateralCustody(target)
	if collateralCustody == nil || observed.DebtCustody(target) == nil {
		return PlannerDecision{Kind: DecisionInvalidObservation}
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
		return PlannerDecision{Kind: DecisionInvalidObservation}
	}
	collateralValue := new(big.Int).Set(position.CollateralValueSF.Int())
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
			return PlannerDecision{Kind: DecisionInvalidObservation}
		}
		debtCustody := observed.DebtCustody(active)
		if debtCustody == nil || observed.CollateralCustody(active) == nil {
			return PlannerDecision{Kind: DecisionInvalidObservation}
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
	for _, entry := range observed.DebtCustodies {
		if entry.Balance.Account == observed.Claim.Account || entry.Balance.AmountRaw == 0 {
			continue
		}
		if entry.Balance.AmountRaw <= minJupiterSwapRaw {
			return PlannerDecision{Kind: DecisionUnresolvedCustody, Condition: "withdrawal_residual_debt_dust"}
		}
		return ExecuteDecision(ActionPlan{Action: ActionSwapDebtToCollateral, StrategyKey: entry.StrategyKey, Amount: AmountExact(entry.Balance.AmountRaw)})
	}
	return CompleteDecision()
}
