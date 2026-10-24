package webapp

import (
	"testing"

	"github.com/solana-foundation/solana-go/v2"
)

func TestRefundScanJudgesPolicyByItsOwnVault(t *testing.T) {
	address := "5nAGZKeGW7HhFhDedgqgDBrjEgjTZjWP3otwPuh6ujZZ"
	index := uint8(0)
	policy := txreadsPolicySeed{address: solana.MustPublicKeyFromBase58(address), seed: 7, state: "ProgramInteraction", accountIndex: &index}
	vaults := map[uint8]txreadsVaultFacts{
		0: {
			holdsChainFunds: true,
			db: txreadsRefundDBState{
				activeManagedVaultAccounts: map[string]bool{address: true},
				routePolicyAccounts:        map[string]bool{address: true},
			},
		},
		1: {},
	}

	got := txreadsScanPolicy(policy, vaults)

	if got.CanRefund || got.BlockedReason == nil || *got.BlockedReason != "Vault still holds funds on chain" {
		t.Fatalf("canRefund=%v blockedReason=%v, want blocked by vault 0 funds", got.CanRefund, got.BlockedReason)
	}
	if !got.ActiveManagedVault || !got.DBPresent {
		t.Fatalf("activeManagedVault=%v dbPresent=%v, want both true from vault 0", got.ActiveManagedVault, got.DBPresent)
	}
}
