package backyard

import (
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
)

// Vault is one Voltr vault Backyard manages: data, nothing else. Its book is
// its own Squads vault (Settings' SquadsIndex), so two vaults' NAVs never
// overlap; every other address is derived from these fields (newVault).
type Vault struct {
	Name string
	// VoltrVault is the Voltr vault, Strategy its one strategy and Adaptor
	// the program Voltr calls for it.
	VoltrVault, Strategy, Adaptor string
	SquadsIndex                   uint8
	// RouteKey names the vault's route row; rwa keeps its original key.
	RouteKey string
	// AdminPerformanceFeeBPS is the vault's approved Voltr admin performance
	// fee. M7 holds unless the chain's term is exactly this; every other
	// Voltr fee term is zero.
	AdminPerformanceFeeBPS int64

	// Derived: the Squads vault that manages it and its USDC account (the
	// vault's cash), and Voltr's accounts of the vault and its strategy.
	squads, cash                               string
	protocol, idleAuth, idleATA, lpMint        string
	strategyAuth, strategyATA, strategyReceipt string
	adaptorReceipt                             string
	// A Trustful vault's withdrawal is paid from its holding authority's
	// USDC account.
	holdingAuth, holding string
	routes               []RuntimeRoute
	manifest             RouteManifest
}

// The rwa vault: Backyard's first vault, on its own FSj27 report adaptor.
var rwa = newVault(Vault{Name: "rwa", VoltrVault: "HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA",
	Strategy: "DCpR24Eb6xCWxDyaZvCBTkadkxCB2vkqJN1EfYNWtLxY", Adaptor: fsj27Program, SquadsIndex: 0,
	RouteKey: "rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh", AdminPerformanceFeeBPS: 0})

// The looping vault, on Voltr's Trustful adaptor: Squads vault index 2 is
// its book.
var looping = newVault(Vault{Name: "looping", VoltrVault: "GrHZD15bMiUwrVPsmBwz8GRoQeDZN6wNEQVdW8k8SxV1",
	Strategy: "GUCvzfn1hqThWp5uQNF7CqoxC1XckCPTjsRGKVXHPxu1", Adaptor: voltr.TrustfulProgramID.String(), SquadsIndex: 2,
	RouteKey: "backyard:6HTfVjwk5FfW4GH67hmEen7VHeURdB5EJj6E429qTrUY", AdminPerformanceFeeBPS: 0})

// vaults are the vaults the engine runs, a Worker each.
var vaults = []*Vault{rwa, looping}

// fsj27Program is the rwa vault's report adaptor. Its strategy-two config's
// key is the Voltr strategy key, derived offline from the setup admin over
// domain loyal-rwa-multiply-mainnet-v3; it replaces the retired v2 config
// 9hDH4acTDrSjg9d5n8c1g53jMTonaDAUesp1diCWuuhj.
const fsj27Program = "FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW"

// findVault is the vault named name.
func findVault(name string) (*Vault, error) {
	for _, v := range vaults {
		if v.Name == name {
			return v, nil
		}
	}
	names := make([]string, len(vaults))
	for i, v := range vaults {
		names[i] = v.Name
	}
	return nil, fmt.Errorf("no Backyard vault %q; there are %q", name, names)
}

