package backyard

import (
	"encoding/json"
	"math"
)

const (
	topupTrancheAllocated  = "allocated"
	topupTrancheCollateral = "collateral"
	topupTrancheComplete   = "complete"
	topupTrancheHandoff    = "handoff"
	// Finalized ordinary-operation collateral, not exclusive top-up inventory.
	topupTrancheOrdinary = "ordinary"
)

// A tranche owns only capital received from its exact Voltr allocation. The
// journal's finalized receipt, never a quote or projection, advances inventory.
// This record is copied into each next operation's phase3 authorization so a
// restart cannot adopt cash from a different allocation or a borrowed-debt swap.
type topupTranche struct {
	Generation             int64                 `json:"generation"`
	Loan                   topupLoan             `json:"loan"`
	Handoff                topupHandoffAuthority `json:"handoff"`
	DebtRemainingRaw       uint64                `json:"debtRemainingRaw"`
	StrategyRemainingRaw   uint64                `json:"strategyRemainingRaw"`
	Lane                   string                `json:"lane"`
	OriginOperationID      string                `json:"originOperationId"`
	AllocatedUSDCRaw       uint64                `json:"allocatedUsdcRaw"`
	USDCRemainingRaw       uint64                `json:"usdcRemainingRaw"`
	CollateralRemainingRaw uint64                `json:"collateralRemainingRaw"`
	// Authenticated finite rounding window from the admitted deposit effects.
	// A completed tranche can retain less than this quantum, but still owns it.
	DepositQuantumRaw uint64 `json:"depositQuantumRaw,omitempty"`
	Stage             string `json:"stage"`
	LastOperationID   string `json:"lastOperationId"`
	LastSlot          int64  `json:"lastSlot"`
	LastEffectsSHA256 string `json:"lastEffectsSha256"`
}

// Before is the exact durable record at admission, including the last receipt.
// A new allocation has no predecessor (or a completed predecessor) and names
// itself as the new origin. Caller-supplied reasons do not establish ownership.
type topupTrancheBinding struct {
	Loan              topupLoan             `json:"loan"`
	Lane              string                `json:"lane"`
	OriginOperationID string                `json:"originOperationId"`
	AllocatedUSDCRaw  uint64                `json:"allocatedUsdcRaw"`
	Before            *topupTranche         `json:"before,omitempty"`
	Handoff           topupHandoffAuthority `json:"handoff"`
}

func (t topupTranche) validate() error {
	if t.Generation <= 0 || !t.Loan.valid() || t.Lane != autoAUTOPYUSD.Lane || !sha256Pattern.MatchString(t.OriginOperationID) ||
		!sha256Pattern.MatchString(t.LastOperationID) || !sha256Pattern.MatchString(t.LastEffectsSHA256) ||
		t.LastSlot <= 0 || t.AllocatedUSDCRaw == 0 || t.AllocatedUSDCRaw > math.MaxInt64 ||
		t.USDCRemainingRaw > math.MaxInt64 || t.CollateralRemainingRaw > math.MaxInt64 || t.DepositQuantumRaw > math.MaxInt64 ||
		t.DebtRemainingRaw > math.MaxInt64 || t.StrategyRemainingRaw > math.MaxInt64 ||
		(t.Handoff != (topupHandoffAuthority{}) && !t.Handoff.valid()) ||
		(t.Stage != topupTrancheHandoff && (t.DebtRemainingRaw != 0 || t.StrategyRemainingRaw != 0)) {
		return budgetHold("invalid_topup_tranche")
	}
	switch t.Stage {
	case topupTrancheAllocated:
		if t.USDCRemainingRaw != t.AllocatedUSDCRaw || (t.CollateralRemainingRaw > 0 && t.CollateralRemainingRaw >= t.DepositQuantumRaw) || t.LastOperationID != t.OriginOperationID {
			return budgetHold("invalid_topup_tranche")
		}
	case topupTrancheCollateral:
		if t.USDCRemainingRaw != 0 || t.CollateralRemainingRaw == 0 || t.LastOperationID == t.OriginOperationID {
			return budgetHold("invalid_topup_tranche")
		}
	case topupTrancheHandoff:
		if !t.Handoff.valid() || (t.USDCRemainingRaw == 0 && t.CollateralRemainingRaw == 0 && t.DebtRemainingRaw == 0 && t.StrategyRemainingRaw == 0) {
			return budgetHold("invalid_topup_handoff")
		}
	case topupTrancheOrdinary:
		if t.USDCRemainingRaw != 0 || t.CollateralRemainingRaw == 0 || t.LastOperationID == t.OriginOperationID {
			return budgetHold("invalid_topup_tranche")
		}
	case topupTrancheComplete:
		if t.USDCRemainingRaw != 0 || t.DepositQuantumRaw == 0 || t.CollateralRemainingRaw >= t.DepositQuantumRaw || t.LastOperationID == t.OriginOperationID {
			return budgetHold("invalid_topup_tranche")
		}
	default:
		return budgetHold("invalid_topup_tranche")
	}
	return nil
}

