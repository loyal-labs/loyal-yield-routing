package backyardrwa

import (
	"math"
	"math/big"
)

const budgetMaxObservationLagSlots int64 = 32

// BudgetPrice is an observation, not an assumed stablecoin peg. The producer
// must bind the mint/program/decimals and reserve/oracle account hashes to its
// coherent chain read. Upper and lower prices share the oracle quote unit.
type BudgetPrice struct {
	Source           string   `json:"source"`
	Mint             string   `json:"mint"`
	TokenProgram     string   `json:"tokenProgram"`
	Decimals         uint8    `json:"decimals"`
	TokenUpperSF     [16]byte `json:"tokenUpperSf"`
	USDCLowerSF      [16]byte `json:"usdcLowerSf"`
	ObservedSlot     int64    `json:"observedSlot"`
	ValidThroughSlot int64    `json:"validThroughSlot"`
	EvidenceSHA256   string   `json:"evidenceSha256"`
}

func (p BudgetPrice) valueUpper(raw uint64, mint, program string, slot int64) (int64, error) {
	if p.Mint != mint || p.TokenProgram != program || p.Decimals > 18 || p.ObservedSlot <= 0 || slot < p.ObservedSlot || slot > p.ValidThroughSlot || p.ValidThroughSlot < p.ObservedSlot || p.ValidThroughSlot-p.ObservedSlot > budgetMaxObservationLagSlots || !sha256Pattern.MatchString(p.EvidenceSHA256) {
		return 0, budgetHold("missing_stale_or_mismatched_usdc_valuation")
	}
	value, err := valueBetweenTokenRaw(raw, p.Decimals, 6, p.TokenUpperSF, p.USDCLowerSF, true)
	if err != nil || value > math.MaxInt64 {
		return 0, budgetHold("invalid_usdc_valuation")
	}
	return int64(value), nil
}

const nativeSOLBudgetAsset = "native:SOL"

type ValuedTransactionCost struct {
	MessageSHA256       string `json:"messageSha256"`
	ObservationSlot     int64  `json:"observationSlot"`
	PrincipalMicros     int64  `json:"principalMicros"`
	NetworkFeeMicros    int64  `json:"networkFeeMicros"`
	SetupLamportsMicros int64  `json:"setupLamportsMicros"`
	TotalMicros         int64  `json:"totalMicros"`
}

// ValueTransactionCost counts source principal once and fees once. Mint rent
// or account setup paid in lamports is distinct from network fees; callers
// must supply its independently bounded amount, including zero only when no
// account-creation/rent debit is possible. No protocol fee may be omitted:
// fees taken from the token source belong in the measured executable debit.
func ValueTransactionCost(message []byte, debit ExecutableDebit, fee MessageFeeObservation, setupLamports uint64, tokenPrice, solPrice BudgetPrice, slot int64) (ValuedTransactionCost, error) {
	result := ValuedTransactionCost{MessageSHA256: sha256Bytes(message), ObservationSlot: slot}
	if _, err := checkedUnsignedMessage(message); err != nil {
		return result, err
	}
	if fee.MessageSHA256 != result.MessageSHA256 || fee.Slot <= 0 || fee.Slot > slot || slot-fee.Slot > budgetMaxObservationLagSlots || fee.Lamports == 0 {
		return result, budgetHold("fee_message_or_slot_mismatch")
	}
	var err error
	if debit.Raw > 0 {
		if debit.Source == "" {
			return result, budgetHold("economic_source_missing")
		}
		result.PrincipalMicros, err = tokenPrice.valueUpper(debit.Raw, debit.Mint, debit.TokenProgram, slot)
		if err != nil {
			return result, err
		}
	}
	if solPrice.Decimals != 9 {
		return result, budgetHold("native_sol_decimal_mismatch")
	}
	result.NetworkFeeMicros, err = solPrice.valueUpper(fee.Lamports, nativeSOLBudgetAsset, "11111111111111111111111111111111", slot)
	if err != nil {
		return result, err
	}
	if setupLamports > 0 {
		result.SetupLamportsMicros, err = solPrice.valueUpper(setupLamports, nativeSOLBudgetAsset, "11111111111111111111111111111111", slot)
		if err != nil {
			return result, err
		}
	}
	result.TotalMicros, err = budgetSum(result.PrincipalMicros, result.NetworkFeeMicros, result.SetupLamportsMicros)
	return result, err
}

// UpperPriceMargin applies a caller-reviewed conservative basis-point margin
// to a positive observed price without float precision loss or u128 wrapping.
func UpperPriceMargin(price [16]byte, marginBPS uint16) ([16]byte, error) {
	var out [16]byte
	value := littleInt(price[:])
	if value.Sign() <= 0 {
		return out, budgetHold("invalid_usdc_valuation")
	}
	value.Mul(value, new(big.Int).SetUint64(10_000+uint64(marginBPS)))
	value.Add(value, big.NewInt(9_999))
	value.Div(value, big.NewInt(10_000))
	if value.BitLen() > 128 {
		return out, budgetHold("invalid_usdc_valuation")
	}
	bytes := value.Bytes()
	for i := range bytes {
		out[i] = bytes[len(bytes)-1-i]
	}
	return out, nil
}

func lowerPriceMargin(price [16]byte, marginBPS uint16) ([16]byte, error) {
	var out [16]byte
	if marginBPS >= 10_000 {
		return out, budgetHold("invalid_usdc_valuation")
	}
	value := littleInt(price[:])
	value.Mul(value, new(big.Int).SetUint64(10_000-uint64(marginBPS)))
	value.Div(value, big.NewInt(10_000))
	if value.Sign() <= 0 {
		return out, budgetHold("invalid_usdc_valuation")
	}
	bytes := value.Bytes()
	for i := range bytes {
		out[i] = bytes[len(bytes)-1-i]
	}
	return out, nil
}
