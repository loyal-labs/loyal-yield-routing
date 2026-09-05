package backyardrwa

import (
	"context"
	"encoding/binary"
	"math"
	"math/big"
	"reflect"
	"testing"
)

// This is controlled production planning/admission, not execution of a release.
// Real-program borrow/deposit witnesses remain separate verifier measurements.
func fundingContinuationFixture(t *testing.T, output uint64) (Observation, Decision, BridgeExecutionEvidence, RouteManifest, *RPCClient, *jupiterClient, []ConfirmedAccount) {
	t.Helper()
	o, _, _, m, rpc, client, accounts := fundingAdmissionFixtureForSource(t, output, SwapUSDCToDebtStep)
	route := ethenaUSDePYUSD
	o.Snapshot.CollateralIdleRaw, o.Snapshot.PrimeIdleRaw, o.Snapshot.CollateralIdleValueRaw = 1, 1, 0
	o.Snapshot.SquadsIdleRaw, o.Snapshot.DebtIdleRaw = 0, 1_000
	o.Snapshot.PositionDebtRaw, o.Snapshot.PositionDebtValueRaw, o.Snapshot.PayoffDebtRaw = 1_004, 2_008, 1_005
	o.Snapshot.CutoverDrain, o.Snapshot.PostMutationNAVRequired = true, true
	o.Snapshot.LiquidationThresholdBPS = 8_000
	binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 1)
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeSquadsATA).Data[64:72], 0)
	putScaledFraction(accountAt(accounts, route.Kamino.Obligation).Data[1296:1312], new(big.Int).Lsh(big.NewInt(1_004), 60))
	nav := bridgeTestRequest(ReportNAV, 0)
	nav.Report.Sequence, nav.Report.ObservedSlot = 42, 42
	d := Decision{Action: ReportNAV, StrategyKey: o.Snapshot.RouteLane}
	e, _, _, err := bridgeExpectedEffects(d, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return o, d, BridgeExecutionEvidence{Request: nav, ExpectedEffects: e}, m, rpc, client, accounts
}

func TestFundingContinuationReservesNAVReleaseAndCombinedRemainder(t *testing.T) {
	o, d, e, m, rpc, client, accounts := fundingContinuationFixture(t, 20_000)
	if got := Decide(o.Snapshot); got.Action != ReportNAV {
		t.Fatal("post-borrow NAV lost precedence", got)
	}
	plan, err := observePhase3FundingAdmission(context.Background(), rpc, client, m, o, d, e.Request, e.ExpectedEffects)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Exit) != 16 || plan.FundingRelease == nil || plan.FundingSwap == nil || plan.Payoff == nil || plan.Payoff.ThroughUnix != 1360 || plan.ValidThroughSlot > 74 {
		t.Fatal("NAV omitted complete six-window funding return", len(plan.Exit), plan.Payoff)
	}
	current, _, _, err := plan.Input.decode()
	if err != nil || !reflect.DeepEqual(current, e.Request) || plan.Snapshot != o.Snapshot {
		t.Fatal("future release replaced current NAV", err)
	}
	raw, effects, _, err := plan.FundingRelease.decode()
	if err != nil {
		t.Fatal(err)
	}
	release := raw.(KaminoPrimeUSDCRequest)
	swap, swapEffects, _, err := plan.FundingSwap.Input.decode()
	if err != nil || !release.RepaymentRelease || release.ReleaseDebtIdleRaw != 1_000 || effects.Accounts[1].BeforeRaw != 1 || effects.Accounts[1].AfterRaw <= 1 || swap.(JupiterSwapRequest).AmountRaw != effects.Accounts[1].AfterRaw || swapEffects.Accounts[0].BeforeRaw != effects.Accounts[1].AfterRaw {
		t.Fatal("funding did not combine safe release and dust", err)
	}
	var total int64
	for _, step := range plan.Exit {
		total += step.Cost.TotalMicros
	}
	if total != plan.ExitAfterMicros || plan.Exit[0].Action != DeleverRouteStep || plan.Exit[1].Action != ReportNAV || plan.Exit[2].Action != SwapCollateralToDebtStep {
		t.Fatal("release prefix costs omitted")
	}
	s := o.Snapshot
	s.PostMutationNAVRequired, s.CapitalMutated = false, false
	got := Decide(s)
	if got.Action != DeleverRouteStep || got.Reason != "withdrawal_release_repayment_collateral" {
		t.Fatal("dust-only funding still selected", got)
	}
	// Re-admit the actual release against unchanged observed custody. This
	// exercises the production boundary; the future template was never sent.
	next, err := observePhase3FundingAdmission(context.Background(), rpc, client, m, o, got, release, effects)
	if err != nil || len(next.Exit) != 15 || next.FundingRelease != nil || next.Payoff.ThroughUnix != 1300 {
		t.Fatal("mixed-custody release cannot continue", err)
	}
	if binary.LittleEndian.Uint64(accountAt(accounts, ethenaUSDePYUSD.CollateralCustody).Data[64:72]) != 1 || binary.LittleEndian.Uint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72]) != 1_000 {
		t.Fatal("cost projection mutated actual custody")
	}
	if _, _, err := validateRepaymentReleaseRequest(context.Background(), rpc, release, effects, 42); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.CollateralCustody).Data[64:72], 2)
	_, _, err = validateRepaymentReleaseRequest(context.Background(), rpc, release, effects, 42)
	assertBudgetHold(t, err, "repayment_release_effects_changed")
}

