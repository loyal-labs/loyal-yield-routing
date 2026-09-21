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
	m, err := loadEmbeddedRouteManifest()
	if err != nil {
		return ExecutableDebit{}, err
	}
	return m.measureExecutableDebit(request, effects)
}

// The manifest-aware form exists only for the internal recipe-pricing path:
// retained selector payoff inputs compile against the SAME manifest that
// produced them (existing lanes and exact AUTO with a validated autoPolicy
// binding). Every other caller keeps the embedded-manifest behavior above.
func (m RouteManifest) measureExecutableDebit(request any, effects ExpectedEffects) (ExecutableDebit, error) {
	if effects.Kind == "kamino-initialize" || effects.Initialization != nil {
		r, ok := request.(KaminoInitializationRequest)
		// The manifest-aware effects validator shares every structural check
		// and recompiles the embedded request through the SAME explicit
		// manifest that produced this recipe.
		if !ok || m.validateInitializationEffects(effects) != nil || *effects.Initialization != r {
			return ExecutableDebit{}, budgetHold("initializer_effects_request_mismatch")
		}
		// Native creation is priced as SetupLamports, never as zero-cost setup
		// or a fictional USDC transfer.
		return ExecutableDebit{}, nil
	}
	if effects.Kind == "kamino-borrow" {
		r, ok := request.(KaminoPrimeUSDCRequest)
		_, leg, err := kaminoPrimeUSDCInstruction(r)
		if !ok || err != nil || leg != kaminoLegBorrow {
			return ExecutableDebit{}, budgetHold("borrow_effects_on_non_borrow")
		}
	}
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
	var deposit bool
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
		if _, err = m.compileKaminoMessage(r, mustKey(bridgeDelegate)); err != nil {
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
		if leg == kaminoLegBorrow && (r.Action == OpenRouteStep || effects.Kind == "kamino-borrow") {
			return measureBorrowDebit(r, effects, route)
		}
		source, _ = kaminoLegCustodiesForRoute(leg, route)
		if effects.Deposit != nil {
			_, destination := kaminoLegCustodiesForRoute(leg, route)
			if leg != kaminoLegDeposit || effects.Deposit.MaximumDebitRaw != r.AmountRaw || effects.Accounts[0].Address != source.Address ||
				effects.Accounts[0].Owner != route.CollateralTokenProgram || effects.Accounts[1].Address != destination.Address || effects.Accounts[1].Mint != destination.Mint || effects.Accounts[1].Authority != destination.Authority {
				return ExecutableDebit{}, budgetHold("deposit_request_or_destination_mismatch")
			}
			deposit = true
		}
		if r.RepaymentRelease && (r.FullPayoff || r.Action != DeleverRouteStep || leg != kaminoLegWithdraw || !positionReturnRoute(lane)) {
			return ExecutableDebit{}, budgetHold("invalid_repayment_release_intent")
		}
		if r.FullPayoff && (leg != kaminoLegRepay || effects.Repayment == nil) {
			return ExecutableDebit{}, budgetHold("invalid_full_payoff_intent")
		}
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
		if r.PositionReturnReserved && (r.Action != SwapDebtToCollateralStep || r.FullPayoffFunding || r.EntryReturnReserved) {
			return ExecutableDebit{}, budgetHold("invalid_position_return_intent")
		}
		if r.EntryReturnReserved && (r.Action != SwapStableToCollateralStep || r.FullPayoffFunding) {
			return ExecutableDebit{}, budgetHold("invalid_entry_return_intent")
		}
		if r.FullPayoffFunding && !isPayoffFundingAction(r.Action) {
			return ExecutableDebit{}, budgetHold("funding_bounds_on_non_funding_swap")
		}
		if _, err = m.compileJupiterMessage(r, mustKey(bridgeDelegate)); err != nil {
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
	if effects.Deposit != nil && !deposit {
		return ExecutableDebit{}, budgetHold("deposit_bounds_on_non_deposit")
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
