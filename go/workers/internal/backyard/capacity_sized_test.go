package backyard

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"
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

func TestCapacitySizedPartialRatioUsesActualBeforeRelease(t *testing.T) {
	t.Parallel()
	s := leverageSnapshot(1.5)
	s.PositionCollateralValueRaw, s.PositionDebtValueRaw, s.PositionDebtRaw = 136_000_000, 36_000_000, 36_000_000
	s.LTVBPS = 2647
	target, ok := partialWithdrawalTargetLTVBPS(s)
	if !ok || target != 2647 {
		t.Fatalf("desired ceiling leaked into withdrawal: %d", target)
	}
}

// Old durable targets decode, but carry no authority for a new raw borrow.
func TestCapacitySizedOldTargetAndMarkerStayReadable(t *testing.T) {
	t.Parallel()
	target, err := decodeLeverageTarget([]byte(`{"lane":"OnRe/ONyc/USDC","level":1.5,"decidedAt":"2026-09-28T12:00:00Z"}`))
	if err != nil || target == nil {
		t.Fatal(err)
	}
	s := leverageSnapshot(1)
	s.RouteLane, s.StrategyKey = onreONycUSDC, onreONycUSDC
	s.BorrowCapacityKnown, s.LeverageBorrow150Raw = true, 50_000_000
	applyLeverageTarget(&s, target)
	if got := Decide(s); got.Action == OpenRouteStep {
		t.Fatalf("old target borrowed: %+v", got)
	}
	marker := Decision{Action: OpenRouteStep, Reason: leverageUpReason, StrategyKey: s.RouteLane, AmountRaw: 150}
	if _, _, _, err := selectKaminoLeg(marker, leverageTestPosition(100_000_000, 0)); err == nil {
		t.Fatal("old marker rebuilt as a new loan")
	}
	s.WithdrawalDemandRaw = 20_000_000
	if got := Decide(s); got.Reason == "leverage_capacity_settled" {
		t.Fatal("old target blocked withdrawal")
	}
}

