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

// The selector prices a lane from the transactions the executor would send,
// never from a model of them: a leverage step is the atomic step
// assembleStep builds (docs/backyard-atomic-step.md), a conversion is the
// quote its own builder takes, and every amount is valued at the reserve
// prices of the refreshed batch the decision reads. One pricer serves the
// level decision (leverage_target.go) and the entry and rotation ranking
// (SelectOpportunity).

// lanePricer is one selector sample's pricing inputs: the policies every
// built message executes through and the SOL price network fees are valued
// at, each read once per sample.
type lanePricer struct {
	rpc      *chain.Client
	view     *View
	client   *jupiter.Client
	manifest RouteManifest
	policies installedPolicies
	sol      BudgetPrice
}

// newLanePricer reads the sample's policies and SOL price.
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

// networkMicros is a compiled message's network fee in USDC micros.
func (p lanePricer) networkMicros(message []byte) (int64, error) {
	fee, err := chain.MessageFee(message)
	if err != nil {
		return 0, budgetHoldFrom(err, "message_fee_unpriced")
	}
	return p.lamportsMicros(fee)
}

// lamportsMicros is native SOL in USDC micros.
func (p lanePricer) lamportsMicros(lamports uint64) (int64, error) {
	if p.sol.Decimals != 9 {
		return 0, budgetHold("native_sol_decimal_mismatch")
	}
	return p.sol.valueUpper(lamports, nativeSOLBudgetAsset, "11111111111111111111111111111111", p.sol.ObservedSlot)
}

// setupMicros is what creating route's obligation costs, the initializer
// the executor sends before the entry: the obligation's rent, its farm user
// states' rent and the initializer's network fee.
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

// lanePrices are a lane's reserve market prices in one refreshed batch:
// its collateral, its debt and the pinned USDC reference.
type lanePrices struct {
	collateral, debt, usdc           [16]byte
	collateralDecimals, debtDecimals uint8
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
	// One price age holds every valuation, the USDC reference's included.
	if err := validateKaminoOracleAge(clockUnixTimestamp(accounts), collateral, debt, usdc); err != nil {
		return lanePrices{}, err
	}
	return lanePrices{collateral: collateral.marketPriceSF, debt: debt.marketPriceSF, usdc: usdc.marketPriceSF,
		collateralDecimals: collateral.mintDecimals, debtDecimals: debt.mintDecimals}, nil
}

// collateralValue and debtValue are raw amounts in USDC micros at the batch's
// central prices: expectations, never bounds.
func (p lanePrices) collateralValue(raw uint64) (int64, error) {
	return centralValue(raw, p.collateralDecimals, p.collateral, p.usdc)
}

func (p lanePrices) debtValue(raw uint64) (int64, error) {
	return centralValue(raw, p.debtDecimals, p.debt, p.usdc)
}

func centralValue(raw uint64, decimals uint8, price, usdc [16]byte) (int64, error) {
	if raw == 0 {
		return 0, nil
	}
	value, err := valueBetweenTokenRaw(raw, decimals, 6, price, usdc, false)
	if err != nil || value > math.MaxInt64 {
		return 0, budgetHoldFrom(err, "invalid_usdc_valuation")
	}
	return int64(value), nil
}

// conversionQuotedOutput is a step's conversion's quoted output: Jupiter's
// quote, or Hastra's output at its quoted price.
func conversionQuotedOutput(r StepRequest) uint64 {
	switch {
	case r.Swap != nil:
		return r.Swap.QuotedOutputRaw
	case r.Hastra != nil:
		return r.Hastra.OutputRaw
	}
	return 0
}

