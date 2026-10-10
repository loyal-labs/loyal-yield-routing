package backyard

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// A destination forecast retains its hypothetical amounts separately from the
// observed accounts. It does not claim to have executed or simulated the loop.
type selectorDestinationQuote struct {
	Lane                        string `json:"lane"`
	EquityRaw                   uint64 `json:"equityRaw"`
	AccountSlot                 int64  `json:"accountSlot"`
	AccountsSHA256              string `json:"accountsSha256"`
	InitialCollateralMinimumRaw uint64 `json:"initialCollateralMinimumRaw"`
	BorrowReceiveRaw            uint64 `json:"borrowReceiveRaw"`
	BorrowFeeRaw                uint64 `json:"borrowFeeRaw"`
	// DebtPrice is nil for USDC-debt lanes. A non-USDC debt lane carries the
	// established budget price observation that converts its debt-denominated
	// capacity and borrow quantities into USDC without assuming a peg.
	DebtPrice      *BudgetPrice `json:"debtPrice,omitempty"`
	PayoffUpperRaw uint64       `json:"payoffUpperRaw"`
	// Unlevered marks a B2 1x entry priced while the destination debt reserve
	// blocks borrowing: no borrow, no debt, exit = withdraw -> swap back.
	Unlevered  bool                     `json:"unlevered,omitempty"`
	PayoffSwap JupiterExecutionEvidence `json:"payoffSwap"`
	// PayoffLegs retains EVERY non-USDC payoff swap leg (funding, residue?,
	// return?) as recipe evidence; nil on USDC-debt lanes, whose payoff stays
	// the single PayoffSwap leg. PayoffResidueInputRaw is the guaranteed
	// debt-mint residue input (funding minimum minus maximum repayment), and
	// PayoffReturnAmountRaw the FULL post-repay position remainder sold back
	// to USDC; both zero on USDC-debt lanes.
	PayoffLegs            []JupiterExecutionEvidence `json:"payoffLegs,omitempty"`
	PayoffResidueInputRaw uint64                     `json:"payoffResidueInputRaw,omitempty"`
	PayoffReturnAmountRaw uint64                     `json:"payoffReturnAmountRaw,omitempty"`
	// PayoffResidualReceiptsRaw is an UPPER UNCERTAINTY BOUND, not an observed
	// balance: receipts the deposits could still have minted at the current
	// rate beyond the guaranteed budget. It is retained so the remainder is
	// never deleted or credited at zero cost, but retaining it is NOT cleanup
	// — no AUTO route switch/closure may rely on this forecast until refreshed
	// observed balances, lane attribution and a bounded costed cleanup or a
	// reviewed residual policy exist.
	PayoffResidualReceiptsRaw uint64 `json:"payoffResidualReceiptsRaw,omitempty"`
	// PayoffLeftoverCollateralRaw is a GUARANTEED REMAINDER BOUND, not
	// necessarily the full realized remainder: the collateral custody the
	// one-pass exit provably cannot sell past the repayment bound. It is
	// never claimed as flat exit custody, and never wired on today's evidence.
	PayoffLeftoverCollateralRaw uint64 `json:"payoffLeftoverCollateralRaw,omitempty"`
	// CollateralAssetPrice is the retained observed collateral price evidence
	// behind CollateralAssetUSDCRaw (non-USDC lanes only): production
	// economics revalues the actual redeposited collateral from it, so its
	// identity and validity window bind the quote like the debt price's.
	CollateralAssetPrice *BudgetPrice `json:"collateralAssetPrice,omitempty"`
	// RedepositCollateralRaw is the bounded leverage swap minimum — the
	// collateral raw actually purchased with the borrowed principal.
	RedepositCollateralRaw uint64 `json:"redepositCollateralRaw,omitempty"`
	// CollateralAssetUSDCRaw values the two bounded collateral purchase
	// outputs (entry and leverage swap minima) at the observed collateral
	// price's lower bound — an asset-side estimate, never a liability: the
	// debt price upper stays on the liability side only. nil on USDC-debt
	// lanes keeps legacy USDC parity byte-identical.
	CollateralAssetUSDCRaw *uint64        `json:"collateralAssetUsdcRaw,omitempty"`
	Recipe                 selectorRecipe `json:"recipe"`
}

// selectorDestinationDebtPrice observes a non-USDC debt lane's price through
// the established budget valuation path: one coherent batch of the route debt
// reserve, the pinned USDC reference, both mints and chain Clock, with the
// strict Token-2022 mint parser and +-100bps margins applied there. USDC-debt
// lanes need no conversion and return nil, keeping exact raw==USDC parity.
// borrowCeiling only justifies a nonzero debit request; every conversion
// recomputes from raw quantities against the bound evidence.
func selectorDestinationDebtPrice(ctx context.Context, rpc *chain.Client, view *View, route RuntimeRoute, borrowCeiling, minimumSlot int64) (*BudgetPrice, error) {
	if route.Kamino.DebtMint == bridgeUSDC {
		return nil, nil
	}
	if rpc == nil || borrowCeiling <= 0 || borrowCeiling > math.MaxInt64 || minimumSlot <= 0 {
		return nil, budgetHold("invalid_price_observation_request")
	}
	price, err := ObserveBudgetTokenPrice(ctx, rpc, view, route.Lane, ExecutableDebit{Mint: route.Kamino.DebtMint, TokenProgram: route.DebtTokenProgram, Raw: uint64(borrowCeiling)}, minimumSlot)
	if err != nil {
		return nil, err
	}
	return &price, nil
}