func decodeTopupTranche(raw []byte) (*topupTranche, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var t topupTranche
	if json.Unmarshal(raw, &t) != nil || t.validate() != nil {
		return nil, budgetHold("invalid_topup_tranche")
	}
	return &t, nil
}

func (b topupTrancheBinding) matches(before *topupTranche) bool {
	return (before == nil && b.Before == nil) || (before != nil && b.Before != nil && *before == *b.Before)
}

// Called only inside finalized reconciliation, under the existing route lock.
// The production reconciler is repeated here so this financial transition can
// never consume expected/minimum balances as if they were actual receipts.
func reconcileTopupTranche(before *topupTranche, b topupTrancheBinding, operationID string, action Action, expected ExpectedEffects, receipt ConfirmedTransactionEvidence) (*topupTranche, error) {
	if !b.Loan.valid() || b.Lane != autoAUTOPYUSD.Lane || receipt.Slot < b.Loan.ObservedSlot || !sha256Pattern.MatchString(operationID) ||
		!sha256Pattern.MatchString(b.OriginOperationID) || b.AllocatedUSDCRaw == 0 || b.AllocatedUSDCRaw > math.MaxInt64 ||
		!b.matches(before) || !receipt.Finalized || (before != nil && (before.validate() != nil || receipt.Slot < before.LastSlot || operationID == before.LastOperationID)) {
		return nil, budgetHold("topup_tranche_binding_changed")
	}
	route := autoAUTOPYUSD
	if expected.Schema != "loyal-backyard-rwa-expected-effects/v1" {
		return nil, budgetHold("topup_receipt_namespace_changed")
	}
	for _, effect := range expected.Accounts {
		if !topupReceiptIdentity(effect) {
			return nil, budgetHold("topup_receipt_namespace_changed")
		}
	}
	reconciled, _, err := ReconcileConfirmedTransaction(expected, receipt)
	if err != nil {
		return nil, err
	}
	pre, err := transactionBalancesByAddress(receipt.PreTokenBalances)
	if err != nil {
		return nil, err
	}
	post, err := transactionBalancesByAddress(receipt.PostTokenBalances)
	if err != nil {
		return nil, err
	}
	debtBefore, hasDebtBefore := pre[route.DebtCustody]
	debtAfter, hasDebtAfter := post[route.DebtCustody]
	if hasDebtBefore != hasDebtAfter || (hasDebtBefore && (debtBefore.OwnerProgram != route.DebtTokenProgram || debtBefore.Mint != route.Kamino.DebtMint ||
		debtBefore.Authority != bridgeVault || debtBefore.Raw != 0 || debtAfter != debtBefore)) {
		return nil, budgetHold("topup_receipt_debt_custody_changed")
	}
	t := topupTranche{Generation: 1, Loan: b.Loan, Lane: b.Lane, OriginOperationID: b.OriginOperationID, AllocatedUSDCRaw: b.AllocatedUSDCRaw,
		LastOperationID: operationID, LastSlot: receipt.Slot, LastEffectsSHA256: reconciled.EffectsSHA256}
	if before != nil {
		t.Generation = before.Generation + 1
	}
	switch action {
	case VoltrAllocateToSquads:
		if (before != nil && before.Stage != topupTrancheComplete) || b.OriginOperationID != operationID ||
			expected.Kind != "bridge" || !expected.Conserved || expected.ReturnData == nil || expected.ReturnData.ProgramID != bridgeAdaptorProgram ||
			len(expected.Accounts) != 3 || expected.Repayment != nil || expected.Deposit != nil ||
			expected.Accounts[0].Address != bridgeIdleATA || expected.Accounts[1].Address != bridgeStrategyATA || expected.Accounts[2].Address != bridgeSquadsATA ||
			pre[bridgeSquadsATA].Raw != 0 || post[bridgeSquadsATA].Raw != b.AllocatedUSDCRaw ||
			pre[bridgeIdleATA].Raw < post[bridgeIdleATA].Raw || pre[bridgeIdleATA].Raw-post[bridgeIdleATA].Raw != b.AllocatedUSDCRaw ||
			pre[bridgeStrategyATA].Raw != 0 || post[bridgeStrategyATA].Raw != 0 {
			return nil, budgetHold("topup_allocation_receipt_mismatch")
		}
		t.Stage, t.USDCRemainingRaw = topupTrancheAllocated, b.AllocatedUSDCRaw
		if before != nil {
			t.CollateralRemainingRaw, t.DepositQuantumRaw = before.CollateralRemainingRaw, before.DepositQuantumRaw
		}
	case SwapStableToCollateralStep:
		if before == nil || before.Stage != topupTrancheAllocated || before.Loan != b.Loan || before.Lane != b.Lane || before.OriginOperationID != b.OriginOperationID || before.AllocatedUSDCRaw != b.AllocatedUSDCRaw ||
			expected.Kind != "cross-mint-swap" || len(expected.Accounts) != 2 || expected.Repayment != nil || expected.Deposit != nil ||
			expected.Accounts[0].Address != bridgeSquadsATA || expected.Accounts[1].Address != route.CollateralCustody ||
			pre[bridgeSquadsATA].Raw != before.USDCRemainingRaw || post[bridgeSquadsATA].Raw != 0 || pre[route.CollateralCustody].Raw != before.CollateralRemainingRaw || post[route.CollateralCustody].Raw <= before.CollateralRemainingRaw {
			return nil, budgetHold("topup_swap_receipt_mismatch")
		}
		t.Stage, t.CollateralRemainingRaw = topupTrancheCollateral, post[route.CollateralCustody].Raw
	case OpenRouteStep:
		if before == nil || before.Stage != topupTrancheCollateral || before.Loan != b.Loan || before.Lane != b.Lane || before.OriginOperationID != b.OriginOperationID || before.AllocatedUSDCRaw != b.AllocatedUSDCRaw ||
			expected.Kind != "kamino-deposit" || !expected.Conserved || len(expected.Accounts) != 2 || expected.Deposit == nil || expected.Repayment != nil ||
			expected.Accounts[0].Address != route.CollateralCustody || expected.Accounts[1].Address != route.CollateralLiquiditySupply ||
			expected.Deposit.MaximumDebitRaw != before.CollateralRemainingRaw ||
			pre[route.CollateralCustody].Raw != before.CollateralRemainingRaw || post[route.CollateralCustody].Raw >= before.CollateralRemainingRaw {
			return nil, budgetHold("topup_deposit_receipt_mismatch")
		}
		// Maximum-minus-minimum is the reserve/Clock-validated rounding loss
		// bound. Keep the actual remainder; never write it off or adopt more.
		t.DepositQuantumRaw = expected.Deposit.MaximumDebitRaw - expected.Deposit.MinimumDebitRaw + 1
		t.Stage, t.CollateralRemainingRaw = topupTrancheComplete, post[route.CollateralCustody].Raw
	default:
		return nil, budgetHold("invalid_topup_tranche_action")
	}
	if err = t.validate(); err != nil {
		return nil, err
	}
	return &t, nil
}

