package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// sfRaw lifts a raw token amount to its 2^60-scaled fixed point, the unit
// every receipt conversion divides by. Raw token units never multiply with
// token-scale units elsewhere in these cases.
func sfRaw(raw uint64) *big.Int {
	return new(big.Int).Lsh(new(big.Int).SetUint64(raw), 60)
}

// Task877 numerical regressions: the deposit receipt conversion multiplies by
// the receipt mint supply in BOTH factors, at unit rates, non-unit rates, and
// across unequal decimals. totalLiquidity=100 tokens, mintSupply=50 receipts,
// deposit=10 tokens yields exactly 5 receipts.
func TestPayoffDepositReceiptConversionIncludesMintSupply(t *testing.T) {
	one := new(big.Int).Lsh(big.NewInt(1), 60)
	for _, tc := range []struct {
		name                string
		liquiditySF         *big.Int
		mintSupply, deposit uint64
		want                uint64
	}{
		{"coordinator_regression_unit_rate", new(big.Int).Mul(big.NewInt(100), one), 50, 10, 5},
		{"non_unit_rate_floors", new(big.Int).Mul(big.NewInt(100), one), 37, 10, 3},
		{"half_token_per_receipt", new(big.Int).Mul(big.NewInt(100), one), 200, 10, 20},
		{"floor_keeps_remainder", new(big.Int).Mul(big.NewInt(100), one), 50, 11, 5},
		{"unequal_decimals", sfRaw(100_000_000_000), 50_000_000, 10_000_000_000, 5_000_000},
	} {
		got, err := payoffDepositReceiptsAt(tc.liquiditySF, tc.mintSupply, tc.deposit)
		if err != nil || got != tc.want {
			t.Fatalf("%s: receipts=%d want %d err=%v", tc.name, got, tc.want, err)
		}
	}
	for _, bad := range []struct {
		name                string
		liquiditySF         *big.Int
		mintSupply, deposit uint64
	}{
		{"zero_deposit", sfRaw(100), 50, 0},
		{"zero_supply", sfRaw(100), 0, 10},
		{"zero_pool", new(big.Int), 50, 10},
	} {
		if _, err := payoffDepositReceiptsAt(bad.liquiditySF, bad.mintSupply, bad.deposit); err == nil {
			t.Fatalf("%s accepted", bad.name)
		}
	}
}

// The inverse conversions floor redemptions and ceil wires so the wire's
// redemption at ANY later (higher) rate still covers its target.
func TestPayoffWireAndLiquidityConversionsRoundTrip(t *testing.T) {
	one := new(big.Int).Lsh(big.NewInt(1), 60)
	if got, err := payoffLiquidityForReceipts(new(big.Int).Mul(big.NewInt(100), one), 50, 5); err != nil || got != 10 {
		t.Fatalf("liquidity(5)=%d err=%v", got, err)
	}
	if got, err := payoffLiquidityForReceipts(new(big.Int).Mul(big.NewInt(100), one), 50, 0); err != nil || got != 0 {
		t.Fatalf("liquidity(0)=%d err=%v", got, err)
	}
	if got, err := payoffReceiptsForLiquidity(new(big.Int).Mul(big.NewInt(100), one), 50, 10); err != nil || got != 5 {
		t.Fatalf("wire(10)=%d err=%v", got, err)
	}
	if got, err := payoffReceiptsForLiquidity(new(big.Int).Mul(big.NewInt(100), one), 50, 11); err != nil || got != 6 {
		t.Fatalf("ceil wire(11)=%d err=%v", got, err)
	}
	// A positive remainder below the denominator ceils to ONE, never zero.
	if got, err := payoffReceiptsForLiquidity(new(big.Int).Mul(big.NewInt(100), one), 50, 1); err != nil || got != 1 {
		t.Fatalf("sub-unit wire=%d err=%v", got, err)
	}
	pool := new(big.Int).Mul(big.NewInt(97), one)
	wire, err := payoffReceiptsForLiquidity(pool, 53, 1234)
	if err != nil {
		t.Fatal(err)
	}
	got, err := payoffLiquidityForReceipts(pool, 53, wire)
	if err != nil || got < 1234 {
		t.Fatalf("wire %d guarantees only %d < 1234", wire, got)
	}
}

// autoPayoffBatch reshapes the A-owned observation batch for the payoff
// producer: 100 tokens (nine decimals) of collateral liquidity over 50e6
// receipts, an LTV/liquidation pair the release ceiling accepts, and a
// nonzero global borrow allowance. mutate runs last.
func autoPayoffBatch(t *testing.T, slot int64, mutate func([]ConfirmedAccount)) (RouteManifest, RuntimeRoute, []ConfirmedAccount) {
	t.Helper()
	route, err := runtimeRoute(testAutoLane)
	if err != nil {
		t.Fatal(err)
	}
	m, _, accounts := autoObservationBatch(t, slot, func(accounts []ConfirmedAccount) {
		reserve := accountAt(accounts, route.Kamino.CollateralReserve)
		binary.LittleEndian.PutUint64(reserve.Data[224:232], 100_000_000_000)
		binary.LittleEndian.PutUint64(reserve.Data[2592:2600], 50_000_000)
		reserve.Data[kaminoLoanToValueOffset] = 60
		reserve.Data[kaminoReserveConfigOffset+17] = 90
		market := accountAt(accounts, route.Kamino.Market)
		binary.LittleEndian.PutUint64(market.Data[kaminoGlobalBorrowValueOffset:], 1_000_000_000)
		if mutate != nil {
			mutate(accounts)
		}
	})
	return m, route, accounts
}

// autoPayoffMints appends the mint images the price observer decodes: the
// nine-decimal classic AUTO collateral, the six-decimal Token-2022 PYUSD debt,
// and the classic USDC reference.
func autoPayoffMints(t *testing.T, route RuntimeRoute) []ConfirmedAccount {
	t.Helper()
	mint := func(decimals byte) []byte {
		data := make([]byte, 82)
		data[44], data[45] = decimals, 1
		return data
	}
	return []ConfirmedAccount{
		{Address: route.Kamino.CollateralMint, Owner: classicTokenProgram, Lamports: 1, Data: mint(9)},
		{Address: route.Kamino.DebtMint, Owner: token2022Program, Lamports: 1, Data: mint(6)},
		{Address: bridgeUSDC, Owner: classicTokenProgram, Lamports: 1, Data: mint(6)},
	}
}

func autoPayoffPosition() KaminoPosition {
	var collPrice, debtPrice [16]byte
	putScaledFraction(collPrice[:], new(big.Int).Mul(big.NewInt(3), new(big.Int).Lsh(big.NewInt(1), 59)))
	putScaledFraction(debtPrice[:], new(big.Int).Lsh(big.NewInt(1), 60))
	return KaminoPosition{CollateralDecimals: 9, DebtDecimals: 6, CollateralPriceSF: collPrice, DebtPriceSF: debtPrice}
}

// autoJupiterTransport serves the five AUTO edges through the reviewed
// instruction shape; `quote` maps each edge to its fixture's integer
// economics, and `probe`, when set and returning true, overrides the quote
// economics for that exact request.
func autoJupiterTransport(t *testing.T, route RuntimeRoute, quote func(in, destination string, amount uint64) (quoted, minimum uint64), probe func(amount uint64) (out, minimum uint64, ok bool)) *jupiterClient {
	t.Helper()
	client, err := newJupiterClient("https://jupiter.invalid", &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/quote":
			q := req.URL.Query()
			amount, _ := strconv.ParseUint(q.Get("amount"), 10, 64)
			in, destination := q.Get("inputMint"), q.Get("outputMint")
			quoted, minimum := quote(in, destination, amount)
			if probe != nil {
				if out, min, ok := probe(amount); ok {
					quoted, minimum = out, min
				}
			}
			payload := JupiterQuote{InputMint: in, OutputMint: destination, InAmount: fmt.Sprint(amount), OutAmount: fmt.Sprint(quoted), OtherAmountThreshold: fmt.Sprint(minimum), SwapMode: "ExactIn", SlippageBPS: 50, RoutePlan: []json.RawMessage{json.RawMessage(`{}`)}}
			raw, _ := json.Marshal(payload)
			return response(string(raw)), nil
		case "/swap-instructions":
			var body struct {
				QuoteResponse JupiterQuote
			}
			if json.NewDecoder(req.Body).Decode(&body) != nil {
				t.Fatal("quote body")
			}
			amount, _ := strconv.ParseUint(body.QuoteResponse.InAmount, 10, 64)
			quoted, _ := strconv.ParseUint(body.QuoteResponse.OutAmount, 10, 64)
			var action Action
			switch {
			case body.QuoteResponse.OutputMint == route.Kamino.CollateralMint && body.QuoteResponse.InputMint == bridgeUSDC:
				action = SwapStableToCollateralStep
			case body.QuoteResponse.OutputMint == route.Kamino.CollateralMint && body.QuoteResponse.InputMint == route.Kamino.DebtMint:
				action = SwapDebtToCollateralStep
			case body.QuoteResponse.InputMint == route.Kamino.CollateralMint && body.QuoteResponse.OutputMint == route.Kamino.DebtMint:
				action = SwapCollateralToDebtStep
			case body.QuoteResponse.InputMint == route.Kamino.DebtMint:
				action = SwapDebtToUSDCStep
			default:
				action = SwapCollateralToStableStep
			}
			instruction := autoJupiterTestInstruction(t, action, amount, quoted, 2)
			raw, _ := json.Marshal(map[string]any{"swapInstruction": instruction, "addressLookupTableAddresses": []string{}})
			return response(string(raw)), nil
		}
		t.Fatalf("unexpected Jupiter path %s", req.URL.Path)
		return nil, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// autoPayoffQuote is the synthetic boundary economics of the payoff ledger
// tests: AUTO->PYUSD at 2x, repriced 4x below 1e9 raw so the funding quote is
// cheaper per unit than the probe, PYUSD->USDC at parity, AUTO->USDC at 1.5
// USDC per 1e9 raw. These ratios are intentionally NOT token-coherent; the
// boundary tests derive their expectations from the same ratios.
func autoPayoffQuote(route RuntimeRoute) func(in, destination string, amount uint64) (uint64, uint64) {
	return func(in, destination string, amount uint64) (uint64, uint64) {
		switch {
		case in == route.Kamino.CollateralMint && destination == route.Kamino.DebtMint:
			if amount <= 1_000_000_000 {
				return amount * 4, amount * 4
			}
			return amount * 2, amount * 2
		case in == route.Kamino.DebtMint && destination == bridgeUSDC:
			return amount, amount
		case in == route.Kamino.CollateralMint && destination == bridgeUSDC:
			return amount * 15 / 10_000, amount * 15 / 10_000
		default:
			return amount * 2 / 3, amount * 2 / 3
		}
	}
}

func autoPayoffJupiter(t *testing.T, route RuntimeRoute, probe func(amount uint64) (out, minimum uint64, ok bool)) *jupiterClient {
	t.Helper()
	return autoJupiterTransport(t, route, autoPayoffQuote(route), probe)
}

// autoCandidateQuote is the coherent full-entry economics at the observed
// fixture prices, AUTO 1.5 USDC and PYUSD 1 USDC. AUTO is 9dp and both
// stables 6dp, so one AUTO raw is 3/2000 stable raw and one stable raw is
// 2000/3 AUTO raw, floored; PYUSD and USDC trade at parity. Above-peg debt
// economics stay an explicit later case.
func autoCandidateQuote(route RuntimeRoute) func(in, destination string, amount uint64) (uint64, uint64) {
	return func(in, destination string, amount uint64) (uint64, uint64) {
		switch {
		case destination == route.Kamino.CollateralMint:
			return amount * 2000 / 3, amount * 2000 / 3
		case in == route.Kamino.CollateralMint:
			return amount * 3 / 2000, amount * 3 / 2000
		default:
			return amount, amount
		}
	}
}

func autoCandidateJupiter(t *testing.T, route RuntimeRoute) *jupiterClient {
	t.Helper()
	return autoJupiterTransport(t, route, autoCandidateQuote(route), nil)
}

// autoPayoffRPC serves every fee, account and slot read at the ONE fixture
// slot and unix timestamp the batch, Clock and producer already use. The
// shared fee/valuation fixtures are re-stamped fresh at the batch's own
// timestamp so no observer falls into the refresh-simulation fallback; the
// production freshness checks themselves run unmodified.
func autoPayoffRPC(t *testing.T, slot int64, batch []ConfirmedAccount) *RPCClient {
	t.Helper()
	config, err := pinnedKaminoObservationConfig()
	if err != nil {
		t.Fatal(err)
	}
	one := new(big.Int).Lsh(big.NewInt(1), 60)
	usdc := reserveFixture(t, config.DebtReserve, bridgeUSDC, 42, one, 1, 1)
	sol := reserveFixture(t, budgetSOLReserve, budgetWrappedSOLMint, 42, new(big.Int).Mul(one, big.NewInt(100)), 1, 1)
	putKey(t, sol.Data[32:64], budgetSOLMarket)
	binary.LittleEndian.PutUint64(sol.Data[272:280], 9)
	batch = append(append([]ConfirmedAccount(nil), batch...), usdc, sol)
	rpc := budgetBuildRPCWithAccounts(t, 5000, slot, batch)
	base := rpc.client.Transport
	rpc.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		var call struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(body, &call) != nil {
			return nil, fmt.Errorf("unparseable rpc request")
		}
		res, err := base.RoundTrip(request)
		if err != nil {
			return nil, err
		}
		if call.Method != "getFeeForMessage" && call.Method != "getMultipleAccounts" && call.Method != "getSlot" {
			return res, nil
		}
		defer res.Body.Close()
		var payload map[string]any
		if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
			return nil, err
		}
		if call.Method == "getSlot" {
			payload["result"] = slot
		} else if contextMap, ok := payload["result"].(map[string]any); ok {
			if context, ok := contextMap["context"].(map[string]any); ok {
				context["slot"] = slot
			}
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		return response(string(encoded)), nil
	})
	return rpc
}

