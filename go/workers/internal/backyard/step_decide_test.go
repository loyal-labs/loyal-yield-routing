package backyard

import (
	"context"
	"encoding/binary"
	"math/big"
	"reflect"
	"strings"
	"testing"
)

// Every leverage move decides one atomic step: hard LTV, a drain, the down
// moves to 1x and 1.5x and a partial withdrawal's release go down; leverage
// up goes up. Each down step's repay is sized from the decided snapshot.
func TestLeverageMovesDecideAtomicSteps(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{autoAUTOPYUSD.Lane, autoAUTOWYLDSLane, onreONycUSDC} {
		hard := leverageExitSnapshot(lane)
		hard.LTVBPS = 6000
		drain := leverageExitSnapshot(lane)
		drain.Unwind = true
		down := leverageExitSnapshot(lane)
		down.LeverageTargetLevel = 1
		partialDown := leverageSnapshot(1.75)
		partialDown.RouteLane, partialDown.StrategyKey, partialDown.LeverageTargetLevel = lane, lane, 1.5
		withdrawal := livePartialSnapshot()
		withdrawal.RouteLane, withdrawal.StrategyKey = lane, lane
		for _, c := range []struct {
			s      Snapshot
			reason string
			full   bool
		}{
			{hard, hardLTVDeleverReason, false},
			{drain, withdrawalDeleverReason, true},
			{down, leverageDownReason, true},
			{partialDown, leverageDownPartialReason, false},
			{withdrawal, partialReleaseReason, false},
		} {
			got := Decide(c.s)
			if got.Action != DeleverRouteStep || got.Reason != c.reason || got.StrategyKey != lane {
				t.Fatalf("%s %s: decided %+v", lane, c.reason, got)
			}
			if up, ok := stepDecision(c.s, got); !ok || up {
				t.Fatalf("%s %s: not a down step", lane, c.reason)
			}
			if c.reason == partialReleaseReason {
				continue
			}
			repay, full := stepDownRepay(c.s, c.reason)
			if full != c.full || repay == 0 || repay > uint64(max(c.s.PositionDebtRaw, c.s.PayoffDebtRaw)) {
				t.Fatalf("%s %s: repay %d full %t", lane, c.reason, repay, full)
			}
			if full && repay != uint64(max(c.s.PositionDebtRaw, c.s.PayoffDebtRaw)) {
				t.Fatalf("%s %s: a full step repays the payoff bound, not %d", lane, c.reason, repay)
			}
		}
	}
}

// A partial down step lands at its target LTV: repaying r of debt with r of
// collateral value leaves (D-r)/(C-r) at the target, within a raw unit.
func TestStepDebtToLTVLandsOnTheTarget(t *testing.T) {
	t.Parallel()
	s := leverageSnapshot(1.75)
	for _, target := range []int64{hardLTVTargetBPS(s), leverageLevelLTVBPS(1.5), 2000} {
		r := stepDebtToLTVRaw(s, target)
		if r <= 0 || r >= s.PositionDebtRaw {
			t.Fatalf("target %d: repay %d of %d", target, r, s.PositionDebtRaw)
		}
		after := (s.PositionDebtValueRaw - r) * 10_000 / (s.PositionCollateralValueRaw - r)
		if after > target || after < target-1 {
			t.Fatalf("target %d: landed at %d", target, after)
		}
	}
	if r := stepDebtToLTVRaw(s, 9_000); r != 0 {
		t.Fatalf("a target above the current LTV repaid %d", r)
	}
}

// The worker carries a step decision through prepareStep, record, bind (the
// StepRequest) and buildStep, never through the single-leg Kamino path.
func TestWorkerDispatchesAtomicStep(t *testing.T) {
	s := leverageExitSnapshot(autoAUTOPYUSD.Lane)
	s.LTVBPS = 6000
	o := tickObservation(s)
	want := Decide(s)
	if want.Reason != hardLTVDeleverReason {
		t.Fatal(want)
	}
	request := StepRequest{Open: KaminoPrimeUSDCRequest{RouteLane: s.RouteLane}, Close: KaminoPrimeUSDCRequest{RouteLane: s.RouteLane}}
	var order []string
	worker := &Worker{routeKey: productionRouteKey, manifest: readyWorkerManifest(t), runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe:         func(context.Context) (Observation, error) { return o, nil },
		prepareKamino: func(context.Context, RouteManifest, Decision, Observation) (Observation, KaminoExecutionEvidence, error) {
			t.Fatal("a step reached the single-leg Kamino path")
			return Observation{}, KaminoExecutionEvidence{}, nil
		},
		prepareStep: func(_ context.Context, _ RouteManifest, d Decision, _ Observation) (Observation, StepExecutionEvidence, error) {
			if d != want {
				t.Fatal("step identity changed")
			}
			order = append(order, "prepare")
			return o, StepExecutionEvidence{Request: request}, nil
		},
		recordDecision: func(context.Context, string, Observation, Decision, string) (DecisionRecord, error) {
			order = append(order, "record")
			return DecisionRecord{OperationID: "local-step", Status: Decided}, nil
		},
		bind: func(_ context.Context, id string, _ Observation, d Decision, r any, _ ExpectedEffects) error {
			if id != "local-step" || d != want || !reflect.DeepEqual(r, request) {
				t.Fatalf("bound %T", r)
			}
			order = append(order, "admit")
			return nil
		},
		buildStep: func(_ context.Context, id string, e StepExecutionEvidence) error {
			if id != "local-step" || !reflect.DeepEqual(e.Request, request) {
				t.Fatal("build drift")
			}
			order = append(order, "build")
			return nil
		},
	}}
	if err := worker.Tick(context.Background()); err != nil || strings.Join(order, ",") != "prepare,record,admit,build" {
		t.Fatal(order, err)
	}
}