// UserState/FarmState fields match farms-sdk 3.2.14 Borsh layouts, including
// discriminators. A token-loop quote cannot silently assume manual farm setup.
func validateSelectorFarms(route RuntimeRoute, accounts []ConfirmedAccount) error {
	for _, pair := range [][2]string{{route.CollateralFarm, route.ObligationCollateralFarm}, {route.DebtFarm, route.ObligationDebtFarm}} {
		if pair[0] == "" && pair[1] == "" {
			continue
		}
		derived, err := deriveKaminoObligationFarmUserState(pair[0], route.Kamino.Obligation)
		if err != nil || derived != pair[1] {
			return budgetHold("selector_farm_binding_invalid")
		}
		farm, user := accountAt(accounts, pair[0]), accountAt(accounts, pair[1])
		if farm.Owner != kamino.FarmsProgramID.String() || farm.Executable || farm.Lamports == 0 || len(farm.Data) != 8336 || !bytes.Equal(farm.Data[:8], []byte{198, 102, 216, 74, 63, 66, 163, 190}) ||
			!sameKey(farm.Data[7328:7360], route.Kamino.MarketAuthority) || farm.Data[7361] != 0 || farm.Data[7362] != 1 {
			return budgetHold("selector_farm_unavailable")
		}
		if state, err := kamino.DecodeFarmUserState(chainAccount(user, user.Address)); err != nil || !sameKey(state.FarmState[:], pair[0]) ||
			!sameKey(state.Owner[:], bridgeVault) || !state.IsFarmDelegated || !sameKey(state.Delegatee[:], route.Kamino.Obligation) {
			return budgetHold("selector_farm_registration_unavailable")
		}
	}
	return nil
}

func selectorDestinationAccounts(ctx context.Context, rpc *chain.Client, view *View, route RuntimeRoute, minimumSlot int64) (int64, []ConfirmedAccount, KaminoPosition, error) {
	var empty KaminoPosition
	slot, accounts, position, err := observeSelectorDestinationBatch(ctx, rpc, view, route, minimumSlot)
	if err != nil {
		return 0, nil, empty, err
	}
	if position.HasPosition || position.CollateralDepositedRaw != 0 || position.DebtRaw != 0 {
		return 0, nil, empty, budgetHold("selector_destination_not_flat")
	}
	custody, err := validateSelectorDestinationCommon(route, slot, accounts, position)
	if err != nil {
		return 0, nil, empty, err
	}
	if custody != 0 {
		return 0, nil, empty, budgetHold("selector_destination_not_flat")
	}
	return slot, accounts, position, nil
}

// observeSelectorDestinationBatch fetches the full destination batch and the
// lane position, tolerating an absent optional
// obligation/farm, and retries once against the closed unsigned
// reserve-refresh simulation when stale.
func observeSelectorDestinationBatch(ctx context.Context, rpc *chain.Client, view *View, route RuntimeRoute, minimumSlot int64) (int64, []ConfirmedAccount, KaminoPosition, error) {
	var empty KaminoPosition
	addresses := []string{route.Kamino.Market, route.Kamino.Obligation, route.Kamino.CollateralReserve, route.Kamino.DebtReserve,
		route.Kamino.CollateralMint, route.Kamino.DebtMint, route.CollateralCustody, route.DebtCustody, route.CollateralLiquiditySupply,
		route.DebtLiquiditySupply, route.DebtFeeReceiver, route.CollateralReceiptMint, route.CollateralReceiptSupply,
		route.CollateralFarm, route.ObligationCollateralFarm, route.DebtFarm, route.ObligationDebtFarm,
		budgetClockAddress, bridgeVault, bridgeDelegate, bridgeStrategy, reportTicketPDA}
	optional := uniqueNonzero([]string{route.Kamino.Obligation, route.ObligationCollateralFarm, route.ObligationDebtFarm})
	slot, accounts, _, err := view.read(ctx, uniqueNonzero(addresses), minimumSlot, optional...)
	if err != nil {
		return 0, nil, empty, err
	}
	position, err := observeKaminoFromFixedAccounts(slot, accounts, route.Kamino)
	if errors.Is(err, errKaminoReserveStale) {
		// Idle reserves only advance through the permissionless refresh the
		// production prefix already carries, so re-observe against its closed
		// unsigned simulation — never a broadcast — over the full original
		// batch. bridgeDelegate's captured lamports differ by the simulated
		// fee; this snapshot is unsigned evidence, and every freshness,
		// ownership, capacity and funding check below still runs fail-closed
		// on it as-is.
		simulatedSlot, simulatedAccounts, refreshErr := simulateBudgetReserveRefreshOptional(ctx, rpc, route.Lane, uniqueNonzero(addresses), optional, slot)
		if refreshErr != nil {
			return 0, nil, empty, refreshErr
		}
		slot, accounts = simulatedSlot, simulatedAccounts
		position, err = observeKaminoFromFixedAccounts(slot, accounts, route.Kamino)
	}
	if err != nil {
		return 0, nil, empty, err
	}
	return slot, accounts, position, nil
}

