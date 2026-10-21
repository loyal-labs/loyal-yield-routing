package backyard

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

type lanePricer struct {
	rpc      *chain.Client
	view     *View
	client   *jupiter.Client
	manifest RouteManifest
	policies installedPolicies
	sol      BudgetPrice
}

func newLanePricer(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, m RouteManifest, slot int64) (lanePricer, error) {
	p := lanePricer{rpc: rpc, view: view, client: client, manifest: m}
	if rpc == nil || view == nil || client == nil || slot <= 0 {
		return p, costOutage("inputs", budgetHold("leverage_cost_unavailable"))
	}
	var err error
	if p.policies, err = observeInstalledPolicies(ctx, rpc, m.vault, slot); err != nil {
		return p, costOutage("policies", err)
	}
	if p.sol, err = ObserveNativeSOLBudgetPrice(ctx, rpc, view, slot); err != nil {
		return p, costOutage("price", err)
	}
	return p, nil
}

func (p lanePricer) networkMicros(message []byte) (int64, error) {
	fee, err := chain.MessageFee(message)
	if err != nil {
		return 0, budgetHoldFrom(err, "message_fee_unpriced")
	}
	return p.lamportsMicros(fee)
}

func (p lanePricer) lamportsMicros(lamports uint64) (int64, error) {
	if p.sol.Decimals != 9 {
		return 0, budgetHold("native_sol_decimal_mismatch")
	}
	return p.sol.valueUpper(lamports, nativeSOLBudgetAsset, "11111111111111111111111111111111", p.sol.ObservedSlot)
}

func (p lanePricer) setupMicros(ctx context.Context, route RuntimeRoute, blockhash LatestBlockhash) (int64, error) {
	rent, err := p.rpc.RentExempt(ctx, kamino.ObligationSize)
	if err != nil {
		return 0, budgetHoldFrom(err, "initializer_rent_unavailable")
	}
	lamports := rent
	if farms := uint64(len(obligationFarms(route))); farms > 0 {
		farmRent, err := p.rpc.RentExempt(ctx, kamino.FarmUserStateSize)
		if err != nil {
			return 0, budgetHoldFrom(err, "initializer_rent_unavailable")
		}
		lamports += farms * farmRent
	}
	r, err := p.manifest.initializationRequest(p.policies, route.Lane, blockhash, rent, 1)
	if err != nil {
		return 0, err
	}
	message, err := p.manifest.compileKaminoInitializationMessage(r)
	if err != nil {
		return 0, err
	}
	fee, err := p.networkMicros(message)
	if err != nil {
		return 0, err
	}
	value, err := p.lamportsMicros(lamports)
	if err != nil {
		return 0, err
	}
	return budgetSum(fee, value)
}

type lanePrices struct {
	collateral, debt, usdc           kamino.ScaledFraction
	collateralDecimals, debtDecimals uint8
	debtBorrowFeeSF                  uint64
}

func batchLanePrices(accounts []ConfirmedAccount, route RuntimeRoute) (lanePrices, error) {
	collateral, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return lanePrices{}, err
	}
	debt, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.DebtReserve), route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		return lanePrices{}, err
	}
	usdc := debt
	if route.Kamino.DebtMint != bridgeUSDC {
		if usdc, err = decodeKaminoReserve(accountAt(accounts, usdcReference.DebtReserve), bridgeUSDC, usdcReference); err != nil {
			return lanePrices{}, err
		}
	}
	if usdc.mintDecimals != 6 {
		return lanePrices{}, budgetHold("usdc_reference_metadata_mismatch")
	}
	if err := validateKaminoOracleAge(clockUnixTimestamp(accounts), collateral, debt, usdc); err != nil {
		return lanePrices{}, err
	}
	return lanePrices{collateral: collateral.marketPriceSF, debt: debt.marketPriceSF, usdc: usdc.marketPriceSF,
		collateralDecimals: collateral.mintDecimals, debtDecimals: debt.mintDecimals, debtBorrowFeeSF: debt.terms.BorrowFeeSF}, nil
}

func (p lanePrices) collateralMicros(raw uint64) (int64, error) {
	return centralValue(raw, p.collateralDecimals, p.collateral, p.usdc)
}

func (p lanePrices) debtMicros(raw uint64) (int64, error) {
	return centralValue(raw, p.debtDecimals, p.debt, p.usdc)
}

