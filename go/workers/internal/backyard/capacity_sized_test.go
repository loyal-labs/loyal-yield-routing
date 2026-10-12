package backyard

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

func TestCapacitySizedEntryRetainsCollateralRoom(t *testing.T) {
	t.Parallel()
	route, accounts, position := pairCapacityFixture(t)
	route.Lane = onreONycUSDC
	binary.LittleEndian.PutUint64(accountAt(accounts, budgetClockAddress).Data[:8], 77)
	// $100k eligible collateral, $10k new principal room. Fixture prices are par.
	position = leverageTestPosition(0, 0)
	position.EntryCapacityRaw = 20_000_000_000
	for _, a := range accounts[:2] {
		binary.LittleEndian.PutUint64(a.Data[kaminoReserveConfigOffset+160:], 101_010_102_000)
		binary.LittleEndian.PutUint64(a.Data[kaminoReserveConfigOffset+168:], 200_000_000_000)
		binary.LittleEndian.PutUint64(a.Data[kaminoOutsideBorrowLimitOffset:], 200_000_000_000)
	}
	binary.LittleEndian.PutUint64(accounts[1].Data[224:], 10_000_000_000)
	got, err := kaminoPairEntryCapacity(position, accounts, route)
	if err != nil || got < 100_000_000_000 {
		t.Fatalf("equity clipped by borrowing: %d %v", got, err)
	}
	binary.LittleEndian.PutUint64(accounts[1].Data[224:], 0)
	zero, err := kaminoPairEntryCapacity(position, accounts, route)
	if err != nil || zero != got {
		t.Fatalf("zero debt room changed equity: %d %v", zero, err)
	}
}

func TestCapacitySizedActualCarry(t *testing.T) {
	t.Parallel()
	s := leverageSnapshot(1.5)
	s.PositionCollateralValueRaw, s.PositionDebtValueRaw = 136_000_000, 36_000_000
	s.PositionDebtRaw, s.LTVBPS = 36_000_000, 2647
	m := LaneEconomics{Lane: s.RouteLane, NativeAPY: .10, SupplyAPY: .01, CurrentBorrowAPY: .05,
		BorrowCurve: []BorrowCurvePoint{{0, 500}, {10_000, 500}}, DebtSupplyRaw: 1e12, DebtBorrowRaw: 1e11}
	got, ok := currentPositionAPY(s, []LaneEconomics{m})
	want := int64(math.Round(performanceFeeForecast(1.36*.11-.36*.05) * 10_000))
	if !ok || got.APYBPS != want || got.Level != 1.36 {
		t.Fatalf("carry=%+v want=%d", got, want)
	}
}

func TestCapacitySizedFinalBorrowGateRejectsShrinkingRoom(t *testing.T) {
	t.Parallel()
	for _, debt := range []uint64{0, 33_333_333} {
		t.Run(fmt.Sprintf("existing_debt_%d", debt), func(t *testing.T) { capacityFinalBorrowGate(t, debt) })
	}
}

