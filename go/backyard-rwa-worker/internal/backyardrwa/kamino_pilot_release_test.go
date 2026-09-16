package backyardrwa

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
)

// Realistic 10-USDC equity, one 50% borrow and full redeposit. This is a
// controlled account fixture, not an executed protocol or price-peg assertion.
func pilotReleaseFixture(t *testing.T, lane string) (RuntimeRoute, []ConfirmedAccount) {
	t.Helper()
	route, _ := runtimeRoute(lane)
	one := new(big.Int).Lsh(big.NewInt(1), 60)
	c := reserveFixture(t, route.Kamino.CollateralReserve, route.Kamino.CollateralMint, 42, one, 1_000_000_000, 1_000_000_000)
	d := reserveFixture(t, route.Kamino.DebtReserve, route.Kamino.DebtMint, 42, one, 1_000_000_000, 1_000_000_000)
	for _, a := range []*ConfirmedAccount{&c, &d} {
		putKey(t, a.Data[32:64], route.Kamino.Market)
		binary.LittleEndian.PutUint64(a.Data[272:280], 6)
		binary.LittleEndian.PutUint64(a.Data[264:272], 1000)
		binary.LittleEndian.PutUint32(a.Data[28:32], 1000)
		a.Data[kaminoReserveConfigOffset+9] = 1
		a.Data[kaminoLoanToValueOffset], a.Data[kaminoLoanToValueOffset+1] = 80, 90
		binary.LittleEndian.PutUint64(a.Data[kaminoBorrowFactorOffset:], 100)
		for i := 0; i < 11; i++ {
			off := kaminoReserveConfigOffset + 64 + i*8
			if i > 0 {
				binary.LittleEndian.PutUint32(a.Data[off:], 10_000)
			}
			binary.LittleEndian.PutUint32(a.Data[off+4:], 7500)
		}
	}
	o := obligationFixture(t, 42, 15_000_000, 5_000_000)
	o.Address = route.Kamino.Obligation
	putKey(t, o.Data[32:64], route.Kamino.Market)
	putKey(t, o.Data[96:128], route.Kamino.CollateralReserve)
	putKey(t, o.Data[1208:1240], route.Kamino.DebtReserve)
	market := marketFixture(t, route.Kamino.Market)
	binary.LittleEndian.PutUint64(market.Data[kaminoGlobalBorrowValueOffset:], 45_000_000)
	clock := clockFixture()
	binary.LittleEndian.PutUint64(clock.Data[:8], 42)
	binary.LittleEndian.PutUint64(clock.Data[32:40], 1000)
	accounts := []ConfirmedAccount{c, d, o, market, clock}
	for _, row := range []struct {
		address, mint, owner string
		amount               uint64
	}{
		{route.CollateralCustody, route.Kamino.CollateralMint, bridgeVault, 0},
		{route.CollateralLiquiditySupply, route.Kamino.CollateralMint, route.Kamino.MarketAuthority, 1_000_000_000},
		{route.DebtCustody, route.Kamino.DebtMint, bridgeVault, 0},
		{route.DebtLiquiditySupply, route.Kamino.DebtMint, route.Kamino.MarketAuthority, 1_000_000_000},
	} {
		data := make([]byte, 165)
		putKey(t, data[:32], row.mint)
		putKey(t, data[32:64], row.owner)
		binary.LittleEndian.PutUint64(data[64:72], row.amount)
		data[108] = 1
		accounts = append(accounts, ConfirmedAccount{Address: row.address, Owner: classicTokenProgram, Lamports: 1, Data: data})
	}
	return route, accounts
}

