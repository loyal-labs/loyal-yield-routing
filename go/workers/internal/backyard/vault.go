package backyard

import (
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
)

type Vault struct {
	Name                          string
	VoltrVault, Strategy, Adaptor string
	SquadsIndex                   uint8
	RouteKey                      string
	AdminPerformanceFeeBPS        int64
	IdleBufferRaw                 int64

	squads, cash                               string
	protocol, idleAuth, idleATA, lpMint        string
	strategyAuth, strategyATA, strategyReceipt string
	adaptorReceipt                             string
	holdingAuth, holding                       string
	routes                                     []RuntimeRoute
	manifest                                   RouteManifest
}

var rwa = newVault(Vault{Name: "rwa", VoltrVault: "HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA",
	Strategy: "DCpR24Eb6xCWxDyaZvCBTkadkxCB2vkqJN1EfYNWtLxY", Adaptor: fsj27Program, SquadsIndex: 0,
	RouteKey: "rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh", AdminPerformanceFeeBPS: 0, IdleBufferRaw: 5_000_000_000})

var looping = newVault(Vault{Name: "looping", VoltrVault: "GrHZD15bMiUwrVPsmBwz8GRoQeDZN6wNEQVdW8k8SxV1",
	Strategy: "GUCvzfn1hqThWp5uQNF7CqoxC1XckCPTjsRGKVXHPxu1", Adaptor: voltr.TrustfulProgramID.String(), SquadsIndex: 2,
	RouteKey: "backyard:6HTfVjwk5FfW4GH67hmEen7VHeURdB5EJj6E429qTrUY", AdminPerformanceFeeBPS: 0, IdleBufferRaw: 0})

var vaults = []*Vault{rwa, looping}

func (v *Vault) selectorPolicy() SelectorPolicy {
	p := DefaultSelectorPolicy()
	p.IdleBufferRaw = v.IdleBufferRaw
	return p
}

const fsj27Program = "FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW"

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
		return must(spl.AssociatedTokenAddress(key(owner), key(fleet.USDCMint), solana.TokenProgramID))
	}
	squadsVault, _, err := squads.SmartAccountAddress(key(backyardSettings), v.SquadsIndex)
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

func (v *Vault) trustful() bool { return v.Adaptor == voltr.TrustfulProgramID.String() }

func (v *Vault) route(lane string) (RuntimeRoute, error) {
	for _, route := range v.routes {
		if route.Lane == lane {
			return route, nil
		}
	}
	return RuntimeRoute{}, fmt.Errorf("runtime lane %q is not installed", lane)
}

func (v *Vault) strategyAccounts() voltr.StrategyAccounts {
	return voltr.StrategyAccounts{Manager: solanaKey(v.squads), Protocol: solanaKey(v.protocol), Vault: solanaKey(v.VoltrVault),
		Strategy: solanaKey(v.Strategy), AdaptorAddReceipt: solanaKey(v.adaptorReceipt), StrategyInitReceipt: solanaKey(v.strategyReceipt),
		VaultAssetIdleAuth: solanaKey(v.idleAuth), VaultStrategyAuth: solanaKey(v.strategyAuth), AssetMint: solanaKey(fleet.USDCMint),
		LPMint: solanaKey(v.lpMint), VaultAssetIdleATA: solanaKey(v.idleATA), VaultStrategyAssetATA: solanaKey(v.strategyATA),
		AssetTokenProgram: solanaKey(bridgeTokenProgram), AdaptorProgram: solanaKey(v.Adaptor)}
}

func vaultOfRoute(routeKey string) (*Vault, error) {
	for _, v := range vaults {
		if v.RouteKey == routeKey {
			return v, nil
		}
	}
	return nil, fmt.Errorf("route %q is no Backyard vault's", routeKey)
}

func RouteKey(vault string) (string, error) {
	v, err := findVault(vault)
	if err != nil {
		return "", err
	}
	return v.RouteKey, nil
}

func RouteKeys() []string {
	out := make([]string, len(vaults))
	for i, v := range vaults {
		out[i] = v.RouteKey
	}
	return out
}