func centralValue(raw uint64, decimals uint8, price, usdc kamino.ScaledFraction) (int64, error) {
	if raw == 0 {
		return 0, nil
	}
	value, err := valueBetweenTokenRaw(raw, decimals, 6, price, usdc, false)
	if err != nil || value > math.MaxInt64 {
		return 0, budgetHoldFrom(err, "invalid_usdc_valuation")
	}
	return int64(value), nil
}

func conversionQuotedOutput(r StepRequest) uint64 {
	switch {
	case r.Swap != nil:
		return r.Swap.QuotedOutputRaw
	case r.Hastra != nil:
		return r.Hastra.OutputRaw
	}
	return 0
}

func (p lanePricer) priceStep(ctx context.Context, accounts []ConfirmedAccount, route RuntimeRoute, topology []string, slot int64, borrowRaw uint64) (leverageStepCost, error) {
	if borrowRaw == 0 || slot <= 0 {
		return leverageStepCost{}, costOutage("inputs", budgetHold("leverage_cost_unavailable"))
	}
	collateral, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return leverageStepCost{}, costOutage("inputs", err)
	}
	debtReserve := accountAt(accounts, route.Kamino.DebtReserve)
	if len(debtReserve.Data) < kaminoReserveConfigOffset+48 {
		return leverageStepCost{}, costOutage("inputs", budgetHold("leverage_cost_unavailable"))
	}
	fee, err := kaminoBorrowFeeAtRate(binary.LittleEndian.Uint64(debtReserve.Data[kaminoReserveConfigOffset+40:]), borrowRaw)
	if err != nil {
		return leverageStepCost{}, costOutage("inputs", err)
	}
	prices, err := batchLanePrices(accounts, route)
	if err != nil {
		return leverageStepCost{}, costOutage("price", err)
	}
	st := stepState{route: route, collateral: collateral, slot: uint64(slot), topology: topology}
	up, _, err := assembleStep(ctx, p.rpc, p.view, p.client, p.manifest, p.policies, st, stepUp, borrowRaw, borrowRaw, slot)
	if err != nil {
		return leverageStepCost{}, costOutage(stepStage("entry_quote", err), err)
	}
	bought := conversionQuotedOutput(up.Request)
	receipts, err := payoffReceiptsForLiquidity(collateral.totalLiquiditySF, collateral.collateralMintSupply, bought)
	if err != nil {
		return leverageStepCost{}, costOutage("exit_quote", err)
	}
	st.topology = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	down, _, err := assembleStep(ctx, p.rpc, p.view, p.client, p.manifest, p.policies, st, stepDown, receipts, bought, slot)
	if err != nil {
		return leverageStepCost{}, costOutage(stepStage("exit_quote", err), err)
	}
	var legs leverageStepLegs
	for _, v := range []struct {
		into  *int64
		value func(uint64) (int64, error)
		raw   uint64
	}{
		{&legs.BorrowFeeMicros, prices.debtMicros, fee},
		{&legs.EntryInMicros, prices.debtMicros, borrowRaw},
		{&legs.EntryOutMicros, prices.collateralMicros, bought},
		{&legs.ExitInMicros, prices.collateralMicros, bought},
		{&legs.ExitOutMicros, prices.debtMicros, conversionQuotedOutput(down.Request)},
	} {
		if *v.into, err = v.value(v.raw); err != nil {
			return leverageStepCost{}, costOutage("price", err)
		}
	}
	for _, step := range []StepRequest{up.Request, down.Request} {
		message, err := compileStepMessageForDelegate(p.manifest.vault, step, mustKey(bridgeDelegate))
		if err != nil {
			return leverageStepCost{}, costOutage("network_fee", err)
		}
		micros, err := p.networkMicros(message)
		if err != nil {
			return leverageStepCost{}, costOutage("network_fee", err)
		}
		if legs.NetworkMicros, err = budgetSum(legs.NetworkMicros, micros); err != nil {
			return leverageStepCost{}, costOutage("network_fee", err)
		}
	}
	c, err := legs.cost()
	if err != nil {
		return c, costOutage("inputs", err)
	}
	return c, nil
}

func stepStage(quote string, err error) string {
	var hold *BudgetHold
	if errors.As(err, &hold) {
		switch holdCause(hold) {
		case "step_does_not_compile", "step_over_policy_cap", "step_amounts_invalid", "step_over_key_limit":
			return "step_build"
		}
	}
	return quote
}

