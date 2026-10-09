package multiply

// Policy contracts ported from
// 91694cd9^:crates/loyal-fleet-worker/src/multiply/policy.rs: canonical KLend v2
// discriminators and per-family constraint indexes.

import (
	"encoding/binary"
	"errors"

	"github.com/solana-foundation/solana-go/v2"
)

// PolicyFamily mirrors policy::PolicyFamily.
type PolicyFamily string

const (
	FamilyCollateral PolicyFamily = "CollateralLifecycle"
	FamilyDebt       PolicyFamily = "DebtLifecycle"
	FamilySwap       PolicyFamily = "SwapRoutes"
)

// FamilyForAction mirrors family_for_action.
func FamilyForAction(action MultiplyAction) (PolicyFamily, error) {
	switch action {
	case ActionDepositCollateral, ActionWithdrawCollateral, ActionWithdrawRemainingCollateral:
		return FamilyCollateral, nil
	case ActionBorrowDebt, ActionRepayDebt:
		return FamilyDebt, nil
	case ActionSwapClaimToCollateral, ActionSwapDebtToCollateral,
		ActionSwapCollateralToDebt, ActionSwapCollateralToClaim:
		return FamilySwap, nil
	}
	return "", errors.New("action has no strategy policy family")
}

// ConstraintIndexes mirrors constraint_indexes: the terminal instruction must
// be the single policy-wrapped instruction, and its single constraint index is
// derived from the action and route shape.
func ConstraintIndexes(config StrategyConfig, action MultiplyAction, instructions []Instruction) ([]byte, error) {
	if len(instructions) != 1 {
		return nil, errors.New("policy execution must contain exactly one terminal instruction")
	}
	var index uint8
	switch action {
	case ActionDepositCollateral:
		index = 0
	case ActionWithdrawCollateral, ActionWithdrawRemainingCollateral:
		index = 1
	case ActionBorrowDebt:
		index = 0
	case ActionRepayDebt:
		index = 1
	case ActionSwapClaimToCollateral:
		return routeConstraintIndex(instructions[0], claimToCollateralIndex(config.Key))
	case ActionSwapDebtToCollateral:
		return routeConstraintIndex(instructions[0], debtToCollateralIndex(config.Key))
	case ActionSwapCollateralToDebt:
		return routeConstraintIndex(instructions[0], collateralToDebtIndex(config.Key))
	case ActionSwapCollateralToClaim:
		return routeConstraintIndex(instructions[0], collateralToClaimIndex(config.Key))
	default:
		return nil, errors.New("action does not use a strategy policy")
	}
	return []byte{index}, nil
}

func routeConstraintIndex(instruction Instruction, index uint8) ([]byte, error) {
	if len(instruction.Data) < 13 || !equalBytes(instruction.Data[:8], JupiterSharedAccountsRouteDiscriminator[:]) {
		return nil, errors.New("Jupiter action is not SharedAccountsRoute")
	}
	routeCount := binary.LittleEndian.Uint32(instruction.Data[9:13])
	if routeCount < 1 || routeCount > 4 {
		return nil, errors.New("Jupiter route must contain one to four legs")
	}
	return []byte{index}, nil
}

func claimToCollateralIndex(key StrategyKey) uint8 {
	switch key {
	case OnycUsdc, OnycUsds, PrimeUsdc, PrimePyusd, PrimeUsds:
		return 0
	default:
		return 1
	}
}

func debtToCollateralIndex(key StrategyKey) uint8 {
	switch key {
	case PrimePyusd, SyrupUsdcUsdc, SyrupUsdcPyusd:
		return 1
	default:
		return 0
	}
}

func collateralToDebtIndex(key StrategyKey) uint8 {
	switch key {
	case PrimePyusd, SyrupUsdcUsdc, SyrupUsdcPyusd:
		return 3
	default:
		return 2
	}
}

func collateralToClaimIndex(key StrategyKey) uint8 {
	switch key {
	case SyrupUsdcUsdc, SyrupUsdcPyusd:
		return 3
	default:
		return 2
	}
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// AccountMeta mirrors solana_sdk::Instruction's account entry.
type AccountMeta struct {
	PubKey     solana.PublicKey
	IsSigner   bool
	IsWritable bool
}

// Instruction is the multiply package's instruction value.
type Instruction struct {
	ProgramID solana.PublicKey
	Accounts  []AccountMeta
	Data      []byte
}
