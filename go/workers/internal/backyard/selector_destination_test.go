package backyard

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// Controlled real-layout accounts and captured Jupiter instruction topology.
// Neither fixture transport supports simulation/signing/submission.
func selectorDestinationFixture(t *testing.T) (RouteManifest, *chain.Client, *jupiter.Client, []ConfirmedAccount) {
	t.Helper()
	return selectorDestinationFixtureForLane(t, SelectedRouteID, nil)
}

// selectorDestinationFixtureForLane builds the same controlled destination
// for any basic-policy lane; tweak may edit the accounts before the RPC
// fixture captures them.
func selectorDestinationFixtureForLane(t *testing.T, lane string, tweak func([]ConfirmedAccount)) (RouteManifest, *chain.Client, *jupiter.Client, []ConfirmedAccount) {
	t.Helper()
	m := embeddedTestManifest(t)
	route, _ := runtimeRoute(lane)
	var accounts []ConfirmedAccount
	add := func(a ConfirmedAccount) { accounts = append(accounts, a) }
	one := new(big.Int).Lsh(big.NewInt(1), 60)
	for _, side := range []struct{ reserve, mint, supply, farm string }{
		{route.Kamino.CollateralReserve, route.Kamino.CollateralMint, route.CollateralLiquiditySupply, ""},
		{route.Kamino.DebtReserve, bridgeUSDC, route.DebtLiquiditySupply, route.DebtFarm},
	} {
		a := reserveFixture(t, side.reserve, side.mint, 42, one, 1_000_000_000, 1_000_000_000)
		putKey(t, a.Data[32:64], route.Kamino.Market)
		putKey(t, a.Data[160:192], side.supply)
		putKey(t, a.Data[408:440], classicTokenProgram)
		putKey(t, a.Data[5112:5144], kaminoScopePrices)
		binary.LittleEndian.PutUint64(a.Data[264:272], 1000)
		for _, off := range []int{kaminoReserveConfigOffset + 160, kaminoReserveConfigOffset + 168, kaminoOutsideBorrowLimitOffset} {
			binary.LittleEndian.PutUint64(a.Data[off:], 10_000_000_000)
		}
		binary.LittleEndian.PutUint64(a.Data[kaminoBorrowFactorOffset:], 100)
		a.Data[kaminoLoanToValueOffset], a.Data[kaminoReserveConfigOffset+17] = 80, 85
		for i := 1; i < 11; i++ {
			binary.LittleEndian.PutUint32(a.Data[kaminoReserveConfigOffset+64+i*8:], 10_000)
		}
		if side.farm != "" {
			putKey(t, a.Data[96:128], side.farm)
		}
		if side.mint == bridgeUSDC {
			putKey(t, a.Data[192:224], route.DebtFeeReceiver)
		} else {
			putKey(t, a.Data[2560:2592], route.CollateralReceiptMint)
			putKey(t, a.Data[2600:2632], route.CollateralReceiptSupply)
		}
		add(a)
	}
	add(ConfirmedAccount{Address: kaminoScopePrices, Owner: kamino.ProgramID.String(), Lamports: 1, Data: []byte{1}})
	add(marketFixture(t, route.Kamino.Market))
	o := obligationFixture(t, 42, 0, 0)
	o.Address = route.Kamino.Obligation
	putKey(t, o.Data[32:64], route.Kamino.Market)
	add(o)
	clock := clockFixture()
	binary.LittleEndian.PutUint64(clock.Data[:8], 42)
	binary.LittleEndian.PutUint64(clock.Data[32:40], 1000)
	add(clock)
	for _, mint := range []string{route.Kamino.CollateralMint, route.CollateralReceiptMint} {
		a := ConfirmedAccount{Address: mint, Owner: classicTokenProgram, Lamports: 1, Data: make([]byte, 82)}
		a.Data[44], a.Data[45] = 6, 1
		if mint == route.CollateralReceiptMint {
			binary.LittleEndian.PutUint32(a.Data[:4], 1)
			putKey(t, a.Data[4:36], route.Kamino.MarketAuthority)
		}
		add(a)
	}
	for _, b := range []kaminoCustodyBoundary{{route.CollateralCustody, route.Kamino.CollateralMint, bridgeVault}, {bridgeSquadsATA, bridgeUSDC, bridgeVault},
		{route.CollateralLiquiditySupply, route.Kamino.CollateralMint, route.Kamino.MarketAuthority}, {route.DebtLiquiditySupply, bridgeUSDC, route.Kamino.MarketAuthority},
		{route.DebtFeeReceiver, bridgeUSDC, route.Kamino.MarketAuthority}, {route.CollateralReceiptSupply, route.CollateralReceiptMint, route.Kamino.MarketAuthority}} {
		amount := uint64(0)
		if b.Authority != bridgeVault {
			amount = 1_000_000_000
		}
		add(tokenAccountFixture(t, b.Address, b.Mint, b.Authority, amount))
	}
	add(exactAdaptorConfigAccount(t))
	add(exactReportTicketAccount(t, 1))
	for _, a := range []string{bridgeVault, bridgeDelegate} {
		add(ConfirmedAccount{Address: a, Owner: "11111111111111111111111111111111", Lamports: 1_000_000_000})
	}
	farm := ConfirmedAccount{Address: route.DebtFarm, Owner: kamino.FarmsProgramID.String(), Lamports: 1, Data: make([]byte, 8336)}
	copy(farm.Data, []byte{198, 102, 216, 74, 63, 66, 163, 190})
	putKey(t, farm.Data[7328:7360], route.Kamino.MarketAuthority)
	farm.Data[7362] = 1
	add(farm)
	u := ConfirmedAccount{Address: route.ObligationDebtFarm, Owner: kamino.FarmsProgramID.String(), Lamports: 1, Data: make([]byte, 920)}
	copy(u.Data, kamino.FarmUserStateDiscriminator[:])
	putKey(t, u.Data[16:48], route.DebtFarm)
	putKey(t, u.Data[48:80], bridgeVault)
	u.Data[80] = 1
	putKey(t, u.Data[480:512], route.Kamino.Obligation)
	add(u)
	captured := basicJupiterRequest(t, route.Lane, "USDC->"+route.CollateralSymbol)
	reverse := basicJupiterRequest(t, route.Lane, route.CollateralSymbol+"->USDC")
	if tweak != nil {
		tweak(accounts)
	}
	rpc := budgetBuildRPCWithAccounts(t, 5000, 42, accounts)
	client, err := fixtureJupiter(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var payload any
		switch req.URL.Path {
		case "/quote":
			amount, _ := strconv.ParseUint(req.URL.Query().Get("amount"), 10, 64)
			payload = jupiter.Quote{InputMint: req.URL.Query().Get("inputMint"), OutputMint: req.URL.Query().Get("outputMint"), InAmount: fmt.Sprint(amount), OutAmount: fmt.Sprint(amount), OtherAmountThreshold: fmt.Sprint(amount * 995 / 1000), SwapMode: "ExactIn", SlippageBPS: 50, RoutePlan: []json.RawMessage{json.RawMessage(`{}`)}}
		case "/swap-instructions":
			var body struct{ QuoteResponse jupiter.Quote }
			if json.NewDecoder(req.Body).Decode(&body) != nil {
				t.Fatal("quote body")
			}
			amount, _ := strconv.ParseUint(body.QuoteResponse.InAmount, 10, 64)
			instruction := captured.Instruction
			if body.QuoteResponse.InputMint != bridgeUSDC {
				instruction = reverse.Instruction
			}
			data, _ := base64.StdEncoding.DecodeString(instruction.Data)
			binary.LittleEndian.PutUint64(data[jupiter.V2InAmountOffset:], amount)
			quotedOut, _ := strconv.ParseUint(body.QuoteResponse.OutAmount, 10, 64)
			binary.LittleEndian.PutUint64(data[jupiter.V2QuotedOutOffset:], quotedOut)
			binary.LittleEndian.PutUint16(data[jupiter.V2SlippageOffset:], 50)
			instruction.Data = base64.StdEncoding.EncodeToString(data)
			payload = map[string]any{"swapInstruction": instruction}
		default:
			t.Fatal("unexpected Jupiter request")
		}
		raw, _ := json.Marshal(payload)
		return response(string(raw)), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return m, rpc, client, accounts
}

// A flat vault's entry is priced from the legs the executor builds, on the
// lane's own refreshed batch, and reads it without changing it: the entry
// swap, the deposit, the up step to 1.5x and their unwind.
func TestEntryPricesTheExecutorsLegsWithoutMutatingAccounts(t *testing.T) {
	t.Parallel()
	m, rpc, client, accounts := selectorDestinationFixture(t)
	before := hashConfirmedAccounts(accounts)
	q, err := testLanePricer(t, rpc, client, m, 42).priceEntry(context.Background(), flatEntryObservation(42, 100_000_000), leveredTestMarket(SelectedRouteID), 100_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if before != hashConfirmedAccounts(accounts) || q.SampleSlot != 42 || q.ValidThroughSlot != 74 || !q.currentAtSlot(42) || !sha256Pattern.MatchString(q.EvidenceID) {
		t.Fatal("invalid entry window or evidence", q)
	}
	// Maple's USDC debt values at par: the up step owes its receive (a zero
	// fee here) and converts all of it. The fixture's swaps quote at par, so
	// only network fees cost: four lane legs and the two steps.
	if q.Unlevered || q.BorrowReceiveRaw != 49_749_999 || q.BorrowValueRaw != q.BorrowReceiveRaw || q.ProceedsValueRaw != q.BorrowReceiveRaw || q.EquityRaw != 100_000_000 || !q.validBorrow() {
		t.Fatal("entry borrow", q)
	}
	if q.Cost.EntrySwapCostMicros != 0 || q.Cost.ExitSwapCostMicros != 0 || q.Cost.NetworkMicros <= 0 || q.Cost.Step.NetworkMicros <= 0 ||
		q.CostRaw != q.Cost.NetworkMicros+q.Cost.Step.TotalMicros || q.Cost.TotalMicros != q.CostRaw {
		t.Fatalf("entry cost %+v", q.Cost)
	}
}

// A lane whose obligation does not exist yet (a new vault) is a flat
// position: its entry prices, carrying the initializer that creates the
// obligation and registers its farm (live 2026-10-13: the looping vault's
// every lane refused, so it never entered).
func TestEntryIntoAnAbsentObligationCarriesItsSetup(t *testing.T) {
	t.Parallel()
	route, _ := runtimeRoute(SelectedRouteID)
	m, rpc, client, _ := selectorDestinationFixtureForLane(t, SelectedRouteID, func(accounts []ConfirmedAccount) {
		for i := range accounts {
			if a := &accounts[i]; a.Address == route.Kamino.Obligation || a.Address == route.ObligationDebtFarm {
				a.Lamports, a.Data = 0, nil
			}
		}
	})
	q, err := testLanePricer(t, rpc, client, m, 42).priceEntry(context.Background(), flatEntryObservation(42, 100_000_000), leveredTestMarket(SelectedRouteID), 100_000_000)
	if err != nil {
		t.Fatal(err)
	}
	// The vault pays the obligation's rent and the delegate the farm user
	// state's, both above any network fee.
	rent := uint64((128+kamino.ObligationSize)*3480*2 + (128+kamino.FarmUserStateSize)*3480*2)
	if q.Cost.SetupMicros <= 0 || q.CostRaw != q.Cost.NetworkMicros+q.Cost.SetupMicros+q.Cost.Step.TotalMicros {
		t.Fatalf("entry cost %+v", q.Cost)
	}
	if rentOnly, err := testLanePricer(t, rpc, client, m, 42).lamportsMicros(rent); err != nil || q.Cost.SetupMicros <= rentOnly {
		t.Fatalf("setup %d does not carry the rent %d: %v", q.Cost.SetupMicros, rentOnly, err)
	}
}

// A held lane is a destination too: cash or a rotation's tranche enters
// beside its position (docs/backyard-target.md rule 5). Live 2026-10-20:
// every lane worth entering was held, so Prime's tranche never priced.
func TestEntryBesideAHeldPositionPrices(t *testing.T) {
	t.Parallel()
	route, _ := runtimeRoute(SelectedRouteID)
	m, rpc, client, _ := selectorDestinationFixtureForLane(t, SelectedRouteID, func(accounts []ConfirmedAccount) {
		o := accountAt(accounts, route.Kamino.Obligation)
		putKey(t, o.Data[96:128], route.Kamino.CollateralReserve)
		binary.LittleEndian.PutUint64(o.Data[128:136], 100_000_000)
		putKey(t, o.Data[1208:1240], route.Kamino.DebtReserve)
		putScaledFraction(o.Data[1240:1272], new(big.Int).Lsh(big.NewInt(1), 60))
		putScaledFraction(o.Data[1296:1312], new(big.Int).Lsh(big.NewInt(40_000_000), 60))
	})
	q, err := testLanePricer(t, rpc, client, m, 42).priceEntry(context.Background(), flatEntryObservation(42, 100_000_000), leveredTestMarket(SelectedRouteID), 100_000_000)
	if err != nil || q.EquityRaw != 100_000_000 {
		t.Fatalf("held destination did not price: %+v %v", q, err)
	}
}

func TestSelectorDestinationDeclinesMissingFarmAndCustodyDrift(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"farm_missing", "farm_frozen", "foreign_user", "foreign_delegate", "reserve_farm", "receipt_authority", "nonflat", "capacity"} {
		t.Run(kind, func(t *testing.T) {
			m, rpc, client, accounts := selectorDestinationFixture(t)
			route, _ := runtimeRoute(SelectedRouteID)
			switch kind {
			case "farm_missing":
				clear(accountAt(accounts, route.ObligationDebtFarm).Data[:8])
			case "farm_frozen":
				accountAt(accounts, route.DebtFarm).Data[7361] = 1
			case "foreign_user":
				putKey(t, accountAt(accounts, route.ObligationDebtFarm).Data[48:80], bridgeDelegate)
			case "foreign_delegate":
				putKey(t, accountAt(accounts, route.ObligationDebtFarm).Data[480:512], bridgeVault)
			case "reserve_farm":
				clear(accountAt(accounts, route.Kamino.DebtReserve).Data[96:128])
			case "receipt_authority":
				clear(accountAt(accounts, route.CollateralReceiptMint).Data[4:36])
			case "nonflat":
				binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 1)
			case "capacity":
				binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.DebtReserve).Data[kaminoOutsideBorrowLimitOffset:], 0)
			}
			q, err := testLanePricer(t, rpc, client, m, 42).priceEntry(context.Background(), flatEntryObservation(42, 100_000_000), leveredTestMarket(SelectedRouteID), 100_000_000)
			if kind == "capacity" {
				// Maple is a B2 lane (registry): a closed borrow limit prices
				// the debt-free 1x entry instead of refusing the lane.
				if err != nil || !q.Unlevered || q.BorrowReceiveRaw != 0 {
					t.Fatalf("closed borrowing did not price the 1x entry: %+v %v", q, err)
				}
				return
			}
			if err == nil {
				t.Fatal("unready lane priced")
			}
		})
	}
}

