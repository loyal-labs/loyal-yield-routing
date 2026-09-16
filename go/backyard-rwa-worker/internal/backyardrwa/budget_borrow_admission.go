package backyardrwa

import (
	"context"
	"encoding/binary"
	"math"
	"time"
)

func validateInitialBorrowPrestate(ctx context.Context, rpc *RPCClient, route RuntimeRoute, s Snapshot, slot int64) (int64, error) {
	observed, accounts, err := rpc.GetMultipleAccounts(ctx, []string{route.Kamino.Obligation, route.CollateralCustody, route.DebtCustody}, slot)
	if err != nil {
		return 0, err
	}
	o, err := decodeKaminoObligation(accountAt(accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil || o.debtRaw != 0 || s.PositionCollateralRaw <= 0 || o.collateralDepositedRaw != uint64(s.PositionCollateralRaw) {
		return 0, budgetHold("borrow_prestate_changed")
	}
	for _, row := range []struct {
		address, mint string
		raw           int64
	}{{route.CollateralCustody, route.Kamino.CollateralMint, s.CollateralIdleRaw}, {route.DebtCustody, route.Kamino.DebtMint, debtCashRaw(s)}} {
		a := accountAt(accounts, row.address)
		mint, _ := decodeBase58PublicKey(row.mint)
		owner, _ := decodeBase58PublicKey(bridgeVault)
		cash, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner)
		if err != nil || a.Executable || a.Lamports == 0 || row.raw < 0 || cash.Raw != uint64(row.raw) {
			return 0, budgetHold("borrow_prestate_changed")
		}
	}
	return observed, nil
}

func validateBorrowProjection(r KaminoPrimeUSDCRequest, e ExpectedEffects, s Snapshot, p phase3KaminoProjection) (KaminoPayoffBound, error) {
	message, err := CompileKaminoMessage(r)
	if err != nil || p.MessageSHA256 != sha256Bytes(message) || p.UnitsConsumed == 0 {
		return KaminoPayoffBound{}, budgetHold("borrow_projection_identity_mismatch")
	}
	debit, err := MeasureExecutableDebit(r, e)
	if err != nil {
		return KaminoPayoffBound{}, err
	}
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		return KaminoPayoffBound{}, err
	}
	for _, effect := range e.Accounts {
		a := accountAt(p.Accounts, effect.Address)
		mint, _ := decodeBase58PublicKey(effect.Mint)
		owner, _ := decodeBase58PublicKey(effect.Authority)
		cash, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner)
		if err != nil || a.Executable || a.Lamports == 0 || a.Owner != effect.Owner || cash.Raw != effect.AfterRaw {
			return KaminoPayoffBound{}, budgetHold("borrow_projection_custody_mismatch")
		}
	}
	o, err := decodeKaminoObligation(accountAt(p.Accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil || s.PositionCollateralRaw <= 0 || o.collateralDepositedRaw != uint64(s.PositionCollateralRaw) || o.debtRaw != debit.Raw {
		return KaminoPayoffBound{}, budgetHold("borrow_projection_position_mismatch")
	}
	bound, err := decodeKaminoPayoffWindow(p.Accounts, route, p.Slot, 3)
	if err != nil || bound.ObservedDebtRaw != debit.Raw {
		return bound, budgetHold("borrow_projection_debt_mismatch")
	}
	if _, err := decodeKaminoReserve(accountAt(p.Accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino); err != nil {
		return bound, err
	}
	clock := binary.LittleEndian.Uint64(accountAt(p.Accounts, budgetClockAddress).Data[:8])
	for _, address := range []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve, route.Kamino.Obligation} {
		if binary.LittleEndian.Uint64(accountAt(p.Accounts, address).Data[16:24]) != clock {
			return bound, budgetHold("borrow_projection_refresh_mismatch")
		}
	}
	a := accountAt(p.Accounts, route.CollateralCustody)
	mint, _ := decodeBase58PublicKey(route.Kamino.CollateralMint)
	owner, _ := decodeBase58PublicKey(bridgeVault)
	col, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner)
	if err != nil || a.Executable || a.Lamports == 0 || s.CollateralIdleRaw < 0 || col.Raw != uint64(s.CollateralIdleRaw) {
		return bound, budgetHold("borrow_projection_collateral_changed")
	}
	return bound, nil
}

