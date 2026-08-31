package watch

import "testing"

func TestRetainPreviousEarnBindingsKeepsRemovedAccountsAndVaults(t *testing.T) {
	oldAnchor := uint64(100)
	previous := &Set{
		ATAs: make(map[string]ATATarget),
		Vaults: []Vault{
			{Environment: "mainnet-beta", Settings: "settings-a", Vault: "vault-a", VaultIndex: 1, Accounts: []Account{{Pubkey: "existing", Role: "policy"}, {Pubkey: "removed", Role: "recurring_delegation"}}},
			{Environment: "mainnet-beta", Settings: "settings-b", Vault: "vault-b", VaultIndex: 1, Accounts: []Account{{Pubkey: "removed-vault-account", Role: "policy"}}},
		},
		BindingStartSlots: make(map[string]uint64),
	}
	for _, vault := range previous.Vaults {
		for _, account := range vault.Accounts {
			previous.recordBindingStart(vault, account, oldAnchor)
		}
	}
	previous.rebuildChannels()

	next := &Set{
		ATAs:              make(map[string]ATATarget),
		Vaults:            []Vault{{Environment: "mainnet-beta", Settings: "settings-a", Vault: "vault-a", VaultIndex: 1, Accounts: []Account{{Pubkey: "existing", Role: "policy"}, {Pubkey: "added", Role: "subscription_authority"}}}},
		BindingStartSlots: make(map[string]uint64),
	}
	if err := next.AnchorNewEarnBindings(previous, 900); err != nil {
		t.Fatal(err)
	}
	start, err := next.NewEarnBindingStart(previous)
	if err != nil {
		t.Fatal(err)
	}
	if start == nil || *start != 900 {
		t.Fatalf("new binding start = %v, want 900", start)
	}
	if err := next.RetainPreviousEarnBindings(previous); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"existing", "removed", "added", "removed-vault-account"} {
		if len(next.AffectedVaults(account)) != 1 {
			t.Fatalf("retained set does not route %q: %#v", account, next.Vaults)
		}
	}
	if len(next.Channels[EarnRecurringDelegations]) != 1 || next.Channels[EarnRecurringDelegations][0] != "removed" {
		t.Fatalf("candidate subscription dropped removed binding: %#v", next.Channels)
	}
	start, err = next.NewEarnBindingStart(previous)
	if err != nil {
		t.Fatal(err)
	}
	if start == nil || *start != 900 {
		t.Fatalf("retained set new binding start = %v, want 900", start)
	}
}

func TestNewEarnBindingRequiresReplayAnchor(t *testing.T) {
	next := &Set{Vaults: []Vault{{Environment: "mainnet-beta", Vault: "vault", Accounts: []Account{{Pubkey: "new", Role: "policy"}}}}}
	if err := next.AnchorNewEarnBindings(nil, 0); err == nil {
		t.Fatal("new binding without fallback anchor was accepted")
	}
	if _, err := next.NewEarnBindingStart(nil); err == nil {
		t.Fatal("new binding without recorded anchor was accepted")
	}
}