func TestPilotReleaseCanFundFullyRedepositedSinglePass(t *testing.T) {
	for _, lane := range selectorLanes {
		t.Run(lane, func(t *testing.T) {
			route, accounts := pilotReleaseFixture(t, lane)
			legacy, err := decodeKaminoRepaymentReleaseForMode(accounts, route, 42, 7, false)
			if err != nil || legacy.LiquidityRaw >= legacy.Payoff.UpperDebtRaw {
				t.Fatal("fixture must expose old full-payoff shortfall", legacy, err)
			}
			pilot, err := decodeKaminoRepaymentReleaseForMode(accounts, route, 42, 7, true)
			if err != nil {
				t.Fatal(err)
			}
			// A controlled 30bps minimum quote covers the upper debt, including the
			// seven-step interest window, while all remaining collateral stays booked.
			if pilot.LiquidityRaw*9970/10_000 < pilot.Payoff.UpperDebtRaw || pilot.ReceiptRaw+pilot.RemainingReceiptRaw != 15_000_000 || pilot.Payoff.UpperDebtRaw*10_000 > pilot.RemainingReceiptRaw*5500 {
				t.Fatal(pilot)
			}
			// Cached valuation is deliberately nonsense; current prices determine size.
			for _, off := range []int{1192, 2208, 2240} {
				putScaledFraction(accountAt(accounts, route.Kamino.Obligation).Data[off:off+16], new(big.Int).Lsh(big.NewInt(999), 60))
			}
			unchanged, err := decodeKaminoRepaymentReleaseForMode(accounts, route, 42, 7, true)
			if err != nil || unchanged.ReceiptRaw != pilot.ReceiptRaw {
				t.Fatal("cached valuation affected allowance", unchanged, err)
			}
			// A binding market cap reduces the allowable release below payoff needs.
			binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.Market).Data[kaminoGlobalBorrowValueOffset:], 6)
			capped, err := decodeKaminoRepaymentReleaseForMode(accounts, route, 42, 7, true)
			if err != nil || capped.LiquidityRaw >= capped.Payoff.UpperDebtRaw || capped.LiquidityRaw >= pilot.LiquidityRaw {
				t.Fatal("market cap ignored", capped, err)
			}
		})
	}
}

func TestPilotReleaseRefusesChangedRiskModel(t *testing.T) {
	for _, drift := range []string{"factor", "group", "max_ltv", "threshold", "cap", "missing_market", "emergency", "no_excess"} {
		t.Run(drift, func(t *testing.T) {
			route, accounts := pilotReleaseFixture(t, SelectedRouteID)
			switch drift {
			case "factor":
				binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.DebtReserve).Data[kaminoBorrowFactorOffset:], 101)
			case "group":
				accountAt(accounts, route.Kamino.Obligation).Data[kaminoObligationElevationGroupOffset] = 1
			case "max_ltv":
				accountAt(accounts, route.Kamino.CollateralReserve).Data[kaminoLoanToValueOffset] = 55
			case "threshold":
				accountAt(accounts, route.Kamino.CollateralReserve).Data[kaminoLoanToValueOffset+1] = 70
			case "cap":
				binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.Market).Data[kaminoGlobalBorrowValueOffset:], 5)
			case "missing_market":
				for i := range accounts {
					if accounts[i].Address == route.Kamino.Market {
						accounts[i].Data = nil
					}
				}
			case "emergency":
				accountAt(accounts, route.Kamino.Market).Data[kaminoMarketEmergencyModeOffset] = 1
			case "no_excess":
				putScaledFraction(accountAt(accounts, route.Kamino.Obligation).Data[1296:1312], new(big.Int).Lsh(big.NewInt(9_000_000), 60))
			}
			if _, err := decodeKaminoRepaymentReleaseForMode(accounts, route, 42, 7, true); err == nil {
				t.Fatal("unsafe pilot release accepted")
			}
		})
	}
}

