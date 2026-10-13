package backyard

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
	request, err := manifest.kaminoPacketForRoute(testPolicies(t), DeleverRouteStep, kaminoLegRepay, 1_010, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
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
	t.Parallel()
	_, effects, receipt := repaymentEffectsFixture(t)
	for _, amount := range []uint64{1_000, 1_005, 1_010, 0, 999, 1_011} {
		receipt.PostTokenBalances[0].Raw = receipt.PreTokenBalances[0].Raw - amount
		receipt.PostTokenBalances[1].Raw = receipt.PreTokenBalances[1].Raw + amount
		_, body, err := ReconcileConfirmedTransaction(rwa, effects, receipt)
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
	if _, _, err := ReconcileConfirmedTransaction(rwa, effects, receipt); err == nil {
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
		if _, _, err := ReconcileConfirmedTransaction(rwa, effects, bad); err == nil {
			t.Fatal("repayment identity, precondition or direction drift accepted")
		}
	}
}