// newVault derives v's addresses: the Squads vault PDA and its USDC ATA,
// Voltr's PDAs of the vault and strategy and their USDC ATAs, and every lane's
// accounts owned by the Squads vault (earnLane.routeFor).
func newVault(v Vault) *Vault {
	key := func(value string) solana.PublicKey { return solana.PublicKey(mustKey(value)) }
	must := func(address solana.PublicKey, err error) string {
		if err != nil {
			panic("Backyard vault " + v.Name + ": " + err.Error())
		}
		return address.String()
	}
	pda := func(seeds ...[]byte) string {
		address, _, err := solana.FindProgramAddress(seeds, voltr.ProgramID)
		return must(address, err)
	}
	usdcATA := func(owner string) string {
		return must(spl.AssociatedTokenAddress(key(owner), key(bridgeUSDC), solana.TokenProgramID))
	}
	squadsVault, _, err := squads.SmartAccountAddress(key(bridgeSettings), v.SquadsIndex)
	v.squads = must(squadsVault, err)
	v.cash = usdcATA(v.squads)
	vault, strategy := key(v.VoltrVault), key(v.Strategy)
	v.protocol = pda([]byte("protocol"))
	v.idleAuth = pda([]byte("vault_asset_idle_auth"), vault[:])
	v.idleATA = usdcATA(v.idleAuth)
	v.lpMint = pda([]byte("vault_lp_mint"), vault[:])
	v.strategyAuth = pda([]byte("vault_strategy_auth"), vault[:], strategy[:])
	v.strategyATA = usdcATA(v.strategyAuth)
	v.strategyReceipt = pda([]byte("strategy_init_receipt"), vault[:], strategy[:])
	v.adaptorReceipt = pda([]byte("adaptor_add_receipt"), vault[:], key(v.Adaptor).Bytes())
	if v.trustful() {
		v.holdingAuth = must(voltr.TrustfulHoldingAuthority(key(v.strategyAuth), strategy))
		v.holding = usdcATA(v.holdingAuth)
	}
	for _, l := range earnLanes {
		v.routes = append(v.routes, l.routeFor(squadsVault))
	}
	manifest, err := manifestOf(&v)
	if err != nil {
		panic("Backyard vault " + v.Name + ": " + err.Error())
	}
	v.manifest = manifest
	return &v
}

// trustful reports a vault on Voltr's Trustful adaptor: its NAV is the
// strategy receipt, reported by deposit_strategy's end value, and a
// withdrawal is one transaction through the holding account. Every other
// vault is on the FSj27 report adaptor (report_ticket.go).
func (v *Vault) trustful() bool { return v.Adaptor == voltr.TrustfulProgramID.String() }

// route is lane's chain facts in v: the lane's template with the accounts v's
// Squads vault owns.
func (v *Vault) route(lane string) (RuntimeRoute, error) {
	for _, route := range v.routes {
		if route.Lane == lane {
			return route, nil
		}
	}
	return RuntimeRoute{}, fmt.Errorf("runtime lane %q is not installed", lane)
}

// strategyAccounts are Voltr's deposit_strategy and withdraw_strategy
// accounts of v's strategy, managed by v's Squads vault.
func (v *Vault) strategyAccounts() voltr.StrategyAccounts {
	return voltr.StrategyAccounts{Manager: solanaKey(v.squads), Protocol: solanaKey(v.protocol), Vault: solanaKey(v.VoltrVault),
		Strategy: solanaKey(v.Strategy), AdaptorAddReceipt: solanaKey(v.adaptorReceipt), StrategyInitReceipt: solanaKey(v.strategyReceipt),
		VaultAssetIdleAuth: solanaKey(v.idleAuth), VaultStrategyAuth: solanaKey(v.strategyAuth), AssetMint: solanaKey(bridgeUSDC),
		LPMint: solanaKey(v.lpMint), VaultAssetIdleATA: solanaKey(v.idleATA), VaultStrategyAssetATA: solanaKey(v.strategyATA),
		AssetTokenProgram: solanaKey(bridgeTokenProgram), AdaptorProgram: solanaKey(v.Adaptor)}
}

// vaultOfRoute is the vault whose route row is routeKey.
func vaultOfRoute(routeKey string) (*Vault, error) {
	for _, v := range vaults {
		if v.RouteKey == routeKey {
			return v, nil
		}
	}
	return nil, fmt.Errorf("route %q is no Backyard vault's", routeKey)
}

// RouteKey is the route row of the vault named vault, for the operator
// commands.
func RouteKey(vault string) (string, error) {
	v, err := findVault(vault)
	if err != nil {
		return "", err
	}
	return v.RouteKey, nil
}

// RouteKeys are every vault's route rows.
func RouteKeys() []string {
	out := make([]string, len(vaults))
	for i, v := range vaults {
		out[i] = v.RouteKey
	}
	return out
}
