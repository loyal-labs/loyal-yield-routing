package backyard

import (
	"context"
	"encoding/binary"
	"math/big"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

// This is controlled production planning/admission, not execution of a release.
// Real-program borrow/deposit witnesses remain separate verifier measurements.
func fundingContinuationFixture(t *testing.T, output uint64) (Observation, Decision, BridgeExecutionEvidence, RouteManifest, *chain.Client, *jupiter.Client, []ConfirmedAccount) {
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
