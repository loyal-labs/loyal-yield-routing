package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"reflect"
	"strconv"
	"testing"
)

// Controlled real-layout accounts and captured Jupiter instruction topology.
// Neither fixture transport supports simulation/signing/submission.
func selectorDestinationFixture(t *testing.T) (RouteManifest, *RPCClient, *jupiterClient, []ConfirmedAccount) {
	t.Helper()
	m := basicPolicyFixtureManifest(t)
	route, _ := runtimeRoute(SelectedRouteID)
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
	add(ConfirmedAccount{Address: kaminoScopePrices, Owner: kaminoProgram, Lamports: 1, Data: []byte{1}})
	market := marketFixture(t, route.Kamino.Market)
	binary.LittleEndian.PutUint64(market.Data[kaminoGlobalBorrowValueOffset:], 1_000_000)
	add(market)
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
	for i, f := range []BasicPolicyFamily{BasicCollateralLifecycle, BasicDebtLifecycle, BasicSwapRoutesA, BasicSwapRoutesB} {
		binding, _, err := m.basicPolicyBinding(f)
		if err != nil {
			t.Fatal(err)
		}
		data := []byte(fmt.Sprint("controlled basic policy ", i))
		hash := sha256Bytes(data)
		switch f {
		case BasicCollateralLifecycle:
			m.RuntimeBindings.CollateralLifecycle.DataSHA256 = &hash
		case BasicDebtLifecycle:
			m.RuntimeBindings.DebtLifecycle.DataSHA256 = &hash
		case BasicSwapRoutesA:
			m.RuntimeBindings.SwapRoutesA.DataSHA256 = &hash
		case BasicSwapRoutesB:
			m.RuntimeBindings.SwapRoutesB.DataSHA256 = &hash
		}
		add(ConfirmedAccount{Address: binding.Policy, Owner: bridgeSquadsProgram, Lamports: 1, Data: data})
	}
	for i := range m.RuntimeBindings.BridgePolicies {
		p := &m.RuntimeBindings.BridgePolicies[i]
		data := []byte("controlled bridge policy " + string(p.Action))
		hash := sha256Bytes(data)
		p.NormalizedDigest = hash
		p.DataSHA256Raw = hash
		p.MaskedByteRanges = nil
		add(ConfirmedAccount{Address: p.Account, Owner: bridgeSquadsProgram, Lamports: 1, Data: data})
	}
	farm := ConfirmedAccount{Address: route.DebtFarm, Owner: kaminoFarmsProgram, Lamports: 1, Data: make([]byte, 8336)}
	copy(farm.Data, []byte{198, 102, 216, 74, 63, 66, 163, 190})
	putKey(t, farm.Data[7328:7360], route.Kamino.MarketAuthority)
	farm.Data[7362] = 1
	add(farm)
	u := ConfirmedAccount{Address: route.ObligationDebtFarm, Owner: kaminoFarmsProgram, Lamports: 1, Data: make([]byte, 920)}
	copy(u.Data, []byte{72, 177, 85, 249, 76, 167, 186, 126})
	putKey(t, u.Data[16:48], route.DebtFarm)
	putKey(t, u.Data[48:80], bridgeVault)
	u.Data[80] = 1
	putKey(t, u.Data[480:512], route.Kamino.Obligation)
	add(u)
	captured, _ := basicJupiterRequestFromExport(t, route.Lane, "USDC->syrupUSDC")
	reverse, record := basicJupiterRequestFromExport(t, route.Lane, "syrupUSDC->USDC")
	for _, table := range retainedOrReconstructedLookupTables(t, reverse.Instruction.LookupTableAddresses, legacyMessageKeys(t, record.MessageBase64), []string{record.PolicyAccount}) {
		binary.LittleEndian.PutUint64(table.Data[12:20], 41)
		add(ConfirmedAccount{Address: table.Address, Owner: table.Owner, Lamports: table.Lamports, Data: table.Data})
	}
	rpc := budgetBuildRPCWithAccounts(t, 5000, 42, accounts)
	client, err := newJupiterClient("https://jupiter.invalid", &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var payload any
		switch req.URL.Path {
		case "/quote":
			amount, _ := strconv.ParseUint(req.URL.Query().Get("amount"), 10, 64)
			payload = JupiterQuote{InputMint: req.URL.Query().Get("inputMint"), OutputMint: req.URL.Query().Get("outputMint"), InAmount: fmt.Sprint(amount), OutAmount: fmt.Sprint(amount), OtherAmountThreshold: fmt.Sprint(amount * 995 / 1000), SwapMode: "ExactIn", SlippageBPS: 50, RoutePlan: []json.RawMessage{json.RawMessage(`{}`)}}
		case "/swap-instructions":
			var body struct{ QuoteResponse JupiterQuote }
			if json.NewDecoder(req.Body).Decode(&body) != nil {
				t.Fatal("quote body")
			}
			amount, _ := strconv.ParseUint(body.QuoteResponse.InAmount, 10, 64)
			instruction := captured.Instruction
			if body.QuoteResponse.InputMint != bridgeUSDC {
				instruction = reverse.Instruction
			}
			if body.QuoteResponse.InputMint == bridgeUSDC {
				instruction.LookupTableAddresses = nil
			}
			data, _ := base64.StdEncoding.DecodeString(instruction.Data)
			binary.LittleEndian.PutUint64(data[len(data)-19:], amount)
			quotedOut, _ := strconv.ParseUint(body.QuoteResponse.OutAmount, 10, 64)
			binary.LittleEndian.PutUint64(data[len(data)-11:], quotedOut)
			binary.LittleEndian.PutUint16(data[len(data)-3:], 50)
			instruction.Data = base64.StdEncoding.EncodeToString(data)
			payload = map[string]any{"swapInstruction": instruction, "addressLookupTableAddresses": instruction.LookupTableAddresses}
		default:
			t.Fatal("unexpected Jupiter request")
		}
		raw, _ := json.Marshal(payload)
		return response(string(raw)), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	return m, rpc, client, accounts
}

func TestSelectorDestinationPricesCompleteEntryWithoutMutatingAccounts(t *testing.T) {
	m, rpc, client, accounts := selectorDestinationFixture(t)
	before := hashConfirmedAccounts(accounts)
	q, err := observeSelectorDestination(context.Background(), rpc, client, m, SelectedRouteID, 1_000_000, 42)
	if err != nil {
		t.Fatal(err)
	}
	if before != hashConfirmedAccounts(accounts) || q.AccountSlot != 42 || q.Recipe.ValidThroughSlot != 74 || q.Recipe.SetupLamports != 0 || q.Recipe.CostRaw <= 0 || q.Recipe.CostRaw >= 1_000_000 {
		t.Fatal("invalid entry economics", q)
	}
	if q.PayoffUpperRaw < q.BorrowReceiveRaw+q.BorrowFeeRaw || q.PayoffSwap.Request.MinimumOutputRaw < q.PayoffUpperRaw {
		t.Fatal("repayment not covered", q)
	}
	var actions []Action
	for _, input := range q.Recipe.Inputs {
		r, _, _, err := input.decode()
		if err != nil {
			t.Fatal(err)
		}
		switch r := r.(type) {
		case BridgeBuildRequest:
			actions = append(actions, r.Action)
		case JupiterSwapRequest:
			actions = append(actions, r.Action)
		case KaminoPrimeUSDCRequest:
			actions = append(actions, r.Action)
		}
	}
	want := []Action{VoltrAllocateToSquads, ReportNAV, SwapStableToCollateralStep, ReportNAV, OpenRouteStep, ReportNAV, OpenRouteStep, ReportNAV, SwapDebtToCollateralStep, ReportNAV, OpenRouteStep, ReportNAV}
	if !reflect.DeepEqual(actions, want) || q.Recipe.NetworkLamports != 60_000 || q.InitialCollateralMinimumRaw != 995_000 || q.BorrowReceiveRaw != 497_498 {
		t.Fatal("incomplete entry", actions, q.BorrowReceiveRaw)
	}
}

func TestSelectorDestinationDeclinesMissingFarmAndCustodyDrift(t *testing.T) {
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
			if _, err := observeSelectorDestination(context.Background(), rpc, client, m, SelectedRouteID, 1_000_000, 42); err == nil {
				t.Fatal("unready lane priced")
			}
		})
	}
}