type entryCost struct {
	EntrySwapCostMicros  int64            `json:"entrySwapLossMicros"`
	ExitSwapCostMicros   int64            `json:"exitSwapLossMicros"`
	NetworkMicros        int64            `json:"networkMicros"`
	SetupMicros          int64            `json:"setupMicros,omitempty"`
	ReleaseNetworkMicros int64            `json:"releaseMicros,omitempty"`
	Step                 leverageStepCost `json:"step"`
	TotalMicros          int64            `json:"totalMicros"`
}

func (p lanePricer) priceEntry(ctx context.Context, o Observation, market LaneEconomics, equity uint64) (MoveQuote, error) {
	return p.priceMove(ctx, o, nil, nil, market, equity)
}

func (p lanePricer) priceMove(ctx context.Context, o Observation, source *Snapshot, keep *heldSettlement, market LaneEconomics, equity uint64) (MoveQuote, error) {
	s := o.Snapshot
	lane := market.Lane
	q := MoveQuote{SourceLane: s.RouteLane, DestinationLane: lane, ObservationID: s.ObservationID, ObservedAt: o.ObservedAt, SampleSlot: s.Slot,
		MinimumIdleRaw: uint64(max(s.VoltrIdleRaw-s.WithdrawalDemandRaw, 0)) + uint64(max(s.SquadsIdleRaw, 0)), ValidThroughSlot: s.Slot + observationLagSlots()}
	if source != nil {
		q.RotateFrom, q.MinimumIdleRaw, q.Keep = source.RouteLane, uint64(laneReleaseLimitRaw(*source)), keep
		if !rotationPair(source.RouteLane, lane) || keep == nil {
			return q, budgetHold("invalid_selector_destination")
		}
	}
	if !selectorDestinationLane(lane) || equity == 0 || equity > strategyTwoBridgeLegCapRaw || equity > q.MinimumIdleRaw || s.Slot <= 0 || o.ObservedAt.IsZero() {
		return q, budgetHold("invalid_selector_destination")
	}
	route, err := p.manifest.route(lane)
	if err != nil {
		return q, err
	}
	slot, accounts, position, err := selectorDestinationAccounts(ctx, p.manifest.vault, p.rpc, p.view, route, s.Slot)
	if err != nil {
		return q, err
	}
	if position.Obligation != obligationPresent && !earnInitializerLane(lane) {
		return q, budgetHold("selector_destination_obligation_absent")
	}
	if slot > q.ValidThroughSlot {
		return q, budgetHold("selector_quote_observation_expired")
	}
	prices, err := batchLanePrices(accounts, route)
	if err != nil {
		return q, err
	}
	room, err := kaminoAdditionalDebtRoom(accounts, route)
	if err != nil {
		return q, err
	}
	if value, err := prices.debtMicros(room); err == nil {
		display := uint64(value)
		q.DebtRoomUSDCRaw = &display
	}
	capacity, err := topupDepositRoomUSDC(accounts, route)
	if err != nil {
		return q, err
	}
	equity = min(equity, capacity)
	if equity == 0 || hardLTVBPS(position.LiquidationThresholdBPS, position.MaxLTVBPS()) <= laneCeilingLTVBPS(route.Lane) {
		return q, budgetHold("selector_destination_capacity_unavailable")
	}
	q.EquityRaw = int64(equity)
	entry, err := prepareJupiterQuoteEvidence(ctx, p.rpc, p.client, p.manifest, p.policies, Decision{Action: SwapStableToCollateralStep, AmountRaw: int64(equity), StrategyKey: lane}, equity, 0, slot)
	if err != nil {
		return q, err
	}
	bought, guaranteed := entry.Request.QuotedOutputRaw, entry.Request.MinimumOutputRaw
	if guaranteed <= 1 {
		return q, budgetHold("selector_destination_below_deposit_minimum")
	}
	funded := position
	funded.CollateralDepositedRaw, funded.RedeemablePrimeRaw, funded.DebtRaw = 1, guaranteed-1, 0
	depositRoom, err := unleveredEntryCapacityDebtRaw(position, accounts, route)
	if err != nil {
		return q, err
	}
	deposited, err := valueBetweenTokenRaw(guaranteed, position.CollateralDecimals, position.DebtDecimals, position.CollateralPriceSF, position.DebtPriceSF, true)
	if err != nil {
		return q, err
	}
	depositValue, err := prices.collateralMicros(guaranteed)
	if err != nil {
		return q, err
	}
	q.DepositValueRaw = uint64(depositValue)
	rate := prices.debtBorrowFeeSF
	debtLeft, depositLeft := room, depositRoom-min(deposited, depositRoom)
	collateralValue, debtValue := float64(depositValue), 0.0
	years := DefaultSelectorPolicy().Horizon.Hours() / (365.25 * 24)
	feeTerms := s.MonitorsArmed && approvedVoltrFeeTerms(s)
	var borrows, borrowFees []uint64
	var stepCosts []leverageStepCost
	var walkErr error
	level := settledLevel(market, 1, func(level float64) (float64, bool) {
		if n := len(borrows); n > 0 {
			bought, err := valueBetweenTokenRaw(borrows[n-1], position.DebtDecimals, position.CollateralDecimals, position.DebtPriceSF, position.CollateralPriceSF, false)
			if err != nil {
				walkErr = err
				return 0, false
			}
			funded.RedeemablePrimeRaw += bought
			funded.DebtRaw += borrows[n-1] + borrowFees[n-1]
		}
		borrow, err := capacitySizedBorrow(funded, accounts, route, level)
		if err != nil {
			walkErr = err
			return 0, false
		}
		if borrow = min(borrow, kaminoReceiveWithinRoom(debtLeft, rate), depositLeft); borrow < leverageMinimumBorrowRaw {
			return 0, false
		}
		fee, err := kaminoBorrowFeeAtRate(rate, borrow)
		if err != nil {
			walkErr = err
			return 0, false
		}
		debtLeft, depositLeft = debtLeft-min(borrow+fee, debtLeft), depositLeft-borrow
		borrows, borrowFees = append(borrows, borrow), append(borrowFees, fee)
		return float64(funded.DebtRaw + borrow + fee), true
	}, func(level, debt float64) bool {
		n := len(borrows)
		topology := []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
		if n == 1 {
			topology = topology[:1]
		}
		stepCost, err := p.priceStep(ctx, accounts, route, topology, slot, borrows[n-1])
		if err != nil {
			if n == 1 {
				walkErr = err
			}
			return false
		}
		proceeds, err := prices.debtMicros(borrows[n-1])
		if err != nil {
			walkErr = err
			return false
		}
		liability, err := prices.debtMicros(borrows[n-1] + borrowFees[n-1])
		if err != nil {
			walkErr = err
			return false
		}
		if n > 1 {
			keepAPR, errKeep := projectedBorrowAPR(market, float64(funded.DebtRaw))
			apr, errStep := projectedBorrowAPR(market, debt)
			if !feeTerms || errKeep != nil || errStep != nil ||
				!(upStepGain(market, collateralValue, debtValue, float64(liability), float64(proceeds), keepAPR, apr, float64(stepCost.TotalMicros), years, s.ApprovedAdminPerformanceFeeBPS) > 0) {
				return false
			}
		}
		collateralValue, debtValue = collateralValue+float64(proceeds), debtValue+float64(liability)
		stepCosts = append(stepCosts, stepCost)
		return true
	})
	if walkErr != nil {
		return q, walkErr
	}
	steps, _ := leverageRungIndex(level)
	q.SpreadUnlevered = steps == 0 && len(borrows) > 0
	q.Unlevered = steps == 0
	var cost entryCost
	if steps > 0 {
		var borrow, fee uint64
		for i := range steps {
			borrow, fee = borrow+borrows[i], fee+borrowFees[i]
			if cost.Step, err = cost.Step.plus(stepCosts[i]); err != nil {
				return q, err
			}
		}
		debt, err := prices.debtMicros(borrow + fee)
		if err != nil {
			return q, err
		}
		proceeds, err := prices.debtMicros(borrow)
		if err != nil {
			return q, err
		}
		q.BorrowReceiveRaw, q.BorrowFeeRaw, q.BorrowValueRaw, q.ProceedsValueRaw = borrow, fee, uint64(debt), uint64(proceeds)
	}
	reserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return q, err
	}
	receipts, err := payoffReceiptsForLiquidity(reserve.totalLiquiditySF, reserve.collateralMintSupply, bought)
	if err != nil {
		return q, err
	}
	exit, _, err := assembleStep(ctx, p.rpc, p.view, p.client, p.manifest, p.policies, stepState{route: route, collateral: reserve, slot: uint64(slot), topology: []string{route.Kamino.CollateralReserve}},
		stepExit, receipts, bought, slot)
	if err != nil {
		return q, err
	}
	boughtValue, err := prices.collateralMicros(bought)
	if err != nil {
		return q, err
	}
	cost.EntrySwapCostMicros = int64(equity) - boughtValue
	cost.ExitSwapCostMicros = boughtValue - int64(conversionQuotedOutput(exit.Request))
	blockhash := LatestBlockhash{Blockhash: entry.Request.RecentBlockhash, LastValidBlockHeight: entry.Request.LastValidBlockHeight}
	deposit, err := p.manifest.kaminoPacketForRoute(p.policies, OpenRouteStep, kaminoLegDeposit, guaranteed, blockhash, lane)
	if err != nil {
		return q, err
	}
	for _, compile := range []func() ([]byte, error){
		func() ([]byte, error) {
			return compileJupiterMessageForDelegate(p.manifest.vault, entry.Request, mustKey(bridgeDelegate))
		},
		func() ([]byte, error) {
			return compileKaminoMessageForDelegate(p.manifest.vault, deposit, mustKey(bridgeDelegate))
		},
		func() ([]byte, error) {
			return compileStepMessageForDelegate(p.manifest.vault, exit.Request, mustKey(bridgeDelegate))
		},
	} {
		message, err := compile()
		if err != nil {
			return q, err
		}
		micros, err := p.networkMicros(message)
		if err != nil {
			return q, err
		}
		if cost.NetworkMicros, err = budgetSum(cost.NetworkMicros, micros); err != nil {
			return q, err
		}
	}
	if position.Obligation != obligationPresent {
		if cost.SetupMicros, err = p.setupMicros(ctx, route, blockhash); err != nil {
			return q, err
		}
	}
	if source != nil {
		if cost.ReleaseNetworkMicros, err = p.releaseNetworkMicros(ctx, o, *source, equity); err != nil {
			return q, err
		}
	}
	fees, err := budgetSum(cost.NetworkMicros, cost.SetupMicros, cost.ReleaseNetworkMicros, cost.Step.TotalMicros)
	if err != nil {
		return q, err
	}
	if cost.TotalMicros = fees + cost.EntrySwapCostMicros + cost.ExitSwapCostMicros; cost.TotalMicros < 0 {
		cost.TotalMicros = 0
	}
	q.Cost, q.CostRaw = cost, cost.TotalMicros
	if !q.validBorrow() {
		return q, budgetHold("selector_entry_borrow_invalid")
	}
	if q.CostRaw >= q.EquityRaw {
		return q, budgetHold("selector_move_cost_exceeds_equity")
	}
	raw, err := json.Marshal(struct {
		Quote          MoveQuote
		AccountSlot    int64
		AccountsSHA256 string
	}{q, slot, hashConfirmedAccounts(accounts)})
	if err != nil {
		return q, err
	}
	q.EvidenceID = sha256Bytes(raw)
	return q, nil
}