// A step's close moves its conversion's minimum; the excess beside the
// position joins it: borrowed-token excess (above dust) and Squads cash
// beside USDC debt convert to collateral, and collateral deposits.
func TestStepConversionExcessJoinsThePosition(t *testing.T) {
	t.Parallel()
	pyusd := leverageSnapshot(1.5)
	pyusd.RouteLane, pyusd.StrategyKey, pyusd.LeverageTargetLevel = autoAUTOPYUSD.Lane, autoAUTOPYUSD.Lane, 1.5
	pyusd.MinimumCollateralDepositRaw = 1
	if got := Decide(pyusd); got.Action != Hold {
		t.Fatalf("a settled position moved: %+v", got)
	}
	excess := pyusd
	excess.DebtIdleRaw = 2_500_000
	if got := Decide(excess); got.Action != SwapDebtToCollateralStep || got.Reason != stepResidueReason || got.AmountRaw != 2_500_000 {
		t.Fatalf("PYUSD excess: %+v", got)
	}
	excess.DebtIdleRaw = partialWithdrawalDustRaw
	if got := Decide(excess); got.Action != Hold {
		t.Fatalf("dust started a leg: %+v", got)
	}
	excess.DebtIdleRaw, excess.CollateralIdleRaw, excess.PrimeIdleRaw = 0, 2_400_000, 2_400_000
	if got := Decide(excess); got.Action != OpenRouteStep || got.Reason != topupDepositReason || got.AmountRaw != 2_400_000 {
		t.Fatalf("AUTO excess: %+v", got)
	}
	maple := leverageSnapshot(1.5)
	maple.RouteLane, maple.StrategyKey, maple.LeverageTargetLevel = SelectedRouteID, SelectedRouteID, 1.5
	maple.MinimumCollateralDepositRaw = 1
	maple.SquadsIdleRaw = 2_500_000
	if got := Decide(maple); got.Action != SwapDebtToCollateralStep || got.Reason != stepResidueReason || got.AmountRaw != 2_500_000 || got.Validate() != nil {
		t.Fatalf("Squads cash beside USDC debt: %+v", got)
	}
	maple.SquadsIdleRaw, maple.CollateralIdleRaw, maple.PrimeIdleRaw = 0, 2_400_000, 2_400_000
	got := Decide(maple)
	if got.Action != OpenRouteStep || got.Reason != topupDepositReason {
		t.Fatalf("syrupUSDC excess: %+v", got)
	}
	position := leverageTestPosition(3_000_000_000, 1_000_000_000)
	if leg, wire, _, err := selectKaminoLeg(got, position); err != nil || leg != kaminoLegDeposit || wire != 2_400_000 {
		t.Fatalf("excess deposit beside debt: leg %d wire %d %v", leg, wire, err)
	}
}

// A step on a shared-USDC-debt lane never spends the Squads bridge cash:
// its debt custody balance only starts the effects.
func TestStepNeverSpendsBridgeCash(t *testing.T) {
	t.Parallel()
	_, _, _, _, accounts, route := leverage175Fixture(t)
	for _, reserve := range []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve} {
		putKey(t, accountAt(accounts, reserve).Data[5112:5144], kaminoScopePrices)
	}
	// $50 of bridge cash waits in the Squads USDC account, the lane's debt
	// custody.
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeSquadsATA).Data[64:72], 50_000_000)
	// The obligation's own values: $1,750 deposited at a 66% borrow limit
	// against $750 of debt.
	obligation := accountAt(accounts, route.Kamino.Obligation).Data
	for offset, value := range map[int]int64{1192: 1750, 2208: 750, 2224: 750, 2240: 1155} {
		putScaledFraction(obligation[offset:offset+16], new(big.Int).Lsh(big.NewInt(value), 60))
	}
	st, err := readStepState(append(accounts, clockFixture()), route, 42)
	if err != nil {
		t.Fatal(err)
	}
	if st.debtBalance != 50_000_000 || st.heldDebt != 0 || st.heldCollateral != st.collateralBalance {
		t.Fatalf("OnRe step state: balance %d held %d", st.debtBalance, st.heldDebt)
	}
	// Held debt covering the repay shortens the conversion, never refuses it.
	st.heldDebt = 10_000
	s := leverageSnapshot(1.5)
	if _, convert, err := planDownStep(s, Decision{Reason: hardLTVDeleverReason}, st, 5_000); err != nil || convert == 0 {
		t.Fatalf("held debt over the repay refused the step: %d %v", convert, err)
	}
}