// Borrow admission prices the immediate complete unwind, not permission for a
// later leverage loop. Simulated accounts remain cost inputs; only r is current.
func observePhase3BorrowAdmission(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, o Observation, d Decision, e KaminoExecutionEvidence) (phase3BridgeAdmission, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	s, r := o.Snapshot, e.Request
	if rpc == nil || client == nil || !s.Fresh || s.Slot <= 0 || s.RouteKind != RouteKind || s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.CutoverDrain ||
		s.RouteLane != s.StrategyKey || s.RouteLane != d.StrategyKey || s.RouteLane != r.RouteLane || !positionReturnRoute(s.RouteLane) || !s.HasPosition || s.PositionCollateralRaw <= 0 || s.PositionCollateralValueRaw <= 0 || s.PositionDebtRaw != 0 || s.PositionDebtValueRaw != 0 || debtCashRaw(s) != 0 || s.CollateralIdleRaw < 0 || s.PrimeIdleRaw != s.CollateralIdleRaw || s.SquadsIdleRaw < 0 || s.VoltrIdleRaw < 0 || s.VoltrStrategyIdleRaw != 0 || d.Action != OpenRouteStep || r.Action != d.Action || d.AmountRaw <= 0 || e.ExpectedEffects.Kind != "kamino-borrow" {
		return phase3BridgeAdmission{}, budgetHold("complete_initial_borrow_return_unavailable")
	}
	current, err := observePhase3KnownBuildCost(ctx, rpc, r, e.ExpectedEffects)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	route, err := runtimeRoute(s.RouteLane)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	slot, err := validateInitialBorrowPrestate(ctx, rpc, route, s, max(s.Slot, current.ObservationSlot))
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	projection, err := rpc.simulateKaminoEntryProjection(ctx, r, slot)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	_, err = validateBorrowProjection(r, e.ExpectedEffects, s, projection)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	if e.ExpectedEffects.Accounts[1].BeforeRaw != 0 {
		return phase3BridgeAdmission{}, budgetHold("borrow_cash_snapshot_changed")
	}
	plan, err := pricePhase3ProjectedPositionReturn(ctx, rpc, client, m, o, d, r, e.ExpectedEffects, current, projection)
	if err == nil {
		plan.BorrowProjection = &projection
	}
	return plan, err
}

