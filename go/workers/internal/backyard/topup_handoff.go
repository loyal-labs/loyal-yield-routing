package backyard

// This reference identifies the immutable, independently authorized destination
// origin. The locked caller must verify its partial-withdrawal, unwind or risk
// authority. Neither this ledger nor a decision reason grants exit permission.
type topupHandoffAuthority struct {
	OriginOperationID string `json:"originOperationId"`
	AuthoritySHA256   string `json:"authoritySha256"`
}

func (a topupHandoffAuthority) valid() bool {
	return sha256Pattern.MatchString(a.OriginOperationID) && sha256Pattern.MatchString(a.AuthoritySHA256)
}

func topupReceiptIdentity(effect ExpectedAccountEffect) bool {
	route := autoAUTOPYUSD
	owner, mint, authority := bridgeTokenProgram, bridgeUSDC, bridgeVault
	switch effect.Address {
	case bridgeIdleATA:
		authority = bridgeIdleAuthority
	case bridgeStrategyATA:
		authority = bridgeStrategyAuth
	case bridgeSquadsATA:
	case route.CollateralCustody:
		owner, mint = route.CollateralTokenProgram, route.Kamino.CollateralMint
	case route.CollateralLiquiditySupply:
		owner, mint, authority = route.CollateralTokenProgram, route.Kamino.CollateralMint, route.Kamino.MarketAuthority
	case route.DebtCustody:
		owner, mint = route.DebtTokenProgram, route.Kamino.DebtMint
	case route.DebtLiquiditySupply:
		owner, mint, authority = route.DebtTokenProgram, route.Kamino.DebtMint, route.Kamino.MarketAuthority
	default:
		return false
	}
	return effect.Owner == owner && effect.Mint == mint && effect.Authority == authority
}

// Reassign the owned inventory to an already-authorized exit and follow its
// actual receipts. All prior owned amounts remain in the predecessor binding.
// An authorized position release may add its proven proceeds to the exit's
// inventory; a borrow, balance observation or quote cannot add any inventory.
func reconcileTopupHandoff(before topupTranche, binding topupTrancheBinding, destination topupHandoffAuthority, operationID string, action Action, expected ExpectedEffects, receipt ConfirmedTransactionEvidence) (*topupTranche, error) {
	if before.validate() != nil || !binding.matches(&before) || binding.Lane != before.Lane || binding.OriginOperationID != before.OriginOperationID ||
		binding.AllocatedUSDCRaw != before.AllocatedUSDCRaw || binding.Loan != before.Loan || !destination.valid() || !sha256Pattern.MatchString(operationID) ||
		!receipt.Finalized || receipt.Slot < before.LastSlot || operationID == before.LastOperationID ||
		(before.Handoff.valid() && before.Handoff != destination) || expected.Schema != "loyal-backyard-rwa-expected-effects/v1" {
		return nil, budgetHold("topup_handoff_binding_changed")
	}
	if before.Stage != topupTrancheHandoff && before.USDCRemainingRaw == 0 && before.CollateralRemainingRaw == 0 {
		return nil, budgetHold("topup_handoff_inventory_unavailable")
	}
	route := autoAUTOPYUSD
	valid := false
	switch expected.Kind {
	case "bridge":
		if action == StageSquadsToVoltr {
			valid = expected.Conserved && expected.ReturnData == nil && len(expected.Accounts) == 2 &&
				expected.Accounts[0].Address == bridgeStrategyATA && expected.Accounts[1].Address == bridgeSquadsATA
		} else {
			valid = expected.Conserved && expected.ReturnData != nil && expected.ReturnData.ProgramID == bridgeAdaptorProgram && len(expected.Accounts) == 3 &&
				expected.Accounts[0].Address == bridgeIdleATA && expected.Accounts[1].Address == bridgeStrategyATA && expected.Accounts[2].Address == bridgeSquadsATA &&
				(action == VoltrRestoreIdle || action == ReportNAV && before.Stage == topupTrancheHandoff)
		}
	case "cross-mint-swap":
		sourceMint, destinationMint, source, destination, err := jupiterEdgeForRoute(action, route.Lane)
		valid = err == nil && (action == SwapStableToCollateralStep || action == SwapCollateralToStableStep || action == SwapCollateralToDebtStep || action == SwapUSDCToDebtStep || action == SwapDebtToUSDCStep) &&
			len(expected.Accounts) == 2 && expected.Accounts[0].Address == source && expected.Accounts[0].Mint == sourceMint &&
			expected.Accounts[1].Address == destination && expected.Accounts[1].Mint == destinationMint
	case "": // Exact withdrawal effects use the existing untagged conserved form.
		valid = expected.Conserved && action == DeleverRouteStep && len(expected.Accounts) == 2 && expected.Accounts[0].Address == route.CollateralLiquiditySupply && expected.Accounts[1].Address == route.CollateralCustody
	case "kamino-repay":
		valid = action == DeleverRouteStep && expected.Repayment != nil && len(expected.Accounts) == 2 && expected.Accounts[0].Address == route.DebtCustody && expected.Accounts[1].Address == route.DebtLiquiditySupply
	}
	if !valid || expected.Deposit != nil {
		return nil, budgetHold("topup_handoff_action_unavailable")
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
	next := before
	next.Generation++
	owned := map[string]*uint64{bridgeSquadsATA: &next.USDCRemainingRaw, route.CollateralCustody: &next.CollateralRemainingRaw,
		route.DebtCustody: &next.DebtRemainingRaw, bridgeStrategyATA: &next.StrategyRemainingRaw}
	changed := false
	for _, effect := range expected.Accounts {
		remaining, tracked := owned[effect.Address]
		if !tracked {
			continue
		}
		if pre[effect.Address].Raw != *remaining {
			return nil, budgetHold("topup_handoff_custody_changed")
		}
		changed = changed || *remaining != post[effect.Address].Raw
		*remaining = post[effect.Address].Raw
	}
	if !changed && before.Stage != topupTrancheHandoff {
		return nil, budgetHold("topup_handoff_transfer_unproven")
	}
	next.Stage, next.Handoff = topupTrancheHandoff, destination
	next.LastOperationID, next.LastSlot, next.LastEffectsSHA256 = operationID, receipt.Slot, reconciled.EffectsSHA256
	if next.USDCRemainingRaw == 0 && next.DebtRemainingRaw == 0 && next.StrategyRemainingRaw == 0 &&
		(next.CollateralRemainingRaw == 0 || next.DepositQuantumRaw > 0 && next.CollateralRemainingRaw < next.DepositQuantumRaw) {
		// Retain an already authenticated deposit-rounding carry exactly. A
		// demand change cannot erase it, and returning other cash must not
		// strand that known carry in an unfinished handoff forever.
		next.Stage = topupTrancheComplete
		if next.DepositQuantumRaw == 0 {
			next.DepositQuantumRaw = 1
		}
	}
	if err := next.validate(); err != nil {
		return nil, err
	}
	return &next, nil
}
