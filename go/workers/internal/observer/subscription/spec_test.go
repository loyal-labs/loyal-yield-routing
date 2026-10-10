package subscription

import (
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/autodeposit"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

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
	// Kamino reserve accounts change inside transactions. Rust leaves the
	// signature filter unset; an explicit false filters every such update out.
	if request.Accounts[KaminoReserves].NonemptyTxnSignature != nil {
		t.Fatalf("Kamino reserve signature filter = %v, want unset", *request.Accounts[KaminoReserves].NonemptyTxnSignature)
	}
	for _, label := range []string{BalanceSweepWalletATAs, "earn_vault_accounts"} {
		if value := request.Accounts[label].NonemptyTxnSignature; value == nil || !*value {
			t.Fatalf("%s must require transaction signatures, got %v", label, value)
		}
	}
	if include := request.Transactions[EarnMaxPolicyTransactions].AccountInclude; len(include) != 2 || include[0] != squads.ProgramID.String() || include[1] != autodeposit.SubscriptionsProgramID {
		t.Fatalf("policy transaction filter = %v, want the Squads and Subscriptions programs", include)
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