func TestSelectorDestinationRejectsUnfundableProtocolExit(t *testing.T) {
	for _, kind := range []string{"global_cap", "release_ceiling", "minimum_collateral", "repayment_quote"} {
		t.Run(kind, func(t *testing.T) {
			m, rpc, client, accounts := selectorDestinationFixture(t)
			route, _ := runtimeRoute(SelectedRouteID)
			switch kind {
			case "global_cap":
				binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.Market).Data[kaminoGlobalBorrowValueOffset:], 1)
			case "release_ceiling":
				accountAt(accounts, route.Kamino.CollateralReserve).Data[kaminoLoanToValueOffset] = 55
				accountAt(accounts, route.Kamino.CollateralReserve).Data[kaminoReserveConfigOffset+17] = 75
			case "minimum_collateral":
				minimum := new(big.Int).Lsh(big.NewInt(20), 60)
				raw := minimum.Bytes()
				dst := accountAt(accounts, route.Kamino.Market).Data[kaminoMinRemainingValueOffset : kaminoMinRemainingValueOffset+16]
				for i, b := range raw {
					dst[len(raw)-1-i] = b
				}
			case "repayment_quote":
				original := client.http.Transport
				client.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Path == "/quote" && req.URL.Query().Get("inputMint") != bridgeUSDC {
						amount, _ := strconv.ParseUint(req.URL.Query().Get("amount"), 10, 64)
						raw, _ := json.Marshal(JupiterQuote{InputMint: route.Kamino.CollateralMint, OutputMint: bridgeUSDC, InAmount: fmt.Sprint(amount), OutAmount: fmt.Sprint(amount / 2), OtherAmountThreshold: fmt.Sprint((amount / 2) * 995 / 1000), SwapMode: "ExactIn", SlippageBPS: 50, RoutePlan: []json.RawMessage{json.RawMessage(`{}`)}})
						return response(string(raw)), nil
					}
					return original.RoundTrip(req)
				})
			}
			if _, err := observeSelectorDestination(context.Background(), rpc, client, m, SelectedRouteID, 10_000_000, 42); err == nil {
				t.Fatal("unfundable destination quoted")
			}
		})
	}
}

