package backyardrwa

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"math/big"
	"time"
)

// A destination forecast retains its hypothetical amounts separately from the
// observed accounts. It does not claim to have executed or simulated the loop.
type selectorDestinationQuote struct {
	Lane                        string                   `json:"lane"`
	EquityRaw                   uint64                   `json:"equityRaw"`
	AccountSlot                 int64                    `json:"accountSlot"`
	AccountsSHA256              string                   `json:"accountsSha256"`
	InitialCollateralMinimumRaw uint64                   `json:"initialCollateralMinimumRaw"`
	BorrowReceiveRaw            uint64                   `json:"borrowReceiveRaw"`
	BorrowFeeRaw                uint64                   `json:"borrowFeeRaw"`
	PayoffUpperRaw              uint64                   `json:"payoffUpperRaw"`
	PayoffSwap                  JupiterExecutionEvidence `json:"payoffSwap"`
	Recipe                      selectorRecipe           `json:"recipe"`
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
		if farm.Owner != kaminoFarmsProgram || farm.Executable || farm.Lamports == 0 || len(farm.Data) != 8336 || !bytes.Equal(farm.Data[:8], []byte{198, 102, 216, 74, 63, 66, 163, 190}) ||
			!sameKey(farm.Data[7328:7360], route.Kamino.MarketAuthority) || farm.Data[7361] != 0 || farm.Data[7362] != 1 {
			return budgetHold("selector_farm_unavailable")
		}
		if user.Owner != kaminoFarmsProgram || user.Executable || user.Lamports == 0 || len(user.Data) != 920 || !bytes.Equal(user.Data[:8], []byte{72, 177, 85, 249, 76, 167, 186, 126}) ||
			!sameKey(user.Data[16:48], pair[0]) || !sameKey(user.Data[48:80], bridgeVault) || user.Data[80] != 1 || !sameKey(user.Data[480:512], route.Kamino.Obligation) {
			return budgetHold("selector_farm_registration_unavailable")
		}
	}
	return nil
}