// The simulation's poststate is used only for complete exit costing. Each
// producer validates its own current transition before entering this function;
// no projected account replaces a current build, RPC read or send prestate.
func pricePhase3ProjectedPositionReturn(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, o Observation, d Decision, request any, effects ExpectedEffects, current ValuedTransactionCost, projection phase3KaminoProjection) (phase3BridgeAdmission, error) {
	s := o.Snapshot
	route, err := runtimeRoute(s.RouteLane)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	bound, err := decodeKaminoPayoffWindow(projection.Accounts, route, projection.Slot, 3)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	position, err := decodeKaminoObligation(accountAt(projection.Accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil || position.collateralDepositedRaw == 0 || position.collateralDepositedRaw > math.MaxInt64 {
		return phase3BridgeAdmission{}, budgetHold("projected_return_position_unavailable")
	}
	s.PositionCollateralRaw = int64(position.collateralDepositedRaw)
	var cash uint64
	for _, row := range []struct{ address, mint string }{{route.CollateralCustody, route.Kamino.CollateralMint}, {route.DebtCustody, route.Kamino.DebtMint}} {
		a := accountAt(projection.Accounts, row.address)
		mint, _ := decodeBase58PublicKey(row.mint)
		owner, _ := decodeBase58PublicKey(bridgeVault)
		balance, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner)
		if err != nil || a.Executable || a.Lamports == 0 || balance.Raw > math.MaxInt64 {
			return phase3BridgeAdmission{}, budgetHold("projected_return_custody_unavailable")
		}
		if row.address == route.CollateralCustody {
			s.CollateralIdleRaw, s.PrimeIdleRaw = int64(balance.Raw), int64(balance.Raw)
		} else {
			cash = balance.Raw
		}
	}
	var blockhash LatestBlockhash
	switch r := request.(type) {
	case KaminoPrimeUSDCRequest:
		blockhash = LatestBlockhash{Blockhash: r.RecentBlockhash, LastValidBlockHeight: r.LastValidBlockHeight}
	case JupiterSwapRequest:
		blockhash = LatestBlockhash{Blockhash: r.RecentBlockhash, LastValidBlockHeight: r.LastValidBlockHeight}
	default:
		return phase3BridgeAdmission{}, budgetHold("invalid_projected_return_request")
	}
	remaining := uint64(s.PositionCollateralRaw)
	reserve, err := decodeKaminoReserve(accountAt(projection.Accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	accounts := append([]ConfirmedAccount(nil), projection.Accounts...)
	var release *KaminoExecutionEvidence
	var funding *JupiterExecutionEvidence
	var releaseCost, fundingCost ValuedTransactionCost
	upperCash := cash
	if cash < bound.UpperDebtRaw {
		// Borrow -> NAV -> release -> NAV -> swap -> NAV -> payoff. Combine
		// existing residue with the safe release; never require a dust-only swap.
		limit, err := decodeKaminoRepaymentReleaseWindow(projection.Accounts, route, projection.Slot, 7)
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		bound = limit.Payoff
		if limit.LiquidityRaw > uint64(math.MaxInt64-s.CollateralIdleRaw) {
			return phase3BridgeAdmission{}, budgetHold("borrow_release_buffer_overflow")
		}
		req, err := m.kaminoPacketForRoute(DeleverRouteStep, kaminoLegWithdraw, limit.ReceiptRaw, blockhash, s.RouteLane)
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		req.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
		source, destination := kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
		effects, err := exactKaminoTokenEffects(projection.Accounts, source, destination, limit.LiquidityRaw)
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		release = &KaminoExecutionEvidence{req, effects}
		releaseCost, err = observePhase3KnownBuildCost(ctx, rpc, req, effects)
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		buffer := uint64(s.CollateralIdleRaw) + limit.LiquidityRaw
		quote, err := prepareJupiterQuoteEvidence(ctx, rpc, client, m, Decision{Action: SwapCollateralToDebtStep, StrategyKey: s.RouteLane, AmountRaw: int64(buffer)}, buffer, cash, projection.Slot)
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		// Validate enforceable wire minimum against all interest windows using
		// a cost-only view of the post-release custody, not optimistic output.
		projected := append([]ConfirmedAccount(nil), projection.Accounts...)
		for i, a := range projected {
			if a.Address == route.CollateralCustody {
				projected[i].Data = append([]byte(nil), a.Data...)
				binary.LittleEndian.PutUint64(projected[i].Data[64:72], buffer)
			}
		}
		check := quote.Request
		check.FullPayoffFunding = true
		if _, _, err = validatePayoffFundingAccounts(check, quote.ExpectedEffects, bound, projected, route); err != nil {
			return phase3BridgeAdmission{}, err
		}
		funding = &quote
		fundingCost, err = observePhase3KnownBuildCost(ctx, rpc, quote.Request, quote.ExpectedEffects)
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		upper, err := withdrawalUSDCExitEstimate(quote.Request.QuotedOutputRaw)
		if err != nil || upper > math.MaxInt64-cash {
			return phase3BridgeAdmission{}, budgetHold("borrow_funding_output_overflow")
		}
		upperCash += upper
		remaining = limit.RemainingReceiptRaw
		reserve, err = projectKaminoReleaseReserve(reserve, limit.ReceiptRaw, limit.LiquidityRaw)
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		for i, a := range accounts {
			if a.Address == route.CollateralCustody || a.Address == route.CollateralLiquiditySupply {
				accounts[i].Data = append([]byte(nil), a.Data...)
				amount := uint64(0)
				if a.Address == route.CollateralLiquiditySupply {
					amount = effects.Accounts[0].AfterRaw
				}
				binary.LittleEndian.PutUint64(accounts[i].Data[64:72], amount)
			}
		}
	}
	payoff, err := m.kaminoPacketForRoute(DeleverRouteStep, kaminoLegRepay, bound.UpperDebtRaw, blockhash, s.RouteLane)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	payoff.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	payoffAccounts := append([]ConfirmedAccount(nil), accounts...)
	for i, a := range payoffAccounts {
		if a.Address == route.DebtCustody {
			payoffAccounts[i].Data = append([]byte(nil), a.Data...)
			binary.LittleEndian.PutUint64(payoffAccounts[i].Data[64:72], upperCash)
		}
	}
	source, destination := kaminoLegCustodiesForRoute(kaminoLegRepay, route)
	payoffEffects, err := boundedKaminoRepaymentEffects(payoffAccounts, source, destination, bound.ObservedDebtRaw, bound.UpperDebtRaw)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	payoffCost, err := observePhase3KnownBuildCost(ctx, rpc, payoff, payoffEffects)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	withdrawal, err := m.kaminoPacketForRoute(DeleverRouteStep, kaminoLegWithdraw, remaining, blockhash, s.RouteLane)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	withdrawal.ObligationReserves = []string{route.Kamino.CollateralReserve}
	policies := map[string]string{payoff.Policy: payoff.PolicyAccountDataSHA256, withdrawal.Policy: withdrawal.PolicyAccountDataSHA256}
	if release != nil {
		policies[release.Request.Policy] = release.Request.PolicyAccountDataSHA256
	}
	var addresses []string
	for address := range policies {
		addresses = append(addresses, address)
	}
	_, rows, err := rpc.GetMultipleAccounts(ctx, addresses, projection.Slot)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	for address, hash := range policies {
		a := accountAt(rows, address)
		if a.Owner != bridgeSquadsProgram || a.Executable || a.Lamports == 0 || sha256Bytes(a.Data) != hash {
			return phase3BridgeAdmission{}, budgetHold("borrow_exit_policy_drift")
		}
	}
	amount, err := reserve.redeemLiquidityRaw(remaining)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	source, destination = kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
	withdrawalEffects, err := exactKaminoTokenEffects(accounts, source, destination, amount)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	post := o
	post.Snapshot.PositionDebtRaw, post.Snapshot.PositionDebtValueRaw, post.Snapshot.PayoffDebtRaw = 0, 0, 0
	post.Snapshot.CollateralIdleRaw, post.Snapshot.PrimeIdleRaw = s.CollateralIdleRaw, s.PrimeIdleRaw
	post.Snapshot.PositionCollateralRaw = int64(remaining)
	setDebtCashRaw(&post.Snapshot, int64(upperCash-bound.ObservedDebtRaw))
	if funding != nil {
		post.Snapshot.CollateralIdleRaw, post.Snapshot.PrimeIdleRaw = 0, 0
	}
	tail, err := observePhase3WithdrawalAdmission(ctx, rpc, client, m, post, Decision{Action: DeleverRouteStep, StrategyKey: s.RouteLane}, KaminoExecutionEvidence{withdrawal, withdrawalEffects})
	if err != nil {
		return tail, err
	}
	if len(tail.Exit) == 0 || tail.Exit[0].Action != ReportNAV {
		return tail, budgetHold("borrow_exit_nav_unavailable")
	}
	encode := func(request any, effects ExpectedEffects) (*phase3BuildInput, error) {
		encoded, err := jsonMarshalExpectedEffects(effects)
		if err != nil {
			return nil, err
		}
		return encodePhase3BuildInput(request, encoded)
	}
	plan := tail
	plan.Snapshot, plan.Decision, plan.CurrentCost = o.Snapshot, d, current
	plan.Input, err = encode(request, effects)
	if err != nil {
		return plan, err
	}
	plan.Payoff, plan.PayoffWithdrawal = &bound, tail.Input
	plan.PayoffRepayment, err = encode(payoff, payoffEffects)
	if err != nil {
		return plan, err
	}
	nav := phase3BridgeExitCost{Action: ReportNAV, Cost: tail.Exit[0].Cost}
	prefix := []phase3BridgeExitCost{nav}
	if funding != nil {
		plan.BorrowRelease, err = encode(release.Request, release.ExpectedEffects)
		if err != nil {
			return plan, err
		}
		input, err := encode(funding.Request, funding.ExpectedEffects)
		if err != nil {
			return plan, err
		}
		plan.FundingSwap = &phase3QuotedExit{Input: input, QuotedOutputRaw: funding.Request.QuotedOutputRaw, EstimatedUpperOutputRaw: upperCash - cash, ProofLevel: "COST_ONLY_BORROW_RETURN_NOT_EXECUTED_FUNDING"}
		prefix = append(prefix, phase3BridgeExitCost{Action: DeleverRouteStep, Amount: release.Request.AmountRaw, Cost: releaseCost}, nav, phase3BridgeExitCost{Action: SwapCollateralToDebtStep, Amount: funding.Request.AmountRaw, Cost: fundingCost}, nav)
	}
	prefix = append(prefix, phase3BridgeExitCost{Action: DeleverRouteStep, Amount: payoff.AmountRaw, Cost: payoffCost}, nav, phase3BridgeExitCost{Action: DeleverRouteStep, Amount: withdrawal.AmountRaw, Cost: tail.CurrentCost})
	plan.Exit = append(prefix, tail.Exit...)
	plan.ExitAfterMicros = 0
	plan.ValidThroughSlot = min(current.ValidThroughSlot, tail.ValidThroughSlot, projection.Slot+budgetMaxObservationLagSlots)
	for _, step := range plan.Exit {
		plan.ExitAfterMicros, err = budgetSum(plan.ExitAfterMicros, step.Cost.TotalMicros)
		if err != nil {
			return plan, err
		}
		plan.ValidThroughSlot = min(plan.ValidThroughSlot, step.Cost.ValidThroughSlot)
	}
	if projection.Slot > plan.ValidThroughSlot {
		return plan, budgetHold("stale_borrow_exit_admission")
	}
	return plan, nil
}

func (d *Database) admitPhase3Borrow(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, id string, o Observation, decision Decision, e KaminoExecutionEvidence) error {
	plan, err := observePhase3BorrowAdmission(ctx, rpc, client, m, o, decision, e)
	if err != nil {
		return err
	}
	return d.persistPhase3ExitAdmission(ctx, rpc, id, o, decision, plan)
}

func validateBorrowAdmissionPrestate(ctx context.Context, rpc *RPCClient, r KaminoPrimeUSDCRequest, p *phase3BridgeAdmission, slot int64) (int64, error) {
	_, leg, err := kaminoPrimeUSDCInstruction(r)
	if err != nil || r.Action != OpenRouteStep || leg != kaminoLegBorrow || p == nil || p.Payoff == nil || p.BorrowProjection == nil || p.Snapshot.RouteLane != r.RouteLane {
		return 0, budgetHold("borrow_projection_identity_mismatch")
	}
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		return 0, err
	}
	observed, err := validateInitialBorrowPrestate(ctx, rpc, route, p.Snapshot, slot)
	if err != nil {
		return 0, err
	}
	fresh, accounts, err := rpc.GetMultipleAccounts(ctx, []string{route.Kamino.DebtReserve, budgetClockAddress}, observed)
	if err != nil {
		return 0, err
	}
	reserve := accountAt(accounts, route.Kamino.DebtReserve)
	if _, err = decodeKaminoReserve(reserve, route.Kamino.DebtMint, route.Kamino); err != nil {
		return 0, err
	}
	config := reserve.Data[kaminoReserveConfigOffset:]
	rate, err := kaminoMaximumBorrowRate(config)
	clock := accountAt(accounts, budgetClockAddress)
	if err != nil || config[9] != p.Payoff.InterestBasis || config[7] != 0 || !allZero(config[920:936]) || rate > p.Payoff.MaximumRateBPS || clock.Owner != "Sysvar1111111111111111111111111111111111111" || clock.Executable || len(clock.Data) != 40 {
		return 0, budgetHold("borrow_return_interest_window_changed")
	}
	now := int64(binary.LittleEndian.Uint64(clock.Data[32:40]))
	if now < p.Payoff.ChainUnix || now > p.Payoff.ChainUnix+kaminoPayoffWindowSeconds {
		return 0, budgetHold("borrow_return_interest_window_changed")
	}
	return fresh, nil
}
