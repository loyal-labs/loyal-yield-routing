package watch

import (
	"context"
	"slices"
	"testing"
)

// These tests consume the registered Yield migrations. They never create,
// replace or drop relations, and acceptance exercises the full Load. The
// expected set is Rust's load_earn_subscription_targets over the Yield
// database alone (loyal-yield-store store.rs), which is what production ran.
func TestLoaderRegisteredYieldCompleteWatchCoverage(t *testing.T) {
	f := registeredWatchFixture(t)
	owner := f.identity()
	closed := f.identity()
	unregistered := f.identity()
	pending := f.identity()
	devnetOwner := f.identity()
	activePolicy, activeVault, _ := f.managed(owner, 1, true, "mainnet-beta", 200)
	setup := f.policy(owner, 1, true, "mainnet-beta", 180)
	if _, err := f.yield.Exec(f.ctx, `UPDATE loyal_yield.managed_vaults SET setup_policy_id=$2 WHERE vault_pubkey=$1`, activeVault, setup.id); err != nil {
		t.Fatal(err)
	}
	closedPolicy, closedVault, _ := f.managed(closed, 1, false, "mainnet-beta", 80)
	unregisteredPolicy, _, _ := f.managed(unregistered, 1, true, "mainnet-beta", 300)
	f.onboarding(pending)
	f.position(owner, activePolicy, activeVault)
	crossPolicy := f.cross(owner, true, "mainnet-beta")
	inactiveCross := f.cross(owner, false, "mainnet-beta")
	devnetCross := f.cross(devnetOwner, true, "devnet")
	activeATA := f.autodeposit(owner, true, "active", "mainnet-beta", nil, nil)
	delegation := f.key("delegation")
	pausedATA := f.autodeposit(f.identity(), false, "pending", "mainnet-beta", nil, &delegation)
	closedATA := f.autodeposit(f.identity(), false, "closed", "mainnet-beta", nil, nil)
	devnetATA := f.autodeposit(devnetOwner, true, "active", "devnet", nil, nil)
	max := f.earnMax()
	set, err := NewLoader(f.yield, "mainnet-beta").Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	// owner, closed, unregistered, pending, paused and the Earn MAX vault.
	if len(set.Vaults) != 6 {
		t.Fatalf("watched vaults = %d, want 6", len(set.Vaults))
	}
	for _, vault := range set.Vaults {
		if len(vault.Accounts) == 0 {
			t.Fatalf("vault %s watched without accounts", vault.Vault)
		}
	}
	if len(set.ATAs) != 1 || set.ATAs[activeATA.walletATA].ID != activeATA.id {
		t.Fatalf("enabled ATA coverage: %#v", set.ATAs)
	}
	for _, omitted := range []string{pausedATA.walletATA, closedATA.walletATA, devnetATA.walletATA} {
		if slices.Contains(set.Channels[BalanceSweepWalletATAs], omitted) {
			t.Fatal("nonenabled ATA entered pull channel")
		}
	}
	// No Apps identity gates the Yield rows: a managed vault without any Apps
	// account is watched exactly like one with it.
	for _, expected := range []Account{{owner.settings, "smart_account"}, {activeVault, "vault"}, {activePolicy.account, "policy"}, {setup.account, "policy"}, {crossPolicy, "policy"}, {unregisteredPolicy.account, "policy"}, {unregistered.settings, "smart_account"}, {pending.settings, "smart_account"}, {pausedATA.vault, "vault"}, {delegation, "recurring_delegation"}, {max.policy, "policy"}} {
		if !slices.Contains(set.Channels[ChannelForRole(expected.Role)], expected.Pubkey) {
			t.Fatalf("full Load omitted %s %s", expected.Role, expected.Pubkey)
		}
	}
	for _, omitted := range []string{closedPolicy.account, inactiveCross, devnetCross} {
		if slices.Contains(set.Channels[EarnPolicyAccounts], omitted) {
			t.Fatalf("closed/inactive/foreign policy still watched: %s", omitted)
		}
	}
	if slices.Contains(set.Channels[EarnSubscriptionAuthorities], delegation) {
		t.Fatal("nullable subscription shifted delegation role")
	}
	if len(set.AffectedVaults(closedVault)) != 1 || len(set.AffectedVaults(devnetOwner.settings)) != 0 {
		t.Fatal("closed mainnet identity lost or devnet-only identity leaked")
	}
	var sawMax, sawClosed bool
	for _, vault := range set.Vaults {
		if vault.Environment != "mainnet-beta" {
			t.Fatal("watch changed durable namespace")
		}
		if vault.Vault == max.vault {
			sawMax = vault.EarnMax && vault.VaultIndex == 0 && vault.ObservationStartSlot != nil && *vault.ObservationStartSlot == 400
		}
		if vault.Vault == closedVault {
			sawClosed = vault.ObservationStartSlot == nil
		}
	}
	if !sawMax || !sawClosed {
		t.Fatal("EarnMax replay floor or closed identity policy floor changed")
	}
	for channel, addresses := range set.Channels {
		for i := 1; i < len(addresses); i++ {
			if addresses[i-1] >= addresses[i] {
				t.Fatalf("channel %s not normalized", channel)
			}
		}
	}
}

