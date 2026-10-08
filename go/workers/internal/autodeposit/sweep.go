package autodeposit

import "time"

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

// LotSelection is a decided sweep: total plus the oldest-first lot split that
// funds it.
type LotSelection struct {
	AmountRaw int64
	Lots      []SelectedLot
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

// SelectEligibleLots resolves the sweep and splits it across eligible lots,
// oldest eligible first. Lots still inside the one-hour delay are never
// selected, even if the wallet holds enough excess to fund them.
func SelectEligibleLots(lots []SurplusLot, now time.Time, walletBalanceRaw, walletBalanceFloorRaw int64, remainingAllowanceRaw *int64) (SweepAmountDecision, *LotSelection, error) {
	eligible := make([]SurplusLot, 0, len(lots))
	for _, lot := range lots {
		if lot.Status == LotOpen && lot.RemainingAmountRaw > 0 && !lot.EligibleAfter.After(now) {
			eligible = append(eligible, lot)
		}
	}
	sortLots(eligible, func(a, b SurplusLot) bool {
		if !a.EligibleAfter.Equal(b.EligibleAfter) {
			return a.EligibleAfter.Before(b.EligibleAfter)
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	var eligibleLotAmountRaw int64
	for _, lot := range eligible {
		if lot.RemainingAmountRaw < 0 {
			return SweepAmountDecision{}, nil, &LotError{Code: LotErrInvalidLotRemaining, LotID: lot.ID, RemainingAmountRaw: lot.RemainingAmountRaw}
		}
		total, ok := addChecked(eligibleLotAmountRaw, lot.RemainingAmountRaw)
		if !ok {
			return SweepAmountDecision{}, nil, &LotError{Code: LotErrAmountOverflow}
		}
		eligibleLotAmountRaw = total
	}

	decision := ComputeSweepAmount(SweepCaps{
		EligibleLotAmountRaw:  eligibleLotAmountRaw,
		WalletBalanceRaw:      walletBalanceRaw,
		WalletBalanceFloorRaw: walletBalanceFloorRaw,
		RemainingAllowanceRaw: remainingAllowanceRaw,
	})
	if decision.Kind != SweepGo {
		return decision, nil, nil
	}

	remaining := decision.AmountRaw
	selection := &LotSelection{AmountRaw: decision.AmountRaw}
	for _, lot := range eligible {
		if remaining == 0 {
			break
		}
		amount := min64(remaining, lot.RemainingAmountRaw)
		selection.Lots = append(selection.Lots, SelectedLot{LotID: lot.ID, AmountRaw: amount})
		left, ok := subChecked(remaining, amount)
		if !ok {
			return SweepAmountDecision{}, nil, &LotError{Code: LotErrAmountOverflow}
		}
		remaining = left
	}
	return decision, selection, nil
}

// ApplyAutodepositConsumption commits a decided sweep against the working lot
// set. A fully consumed lot becomes Consumed, distinct from Depleted: a sweep
// spent the money on purpose, an external outflow only observed it gone.
func ApplyAutodepositConsumption(lots []SurplusLot, selection *LotSelection) (int64, error) {
	if selection == nil {
		return 0, nil
	}
	var consumedTotal int64
	for _, selected := range selection.Lots {
		found := -1
		for i := range lots {
			if lots[i].ID == selected.LotID {
				found = i
				break
			}
		}
		if found < 0 {
			continue
		}
		lot := &lots[found]
		if lot.RemainingAmountRaw < selected.AmountRaw {
			return 0, &LotError{Code: LotErrInvalidLotRemaining, LotID: lot.ID, RemainingAmountRaw: lot.RemainingAmountRaw}
		}
		next, ok := subChecked(lot.RemainingAmountRaw, selected.AmountRaw)
		if !ok {
			return 0, &LotError{Code: LotErrAmountOverflow}
		}
		lot.RemainingAmountRaw = next
		total, ok := addChecked(consumedTotal, selected.AmountRaw)
		if !ok {
			return 0, &LotError{Code: LotErrAmountOverflow}
		}
		consumedTotal = total
		if lot.RemainingAmountRaw == 0 {
			lot.Status = LotConsumed
		} else {
			lot.Status = LotOpen
		}
	}
	return consumedTotal, nil
}