// Ordinary operations retain their own admission authority. Follow only their
// finalized collateral receipt: a credit is not new top-up funding. A complete
// deposit's bounded remainder can become the next exact carry. No balance-only
// adoption or new repayment permission is introduced here.
func reconcileOrdinaryTopupCollateral(before topupTranche, operationID string, expected ExpectedEffects, receipt ConfirmedTransactionEvidence) (*topupTranche, error) {
	if before.validate() != nil || topupWorkInFlight(&before) || !sha256Pattern.MatchString(operationID) || operationID == before.LastOperationID || !receipt.Finalized || receipt.Slot < before.LastSlot {
		return nil, budgetHold("topup_ordinary_receipt_unproven")
	}
	for _, effect := range expected.Accounts {
		if effect.Address != autoAUTOPYUSD.CollateralCustody {
			continue
		}
		if !topupReceiptIdentity(effect) {
			return nil, budgetHold("topup_receipt_namespace_changed")
		}
		reconciled, _, err := ReconcileConfirmedTransaction(expected, receipt)
		if err != nil {
			return nil, err
		}
		post, err := transactionBalancesByAddress(receipt.PostTokenBalances)
		if err != nil {
			return nil, err
		}
		next := before
		next.CollateralRemainingRaw = post[effect.Address].Raw
		next.Stage = topupTrancheOrdinary
		if next.CollateralRemainingRaw == 0 {
			next.Stage, next.DepositQuantumRaw = topupTrancheComplete, 1
		} else if expected.Kind == "kamino-deposit" && expected.Deposit != nil && expected.Deposit.MaximumDebitRaw == effect.BeforeRaw {
			quantum := expected.Deposit.MaximumDebitRaw - expected.Deposit.MinimumDebitRaw + 1
			if next.CollateralRemainingRaw < quantum {
				next.Stage, next.DepositQuantumRaw = topupTrancheComplete, quantum
			}
		}
		next.LastOperationID, next.LastSlot, next.LastEffectsSHA256 = operationID, receipt.Slot, reconciled.EffectsSHA256
		if err = next.validate(); err != nil {
			return nil, err
		}
		return &next, nil
	}
	return nil, nil
}
