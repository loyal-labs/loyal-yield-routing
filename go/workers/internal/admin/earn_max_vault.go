package admin

import (
	"github.com/danielgtaylor/huma/v2"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/solana-foundation/solana-go/v2"
)

type EarnMaxIdentity struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"`
	Worker  string `json:"worker"`
	Squads  string `json:"squads"`
	CashATA string `json:"cashAta"`
	IdleATA string `json:"idleAta"`
}

type earnMaxConfig struct {
	identity EarnMaxIdentity
	backyard.VaultDescription
}

func earnMaxConfigFor(id string) (earnMaxConfig, error) {
	var identity EarnMaxIdentity
	var name string
	switch id {
	case "", "loyal":
		identity = EarnMaxIdentity{ID: "loyal", Name: "Loyal Earn MAX", Worker: "loyal-earn-max"}
		name = "rwa"
	case "backyard":
		identity = EarnMaxIdentity{ID: "backyard", Name: "Backyard", Worker: "loyal-backyard"}
		name = "looping"
	default:
		return earnMaxConfig{}, huma.Error400BadRequest("Unknown vault. Choose loyal or backyard.")
	}
	description, err := backyard.DescribeVault(name)
	if err != nil {
		return earnMaxConfig{}, err
	}
	a := description.Accounts
	cash, err := spl.AssociatedTokenAddress(a.Manager, a.AssetMint, solana.TokenProgramID)
	if err != nil {
		return earnMaxConfig{}, err
	}
	identity.Address, identity.Squads = a.Vault.String(), a.Manager.String()
	identity.CashATA, identity.IdleATA = cash.String(), a.VaultAssetIdleATA.String()
	return earnMaxConfig{identity: identity, VaultDescription: description}, nil
}

func (c earnMaxConfig) settings() []BackyardSetting {
	a := c.Accounts
	return []BackyardSetting{
		{Key: "Voltr vault", Value: a.Vault.String()},
		{Key: "Squads vault", Value: a.Manager.String()},
		{Key: "Squads settings", Value: c.Settings},
		{Key: "Adaptor", Value: a.AdaptorProgram.String()},
		{Key: "Strategy", Value: a.Strategy.String()},
		{Key: "Strategy receipt", Value: a.StrategyInitReceipt.String()},
	}
}