func TestSelectorDestinationFullPilotTrancheHasQuotedPayoff(t *testing.T) {
	m, rpc, client, _ := selectorDestinationFixture(t)
	q, err := observeSelectorDestination(context.Background(), rpc, client, m, SelectedRouteID, 10_000_000, 42)
	if err != nil {
		t.Fatal(err)
	}
	if q.PayoffSwap.Request.MinimumOutputRaw < q.PayoffUpperRaw || q.Recipe.CostRaw >= 10_000_000 || len(q.Recipe.Inputs) != 12 {
		t.Fatal("incomplete full tranche forecast", q)
	}
}

func TestSelectorDestinationIncludesPayoffLookupReadInFreshness(t *testing.T) {
	m, rpc, client, accounts := selectorDestinationFixture(t)
	tables := map[string]bool{}
	for _, a := range accounts {
		if a.Owner == "AddressLookupTab1e1111111111111111111111111" {
			tables[a.Address] = true
		}
	}
	original := rpc.client.Transport
	changed := false
	rpc.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		req.Body = io.NopCloser(bytes.NewReader(raw))
		var body struct {
			Method string
			Params []json.RawMessage
		}
		if json.Unmarshal(raw, &body) != nil {
			t.Fatal("bad fixture request")
		}
		res, err := original.RoundTrip(req)
		if err != nil {
			return res, err
		}
		if body.Method == "getMultipleAccounts" {
			var addresses []string
			_ = json.Unmarshal(body.Params[0], &addresses)
			if len(addresses) > 0 && tables[addresses[0]] {
				var envelope map[string]any
				if json.NewDecoder(res.Body).Decode(&envelope) != nil {
					t.Fatal("bad fixture response")
				}
				_ = res.Body.Close()
				envelope["result"].(map[string]any)["context"].(map[string]any)["slot"] = 100
				encoded, _ := json.Marshal(envelope)
				res = response(string(encoded))
				changed = true
			}
		}
		return res, nil
	})
	_, err := observeSelectorDestination(context.Background(), rpc, client, m, SelectedRouteID, 1_000_000, 42)
	if !changed {
		t.Fatal("fixture never read payoff lookups")
	}
	assertBudgetHold(t, err, "selector_recipe_observation_expired")
}

func TestSelectorReceiptBoundIncludesPreBorrowInterest(t *testing.T) {
	_, _, _, accounts := selectorDestinationFixture(t)
	route, _ := runtimeRoute(SelectedRouteID)
	a := accountAt(accounts, route.Kamino.CollateralReserve)
	binary.LittleEndian.PutUint64(a.Data[224:232], 999_994_000)
	for i := 0; i < 11; i++ {
		binary.LittleEndian.PutUint32(a.Data[kaminoReserveConfigOffset+68+i*8:], 10_000)
	}
	reserve, err := decodeKaminoReserve(a, route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		t.Fatal(err)
	}
	pool := new(big.Int).Add(reserve.totalLiquiditySF, new(big.Int).Lsh(big.NewInt(2), 60))
	old, err := selectorProspectiveUpper(accounts, route, 42, route.Kamino.CollateralReserve, route.Kamino.CollateralMint, pool, selectorPayoffWindows)
	if err != nil || old > reserve.collateralMintSupply {
		t.Fatal("fixture not below first receipt boundary", old, err)
	}
	got, err := selectorReceiptRounding(accounts, route, 42)
	if err != nil || got != 2 {
		t.Fatal("sample-to-payoff interest omitted", got, err)
	}
}
