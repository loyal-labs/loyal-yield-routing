package backyardrwa

// ExecutableDebit identifies the underlying economic source. Receipt tokens,
// swap outputs and intermediate hops are not additional principal debits.
// Network fees are priced separately from the exact unsigned message.
type ExecutableDebit struct {
	Source       string `json:"source"`
	Mint         string `json:"mint"`
	TokenProgram string `json:"tokenProgram"`
	Raw          uint64 `json:"raw"`
}

// MeasureExecutableDebit shares the production compilers and custody graph.
// It cannot accept an arbitrary caller-supplied action-to-value mapping.
// A withdrawal's wire amount may be receipt units: charge its underlying
// liquidity debit from the checked effect graph, not that receipt amount.
func MeasureExecutableDebit(request any, effects ExpectedEffects) (ExecutableDebit, error) {
	encoded, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		return ExecutableDebit{}, err
	}
	if _, err = DecodeExpectedEffects(encoded); err != nil {
		return ExecutableDebit{}, err
	}
	var source kaminoCustodyBoundary
	var exactAmount *uint64
	var sweep bool
	var repayment bool
	switch r := request.(type) {
	case BridgeBuildRequest:
		if _, err = CompileBridgeMessage(r); err != nil {
			return ExecutableDebit{}, err
		}
		switch r.Action {
		case VoltrAllocateToSquads:
			source = kaminoCustodyBoundary{bridgeIdleATA, bridgeUSDC, bridgeIdleAuthority}
		case StageSquadsToVoltr:
			source = kaminoCustodyBoundary{bridgeSquadsATA, bridgeUSDC, bridgeVault}
		case VoltrRestoreIdle:
			source = kaminoCustodyBoundary{bridgeStrategyATA, bridgeUSDC, bridgeStrategyAuth}
			sweep = true
		case ReportNAV:
			for _, account := range effects.Accounts {
				if account.BeforeRaw != account.AfterRaw {
					return ExecutableDebit{}, budgetHold("report_contains_capital_movement")
				}
			}
			return ExecutableDebit{}, nil
		default:
			return ExecutableDebit{}, budgetHold("unmapped_economic_action")
		}
		exactAmount = &r.AmountRaw
	case KaminoPrimeUSDCRequest:
		if _, err = CompileKaminoMessage(r); err != nil {
			return ExecutableDebit{}, err
		}
		_, leg, err := kaminoPrimeUSDCInstruction(r)
		if err != nil {
			return ExecutableDebit{}, err
		}
		lane := r.RouteLane
		if lane == "" {
			lane = RouteID
		}
		route, err := runtimeRoute(lane)
		if err != nil {
			return ExecutableDebit{}, err
		}
		source, _ = kaminoLegCustodiesForRoute(leg, route)
		if effects.Repayment != nil {
			_, destination := kaminoLegCustodiesForRoute(leg, route)
			if leg != kaminoLegRepay || effects.Repayment.MaximumDebitRaw != r.AmountRaw ||
				effects.Accounts[0].Owner != route.DebtTokenProgram ||
				effects.Accounts[0].Address != source.Address || effects.Accounts[1].Address != destination.Address ||
				effects.Accounts[1].Mint != destination.Mint || effects.Accounts[1].Authority != destination.Authority {
				return ExecutableDebit{}, budgetHold("repayment_request_or_destination_mismatch")
			}
			repayment = true
		}
		if leg != kaminoLegWithdraw {
			exactAmount = &r.AmountRaw
		}
	case JupiterSwapRequest:
		if _, err = CompileJupiterMessage(r); err != nil {
			return ExecutableDebit{}, err
		}
		mint, _, address, _, err := jupiterEdgeForRoute(r.Action, r.RouteLane)
		if err != nil {
			return ExecutableDebit{}, err
		}
		source = kaminoCustodyBoundary{address, mint, bridgeVault}
		exactAmount = &r.AmountRaw
	default:
		return ExecutableDebit{}, budgetHold("unmapped_economic_action")
	}
	if effects.Repayment != nil && !repayment {
		return ExecutableDebit{}, budgetHold("repayment_bounds_on_non_repayment")
	}
	for _, account := range effects.Accounts {
		if account.Address != source.Address {
			continue
		}
		if account.Mint != source.Mint || account.Authority != source.Authority || account.AfterRaw > account.BeforeRaw {
			return ExecutableDebit{}, budgetHold("economic_source_mismatch")
		}
		debit := ExecutableDebit{Source: source.Address, Mint: source.Mint, TokenProgram: account.Owner, Raw: account.BeforeRaw - account.AfterRaw}
		if sweep {
			// The observed Phase 2 restore swept the whole strategy balance;
			// a smaller requested amount is not an executable upper bound.
			debit.Raw = account.BeforeRaw
			if account.AfterRaw != 0 || exactAmount == nil || *exactAmount != debit.Raw {
				return debit, budgetHold("full_sweep_amount_mismatch")
			}
		}
		if debit.Raw == 0 || (exactAmount != nil && debit.Raw != *exactAmount) {
			return debit, budgetHold("economic_amount_mismatch")
		}
		return debit, nil
	}
	return ExecutableDebit{}, budgetHold("economic_source_missing")
}
