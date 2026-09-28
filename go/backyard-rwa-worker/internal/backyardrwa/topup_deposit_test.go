package backyardrwa

import (
	"context"
	"encoding/binary"
	"reflect"
	"testing"
)

func TestTopupDepositDecisionJoinsDebtFreePosition(t *testing.T) {
	s := liveTopupSnapshot()
	s.VoltrIdleRaw, s.CollateralIdleRaw, s.PrimeIdleRaw, s.MinimumCollateralDepositRaw = 0, 300_000_000, 300_000_000, 1
	got := Decide(s)
	if got.Action != OpenRouteStep || got.Reason != topupDepositReason || got.AmountRaw != s.CollateralIdleRaw || got.Validate() != nil {
		t.Fatalf("top-up collateral was not deposited: %+v", got)
	}
	// Idle Voltr cash waits until this deposit lands (one tranche at a time).
	s.VoltrIdleRaw = 1_295_000_000
	if got := Decide(s); got.Reason != topupDepositReason {
		t.Fatalf("allocation jumped ahead of the pending deposit: %+v", got)
	}
	below := s
	below.MinimumCollateralDepositRaw = below.CollateralIdleRaw + 1
	if got := Decide(below); got.Reason == topupDepositReason || got.Reason == topupAllocationReason {
		t.Fatalf("deposit below the rounding window or allocation beside it: %+v", got)
	}
	for name, mutate := range map[string]func(*Snapshot){
		"demand":     func(s *Snapshot) { s.WithdrawalDemandRaw = 1 },
		"unwind":     func(s *Snapshot) { s.Unwind = true },
		"hard ltv":   func(s *Snapshot) { s.PositionDebtRaw, s.PositionDebtValueRaw, s.LTVBPS, s.DebtIdleRaw = 1, 1, 6000, 1 },
		"report due": func(s *Snapshot) { s.PostMutationNAVRequired = true },
	} {
		c := s
		mutate(&c)
		if got := Decide(c); got.Reason == topupDepositReason {
			t.Fatalf("%s lost priority to the top-up deposit: %+v", name, got)
		}
	}
	// With debt the existing redeposit keeps its own reason (B3 stays off).
	debt := s
	debt.PositionDebtRaw, debt.PositionDebtValueRaw, debt.LTVBPS = 100_000_000, 100_000_000, 3000
	if got := Decide(debt); got.Reason != "single_loop_redeposit" {
		t.Fatalf("leveraged position used the top-up deposit: %+v", got)
	}
}

func TestTopupDepositAdmissionJoinsDebtFreeObligation(t *testing.T) {
	o, d, e, m, rpc, client, accounts := depositAdmissionFixture(t, "")
	route := ethenaUSDePYUSD
	binary.LittleEndian.PutUint64(accountAt(accounts, route.Kamino.Obligation).Data[128:136], 100_000_000)
	o.Snapshot.HasPosition, o.Snapshot.PositionCollateralRaw, o.Snapshot.PositionCollateralValueRaw = true, 100_000_000, 100_000
	flatDecision := d
	d.Reason = topupDepositReason
	plan, err := observePhase3DepositAdmission(context.Background(), rpc, client, m, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	var actions []Action
	var total int64
	for _, step := range plan.Exit {
		actions = append(actions, step.Action)
		total += step.Cost.TotalMicros
	}
	want := []Action{ReportNAV, DeleverRouteStep, ReportNAV, SwapCollateralToStableStep, ReportNAV, StageSquadsToVoltr, ReportNAV, VoltrRestoreIdle, ReportNAV}
	if !reflect.DeepEqual(actions, want) || total != plan.ExitAfterMicros || plan.DepositProjection == nil || plan.Snapshot != o.Snapshot {
		t.Fatalf("incomplete top-up deposit reserve: %v", actions)
	}
	withdraw, _, _, err := plan.PayoffWithdrawal.decode()
	if err != nil || withdraw.(KaminoPrimeUSDCRequest).AmountRaw != 100_909_090 {
		t.Fatal("return does not withdraw the whole enlarged position", err)
	}
	// Final send rechecks the unchanged debt-free position.
	_, _, message, err := plan.Input.decode()
	if err != nil {
		t.Fatal(err)
	}
	wire := append(make([]byte, 65), message...)
	wire[0] = 1
	intent, _ := Phase3IntentDigest(e.Request, plan.Input.Effects)
	op := PersistedOperation{Status: Signed, SignedWire: wire, SignedWireSHA256: sha256Bytes(wire), TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: e.Request.RecentBlockhash, LastValidBlockHeight: e.Request.LastValidBlockHeight}
	auth := phase3OperationAuthorization{GoalID: Phase3GoalID, BuildInput: plan.Input, IntentSHA256: intent, SignedWireSHA256: op.SignedWireSHA256, BridgeAdmission: &plan}
	if _, err := revaluePhase3SignedInput(context.Background(), rpc, auth, op); err != nil {
		t.Fatal(err)
	}
	collateral := accountAt(accounts, route.Kamino.Obligation).Data[128:136]
	binary.LittleEndian.PutUint64(collateral, 100_000_001)
	if _, err := revaluePhase3SignedInput(context.Background(), rpc, auth, op); err == nil {
		t.Fatal("final send accepted a changed position")
	}
	binary.LittleEndian.PutUint64(collateral, 100_000_000)
	// Without the journaled reason, or with debt/demand, a funded obligation
	// is still refused; the top-up reason never admits a flat one.
	if _, err := observePhase3DepositAdmission(context.Background(), rpc, client, m, o, flatDecision, e); err == nil {
		t.Fatal("plain deposit admitted beside a position")
	}
	for name, mutate := range map[string]func(*Observation){
		"debt":   func(o *Observation) { o.Snapshot.PositionDebtRaw, o.Snapshot.PositionDebtValueRaw = 1, 1 },
		"demand": func(o *Observation) { o.Snapshot.WithdrawalDemandRaw = 1 },
		"unwind": func(o *Observation) { o.Snapshot.Unwind = true },
		"flat": func(o *Observation) {
			o.Snapshot.HasPosition, o.Snapshot.PositionCollateralRaw, o.Snapshot.PositionCollateralValueRaw = false, 0, 0
		},
		"moved": func(o *Observation) { o.Snapshot.PositionCollateralRaw++ },
	} {
		bad := o
		mutate(&bad)
		if _, err := observePhase3DepositAdmission(context.Background(), rpc, client, m, bad, d, e); err == nil {
			t.Fatalf("%s: unsafe top-up deposit admitted", name)
		}
	}
}