// validateSelectorDestinationCommon runs every destination check that does not
// depend on flatness, and returns the observed collateral custody balance so
// each caller applies its own flatness contract.
func validateSelectorDestinationCommon(route RuntimeRoute, slot int64, accounts []ConfirmedAccount, position KaminoPosition) (uint64, error) {
	if _, err := decodeObservedAdaptorConfig(accountAt(accounts, bridgeStrategy)); err != nil {
		return 0, err
	}
	ticket, err := decodeObservedReportTicket(accountAt(accounts, reportTicketPDA))
	if err != nil || ticket.Armed || ticket.LastConsumedSequence >= uint64(slot) {
		return 0, budgetHold("selector_destination_ticket_unavailable")
	}
	if err = validateSelectorFarms(route, accounts); err != nil {
		return 0, err
	}
	for _, side := range []struct {
		reserve, mint, program, supply, farm string
		decimals                             uint8
	}{
		{route.Kamino.CollateralReserve, route.Kamino.CollateralMint, route.CollateralTokenProgram, route.CollateralLiquiditySupply, route.CollateralFarm, position.CollateralDecimals},
		{route.Kamino.DebtReserve, route.Kamino.DebtMint, route.DebtTokenProgram, route.DebtLiquiditySupply, route.DebtFarm, position.DebtDecimals},
	} {
		a := accountAt(accounts, side.reserve)
		farmOffset := 64
		if side.reserve == route.Kamino.DebtReserve {
			farmOffset = 96
		}
		farmKey := side.farm
		if farmKey == "" {
			farmKey = "11111111111111111111111111111111"
		}
		if !sameKey(a.Data[160:192], side.supply) || !sameKey(a.Data[408:440], side.program) || !sameKey(a.Data[farmOffset:farmOffset+32], farmKey) {
			return 0, budgetHold("selector_destination_reserve_binding_changed")
		}
		if err = validateExecutionMint(accountAt(accounts, side.mint), side.program, side.decimals); err != nil {
			return 0, err
		}
	}
	c := accountAt(accounts, route.Kamino.CollateralReserve)
	d := accountAt(accounts, route.Kamino.DebtReserve)
	if !sameKey(c.Data[2560:2592], route.CollateralReceiptMint) || !sameKey(c.Data[2600:2632], route.CollateralReceiptSupply) || !sameKey(d.Data[192:224], route.DebtFeeReceiver) {
		return 0, budgetHold("selector_destination_reserve_binding_changed")
	}
	receipt := accountAt(accounts, route.CollateralReceiptMint)
	if receipt.Owner != classicTokenProgram || receipt.Executable || receipt.Lamports == 0 || len(receipt.Data) != 82 || receipt.Data[45] != 1 || binary.LittleEndian.Uint32(receipt.Data[:4]) != 1 || !sameKey(receipt.Data[4:36], route.Kamino.MarketAuthority) {
		return 0, budgetHold("selector_destination_receipt_mint_unavailable")
	}
	collateralCustody := uint64(0)
	for _, b := range []struct{ address, mint, authority, program string }{
		{route.CollateralCustody, route.Kamino.CollateralMint, bridgeVault, route.CollateralTokenProgram},
		{route.DebtCustody, route.Kamino.DebtMint, bridgeVault, route.DebtTokenProgram},
		{route.CollateralLiquiditySupply, route.Kamino.CollateralMint, route.Kamino.MarketAuthority, route.CollateralTokenProgram},
		{route.DebtLiquiditySupply, route.Kamino.DebtMint, route.Kamino.MarketAuthority, route.DebtTokenProgram},
		{route.DebtFeeReceiver, route.Kamino.DebtMint, route.Kamino.MarketAuthority, route.DebtTokenProgram},
		{route.CollateralReceiptSupply, route.CollateralReceiptMint, route.Kamino.MarketAuthority, route.CollateralTokenProgram},
	} {
		a := accountAt(accounts, b.address)
		custody, err := DecodeTokenCustody(a.Owner, a.Data, mustKey(b.mint), mustKey(b.authority))
		if err != nil || a.Owner != b.program || a.Executable || a.Lamports == 0 {
			return 0, budgetHold("selector_destination_custody_unavailable")
		}
		if b.address == route.CollateralCustody {
			collateralCustody = custody.Raw
		}
	}
	return collateralCustody, nil
}

// observeSelectorDestinationCandidate is the narrow internal reviewed-manifest
// candidate entry point for the AUTO lane. It prices the exact same entry
// graph as the public selector path — every size, capacity, account,
// freshness and economics check unchanged. The public
// observeSelectorDestination gate above stays strictly selectorLane; adding
// AUTO to the reviewed lane set remains a coordinator-owned admission decision.
func observeSelectorDestinationCandidate(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, m RouteManifest, policies installedPolicies, equity uint64, sampleSlot int64) (selectorDestinationQuote, error) {
	return observeSelectorDestinationForecastAuthorized(ctx, rpc, view, client, m, policies, autoAUTOPYUSD.Lane, equity, sampleSlot, false, nil)
}

// observeSelectorDestinationCandidateReentry is the narrow candidate reentry
// forecast: the identical shared body and checks behind the entry above,
// carrying the validated source exit bound so the funded same-lane position's
// bounded recreation can be priced from that supplied bound. The bound's own
// exit economics stay the source quote's and are not re-proved here — this is
// not a proved complete unwind. It exists solely for that forecast — the
// public reentry wrapper keeps refusing AUTO, and the execution prestate
// stays strictly absent-only.
func observeSelectorDestinationCandidateReentry(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, m RouteManifest, policies installedPolicies, equity uint64, sampleSlot int64, reentry *selectorReentryForecast) (selectorDestinationQuote, error) {
	if reentry == nil {
		return selectorDestinationQuote{Lane: autoAUTOPYUSD.Lane, EquityRaw: equity}, budgetHold("invalid_selector_destination")
	}
	return observeSelectorDestinationForecastAuthorized(ctx, rpc, view, client, m, policies, autoAUTOPYUSD.Lane, equity, sampleSlot, false, reentry)
}

// observeSelectorDestinationForecast prices the real one-pass entry graph from
// a flat destination, or — with reentry set — the same graph as the recreated
// position after the bound source exit closes the funded one. Future balances
// stay explicit scalars; no invented account image reaches an RPC simulation
// or execution admission. Its gate is unchanged: strictly the reviewed
// selector lane set, exactly as before the candidate entry existed.
func observeSelectorDestinationForecast(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, m RouteManifest, policies installedPolicies, lane string, equity uint64, sampleSlot int64, clampCapacity bool, reentry *selectorReentryForecast) (selectorDestinationQuote, error) {
	out := selectorDestinationQuote{Lane: lane, EquityRaw: equity}
	if !selectorLane(lane) {
		return out, budgetHold("invalid_selector_destination")
	}
	return observeSelectorDestinationForecastAuthorized(ctx, rpc, view, client, m, policies, lane, equity, sampleSlot, clampCapacity, reentry)
}