// priceStep is the expected cost of borrowing borrowRaw more on route's lane
// (leverageStepCost): the up step the executor assembles from accounts
// (borrow, convert, deposit in one transaction), and the down step that
// unwinds it (withdraw the collateral the up step's conversion quotes,
// convert it back, repay). topology is the obligation's reserves before the
// up step. Held custody is vault cash, not the step's, so neither step
// counts any.
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
	up, _, err := assembleStep(ctx, p.rpc, p.view, p.client, p.manifest, p.policies, st, SwapDebtToCollateralStep, borrowRaw, borrowRaw, slot)
	if err != nil {
		return leverageStepCost{}, costOutage(stepStage("entry_quote", err), err)
	}
	bought := conversionQuotedOutput(up.Request)
	receipts, err := payoffReceiptsForLiquidity(collateral.totalLiquiditySF, collateral.collateralMintSupply, bought)
	if err != nil {
		return leverageStepCost{}, costOutage("exit_quote", err)
	}
	st.topology = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	down, _, err := assembleStep(ctx, p.rpc, p.view, p.client, p.manifest, p.policies, st, SwapCollateralToDebtStep, receipts, bought, slot)
	if err != nil {
		return leverageStepCost{}, costOutage(stepStage("exit_quote", err), err)
	}
	var legs leverageStepLegs
	for _, v := range []struct {
		into  *int64
		value func(uint64) (int64, error)
		raw   uint64
	}{
		{&legs.BorrowFeeMicros, prices.debtValue, fee},
		{&legs.EntryInMicros, prices.debtValue, borrowRaw},
		{&legs.EntryOutMicros, prices.collateralValue, bought},
		{&legs.ExitInMicros, prices.collateralValue, bought},
		{&legs.ExitOutMicros, prices.debtValue, conversionQuotedOutput(down.Request)},
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

// stepStage names a step pricing failure: step_build when the step the
// executor would assemble at this size does not build (it does not compile
// within the key budget, or exceeds a policy leg cap), else the quote's own
// stage.
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

// entryCost is what an entry into a lane is expected to cost the vault's
// capital: the entry conversion's loss, the sale's loss when the collateral
// returns to USDC, the network fees of the lane's own transactions (entry
// swap, deposit, exit step), the up step the entry levers with, with its
// unwind (zero at 1x), the creation of an absent obligation (setupMicros) and,
// on a rotation, its source's release (releaseMicros). The swap losses are
// signed, against the batch's mark: a collateral bought under its mark gains
// on entry what it loses on exit. Voltr's allocate, stage and restore move
// the same cash for every lane and are not lane costs.
type entryCost struct {
	EntrySwapLossMicros int64            `json:"entrySwapLossMicros"`
	ExitSwapLossMicros  int64            `json:"exitSwapLossMicros"`
	NetworkMicros       int64            `json:"networkMicros"`
	SetupMicros         int64            `json:"setupMicros,omitempty"`
	ReleaseMicros       int64            `json:"releaseMicros,omitempty"`
	Step                leverageStepCost `json:"step"`
	TotalMicros         int64            `json:"totalMicros"`
}

// priceEntry prices cash's entry of equity into market's lane at o's
// sample (priceMove with no source).
func (p lanePricer) priceEntry(ctx context.Context, o Observation, market LaneEconomics, equity uint64) (MoveQuote, error) {
	return p.priceMove(ctx, o, nil, market, equity)
}

// priceMove prices an entry of equity into market's lane at o's sample, as
// the executor makes it: the entry swap step's own quote, the deposit, then
// the level rule's up step to 1.5x when the spread rule levers the lane (the
// capacity-sized borrow the funded position would take), and the unwind of
// both: the down step and the atomic exit step. The entry sits beside the
// lane's position when it holds one. The lane's batch is one refreshed
// capture of its reserves; every value is at its prices.
//
// Without a source the equity is cash: Voltr idle and Squads cash. With one
// it is a rotation's tranche of the source's equity, which the source's
// release frees as Squads cash (releaseMicros); a release never frees 90%
// of a lane.
func (p lanePricer) priceMove(ctx context.Context, o Observation, source *Snapshot, market LaneEconomics, equity uint64) (MoveQuote, error) {
	s := o.Snapshot
	lane := market.Lane
	q := MoveQuote{SourceLane: s.RouteLane, DestinationLane: lane, ObservationID: s.ObservationID, ObservedAt: o.ObservedAt, SampleSlot: s.Slot,
		MinimumIdleRaw: uint64(max(s.VoltrIdleRaw, 0)) + uint64(max(s.SquadsIdleRaw, 0)), ValidThroughSlot: s.Slot + observationLagSlots()}
	if source != nil {
		q.RotateFrom, q.MinimumIdleRaw = source.RouteLane, uint64(laneReleaseLimitRaw(*source))
		if !rotationPair(source.RouteLane, lane) {
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
	// An absent obligation is a flat position the lane's initializer creates
	// before the entry allocates (target's obligationPrerequisiteHold), at
	// the setup cost the entry carries; a lane without one holds there, so
	// it is no entry.
	if !position.ObligationPresent && !earnInitializerLane(lane) {
		return q, budgetHold("selector_destination_obligation_absent")
	}
	if slot > q.ValidThroughSlot {
		return q, budgetHold("selector_quote_observation_expired")
	}
	prices, err := batchLanePrices(accounts, route)
	if err != nil {
		return q, err
	}
	// The entry takes at most the collateral reserve's deposit room, the
	// capacity the tick's observation gives the lane (topupDepositRoomUSDC):
	// one derivation, so a quote never asks more than the step allocates.
	// The borrowing evidence validates first; unknown is not a zero loan.
	room, err := kaminoAdditionalDebtRoom(accounts, route)
	if err != nil {
		return q, err
	}
	if value, err := prices.debtValue(room); err == nil {
		display := uint64(value)
		q.DebtRoomUSDCRaw = &display
	}
	capacity, err := topupDepositRoomUSDC(accounts, route)
	if err != nil {
		return q, err
	}
	equity = min(equity, capacity)
	if equity == 0 || hardLTVBPS(position.LiquidationThresholdBPS) <= laneCeilingLTVBPS(route.Lane) {
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
	// The level rule's up step once the entry's guaranteed collateral is
	// deposited: the capacity-sized borrow of that funded position, within the
	// collateral reserve's deposit room left after the entry's own deposit.
	funded := position
	funded.CollateralDepositedRaw, funded.RedeemablePrimeRaw, funded.DebtRaw = 1, guaranteed-1, 0
	borrow, err := capacitySizedBorrow(funded, accounts, route, leverageLadder[1].level)
	if err != nil {
		return q, err
	}
	depositRoom, err := unleveredEntryCapacityDebtRaw(position, accounts, route)
	if err != nil {
		return q, err
	}
	deposited, err := valueBetweenTokenRaw(guaranteed, position.CollateralDecimals, position.DebtDecimals, position.CollateralPriceSF, position.DebtPriceSF, true)
	if err != nil {
		return q, err
	}
	if deposited >= depositRoom {
		borrow = 0
	}
	borrow = min(borrow, depositRoom-min(deposited, depositRoom))
	if borrow < leverageMinimumBorrowRaw {
		borrow = 0
	}
	var fee uint64
	if borrow > 0 {
		if fee, err = kaminoBorrowFeeAtRate(binary.LittleEndian.Uint64(accountAt(accounts, route.Kamino.DebtReserve).Data[kaminoReserveConfigOffset+40:]), borrow); err != nil {
			return q, err
		}
		// The entry levers only where the live spread rule levers the funded
		// lane: the spread at this borrow (fee included) must reach the first
		// level's.
		if _, levered := leverageUpAllowed(market, leverageLadder[1].level, float64(borrow+fee)); !levered {
			borrow, fee, q.SpreadUnlevered = 0, 0, true
		}
	}
	q.Unlevered = borrow == 0
	var cost entryCost
	if borrow > 0 {
		if cost.Step, err = p.priceStep(ctx, accounts, route, []string{route.Kamino.CollateralReserve}, slot, borrow); err != nil {
			return q, err
		}
		debt, err := prices.debtValue(borrow + fee)
		if err != nil {
			return q, err
		}
		proceeds, err := prices.debtValue(borrow)
		if err != nil {
			return q, err
		}
		q.BorrowReceiveRaw, q.BorrowFeeRaw, q.BorrowValueRaw, q.ProceedsValueRaw = borrow, fee, uint64(debt), uint64(proceeds)
	}
	// The unwind's last step is the executor's atomic exit: withdraw the
	// collateral and sell it to USDC in one transaction.
	reserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return q, err
	}
	receipts, err := payoffReceiptsForLiquidity(reserve.totalLiquiditySF, reserve.collateralMintSupply, bought)
	if err != nil {
		return q, err
	}
	exit, _, err := assembleStep(ctx, p.rpc, p.view, p.client, p.manifest, p.policies, stepState{route: route, collateral: reserve, slot: uint64(slot), topology: []string{route.Kamino.CollateralReserve}},
		SwapCollateralToStableStep, receipts, bought, slot)
	if err != nil {
		return q, err
	}
	boughtValue, err := prices.collateralValue(bought)
	if err != nil {
		return q, err
	}
	cost.EntrySwapLossMicros = int64(equity) - boughtValue
	cost.ExitSwapLossMicros = boughtValue - int64(conversionQuotedOutput(exit.Request))
	// The lane's own transactions besides the up step: the entry swap, the
	// deposit of its guaranteed output and the exit step.
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
	if !position.ObligationPresent {
		if cost.SetupMicros, err = p.setupMicros(ctx, route, blockhash); err != nil {
			return q, err
		}
	}
	if source != nil {
		if cost.ReleaseMicros, err = p.releaseMicros(ctx, o, *source, equity); err != nil {
			return q, err
		}
	}
	// The round trip's swap result is signed; the move's cost is never below
	// zero, the fees it pays whatever the venue.
	fees, err := budgetSum(cost.NetworkMicros, cost.SetupMicros, cost.ReleaseMicros, cost.Step.TotalMicros)
	if err != nil {
		return q, err
	}
	if cost.TotalMicros = fees + cost.EntrySwapLossMicros + cost.ExitSwapLossMicros; cost.TotalMicros < 0 {
		cost.TotalMicros = 0
	}
	q.Cost, q.CostRaw = cost, cost.TotalMicros
	if !q.validBorrow() {
		return q, budgetHold("selector_entry_borrow_invalid")
	}
	// An economic outcome a smaller size can repair (the ladder's).
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

// releaseMicros is the network fee of the legs a rotation's release of
// equity from source sends, built from the tick's batch as the executor
// builds them (target rule 5r): beside debt the repay of its share, then the
// withdrawal of its collateral share as USDC. Their swap loss is the
// source's exit, which staying would pay later: it is not the move's cost.
// Building them bounds the release at this size.
func (p lanePricer) releaseMicros(ctx context.Context, o Observation, source Snapshot, equity uint64) (int64, error) {
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
		if limit := stepLegCap(route); repay > limit-limit/10 {
			repay = limit - limit/10
		}
		receipts, convert, err := planDownStep(source, d, st, repay)
		if err != nil {
			return 0, err
		}
		step, _, err := assembleStep(ctx, p.rpc, p.view, p.client, p.manifest, p.policies, st, SwapCollateralToDebtStep, receipts, convert, source.Slot)
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
