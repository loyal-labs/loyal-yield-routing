package backyardrwa

import (
	"context"
	"encoding/binary"
	"math"
	"reflect"
	"testing"
)

func TestDepositRemainderDoesNotRestartEntryLoop(t *testing.T) {
	_, _, _, _, _, _, accounts := depositAdmissionFixtureForPosition(t, "", true)
	minimum, err := kaminoDepositMinimum(accounts, ethenaUSDePYUSD, 42, math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	s := base()
	s.MinimumCollateralDepositRaw = math.MaxInt64 - int64(minimum) + 1
	if s.MinimumCollateralDepositRaw != 3 {
		t.Fatal("unexpected current rounding bound", s.MinimumCollateralDepositRaw)
	}
	if _, err := kaminoDepositMinimum(accounts, ethenaUSDePYUSD, 42, 2); err == nil {
		t.Fatal("planner threshold differs from builder")
	}
	if _, err := kaminoDepositMinimum(accounts, ethenaUSDePYUSD, 42, 3); err != nil {
		t.Fatal(err)
	}
	s.RouteLane, s.StrategyKey = ethenaUSDePYUSD.Lane, ethenaUSDePYUSD.Lane
	s.HasPosition, s.PositionCollateralRaw, s.PositionCollateralValueRaw = true, 100_000_000, 110_000
	s.CollateralIdleRaw, s.PrimeIdleRaw, s.PostMutationNAVRequired = 1, 1, true
	if got := Decide(s); got.Action != ReportNAV {
		t.Fatal("deposit bypassed NAV", got)
	}
	s.PostMutationNAVRequired = false
	if got := Decide(s); got.Reason != "collateral_requires_borrow" {
		t.Fatal("rounding residue blocked borrowing", got)
	}
	s.PositionDebtRaw, s.PositionDebtValueRaw, s.DebtIdleRaw = 1004, 2008, 1000
	if got := Decide(s); got.Action != SwapDebtToCollateralStep {
		t.Fatal(got)
	}
	s.DebtIdleRaw, s.CollateralIdleRaw, s.PrimeIdleRaw = 0, 1_000_000, 1_000_000
	if got := Decide(s); got.Reason != "single_loop_redeposit" {
		t.Fatal("real reinvestment buffer skipped", got)
	}
	s.CollateralIdleRaw, s.PrimeIdleRaw = 1, 1
	if got := Decide(s); got.Reason != "single_loop_position_ready" {
		t.Fatal("rounding residue restarted redeposit", got)
	}
	s.CutoverDrain = true
	if got := Decide(s); got.Reason != "withdrawal_release_repayment_collateral" {
		t.Fatal("rounding rule blocked exit", got)
	}
	s.CutoverDrain, s.MinimumCollateralDepositRaw = false, 0
	if got := Decide(s); got.Reason != "deposit_rounding_window_unavailable" {
		t.Fatal("missing bound treated as dust", got)
	}
}

func TestRedepositAdmissionReservesRefreshedPositionAndCompleteReturn(t *testing.T) {
	o, d, e, m, rpc, client, accounts := depositAdmissionFixtureForPosition(t, "", true)
	plan, err := observePhase3DepositAdmission(context.Background(), rpc, client, m, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	if plan.DepositProjection == nil || plan.BorrowProjection != nil || plan.FundingRelease == nil || plan.PayoffRepayment == nil || plan.PayoffWithdrawal == nil || len(plan.Exit) != 17 || plan.Payoff == nil || plan.Payoff.ThroughUnix != 1420 || plan.ValidThroughSlot > 74 {
		t.Fatal("redeposit omitted refreshed position/complete exit")
	}
	current, _, _, err := plan.Input.decode()
	if err != nil || !reflect.DeepEqual(current, e.Request) || plan.Snapshot != o.Snapshot {
		t.Fatal("poststate replaced current redeposit")
	}
	position, err := decodeKaminoObligation(accountAt(plan.DepositProjection.Accounts, ethenaUSDePYUSD.Kamino.Obligation), ethenaUSDePYUSD.Kamino)
	if err != nil || position.collateralDepositedRaw != 100_909_090 || plan.Payoff.ObservedDebtRaw != 1000 {
		t.Fatal("redeposit lost preexisting receipts/debt", err)
	}
	release, effects, _, err := plan.FundingRelease.decode()
	if err != nil || effects.Accounts[1].BeforeRaw != 1 {
		t.Fatal("redeposit remainder disappeared", err)
	}
	withdrawal, _, _, err := plan.PayoffWithdrawal.decode()
	if err != nil || withdrawal.(KaminoPrimeUSDCRequest).AmountRaw+release.(KaminoPrimeUSDCRequest).AmountRaw != position.collateralDepositedRaw {
		t.Fatal("return omitted receipts", err)
	}
	var total int64
	for _, step := range plan.Exit {
		total += step.Cost.TotalMicros
	}
	if total != plan.ExitAfterMicros || binary.LittleEndian.Uint64(accountAt(accounts, ethenaUSDePYUSD.CollateralCustody).Data[64:72]) != 1_000_000 {
		t.Fatal("cost-only projection changed current custody")
	}
	assertBudgetHold(t, (&Database{}).admitPhase3Deposit(context.Background(), rpc, client, m, "missing", o, d, e), "bridge_admission_database_unavailable")
	_, _, message, _ := plan.Input.decode()
	wire := append(make([]byte, 65), message...)
	wire[0] = 1
	intent, _ := Phase3IntentDigest(e.Request, plan.Input.Effects)
	op := PersistedOperation{Status: Signed, SignedWire: wire, SignedWireSHA256: sha256Bytes(wire), TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: e.Request.RecentBlockhash, LastValidBlockHeight: e.Request.LastValidBlockHeight}
	auth := phase3OperationAuthorization{GoalID: Phase3GoalID, BuildInput: plan.Input, IntentSHA256: intent, SignedWireSHA256: op.SignedWireSHA256, BridgeAdmission: &plan}
	if _, err := revaluePhase3SignedInput(context.Background(), rpc, auth, op); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72], 1)
	_, err = revaluePhase3SignedInput(context.Background(), rpc, auth, op)
	assertBudgetHold(t, err, "redeposit_debt_custody_changed")
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72], 0)
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.Kamino.Obligation).Data[128:136], 100_000_001)
	_, err = revaluePhase3SignedInput(context.Background(), rpc, auth, op)
	assertBudgetHold(t, err, "redeposit_position_changed")
}

func TestRedepositAdmissionRejectsChangedDebtReceiptsAndConservation(t *testing.T) {
	for _, variant := range []string{"debt", "receipts", "debt_cash", "conservation", "clock", "failed", "stale", "missing"} {
		t.Run(variant, func(t *testing.T) {
			o, d, e, m, rpc, client, _ := depositAdmissionFixtureForPosition(t, variant, true)
			if _, err := observePhase3DepositAdmission(context.Background(), rpc, client, m, o, d, e); err == nil {
				t.Fatal("unsafe redeposit admitted")
			}
		})
	}
	o, d, e, m, rpc, client, _ := depositAdmissionFixtureForPosition(t, "", true)
	o.Snapshot.CutoverDrain = true
	_, err := observePhase3DepositAdmission(context.Background(), rpc, client, m, o, d, e)
	assertBudgetHold(t, err, "complete_redeposit_return_unavailable")
}