// observeSelectorDestinationForecastAuthorized is the shared body behind the
// unchanged wrapper above: the identical entry graph, entered either through a
// reviewed selector lane or — solely through the explicit candidate entry —
// through the manifest-bound AUTO authorization. No other path reaches it
// with a non-selector lane.
func observeSelectorDestinationForecastAuthorized(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, m RouteManifest, policies installedPolicies, lane string, equity uint64, sampleSlot int64, clampCapacity bool, reentry *selectorReentryForecast) (selectorDestinationQuote, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out := selectorDestinationQuote{Lane: lane, EquityRaw: equity}
	if rpc == nil || client == nil || !selectorOrAutoLane(lane) || equity == 0 || equity > strategyTwoBridgeLegCapRaw || sampleSlot <= 0 || sampleSlot > math.MaxInt64-budgetMaxObservationLagCeilingSlots {
		return out, budgetHold("invalid_selector_destination")
	}
	route, _ := runtimeRoute(lane)
	var slot int64
	var accounts []ConfirmedAccount
	var position KaminoPosition
	var err error
	if reentry == nil {
		slot, accounts, position, err = selectorDestinationAccounts(ctx, rpc, view, route, sampleSlot)
	} else {
		slot, accounts, position, err = selectorReentryDestinationAccounts(ctx, rpc, view, route, sampleSlot, reentry.bound, reentry.collateralIdle)
	}
	if err != nil {
		return out, err
	}
	if !selectorOrAutoLane(route.Lane) {
		return out, fmt.Errorf("pair_capacity_lane_unreviewed")
	}
	capacity, err := kaminoPairEntryCapacityAuthorized(position, accounts, route)
	if err != nil {
		return out, err
	}
	// AUTO/OnRe retain collateral equity independently of additional debt room.
	unlevered := false
	// Pair capacity is DEBT-denominated. A non-USDC debt lane converts it
	// downwards at the established budget price observation before the result
	// may bound a USDC equity; USDC lanes keep exact raw==USDC parity.
	if capacity > 0 {
		debtPrice, err := selectorDestinationDebtPrice(ctx, rpc, view, route, int64(capacity/2), sampleSlot)
		if err != nil {
			return out, err
		}
		out.DebtPrice = debtPrice
		if debtPrice != nil {
			bounded, err := debtPrice.valueLower(capacity, route.Kamino.DebtMint, route.DebtTokenProgram, debtPrice.ObservedSlot)
			if err != nil || bounded <= 0 {
				return out, budgetHold("selector_destination_capacity_unpriced")
			}
			capacity = uint64(bounded)
		}
	}
	if clampCapacity {
		equity = min(equity, capacity)
		out.EquityRaw = equity
	}
	if equity == 0 || equity > capacity || min(position.LiquidationThresholdBPS-1500, 6000) <= TargetLTVBPS {
		return out, budgetHold("selector_destination_capacity_unavailable")
	}
	out.AccountSlot, out.AccountsSHA256 = slot, hashConfirmedAccounts(accounts)
	blockhash, err := latestBlockhash(ctx, rpc)
	if err != nil {
		return out, err
	}
	observationFloor := slot
	var inputs []*phase3BuildInput
	appendInput := func(request any, effects ExpectedEffects) error {
		raw, err := jsonMarshalExpectedEffects(effects)
		if err != nil {
			return err
		}
		input, err := encodePhase3BuildInput(request, raw)
		if err != nil {
			return err
		}
		inputs = append(inputs, input)
		return nil
	}
	// A reentry recipe always recreates the obligation: the bound source exit
	// closes the currently funded one, so its rent and exact initializer fee
	// stay in this quote even though the account is observed present. Future
	// rent refunds are not spendable, so the funding checks below still see
	// the full recreation rent.
	if !position.ObligationPresent || reentry != nil {
		rent, err := rpc.RentExempt(ctx, kamino.ObligationSize)
		if err != nil {
			return out, unavailable(err)
		}
		r, err := m.initializationRequest(policies, lane, blockhash, rent, 1)
		if err != nil {
			return out, err
		}
		// The manifest-aware forms keep compile and both prestates on the SAME
		// explicit manifest that produced the request: installed lanes take the
		// exact public path, and the execution admission wrapper above stays
		// strictly absent-only.
		message, err := m.compileKaminoInitializationMessage(r)
		if err != nil {
			return out, err
		}
		fee, err := observeMessageFee(ctx, rpc, message, slot)
		if err != nil {
			return out, err
		}
		r.MaximumFeeLamports = fee.Lamports
		var initSlot int64
		if reentry != nil && position.ObligationPresent {
			// Forecast-only prestate: the initializer is priced against the
			// exact observed funded obligation this exit will close. The
			// execution admission wrapper stays strictly absent-only.
			if initSlot, err = m.validateKaminoReentryForecastPrestate(ctx, view, r, sampleSlot, reentry.bound); err != nil {
				return out, err
			}
		} else if initSlot, err = m.validateKaminoInitializationPrestate(ctx, view, r, sampleSlot); err != nil {
			return out, err
		}
		observationFloor = max(slot, initSlot)
		if observationFloor > sampleSlot+observationLagSlots() {
			return out, budgetHold("selector_recipe_observation_expired")
		}
		if err = appendInput(r, ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &r}); err != nil {
			return out, err
		}
	}
	// Fixed-width report fields below are fee templates, never NAV evidence.
	// Allocation updates NAV itself; an extra report is conservative coverage.
	report := BridgeReport{Sequence: uint64(slot), ObservedSlot: uint64(slot), NAVAfterRaw: equity, SnapshotDigest: out.AccountsSHA256}
	appendBridge := func(action Action, amount, idle, strategy, squads uint64) error {
		d := Decision{Action: action, AmountRaw: int64(amount)}
		e, _, _, err := bridgeExpectedEffects(d, idle, strategy, squads)
		if err != nil {
			return err
		}
		e.Kind, e.ReturnData = "bridge", expectedAdaptorReturnData(equity)
		policy, err := policies.account(policyKey{action: action})
		if err != nil {
			return err
		}
		r := BridgeBuildRequest{Action: action, AmountRaw: amount, Report: report, Policy: policy, AdaptorConfig: bridgeStrategy, Settings: bridgeSettings, RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight}
		return appendInput(r, e)
	}
	if err = appendBridge(VoltrAllocateToSquads, equity, equity, 0, 0); err != nil {
		return out, err
	}
	if err = appendBridge(ReportNAV, 0, 0, 0, equity); err != nil {
		return out, err
	}
	swap, err := prepareJupiterQuoteEvidence(ctx, rpc, client, m, policies, Decision{Action: SwapStableToCollateralStep, AmountRaw: int64(equity), StrategyKey: lane}, equity, 0, observationFloor)
	if err != nil {
		return out, err
	}
	observationFloor, err = selectorSwapObservationFloor(swap.Request, sampleSlot, observationFloor)
	if err != nil {
		return out, err
	}
	if err = appendInput(swap.Request, swap.ExpectedEffects); err != nil {
		return out, err
	}
	if err = appendBridge(ReportNAV, 0, 0, 0, 0); err != nil {
		return out, err
	}
	out.InitialCollateralMinimumRaw = swap.Request.MinimumOutputRaw
	rounding, err := selectorReceiptRounding(accounts, route, slot)
	if err != nil {
		return out, err
	}
	if swap.Request.MinimumOutputRaw <= rounding+1 {
		return out, budgetHold("selector_destination_below_deposit_minimum")
	}
	minimum := swap.Request.MinimumOutputRaw - rounding
	liquidity := binary.LittleEndian.Uint64(accountAt(accounts, route.CollateralLiquiditySupply).Data[64:72])
	deposit := func(amount, minDebit, beforeSupply uint64, reserves []string) error {
		if amount > math.MaxUint64-beforeSupply {
			return budgetHold("selector_destination_amount_overflow")
		}
		r, err := m.kaminoPacketForRoute(policies, OpenRouteStep, kaminoLegDeposit, amount, blockhash, lane)
		if err != nil {
			return err
		}
		r.ObligationReserves = reserves
		e := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-deposit", Conserved: true, Deposit: &ExpectedDeposit{minDebit, amount}, Accounts: []ExpectedAccountEffect{
			{Address: route.CollateralCustody, Owner: route.CollateralTokenProgram, Mint: route.Kamino.CollateralMint, Authority: bridgeVault, BeforeRaw: amount, AfterRaw: 0},
			{Address: route.CollateralLiquiditySupply, Owner: route.CollateralTokenProgram, Mint: route.Kamino.CollateralMint, Authority: route.Kamino.MarketAuthority, BeforeRaw: beforeSupply, AfterRaw: beforeSupply + amount},
		}}
		return appendInput(r, e)
	}
	var borrow, fee uint64
	if leverageLane(route.Lane) {
		// Explicit future collateral, not a fabricated observed account image.
		funded := position
		funded.CollateralDepositedRaw, funded.RedeemablePrimeRaw, funded.DebtRaw = 1, minimum-1, 0
		borrow, err = capacitySizedBorrow(funded, accounts, route, 150)
		if err != nil {
			return out, err
		}
		depositRoom, err := unleveredEntryCapacityDebtRaw(position, accounts, route)
		if err != nil {
			return out, err
		}
		initialDebtUnits, err := valueBetweenTokenRaw(swap.Request.MinimumOutputRaw, position.CollateralDecimals, position.DebtDecimals, position.CollateralPriceSF, position.DebtPriceSF, true)
		if err != nil {
			return out, err
		}
		if initialDebtUnits >= depositRoom {
			borrow = 0
		} else {
			borrow = min(borrow, depositRoom-initialDebtUnits)
		}
		if borrow < leverageMinimumBorrowRaw {
			borrow = 0
		}
		unlevered, out.Unlevered = borrow == 0, borrow == 0
	} else {
		borrow, err = targetBorrowForCollateralRaw(minimum-1, position.CollateralDecimals, position.DebtDecimals, position.CollateralPriceSF, position.DebtPriceSF)
		if err != nil {
			return out, err
		}
	}
	var payoffLegs []JupiterExecutionEvidence
	if unlevered {
		if err = appendSelectorUnleveredTail(ctx, rpc, client, m, policies, route, accounts, slot, observationFloor, blockhash, swap, minimum, liquidity, deposit, appendInput, appendBridge, &out); err != nil {
			return out, err
		}
	} else {
		fee, err = kaminoBorrowFeeAtRate(binary.LittleEndian.Uint64(accountAt(accounts, route.Kamino.DebtReserve).Data[kaminoReserveConfigOffset+40:]), borrow)
		if err != nil {
			return out, err
		}
		out.BorrowReceiveRaw, out.BorrowFeeRaw = borrow, fee
		// Amount-specific origination rounding can close a tiny tranche even when
		// the reserve's maximum-size capacity is positive.
		debtValue, err := valueBetweenTokenRaw(borrow+fee, position.DebtDecimals, position.CollateralDecimals, position.DebtPriceSF, position.CollateralPriceSF, true)
		allowed := new(big.Int).Mul(new(big.Int).SetUint64(minimum-1), new(big.Int).SetUint64(uint64(accountAt(accounts, route.Kamino.CollateralReserve).Data[kaminoLoanToValueOffset])))
		allowed.Quo(allowed, big.NewInt(100))
		if err != nil || new(big.Int).SetUint64(debtValue).Cmp(allowed) > 0 {
			return out, budgetHold("selector_destination_initial_borrow_unsafe")
		}
		if err = deposit(swap.Request.MinimumOutputRaw, minimum, liquidity, nil); err != nil {
			return out, err
		}
		if err = appendBridge(ReportNAV, 0, 0, 0, 0); err != nil {
			return out, err
		}
		r, err := m.kaminoPacketForRoute(policies, OpenRouteStep, kaminoLegBorrow, borrow, blockhash, lane)
		if err != nil {
			return out, err
		}
		r.ObligationReserves = []string{route.Kamino.CollateralReserve}
		supply := binary.LittleEndian.Uint64(accountAt(accounts, route.DebtLiquiditySupply).Data[64:72])
		feeBalance := binary.LittleEndian.Uint64(accountAt(accounts, route.DebtFeeReceiver).Data[64:72])
		if borrow+fee > supply || fee > math.MaxUint64-feeBalance {
			return out, budgetHold("selector_destination_borrow_liquidity_unavailable")
		}
		e := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-borrow", Conserved: true, Accounts: []ExpectedAccountEffect{
			{Address: route.DebtLiquiditySupply, Owner: route.DebtTokenProgram, Mint: route.Kamino.DebtMint, Authority: route.Kamino.MarketAuthority, BeforeRaw: supply, AfterRaw: supply - borrow - fee},
			{Address: route.DebtCustody, Owner: route.DebtTokenProgram, Mint: route.Kamino.DebtMint, Authority: bridgeVault, BeforeRaw: 0, AfterRaw: borrow},
			{Address: route.DebtFeeReceiver, Owner: route.DebtTokenProgram, Mint: route.Kamino.DebtMint, Authority: route.Kamino.MarketAuthority, BeforeRaw: feeBalance, AfterRaw: feeBalance + fee},
		}}
		if err = appendInput(r, e); err != nil {
			return out, err
		}
		if err = appendBridge(ReportNAV, 0, 0, 0, borrow); err != nil {
			return out, err
		}
		leverage, err := prepareJupiterQuoteEvidence(ctx, rpc, client, m, policies, Decision{Action: SwapDebtToCollateralStep, AmountRaw: int64(borrow), StrategyKey: lane}, borrow, 0, observationFloor)
		if err != nil {
			return out, err
		}
		observationFloor, err = selectorSwapObservationFloor(leverage.Request, sampleSlot, observationFloor)
		if err != nil {
			return out, err
		}
		if err = appendInput(leverage.Request, leverage.ExpectedEffects); err != nil {
			return out, err
		}
		if err = appendBridge(ReportNAV, 0, 0, 0, 0); err != nil {
			return out, err
		}
		if leverage.Request.MinimumOutputRaw <= rounding+1 {
			return out, budgetHold("selector_destination_below_deposit_minimum")
		}
		if leverageLane(route.Lane) {
			reserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
			if err != nil {
				return out, err
			}
			used, err := ceilScaledBigFraction(reserve.totalLiquiditySF)
			total := new(big.Int).Add(new(big.Int).SetUint64(swap.Request.MinimumOutputRaw), new(big.Int).SetUint64(leverage.Request.MinimumOutputRaw))
			total.Add(total, new(big.Int).SetUint64(used))
			if err != nil || total.Cmp(new(big.Int).SetUint64(reserve.depositLimitRaw)) > 0 {
				return out, budgetHold("selector_redeposit_capacity_unavailable")
			}
		}
		redepositMinimum := leverage.Request.MinimumOutputRaw - rounding
		if err = deposit(leverage.Request.MinimumOutputRaw, redepositMinimum, liquidity+swap.Request.MinimumOutputRaw, []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}); err != nil {
			return out, err
		}
		if err = appendBridge(ReportNAV, 0, 0, 0, 0); err != nil {
			return out, err
		}
		payoff, err := selectorDestinationExit(ctx, rpc, client, m, policies, route, accounts, position, slot, observationFloor, swap.Request.MinimumOutputRaw, leverage.Request.MinimumOutputRaw, minimum, redepositMinimum, borrow, fee, rounding)
		if err != nil {
			return out, err
		}
		out.PayoffUpperRaw, out.PayoffSwap = payoff.DebtUpperRaw, payoff.Funding
		out.PayoffLegs, out.PayoffResidueInputRaw, out.PayoffReturnAmountRaw = payoff.Legs, payoff.ResidueInputRaw, payoff.ReturnAmountRaw
		// The one-pass exit is bounded, not flat: the two retained fields are the
		// residual bounds (an upper uncertainty and a guaranteed remainder), never
		// observed balances — complete cleanup stays explicitly gated behind
		// refreshed balances, attribution and a reviewed residual policy.
		out.PayoffResidualReceiptsRaw, out.PayoffLeftoverCollateralRaw = payoff.ResidualReceiptsRaw, payoff.CustodyLeftoverRaw
		// Asset-side correction for non-USDC lanes (doc 12): the two bounded
		// collateral purchase outputs valued at the observed collateral price
		// lower bound. The debt price never lifts the asset side. The collateral
		// price evidence itself is retained on the quote so production economics
		// can revalue the actual redeposited collateral, and its validity window
		// intersects the quote's like the debt price's.
		if route.Kamino.DebtMint != bridgeUSDC {
			deposited, err := budgetSumU64(swap.Request.MinimumOutputRaw, leverage.Request.MinimumOutputRaw)
			if err != nil {
				return out, err
			}
			assetPrice, err := ObserveBudgetTokenPrice(ctx, rpc, view, lane, ExecutableDebit{Source: route.CollateralCustody, Mint: route.Kamino.CollateralMint, TokenProgram: route.CollateralTokenProgram, Raw: deposited}, sampleSlot)
			if err != nil {
				return out, err
			}
			asset, err := assetPrice.valueLower(deposited, route.Kamino.CollateralMint, route.CollateralTokenProgram, assetPrice.ObservedSlot)
			if err != nil || asset <= 0 {
				return out, budgetHold("selector_destination_collateral_asset_unpriced")
			}
			assetRaw := uint64(asset)
			out.CollateralAssetUSDCRaw = &assetRaw
			out.CollateralAssetPrice = copyDebtPrice(&assetPrice)
			out.RedepositCollateralRaw = leverage.Request.MinimumOutputRaw
			// Every payoff step between the entry tail and the residue/return
			// swaps — funding withdrawal, repay, post-repay withdrawal, the NAV
			// reports between them, the custody return and the idle restore —
			// enters the retained recipe evidence through the real pricing
			// consumer below. The recipe ledger continues from the leverage
			// deposit, which emptied the collateral custody: observed unrelated
			// custody dust never enters.
			state := selectorPayoffTemplateState{
				blockhash:    blockhash,
				liquidityRaw: liquidity, debtSupplyRaw: supply - borrow - fee,
				entryDeposit: swap.Request.MinimumOutputRaw, redepositDeposit: leverage.Request.MinimumOutputRaw,
				borrow: borrow, fee: fee, rounding: rounding,
			}
			if err = appendSelectorPayoffRecipeInputs(m, policies, route, blockhash, accounts, payoff, state, appendInput, appendBridge); err != nil {
				return out, err
			}
		}
		payoffLegs = payoff.Legs
	}
	// The bounded compounding horizons cover the retained recipe exactly; a
	// structural drift holds the lane instead of silently understating debt
	// accrual or receipt rounding.
	if int64(len(inputs)) > selectorFullRecipeWindows {
		return out, budgetHold("selector_destination_recipe_window_mismatch")
	}
	observationFloor, err = selectorSwapObservationFloor(out.PayoffSwap.Request, sampleSlot, observationFloor)
	if err != nil {
		return out, err
	}
	observationFloor, err = selectorPayoffObservationFloor(sampleSlot, observationFloor, payoffLegs...)
	if err != nil {
		return out, err
	}
	out.Recipe, err = m.priceSelectorRecipeWithFloor(ctx, rpc, view, lane, inputs, sampleSlot, observationFloor)
	if err != nil {
		return out, err
	}
	for _, funding := range []struct {
		address string
		amount  uint64
	}{{bridgeDelegate, out.Recipe.NetworkLamports}, {bridgeVault, out.Recipe.SetupLamports}} {
		a := accountAt(accounts, funding.address)
		if a.Owner != "11111111111111111111111111111111" || a.Executable || len(a.Data) != 0 || a.Lamports < funding.amount {
			return out, budgetHold("selector_destination_native_funding_unavailable")
		}
	}
	// Bind readiness and hypothetical sizing to the retained exact recipe.
	raw, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	out.Recipe.EvidenceID = sha256Bytes(raw)
	return out, nil
}

