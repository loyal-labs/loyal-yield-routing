package autodeposit

// SweepCaps bounds one sweep decision: how much is schedulable, what the wallet
// actually holds above its floor, and the per-period limits that may shave it.
type SweepCaps struct {
	EligibleLotAmountRaw  int64
	WalletBalanceRaw      int64
	WalletBalanceFloorRaw int64
	MaxAmountPerPeriodRaw *int64
	RemainingAllowanceRaw *int64
}

// SweepDecisionKind names why a sweep did or did not happen.
type SweepDecisionKind string

const (
	SweepNoEligibleLots   SweepDecisionKind = "no_eligible_lots"
	SweepNoWalletExcess   SweepDecisionKind = "no_wallet_excess"
	SweepAllowanceExhaust SweepDecisionKind = "allowance_exhausted"
	SweepGo               SweepDecisionKind = "sweep"
)

// SweepAmountDecision is the sweep outcome for one target at one instant.
// Amounts are zero unless Kind is SweepGo.
type SweepAmountDecision struct {
	Kind                       SweepDecisionKind
	AmountRaw                  int64
	EligibleLotAmountRaw       int64
	ExcessRaw                  int64
	CappedByMaxAmountPerPeriod bool
	CappedByWalletFloor        bool
	CappedByRemainingAllowance bool
}

// SelectedLot is the per-lot split of a sweep amount.
type SelectedLot struct {
	LotID     int64
	AmountRaw int64
}

// ComputeSweepAmount resolves the sweep amount from caps without picking lots.
//
// Order matters: the per-period cap is applied first, then the wallet floor
// (never sweep money the user keeps), then the delegated allowance. A zero or
// negative allowance is a hard stop, not zero-sized sweep.
func ComputeSweepAmount(caps SweepCaps) SweepAmountDecision {
	if caps.EligibleLotAmountRaw <= 0 {
		return SweepAmountDecision{Kind: SweepNoEligibleLots}
	}
	excessRaw, ok := subChecked(caps.WalletBalanceRaw, caps.WalletBalanceFloorRaw)
	if !ok {
		return SweepAmountDecision{Kind: SweepNoWalletExcess}
	}
	if excessRaw <= 0 {
		return SweepAmountDecision{Kind: SweepNoWalletExcess, ExcessRaw: excessRaw}
	}
	if caps.RemainingAllowanceRaw != nil && *caps.RemainingAllowanceRaw <= 0 {
		return SweepAmountDecision{Kind: SweepAllowanceExhaust, ExcessRaw: excessRaw}
	}

	amountRaw := caps.EligibleLotAmountRaw
	decision := SweepAmountDecision{
		Kind:                 SweepGo,
		EligibleLotAmountRaw: caps.EligibleLotAmountRaw,
		ExcessRaw:            excessRaw,
	}
	if caps.MaxAmountPerPeriodRaw != nil {
		if maxAmount := *caps.MaxAmountPerPeriodRaw; maxAmount > 0 && amountRaw > maxAmount {
			amountRaw = maxAmount
			decision.CappedByMaxAmountPerPeriod = true
		}
	}
	if amountRaw > excessRaw {
		amountRaw = excessRaw
		decision.CappedByWalletFloor = true
	}
	if caps.RemainingAllowanceRaw != nil {
		if allowance := *caps.RemainingAllowanceRaw; amountRaw > allowance {
			amountRaw = allowance
			decision.CappedByRemainingAllowance = true
		}
	}
	decision.AmountRaw = amountRaw
	return decision
}