func TestPilotReleaseRevalidationBindsCurrentLimitsAndMode(t *testing.T) {
	route, accounts := pilotReleaseFixture(t, SelectedRouteID)
	bound, err := decodeKaminoRepaymentReleaseForMode(accounts, route, 42, 5, true)
	if err != nil {
		t.Fatal(err)
	}
	m, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.kaminoPacketForRoute(DeleverRouteStep, kaminoLegWithdraw, bound.ReceiptRaw, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	r.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	r.RepaymentRelease, r.PilotRepaymentRelease = true, true
	source, dest := kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
	effects, err := exactKaminoTokenEffects(accounts, source, dest, bound.LiquidityRaw)
	if err != nil {
		t.Fatal(err)
	}
	rpc, _ := NewRPCClient("https://rpc.invalid")
	rpc.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var call struct {
			Method string
			Params []json.RawMessage
			ID     any
		}
		if json.NewDecoder(req.Body).Decode(&call) != nil || call.Method != "getMultipleAccounts" {
			t.Fatal("unexpected RPC")
		}
		var addresses []string
		_ = json.Unmarshal(call.Params[0], &addresses)
		var rows []any
		marketRead := false
		for _, address := range addresses {
			if address == route.Kamino.Market {
				marketRead = true
			}
			a := accountAt(accounts, address)
			rows = append(rows, map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}})
		}
		if !marketRead {
			t.Fatal("release omitted same-batch market cap")
		}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": map[string]any{"context": map[string]any{"slot": 42}, "value": rows}})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
	if _, _, err = validateRepaymentReleaseRequest(context.Background(), rpc, r, effects, 42); err != nil {
		t.Fatal(err)
	}
	encoded, _ := jsonMarshalExpectedEffects(effects)
	digest, _ := Phase3IntentDigest(r, encoded)
	assertBudgetHold(t, emptyTestBudget().validatePilotReleaseAuthority(r), "pilot_release_authority_required")
	if err = pilotTestBudget(t).validatePilotReleaseAuthority(r); err != nil {
		t.Fatal(err)
	}
	r.PilotRepaymentRelease = false
	legacyDigest, _ := Phase3IntentDigest(r, encoded)
	if digest == legacyDigest {
		t.Fatal("release mode escaped persisted intent identity")
	}
	if _, _, err = validateRepaymentReleaseRequest(context.Background(), rpc, r, effects, 42); err == nil {
		t.Fatal("pilot amount authorized by historical model")
	}
	r.PilotRepaymentRelease = true
	binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.Market).Data[kaminoGlobalBorrowValueOffset:], 6)
	if _, _, err = validateRepaymentReleaseRequest(context.Background(), rpc, r, effects, 42); err == nil {
		t.Fatal("persisted release survived tighter market cap")
	}
	r.RepaymentRelease = false
	if _, err = CompileKaminoMessage(r); err == nil {
		t.Fatal("standalone pilot flag compiled")
	}
}

func pilotProjectedReleaseFixture(t *testing.T) (RuntimeRoute, *phase3BridgeAdmission, phase3KaminoProjection) {
	t.Helper()
	route, accounts := pilotReleaseFixture(t, SelectedRouteID)
	bound, err := decodeKaminoRepaymentReleaseForMode(accounts, route, 42, 7, true)
	if err != nil {
		t.Fatal(err)
	}
	m, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	release, err := m.kaminoPacketForRoute(DeleverRouteStep, kaminoLegWithdraw, bound.ReceiptRaw, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	release.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	source, dest := kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
	e, err := exactKaminoTokenEffects(accounts, source, dest, bound.LiquidityRaw)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := jsonMarshalExpectedEffects(e)
	exit, err := encodePhase3BuildInput(release, raw)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := m.kaminoPacketForRoute(OpenRouteStep, kaminoLegDeposit, 5_000_000, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	entry.ObligationReserves = release.ObligationReserves
	before := append([]ConfirmedAccount(nil), accounts...)
	for i, a := range before {
		before[i].Data = append([]byte(nil), a.Data...)
	}
	binary.LittleEndian.PutUint64(accountAt(before, route.CollateralCustody).Data[64:72], 5_000_000)
	e, err = boundedKaminoDepositEffects(before, route, 42, entry.AmountRaw)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = jsonMarshalExpectedEffects(e)
	input, err := encodePhase3BuildInput(entry, raw)
	if err != nil {
		t.Fatal(err)
	}
	_, _, message, err := input.decode()
	if err != nil {
		t.Fatal(err)
	}
	projection := phase3KaminoProjection{Slot: 42, MessageSHA256: sha256Bytes(message), UnitsConsumed: 100, Accounts: accounts}
	plan := &phase3BridgeAdmission{Snapshot: Snapshot{PilotActive: true, RouteLane: route.Lane}, ValidThroughSlot: 74, Input: input, FundingRelease: exit, Payoff: &bound.Payoff, DepositProjection: &projection}
	return route, plan, projection
}

func TestPilotProjectedReleaseRejectsRiskDriftBeforeEntry(t *testing.T) {
	for _, drift := range []string{"", "max_ltv", "threshold", "factor", "group", "cap", "minimum", "collateral_price", "debt_price", "emergency", "backing", "debt"} {
		t.Run(drift, func(t *testing.T) {
			route, plan, projection := pilotProjectedReleaseFixture(t)
			fresh := projection
			fresh.Accounts = append([]ConfirmedAccount(nil), projection.Accounts...)
			for i, a := range fresh.Accounts {
				fresh.Accounts[i].Data = append([]byte(nil), a.Data...)
			}
			accounts := fresh.Accounts
			want := "pilot_release_projection_risk_changed"
			switch drift {
			case "max_ltv":
				accountAt(accounts, route.Kamino.CollateralReserve).Data[kaminoLoanToValueOffset] = 55
			case "threshold":
				accountAt(accounts, route.Kamino.CollateralReserve).Data[kaminoLoanToValueOffset+1] = 70
			case "factor":
				binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.DebtReserve).Data[kaminoBorrowFactorOffset:], 101)
			case "group":
				accountAt(accounts, route.Kamino.Obligation).Data[kaminoObligationElevationGroupOffset] = 1
			case "cap":
				binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.Market).Data[kaminoGlobalBorrowValueOffset:], 6)
			case "minimum":
				putScaledFraction(accountAt(accounts, route.Kamino.Market).Data[kaminoMinRemainingValueOffset:kaminoMinRemainingValueOffset+16], new(big.Int).Lsh(big.NewInt(10), 60))
			case "collateral_price":
				accountAt(accounts, route.Kamino.CollateralReserve).Data[248]++
			case "debt_price":
				accountAt(accounts, route.Kamino.DebtReserve).Data[248]++
			case "emergency":
				accountAt(accounts, route.Kamino.Market).Data[kaminoMarketEmergencyModeOffset] = 1
			case "backing":
				binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.CollateralReserve).Data[224:232], 900_000_000)
				want = "pilot_release_projection_backing_reduced"
			case "debt":
				putScaledFraction(accountAt(accounts, route.Kamino.Obligation).Data[1296:1312], new(big.Int).Lsh(big.NewInt(5_001_000), 60))
				want = "pilot_release_projection_payoff_changed"
			}
			err := validatePilotReleaseProjection(plan, projection, fresh, route)
			if drift == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				assertBudgetHold(t, err, want)
			}
		})
	}
}