func selectorDestinationAccounts(ctx context.Context, rpc *RPCClient, m RouteManifest, route RuntimeRoute, minimumSlot int64) (int64, []ConfirmedAccount, KaminoPosition, error) {
	var empty KaminoPosition
	addresses := []string{route.Kamino.Market, route.Kamino.Obligation, route.Kamino.CollateralReserve, route.Kamino.DebtReserve,
		route.Kamino.CollateralMint, bridgeUSDC, route.CollateralCustody, route.DebtCustody, route.CollateralLiquiditySupply,
		route.DebtLiquiditySupply, route.DebtFeeReceiver, route.CollateralReceiptMint, route.CollateralReceiptSupply,
		route.CollateralFarm, route.ObligationCollateralFarm, route.DebtFarm, route.ObligationDebtFarm,
		budgetClockAddress, bridgeVault, bridgeDelegate, bridgeStrategy, reportTicketPDA}
	for _, family := range []BasicPolicyFamily{BasicCollateralLifecycle, BasicDebtLifecycle, BasicSwapRoutesA, BasicSwapRoutesB} {
		binding, _, err := m.basicPolicyBinding(family)
		if err != nil {
			return 0, nil, empty, err
		}
		addresses = append(addresses, binding.Policy)
	}
	for _, action := range []Action{VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV} {
		p, err := m.bridgePolicy(action)
		if err != nil {
			return 0, nil, empty, err
		}
		addresses = append(addresses, p.Account)
	}
	optional := uniqueNonzero([]string{route.Kamino.Obligation, route.ObligationCollateralFarm, route.ObligationDebtFarm})
	slot, accounts, err := rpc.GetMultipleAccountsWithOptional(ctx, uniqueNonzero(addresses), minimumSlot, optional...)
	if err != nil {
		return 0, nil, empty, err
	}
	position, err := observeKaminoFromFixedAccounts(ctx, rpc.GetMultipleAccounts, slot, accounts, route.Kamino)
	if err != nil {
		return 0, nil, empty, err
	}
	if position.HasPosition || position.CollateralDepositedRaw != 0 || position.DebtRaw != 0 {
		return 0, nil, empty, budgetHold("selector_destination_not_flat")
	}
	if ready, exit := liveRuntimePolicyReadiness(m, route, accounts); !ready || !exit {
		return 0, nil, empty, budgetHold("selector_destination_policy_unavailable")
	}
	for _, action := range []Action{VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV} {
		p, _ := m.bridgePolicy(action)
		a := accountAt(accounts, p.Account)
		if a.Owner != bridgeSquadsProgram || a.Executable || a.Lamports == 0 || !maskedPolicyDigestMatches(a.Data, p.MaskedByteRanges, p.NormalizedDigest) {
			return 0, nil, empty, budgetHold("selector_destination_bridge_policy_unavailable")
		}
	}
	if _, err = decodeObservedAdaptorConfig(accountAt(accounts, bridgeStrategy)); err != nil {
		return 0, nil, empty, err
	}
	ticket, err := decodeObservedReportTicket(accountAt(accounts, reportTicketPDA))
	if err != nil || ticket.Armed || ticket.LastConsumedSequence >= uint64(slot) {
		return 0, nil, empty, budgetHold("selector_destination_ticket_unavailable")
	}
	if err = validateSelectorFarms(route, accounts); err != nil {
		return 0, nil, empty, err
	}
	for _, side := range []struct{ reserve, mint, program, supply, farm string }{
		{route.Kamino.CollateralReserve, route.Kamino.CollateralMint, route.CollateralTokenProgram, route.CollateralLiquiditySupply, route.CollateralFarm},
		{route.Kamino.DebtReserve, bridgeUSDC, route.DebtTokenProgram, route.DebtLiquiditySupply, route.DebtFarm},
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
			return 0, nil, empty, budgetHold("selector_destination_reserve_binding_changed")
		}
		decimals := position.CollateralDecimals
		if side.mint == bridgeUSDC {
			decimals = position.DebtDecimals
		}
		if err = validateExecutionMint(accountAt(accounts, side.mint), side.program, decimals); err != nil {
			return 0, nil, empty, err
		}
	}
	c := accountAt(accounts, route.Kamino.CollateralReserve)
	d := accountAt(accounts, route.Kamino.DebtReserve)
	if !sameKey(c.Data[2560:2592], route.CollateralReceiptMint) || !sameKey(c.Data[2600:2632], route.CollateralReceiptSupply) || !sameKey(d.Data[192:224], route.DebtFeeReceiver) {
		return 0, nil, empty, budgetHold("selector_destination_reserve_binding_changed")
	}
	receipt := accountAt(accounts, route.CollateralReceiptMint)
	if receipt.Owner != classicTokenProgram || receipt.Executable || receipt.Lamports == 0 || len(receipt.Data) != 82 || receipt.Data[45] != 1 || binary.LittleEndian.Uint32(receipt.Data[:4]) != 1 || !sameKey(receipt.Data[4:36], route.Kamino.MarketAuthority) {
		return 0, nil, empty, budgetHold("selector_destination_receipt_mint_unavailable")
	}
	for _, b := range []kaminoCustodyBoundary{
		{route.CollateralCustody, route.Kamino.CollateralMint, bridgeVault}, {route.DebtCustody, bridgeUSDC, bridgeVault},
		{route.CollateralLiquiditySupply, route.Kamino.CollateralMint, route.Kamino.MarketAuthority}, {route.DebtLiquiditySupply, bridgeUSDC, route.Kamino.MarketAuthority},
		{route.DebtFeeReceiver, bridgeUSDC, route.Kamino.MarketAuthority}, {route.CollateralReceiptSupply, route.CollateralReceiptMint, route.Kamino.MarketAuthority},
	} {
		a := accountAt(accounts, b.Address)
		custody, err := DecodeTokenCustody(a.Owner, a.Data, mustKey(b.Mint), mustKey(b.Authority))
		if err != nil || a.Owner != classicTokenProgram || a.Executable || a.Lamports == 0 {
			return 0, nil, empty, budgetHold("selector_destination_custody_unavailable")
		}
		if b.Address == route.CollateralCustody && custody.Raw != 0 {
			return 0, nil, empty, budgetHold("selector_destination_not_flat")
		}
	}
	return slot, accounts, position, nil
}

// Price the real one-pass entry graph. Future balances are explicit scalars;
// no invented account image reaches an RPC simulation or execution admission.
func observeSelectorDestination(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, lane string, equity uint64, sampleSlot int64) (selectorDestinationQuote, error) {
	return observeSelectorDestinationSize(ctx, rpc, client, m, lane, equity, sampleSlot, false)
}