// One price age holds every valuation: a reserve price past the oracle age
// prices nothing, the USDC reference's included.
func TestLanePricesRefuseAStalePrice(t *testing.T) {
	t.Parallel()
	_, _, _, accounts := selectorDestinationFixture(t)
	route, _ := runtimeRoute(SelectedRouteID)
	if _, err := batchLanePrices(accounts, route); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.DebtReserve).Data[264:272], uint64(1000-kaminoOracleMaxAgeSecs-1))
	if _, err := batchLanePrices(accounts, route); !errors.Is(err, errKaminoOracleStale) {
		t.Fatalf("a stale USDC price valued the lane: %v", err)
	}
}

// A step the executor cannot assemble at this size is named step_build, not
// a quote outage.
func TestStepBuildRefusalsAreNamed(t *testing.T) {
	t.Parallel()
	for cause, want := range map[string]string{"step_does_not_compile": "step_build", "step_over_policy_cap": "step_build", "jupiter_auto_wire_floor_zero": "entry_quote"} {
		if got := stepStage("entry_quote", budgetHold(cause)); got != want {
			t.Fatalf("%s: %s", cause, got)
		}
	}
	if got := leverageCostOutageReason(costOutage(stepStage("exit_quote", budgetHold("step_over_policy_cap")), budgetHold("step_over_policy_cap"))); got != "leverage_cost_unavailable(step_build)" {
		t.Fatal(got)
	}
}
