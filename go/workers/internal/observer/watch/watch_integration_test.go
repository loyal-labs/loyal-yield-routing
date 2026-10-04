package watch

import (
	"context"
	"slices"
	"testing"
)

// These tests consume registered Yield and pinned Apps migrations. They never
// create, replace or drop relations, and acceptance exercises the full Load.
func TestLoaderRegisteredAppsAndYieldCompleteWatchCoverage(t *testing.T) {
	f := registeredWatchFixture(t)
	ready := f.app("ready", "mainnet")
	pending := f.app("provisioning", "mainnet")
	closed := f.app("failed", "mainnet")
	foreign := f.app("ready", "devnet")
	activePolicy, activeVault, _ := f.managed(ready, 1, true, "mainnet-beta", 200)
	setup := f.policy(ready, 1, true, "mainnet-beta", 180)
	if _, err := f.yield.Exec(f.ctx, `UPDATE loyal_yield.managed_vaults SET setup_policy_id=$2 WHERE vault_pubkey=$1`, activeVault, setup.id); err != nil {
		t.Fatal(err)
	}
	closedPolicy, closedVault, _ := f.managed(closed, 1, false, "mainnet-beta", 80)
	f.managed(foreign, 1, true, "devnet", 300)
	f.managed(f.identity(), 1, true, "mainnet-beta", 300)
	f.onboarding(pending)
	f.position(ready, activePolicy, activeVault)
	crossPolicy := f.cross(ready, true, "mainnet-beta")
	inactiveCross := f.cross(ready, false, "mainnet-beta")
	foreignCross := f.cross(foreign, true, "devnet")
	activeATA := f.autodeposit(ready, true, "active", "mainnet-beta", nil, nil)
	delegation := f.key("delegation")
	pausedATA := f.autodeposit(f.identity(), false, "pending", "mainnet-beta", nil, &delegation)
	closedATA := f.autodeposit(f.identity(), false, "closed", "mainnet-beta", nil, nil)
	foreignATA := f.autodeposit(foreign, true, "active", "devnet", nil, nil)
	max := f.earnMax()
	set, err := NewLoaderWithApps(f.yield, f.apps, "mainnet-beta").Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Vaults) != 5 || len(set.Channels[EarnIdleTokenAccounts]) != 30 || len(set.Channels[EarnWalletTokenAccounts]) != 24 || len(set.Channels[EarnObligations]) != 25 {
		t.Fatalf("full watch role coverage changed: vaults=%d idle=%d wallet_tokens=%d obligations=%d", len(set.Vaults), len(set.Channels[EarnIdleTokenAccounts]), len(set.Channels[EarnWalletTokenAccounts]), len(set.Channels[EarnObligations]))
	}
	if len(set.ATAs) != 1 || set.ATAs[activeATA.walletATA].ID != activeATA.id {
		t.Fatalf("enabled ATA coverage: %#v", set.ATAs)
	}
	for _, omitted := range []string{pausedATA.walletATA, closedATA.walletATA, foreignATA.walletATA} {
		if slices.Contains(set.Channels[BalanceSweepWalletATAs], omitted) {
			t.Fatal("nonenabled ATA entered pull channel")
		}
	}
	for _, expected := range []Account{{ready.settings, "smart_account"}, {activeVault, "vault"}, {activePolicy.account, "policy"}, {setup.account, "policy"}, {crossPolicy, "policy"}, {pausedATA.vault, "vault"}, {delegation, "recurring_delegation"}, {max.policy, "policy"}} {
		if !slices.Contains(set.Channels[ChannelForRole(expected.Role)], expected.Pubkey) {
			t.Fatalf("full Load omitted %s %s", expected.Role, expected.Pubkey)
		}
	}
	for _, omitted := range []string{closedPolicy.account, inactiveCross, foreignCross} {
		if slices.Contains(set.Channels[EarnPolicyAccounts], omitted) {
			t.Fatalf("closed/inactive/foreign policy still watched: %s", omitted)
		}
	}
	if slices.Contains(set.Channels[EarnSubscriptionAuthorities], delegation) {
		t.Fatal("nullable subscription shifted delegation role")
	}
	if len(set.AffectedVaults(closedVault)) != 1 || len(set.AffectedVaults(foreign.settings)) != 0 {
		t.Fatal("closed identity lost or foreign Apps environment leaked")
	}
	var sawMax, sawClosed bool
	for _, vault := range set.Vaults {
		if vault.Environment != "mainnet-beta" {
			t.Fatal("Apps spelling changed durable namespace")
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
	if _, err := NewLoaderWithApps(f.yield, f.yield, "mainnet-beta").Load(f.ctx); err == nil {
		t.Fatal("wrong Apps pool declared complete watch coverage")
	}
}

func TestLoaderRegisteredCustodyAndReplayCorruptionHoldCoverage(t *testing.T) {
	for _, corruption := range []string{"wallet_ata", "negative_max_slot", "foreign_vault", "invalid_policy", "invalid_market"} {
		t.Run(corruption, func(t *testing.T) {
			f := registeredWatchFixture(t)
			owner := f.app("ready", "mainnet")
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
			if _, err := NewLoaderWithApps(f.yield, f.apps, "mainnet-beta").Load(f.ctx); err == nil {
				t.Fatal("corrupt custody/coverage returned healthy watch set")
			}
		})
	}
}

func TestLoaderRegisteredConfiguredMissingAppsAndCancelledReadHold(t *testing.T) {
	f := registeredWatchFixture(t)
	if _, err := NewLoaderWithApps(f.yield, nil, "mainnet-beta").Load(f.ctx); err == nil {
		t.Fatal("configured Apps capability fell back to Yield")
	}
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := NewLoaderWithApps(f.yield, f.apps, "mainnet-beta").Load(ctx); err == nil {
		t.Fatal("cancelled read returned positive watch set")
	}
}

func TestLoaderRegisteredLegacyUnknownClusterNeedsActualAppsIdentity(t *testing.T) {
	f := registeredWatchFixture(t)
	owner := f.app("ready", "mainnet")
	legacy, _, _ := f.managed(owner, 1, true, "unknown", 170)
	unowned, _, _ := f.managed(f.identity(), 1, true, "unknown", 170)
	foreign, foreignVault, _ := f.managed(owner, 2, true, "devnet", 170)
	devnetOwner := f.app("ready", "devnet")
	devnetUnknown, _, _ := f.managed(devnetOwner, 1, true, "unknown", 170)
	set, err := NewLoaderWithApps(f.yield, f.apps, "mainnet-beta").Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(set.Channels[EarnPolicyAccounts], legacy.account) || slices.Contains(set.Channels[EarnPolicyAccounts], unowned.account) || slices.Contains(set.Channels[EarnPolicyAccounts], foreign.account) || len(set.AffectedVaults(foreignVault)) != 0 {
		t.Fatal("legacy observation coverage escaped current Apps ownership or admitted known foreign cluster")
	}
	compatibility, err := NewLoader(f.yield, "mainnet-beta").Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(compatibility.Channels[EarnPolicyAccounts], legacy.account) {
		t.Fatal("compatibility constructor inferred legacy namespace without configured Apps ownership")
	}
	devnet, err := NewLoaderWithApps(f.yield, f.apps, "devnet").Load(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(devnet.Channels[EarnPolicyAccounts], devnetUnknown.account) {
		t.Fatal("mainnet legacy recovery exception was inferred for another namespace")
	}
}