// selectorPayoffTemplateState carries the observed reserve baselines and
// guaranteed deposit bounds the payoff step templates chain from. Every
// amount is an explicit future scalar for a cost-only forecast — never a
// simulated RPC account image — and the custody ledger continues from the
// leverage deposit, which emptied the collateral custody.
type selectorPayoffTemplateState struct {
	blockhash        LatestBlockhash
	liquidityRaw     uint64 // observed collateral liquidity supply before the entry deposits
	debtSupplyRaw    uint64 // observed debt liquidity supply after the entry borrow
	entryDeposit     uint64 // guaranteed entry swap minimum, fully deposited
	redepositDeposit uint64 // guaranteed leverage swap minimum, fully deposited
	borrow           uint64
	fee              uint64
	rounding         uint64
}

// patchConfirmedTokenRaw copies accounts and rewrites one address's raw
// balance so exact effect builders can validate against the projected
// conserved ledger instead of the stale observation.
func patchConfirmedTokenRaw(accounts []ConfirmedAccount, address string, raw uint64) []ConfirmedAccount {
	out := append([]ConfirmedAccount(nil), accounts...)
	for i, a := range out {
		if a.Address != address || len(a.Data) < 72 {
			continue
		}
		out[i].Data = append([]byte(nil), a.Data...)
		binary.LittleEndian.PutUint64(out[i].Data[64:72], raw)
		return out
	}
	return out
}

