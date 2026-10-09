package backyard

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// Plan B3 leg 2 fixture: a funded debt-free position with 20,000 raw bridge
// USDC in Squads (the top-up tranche).
func topupSwapAdmissionFixture(t *testing.T) (Observation, Decision, JupiterExecutionEvidence, RouteManifest, *chain.Client, *jupiterClient, []ConfirmedAccount) {
	t.Helper()
	o, _, _, m, rpc, client, accounts := fundingAdmissionFixtureForSource(t, 20_000, SwapUSDCToDebtStep)
	clear(accountAt(accounts, ethenaUSDePYUSD.Kamino.Obligation).Data[1208:1408])
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72], 0)
	o.Snapshot.PositionDebtRaw, o.Snapshot.PositionDebtValueRaw, o.Snapshot.DebtIdleRaw, o.Snapshot.PayoffDebtRaw = 0, 0, 0, 0
	d := Decision{Action: SwapStableToCollateralStep, AmountRaw: 20_000, StrategyKey: o.Snapshot.RouteLane, Reason: topupSwapReason, IdempotencyKey: "topup-swap"}
	e, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, m, d, 20_000, 0, 42)
	if err != nil {
		t.Fatal(err)
	}
	e.Request.EntryReturnReserved, e.Request.TopupReturnReserved = true, true
	return o, d, e, m, rpc, client, accounts
}

func TestTopupSwapDecisionBesideDebtFreePosition(t *testing.T) {
	s := liveIdleDebtSnapshot()
	s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw, s.LTVBPS, s.DebtIdleRaw = 0, 0, 0, 0, 0
	s.SquadsIdleRaw = 336_000_000
	got := Decide(s)
	if got.Action != SwapStableToCollateralStep || got.Reason != topupSwapReason || got.AmountRaw != s.SquadsIdleRaw || got.Validate() != nil {
		t.Fatalf("top-up cash was not swapped: %+v", got)
	}
	for name, mutate := range map[string]func(*Snapshot){
		"uncovered demand": func(s *Snapshot) { s.WithdrawalDemandRaw, s.VoltrIdleRaw = 10, 0 },
		"unwind":           func(s *Snapshot) { s.Unwind = true },
		"hard ltv":         func(s *Snapshot) { s.PositionDebtRaw, s.PositionDebtValueRaw, s.LTVBPS = 1, 1, 6000 },
		"report due":       func(s *Snapshot) { s.PostMutationNAVRequired = true },
	} {
		c := s
		mutate(&c)
		if got := Decide(c); got.Reason == topupSwapReason {
			t.Fatalf("%s lost priority to the top-up swap: %+v", name, got)
		}
	}
}

func TestTopupSwapAdmissionReservesPositionReturn(t *testing.T) {
	o, d, e, m, rpc, client, _ := topupSwapAdmissionFixture(t)
	plan, err := observePhase3EntrySwapAdmission(context.Background(), rpc, client, m, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	var actions []Action
	var total int64
	for _, step := range plan.Exit {
		actions = append(actions, step.Action)
		total += step.Cost.TotalMicros
	}
	if len(actions) < 4 || actions[0] != ReportNAV || actions[1] != DeleverRouteStep || total != plan.ExitAfterMicros || plan.Snapshot != o.Snapshot {
		t.Fatalf("top-up swap lacks the complete position return: %v", actions)
	}
	withdraw, effects, _, err := plan.PayoffWithdrawal.decode()
	upper, _ := withdrawalUSDCExitEstimate(e.Request.QuotedOutputRaw)
	if err != nil || withdraw.(KaminoPrimeUSDCRequest).AmountRaw != uint64(o.Snapshot.PositionCollateralRaw) || effects.Accounts[1].BeforeRaw != upper {
		t.Fatal("return omits the position or the swapped collateral", err)
	}
	if _, err := observePhase3KnownBuildCost(context.Background(), rpc, e.Request, e.ExpectedEffects); err != nil {
		t.Fatal("final-send revaluation refused the top-up swap", err)
	}
	// The flag, the journaled reason and the position must agree.
	for name, mutate := range map[string]func(*Observation, *Decision, *JupiterExecutionEvidence){
		"no flag": func(_ *Observation, _ *Decision, e *JupiterExecutionEvidence) { e.Request.TopupReturnReserved = false },
		"reason":  func(_ *Observation, d *Decision, _ *JupiterExecutionEvidence) { d.Reason = "usdc_requires_collateral" },
		"debt":    func(o *Observation, _ *Decision, _ *JupiterExecutionEvidence) { o.Snapshot.PositionDebtRaw = 1 },
		"demand":  func(o *Observation, _ *Decision, _ *JupiterExecutionEvidence) { o.Snapshot.WithdrawalDemandRaw = 1 },
		"partial": func(o *Observation, _ *Decision, _ *JupiterExecutionEvidence) { o.Snapshot.SquadsIdleRaw++ },
		"flat": func(o *Observation, _ *Decision, _ *JupiterExecutionEvidence) {
			o.Snapshot.HasPosition, o.Snapshot.PositionCollateralRaw = false, 0
		},
	} {
		bo, bd, be := o, d, e
		mutate(&bo, &bd, &be)
		if _, err := observePhase3EntrySwapAdmission(context.Background(), rpc, client, m, bo, bd, be); err == nil {
			t.Fatalf("%s: unsafe top-up swap admitted", name)
		}
	}
	// A flat entry swap may not carry the top-up flag.
	flat := e
	flat.Request.TopupReturnReserved = false
	if _, err := observePhase3KnownBuildCost(context.Background(), rpc, flat.Request, flat.ExpectedEffects); err == nil {
		t.Fatal("entry swap without the top-up flag accepted a funded obligation")
	}
}
