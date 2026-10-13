package backyard

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// The synthetic stacks the selector tests price on: a flat AUTO/AUTO/PYUSD
// lane behind a fake chain and a Jupiter that quotes every AUTO edge.

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

// autoJupiterTransport serves the five AUTO edges through the reviewed
// instruction shape; `quote` maps each edge to its fixture's integer
// economics, and `probe`, when set and returning true, overrides the quote
// economics for that exact request.
func autoJupiterTransport(t *testing.T, route RuntimeRoute, quote func(in, destination string, amount uint64) (quoted, minimum uint64), probe func(amount uint64) (out, minimum uint64, ok bool)) *jupiter.Client {
	t.Helper()
	client, err := fixtureJupiter(roundTripFunc(func(req *http.Request) (*http.Response, error) {
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
			payload := jupiter.Quote{InputMint: in, OutputMint: destination, InAmount: fmt.Sprint(amount), OutAmount: fmt.Sprint(quoted), OtherAmountThreshold: fmt.Sprint(minimum), SwapMode: "ExactIn", SlippageBPS: 50, RoutePlan: []json.RawMessage{json.RawMessage(`{}`)}}
			raw, _ := json.Marshal(payload)
			return response(string(raw)), nil
		case "/swap-instructions":
			var body struct {
				QuoteResponse jupiter.Quote
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
	}))
	if err != nil {
		t.Fatal(err)
	}
	return client
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

func autoCandidateJupiter(t *testing.T, route RuntimeRoute) *jupiter.Client {
	t.Helper()
	return autoJupiterTransport(t, route, autoCandidateQuote(route), nil)
}

// autoPayoffRPC serves every fee, account and slot read at the ONE fixture
// slot and unix timestamp the batch, Clock and producer already use. The
// shared fee/valuation fixtures are re-stamped fresh at the batch's own
// timestamp so no observer falls into the refresh-simulation fallback; the
// production freshness checks themselves run unmodified.
func autoPayoffRPC(t *testing.T, slot int64, batch []ConfirmedAccount) *chain.Client {
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
	base := rpcOf(rpc).Transport
	rpcOf(rpc).Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
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
func autoCandidateStack(t *testing.T, slot int64, extra func([]ConfirmedAccount)) (RouteManifest, RuntimeRoute, *chain.Client, *jupiter.Client) {
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
			ensure(ConfirmedAccount{Address: oracle, Owner: kamino.ProgramID.String(), Lamports: 1, Data: []byte{1}})
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

const testAutoLane = "AUTO/AUTO/PYUSD"

// testLanePricer is a sample's pricer over a fixture chain: the captured
// policies and the chain's SOL price at slot.
func testLanePricer(t *testing.T, rpc *chain.Client, client *jupiter.Client, m RouteManifest, slot int64) lanePricer {
	t.Helper()
	view := fixtureView(t, rpc)
	sol, err := ObserveNativeSOLBudgetPrice(context.Background(), rpc, view, slot)
	if err != nil {
		t.Fatal(err)
	}
	return lanePricer{rpc: rpc, view: view, client: client, manifest: m, policies: capturedTestPolicies(), sol: sol}
}

// flatEntryObservation is a flat vault with equity in Voltr idle, sampled at
// slot.
func flatEntryObservation(slot, equity int64) Observation {
	s := base()
	s.Slot, s.ObservationID, s.VoltrIdleRaw, s.TotalVaultNAVRaw = slot, "flat-entry", equity, equity
	o := tickObservation(s)
	o.ObservedAt = time.Now().UTC()
	return o
}