// The live evaluator can quote the exact partial size admitted by current pair
// capacity. Exact-size callers retain their fail-closed size contract.
func observeSelectorDestinationSize(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, lane string, equity uint64, sampleSlot int64, clampCapacity bool) (selectorDestinationQuote, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out := selectorDestinationQuote{Lane: lane, EquityRaw: equity}
	if rpc == nil || client == nil || !selectorLane(lane) || equity == 0 || equity > uint64(PilotWorkingTrancheCapRaw) || sampleSlot <= 0 || sampleSlot > math.MaxInt64-budgetMaxObservationLagSlots {
		return out, budgetHold("invalid_selector_destination")
	}
	route, _ := runtimeRoute(lane)
	slot, accounts, position, err := selectorDestinationAccounts(ctx, rpc, m, route, sampleSlot)
	if err != nil {
		return out, err
	}
	capacity, err := kaminoPairEntryCapacity(position, accounts, route)
	if err != nil {
		return out, err
	}
	if clampCapacity {
		equity = min(equity, capacity)
		out.EquityRaw = equity
	}
	if equity == 0 || equity > capacity || min(position.LiquidationThresholdBPS-1500, 6000) <= TargetLTVBPS {
		return out, budgetHold("selector_destination_capacity_unavailable")
	}
	out.AccountSlot, out.AccountsSHA256 = slot, hashConfirmedAccounts(accounts)
	blockhash, err := rpc.LatestBlockhash(ctx)
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
	if !position.ObligationPresent {
		var rent uint64
		if err = rpc.call(ctx, "getMinimumBalanceForRentExemption", []any{kaminoObligationLength, map[string]string{"commitment": "confirmed"}}, &rent); err != nil {
			return out, err
		}
		r, err := m.initializationRequest(lane, blockhash, rent, 1)
		if err != nil {
			return out, err
		}
		message, err := CompileKaminoInitializationMessage(r)
		if err != nil {
			return out, err
		}
		fee, err := rpc.ObserveMessageFee(ctx, message, slot)
		if err != nil {
			return out, err
		}
		r.MaximumFeeLamports = fee.Lamports
		var initSlot int64
		if initSlot, err = validateKaminoInitializationPrestate(ctx, rpc, r, max(slot, fee.Slot)); err != nil {
			return out, err
		}
		observationFloor = max(slot, fee.Slot, initSlot)
		if observationFloor > sampleSlot+budgetMaxObservationLagSlots {
			return out, budgetHold("selector_recipe_observation_expired")
		}
		if err = appendInput(r, ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &r}); err != nil {
			return out, err
		}
	}
	// Fixed-width report fields below are fee templates, never NAV evidence.
	// Allocation updates NAV itself; an extra report is conservative coverage.
	report := BridgeReport{Sequence: uint64(slot), ObservedSlot: uint64(slot), NAVAfterRaw: equity, SnapshotDigest: out.AccountsSHA256}
	appendBridge := func(action Action, amount, idle, cash uint64) error {
		d := Decision{Action: action, AmountRaw: int64(amount)}
		e, _, _, err := bridgeExpectedEffects(d, idle, 0, cash)
		if err != nil {
			return err
		}
		e.Kind, e.ReturnData = "bridge", expectedAdaptorReturnData(equity)
		r := BridgeBuildRequest{Action: action, AmountRaw: amount, Report: report, AdaptorConfig: bridgeStrategy, Settings: bridgeSettings, RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight}
		return appendInput(r, e)
	}
	if err = appendBridge(VoltrAllocateToSquads, equity, equity, 0); err != nil {
		return out, err
	}
	if err = appendBridge(ReportNAV, 0, 0, equity); err != nil {
		return out, err
	}
	swap, err := prepareJupiterQuoteEvidence(ctx, rpc, client, m, Decision{Action: SwapStableToCollateralStep, AmountRaw: int64(equity), StrategyKey: lane}, equity, 0, observationFloor)
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
	if err = appendBridge(ReportNAV, 0, 0, 0); err != nil {
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
	// Transfer rounding is not receipt redemption rounding. Deduct an extra
	// liquidity unit before estimating a borrow against the first deposit.
	borrow, err := targetBorrowForCollateralRaw(minimum-1, position.CollateralDecimals, position.DebtDecimals, position.CollateralPriceSF, position.DebtPriceSF)
	if err != nil {
		return out, err
	}
	fee, err := kaminoBorrowFeeAtRate(binary.LittleEndian.Uint64(accountAt(accounts, route.Kamino.DebtReserve).Data[kaminoReserveConfigOffset+40:]), borrow)
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
	liquidity := binary.LittleEndian.Uint64(accountAt(accounts, route.CollateralLiquiditySupply).Data[64:72])
	deposit := func(amount, minDebit, beforeSupply uint64, reserves []string) error {
		if amount > math.MaxUint64-beforeSupply {
			return budgetHold("selector_destination_amount_overflow")
		}
		r, err := m.kaminoPacketForRoute(OpenRouteStep, kaminoLegDeposit, amount, blockhash, lane)
		if err != nil {
			return err
		}
		r.ObligationReserves = reserves
		e := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-deposit", Conserved: true, Deposit: &ExpectedDeposit{minDebit, amount}, Accounts: []ExpectedAccountEffect{
			{Address: route.CollateralCustody, Owner: classicTokenProgram, Mint: route.Kamino.CollateralMint, Authority: bridgeVault, BeforeRaw: amount, AfterRaw: 0},
			{Address: route.CollateralLiquiditySupply, Owner: classicTokenProgram, Mint: route.Kamino.CollateralMint, Authority: route.Kamino.MarketAuthority, BeforeRaw: beforeSupply, AfterRaw: beforeSupply + amount},
		}}
		return appendInput(r, e)
	}
	if err = deposit(swap.Request.MinimumOutputRaw, minimum, liquidity, nil); err != nil {
		return out, err
	}
	if err = appendBridge(ReportNAV, 0, 0, 0); err != nil {
		return out, err
	}
	r, err := m.kaminoPacketForRoute(OpenRouteStep, kaminoLegBorrow, borrow, blockhash, lane)
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
		{Address: route.DebtLiquiditySupply, Owner: classicTokenProgram, Mint: bridgeUSDC, Authority: route.Kamino.MarketAuthority, BeforeRaw: supply, AfterRaw: supply - borrow - fee},
		{Address: route.DebtCustody, Owner: classicTokenProgram, Mint: bridgeUSDC, Authority: bridgeVault, BeforeRaw: 0, AfterRaw: borrow},
		{Address: route.DebtFeeReceiver, Owner: classicTokenProgram, Mint: bridgeUSDC, Authority: route.Kamino.MarketAuthority, BeforeRaw: feeBalance, AfterRaw: feeBalance + fee},
	}}
	if err = appendInput(r, e); err != nil {
		return out, err
	}
	if err = appendBridge(ReportNAV, 0, 0, borrow); err != nil {
		return out, err
	}
	leverage, err := prepareJupiterQuoteEvidence(ctx, rpc, client, m, Decision{Action: SwapDebtToCollateralStep, AmountRaw: int64(borrow), StrategyKey: lane}, borrow, 0, observationFloor)
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
	if err = appendBridge(ReportNAV, 0, 0, 0); err != nil {
		return out, err
	}
	if leverage.Request.MinimumOutputRaw <= rounding+1 {
		return out, budgetHold("selector_destination_below_deposit_minimum")
	}
	redepositMinimum := leverage.Request.MinimumOutputRaw - rounding
	if err = deposit(leverage.Request.MinimumOutputRaw, redepositMinimum, liquidity+swap.Request.MinimumOutputRaw, []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}); err != nil {
		return out, err
	}
	if err = appendBridge(ReportNAV, 0, 0, 0); err != nil {
		return out, err
	}
	out.PayoffUpperRaw, out.PayoffSwap, err = selectorDestinationExit(ctx, rpc, client, m, route, accounts, position, slot, observationFloor, minimum, redepositMinimum, borrow, fee, rounding)
	if err != nil {
		return out, err
	}
	observationFloor, err = selectorSwapObservationFloor(out.PayoffSwap.Request, sampleSlot, observationFloor)
	if err != nil {
		return out, err
	}
	out.Recipe, err = priceSelectorRecipeWithFloor(ctx, rpc, lane, inputs, sampleSlot, observationFloor)
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
