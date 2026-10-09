package backyard

import (
	"encoding/json"
	"testing"
)

func topupReceipt(t *testing.T, expected ExpectedEffects, slot int64, actualAfter ...uint64) ConfirmedTransactionEvidence {
	t.Helper()
	r := ConfirmedTransactionEvidence{Finalized: true, Signature: sha256Bytes([]byte(string(rune(slot)))), Slot: slot}
	for i, e := range expected.Accounts {
		r.PreTokenBalances = append(r.PreTokenBalances, TransactionTokenBalance{Address: e.Address, OwnerProgram: e.Owner, Mint: e.Mint, Authority: e.Authority, Raw: e.BeforeRaw})
		r.PostTokenBalances = append(r.PostTokenBalances, TransactionTokenBalance{Address: e.Address, OwnerProgram: e.Owner, Mint: e.Mint, Authority: e.Authority, Raw: actualAfter[i]})
	}
	if expected.ReturnData != nil {
		r.ReturnData = &ProgramReturnData{ProgramID: expected.ReturnData.ProgramID, DataBase64: expected.ReturnData.DataBase64}
	}
	return r
}

func TestTopupTrancheReceiptConservationAcrossRestart(t *testing.T) {
	origin, swapID, depositID := sha256Bytes([]byte("allocation")), sha256Bytes([]byte("swap")), sha256Bytes([]byte("deposit"))
	loan, _ := topupLoanFixture(t)
	binding := topupTrancheBinding{Loan: loan, Lane: autoAUTOPYUSD.Lane, OriginOperationID: origin, AllocatedUSDCRaw: 100}
	allocation, _, _, err := bridgeExpectedEffects(Decision{Action: VoltrAllocateToSquads, AmountRaw: 100}, 500, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	allocation.Kind = "bridge"
	allocation.ReturnData = expectedAdaptorReturnData(100)
	allocated, err := reconcileTopupTranche(nil, binding, origin, VoltrAllocateToSquads, allocation, topupReceipt(t, allocation, 52, 400, 0, 100))
	if err != nil {
		t.Fatal(err)
	}
	// Round-trip the durable inventory between each leg, exactly as restart does.
	restart := func(s *topupTranche) *topupTranche {
		t.Helper()
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeTopupTranche(raw)
		if err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	allocated = restart(allocated)
	binding.Before = allocated
	if _, err := reconcileTopupTranche(allocated, binding, origin, VoltrAllocateToSquads, allocation, topupReceipt(t, allocation, 53, 400, 0, 100)); err == nil {
		t.Fatal("allocation replay could replenish the tranche")
	}
	route := autoAUTOPYUSD
	minimum := uint64(190)
	swap := ExpectedEffects{Schema: allocation.Schema, Kind: "cross-mint-swap", Accounts: []ExpectedAccountEffect{
		{Address: bridgeSquadsATA, Owner: bridgeTokenProgram, Mint: bridgeUSDC, Authority: bridgeVault, BeforeRaw: 100, AfterRaw: 0},
		{Address: route.CollateralCustody, Owner: route.CollateralTokenProgram, Mint: route.Kamino.CollateralMint, Authority: bridgeVault, BeforeRaw: 0, AfterRaw: minimum, MinimumAfterRaw: &minimum},
	}}
	collateral, err := reconcileTopupTranche(allocated, binding, swapID, SwapStableToCollateralStep, swap, topupReceipt(t, swap, 54, 0, 207))
	if err != nil {
		t.Fatal(err)
	}
	collateral = restart(collateral)
	if collateral.USDCRemainingRaw != 0 || collateral.CollateralRemainingRaw != 207 {
		t.Fatal("quote minimum replaced actual swap proceeds", collateral)
	}
	binding.Before = collateral
	deposit := ExpectedEffects{Schema: allocation.Schema, Kind: "kamino-deposit", Conserved: true, Deposit: &ExpectedDeposit{MinimumDebitRaw: 205, MaximumDebitRaw: 207}, Accounts: []ExpectedAccountEffect{
		{Address: route.CollateralCustody, Owner: route.CollateralTokenProgram, Mint: route.Kamino.CollateralMint, Authority: bridgeVault, BeforeRaw: 207, AfterRaw: 0},
		{Address: route.CollateralLiquiditySupply, Owner: route.CollateralTokenProgram, Mint: route.Kamino.CollateralMint, Authority: route.Kamino.MarketAuthority, BeforeRaw: 1000, AfterRaw: 1207},
	}}
	remaining, err := reconcileTopupTranche(collateral, binding, depositID, OpenRouteStep, deposit, topupReceipt(t, deposit, 55, 1, 1206))
	if err != nil {
		t.Fatal(err)
	}
	remaining = restart(remaining)
	if remaining.CollateralRemainingRaw != 1 || remaining.Stage != topupTrancheComplete || remaining.DepositQuantumRaw != 3 {
		t.Fatal("rounded deposit dropped owned residue", remaining)
	}
	for name, mutate := range map[string]func(*topupTrancheBinding, *ExpectedEffects, *ConfirmedTransactionEvidence){
		"foreign origin": func(b *topupTrancheBinding, _ *ExpectedEffects, _ *ConfirmedTransactionEvidence) {
			b.OriginOperationID = sha256Bytes([]byte("foreign"))
		},
		"borrowed residue": func(_ *topupTrancheBinding, e *ExpectedEffects, r *ConfirmedTransactionEvidence) {
			e.Accounts[0].BeforeRaw++
			r.PreTokenBalances[0].Raw++
		},
		"nonfinalized": func(_ *topupTrancheBinding, _ *ExpectedEffects, r *ConfirmedTransactionEvidence) { r.Finalized = false },
		"old receipt": func(_ *topupTrancheBinding, _ *ExpectedEffects, r *ConfirmedTransactionEvidence) {
			r.Slot = collateral.LastSlot - 1
		},
		"unexpected debt cash": func(_ *topupTrancheBinding, _ *ExpectedEffects, r *ConfirmedTransactionEvidence) {
			debt := TransactionTokenBalance{Address: route.DebtCustody, OwnerProgram: route.DebtTokenProgram, Mint: route.Kamino.DebtMint, Authority: bridgeVault}
			r.PreTokenBalances = append(r.PreTokenBalances, debt)
			debt.Raw = 1
			r.PostTokenBalances = append(r.PostTokenBalances, debt)
		},
		"borrow": func(_ *topupTrancheBinding, e *ExpectedEffects, _ *ConfirmedTransactionEvidence) {
			e.Kind = "kamino-borrow"
		},
		"repay": func(_ *topupTrancheBinding, e *ExpectedEffects, _ *ConfirmedTransactionEvidence) {
			e.Repayment = &ExpectedRepayment{MinimumDebitRaw: 1, MaximumDebitRaw: 207}
		},
		"mixed mint": func(_ *topupTrancheBinding, e *ExpectedEffects, r *ConfirmedTransactionEvidence) {
			e.Accounts[0].Mint = bridgeUSDC
			r.PreTokenBalances[0].Mint = bridgeUSDC
			r.PostTokenBalances[0].Mint = bridgeUSDC
		},
	} {
		b, e := binding, deposit
		e.Accounts = append([]ExpectedAccountEffect(nil), deposit.Accounts...)
		r := topupReceipt(t, e, 55, 1, 1206)
		mutate(&b, &e, &r)
		if _, err := reconcileTopupTranche(collateral, b, depositID, OpenRouteStep, e, r); err == nil {
			t.Fatal(name, "advanced tranche")
		}
	}
	// A later allocation carries only the exact prior owned rounding residue.
	nextOrigin := sha256Bytes([]byte("next allocation"))
	nextBinding := topupTrancheBinding{Loan: loan, Lane: route.Lane, OriginOperationID: nextOrigin, AllocatedUSDCRaw: 100, Before: remaining}
	next, err := reconcileTopupTranche(remaining, nextBinding, nextOrigin, VoltrAllocateToSquads, allocation, topupReceipt(t, allocation, 56, 400, 0, 100))
	if err != nil || next.CollateralRemainingRaw != 1 {
		t.Fatal("owned carry blocked or lost", err)
	}
	nextBinding.Before = next
	swap.Accounts[1].BeforeRaw = 1
	minimumWithCarry := minimum + 1
	swap.Accounts[1].AfterRaw, swap.Accounts[1].MinimumAfterRaw = minimumWithCarry, &minimumWithCarry
	nextSwap := sha256Bytes([]byte("next swap"))
	continued, err := reconcileTopupTranche(next, nextBinding, nextSwap, SwapStableToCollateralStep, swap, topupReceipt(t, swap, 57, 0, 208))
	if err != nil || continued.CollateralRemainingRaw != 208 {
		t.Fatal("receipt delta and owned carry not conserved", err)
	}
	swap.Accounts[1].BeforeRaw++
	if _, err := reconcileTopupTranche(next, nextBinding, nextSwap, SwapStableToCollateralStep, swap, topupReceipt(t, swap, 57, 0, 208)); err == nil {
		t.Fatal("foreign dust adopted as carry")
	}
	// An exact full debit completes ownership; a stale predecessor cannot replay it.
	complete, err := reconcileTopupTranche(collateral, binding, depositID, OpenRouteStep, deposit, topupReceipt(t, deposit, 55, 0, 1207))
	if err != nil || complete.Stage != topupTrancheComplete {
		t.Fatal("full deposit did not complete tranche", err)
	}
	if _, err := reconcileTopupTranche(complete, binding, depositID, OpenRouteStep, deposit, topupReceipt(t, deposit, 56, 0, 1207)); err == nil {
		t.Fatal("stale predecessor replayed deposit")
	}
}