// autoPayoffProducer runs the real payoff producer on the reshaped batch and
// returns its conserved ledger for consumer and negative tests.
func autoPayoffProducer(t *testing.T, slot int64, mutate func([]ConfirmedAccount), client *jupiterClient) (RouteManifest, RuntimeRoute, *RPCClient, []ConfirmedAccount, selectorDestinationPayoff, uint64) {
	t.Helper()
	const (
		entryDeposit     = uint64(10_000_000_000)
		redepositDeposit = uint64(6_000_000_000)
		borrow           = uint64(50_000)
		fee              = uint64(100)
	)
	m, route, accounts := autoPayoffBatch(t, slot, mutate)
	rounding, err := selectorReceiptRounding(accounts, route, slot)
	if err != nil {
		t.Fatal(err)
	}
	batch := append(append([]ConfirmedAccount(nil), accounts...), autoPayoffMints(t, route)...)
	rpc := autoPayoffRPC(t, slot, batch)
	if client == nil {
		client = autoPayoffJupiter(t, route, nil)
	}
	payoff, err := selectorDestinationExit(context.Background(), rpc, client, m, route, accounts, autoPayoffPosition(), slot, slot,
		entryDeposit, redepositDeposit, entryDeposit, redepositDeposit, borrow, fee, rounding)
	if err != nil {
		t.Fatal(err)
	}
	return m, route, rpc, accounts, payoff, rounding
}

// autoPayoffRecipeInputs retains the payoff template chain through the exact
// production consumer closures and returns the inputs for pricing.
func autoPayoffRecipeInputs(t *testing.T, m RouteManifest, route RuntimeRoute, accounts []ConfirmedAccount, payoff selectorDestinationPayoff, rounding uint64) ([]*phase3BuildInput, error) {
	t.Helper()
	const (
		entryDeposit     = uint64(10_000_000_000)
		redepositDeposit = uint64(6_000_000_000)
		borrow           = uint64(50_000)
		fee              = uint64(100)
		equity           = uint64(1_000_000)
	)
	blockhash := LatestBlockhash{Blockhash: bridgeSettings, LastValidBlockHeight: 99}
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
	report := BridgeReport{Sequence: 77, ObservedSlot: 77, NAVAfterRaw: equity, SnapshotDigest: hashConfirmedAccounts(accounts)}
	appendBridge := func(action Action, amount, idle, strategy, squads uint64) error {
		d := Decision{Action: action, AmountRaw: int64(amount)}
		e, _, _, err := bridgeExpectedEffects(d, idle, strategy, squads)
		if err != nil {
			return err
		}
		e.Kind, e.ReturnData = "bridge", expectedAdaptorReturnData(equity)
		return appendInput(BridgeBuildRequest{Action: action, AmountRaw: amount, Report: report, AdaptorConfig: bridgeStrategy, Settings: bridgeSettings, RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight}, e)
	}
	state := selectorPayoffTemplateState{blockhash: blockhash, liquidityRaw: 100_000_000_000, debtSupplyRaw: 100 + borrow + fee,
		entryDeposit: entryDeposit, redepositDeposit: redepositDeposit, borrow: borrow, fee: fee, rounding: rounding}
	err := appendSelectorPayoffRecipeInputs(m, route, blockhash, accounts, payoff, state, appendInput, appendBridge)
	return inputs, err
}

