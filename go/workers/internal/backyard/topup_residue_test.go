package backyard

import (
	"context"
	"encoding/binary"
	"testing"
)

// Plan B3 leg 0: the PYUSD residue left beside a debt-free position after
// idle_debt_repay is converted to bridge USDC, keeping the position.
func TestDebtResidueSwapBesideDebtFreePosition(t *testing.T) {
	t.Parallel()
	s := liveIdleDebtSnapshot()
	s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw, s.LTVBPS, s.DebtIdleRaw = 0, 0, 0, 0, 36_520_000
	got := Decide(s)
	if got.Action != SwapDebtToUSDCStep || got.Reason != debtResidueSwapReason || got.AmountRaw != s.DebtIdleRaw || got.Validate() != nil {
		t.Fatalf("residue was not converted: %+v", got)
	}
	// Withdrawal demand, unwind and a due report keep priority.
	for name, mutate := range map[string]func(*Snapshot){
		"uncovered demand": func(s *Snapshot) { s.WithdrawalDemandRaw, s.VoltrIdleRaw = 10, 0 },
		"unwind":           func(s *Snapshot) { s.Unwind = true },
		"report due":       func(s *Snapshot) { s.PostMutationNAVRequired = true },
	} {
		c := s
		mutate(&c)
		if got := Decide(c); got.Reason == debtResidueSwapReason {
			t.Fatalf("%s lost priority to the residue swap: %+v", name, got)
		}
	}
	// The converted USDC then joins the top-up swap; it is never stranded.
	s.DebtIdleRaw, s.SquadsIdleRaw = 0, 36_000_000
	if got := Decide(s); got.Action != SwapStableToCollateralStep || got.Reason != topupSwapReason {
		t.Fatalf("working cash beside a position: %+v", got)
	}
}

func TestDebtResidueSwapAdmissionKeepsPositionAndReservesItsReturn(t *testing.T) {
	t.Parallel()
	// The funded-payoff fixture after its payoff: debt-free position, 10,000
	// raw PYUSD residue in debt custody.
	o, _, _, m, rpc, client, accounts := payoffAdmissionFixture(t, 20_000)
	clear(accountAt(accounts, ethenaUSDePYUSD.Kamino.Obligation).Data[1208:1408])
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72], 10_000)
	o.Snapshot.PositionDebtRaw, o.Snapshot.PositionDebtValueRaw, o.Snapshot.DebtIdleRaw = 0, 0, 10_000
	swap := Decision{Action: SwapDebtToUSDCStep, AmountRaw: 10_000, StrategyKey: o.Snapshot.RouteLane, Reason: debtResidueSwapReason}
	decided := o.Snapshot
	decided.LiquidationThresholdBPS = 8000
	if got := Decide(decided); got.Action != swap.Action || got.Reason != swap.Reason || got.AmountRaw != swap.AmountRaw {
		t.Fatalf("fixture state is not the residue decision: %+v", got)
	}
	evidence, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, m, testPolicies(t), swap, 10_000, uint64(o.Snapshot.SquadsIdleRaw), o.Snapshot.Slot)
	if err != nil {
		t.Fatal(err)
	}
	request, effects := evidence.Request, evidence.ExpectedEffects
	residue, err := observePhase3CollateralReturnAdmission(context.Background(), rpc, fixtureView(t, rpc), client, m, o, swap, request, effects)
	if err != nil {
		t.Fatal(err)
	}
	actions := []Action{}
	var total int64
	for _, step := range residue.Exit {
		actions = append(actions, step.Action)
		total += step.Cost.TotalMicros
	}
	if len(actions) < 4 || actions[0] != ReportNAV || actions[1] != DeleverRouteStep || total != residue.ExitAfterMicros || residue.Snapshot != o.Snapshot {
		t.Fatalf("residue swap did not reserve the complete position return: %v", actions)
	}
	// Without the journaled reason, or with debt or demand, the position is
	// still refused exactly as before.
	for name, mutate := range map[string]func(*Observation, *Decision){
		"plain swap": func(_ *Observation, d *Decision) { d.Reason = "withdrawal_convert_debt_residue_to_usdc" },
		"debt":       func(o *Observation, _ *Decision) { o.Snapshot.PositionDebtRaw = 1 },
		"demand":     func(o *Observation, _ *Decision) { o.Snapshot.WithdrawalDemandRaw = 1 },
		"collateral": func(o *Observation, _ *Decision) { o.Snapshot.CollateralIdleRaw, o.Snapshot.PrimeIdleRaw = 1, 1 },
	} {
		bad, badDecision := o, swap
		mutate(&bad, &badDecision)
		if _, err := observePhase3CollateralReturnAdmission(context.Background(), rpc, fixtureView(t, rpc), client, m, bad, badDecision, request, effects); err == nil {
			t.Fatalf("%s admitted beside a position", name)
		}
	}
}