func TestFundingContinuationPreservesFundedAndUSDCResiduePaths(t *testing.T) {
	for _, funded := range []bool{false, true} {
		o, d, e, m, rpc, client, accounts := fundingContinuationFixture(t, 20_000)
		o.Snapshot.SquadsIdleRaw = 20_000
		binary.LittleEndian.PutUint64(accountAt(accounts, bridgeSquadsATA).Data[64:72], 20_000)
		if funded {
			o.Snapshot.DebtIdleRaw = 2_000
			binary.LittleEndian.PutUint64(accountAt(accounts, ethenaUSDePYUSD.DebtCustody).Data[64:72], 2_000)
		}
		plan, err := observePhase3FundingAdmission(context.Background(), rpc, client, m, o, d, e.Request, e.ExpectedEffects)
		if err != nil {
			t.Fatal(err)
		}
		if plan.FundingRelease != nil {
			t.Fatal("unnecessary release")
		}
		if funded {
			if plan.FundingSwap != nil || len(plan.Exit) != 12 {
				t.Fatal("funded NAV converted leftover collateral/USDC again")
			}
		} else {
			if plan.FundingSwap == nil || len(plan.Exit) != 14 {
				t.Fatal("USDC funding omitted")
			}
			r, effects, _, err := plan.FundingSwap.Input.decode()
			if err != nil || r.(JupiterSwapRequest).Action != SwapUSDCToDebtStep {
				t.Fatal("dust selected ahead of sufficient USDC", err)
			}
			s := o.Snapshot
			s.PostMutationNAVRequired, s.CapitalMutated = false, false
			decision := Decide(s)
			if decision.Action != SwapUSDCToDebtStep {
				t.Fatal(decision)
			}
			if _, err := observePhase3FundingAdmission(context.Background(), rpc, client, m, o, decision, r, effects); err != nil {
				t.Fatal("actual USDC funding rejected collateral remainder", err)
			}
		}
		_, effects, _, err := plan.PayoffWithdrawal.decode()
		if err != nil || effects.Accounts[1].BeforeRaw != 1 {
			t.Fatal("return lost preserved collateral remainder", err)
		}
	}
}

func TestFundingContinuationRejectsUnderfundedReleaseAndInvalidValuation(t *testing.T) {
	o, d, e, m, rpc, client, _ := fundingContinuationFixture(t, 2)
	_, err := observePhase3FundingAdmission(context.Background(), rpc, client, m, o, d, e.Request, e.ExpectedEffects)
	assertBudgetHold(t, err, "funding_quote_cannot_cover_full_payoff")
	s := o.Snapshot
	s.PostMutationNAVRequired, s.CapitalMutated = false, false
	s.SquadsIdleRaw = 1 // insufficient too: both residues require a release
	if got := Decide(s); got.Reason != "withdrawal_release_repayment_collateral" {
		t.Fatal(got)
	}
	s.CollateralIdleValueRaw = -1
	if got := Decide(s); got.Action != HoldManualRecovery {
		t.Fatal("negative funding NAV accepted", got)
	}
	// Cross products must not wrap into a spurious funding decision.
	s.CollateralIdleRaw, s.CollateralIdleValueRaw, s.PositionDebtRaw, s.PositionDebtValueRaw, s.DebtIdleRaw, s.SquadsIdleRaw = math.MaxInt64, math.MaxInt64, math.MaxInt64, math.MaxInt64, 0, 0
	if action, _ := payoffFundingSource(s, math.MaxInt64); action != "" {
		t.Fatal("margin/overflow admitted insufficient funding", action)
	}
}