// Positive producer/consumer proof: the exit's withdrawal ledger is
// receipt-exact and conserved, the funding wire's FUTURE redemption stays
// inside the LTV release, and the whole template chain prices through the
// real manifest-aware recipe consumer without tripping the structural window
// guard.
func TestSelectorDestinationPayoffProducesAndPricesReceiptExactLedger(t *testing.T) {
	m, route, rpc, accounts, payoff, rounding := autoPayoffProducer(t, 77, nil, nil)
	reserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		t.Fatal(err)
	}
	supply := reserve.collateralMintSupply
	// Receipt conservation: the two wires consume the owned budget exactly.
	total, err := budgetSumU64(payoff.FundingReceiptsRaw, payoff.ReturnReceiptsRaw)
	if err != nil || total != payoff.OwnedReceiptsRaw || payoff.OwnedReceiptsRaw == 0 {
		t.Fatalf("wires %d+%d != owned %d err=%v", payoff.FundingReceiptsRaw, payoff.ReturnReceiptsRaw, payoff.OwnedReceiptsRaw, err)
	}
	// The guaranteed budget can never exceed the current-rate counts; the
	// residual names exactly the unproven sliver for a refreshed continuation.
	entryUpper, err := payoffDepositReceiptsAt(reserve.totalLiquiditySF, supply, 10_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	redepositUpper, err := payoffDepositReceiptsAt(reserve.totalLiquiditySF, supply, 6_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	currentUpper, err := budgetSumU64(entryUpper, redepositUpper)
	if err != nil {
		t.Fatal(err)
	}
	if payoff.OwnedReceiptsRaw > currentUpper || payoff.OwnedReceiptsRaw+payoff.ResidualReceiptsRaw != currentUpper {
		t.Fatalf("owned %d + residual %d != current %d", payoff.OwnedReceiptsRaw, payoff.ResidualReceiptsRaw, currentUpper)
	}
	// Funding wire: the exact ceil wire of its sized input; the floored
	// current-rate redemption covers the sized sale and custody reconciles.
	wire, err := payoffReceiptsForLiquidity(reserve.totalLiquiditySF, supply, payoff.Funding.Request.AmountRaw)
	if err != nil || wire != payoff.FundingReceiptsRaw {
		t.Fatalf("wire %d != producer %d err=%v", wire, payoff.FundingReceiptsRaw, err)
	}
	liquidity, err := payoffLiquidityForReceipts(reserve.totalLiquiditySF, supply, wire)
	if err != nil || liquidity != payoff.FundingLiquidityRaw || liquidity < payoff.Funding.Request.AmountRaw {
		t.Fatalf("funding liquidity %d producer %d sized %d err=%v", liquidity, payoff.FundingLiquidityRaw, payoff.Funding.Request.AmountRaw, err)
	}
	if payoff.CustodyLeftoverRaw != payoff.FundingLiquidityRaw-payoff.Funding.Request.AmountRaw {
		t.Fatalf("custody %d != liquidity - sized", payoff.CustodyLeftoverRaw)
	}
	// The funding minimum covers the compounded debt upper and the residue
	// input is exactly its guaranteed remainder; the 4x funding reprice leaves
	// residue, so the residue conversion leg must exist.
	if payoff.Funding.Request.MinimumOutputRaw < payoff.DebtUpperRaw {
		t.Fatalf("funding minimum %d < debt upper %d", payoff.Funding.Request.MinimumOutputRaw, payoff.DebtUpperRaw)
	}
	if payoff.ResidueInputRaw != payoff.Funding.Request.MinimumOutputRaw-payoff.DebtUpperRaw || payoff.ResidueInputRaw == 0 {
		t.Fatalf("residue input %d, minimum %d, debt %d", payoff.ResidueInputRaw, payoff.Funding.Request.MinimumOutputRaw, payoff.DebtUpperRaw)
	}
	if len(payoff.Legs) != 3 || payoff.Legs[0].Request.AmountRaw != payoff.Funding.Request.AmountRaw ||
		payoff.Legs[1].Request.AmountRaw != payoff.ResidueInputRaw {
		t.Fatalf("unexpected leg chain: %d legs", len(payoff.Legs))
	}
	// Full post-repay return: the exact remaining budget, floored.
	returnReceipts := payoff.OwnedReceiptsRaw - payoff.FundingReceiptsRaw
	if payoff.ReturnReceiptsRaw != returnReceipts {
		t.Fatalf("return wire %d != owned - funding %d", payoff.ReturnReceiptsRaw, returnReceipts)
	}
	fullReturn, err := payoffLiquidityForReceipts(reserve.totalLiquiditySF, supply, returnReceipts)
	if err != nil || payoff.ReturnAmountRaw != fullReturn || fullReturn == 0 {
		t.Fatalf("return %d want %d err=%v", payoff.ReturnAmountRaw, fullReturn, err)
	}
	deposited, err := budgetSumU64(10_000_000_000, 6_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	redeemed, err := budgetSumU64(payoff.FundingLiquidityRaw, payoff.ReturnAmountRaw)
	if err != nil {
		t.Fatal(err)
	}
	if redeemed > deposited {
		t.Fatalf("conservation broken: redeemed %d > deposited %d", redeemed, deposited)
	}
	// THE release bound: the funding wire's future redemption, bounded at the
	// compounded pool ceiling, must fit the LTV release — the current-rate
	// floored output cannot bound it.
	debt, err := selectorProspectiveUpper(accounts, route, 77, route.Kamino.DebtReserve, route.Kamino.DebtMint,
		new(big.Int).Lsh(new(big.Int).SetUint64(50_100), 60), selectorPayoffWindows)
	if err != nil || debt != payoff.DebtUpperRaw {
		t.Fatalf("debt upper %d/%d err=%v", debt, payoff.DebtUpperRaw, err)
	}
	market := accountAt(accounts, route.Kamino.Market).Data
	collateralData := accountAt(accounts, route.Kamino.CollateralReserve).Data
	limits := kaminoPilotReleaseLimits{MaxLTVPct: collateralData[kaminoLoanToValueOffset], LiquidationPct: collateralData[kaminoReserveConfigOffset+17],
		GlobalAllowedBorrowValue: binary.LittleEndian.Uint64(market[kaminoGlobalBorrowValueOffset:])}
	copy(limits.MinimumRemainingValueSF[:], market[kaminoMinRemainingValueOffset:kaminoMinRemainingValueOffset+16])
	values := kaminoReleaseValues{CollateralRaw: 10_000_000_000 - 1 + 6_000_000_000 - 1, DebtRaw: debt,
		CollateralDecimals: 9, DebtDecimals: 6, CollateralPriceSF: autoPayoffPosition().CollateralPriceSF, DebtPriceSF: autoPayoffPosition().DebtPriceSF}
	allowance, err := pilotRepaymentLiquidityAllowanceForValues(values, limits)
	if err != nil {
		t.Fatal(err)
	}
	released := allowance - 2*rounding
	poolUpperRaw, err := selectorCollateralPoolUpper(accounts, route, 77, selectorCollateralWindows)
	if err != nil {
		t.Fatal(err)
	}
	upperRedeemed, err := payoffLiquidityForReceipts(sfRaw(poolUpperRaw), supply, payoff.FundingReceiptsRaw)
	if err != nil {
		t.Fatal(err)
	}
	if upperRedeemed > released {
		t.Fatalf("funding wire redeems up to %d collateral against a %d release", upperRedeemed, released)
	}
	// Consumer: every payoff step prices through the real recipe path, and the
	// retained recipe stays inside the structural window bound.
	inputs, err := autoPayoffRecipeInputs(t, m, route, accounts, payoff, rounding)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(inputs)) > selectorFullRecipeWindows {
		t.Fatalf("payoff recipe inputs %d exceed the structural bound %d", len(inputs), selectorFullRecipeWindows)
	}
	// Pricing observes the SAME batch at the SAME fixture slot: fee, token
	// and native reads all resolve at 77, inside one coherent freshness window.
	recipe, err := m.priceSelectorRecipeWithFloor(context.Background(), rpc, route.Lane, inputs, 77, 77)
	if err != nil {
		t.Fatal(err)
	}
	if recipe.CostRaw <= 0 || len(recipe.Costs) != len(inputs) || recipe.ExpectedCostRaw == nil ||
		*recipe.ExpectedCostRaw < 0 || *recipe.ExpectedCostRaw > recipe.CostRaw {
		t.Fatalf("unpriced payoff recipe: cost %d over %d inputs", recipe.CostRaw, len(inputs))
	}
	var bridges []Action
	var jup []JupiterSwapRequest
	var kamino []KaminoPrimeUSDCRequest
	for _, input := range recipe.Inputs {
		request, _, _, err := input.decodeWithManifest(m)
		if err != nil {
			t.Fatal(err)
		}
		switch r := request.(type) {
		case BridgeBuildRequest:
			bridges = append(bridges, r.Action)
		case JupiterSwapRequest:
			jup = append(jup, r)
		case KaminoPrimeUSDCRequest:
			kamino = append(kamino, r)
		default:
			t.Fatalf("unexpected recipe input %T", request)
		}
	}
	stage, restore := -1, -1
	for i, action := range bridges {
		switch action {
		case StageSquadsToVoltr:
			stage = i
		case VoltrRestoreIdle:
			restore = i
		}
	}
	if stage < 0 || restore < stage || len(bridges) == 0 || bridges[len(bridges)-1] != ReportNAV {
		t.Fatalf("canonical stage/restore/NAV tail missing: %v", bridges)
	}
	sawWire, sawRepay := false, false
	for _, r := range kamino {
		switch r.AmountRaw {
		case payoff.FundingReceiptsRaw:
			sawWire = true
		case payoff.DebtUpperRaw:
			sawRepay = true
		}
	}
	if !sawWire || !sawRepay {
		t.Fatalf("withdrawal/repay templates missing from retained recipe: %+v", kamino)
	}
	matchJup := func(leg JupiterExecutionEvidence) bool {
		for _, r := range jup {
			if r.Action == leg.Request.Action && r.AmountRaw == leg.Request.AmountRaw && r.MinimumOutputRaw == leg.Request.MinimumOutputRaw {
				return true
			}
		}
		return false
	}
	for _, leg := range append([]JupiterExecutionEvidence{payoff.Funding}, payoff.Legs[1:]...) {
		if !matchJup(leg) {
			t.Fatalf("payoff leg missing from retained recipe: %+v", leg.Request)
		}
	}
}

// Task877 regression: a funding receipt wire sized near the release redeems
// MORE collateral at the future rate than at the observed one. The producer
// must validate the wire's upper redemption against the LTV release — not
// only the current-rate lower output — and hold with its own reason.
func TestSelectorDestinationPayoffRejectsWireWhoseFutureRedemptionExceedsRelease(t *testing.T) {
	const slot = int64(2000)
	m, route, accounts := autoPayoffBatch(t, slot, func(accounts []ConfirmedAccount) {
		// An older collateral refresh compounds the pool ceiling above the
		// current rate; a ~2 USDC market minimum binds the LTV release to
		// ~14.67e9 raw collateral (16e9 minus the 1.33e9 raw minimum), so the
		// near-release wire's upper redemption can be made to cross it.
		inner, err := runtimeRoute(testAutoLane)
		if err != nil {
			t.Fatal(err)
		}
		binary.LittleEndian.PutUint64(accountAt(accounts, inner.Kamino.CollateralReserve).Data[16:24], uint64(slot-256))
		minimum := new(big.Int).Lsh(big.NewInt(2), 60)
		be := make([]byte, 16)
		minimum.FillBytes(be)
		dst := accountAt(accounts, inner.Kamino.Market).Data[kaminoMinRemainingValueOffset : kaminoMinRemainingValueOffset+16]
		for i, b := range be {
			dst[15-i] = b
		}
	})
	const (
		entryDeposit     = uint64(10_000_000_000)
		redepositDeposit = uint64(6_000_000_000)
		borrow           = uint64(50_000)
		fee              = uint64(100)
	)
	rounding, err := selectorReceiptRounding(accounts, route, slot)
	if err != nil {
		t.Fatal(err)
	}
	debt, err := selectorProspectiveUpper(accounts, route, slot, route.Kamino.DebtReserve, route.Kamino.DebtMint,
		new(big.Int).Lsh(new(big.Int).SetUint64(borrow+fee), 60), selectorPayoffWindows)
	if err != nil {
		t.Fatal(err)
	}
	collateralData := accountAt(accounts, route.Kamino.CollateralReserve).Data
	market := accountAt(accounts, route.Kamino.Market).Data
	limits := kaminoPilotReleaseLimits{MaxLTVPct: collateralData[kaminoLoanToValueOffset], LiquidationPct: collateralData[kaminoReserveConfigOffset+17],
		GlobalAllowedBorrowValue: binary.LittleEndian.Uint64(market[kaminoGlobalBorrowValueOffset:])}
	copy(limits.MinimumRemainingValueSF[:], market[kaminoMinRemainingValueOffset:kaminoMinRemainingValueOffset+16])
	release := func(collateralRaw uint64) uint64 {
		t.Helper()
		values := kaminoReleaseValues{CollateralRaw: collateralRaw, DebtRaw: debt, CollateralDecimals: 9, DebtDecimals: 6,
			CollateralPriceSF: autoPayoffPosition().CollateralPriceSF, DebtPriceSF: autoPayoffPosition().DebtPriceSF}
		allowance, err := pilotRepaymentLiquidityAllowanceForValues(values, limits)
		if err != nil {
			t.Fatalf("release(%d): debt=%d minimumSF=%s collateralSF=%s raw=%s err=%v", collateralRaw, debt,
				littleInt(limits.MinimumRemainingValueSF[:]), littleInt(values.CollateralPriceSF[:]), littleInt(values.DebtPriceSF[:]), err)
		}
		return allowance - 2*rounding
	}
	// Pad `initial` so the released liquidity is an exact multiple of one
	// receipt's current-rate liquidity: the wire then redeems exactly the
	// release at the observed rate and only the FUTURE rate breaks it.
	initial := entryDeposit - release(entryDeposit-1+redepositDeposit-1)%2_000
	released := release(initial - 1 + redepositDeposit - 1)
	if released%2_000 != 0 {
		t.Fatalf("fixture misaligned: released %d", released)
	}
	sized := released
	reserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := payoffReceiptsForLiquidity(reserve.totalLiquiditySF, reserve.collateralMintSupply, sized)
	if err != nil {
		t.Fatal(err)
	}
	liquidity, err := payoffLiquidityForReceipts(reserve.totalLiquiditySF, reserve.collateralMintSupply, wire)
	if err != nil || liquidity > released {
		t.Fatalf("fixture: lower redemption %d already exceeds release %d err=%v", liquidity, released, err)
	}
	poolUpperRaw, err := selectorCollateralPoolUpper(accounts, route, slot, selectorCollateralWindows)
	if err != nil {
		t.Fatal(err)
	}
	poolUpperSF := sfRaw(poolUpperRaw)
	owned, err := payoffOwnedReceiptsLower(poolUpperSF, reserve.collateralMintSupply, entryDeposit)
	if err != nil {
		t.Fatal(err)
	}
	redepositOwned, err := payoffOwnedReceiptsLower(poolUpperSF, reserve.collateralMintSupply, redepositDeposit)
	if err != nil {
		t.Fatal(err)
	}
	owned, err = budgetSumU64(owned, redepositOwned)
	if err != nil {
		t.Fatal(err)
	}
	if wire > owned {
		t.Fatalf("fixture: wire %d exceeds owned budget %d — the budget guard would fire first", wire, owned)
	}
	upperRedeemed, err := payoffLiquidityForReceipts(poolUpperSF, reserve.collateralMintSupply, wire)
	if err != nil || upperRedeemed <= released {
		t.Fatalf("fixture no longer demonstrates the defect: upper %d <= release %d err=%v", upperRedeemed, released, err)
	}
	// The probe's retained (wire-floor) minimum output IS the debt upper: the
	// JSON quote sits above it only by the wire's own slippage, so the sized
	// funding input is exactly the released collateral and the wire redeems
	// the whole release at the observed rate — only the FUTURE rate breaks
	// it. (Threshold values barely above the debt would leave the sizing
	// granularity — about one collateral unit per debt unit — unable to hit
	// a specific near-release target.)
	client := autoPayoffJupiter(t, route, func(amount uint64) (uint64, uint64, bool) {
		if amount == released {
			out := (debt*10_000 + 9_949) / 9_950 // wire floor at slippage 50 lands exactly on `debt`
			if floor := out * 9_950 / 10_000; floor != debt {
				t.Fatalf("fixture: probe wire floor %d != debt upper %d", floor, debt)
			}
			return out, debt, true
		}
		return 0, 0, false
	})
	batch := append(append([]ConfirmedAccount(nil), accounts...), autoPayoffMints(t, route)...)
	rpc := autoPayoffRPC(t, slot, batch)
	_, err = selectorDestinationExit(context.Background(), rpc, client, m, route, accounts, autoPayoffPosition(), slot, slot,
		entryDeposit, redepositDeposit, initial, redepositDeposit, borrow, fee, rounding)
	assertBudgetHold(t, err, "selector_destination_payoff_release_upper_exceeded")
}

// A probe that prices the release far below the funding need sizes an input
// larger than the permitted release; the producer must refuse, never wire it.
func TestSelectorDestinationPayoffRejectsProbePricedBelowFundingNeed(t *testing.T) {
	m, route, accounts := autoPayoffBatch(t, 77, nil)
	rounding, err := selectorReceiptRounding(accounts, route, 77)
	if err != nil {
		t.Fatal(err)
	}
	client := autoPayoffJupiter(t, route, func(uint64) (uint64, uint64, bool) {
		return 1000, 999, true
	})
	rpc := autoPayoffRPC(t, 77, append(append([]ConfirmedAccount(nil), accounts...), autoPayoffMints(t, route)...))
	_, err = selectorDestinationExit(context.Background(), rpc, client, m, route, accounts, autoPayoffPosition(), 77, 77,
		10_000_000_000, 6_000_000_000, 10_000_000_000, 6_000_000_000, 50_000, 100, rounding)
	assertBudgetHold(t, err, "selector_destination_payoff_quote_insufficient")
}

// A quote outage is an observation miss, not a policy hold: the exit
// propagates the transport failure so the serialized loop retries next tick.
func TestSelectorDestinationPayoffQuoteOutageIsNotAHold(t *testing.T) {
	m, route, accounts := autoPayoffBatch(t, 77, nil)
	rounding, err := selectorReceiptRounding(accounts, route, 77)
	if err != nil {
		t.Fatal(err)
	}
	rpc := autoPayoffRPC(t, 77, append(append([]ConfirmedAccount(nil), accounts...), autoPayoffMints(t, route)...))
	client, err := newJupiterClient("https://jupiter.invalid", &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("outage")), Header: make(http.Header)}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, err = selectorDestinationExit(context.Background(), rpc, client, m, route, accounts, autoPayoffPosition(), 77, 77,
		10_000_000_000, 6_000_000_000, 10_000_000_000, 6_000_000_000, 50_000, 100, rounding)
	if err == nil {
		t.Fatal("quote outage accepted")
	}
	var hold *BudgetHold
	if errors.As(err, &hold) {
		t.Fatalf("quote outage became a policy hold: %v", err)
	}
}