// capacityBorrowGateFixture is a borrow request whose raw reserves are
// refreshed at slot 42, the clock's slot, with room, fee receiver and prices
// that pass the final borrow gate.
func capacityBorrowGateFixture(t *testing.T, sourceDebt uint64) ([]ConfirmedAccount, RuntimeRoute, KaminoPrimeUSDCRequest, ExpectedEffects) {
	t.Helper()
	_, m, _, _, accounts, route := leverage175Fixture(t)
	putScaledFraction(accountAt(accounts, route.Kamino.Obligation).Data[1296:1312], new(big.Int).Lsh(new(big.Int).SetUint64(sourceDebt), 60))
	debt := accountAt(accounts, route.Kamino.DebtReserve).Data
	collateral := accountAt(accounts, route.Kamino.CollateralReserve).Data
	binary.LittleEndian.PutUint64(debt[kaminoOutsideBorrowLimitOffset:], 1_000_000_000_000)
	binary.LittleEndian.PutUint64(debt[kaminoReserveConfigOffset+168:], 1_000_000_000_000)
	binary.LittleEndian.PutUint64(collateral[kaminoReserveConfigOffset+160:], 10_000_000_000_000)
	binary.LittleEndian.PutUint64(debt[224:], 1_000_000_000)
	binary.LittleEndian.PutUint64(accountAt(accounts, route.DebtLiquiditySupply).Data[64:], 1_000_000_000)
	binary.LittleEndian.PutUint64(debt[kaminoReserveConfigOffset+40:], 0)
	binary.LittleEndian.PutUint64(debt[kaminoBorrowFactorOffset:], 100)
	putKey(t, debt[192:224], route.DebtFeeReceiver)
	fee := accountAt(accounts, route.DebtLiquiditySupply)
	fee.Address, fee.Data = route.DebtFeeReceiver, append([]byte(nil), fee.Data...)
	binary.LittleEndian.PutUint64(fee.Data[64:], 0)
	accounts = append(accounts, fee)
	clock := clockFixture()
	binary.LittleEndian.PutUint64(clock.Data[:8], 42)
	binary.LittleEndian.PutUint64(clock.Data[32:], 1000)
	accounts = append(accounts, clock)
	for _, address := range []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve} {
		binary.LittleEndian.PutUint64(accountAt(accounts, address).Data[kaminoMarketPriceLastUpdatedTSOffset:], 1000)
	}
	r, err := m.kaminoPacketForRoute(testPolicies(t), OpenRouteStep, kaminoLegBorrow, 10_000_000, LatestBlockhash{Blockhash: bridgeSettings, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	r.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	effects, err := kaminoBorrowEffects(accounts, route, r.AmountRaw)
	if err != nil {
		t.Fatal(err)
	}
	return accounts, route, r, effects
}

func capacityFinalBorrowGate(t *testing.T, sourceDebt uint64) {
	accounts, route, r, effects := capacityBorrowGateFixture(t, sourceDebt)
	debt := accountAt(accounts, route.Kamino.DebtReserve).Data
	collateral := accountAt(accounts, route.Kamino.CollateralReserve).Data
	rpc := budgetBuildRPCWithAccounts(t, 5000, 42, accounts)
	if _, err := validateBorrowRequest(context.Background(), rpc, fixtureView(t, rpc), r, effects, 42); err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{61}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	message, err := compileKaminoMessageForDelegate(r, delegate)
	if err != nil {
		t.Fatal(err)
	}
	signed := signedTestBuildResult(t, key, message)
	if err = signed.validateForDelegate(delegate); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(debt[kaminoOutsideBorrowLimitOffset:], 9_000_000)
	if _, err = validateBorrowRequest(context.Background(), rpc, fixtureView(t, rpc), r, effects, 42); err == nil {
		t.Fatal("shrinking room passed persisted-input final gate")
	}
	binary.LittleEndian.PutUint64(debt[kaminoOutsideBorrowLimitOffset:], 1_000_000_000_000)
	if _, err = validateBorrowRequest(context.Background(), rpc, fixtureView(t, rpc), r, effects, 42); err != nil {
		t.Fatal(err)
	}
	// Another depositor fills the collateral reserve without changing its
	// normalized backing, our receipt balance, prices, risk or debt room.
	originalLiquidity := binary.LittleEndian.Uint64(collateral[224:232])
	originalReceipts := binary.LittleEndian.Uint64(collateral[2592:2600])
	originalLimit := binary.LittleEndian.Uint64(collateral[kaminoReserveConfigOffset+160:])
	normalizedBefore, _ := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	originalBacking, _ := normalizedBefore.redeemLiquidityRaw(100_000_000_000)
	binary.LittleEndian.PutUint64(collateral[224:232], originalLimit)
	binary.LittleEndian.PutUint64(collateral[2592:2600], originalReceipts*(originalLimit/originalLiquidity))
	normalizedAfter, _ := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	filledBacking, _ := normalizedAfter.redeemLiquidityRaw(100_000_000_000)
	if originalBacking != filledBacking {
		t.Fatal("fixture changed normalized collateral backing")
	}
	if _, err = validateBorrowRequest(context.Background(), rpc, fixtureView(t, rpc), r, effects, 42); err == nil {
		t.Fatal("full collateral reserve passed production final borrow validator")
	}
	binary.LittleEndian.PutUint64(collateral[kaminoReserveConfigOffset+160:], originalLimit+originalLiquidity)
	if _, err = validateBorrowRequest(context.Background(), rpc, fixtureView(t, rpc), r, effects, 42); err != nil {
		t.Fatal("expanded collateral capacity refused", err)
	}
	after, err := compileKaminoMessageForDelegate(r, delegate)
	if err != nil || !bytes.Equal(message, after) || r.AmountRaw != 10_000_000 {
		t.Fatal("room expansion resized immutable wire")
	}
}

func TestCapacitySizedDebtRoomBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func([]ConfirmedAccount)
		want   uint64
		fail   bool
	}{
		{"available", func([]ConfirmedAccount) {}, 1000, false},
		{"zero_available", func(a []ConfirmedAccount) { binary.LittleEndian.PutUint64(a[1].Data[224:], 0) }, 0, false},
		{"queued", func(a []ConfirmedAccount) { binary.LittleEndian.PutUint64(a[1].Data[kaminoQueuedCollateralOffset:], 1) }, 0, false},
		{"outside_limit", func(a []ConfirmedAccount) {
			binary.LittleEndian.PutUint64(a[1].Data[kaminoOutsideBorrowLimitOffset:], 100)
		}, 100, false},
		{"elevation", func(a []ConfirmedAccount) { a[2].Data[kaminoObligationElevationGroupOffset] = 1 }, 0, false},
		// Moved from the deleted one-pass pair-capacity table: the same
		// protocol caps now bound the B2 borrow.
		{"outside_limit_closed", func(a []ConfirmedAccount) {
			binary.LittleEndian.PutUint64(a[1].Data[kaminoOutsideBorrowLimitOffset:], 0)
		}, 0, false},
		{"borrow_limit", func(a []ConfirmedAccount) {
			binary.LittleEndian.PutUint64(a[1].Data[kaminoReserveConfigOffset+168:], 50)
		}, 50, false},
		// One fixed-point ulp below the 10% boundary: strict.
		{"utilization_boundary", func(a []ConfirmedAccount) { a[1].Data[kaminoReserveConfigOffset+645] = 10 }, 99, false},
		{"cross_collateral_disabled", func(a []ConfirmedAccount) { a[0].Data[kaminoDisableCrossCollateralOffset] = 1 }, 0, false},
		{"referrer", func(a []ConfirmedAccount) { a[2].Data[2288] = 1 }, 0, false},
		{"weighted_debt", func(a []ConfirmedAccount) { binary.LittleEndian.PutUint64(a[1].Data[kaminoBorrowFactorOffset:], 200) }, 0, false},
		{"unknown_clock", func(a []ConfirmedAccount) { a[3].Data = nil }, 0, true},
		{"stale_oracle", func(a []ConfirmedAccount) {
			binary.LittleEndian.PutUint64(a[1].Data[kaminoMarketPriceLastUpdatedTSOffset:], 1)
		}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route, a, _ := pairCapacityFixture(t)
			binary.LittleEndian.PutUint64(a[3].Data[:8], 77)
			tc.mutate(a)
			got, err := kaminoAdditionalDebtRoom(a, route)
			if got != tc.want || (err != nil) != tc.fail {
				t.Fatalf("room=%d err=%v", got, err)
			}
		})
	}
	for _, room := range []uint64{0, 1, 2, 10_000_000, 1<<63 - 1} {
		rate := (uint64(1) << 60) / 100
		receive := kaminoReceiveWithinRoom(room, rate)
		if receive == 0 {
			continue
		}
		fee, err := kaminoBorrowFeeAtRate(rate, receive)
		if err != nil || receive > room || fee > room-receive {
			t.Fatalf("fee-inclusive room overflow: %d %d %v", room, receive, err)
		}
	}
}