// appendSelectorPayoffRecipeInputs retains every payoff step between the
// entry tail and the residue/return swap legs — the funding withdrawal, a NAV
// report, the repay, another NAV, the post-repay position withdrawal, another
// NAV, the residue/return conversions, the custody return of the guaranteed
// USDC proceeds and the idle restore that lands them back in vault idle —
// through the same recipe consumer as every entry step. The withdrawal ledger
// is receipt-exact and replayed from the payoff's conserved bounds: wires are
// receipt counts inside the owned receipt budget (funding ceiled to guarantee
// the sized input, the return wire the exact remainder), effects are the
// floored liquidity those receipts redeem, and quote inputs are the
// guaranteed ledger custody, never a padded balance. USDC-debt lanes carry no
// payoff legs and keep their existing single-leg proof.
func appendSelectorPayoffRecipeInputs(m RouteManifest, policies installedPolicies, route RuntimeRoute, blockhash LatestBlockhash, accounts []ConfirmedAccount, payoff selectorDestinationPayoff, state selectorPayoffTemplateState, appendInput func(any, ExpectedEffects) error, appendBridge func(Action, uint64, uint64, uint64, uint64) error) error {
	if len(payoff.Legs) == 0 || payoff.DebtUpperRaw == 0 || payoff.FundingReceiptsRaw == 0 || payoff.FundingLiquidityRaw == 0 {
		return budgetHold("selector_destination_payoff_recipe_empty")
	}
	sized := payoff.Funding.Request.AmountRaw
	deposited, err := budgetSumU64(state.entryDeposit, state.redepositDeposit)
	if err != nil {
		return err
	}
	supplyAfterDeposits, err := budgetSumU64(state.liquidityRaw, deposited)
	if err != nil {
		return err
	}
	totalReceipts, err := budgetSumU64(payoff.FundingReceiptsRaw, payoff.ReturnReceiptsRaw)
	if err != nil {
		return err
	}
	redeemed, err := budgetSumU64(payoff.FundingLiquidityRaw, payoff.ReturnAmountRaw)
	if err != nil {
		return err
	}
	// Conservation replays exactly: both wires together consume the whole
	// owned receipt budget once, and the guaranteed redemptions of that one
	// budget can never exceed the deposited liquidity.
	if totalReceipts != payoff.OwnedReceiptsRaw || redeemed > deposited ||
		payoff.CustodyLeftoverRaw != payoff.FundingLiquidityRaw-sized ||
		payoff.FundingLiquidityRaw < sized || payoff.Funding.Request.MinimumOutputRaw < payoff.DebtUpperRaw ||
		payoff.FundingLiquidityRaw > supplyAfterDeposits || redeemed > supplyAfterDeposits {
		return budgetHold("selector_destination_payoff_ledger_broken")
	}
	withdraw := func(wire, liquidity, supplyBefore, custodyBefore uint64) error {
		request, err := m.kaminoPacketForRoute(policies, DeleverRouteStep, kaminoLegWithdraw, wire, blockhash, route.Lane)
		if err != nil {
			return err
		}
		request.ObligationReserves = []string{route.Kamino.CollateralReserve}
		source, destination := kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
		projected := patchConfirmedTokenRaw(accounts, route.CollateralLiquiditySupply, supplyBefore)
		projected = patchConfirmedTokenRaw(projected, route.CollateralCustody, custodyBefore)
		effects, err := exactKaminoTokenEffects(projected, source, destination, liquidity)
		if err != nil {
			return err
		}
		return appendInput(request, effects)
	}
	// Funding withdrawal: the receipt wire whose floored redemption guarantees
	// the sized collateral input.
	if err = withdraw(payoff.FundingReceiptsRaw, payoff.FundingLiquidityRaw, supplyAfterDeposits, 0); err != nil {
		return err
	}
	if err = appendBridge(ReportNAV, 0, 0, 0, 0); err != nil {
		return err
	}
	if err = appendInput(payoff.Funding.Request, payoff.Funding.ExpectedEffects); err != nil {
		return err
	}
	// The repay burns the compounded debt upper from the funding minimum
	// output; the guaranteed residue stays in the debt custody for its own
	// conversion leg. Repayment bounds stay [borrow+fee, debt upper]: the
	// minimum is the entry principal plus origination fee, the maximum the
	// finite compounding upper.
	repaySnapshot := patchConfirmedTokenRaw(accounts, route.DebtCustody, payoff.Funding.Request.MinimumOutputRaw)
	repaySnapshot = patchConfirmedTokenRaw(repaySnapshot, route.DebtLiquiditySupply, state.debtSupplyRaw)
	repay, err := m.kaminoPacketForRoute(policies, DeleverRouteStep, kaminoLegRepay, payoff.DebtUpperRaw, blockhash, route.Lane)
	if err != nil {
		return err
	}
	repay.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	repaySource, repayDestination := kaminoLegCustodiesForRoute(kaminoLegRepay, route)
	repayEffects, err := boundedKaminoRepaymentEffects(repaySnapshot, repaySource, repayDestination, state.borrow+state.fee, payoff.DebtUpperRaw)
	if err != nil {
		return err
	}
	if err = appendInput(repay, repayEffects); err != nil {
		return err
	}
	if err = appendBridge(ReportNAV, 0, 0, 0, 0); err != nil {
		return err
	}
	// Post-repay position withdrawal: the EXACT remaining owned receipts, at
	// their guaranteed floored redemption. Custody continues from the funding
	// leg's guaranteed leftover only.
	if payoff.ReturnAmountRaw > 0 {
		if err = withdraw(payoff.ReturnReceiptsRaw, payoff.ReturnAmountRaw, supplyAfterDeposits-payoff.FundingLiquidityRaw, payoff.CustodyLeftoverRaw); err != nil {
			return err
		}
		if err = appendBridge(ReportNAV, 0, 0, 0, 0); err != nil {
			return err
		}
	}
	// Residue and return legs: only guaranteed minimums count. Their USDC
	// outputs land in the Squads custody the entry tail emptied, so the stage
	// returns exactly the guaranteed proceeds and nothing invented, and the
	// restore sweeps that same proceeds back to vault idle.
	proceeds := uint64(0)
	for _, leg := range payoff.Legs[1:] {
		if err = appendInput(leg.Request, leg.ExpectedEffects); err != nil {
			return err
		}
		if proceeds, err = budgetSumU64(proceeds, leg.Request.MinimumOutputRaw); err != nil {
			return err
		}
	}
	if proceeds > 0 {
		if err = appendBridge(StageSquadsToVoltr, proceeds, 0, 0, proceeds); err != nil {
			return err
		}
		if err = appendBridge(VoltrRestoreIdle, proceeds, 0, proceeds, 0); err != nil {
			return err
		}
	}
	return appendBridge(ReportNAV, 0, 0, 0, 0)
}

