package earnmax

import (
	"context"
	"errors"
	"strconv"
)

type WithdrawalCostStatus struct {
	ApprovedCostUSDC  string  `json:"approvedCostUsdcRaw"`
	SettledCostUSDC   *string `json:"settledCostUsdcRaw"`
	ReservedCostUSDC  *string `json:"reservedCostUsdcRaw"`
	RemainingCostUSDC *string `json:"remainingCostUsdcRaw"`
	ApprovedClaimFee  string  `json:"approvedClaimFeeLamports"`
	SpentClaimFee     *string `json:"spentClaimFeeLamports"`
	ReservedClaimFee  *string `json:"reservedClaimFeeLamports"`
	RemainingClaimFee *string `json:"remainingClaimFeeLamports"`
	ClaimPending      *bool   `json:"claimPending"`
	UnknownReason     string  `json:"unknownReason,omitempty"`
}

type WithdrawalCostStatusReader struct{ db Querier }

func NewWithdrawalCostStatusReader(db Querier) *WithdrawalCostStatusReader {
	return &WithdrawalCostStatusReader{db: db}
}

// Read projects the worker-owned journals across every approval version in the
// same request generation. It never releases a reservation or writes a balance.
func (r *WithdrawalCostStatusReader) Read(ctx context.Context, c WithdrawalConsent) (*WithdrawalCostStatus, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("withdrawal cost status unavailable")
	}
	reference, err := canonicalUint(c.ReferenceUSDC)
	if err != nil {
		return nil, err
	}
	floor, err := canonicalUint(c.MinClaimUSDC)
	if err != nil || floor > reference {
		return nil, errors.New("invalid approved withdrawal cost basis")
	}
	feeBudget, err := canonicalUint(c.MaxUserFeeLamports)
	if err != nil {
		return nil, err
	}
	var spent, reserved, feeSpent, feeReserved string
	var unknown, missingActual, missingFee, pending bool
	err = r.db.QueryRow(ctx, `WITH costs AS (
 SELECT COALESCE(sum(r.actual_incremental_cost_usdc) FILTER(WHERE o.state='settled'),0)::text AS spent,
 COALESCE(sum(r.incremental_cost_usdc) FILTER(WHERE o.state IN ('reserved','unknown')),0)::text AS reserved,
 COALESCE(bool_or(o.state='unknown'),false) AS unknown,
 COALESCE(bool_or(o.state='settled' AND r.actual_incremental_cost_usdc IS NULL),false) AS missing_actual
 FROM loyal_yield.backyard_withdrawal_cost_reservations r JOIN loyal_yield.backyard_withdrawal_cost_operations o USING(operation_id)
 WHERE r.authority=$1 AND r.request_signature=$2 AND r.request_instruction=$3
), fees AS (
 SELECT COALESCE(sum(actual_fee_lamports) FILTER(WHERE state='settled'),0)::text AS spent,
 COALESCE(sum(reserved_fee_lamports) FILTER(WHERE state='reserved'),0)::text AS reserved,
 COALESCE(bool_or(state='settled' AND actual_fee_lamports IS NULL),false) AS missing_actual,
 COALESCE(bool_or(state='reserved'),false) AS pending
 FROM loyal_yield.earn_max_withdrawal_claim_attempts WHERE authority=$1 AND request_signature=$2 AND request_instruction=$3
) SELECT costs.spent,costs.reserved,costs.unknown,costs.missing_actual,fees.spent,fees.reserved,fees.missing_actual,fees.pending FROM costs CROSS JOIN fees`, c.Authority, c.RequestSignature, c.RequestInstruction).Scan(&spent, &reserved, &unknown, &missingActual, &feeSpent, &feeReserved, &missingFee, &pending)
	if err != nil {
		return nil, err
	}
	for _, raw := range []string{spent, reserved, feeSpent, feeReserved} {
		if _, err := canonicalUint(raw); err != nil {
			return nil, err
		}
	}
	budget := strconv.FormatUint(reference-floor, 10)
	status := &WithdrawalCostStatus{ApprovedCostUSDC: budget, SettledCostUSDC: &spent, ReservedCostUSDC: &reserved, ApprovedClaimFee: c.MaxUserFeeLamports, SpentClaimFee: &feeSpent, ReservedClaimFee: &feeReserved, ClaimPending: &pending}
	if missingActual {
		status.SettledCostUSDC = nil
		status.UnknownReason = "cost_settlement_unavailable"
	} else if unknown {
		status.UnknownReason = "awaiting_cost_finalization"
	} else {
		remaining := remainingWithdrawalBudget(reference-floor, spent, reserved)
		status.RemainingCostUSDC = &remaining
	}
	if missingFee {
		status.SpentClaimFee = nil
		status.UnknownReason = "claim_fee_unavailable"
	} else {
		remaining := remainingWithdrawalBudget(feeBudget, feeSpent, feeReserved)
		status.RemainingClaimFee = &remaining
	}
	return status, nil
}

func remainingWithdrawalBudget(approved uint64, spent, reserved string) string {
	for _, raw := range []string{spent, reserved} {
		n, _ := canonicalUint(raw)
		if n > approved {
			return "0"
		}
		approved -= n
	}
	return strconv.FormatUint(approved, 10)
}