type heldSettlement struct {
	Level              float64 `json:"level"`
	CollateralValueRaw int64   `json:"collateralValueRaw"`
	DebtValueRaw       int64   `json:"debtValueRaw"`
	RateDebtRaw        float64 `json:"rateDebtRaw,omitempty"`
	CostRaw            int64   `json:"costRaw,omitempty"`
}

func (p lanePricer) settleHeld(ctx context.Context, o Observation, s Snapshot, m LaneEconomics) (heldSettlement, error) {
	out := heldSettlement{CollateralValueRaw: s.PositionCollateralValueRaw, DebtValueRaw: s.PositionDebtValueRaw}
	equity := laneEquityRaw(s)
	if equity <= 0 {
		return out, budgetHold("selector_release_unpriced")
	}
	out.Level = float64(s.PositionCollateralValueRaw) / float64(equity)
	if !s.MonitorsArmed || !approvedVoltrFeeTerms(s) {
		return out, nil
	}
	years := DefaultSelectorPolicy().Horizon.Hours() / (365.25 * 24)
	collateral, debt := float64(s.PositionCollateralValueRaw), float64(s.PositionDebtValueRaw)
	var taken uint64
	roomLeft := s.AdditionalDebtRoomRaw
	var rateDebt float64
	var cost leverageStepCost
	var walkErr error
	from := heldFromLevel(s)
	level := settledLevel(m, from, func(level float64) (float64, bool) { return heldRungDebt(s, level) }, func(level, rung float64) bool {
		desired := leverageDesiredRaw(s, level)
		if desired < taken+leverageMinimumBorrowRaw {
			return false
		}
		raw := min(desired-taken, kaminoReceiveWithinRoom(roomLeft, s.BorrowFeeRate))
		if raw < leverageMinimumBorrowRaw {
			return false
		}
		fee, err := kaminoBorrowFeeAtRate(s.BorrowFeeRate, raw)
		if err != nil {
			return false
		}
		proceeds, errProceeds := capacityBorrowExpectedUSDC(s, raw, false)
		liability, errDebt := capacityBorrowExpectedUSDC(s, raw+fee, true)
		keepAPR, errKeep := math.Log1p(m.CurrentBorrowAPY), error(nil)
		if taken > 0 {
			keepAPR, errKeep = projectedBorrowAPR(m, rateDebt)
		}
		apr, errRate := projectedBorrowAPR(m, rung)
		if walkErr = errors.Join(errProceeds, errDebt, errKeep, errRate); walkErr != nil {
			return false
		}
		route, err := p.manifest.route(s.RouteLane)
		if err != nil || o.routeBatch == nil {
			walkErr = costOutage("inputs", budgetHold("leverage_cost_unavailable"))
			return false
		}
		topology := []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
		if taken == 0 {
			if topology, err = observedObligationTopology(o.routeBatch.Accounts, route); err != nil {
				walkErr = costOutage("inputs", err)
				return false
			}
		}
		step, err := p.priceStep(ctx, o.routeBatch.Accounts, route, topology, s.Slot, raw)
		if err != nil {
			walkErr = err
			return false
		}
		if !(upStepGain(m, collateral, debt, float64(liability), float64(proceeds), keepAPR, apr, float64(step.TotalMicros), years, s.ApprovedAdminPerformanceFeeBPS) > 0) {
			return false
		}
		if cost, err = cost.plus(step); err != nil {
			walkErr = err
			return false
		}
		collateral, debt = collateral+float64(proceeds), debt+float64(liability)
		taken, roomLeft, rateDebt = taken+raw, roomLeft-min(raw+fee, roomLeft), rung
		return true
	})
	if walkErr != nil {
		return out, walkErr
	}
	if level > from {
		out = heldSettlement{Level: level, CollateralValueRaw: int64(collateral), DebtValueRaw: int64(debt), RateDebtRaw: rateDebt, CostRaw: max(cost.TotalMicros, 0)}
	}
	return out, nil
}

