package fleet

import (
	"strings"
	"testing"
	"time"
)

// testSnapshot holds each vault's collateral 1:1 in one reserve under a
// route policy that delegates exclusively to signer.
func testSnapshot(slot int64, reserves []ReserveState, signer string, holdings map[*RegisteredVault]ReserveIdentity, amount uint64) Snapshot {
	s := Snapshot{Slot: slot, ObservedAt: time.Now().UTC(), Reserves: map[string]ReserveState{}, Vaults: map[string]VaultFacts{}}
	for _, r := range reserves {
		s.Reserves[r.Address] = r
	}
	for v, reserve := range holdings {
		s.Vaults[v.VaultPubkey] = VaultFacts{Policy: &DecodedSquadsPolicy{Settings: v.Settings, AccountIndex: uint8(v.VaultIndex), DelegatedSigners: []string{signer}},
			Holdings: []Holding{{Reserve: reserve.Address, Market: reserve.Market, Mint: reserve.Mint, CollateralRaw: amount, LiquidityRaw: amount}}}
	}
	return s
}

func testReserve(identity ReserveIdentity, slot, apy int64) ReserveState {
	ample, limit := int64(2_000_000_000_000_000), uint64(1<<62)
	return ReserveState{ReserveIdentity: identity, Slot: slot, LastUpdateSlot: slot, SupplyAPYBPS: apy, TotalSupplyRaw: 2_000_000_000_000_000, EconomicLifetimeMillis: 600_000, DataHash: strings.Repeat("a", 64), AvailableLiquidityUSDMicros: &ample, DepositLimitRaw: &limit}
}

// A policy on chain that no longer delegates exclusively to the signer plans
// nothing for that vault, and never fails another vault's plan.
func TestPlanningVaultsNeedTheLivePolicyOfEachVault(t *testing.T) {
	source := ReserveIdentity{Address: "source", Market: "market", Mint: USDCMint}
	target := ReserveIdentity{Address: "target", Market: "market", Mint: USDCMint}
	healthy := RegisteredVault{VaultID: 1, Settings: "settings-1", VaultPubkey: "vault-1", Markets: []string{"market"}, StableMints: []string{USDCMint}, KaminoMints: []string{USDCMint}}
	foreign := RegisteredVault{VaultID: 2, Settings: "settings-2", VaultPubkey: "vault-2", Markets: []string{"market"}, StableMints: []string{USDCMint}, KaminoMints: []string{USDCMint}}
	absent := RegisteredVault{VaultID: 3, Settings: "settings-3", VaultPubkey: "vault-3", Markets: []string{"market"}, StableMints: []string{USDCMint}, KaminoMints: []string{USDCMint}}
	snapshot := testSnapshot(1000, []ReserveState{testReserve(source, 1000, 100), testReserve(target, 1000, 900)}, "signer", map[*RegisteredVault]ReserveIdentity{&healthy: source, &foreign: source, &absent: source}, 1_000_000_000_000)
	snapshot.Vaults["vault-2"].Policy.DelegatedSigners = []string{"someone-else"}
	facts := snapshot.Vaults["vault-3"]
	facts.Policy, facts.PolicyErr = nil, "policy account is absent or not owned by Squads"
	snapshot.Vaults["vault-3"] = facts
	vaults, err := PlanningVaults(snapshot, []ReserveIdentity{source, target}, []RegisteredVault{healthy, foreign, absent}, nil, nil, FleetLoadOptions{DelegatedSigner: "signer"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanFleet(snapshot.Market("policies"), vaults)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Opportunities) != 1 || plan.Opportunities[0].Decision.VaultID != 1 || plan.Rejections[2] != "route_policy_not_delegated" || plan.Rejections[3] != "route_policy_unreadable" {
		t.Fatalf("policy facts: moves=%+v rejections=%v", plan.Opportunities, plan.Rejections)
	}
}
