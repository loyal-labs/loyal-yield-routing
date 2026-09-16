package backyardrwa

import (
	"encoding/binary"
	"encoding/json"
	"testing"
)

func repaymentEffectsFixture(t *testing.T) (KaminoPrimeUSDCRequest, ExpectedEffects, ConfirmedTransactionEvidence) {
	t.Helper()
	route := ethenaUSDePYUSD
	source, destination := kaminoLegCustodiesForRoute(kaminoLegRepay, route)
	accounts := []ConfirmedAccount{}
	for i, boundary := range []kaminoCustodyBoundary{source, destination} {
		data := make([]byte, 165)
		putKey(t, data[:32], boundary.Mint)
		putKey(t, data[32:64], boundary.Authority)
		binary.LittleEndian.PutUint64(data[64:72], uint64(3_000+i*4_000))
		data[108] = 1
		accounts = append(accounts, ConfirmedAccount{Address: boundary.Address, Owner: route.DebtTokenProgram, Lamports: 1, Data: data})
	}
	effects, err := boundedKaminoRepaymentEffects(accounts, source, destination, 1_000, 1_010)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	request, err := manifest.kaminoPacketForRoute(DeleverRouteStep, kaminoLegRepay, 1_010, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	receipt := ConfirmedTransactionEvidence{Signature: "controlled-repayment-not-mainnet", Slot: 77}
	for _, account := range effects.Accounts {
		balance := TransactionTokenBalance{Address: account.Address, OwnerProgram: account.Owner, Mint: account.Mint, Authority: account.Authority, Raw: account.BeforeRaw}
		receipt.PreTokenBalances = append(receipt.PreTokenBalances, balance)
		receipt.PostTokenBalances = append(receipt.PostTokenBalances, balance)
	}
	return request, effects, receipt
}

func TestBoundedKaminoRepaymentReconcilesActualDebitWithoutClaimingPayoff(t *testing.T) {
	_, effects, receipt := repaymentEffectsFixture(t)
	for _, amount := range []uint64{1_000, 1_005, 1_010, 0, 999, 1_011} {
		receipt.PostTokenBalances[0].Raw = receipt.PreTokenBalances[0].Raw - amount
		receipt.PostTokenBalances[1].Raw = receipt.PreTokenBalances[1].Raw + amount
		_, body, err := ReconcileConfirmedTransaction(effects, receipt)
		if want := amount >= 1_000 && amount <= 1_010; (err == nil) != want {
			t.Fatalf("amount=%d err=%v", amount, err)
		}
		if err == nil {
			var evidence map[string]any
			if err := json.Unmarshal(body, &evidence); err != nil || evidence["source"] != "confirmed-transaction-meta" || evidence["debtPaidOff"] != nil {
				t.Fatal("token reconciliation must not claim an obligation payoff", string(body), err)
			}
		}
	}
	receipt.PostTokenBalances[0].Raw = 2_000
	receipt.PostTokenBalances[1].Raw = 8_001 // individually in bounds, but not conserved
	if _, _, err := ReconcileConfirmedTransaction(effects, receipt); err == nil {
		t.Fatal("unequal in-range debit and credit accepted")
	}
	receipt.PostTokenBalances[1].Raw = 8_000
	for _, mutate := range []func(*ConfirmedTransactionEvidence){
		func(r *ConfirmedTransactionEvidence) { r.PostTokenBalances[1].Mint = bridgeUSDC },
		func(r *ConfirmedTransactionEvidence) { r.PostTokenBalances[0].Authority = bridgeDelegate },
		func(r *ConfirmedTransactionEvidence) { r.PreTokenBalances[0].Raw++ },
		func(r *ConfirmedTransactionEvidence) { r.PostTokenBalances[0].OwnerProgram = classicTokenProgram },
		func(r *ConfirmedTransactionEvidence) {
			r.PostTokenBalances[0].Raw = 4_000
			r.PostTokenBalances[1].Raw = 6_000
		},
	} {
		bad := receipt
		bad.PreTokenBalances = append([]TransactionTokenBalance(nil), receipt.PreTokenBalances...)
		bad.PostTokenBalances = append([]TransactionTokenBalance(nil), receipt.PostTokenBalances...)
		mutate(&bad)
		if _, _, err := ReconcileConfirmedTransaction(effects, bad); err == nil {
			t.Fatal("repayment identity, precondition or direction drift accepted")
		}
	}
}

func TestBoundedKaminoRepaymentReservesWireMaximumAndRejectsWeakenedEffects(t *testing.T) {
	request, effects, _ := repaymentEffectsFixture(t)
	debit, err := MeasureExecutableDebit(request, effects)
	if err != nil || debit.Raw != 1_010 || debit.Mint != ethenaUSDePYUSD.Kamino.DebtMint {
		t.Fatal("priced the observed debt instead of the finite executable limit", debit, err)
	}
	encodedEffects, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		t.Fatal(err)
	}
	input, err := encodePhase3BuildInput(request, encodedEffects)
	if err != nil {
		t.Fatal(err)
	}
	// The persisted final-send input must preserve the same bound and token
	// program; it cannot reprice only the smaller expected debt on restart.
	encodedInput, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var restored phase3BuildInput
	if err := json.Unmarshal(encodedInput, &restored); err != nil {
		t.Fatal(err)
	}
	persistedRequest, persistedEffects, _, err := restored.decode()
	if err != nil {
		t.Fatal(err)
	}
	persistedDebit, err := MeasureExecutableDebit(persistedRequest, persistedEffects)
	if err != nil || persistedDebit != debit {
		t.Fatal("persisted repayment repricing lost the maximum or custody identity", persistedDebit, err)
	}
	// Non-pegged valuation and network fees remain part of the gross cap. A
	// 1000-unit predicted transfer fits; the executable 1010-unit limit does not.
	priced, err := ValueDebitMicros(debit.Raw, 6, 990_000_000)
	if err != nil {
		t.Fatal(err)
	}
	reservation := testReservation()
	reservation.UpperMicros = priced + 1_000
	budget := emptyTestBudget()
	assertBudgetHold(t, budget.Admit(reservation), "transaction_cap_exceeded")
	for _, mutate := range []func(*ExpectedEffects){
		func(e *ExpectedEffects) { e.Repayment.MinimumDebitRaw = 0 },
		func(e *ExpectedEffects) { e.Repayment.MinimumDebitRaw = 1_011 },
		func(e *ExpectedEffects) { e.Repayment.MaximumDebitRaw = ^uint64(0) },
		func(e *ExpectedEffects) { e.Repayment.MaximumDebitRaw = 1_000 },
		func(e *ExpectedEffects) { e.Accounts[0].AfterRaw++ },
		func(e *ExpectedEffects) { e.Accounts[1].AfterRaw-- },
		func(e *ExpectedEffects) { e.Accounts[0], e.Accounts[1] = e.Accounts[1], e.Accounts[0] },
		func(e *ExpectedEffects) { e.Accounts[1].Authority = bridgeDelegate },
		func(e *ExpectedEffects) { e.Accounts[1].Address = bridgeSquadsATA },
		func(e *ExpectedEffects) { e.Accounts[1].Mint = bridgeUSDC },
		func(e *ExpectedEffects) {
			e.Accounts[0].Owner = classicTokenProgram
			e.Accounts[1].Owner = classicTokenProgram
		},
		func(e *ExpectedEffects) { e.Accounts[0].MinimumAfterRaw = &e.Accounts[0].AfterRaw },
		func(e *ExpectedEffects) { e.Conserved = false },
		func(e *ExpectedEffects) { e.Kind = "bridge" },
		func(e *ExpectedEffects) { e.Repayment = nil },
	} {
		body, _ := json.Marshal(effects)
		var bad ExpectedEffects
		if err := json.Unmarshal(body, &bad); err != nil {
			t.Fatal(err)
		}
		mutate(&bad)
		if _, err := MeasureExecutableDebit(request, bad); err == nil {
			t.Fatal("weakened repayment effects accepted", bad)
		}
	}
	other := bridgeTestRequest(StageSquadsToVoltr, 1_010)
	if _, err := MeasureExecutableDebit(other, effects); err == nil {
		t.Fatal("repayment bounds accepted on a bridge request")
	}
	request.AmountRaw = 1_000
	request.Data = append([]byte(nil), request.Data...)
	binary.LittleEndian.PutUint64(request.Data[8:], request.AmountRaw)
	if _, err := MeasureExecutableDebit(request, effects); err == nil {
		t.Fatal("repayment wire differs from reserved maximum")
	}
	leg, wire, minimum, err := selectKaminoLeg(false, Decision{Action: DeleverRouteStep, AmountRaw: 1_010}, KaminoPosition{DebtRaw: 1_000})
	if err != nil || leg != kaminoLegRepay || wire != 1_010 || minimum != 1_000 {
		t.Fatal("finite decision limit or refreshed-debt floor lost", wire, minimum, err)
	}
}