// The recipe consumer replays the conserved ledger before retaining anything;
// a tampered wire or custody bound must hold, never price a broken recipe.
func TestSelectorPayoffRecipeConsumerRejectsBrokenLedger(t *testing.T) {
	m, route, _, accounts, payoff, rounding := autoPayoffProducer(t, 77, nil, nil)
	tampered := payoff
	tampered.ReturnReceiptsRaw++
	if _, err := autoPayoffRecipeInputs(t, m, route, accounts, tampered, rounding); err == nil {
		t.Fatal("broken receipt ledger priced")
	} else {
		assertBudgetHold(t, err, "selector_destination_payoff_ledger_broken")
	}
	tampered = payoff
	tampered.CustodyLeftoverRaw++
	if _, err := autoPayoffRecipeInputs(t, m, route, accounts, tampered, rounding); err == nil {
		t.Fatal("padded custody priced")
	} else {
		assertBudgetHold(t, err, "selector_destination_payoff_ledger_broken")
	}
}

// autoCandidateFlatBatch reshapes the shared AUTO batch into a correctly
// initialized, already-existing, FLAT destination obligation: every position
// field zero, elevation group zero, empty custodies, and the cross-mode
// borrowing caps the capacity reader consumes present. The initializer branch
// stays out of scope — the obligation already exists. extra runs last and may
// break the fixture for negative cases (index mutations only; accountAt
// returns values).
func autoCandidateFlatBatch(t *testing.T, slot int64, extra func([]ConfirmedAccount)) (RouteManifest, RuntimeRoute, []ConfirmedAccount) {
	t.Helper()
	route, err := runtimeRoute(testAutoLane)
	if err != nil {
		t.Fatal(err)
	}
	m, _, accounts := autoPayoffBatch(t, slot, func(accounts []ConfirmedAccount) {
		o := accountAt(accounts, route.Kamino.Obligation)
		// Flat keeps the envelope, market and owner keys ([0:96]); only the
		// eight deposit slots ([96:1184]) and five lending slots ([1208:2208])
		// are cleared, exactly like the reviewed flat Maple obligation shape.
		clear(o.Data[96:1184])
		clear(o.Data[1208:2208])
		o.Data[kaminoObligationElevationGroupOffset] = 0
		clear(o.Data[2288:2320])
		clear(accountAt(accounts, route.CollateralCustody).Data[64:72])
		clear(accountAt(accounts, route.DebtCustody).Data[64:72])
		accountAt(accounts, route.Kamino.CollateralReserve).Data[kaminoDisableCrossCollateralOffset] = 0
		// Reserve-bound identities, exactly the reviewed Maple reserve shape:
		// each reserve names its liquidity supply and token program; the
		// collateral reserve names the receipt mint and its supply; the debt
		// reserve names the fee receiver.
		collateralReserve := accountAt(accounts, route.Kamino.CollateralReserve)
		putKey(t, collateralReserve.Data[160:192], route.CollateralLiquiditySupply)
		putKey(t, collateralReserve.Data[408:440], route.CollateralTokenProgram)
		putKey(t, collateralReserve.Data[2560:2592], route.CollateralReceiptMint)
		putKey(t, collateralReserve.Data[2600:2632], route.CollateralReceiptSupply)
		debtReserve := accountAt(accounts, route.Kamino.DebtReserve)
		putKey(t, debtReserve.Data[160:192], route.DebtLiquiditySupply)
		putKey(t, debtReserve.Data[408:440], route.DebtTokenProgram)
		putKey(t, debtReserve.Data[192:224], route.DebtFeeReceiver)
		// The source batch serves the fee receiver as a bare system account;
		// the destination custody validation requires the real token-2022
		// vault. Zero starting fee balance keeps the borrow snapshot trivially
		// consistent: AfterRaw = fee exactly.
		for i := range accounts {
			if accounts[i].Address == route.DebtFeeReceiver {
				accounts[i] = tokenAccountFixture(t, route.DebtFeeReceiver, route.Kamino.DebtMint, route.Kamino.MarketAuthority, 0)
				accounts[i].Owner = token2022Program
			}
		}
		binary.LittleEndian.PutUint64(debtReserve.Data[kaminoBorrowFactorOffset:], 100)
		binary.LittleEndian.PutUint64(debtReserve.Data[kaminoOutsideBorrowLimitOffset:], 10_000_000_000)
		binary.LittleEndian.PutUint64(debtReserve.Data[kaminoQueuedCollateralOffset:], 0)
		// Reserve-wide deposit/borrow limits, far above the 40e9 raw pooled
		// liquidity, so the capacity bound reflects decision inputs rather than
		// the fixture ceiling (same placement as the reviewed Maple shape).
		binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.CollateralReserve).Data[kaminoReserveConfigOffset+160:], 1_000_000_000_000)
		binary.LittleEndian.PutUint64(debtReserve.Data[kaminoReserveConfigOffset+160:], 1_000_000_000_000)
		binary.LittleEndian.PutUint64(debtReserve.Data[kaminoReserveConfigOffset+168:], 1_000_000_000_000)
		if extra != nil {
			extra(accounts)
		}
	})
	return m, route, accounts
}

// autoCandidateStack assembles the coherent single-slot candidate stack: the
// reshaped flat batch plus the price-observer mints, the slot-pinned RPC
// wrapper and the coherent five-edge Jupiter client. It returns the enriched
// batch so expectations derive from exactly the served shapes. The readiness
// batch already serves every pinned policy — the ONE combined reviewed AUTO
// policy and the four masked bridge policies with manifest-bound digests — and
// deliberately serves NONE of the four basic families: they are the reviewed
// Maple readiness surface and must never be fetched or required for this lane.
func autoCandidateStack(t *testing.T, slot int64, extra func([]ConfirmedAccount)) (RouteManifest, RuntimeRoute, *RPCClient, *jupiterClient) {
	t.Helper()
	m, route, accounts := autoCandidateFlatBatch(t, slot, extra)
	batch := append(append([]ConfirmedAccount(nil), accounts...), autoPayoffMints(t, route)...)
	// The batch list also carries the native funding accounts; the source
	// observation batch omits them. Add only when absent so readiness-provided
	// images are never shadowed.
	ensure := func(a ConfirmedAccount) {
		for _, existing := range batch {
			if existing.Address == a.Address {
				return
			}
		}
		batch = append(batch, a)
	}
	ensure(ConfirmedAccount{Address: bridgeVault, Owner: "11111111111111111111111111111111", Lamports: 1_000_000_000})
	ensure(ConfirmedAccount{Address: bridgeDelegate, Owner: "11111111111111111111111111111111", Lamports: 1_000_000_000})
	// The full position observation fetches each reserve's configured oracle
	// accounts. Serve exact dummies for precisely the keys the reserves name,
	// derived through the production decoder.
	for _, row := range [][2]string{{route.Kamino.CollateralReserve, route.Kamino.CollateralMint}, {route.Kamino.DebtReserve, route.Kamino.DebtMint}} {
		reserve, err := decodeKaminoReserve(accountAt(batch, row[0]), row[1], route.Kamino)
		if err != nil {
			t.Fatal(err)
		}
		for _, oracle := range uniqueNonzero(reserve.oracles) {
			ensure(ConfirmedAccount{Address: oracle, Owner: kaminoProgram, Lamports: 1, Data: []byte{1}})
		}
	}
	// The redemption side validates the receipt mint and reads the strategy's
	// owned receipt supply; the source observation batch omits both. The mint
	// mirrors the reviewed Maple shape at the AUTO collateral's 9 decimals.
	receiptMint := make([]byte, 82)
	receiptMint[44], receiptMint[45] = 9, 1
	binary.LittleEndian.PutUint32(receiptMint[:4], 1)
	putKey(t, receiptMint[4:36], route.Kamino.MarketAuthority)
	ensure(ConfirmedAccount{Address: route.CollateralReceiptMint, Owner: classicTokenProgram, Lamports: 1, Data: receiptMint})
	ensure(tokenAccountFixture(t, route.CollateralReceiptSupply, route.CollateralReceiptMint, route.Kamino.MarketAuthority, 1_000_000_000))
	return m, route, autoPayoffRPC(t, slot, batch), autoCandidateJupiter(t, route)
}

// The AUTO producer must retain the minimum its own legacy wire can enforce —
// never the advisory JSON threshold. The adversarial transport quotes
// threshold == output while the instruction encodes slippage 50, so the
// enforceable floor is strictly below the quoted output.
func TestJupiterAutoProducerRetainsEnforceableWireFloor(t *testing.T) {
	const slot = int64(77)
	m, route, rpc, client := autoCandidateStack(t, slot, nil)
	const amount = uint64(1_000_000)
	decision := Decision{Action: SwapStableToCollateralStep, AmountRaw: int64(amount), StrategyKey: route.Lane}
	evidence, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, m, decision, amount, 0, slot)
	if err != nil {
		t.Fatal(err)
	}
	// 1.5 USDC (6dp) per AUTO (9dp): stable raw * 2000/3, floored; the wire's
	// own slippage 50 makes the enforceable floor strictly smaller.
	quoted := amount * 2000 / 3
	floor := quoted * 9950 / 10000
	if evidence.Request.QuotedOutputRaw != quoted || evidence.Request.MinimumOutputRaw != floor || floor >= quoted {
		t.Fatalf("retained minimum %d, want enforceable wire floor %d (quoted %d)", evidence.Request.MinimumOutputRaw, floor, quoted)
	}
	if wireFloor, wireErr := jupiterInstructionWireFloor(evidence.Request.Instruction); wireErr != nil || wireFloor != floor {
		t.Fatalf("wire floor identity: %d %v", wireFloor, wireErr)
	}
	if got := *evidence.ExpectedEffects.Accounts[1].MinimumAfterRaw; got != floor {
		t.Fatalf("expected-effects guarantee is %d, want the same wire floor", got)
	}
	// An honest quote whose JSON threshold already sits at the enforceable
	// floor (nonzero transport slippage, retained unchanged) keeps the
	// byte-identical retained minimum.
	base := autoCandidateQuote(route)
	honest := autoJupiterTransport(t, route, func(in, destination string, a uint64) (uint64, uint64) {
		out, _ := base(in, destination, a)
		return out, out * 9950 / 10000
	}, nil)
	honestEvidence, err := prepareJupiterQuoteEvidence(context.Background(), rpc, honest, m, decision, amount, 0, slot)
	if err != nil {
		t.Fatal(err)
	}
	if honestEvidence.Request.MinimumOutputRaw != floor {
		t.Fatalf("honest nonzero-slippage quote drifted: %d want %d", honestEvidence.Request.MinimumOutputRaw, floor)
	}
}