// The borrow transaction refreshes its reserves before it borrows, so the
// pre-send gate judges the refreshed state, not a raw reserve nobody
// refreshed (2026-10-11: a reserve ~2,000 slots stale crashed the worker 595
// times as a worker_fault). A health verdict on the refreshed state, or a
// refresh Kamino rejects, is a pre-send hold.
func TestBorrowGateJudgesTheRefreshedReserves(t *testing.T) {
	t.Parallel()
	const stale, refreshed = int64(2042), int64(2043)
	setup := func(t *testing.T, refresh func(map[string]ConfirmedAccount), simulationErr string) (*chain.Client, KaminoPrimeUSDCRequest, ExpectedEffects, *atomic.Int64) {
		t.Helper()
		accounts, route, r, effects := capacityBorrowGateFixture(t, 0)
		// Raw: the clock moved on and nobody refreshed the reserves since 42.
		for _, account := range accounts {
			if account.Address == budgetClockAddress {
				binary.LittleEndian.PutUint64(account.Data[:8], uint64(stale))
			}
		}
		// The closed refresh simulation's bank: both reserves refreshed in the
		// simulated slot, everything else as read.
		captured := map[string]ConfirmedAccount{}
		for _, account := range accounts {
			account.Data = append([]byte(nil), account.Data...)
			captured[account.Address] = account
		}
		for _, reserve := range []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve} {
			binary.LittleEndian.PutUint64(captured[reserve].Data[16:24], uint64(refreshed))
		}
		binary.LittleEndian.PutUint64(captured[budgetClockAddress].Data[:8], uint64(refreshed))
		if refresh != nil {
			refresh(captured)
		}
		client := budgetBuildRPCWithAccounts(t, 5000, stale, accounts)
		node := rpcOf(client)
		raw := node.Transport
		var simulations atomic.Int64
		node.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			var payload struct {
				Method string            `json:"method"`
				Params []json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Method != "simulateTransaction" {
				request.Body = io.NopCloser(bytes.NewReader(body))
				return raw.RoundTrip(request)
			}
			simulations.Add(1)
			if simulationErr != "" {
				return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":` + fmt.Sprint(refreshed) + `},"value":{"err":` + simulationErr + `,"accounts":null}}}`), nil
			}
			var config struct {
				Accounts struct {
					Addresses []string `json:"addresses"`
				} `json:"accounts"`
			}
			if err := json.Unmarshal(payload.Params[1], &config); err != nil {
				t.Fatal(err)
			}
			values := make([]any, 0, len(config.Accounts.Addresses))
			for _, address := range config.Accounts.Addresses {
				a, ok := captured[address]
				if !ok {
					values = append(values, nil)
					continue
				}
				values = append(values, map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}})
			}
			encoded, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]int64{"slot": refreshed}, "value": map[string]any{"err": nil, "accounts": values}}})
			if err != nil {
				t.Fatal(err)
			}
			return response(string(encoded)), nil
		})
		return client, r, effects, &simulations
	}
	gate := func(client *chain.Client, r KaminoPrimeUSDCRequest, effects ExpectedEffects) (int64, error) {
		return validateBorrowRequest(context.Background(), client, fixtureView(t, client), r, effects, stale)
	}

	t.Run("stale raw reserve, fresh refreshed capture", func(t *testing.T) {
		t.Parallel()
		client, r, effects, simulations := setup(t, nil, "")
		slot, err := gate(client, r, effects)
		if err != nil || slot != refreshed || simulations.Load() != 1 {
			t.Fatalf("stale raw reserve with a fresh refresh did not pass on the refreshed bank: slot=%d simulations=%d err=%v", slot, simulations.Load(), err)
		}
	})
	route, err := runtimeRoute(onreONycUSDC)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, reason, simulationErr string
		refresh                     func(map[string]ConfirmedAccount)
	}{
		{name: "paused after refresh", reason: "reserve_paused", refresh: func(c map[string]ConfirmedAccount) {
			c[route.Kamino.DebtReserve].Data[kaminoReserveStatusOffset] = 1
		}},
		{name: "emergency after refresh", reason: "reserve_paused", refresh: func(c map[string]ConfirmedAccount) {
			c[route.Kamino.CollateralReserve].Data[kaminoReserveEmergencyModeOffset] = 1
		}},
		{name: "oracle stale after refresh", reason: "kamino_stale", refresh: func(c map[string]ConfirmedAccount) {
			binary.LittleEndian.PutUint64(c[budgetClockAddress].Data[32:], 1000+uint64(kaminoOracleMaxAgeSecs)+1)
		}},
		{name: "reserve still stale after refresh", reason: "kamino_stale", refresh: func(c map[string]ConfirmedAccount) {
			binary.LittleEndian.PutUint64(c[route.Kamino.DebtReserve].Data[16:24], 42)
		}},
		{name: "refresh rejected by Kamino", reason: "price_refresh_simulation_failed", simulationErr: `{"InstructionError":[0,{"Custom":6009}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, r, effects, _ := setup(t, tc.refresh, tc.simulationErr)
			_, err := gate(client, r, effects)
			var hold *BudgetHold
			if !errors.As(err, &hold) || hold.Reason != tc.reason || !isPreSendHold(err) {
				t.Fatalf("reserve health at pre-send is not a %s hold: %v", tc.reason, err)
			}
		})
	}
	// A fresh raw reserve that is paused holds without any simulation.
	t.Run("paused fresh raw reserve", func(t *testing.T) {
		t.Parallel()
		accounts, route, r, effects := capacityBorrowGateFixture(t, 0)
		accountAt(accounts, route.Kamino.DebtReserve).Data[kaminoReserveStatusOffset] = 1
		client := budgetBuildRPCWithAccounts(t, 5000, 42, accounts)
		_, err := validateBorrowRequest(context.Background(), client, fixtureView(t, client), r, effects, 42)
		var hold *BudgetHold
		if !errors.As(err, &hold) || hold.Reason != "reserve_paused" || !isPreSendHold(err) {
			t.Fatalf("paused reserve at pre-send is not a reserve_paused hold: %v", err)
		}
	})
}
