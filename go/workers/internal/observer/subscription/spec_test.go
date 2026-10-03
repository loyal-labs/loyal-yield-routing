package subscription

import "testing"

func TestBuildUsesOneCombinedConfirmedRequest(t *testing.T) {
	request, err := Build(Spec{
		FromSlot: 42,
		Accounts: map[string]AccountFilter{
			KaminoReserves: {
				Addresses: []string{"reserve-b", "reserve-a", "reserve-a"},
			},
			BalanceSweepWalletATAs: {
				Addresses:           []string{"ata-a"},
				RequireTxnSignature: true,
			},
			"earn_vault_accounts": {
				Addresses:           []string{"vault-a"},
				RequireTxnSignature: true,
			},
		},
	})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if request.GetFromSlot() != 42 {
		t.Fatalf("from_slot = %d, want 42", request.GetFromSlot())
	}
	if len(request.Accounts) != 3 || len(request.Transactions) != 1 || len(request.Slots) != 1 {
		t.Fatalf("request was not combined: accounts=%d transactions=%d slots=%d", len(request.Accounts), len(request.Transactions), len(request.Slots))
	}
	if got := request.Accounts[KaminoReserves].Account; len(got) != 2 || got[0] != "reserve-a" || got[1] != "reserve-b" {
		t.Fatalf("sorted compact reserve addresses = %v", got)
	}
	if request.Accounts[KaminoReserves].GetNonemptyTxnSignature() {
		t.Fatal("Kamino reserves unexpectedly require transaction signatures")
	}
	if !request.Accounts[BalanceSweepWalletATAs].GetNonemptyTxnSignature() {
		t.Fatal("balance ATAs must require transaction signatures")
	}
	if request.Transactions[EarnMaxPolicyTransactions].AccountInclude[0] != SquadsSmartAccountProgramID {
		t.Fatal("Earn MAX policy filter does not target the Squads smart-account program")
	}
}

func TestBuildRejectsAccidentalAllAccountsFilter(t *testing.T) {
	_, err := Build(Spec{
		FromSlot: 42,
		Accounts: map[string]AccountFilter{
			KaminoReserves:         {Addresses: []string{"reserve"}},
			BalanceSweepWalletATAs: {},
		},
	})
	if err == nil {
		t.Fatal("empty exact-account filter was accepted")
	}
}
