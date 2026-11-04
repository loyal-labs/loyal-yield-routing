package backyard

import (
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"
)

func TestAtomicSalesReplayActualRepaymentProceeds(t *testing.T) {
	data, err := os.ReadFile("testdata/earn-max-sales-2026-10-29.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Sales []struct {
			Expected        ExpectedEffects              `json:"expected"`
			Receipt         ConfirmedTransactionEvidence `json:"receipt"`
			ReturnedRaw     uint64                       `json:"returnedRaw"`
			ShortfallMicros int64                        `json:"shortfallMicros"`
			MarkSlot        int64                        `json:"markSlot"`
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil || len(fixture.Sales) != 9 {
		t.Fatal("invalid retained sales", err)
	}
	for _, f := range fixture.Sales {
		t.Run(f.Receipt.Signature, func(t *testing.T) {
			if f.Receipt.Slot < f.MarkSlot || f.Receipt.Slot-f.MarkSlot > 100 {
				t.Fatal("replay mark too far from transaction")
			}
			_, body, err := ReconcileConfirmedTransaction(rwa, f.Expected, f.Receipt)
			if err != nil {
				t.Fatal(err)
			}
			var reconciled struct {
				Sale *realizedSale `json:"sale"`
			}
			if err := json.Unmarshal(body, &reconciled); err != nil || reconciled.Sale == nil || reconciled.Sale.ReturnedRaw != f.ReturnedRaw {
				t.Fatalf("missing or wrong realized proceeds: %s (%v)", body, err)
			}
			cost, ok := reconciled.Sale.exitCost(time.Now())
			shortfall, err := cost.liabilityRaw(f.Expected.Sale.MarkValueRaw)
			if !ok || err != nil || math.Abs(float64(int64(shortfall)-f.ShortfallMicros)) > 30 {
				t.Fatalf("lost cost: %+v, shortfall=%d want=%d (%v)", cost, shortfall, f.ShortfallMicros, err)
			}
			// Legacy persisted operations remain reconcilable without inventing a sale.
			legacy := f.Expected
			legacy.Sale = nil
			if _, _, err := ReconcileConfirmedTransaction(rwa, legacy, f.Receipt); err != nil {
				t.Fatal("legacy replay broken", err)
			}
			bad := f.Receipt
			bad.PostTokenBalances = append([]TransactionTokenBalance(nil), bad.PostTokenBalances...)
			bad.PostTokenBalances[len(bad.PostTokenBalances)-1].Authority = backyardDelegate
			if _, _, err := ReconcileConfirmedTransaction(rwa, f.Expected, bad); err == nil {
				t.Fatal("custody substitution accepted")
			}
		})
	}
}

func TestStepSaleCarriesRepaymentAndClippedPayoff(t *testing.T) {
	route, accounts := onreEntryNAVFixture(t, 0, 0)
	accounts = append(accounts, clockFixture())
	step, _, err := stepTestBuild(t, route.Lane, false, -1)
	if err != nil {
		t.Fatal(err)
	}
	effect := func(address, mint, program string, before, after uint64) ExpectedAccountEffect {
		return ExpectedAccountEffect{Address: address, Mint: mint, Owner: program, Authority: route.Kamino.Vault, BeforeRaw: before, AfterRaw: after}
	}
	step.Close.AmountRaw = 1_000
	expected := stepEffects(false, effect(route.CollateralCustody, route.Kamino.CollateralMint, route.CollateralTokenProgram, 0, 0),
		effect(route.DebtCustody, route.Kamino.DebtMint, route.DebtTokenProgram, 200, 0))
	expected = withStepSale(accounts, route, StepExecutionEvidence{Request: step, ExpectedEffects: expected}).ExpectedEffects
	if expected.Sale == nil || expected.Sale.MaxRepayRaw != 1_000 {
		t.Fatal("atomic down step omitted its sale")
	}
	// The swap pays 900; KLend clips a 1,000 request to 800. The custody
	// had 200 before, so it ends at 300. Actual proceeds are still 900.
	receipt := ConfirmedTransactionEvidence{Signature: "local-clipped-payoff", Slot: 77}
	for _, e := range expected.Accounts {
		before := TransactionTokenBalance{Address: e.Address, OwnerProgram: e.Owner, Mint: e.Mint, Authority: e.Authority, Raw: e.BeforeRaw}
		after := before
		after.Raw = e.AfterRaw
		if e.Address == route.DebtCustody {
			after.Raw = 300
		}
		receipt.PreTokenBalances = append(receipt.PreTokenBalances, before)
		receipt.PostTokenBalances = append(receipt.PostTokenBalances, after)
	}
	reserve := TransactionTokenBalance{Address: route.DebtLiquiditySupply, OwnerProgram: route.DebtTokenProgram, Mint: route.Kamino.DebtMint, Authority: route.Kamino.MarketAuthority, Raw: 5_000}
	receipt.PreTokenBalances = append(receipt.PreTokenBalances, reserve)
	reserve.Raw += 800
	receipt.PostTokenBalances = append(receipt.PostTokenBalances, reserve)
	_, body, err := ReconcileConfirmedTransaction(rwa, expected, receipt)
	var got struct {
		Sale realizedSale `json:"sale"`
	}
	if err != nil || json.Unmarshal(body, &got) != nil || got.Sale.ReturnedRaw != 900 {
		t.Fatalf("clipped repayment: %s (%v)", body, err)
	}
	for _, raw := range []uint64{4_999, 6_001} {
		receipt.PostTokenBalances[2].Raw = raw
		if _, _, err := ReconcileConfirmedTransaction(rwa, expected, receipt); err == nil {
			t.Fatal("invalid reserve debit/repayment accepted")
		}
	}
	free := realizedSale{ExpectedSale: *expected.Sale, ReturnedRaw: expected.Sale.MarkValueRaw + 1, Slot: 77}
	if c, ok := free.exitCost(time.Now()); !ok || c.BPS != 1 {
		t.Fatal("favorable sale retained an old expensive cost", c, ok)
	} else if (ExitCost{BPS: 20_000_000}).outlier(&c) {
		t.Fatal("a free sale suppressed the next priced quote")
	}
}