func (p lanePricer) releaseNetworkMicros(ctx context.Context, o Observation, source Snapshot, equity uint64) (int64, error) {
	if o.routeBatch == nil || equity > math.MaxInt64 {
		return 0, budgetHold("selector_release_unpriced")
	}
	rel := newSourceRelease(source, int64(equity))
	if rel == nil {
		return 0, budgetHold("selector_release_unpriced")
	}
	route, err := p.manifest.route(source.RouteLane)
	if err != nil {
		return 0, err
	}
	st, err := readStepState(p.manifest.vault, o.routeBatch.Accounts, route, source.Slot)
	if err != nil {
		return 0, err
	}
	var steps []StepRequest
	if rel.DebtRaw > rel.DebtTargetRaw {
		d := Decision{Action: DeleverRouteStep, Reason: rotateRepayReason, AmountRaw: rel.DebtRaw - rel.DebtTargetRaw, StrategyKey: source.RouteLane}
		repay, _ := stepDownRepay(source, d)
		if headroom := stepLegCap(route).headroom(); headroom.exceeded(repay) {
			repay = headroom.raw
		}
		plan, err := planDownStep(source, d, st, repay)
		if err != nil {
			return 0, err
		}
		step, _, err := assembleStep(ctx, p.rpc, p.view, p.client, p.manifest, p.policies, st, stepDown, plan.withdrawReceiptsRaw, plan.convertCollateralRaw, source.Slot)
		if err != nil {
			return 0, err
		}
		steps = append(steps, step.Request)
	}
	d := Decision{Action: DeleverRouteStep, Reason: rotateWithdrawReason, AmountRaw: rel.CollateralRaw - rel.CollateralTargetRaw, StrategyKey: source.RouteLane}
	step, _, err := releaseLeg(ctx, p.rpc, p.view, p.client, p.manifest, p.policies, o.routeBatch.Accounts, st, source, d)
	if err != nil {
		return 0, err
	}
	steps = append(steps, step.Request)
	total := int64(0)
	for _, request := range steps {
		message, err := compileStepMessageForDelegate(p.manifest.vault, request, mustKey(bridgeDelegate))
		if err != nil {
			return 0, err
		}
		micros, err := p.networkMicros(message)
		if err != nil {
			return 0, err
		}
		if total, err = budgetSum(total, micros); err != nil {
			return 0, err
		}
	}
	return total, nil
}