func TestLoaderRegisteredCustodyAndReplayCorruptionHoldCoverage(t *testing.T) {
	for _, corruption := range []string{"wallet_ata", "negative_max_slot", "foreign_vault", "invalid_policy", "invalid_market"} {
		t.Run(corruption, func(t *testing.T) {
			f := registeredWatchFixture(t)
			owner := f.identity()
			policy, vault, _ := f.managed(owner, 1, true, "mainnet-beta", 200)
			var err error
			switch corruption {
			case "wallet_ata":
				target := f.autodeposit(owner, true, "active", "mainnet-beta", nil, nil)
				_, err = f.yield.Exec(f.ctx, `UPDATE loyal_yield.balance_sweep_targets SET wallet_token_ata=$2 WHERE id=$1`, target.id, f.key("foreign-ata"))
			case "negative_max_slot":
				max := f.earnMax()
				_, err = f.yield.Exec(f.ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{observedSlot}','-1') WHERE route_key=$1`, max.route)
			case "foreign_vault":
				_, err = f.yield.Exec(f.ctx, `UPDATE loyal_yield.managed_vaults SET vault_pubkey=$2 WHERE vault_pubkey=$1`, vault, f.key("foreign-vault"))
			case "invalid_policy":
				_, err = f.yield.Exec(f.ctx, `UPDATE loyal_yield.route_policies SET policy_account='invalid-base58!' WHERE id=$1`, policy.id)
			case "invalid_market":
				_, err = f.yield.Exec(f.ctx, `UPDATE loyal_yield.route_policies SET kamino_markets=ARRAY['invalid-base58!'] WHERE id=$1`, policy.id)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewLoader(f.yield, "mainnet-beta").Load(f.ctx); err == nil {
				t.Fatal("corrupt custody/coverage returned healthy watch set")
			}
		})
	}
}

func TestLoaderRegisteredCancelledReadHolds(t *testing.T) {
	f := registeredWatchFixture(t)
	f.managed(f.identity(), 1, true, "mainnet-beta", 200)
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := NewLoader(f.yield, "mainnet-beta").Load(ctx); err == nil {
		t.Fatal("cancelled read returned positive watch set")
	}
}

// Rust's managed-vault predicate is (vault.active AND active_policy.active)
// OR <cluster match>. An active vault with an active policy is watched with
// its policies whatever the policy's cluster label, so production's legacy
// cluster='unknown' rows stay covered; an inactive vault is watched (without
// policies) only inside the loader's cluster.
func TestLoaderRegisteredManagedVaultPredicateMatchesRust(t *testing.T) {
	f := registeredWatchFixture(t)
	legacy, legacyVault, _ := f.managed(f.identity(), 1, true, "unknown", 170)
	devnet, devnetVault, _ := f.managed(f.identity(), 1, true, "devnet", 170)
	inactiveLegacy, inactiveLegacyVault, _ := f.managed(f.identity(), 1, false, "unknown", 170)
	inactiveMainnet, inactiveMainnetVault, _ := f.managed(f.identity(), 1, false, "mainnet", 170)
	inactiveDevnet, inactiveDevnetVault, _ := f.managed(f.identity(), 1, false, "devnet", 170)
	set, err := NewLoader(f.yield, "mainnet-beta").Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, active := range []struct{ policy, vault string }{{legacy.account, legacyVault}, {devnet.account, devnetVault}} {
		if !slices.Contains(set.Channels[EarnPolicyAccounts], active.policy) || len(set.AffectedVaults(active.vault)) != 1 {
			t.Fatalf("active vault %s with active policy not watched", active.vault)
		}
	}
	if len(set.AffectedVaults(inactiveMainnetVault)) != 1 || slices.Contains(set.Channels[EarnPolicyAccounts], inactiveMainnet.account) {
		t.Fatal("inactive mainnet vault must stay watched without its closed policy")
	}
	for _, foreign := range []struct{ policy, vault string }{{inactiveLegacy.account, inactiveLegacyVault}, {inactiveDevnet.account, inactiveDevnetVault}} {
		if len(set.AffectedVaults(foreign.vault)) != 0 || slices.Contains(set.Channels[EarnPolicyAccounts], foreign.policy) {
			t.Fatalf("inactive vault %s outside the cluster was watched", foreign.vault)
		}
	}
	devnetSet, err := NewLoader(f.yield, "devnet").Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(devnetSet.AffectedVaults(inactiveDevnetVault)) != 1 || len(devnetSet.AffectedVaults(inactiveMainnetVault)) != 0 || len(devnetSet.AffectedVaults(legacyVault)) != 1 {
		t.Fatal("devnet loader did not apply the same predicate")
	}
}