// A retained AUTO request whose minimum exceeds what its wire enforces must
// fail compile and the persisted-evidence path: caller-forged evidence cannot
// carry the advisory threshold past the producer. Strictly smaller (more
// conservative) minimums stay constructible, the floor helper is
// dialect-defensive, and other lanes keep their established semantics.
func TestJupiterAutoRetainedMinimumRejectsForgedRequests(t *testing.T) {
	const slot = int64(77)
	m, _, rpc, client := autoCandidateStack(t, slot, nil)
	const amount = uint64(1_000_000)
	evidence, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, m, Decision{Action: SwapStableToCollateralStep, AmountRaw: int64(amount), StrategyKey: autoAUTOPYUSD.Lane}, amount, 0, slot)
	if err != nil {
		t.Fatal(err)
	}
	floor := evidence.Request.MinimumOutputRaw
	for _, forge := range []struct {
		name    string
		minimum uint64
	}{
		{"advisory_threshold", evidence.Request.QuotedOutputRaw},
		{"floor_plus_one", floor + 1},
	} {
		forged := evidence.Request
		forged.MinimumOutputRaw = forge.minimum
		if _, err := m.compileJupiterMessage(forged, mustKey(bridgeDelegate)); err == nil || !strings.Contains(err.Error(), "exceeds enforceable wire floor") {
			t.Fatalf("%s: forged retained minimum compiled: %v", forge.name, err)
		}
		if err := BuildSimulateAndPersistJupiter(context.Background(), &Database{}, rpc, "forged-"+forge.name, JupiterExecutionEvidence{forged, evidence.ExpectedEffects}); err == nil || !strings.Contains(err.Error(), "exceeds enforceable wire floor") {
			t.Fatalf("%s: persisted-evidence path accepted the forgery: %v", forge.name, err)
		}
	}
	conservative := evidence.Request
	conservative.MinimumOutputRaw = floor - 1
	if _, err := m.compileJupiterMessage(conservative, mustKey(bridgeDelegate)); err != nil {
		t.Fatalf("conservative minimum rejected: %v", err)
	}
	if _, err := m.compileJupiterMessage(evidence.Request, mustKey(bridgeDelegate)); err != nil {
		t.Fatalf("retained request does not compile: %v", err)
	}
	// The floor helper never interprets a non-legacy payload's tail offsets.
	v2 := evidence.Request
	v2Data, _ := base64.StdEncoding.DecodeString(v2.Instruction.Data)
	copy(v2Data[:8], jupiterSharedAccountsRouteV2)
	v2.Instruction.Data = base64.StdEncoding.EncodeToString(v2Data)
	if _, err := jupiterInstructionWireFloor(v2.Instruction); err == nil {
		t.Fatal("wire floor read a non-legacy payload")
	}
	if _, err := m.compileJupiterMessage(v2, mustKey(bridgeDelegate)); err == nil {
		t.Fatal("non-legacy AUTO payload retained")
	}
	// Other lanes keep their established minimum semantics.
	if err := jupiterValidateAutoRetainedMinimum(JupiterSwapRequest{RouteLane: SelectedRouteID, MinimumOutputRaw: ^uint64(0)}); err != nil {
		t.Fatalf("non-AUTO retention gate moved: %v", err)
	}
}

// The candidate entry prices the complete AUTO one-pass graph — entry swap,
// deposit, borrow, leverage, redeposit and the full payoff/return tail — in
// ONE coherent fixture slot, retaining every costed message at the exact
// structural window count with the collateral asset evidence carried. The
// transport is adversarial on the threshold (JSON == quoted output) while the
// legacy wire enforces slippage 50, so every retained minimum must be the
// enforceable wire floor, never the advisory threshold. The equity sample is
// chosen so the funding floor meets the debt upper exactly: no guaranteed
// residue exists and no residue conversion leg is invented.
func TestSelectorDestinationCandidatePricesFullEntryAndPayoff(t *testing.T) {
	const (
		slot   = int64(77)
		equity = uint64(2_000_000)
	)
	m, route, rpc, client := autoCandidateStack(t, slot, nil)
	q, err := observeSelectorDestinationCandidate(context.Background(), rpc, client, m, equity, slot)
	if err != nil {
		t.Fatal(err)
	}
	// 1.5 USDC (6dp) per AUTO (9dp): stable raw * 2000/3, floored. The
	// retained entry minimum is that quoted output scaled by the wire's own
	// slippage 50 — strictly below the advisory JSON threshold.
	entryQuoted := equity * 2000 / 3
	entryMinimum := entryQuoted * 9950 / 10000
	if q.Lane != testAutoLane || q.AccountSlot != slot || q.EquityRaw != equity ||
		q.InitialCollateralMinimumRaw != entryMinimum || entryMinimum >= entryQuoted ||
		q.Recipe.EvidenceID == "" || q.Recipe.ValidThroughSlot <= slot {
		t.Fatal("incomplete candidate quote", q)
	}
	// The fixture's debt reserve carries a zero origination fee curve, so the
	// fee and the guaranteed PYUSD residue are exactly zero and the residue
	// conversion leg is omitted — production must not invent one.
	if q.BorrowReceiveRaw == 0 || q.BorrowFeeRaw != 0 || q.PayoffUpperRaw < q.BorrowReceiveRaw+q.BorrowFeeRaw ||
		q.PayoffSwap.Request.MinimumOutputRaw < q.PayoffUpperRaw || q.PayoffReturnAmountRaw == 0 || q.PayoffResidueInputRaw != 0 {
		t.Fatal("payoff tail not covered", q)
	}
	if q.CollateralAssetUSDCRaw == nil || *q.CollateralAssetUSDCRaw == 0 || q.CollateralAssetPrice == nil ||
		q.RedepositCollateralRaw == 0 {
		t.Fatal("collateral asset evidence missing", q)
	}
	// Exactly the one-pass graph: 12 entry steps plus the 11-step payoff/return
	// tail. With a zero residue the PYUSD→USDC residue swap is omitted, so no
	// SwapDebtToUSDCStep is retained; the return withdrawal and its NAV report
	// are present because the return redemption is positive, and the stage/
	// restore pair carries the guaranteed AUTO→USDC proceeds.
	want := []Action{
		VoltrAllocateToSquads, ReportNAV, SwapStableToCollateralStep, ReportNAV,
		OpenRouteStep, ReportNAV, OpenRouteStep, ReportNAV, SwapDebtToCollateralStep, ReportNAV,
		OpenRouteStep, ReportNAV,
		DeleverRouteStep, ReportNAV, SwapCollateralToDebtStep, DeleverRouteStep, ReportNAV,
		DeleverRouteStep, ReportNAV, SwapCollateralToStableStep,
		StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV,
	}
	type priced struct {
		action        Action
		amount        uint64
		minimum       uint64
		amountPresent bool
	}
	got := make([]priced, 0, len(want))
	var swaps []JupiterSwapRequest
	for _, input := range q.Recipe.Inputs {
		request, _, _, err := input.decodeWithManifest(m)
		if err != nil {
			t.Fatal(err)
		}
		switch r := request.(type) {
		case BridgeBuildRequest:
			got = append(got, priced{action: r.Action, amount: r.AmountRaw, amountPresent: r.AmountRaw != 0})
		case JupiterSwapRequest:
			swaps = append(swaps, r)
			got = append(got, priced{action: r.Action, amount: r.AmountRaw, minimum: r.MinimumOutputRaw, amountPresent: true})
		case KaminoPrimeUSDCRequest:
			got = append(got, priced{action: r.Action, amount: r.AmountRaw, amountPresent: true})
		default:
			t.Fatalf("unexpected recipe input type %T", request)
		}
	}
	if len(got) != len(want) || len(got) != 23 || int64(len(got)) > selectorFullRecipeWindows {
		t.Fatal("recipe window count drifted", len(got))
	}
	for i := range want {
		if got[i].action != want[i] {
			t.Fatalf("recipe step %d is %s, want %s", i, got[i].action, want[i])
		}
	}
	if len(q.Recipe.Costs) != len(q.Recipe.Inputs) || q.Recipe.CostRaw <= 0 || q.Recipe.NetworkLamports == 0 {
		t.Fatal("not every retained message is costed", q.Recipe.CostRaw, len(q.Recipe.Costs))
	}
	for i, cost := range q.Recipe.Costs {
		if cost.Fee.Lamports == 0 || cost.MessageSHA256 == "" {
			t.Fatalf("cost %d has no observed message fee", i)
		}
	}
	// Amount identities across the graph, from the quote's own guaranteed
	// bounds: entry buy, deposits at guaranteed minimums, borrow sizing,
	// repay at the debt upper, residue proceeds staged and restored whole.
	proceeds := uint64(0)
	for _, leg := range q.PayoffLegs[1:] {
		if proceeds, err = budgetSumU64(proceeds, leg.Request.MinimumOutputRaw); err != nil {
			t.Fatal(err)
		}
	}
	checks := []struct {
		index int
		want  uint64
	}{
		{2, equity}, {4, entryMinimum}, {6, q.BorrowReceiveRaw}, {8, q.BorrowReceiveRaw},
		{10, q.RedepositCollateralRaw}, {14, q.PayoffSwap.Request.AmountRaw},
		{15, q.PayoffUpperRaw}, {20, proceeds}, {21, proceeds},
	}
	for _, check := range checks {
		if !got[check.index].amountPresent || got[check.index].amount != check.want || check.want == 0 {
			t.Fatalf("costed message at %d drifted: got %d, want %d", check.index, got[check.index].amount, check.want)
		}
	}
	if got[2].minimum != entryMinimum || got[14].minimum < q.PayoffUpperRaw {
		t.Fatal("guaranteed swap minimums drifted")
	}
	// Every retained AUTO swap carries exactly its own wire floor — the
	// through-destination mismatch regression against the advisory threshold.
	for i, swap := range swaps {
		wireFloor, wireErr := jupiterInstructionWireFloor(swap.Instruction)
		if wireErr != nil || swap.MinimumOutputRaw != wireFloor {
			t.Fatalf("retained swap %d minimum %d is not its wire floor %d: %v", i, swap.MinimumOutputRaw, wireFloor, wireErr)
		}
	}
	if len(swaps) != 4 || len(q.PayoffLegs) != 2 {
		t.Fatal("Jupiter leg count drifted", len(swaps), len(q.PayoffLegs))
	}
	// An honest quote whose JSON threshold already sits at the enforceable
	// floor prices the identical full graph.
	base := autoCandidateQuote(route)
	honest := autoJupiterTransport(t, route, func(in, destination string, a uint64) (uint64, uint64) {
		out, _ := base(in, destination, a)
		return out, out * 9950 / 10000
	}, nil)
	honestQuote, err := observeSelectorDestinationCandidate(context.Background(), rpc, honest, m, equity, slot)
	if err != nil {
		t.Fatal(err)
	}
	if honestQuote.InitialCollateralMinimumRaw != entryMinimum || len(honestQuote.Recipe.Inputs) != len(q.Recipe.Inputs) {
		t.Fatal("honest threshold quote diverged", honestQuote.InitialCollateralMinimumRaw)
	}
	// Custody trail: the funding leg sells only its sized input from the
	// released collateral, and the unswapped leftover stays in the collateral
	// custody after the FULL return leg — the payoff never claims a flat
	// custody; the leftover is the continuation pass's explicit inventory.
	funding, tail := q.PayoffLegs[0], q.PayoffLegs[len(q.PayoffLegs)-1]
	leftover := funding.ExpectedEffects.Accounts[0].AfterRaw
	if leftover == 0 || funding.ExpectedEffects.Accounts[0].BeforeRaw-leftover != q.PayoffSwap.Request.AmountRaw {
		t.Fatalf("funding custody trail drifted: %+v", funding.ExpectedEffects.Accounts[0])
	}
	if tail.ExpectedEffects.Accounts[0].BeforeRaw != leftover+q.PayoffReturnAmountRaw || tail.ExpectedEffects.Accounts[0].AfterRaw != leftover {
		t.Fatalf("full return does not end at the explicit leftover %d: %+v", leftover, tail.ExpectedEffects.Accounts[0])
	}
	// The quote itself retains the bounded-exit inventory: the leftover
	// custody and the unproven residual receipts — never deleted, never
	// credited as proceeds. The fixture's compounding ceiling leaves exactly
	// one receivable receipt beyond the guaranteed budget, and it is carried
	// for the continuation pass instead of being wired or dropped.
	if q.PayoffLeftoverCollateralRaw != leftover {
		t.Fatalf("leftover collateral not retained on the quote: %d want %d", q.PayoffLeftoverCollateralRaw, leftover)
	}
	if q.PayoffResidualReceiptsRaw != 1 {
		t.Fatalf("unproven residual receipt not explicitly retained: %d", q.PayoffResidualReceiptsRaw)
	}
	// The two receipt withdrawals are the only redemptions wired; production
	// conservation requires their receipt counts to sum to the whole owned
	// budget, and any residual receipts stay unwired for the continuation.
	if got[12].amount == 0 || got[17].amount == 0 {
		t.Fatalf("receipt withdrawal wires drifted: %d %d", got[12].amount, got[17].amount)
	}
}