func TestCapacitySizedPartialWithdrawalDBRestart(t *testing.T) {
	for _, lane := range []string{onreONycUSDC, autoAUTOPYUSD.Lane} {
		t.Run(lane, func(t *testing.T) { runCapacityPartialRestart(t, lane) })
	}
}
func runCapacityPartialRestart(t *testing.T, lane string) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	defer db.Close()
	key := fmt.Sprintf("capacity-partial-%d", time.Now().UnixNano())
	_, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,'{"generation":1,"cycle":1}')`, key)
	if err != nil {
		t.Fatal(err)
	}
	defer db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_operations WHERE route_key=$1`, key)
	if _, err = db.AcquireRouteLease(ctx, key, "capacity-partial", time.Minute); err != nil {
		t.Fatal(err)
	}
	s := leverageSnapshot(1.5)
	s.RouteLane, s.StrategyKey = lane, lane
	s.PositionCollateralRaw, s.PositionCollateralValueRaw = 1_360_000_000, 1_360_000_000
	s.PositionDebtRaw, s.PositionDebtValueRaw, s.LTVBPS = 360_000_000, 360_000_000, 2647
	s.StrategyNAVRaw, s.TotalVaultNAVRaw = 1_000_000_000, 1_000_000_000
	s.WithdrawalDemandRaw = 200_000_000
	initialDebt := s.PositionDebtRaw
	sawRepay, sawStage := false, false
	for i := 0; i < 30; i++ {
		if i == 1 {
			sparse := Observation{Snapshot: base(), ObservedAt: time.Now().UTC()}
			hold := Decision{Action: Hold, Reason: "safety_wait", IdempotencyKey: strings.Repeat("d", 64), StrategyKey: s.RouteLane}
			if _, err := db.RecordDecisionOnManifest(ctx, embeddedTestManifest(t), key, sparse, hold, strings.Repeat("a", 64)); err != nil {
				t.Fatal("sparse safety hold refused", err)
			}
			if context, err := db.LoadPartialWithdrawal(ctx, key); err != nil || context == nil {
				t.Fatal("sparse safety hold erased context", context, err)
			}
		}
		d := Decide(s)
		o := Observation{Snapshot: s, ObservedAt: time.Now().UTC()}
		if i == 0 {
			tx, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.recordDecisionTx(ctx, tx, key, o, d, strings.Repeat("a", 64)); err != nil {
				t.Fatal(err)
			}
			if err = tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if context, err := db.LoadPartialWithdrawal(ctx, key); err != nil || context != nil {
				t.Fatal("rolled-back capture leaked", context, err)
			}
		}
		record, err := db.RecordDecisionOnManifest(ctx, embeddedTestManifest(t), key, o, d, strings.Repeat("a", 64))
		if err != nil {
			t.Fatalf("record %s: %v", d.Reason, err)
		}
		stored, err := db.LoadPartialWithdrawal(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if stored == nil {
			if !sawRepay || !sawStage || s.PositionDebtRaw >= initialDebt || s.LTVBPS > 2647+leverageUpNearBPS {
				t.Fatalf("lost context before completion %+v", s)
			}
			return
		}
		if i == 0 {
			original, _ := json.Marshal(stored)
			for _, kind := range []string{"missing", "foreign", "future"} {
				bad := *stored
				switch kind {
				case "missing":
					bad.OperationID = strings.Repeat("0", 64)
				case "foreign":
					bad.Lane = onreONycUSDC
					if lane == onreONycUSDC {
						bad.Lane = autoAUTOPYUSD.Lane
					}
				case "future":
					bad.Generation = 999
				}
				mutated, _ := json.Marshal(bad)
				if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{partialWithdrawal}',$2::jsonb) WHERE route_key=$1`, key, string(mutated)); err != nil {
					t.Fatal(err)
				}
				if _, err = db.LoadPartialWithdrawal(ctx, key); err == nil {
					t.Fatal("accepted", kind, "origin")
				}
				if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{partialWithdrawal}',$2::jsonb) WHERE route_key=$1`, key, string(original)); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = db.authorizePartialWithdrawalTx(ctx, tx, record.OperationID); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET expected_effects=expected_effects-'partialWithdrawal' WHERE operation_id=$1`, record.OperationID); err != nil {
				t.Fatal(err)
			}
			if err = db.authorizePartialWithdrawalTx(ctx, tx, record.OperationID); err == nil {
				t.Fatal("stale operation context authorized")
			}
			if err = tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
		}
		raw, _ := json.Marshal(stored)
		restarted, err := decodePartialWithdrawal(raw)
		if err != nil {
			t.Fatal(err)
		}
		s.PartialWithdrawalOperationID = ""
		if err = applyPartialWithdrawal(&s, restarted); err != nil {
			t.Fatal(err)
		}
		if s.PartialWithdrawalLTVBPS != 2647 {
			t.Fatal("ratio drifted")
		}
		if wire, sized := partialWithdrawalWireAmount(s, d); sized {
			d.AmountRaw = wire
		}
		if d.Reason == exitPartialRepayReason {
			sawRepay = true
		}
		if d.Reason == partialStageReason {
			sawStage = true
		}
		s = applyPartialLeg(t, s, d, 1)
		// Cancellation after release must still repay and stage the released cash.
		s.WithdrawalDemandRaw = 0
		if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='reconciled',confirmed_slot=$2,updated_at=clock_timestamp() WHERE operation_id=$1`, record.OperationID, 100+i); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("withdrawal did not finish")
}

func TestCapacitySizedFinalBorrowGateRejectsShrinkingRoom(t *testing.T) {
	t.Parallel()
	for _, debt := range []uint64{0, 33_333_333} {
		t.Run(fmt.Sprintf("existing_debt_%d", debt), func(t *testing.T) { capacityFinalBorrowGate(t, debt) })
	}
}
func capacityFinalBorrowGate(t *testing.T, sourceDebt uint64) {
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
	rpc := budgetBuildRPCWithAccounts(t, 5000, 42, accounts)
	if _, err = validateBorrowRequest(context.Background(), fixtureView(t, rpc), r, effects, 42); err != nil {
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
	if _, err = validateBorrowRequest(context.Background(), fixtureView(t, rpc), r, effects, 42); err == nil {
		t.Fatal("shrinking room passed persisted-input final gate")
	}
	binary.LittleEndian.PutUint64(debt[kaminoOutsideBorrowLimitOffset:], 1_000_000_000_000)
	if _, err = validateBorrowRequest(context.Background(), fixtureView(t, rpc), r, effects, 42); err != nil {
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
	if _, err = validateBorrowRequest(context.Background(), fixtureView(t, rpc), r, effects, 42); err == nil {
		t.Fatal("full collateral reserve passed production final borrow validator")
	}
	binary.LittleEndian.PutUint64(collateral[kaminoReserveConfigOffset+160:], originalLimit+originalLiquidity)
	if _, err = validateBorrowRequest(context.Background(), fixtureView(t, rpc), r, effects, 42); err != nil {
		t.Fatal("expanded collateral capacity refused", err)
	}
	after, err := compileKaminoMessageForDelegate(r, delegate)
	if err != nil || !bytes.Equal(message, after) || r.AmountRaw != 10_000_000 {
		t.Fatal("room expansion resized immutable wire")
	}
}

func TestCapacitySizedAuthorizationIsSingleUseBelowInterestTolerance(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	defer db.Close()
	key := fmt.Sprintf("capacity-borrow-%d", time.Now().UnixNano())
	target := LeverageTarget{Lane: onreONycUSDC, Level: 1.75, DecidedAt: time.Now().UTC(), BorrowRaw: 10_000_000, SourceDebtRaw: 20_000_000_000}
	raw, _ := json.Marshal(target)
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,jsonb_build_object('generation',1,'cycle',1,'leverageTarget',$2::jsonb))`, key, string(raw)); err != nil {
		t.Fatal(err)
	}
	defer db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_operations WHERE route_key=$1`, key)
	if _, err := db.AcquireRouteLease(ctx, key, "capacity-borrow", time.Minute); err != nil {
		t.Fatal(err)
	}
	s := leverageSnapshot(1.5)
	s.RouteLane, s.StrategyKey = onreONycUSDC, onreONycUSDC
	s.PositionDebtRaw = int64(target.SourceDebtRaw)
	d := Decision{Action: OpenRouteStep, StrategyKey: s.RouteLane, Reason: leverageUpReason, AmountRaw: int64(target.BorrowRaw), IdempotencyKey: strings.Repeat("c", 64)}
	o := Observation{Snapshot: s, ObservedAt: time.Now().UTC()}
	first, err := db.RecordDecision(ctx, key, o, d, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := db.LoadLeverageTarget(ctx, key)
	if err != nil || stored.OperationID != first.OperationID {
		t.Fatalf("authorization not bound: %+v %v", stored, err)
	}
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='reconciled',updated_at=clock_timestamp() WHERE operation_id=$1`, first.OperationID); err != nil {
		t.Fatal(err)
	}
	s.PositionDebtRaw += int64(target.BorrowRaw)
	if !borrowDebtMatches(uint64(s.PositionDebtRaw), target.SourceDebtRaw) {
		t.Fatal("counterexample did not fit interest tolerance")
	}
	o.Snapshot = s
	if _, err = db.RecordDecision(ctx, key, o, d, strings.Repeat("a", 64)); err == nil {
		t.Fatal("same economic authority borrowed twice")
	}
	applyLeverageTarget(&s, stored)
	s.BorrowCapacityKnown, s.LeverageBorrow175Raw = true, 100_000_000
	if leverageBorrowReceive(s, 1.75) != 0 {
		t.Fatal("consumed target requests a new loan")
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
