package backyard

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"testing"
)

func TestDebtTopupReturnUsesIdleFullFunding(t *testing.T) {
	for _, tc := range []struct {
		name       string
		collateral uint64
		funded     bool
	}{{"funded", 60_000_000_000, true}, {"insufficient", 12_000_000_000, false}, {"optimistic_only", 42_300_000_000, false}} {
		t.Run(tc.name, func(t *testing.T) {
			o, _, _, m, rpc, client, accounts := autoEmergencyFundingFixture(t, tc.collateral)
			d := Decision{Action: SwapStableToCollateralStep, StrategyKey: autoAUTOPYUSD.Lane, Reason: topupSwapReason, AmountRaw: int64(tc.collateral / 6000)}
			entry, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, m, d, uint64(d.AmountRaw), 0, o.Snapshot.Slot)
			if err != nil {
				t.Fatal(err)
			}
			entry.Request.EntryReturnReserved, entry.Request.TopupReturnReserved = true, true
			binary.LittleEndian.PutUint64(accountAt(accounts, bridgeSquadsATA).Data[64:72], uint64(d.AmountRaw))
			binary.LittleEndian.PutUint64(accountAt(accounts, autoAUTOPYUSD.CollateralCustody).Data[64:72], 0)
			current, err := m.observePhase3KnownBuildCost(context.Background(), rpc, entry.Request, entry.ExpectedEffects)
			if err != nil {
				t.Fatal(err)
			}
			binary.LittleEndian.PutUint64(accountAt(accounts, bridgeSquadsATA).Data[64:72], 0)
			binary.LittleEndian.PutUint64(accountAt(accounts, autoAUTOPYUSD.CollateralCustody).Data[64:72], tc.collateral)
			before, _ := json.Marshal(accounts)
			plan, err := pricePhase3ProjectedPositionReturn(context.Background(), rpc, client, m, o, d, entry.Request, entry.ExpectedEffects, current, phase3KaminoProjection{Slot: o.Snapshot.Slot, Accounts: accounts})
			after, _ := json.Marshal(accounts)
			if !bytes.Equal(before, after) {
				t.Fatal("pricing mutated actual accounts")
			}
			if !tc.funded {
				assertBudgetHold(t, err, "no_safe_repayment_collateral_release")
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if plan.BorrowRelease != nil || plan.ExitCycles != 0 || plan.FundingSwap == nil || plan.PayoffRepayment == nil || plan.PayoffWithdrawal == nil {
				t.Fatal("idle funding required a position release or omitted the remaining exit")
			}
			if len(plan.Exit) < 5 || plan.Exit[0].Action != ReportNAV || plan.Exit[1].Action != SwapCollateralToDebtStep || plan.Exit[2].Action != ReportNAV || plan.Exit[3].Action != DeleverRouteStep {
				t.Fatal("funding did not precede repayment")
			}
			funding, _, _, err := plan.FundingSwap.Input.decodeWithManifest(m)
			if err != nil {
				t.Fatal(err)
			}
			r := funding.(JupiterSwapRequest)
			floor, err := jupiterInstructionWireFloor(r.Instruction)
			if err != nil || min(floor, r.MinimumOutputRaw) < plan.Payoff.UpperDebtRaw || r.AmountRaw != tc.collateral {
				t.Fatal("repayment depends on optimistic output or extra collateral", err)
			}
			for _, step := range plan.Exit {
				request, _, wire, err := step.Template.decodeWithManifest(m)
				if err != nil || len(wire) == 0 {
					t.Fatal("unbuildable exit leg", err)
				}
				if k, ok := request.(KaminoPrimeUSDCRequest); ok && k.FullPayoff {
					t.Fatal("cost-only pricing created payoff authority")
				}
			}
			withdrawal, _, _, err := plan.PayoffWithdrawal.decodeWithManifest(m)
			if err != nil || withdrawal.(KaminoPrimeUSDCRequest).AmountRaw != uint64(o.Snapshot.PositionCollateralRaw) {
				t.Fatal("funding reduced position collateral before payoff", err)
			}
		})
	}
}