func TestPilotProjectedReleaseUsesEquivalentRefreshedSimulation(t *testing.T) {
	_, plan, projection := pilotProjectedReleaseFixture(t)
	_, _, message, err := plan.Input.decode()
	if err != nil {
		t.Fatal(err)
	}
	rpc, _ := NewRPCClient("https://rpc.invalid")
	rpc.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var call struct {
			Method string
			Params []json.RawMessage
			ID     any
		}
		if json.NewDecoder(req.Body).Decode(&call) != nil || call.Method != "simulateTransaction" {
			t.Fatal("must compare refreshed simulation, not persisted reserve prices")
		}
		var encoded string
		_ = json.Unmarshal(call.Params[0], &encoded)
		wire, _ := base64.StdEncoding.DecodeString(encoded)
		if len(wire) < 65 || sha256Bytes(wire[65:]) != sha256Bytes(message) || !allZero(wire[1:65]) {
			t.Fatal("changed or signed entry in risk probe")
		}
		var rows []any
		for _, a := range projection.Accounts {
			rows = append(rows, map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}})
		}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": map[string]any{"context": map[string]any{"slot": 42}, "value": map[string]any{"err": nil, "unitsConsumed": 100, "accounts": rows}}})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
	for _, kind := range []string{"deposit", "borrow", "leverage"} {
		plan.DepositProjection, plan.BorrowProjection, plan.LeverageProjection = nil, nil, nil
		switch kind {
		case "deposit":
			plan.DepositProjection = &projection
		case "borrow":
			plan.BorrowProjection = &projection
		case "leverage":
			plan.LeverageProjection = &projection
		}
		slot, err := validatePilotProjectedReleaseRisk(context.Background(), rpc, plan, 42)
		if err != nil || slot != 42 {
			t.Fatal(kind, slot, err)
		}
	}
}

func TestPilotReleaseRetainsProtocolMinimumCollateral(t *testing.T) {
	route, accounts := pilotReleaseFixture(t, SelectedRouteID)
	putScaledFraction(accountAt(accounts, route.Kamino.Market).Data[kaminoMinRemainingValueOffset:kaminoMinRemainingValueOffset+16], new(big.Int).Lsh(big.NewInt(10), 60))
	bound, err := decodeKaminoRepaymentReleaseForMode(accounts, route, 42, 7, true)
	if err != nil || bound.RemainingReceiptRaw < 10_000_000 || bound.LiquidityRaw >= bound.Payoff.UpperDebtRaw {
		t.Fatal("minimum remaining collateral ignored", bound, err)
	}
}