// The honest wire floor can leave a single raw PYUSD unit of guaranteed
// residue above the debt upper. At parity that unit quotes exactly one raw
// USDC whose enforceable wire floor is zero, so the shared producer fails
// closed with a typed hold and the candidate propagates it — instead of
// retaining a zero minimum, discarding the dust, or fabricating proceeds.
// Remaining blocker: tranche sizings that land on this boundary stay HOLD
// until unconverted-dust continuation accounting is reviewed.
func TestSelectorDestinationCandidateHoldsUnconvertibleDustResidue(t *testing.T) {
	const slot = int64(77)
	m, _, rpc, client := autoCandidateStack(t, slot, nil)
	_, err := observeSelectorDestinationCandidate(context.Background(), rpc, client, m, 1_000_000, slot)
	assertBudgetHold(t, err, "jupiter_auto_wire_floor_zero")
}

// Off-peg residues decide convertibility by their ACTUAL quoted floor, never
// by residue units: above peg a single raw unit quotes two and keeps an
// enforceable floor of one, while below peg three raw units quote one whose
// floor is zero and hold.
func TestJupiterAutoTinyResidueConvertibilityFollowsQuotedFloor(t *testing.T) {
	const slot = int64(77)
	m, route, rpc, _ := autoCandidateStack(t, slot, nil)
	base := autoCandidateQuote(route)
	pegged := func(scale uint64) *jupiterClient {
		return autoJupiterTransport(t, route, func(in, destination string, amount uint64) (uint64, uint64) {
			quoted, _ := base(in, destination, amount)
			if in == route.Kamino.DebtMint && destination == bridgeUSDC {
				return quoted * scale / 1_000_000, quoted * scale / 1_000_000 // debt-mint USDC price in millionths
			}
			return quoted, quoted
		}, nil)
	}
	decision := func(amount uint64) Decision {
		return Decision{Action: SwapDebtToUSDCStep, AmountRaw: int64(amount), StrategyKey: route.Lane}
	}
	above, err := prepareJupiterQuoteEvidence(context.Background(), rpc, pegged(2_000_000), m, decision(1), 1, 0, slot)
	if err != nil {
		t.Fatal(err)
	}
	if above.Request.AmountRaw != 1 || above.Request.MinimumOutputRaw != 1 {
		t.Fatalf("above-peg single-unit residue drifted: %+v", above.Request)
	}
	_, err = prepareJupiterQuoteEvidence(context.Background(), rpc, pegged(333_334), m, decision(3), 3, 0, slot)
	assertBudgetHold(t, err, "jupiter_auto_wire_floor_zero")
}

// The parity fixture leaves exactly one raw PYUSD of guaranteed residue; with
// PYUSD above peg that unit quotes two raw USDC and retains a floor of one,
// so the residue conversion leg is retained and priced — and the quote still
// carries the bounded-exit residual inventory.
func TestSelectorDestinationCandidateConvertsTinyAbovePegResidue(t *testing.T) {
	const slot = int64(77)
	m, route, rpc, _ := autoCandidateStack(t, slot, nil)
	base := autoCandidateQuote(route)
	abovePeg := autoJupiterTransport(t, route, func(in, destination string, amount uint64) (uint64, uint64) {
		quoted, _ := base(in, destination, amount)
		if in == route.Kamino.DebtMint && destination == bridgeUSDC {
			return quoted * 2, quoted * 2
		}
		return quoted, quoted
	}, nil)
	q, err := observeSelectorDestinationCandidate(context.Background(), rpc, abovePeg, m, 1_000_000, slot)
	if err != nil {
		t.Fatal(err)
	}
	if q.PayoffResidueInputRaw != 1 || len(q.PayoffLegs) != 3 {
		t.Fatalf("tiny above-peg residue not retained: residue=%d legs=%d", q.PayoffResidueInputRaw, len(q.PayoffLegs))
	}
	converted := q.PayoffLegs[1]
	if converted.Request.Action != SwapDebtToUSDCStep || converted.Request.AmountRaw != 1 || converted.Request.MinimumOutputRaw != 1 {
		t.Fatalf("residue conversion leg drifted: %+v", converted.Request)
	}
	proceeds := uint64(0)
	for _, leg := range q.PayoffLegs[1:] {
		if proceeds, err = budgetSumU64(proceeds, leg.Request.MinimumOutputRaw); err != nil {
			t.Fatal(err)
		}
	}
	if proceeds == 0 || q.PayoffLeftoverCollateralRaw == 0 {
		t.Fatalf("retained proceeds or leftover custody lost: proceeds=%d leftover=%d", proceeds, q.PayoffLeftoverCollateralRaw)
	}
	var actions []Action
	for _, input := range q.Recipe.Inputs {
		request, _, _, err := input.decodeWithManifest(m)
		if err != nil {
			t.Fatal(err)
		}
		switch r := request.(type) {
		case BridgeBuildRequest:
			actions = append(actions, r.Action)
		case JupiterSwapRequest:
			actions = append(actions, r.Action)
		case KaminoPrimeUSDCRequest:
			actions = append(actions, r.Action)
		default:
			t.Fatalf("unexpected recipe input type %T", request)
		}
	}
	want := []Action{
		VoltrAllocateToSquads, ReportNAV, SwapStableToCollateralStep, ReportNAV,
		OpenRouteStep, ReportNAV, OpenRouteStep, ReportNAV, SwapDebtToCollateralStep, ReportNAV,
		OpenRouteStep, ReportNAV,
		DeleverRouteStep, ReportNAV, SwapCollateralToDebtStep, DeleverRouteStep, ReportNAV,
		DeleverRouteStep, ReportNAV, SwapDebtToUSDCStep, SwapCollateralToStableStep,
		StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV,
	}
	if len(actions) != len(want) || len(actions) != 24 {
		t.Fatal("recipe window count drifted", len(actions))
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("recipe step %d is %s, want %s", i, actions[i], want[i])
		}
	}
	if len(q.Recipe.Costs) != len(q.Recipe.Inputs) {
		t.Fatal("not every retained message is costed")
	}
}

// The three pre-candidate entry surfaces keep rejecting the AUTO lane even
// when the manifest carries a fully valid candidate binding. Only the
// explicit candidate entry admits it.
func TestSelectorDestinationCandidateKeepsPublicGatesClosed(t *testing.T) {
	const slot = int64(77)
	m, _, rpc, client := autoCandidateStack(t, slot, nil)
	if _, err := m.autoPolicyBinding(); err != nil {
		t.Fatal("fixture manifest lost the candidate binding")
	}
	if _, err := observeSelectorDestination(context.Background(), rpc, client, m, testAutoLane, 1_000_000, slot); err == nil {
		t.Fatal("public destination priced the candidate lane")
	} else {
		assertBudgetHold(t, err, "invalid_selector_destination")
	}
	if _, err := observeSelectorDestinationSize(context.Background(), rpc, client, m, testAutoLane, 1_000_000, slot, true); err == nil {
		t.Fatal("size evaluator priced the candidate lane")
	} else {
		assertBudgetHold(t, err, "invalid_selector_destination")
	}
	if _, err := observeSelectorDestinationForecast(context.Background(), rpc, client, m, testAutoLane, 1_000_000, slot, false, nil); err == nil {
		t.Fatal("forecast priced the candidate lane")
	} else {
		assertBudgetHold(t, err, "invalid_selector_destination")
	}
}

// Without the validated binding the candidate entry itself fails closed
// before any account read.
func TestSelectorDestinationCandidateFailsClosedWithoutAutoBinding(t *testing.T) {
	m, _, rpc, client := autoCandidateStack(t, 77, nil)
	m.RuntimeBindings.AutoPolicy = nil
	_, err := observeSelectorDestinationCandidate(context.Background(), rpc, client, m, 1_000_000, 77)
	assertBudgetHold(t, err, "auto_policy_not_activated")
}

// An absent AUTO obligation still cannot be initialized: the reviewed
// candidate binding carries no appended initializer constraint entry, so the
// initializer's own gate holds the graph before any activation scope is
// invented for the lane.
func TestSelectorDestinationCandidateHoldsAbsentObligation(t *testing.T) {
	route, err := runtimeRoute(testAutoLane)
	if err != nil {
		t.Fatal(err)
	}
	m, _, rpc, client := autoCandidateStack(t, 77, func(accounts []ConfirmedAccount) {
		for i := range accounts {
			if accounts[i].Address == route.Kamino.Obligation {
				accounts[i].Lamports, accounts[i].Data = 0, nil
			}
		}
	})
	_, err = observeSelectorDestinationCandidate(context.Background(), rpc, client, m, 1_000_000, 77)
	assertBudgetHold(t, err, "auto_initializer_constraint_not_reviewed")
}

// The candidate readiness pins exactly the ONE combined reviewed AUTO policy:
// an absent or wrong-bytes combined policy image holds the quote, while the
// four unrelated basic policy families are absent entirely without blocking
// the lane (the positive test above fetches none of them).
func TestSelectorDestinationCandidateRequiresCombinedAutoPolicy(t *testing.T) {
	for name, breakPolicy := range map[string]func(accounts []ConfirmedAccount){
		"missing": func(accounts []ConfirmedAccount) {
			for i := range accounts {
				if accounts[i].Address == autoFixturePolicy {
					accounts[i].Lamports, accounts[i].Data = 0, nil
				}
			}
		},
		"wrong_bytes": func(accounts []ConfirmedAccount) {
			accountAt(accounts, autoFixturePolicy).Data[100] ^= 0x40
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, _, rpc, client := autoCandidateStack(t, 77, breakPolicy)
			_, err := observeSelectorDestinationCandidate(context.Background(), rpc, client, m, 1_000_000, 77)
			assertBudgetHold(t, err, "selector_destination_policy_unavailable")
		})
	}
}

// autoCandidateInitializerRent is the transport's rent-exemption stub for the
// obligation size: (128+bytes) lamports per byte-year across the two-year
// exemption threshold at 3480 lamports. The initializer prestate re-derives
// the same product from its rent sysvar, so both sides must stay aligned.
func autoCandidateInitializerRent() uint64 {
	return uint64((128 + kaminoObligationLength) * 3480 * 2)
}