// unleveredEntryCapacityDebtRaw bounds a B2 1x destination entry by the
// collateral reserve's remaining deposit limit, valued in debt-mint units
// (floored, less 1%) so the caller's existing debt-price conversion applies
// unchanged. No borrow capacity is involved.
func unleveredEntryCapacityDebtRaw(position KaminoPosition, accounts []ConfirmedAccount, route RuntimeRoute) (uint64, error) {
	collateral, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil || collateral.totalLiquiditySF == nil || collateral.depositLimitRaw == 0 {
		return 0, err
	}
	deposited, err := ceilScaledBigFraction(collateral.totalLiquiditySF)
	if err != nil || deposited >= collateral.depositLimitRaw {
		return 0, err
	}
	room, err := valueBetweenTokenRaw(collateral.depositLimitRaw-deposited, position.CollateralDecimals, position.DebtDecimals, position.CollateralPriceSF, position.DebtPriceSF, false)
	if err != nil {
		return 0, err
	}
	return min(room/100*99, math.MaxInt64), nil
}

// appendSelectorUnleveredTail prices the B2 1x entry tail and its complete
// exit on the same ledger rules as the leveraged recipe: deposit the entry
// swap minimum into the debt-free obligation, then withdraw every guaranteed
// receipt, swap the guaranteed redemption back to USDC and return it to
// Voltr idle. Every wire shape here is an existing one (flat deposit,
// collateral-only withdraw, collateral->USDC swap, stage/restore/NAV).
func appendSelectorUnleveredTail(ctx context.Context, rpc *chain.Client, client *jupiter.Client, m RouteManifest, policies installedPolicies, route RuntimeRoute, accounts []ConfirmedAccount, slot, observationFloor int64, blockhash LatestBlockhash,
	swap JupiterExecutionEvidence, minimum, liquidity uint64, deposit func(uint64, uint64, uint64, []string) error,
	appendInput func(any, ExpectedEffects) error, appendBridge func(Action, uint64, uint64, uint64, uint64) error, out *selectorDestinationQuote) error {
	entry := swap.Request.MinimumOutputRaw
	if err := deposit(entry, minimum, liquidity, nil); err != nil {
		return err
	}
	if err := appendBridge(ReportNAV, 0, 0, 0, 0); err != nil {
		return err
	}
	reserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return err
	}
	poolUpperRaw, err := selectorCollateralPoolUpper(accounts, route, slot, selectorCollateralWindows)
	if err != nil {
		return err
	}
	receipts, err := payoffOwnedReceiptsLower(new(big.Int).Lsh(new(big.Int).SetUint64(poolUpperRaw), 60), reserve.collateralMintSupply, entry)
	if err != nil {
		return err
	}
	returned, err := payoffLiquidityForReceipts(reserve.totalLiquiditySF, reserve.collateralMintSupply, receipts)
	if err != nil {
		return err
	}
	if receipts == 0 || returned == 0 || returned > entry || returned > math.MaxInt64 || entry > math.MaxUint64-liquidity {
		return budgetHold("selector_destination_unlevered_exit_unavailable")
	}
	withdrawal, err := m.kaminoPacketForRoute(policies, DeleverRouteStep, kaminoLegWithdraw, receipts, blockhash, route.Lane)
	if err != nil {
		return err
	}
	withdrawal.ObligationReserves = []string{route.Kamino.CollateralReserve}
	source, destination := kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
	projected := patchConfirmedTokenRaw(accounts, route.CollateralLiquiditySupply, liquidity+entry)
	projected = patchConfirmedTokenRaw(projected, route.CollateralCustody, 0)
	effects, err := exactKaminoTokenEffects(projected, source, destination, returned)
	if err != nil {
		return err
	}
	if err = appendInput(withdrawal, effects); err != nil {
		return err
	}
	if err = appendBridge(ReportNAV, 0, 0, 0, 0); err != nil {
		return err
	}
	exit, err := prepareJupiterQuoteEvidence(ctx, rpc, client, m, policies, Decision{Action: SwapCollateralToStableStep, AmountRaw: int64(returned), StrategyKey: route.Lane}, returned, 0, observationFloor)
	if err != nil {
		return err
	}
	if err = appendInput(exit.Request, exit.ExpectedEffects); err != nil {
		return err
	}
	proceeds := exit.Request.MinimumOutputRaw
	if err = appendBridge(StageSquadsToVoltr, proceeds, 0, 0, proceeds); err != nil {
		return err
	}
	if err = appendBridge(VoltrRestoreIdle, proceeds, 0, proceeds, 0); err != nil {
		return err
	}
	out.PayoffSwap, out.PayoffReturnAmountRaw = exit, returned
	return appendBridge(ReportNAV, 0, 0, 0, 0)
}