// autoCandidateInitializerStack upgrades the coherent candidate stack to A's
// eight-constraint initializer binding: the manifest binding is swapped to the
// superseding reviewed fixture, the flat batch loses its obligation so the
// producer must price recreation (or, with funded, carries the exact bounded
// fixture position the forecast exit closes), and the transport gains a server
// for the initializer prestate read — the one getMultipleAccounts whose
// address list carries the user metadata PDA — holding exactly the
// initializer's account graph. The variant hook rewrites the served prestate
// accounts for the refusal cases; the obligation is absent from the served
// map unless a variant adds it back.
func autoCandidateInitializerStack(t *testing.T, slot int64, responseSlot int64, funded bool, variant func(route RuntimeRoute, prestate map[string]ConfirmedAccount)) (RouteManifest, RuntimeRoute, *RPCClient, *jupiterClient) {
	t.Helper()
	binding := autoInitializerFixtureBinding(t)
	route, err := runtimeRoute(testAutoLane)
	if err != nil {
		t.Fatal(err)
	}
	var batch []ConfirmedAccount
	m, _, rpc, client := autoCandidateStack(t, slot, func(accounts []ConfirmedAccount) {
		if funded {
			// Coherent 9dp position economics: pin the receipt exchange rate
			// to 1:1 (the pool's 100e9 raw liquidity over 100e9 outstanding
			// receipts) so the fixture deposit of 15e9 receipts redeems to
			// exactly 15 AUTO of collateral against the 5e6-raw (6dp) PYUSD
			// debt, with the receipts fully backed by the observed pool.
			binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.CollateralReserve).Data[2592:2600], 100_000_000_000)
		}
		for i := range accounts {
			if accounts[i].Address == route.Kamino.Obligation {
				if funded {
					// The bounded reentry forecast validates recreation of
					// the FUNDED same-lane obligation against its supplied
					// source exit bound — it is not a proved complete
					// unwind. The position is re-bound to the AUTO topology;
					// custody residue stays zero, so the forecast's
					// collateral idle is zero.
					image := obligationFixture(t, slot, 15_000_000_000, 5_000_000)
					putKey(t, image.Data[32:64], route.Kamino.Market)
					putKey(t, image.Data[96:128], route.Kamino.CollateralReserve)
					putKey(t, image.Data[1208:1240], route.Kamino.DebtReserve)
					accounts[i].Data = image.Data
				} else {
					accounts[i].Lamports, accounts[i].Data = 0, nil
				}
			}
			// Readiness pins the binding's own policy account by its raw bytes
			// hash: serve the reviewed superseding image at its derived address.
			if accounts[i].Address == autoFixturePolicy {
				accounts[i].Address = autoInitializerFixturePolicy
				accounts[i].Data = []byte(autoInitializerFixtureSyntheticAccountData)
			}
		}
		batch = append(batch, accounts...)
	})
	// The reviewed superseding binding drives compile and prestate identity.
	m.RuntimeBindings.AutoPolicy = &binding
	inner, err := kaminoRouteInitializer(route)
	if err != nil {
		t.Fatal(err)
	}
	metadataAddress := encodeBase58(inner.accounts[6].key[:])
	prestate := map[string]ConfirmedAccount{}
	for _, a := range append(append([]ConfirmedAccount(nil), batch...), autoPayoffMints(t, route)...) {
		prestate[a.Address] = a
	}
	// Native funding and derived accounts, mirroring A's coherent initializer
	// fixture: settings seed counter exactly the candidate seed, the reviewed
	// policy image carrying its synthetic bytes, the 1032-byte user metadata
	// naming the vault, the reviewed market image, and the rent sysvar aligned
	// with the transport's rent stub above.
	settings := setupSettingsAccount(t)
	binary.LittleEndian.PutUint64(settings.Data[159:167], binding.PolicySeed)
	prestate[bridgeSettings] = settings
	prestate[autoInitializerFixturePolicy] = ConfirmedAccount{Address: autoInitializerFixturePolicy, Owner: bridgeSquadsProgram, Lamports: 1, Data: []byte(autoInitializerFixtureSyntheticAccountData)}
	prestate[bridgeVault] = ConfirmedAccount{Address: bridgeVault, Owner: "11111111111111111111111111111111", Lamports: 1_000_000_000}
	prestate[bridgeDelegate] = ConfirmedAccount{Address: bridgeDelegate, Owner: "11111111111111111111111111111111", Lamports: 1_000_000_000}
	rent := ConfirmedAccount{Address: "SysvarRent111111111111111111111111111111111", Owner: "Sysvar1111111111111111111111111111111111111", Lamports: 1, Data: make([]byte, 17)}
	binary.LittleEndian.PutUint64(rent.Data, 6960)
	binary.LittleEndian.PutUint64(rent.Data[8:16], math.Float64bits(1))
	prestate["SysvarRent111111111111111111111111111111111"] = rent
	prestate[route.Kamino.Market] = marketFixture(t, route.Kamino.Market)
	for _, meta := range inner.accounts {
		address := encodeBase58(meta.key[:])
		if _, ok := prestate[address]; !ok {
			prestate[address] = ConfirmedAccount{Address: address, Owner: "11111111111111111111111111111111", Lamports: 1}
		}
	}
	metadata := prestate[metadataAddress]
	metadata = ConfirmedAccount{Address: metadataAddress, Owner: kaminoProgram, Lamports: 1, Data: make([]byte, 1032)}
	copy(metadata.Data, []byte{157, 214, 220, 235, 98, 135, 171, 28})
	putKey(t, metadata.Data[80:112], bridgeVault)
	prestate[metadataAddress] = metadata
	if variant != nil {
		variant(route, prestate)
	}
	base := rpc.client.Transport
	rpc.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		var call struct {
			ID     any               `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if json.Unmarshal(body, &call) != nil || call.Method != "getMultipleAccounts" {
			return base.RoundTrip(request)
		}
		var addresses []string
		if json.Unmarshal(call.Params[0], &addresses) != nil {
			return base.RoundTrip(request)
		}
		pinned := false
		for _, address := range addresses {
			pinned = pinned || address == metadataAddress
		}
		if !pinned {
			return base.RoundTrip(request)
		}
		values := make([]any, len(addresses))
		for i, address := range addresses {
			if a, ok := prestate[address]; ok {
				values[i] = map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": false,
					"data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}}
			}
		}
		encoded, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": call.ID,
			"result": map[string]any{"context": map[string]any{"slot": responseSlot}, "value": values}})
		if err != nil {
			return nil, err
		}
		return response(string(encoded)), nil
	})
	return m, route, rpc, client
}

// Doc 17: with A's eight-constraint initializer binding reviewed in the
// manifest, the destination producer admits an ABSENT obligation — it compiles
// the initializer against the reviewed binding, prices its exact rent and fee
// as the recipe's FIRST step, and retains the binding identity — while the
// seven-constraint manifest above keeps holding the identical graph.
func TestSelectorDestinationCandidateAdmitsAbsentObligationWithInitializer(t *testing.T) {
	const (
		slot   = int64(77)
		equity = uint64(2_000_000) // coherent parity economics: zero guaranteed residue
	)
	m, route, rpc, client := autoCandidateInitializerStack(t, slot, slot, false, nil)
	binding := autoInitializerFixtureBinding(t)
	q, err := observeSelectorDestinationCandidate(context.Background(), rpc, client, m, equity, slot)
	if err != nil {
		t.Fatal(err)
	}
	// First retained input is the initializer request itself: exact reviewed
	// binding identity, the transport's exact rent, the observed fee bound.
	request, effects, message, err := q.Recipe.Inputs[0].decodeWithManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	init, ok := request.(KaminoInitializationRequest)
	if !ok || effects.Kind != "kamino-initialize" || !effects.Conserved || effects.Initialization == nil || *effects.Initialization != init {
		t.Fatalf("initializer step drifted: %+v %+v", request, effects)
	}
	if init.RouteLane != route.Lane || init.PolicySeed != binding.PolicySeed || init.PolicyAccountDataSHA256 != binding.AccountDataSHA256 {
		t.Fatalf("initializer lost the reviewed binding identity: %+v", init)
	}
	if init.RentLamports != autoCandidateInitializerRent() || init.MaximumFeeLamports != 5000 {
		t.Fatalf("initializer rent/fee drifted: %+v", init)
	}
	// The retained message is exactly the manifest compile, wrapped at the
	// appended eighth constraint index of the reviewed policy.
	manifestMessage, err := m.compileKaminoInitializationMessage(init)
	if err != nil || !bytes.Equal(manifestMessage, message) {
		t.Fatalf("retained initializer message is not the manifest compile: %v", err)
	}
	if wrapped := bytes.Index(message, squadsExecuteSyncDiscriminator); wrapped < 0 || wrapped+17 >= len(message) || message[wrapped+17] != autoInitializerConstraintIndex {
		t.Fatalf("initializer not wrapped at the appended index %d", autoInitializerConstraintIndex)
	}
	// Rent is priced FIRST, ahead of every entry step.
	if second, _, _, err := q.Recipe.Inputs[1].decodeWithManifest(m); err != nil {
		t.Fatal(err)
	} else if bridge, ok := second.(BridgeBuildRequest); !ok || bridge.Action != VoltrAllocateToSquads {
		t.Fatalf("initializer is not the first priced step: %T", second)
	}
	if q.Recipe.SetupLamports != init.RentLamports || len(q.Recipe.Costs) != len(q.Recipe.Inputs) || q.Recipe.Costs[0].Fee.Lamports != 5000 {
		t.Fatalf("initializer rent or costing drifted: setup=%d costs=%d", q.Recipe.SetupLamports, len(q.Recipe.Costs))
	}
	// The identical one-pass graph as the flat candidate plus exactly the
	// initializer step, still inside the bounded compounding windows.
	if len(q.Recipe.Inputs) != 24 || int64(len(q.Recipe.Inputs)) > selectorFullRecipeWindows {
		t.Fatal("recipe window count drifted", len(q.Recipe.Inputs))
	}
}

// The initializer prestate stays a live admission surface even inside the
// destination forecast: an obligation that appears between the flat scan and
// the prestate read, unfunded native accounts, and a prestate observation
// outside the freshness window each hold the graph.
func TestSelectorDestinationCandidateInitializerPrestateRefusals(t *testing.T) {
	for name, variant := range map[string]struct {
		responseSlot int64
		serve        func(route RuntimeRoute, prestate map[string]ConfirmedAccount)
		hold         string
	}{
		"existing_obligation": {
			responseSlot: 77,
			serve: func(route RuntimeRoute, prestate map[string]ConfirmedAccount) {
				prestate[route.Kamino.Obligation] = ConfirmedAccount{Address: route.Kamino.Obligation, Owner: kaminoProgram, Lamports: 1, Data: []byte{1}}
			},
			hold: "initializer_obligation_already_present",
		},
		"unfunded_vault": {
			responseSlot: 77,
			serve: func(_ RuntimeRoute, prestate map[string]ConfirmedAccount) {
				prestate[bridgeVault] = ConfirmedAccount{Address: bridgeVault, Owner: "11111111111111111111111111111111", Lamports: 1}
			},
			hold: "initializer_native_funding_unavailable",
		},
		"stale_prestate": {
			responseSlot: 77 + budgetMaxObservationLagSlots + 1,
			serve:        nil,
			hold:         "selector_recipe_observation_expired",
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, _, rpc, client := autoCandidateInitializerStack(t, 77, variant.responseSlot, false, variant.serve)
			_, err := observeSelectorDestinationCandidate(context.Background(), rpc, client, m, 2_000_000, 77)
			assertBudgetHold(t, err, variant.hold)
		})
	}
}

// The decode-only path binds a retained initializer to the manifest that
// produced it: drifted seed or hash refuses against the reviewed binding, a
// seven-constraint manifest never implies the eighth constraint, and the
// embedded production manifest stays closed. The honest request decodes to
// exactly the manifest compile.
func TestCandidateInitializerDecodeOnlyAcceptsReviewedBinding(t *testing.T) {
	m := autoInitializerFixtureManifest(t)
	binding := autoInitializerFixtureBinding(t)
	r, err := m.initializationRequest(testAutoLane, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 100}, autoCandidateInitializerRent(), 5000)
	if err != nil {
		t.Fatal(err)
	}
	if r.PolicySeed != binding.PolicySeed || r.PolicyAccountDataSHA256 != binding.AccountDataSHA256 {
		t.Fatalf("bound request lost the reviewed identity: %+v", r)
	}
	encoded, err := jsonMarshalExpectedEffects(ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &r})
	if err != nil {
		t.Fatal(err)
	}
	input, err := encodePhase3BuildInput(r, encoded)
	if err != nil {
		t.Fatal(err)
	}
	decoded, effects, message, err := input.decodeWithManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != any(r) || effects.Kind != "kamino-initialize" || *effects.Initialization != r {
		t.Fatal("honest initializer decode drifted")
	}
	manifestMessage, err := m.compileKaminoInitializationMessage(r)
	if err != nil || !bytes.Equal(manifestMessage, message) {
		t.Fatalf("decoded initializer message is not the manifest compile: %v", err)
	}
	for name, drifted := range map[string]KaminoInitializationRequest{
		"seed": {RouteLane: r.RouteLane, PolicySeed: autoFixtureSeed, PolicyAccountDataSHA256: r.PolicyAccountDataSHA256, RecentBlockhash: r.RecentBlockhash, LastValidBlockHeight: r.LastValidBlockHeight, RentLamports: r.RentLamports, MaximumFeeLamports: r.MaximumFeeLamports},
		"hash": {RouteLane: r.RouteLane, PolicySeed: r.PolicySeed, PolicyAccountDataSHA256: sha256Bytes([]byte("other candidate bytes")), RecentBlockhash: r.RecentBlockhash, LastValidBlockHeight: r.LastValidBlockHeight, RentLamports: r.RentLamports, MaximumFeeLamports: r.MaximumFeeLamports},
	} {
		// The manifest compile refuses drifted identity with the typed
		// mismatch hold; the persisted decode boundary wraps every compile
		// refusal as a build that no longer compiles under THIS manifest.
		if _, err := m.compileKaminoInitializationMessage(drifted); err == nil {
			t.Fatalf("%s: drifted initializer compiled", name)
		} else {
			assertBudgetHold(t, err, "initializer_request_manifest_mismatch")
		}
		driftedInput, err := encodePhase3BuildInput(drifted, encoded)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := driftedInput.decodeWithManifest(m); err == nil {
			t.Fatalf("%s: drifted initializer decoded", name)
		} else {
			assertBudgetHold(t, err, "persisted_build_no_longer_compiles")
		}
	}
	seven := autoFixtureManifest(t)
	// The seven-constraint binding never implies the appended eighth
	// constraint, at the compile or through the decode boundary.
	if _, err := seven.compileKaminoInitializationMessage(r); err == nil {
		t.Fatal("seven-constraint manifest compiled the initializer")
	} else {
		assertBudgetHold(t, err, "auto_initializer_constraint_not_reviewed")
	}
	if _, _, _, err := input.decodeWithManifest(seven); err == nil {
		t.Fatal("seven-constraint manifest decoded the initializer")
	} else {
		assertBudgetHold(t, err, "persisted_build_no_longer_compiles")
	}
	// Both binding states keep the persisted decode boundary closed: the
	// explicit absent fixture (the shipped pre-install state) holds the compile
	// with the shipped token, and the embedded manifest's installed binding
	// refuses the candidate identity with the exact typed mismatch — in both
	// cases the build no longer compiles under THAT manifest.
	_, absentErr := autoAbsentBindingManifest(t).compileKaminoInitializationMessage(r)
	assertBudgetHold(t, absentErr, "auto_policy_not_activated")
	embedded := requireEmbeddedInstalledBinding(t)
	_, embeddedErr := embedded.compileKaminoInitializationMessage(r)
	assertBudgetHold(t, embeddedErr, "initializer_request_manifest_mismatch")
	if _, _, _, err := input.decodeWithManifest(embedded); err == nil {
		t.Fatal("embedded manifest decoded the initializer")
	} else {
		assertBudgetHold(t, err, "persisted_build_no_longer_compiles")
	}
	if _, _, _, err := input.decodeWithManifest(autoAbsentBindingManifest(t)); err == nil {
		t.Fatal("absent manifest decoded the initializer")
	} else {
		assertBudgetHold(t, err, "persisted_build_no_longer_compiles")
	}
	// The installed state resolves its own request end to end: the request
	// built from the embedded manifest carries exactly the installed identity
	// and decodes through the persisted boundary.
	installedRequest, err := embedded.initializationRequest(testAutoLane, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 100}, autoCandidateInitializerRent(), 5000)
	if err != nil || installedRequest.PolicySeed != installedAutoPolicySeed || installedRequest.PolicyAccountDataSHA256 != installedAutoPolicyDigest {
		t.Fatalf("installed initializer request drifted: %+v %v", installedRequest, err)
	}
	installedEncoded, err := jsonMarshalExpectedEffects(ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &installedRequest})
	if err != nil {
		t.Fatal(err)
	}
	installedInput, err := encodePhase3BuildInput(installedRequest, installedEncoded)
	if err != nil {
		t.Fatal(err)
	}
	if decodedRequest, _, _, err := installedInput.decodeWithManifest(embedded); err != nil || decodedRequest != any(installedRequest) {
		t.Fatalf("installed initializer did not decode through the embedded manifest: %v", err)
	}
}

// The narrow candidate reentry entry shares the exact authorized body behind
// the candidate entry and refuses to run without a bound; the public reentry
// wrapper keeps refusing the candidate lane outright.
func TestSelectorDestinationCandidateReentryEntryStaysGated(t *testing.T) {
	const slot = int64(77)
	m, _, rpc, client := autoCandidateInitializerStack(t, slot, slot, false, nil)
	if _, err := observeSelectorDestinationCandidateReentry(context.Background(), rpc, client, m, 2_000_000, slot, nil); err == nil {
		t.Fatal("candidate reentry priced without a bound")
	} else {
		assertBudgetHold(t, err, "invalid_selector_destination")
	}
	m.RuntimeBindings.AutoPolicy = nil
	if _, err := observeSelectorDestinationCandidateReentry(context.Background(), rpc, client, m, 2_000_000, slot, &selectorReentryForecast{bound: selectorExitBound{MaxCollateralRaw: 1, MaxDebtRaw: 1}}); err == nil {
		t.Fatal("candidate reentry priced without the reviewed binding")
	} else {
		assertBudgetHold(t, err, "auto_policy_not_activated")
	}
	// The public production reentry wrapper refuses AUTO regardless of the
	// manifest's reviewed binding.
	m.RuntimeBindings.AutoPolicy = nil
	funded := Observation{Snapshot: Snapshot{RouteLane: testAutoLane, StrategyKey: testAutoLane, PilotActive: true, Fresh: true, ObligationPresenceKnown: true, ObligationPresent: true, HasPosition: true, PositionCollateralRaw: 10_000_000_000, PositionDebtRaw: 5_000_000, StrategyNAVRaw: 1, Slot: slot, ObservationID: "candidate-observation"}}
	source := selectorSourceQuote{Lane: testAutoLane, ObservationID: funded.Snapshot.ObservationID, ExitBound: &selectorExitBound{MaxCollateralRaw: 10_000_000_000, MaxDebtRaw: 5_000_000}, Recipe: selectorRecipe{EvidenceID: sha256Bytes([]byte("candidate-exit")), ValidThroughSlot: slot + 1}}
	if _, err := observeSelectorReentryDestinationSize(context.Background(), rpc, client, m, funded, source, 2_000_000, false); err == nil {
		t.Fatal("public reentry wrapper priced the candidate lane")
	} else {
		assertBudgetHold(t, err, "selector_reentry_destination_unavailable")
	}
}

// Doc 17: the bounded reentry forecast validates recreation pricing against a
// SUPPLIED source exit bound over the observed funded same-lane position — the
// bound admits the funded obligation into the forecast-only prestate and the
// recipe still leads with the exact initializer recreation rent and fee. This
// proves the recreation half of the loop from the given bound; it is NOT a
// proved complete unwind (the bound's own exit economics are the source
// quote's, not re-proved here). The strictly absent-only EXECUTION prestate
// behind the same manifest stays enforced.
func TestSelectorDestinationCandidateReentryPricesBoundedRecreation(t *testing.T) {
	const (
		slot       = int64(77)
		equity     = uint64(2_000_000)
		collateral = int64(15_000_000_000) // 15 AUTO of receipts at the pinned 1:1 rate (9dp)
		debt       = int64(5_000_000)      // 5 PYUSD (6dp)
	)
	m, route, rpc, client := autoCandidateInitializerStack(t, slot, slot, true, nil)
	binding := autoInitializerFixtureBinding(t)
	bound := selectorExitBound{MaxCollateralRaw: collateral, MaxDebtRaw: debt}
	q, err := observeSelectorDestinationCandidateReentry(context.Background(), rpc, client, m, equity, slot, &selectorReentryForecast{bound: bound})
	if err != nil {
		t.Fatal(err)
	}
	// Recreation is still priced FIRST: exact reviewed binding identity, the
	// transport's exact rent and the observed fee, wrapped at the appended
	// eighth constraint of the reviewed policy — despite the obligation being
	// observed present.
	request, effects, message, err := q.Recipe.Inputs[0].decodeWithManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	init, ok := request.(KaminoInitializationRequest)
	if !ok || effects.Kind != "kamino-initialize" || !effects.Conserved || effects.Initialization == nil || *effects.Initialization != init {
		t.Fatalf("initializer step drifted: %+v %+v", request, effects)
	}
	if init.RouteLane != route.Lane || init.PolicySeed != binding.PolicySeed || init.PolicyAccountDataSHA256 != binding.AccountDataSHA256 {
		t.Fatalf("initializer lost the reviewed binding identity: %+v", init)
	}
	if init.RentLamports != autoCandidateInitializerRent() || init.MaximumFeeLamports != 5000 {
		t.Fatalf("initializer rent/fee drifted: %+v", init)
	}
	manifestMessage, err := m.compileKaminoInitializationMessage(init)
	if err != nil || !bytes.Equal(manifestMessage, message) {
		t.Fatalf("retained initializer message is not the manifest compile: %v", err)
	}
	if wrapped := bytes.Index(message, squadsExecuteSyncDiscriminator); wrapped < 0 || wrapped+17 >= len(message) || message[wrapped+17] != autoInitializerConstraintIndex {
		t.Fatalf("initializer not wrapped at the appended index %d", autoInitializerConstraintIndex)
	}
	if second, _, _, err := q.Recipe.Inputs[1].decodeWithManifest(m); err != nil {
		t.Fatal(err)
	} else if bridge, ok := second.(BridgeBuildRequest); !ok || bridge.Action != VoltrAllocateToSquads {
		t.Fatalf("initializer is not the first priced step: %T", second)
	}
	if q.Recipe.SetupLamports != init.RentLamports || len(q.Recipe.Costs) != len(q.Recipe.Inputs) || q.Recipe.Costs[0].Fee.Lamports != 5000 {
		t.Fatalf("initializer rent or costing drifted: setup=%d costs=%d", q.Recipe.SetupLamports, len(q.Recipe.Costs))
	}
	if len(q.Recipe.Inputs) != 24 || int64(len(q.Recipe.Inputs)) > selectorFullRecipeWindows {
		t.Fatal("recipe window count drifted", len(q.Recipe.Inputs))
	}
	// The EXECUTION admission gate on the same reviewed manifest stays strictly
	// absent-only: priced against this exact funded state it must refuse.
	execution, err := m.initializationRequest(testAutoLane, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, autoCandidateInitializerRent(), 1)
	if err != nil {
		t.Fatal(err)
	}
	execution.MaximumFeeLamports = 5000
	_, err = m.observePhase3KnownBuildCost(context.Background(), rpc, execution, ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &execution})
	assertBudgetHold(t, err, "initializer_obligation_already_present")
}
