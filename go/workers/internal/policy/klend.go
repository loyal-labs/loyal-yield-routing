package policy

import (
	"context"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// KLend is the product of one smart-account vault lending in one KLend
// reserve through its vanilla obligation: create the user metadata and the
// obligation, deposit amount of liquidity, withdraw the collateral that
// deposit minted. The vault signs and pays rent for what it owns; payer funds
// the vault's ATA and the obligation farm outside the policy, as the Earn
// route does.
//
// Its deposit and withdrawal are constrained as Backyard's are: the vault, an
// obligation it owns, the reserve and the vault's ATA are pinned, and KLend
// checks the rest itself.
func KLend(c *chain.Client, settings solana.PublicKey, vaultIndex uint8, reserve, payer solana.PublicKey, amount uint64) Build {
	return func(ctx context.Context, minSlot uint64) (Product, error) {
		vault, _, err := squads.SmartAccountAddress(settings, vaultIndex)
		if err != nil {
			return Product{}, err
		}
		_, read, err := c.Accounts(ctx, []solana.PublicKey{reserve}, rpc.CommitmentConfirmed, minSlot)
		if err != nil {
			return Product{}, err
		}
		r, err := kamino.DecodeReserve(read[0])
		if err != nil {
			return Product{}, fmt.Errorf("reserve %s: %w", reserve, err)
		}
		market := r.LendingMarket
		authority, err := kamino.LendingMarketAuthority(market)
		if err != nil {
			return Product{}, err
		}
		metadata, err := kamino.UserMetadataAddress(vault)
		if err != nil {
			return Product{}, err
		}
		obligation, err := kamino.VanillaObligation(vault, market)
		if err != nil {
			return Product{}, err
		}
		ata, err := spl.AssociatedTokenAddress(vault, r.LiquidityMint, r.LiquidityTokenProgram)
		if err != nil {
			return Product{}, err
		}
		keys := []solana.PublicKey{metadata, obligation}
		var farmUser solana.PublicKey
		if !r.FarmCollateral.IsZero() {
			if farmUser, err = kamino.ObligationFarmUserState(r.FarmCollateral, obligation); err != nil {
				return Product{}, err
			}
			keys = append(keys, farmUser)
		}
		_, state, err := c.Accounts(ctx, keys, rpc.CommitmentConfirmed, minSlot)
		if err != nil {
			return Product{}, err
		}
		var deposited []solana.PublicKey
		if state[1] != nil {
			o, err := kamino.DecodeObligation(state[1])
			if err != nil {
				return Product{}, err
			}
			deposited = o.DepositReserves()
		}

		// The withdrawal takes back only what the deposit minted, never the
		// vault's other collateral in the reserve.
		minted, err := r.LiquidityToCollateral(amount)
		if err != nil {
			return Product{}, err
		}
		collateral := kamino.CollateralAccounts{Owner: vault, Obligation: obligation, LendingMarket: market, LendingMarketAuthority: authority,
			Reserve: reserve, LiquidityMint: r.LiquidityMint, LiquiditySupply: r.LiquiditySupply, CollateralMint: r.CollateralMint,
			CollateralSupply: r.CollateralSupply, UserLiquidity: ata, LiquidityTokenProgram: r.LiquidityTokenProgram,
			ObligationFarmUserState: farmUser, ReserveFarmState: r.FarmCollateral}
		allowed := kamino.CollateralAllowed{Owner: squads.Pin(vault), Obligation: kamino.OwnedObligation(vault), LendingMarket: squads.Any,
			LendingMarketAuthority: squads.Any, Reserve: squads.Pin(reserve), LiquidityMint: squads.Any, LiquiditySupply: squads.Any,
			CollateralMint: squads.Any, CollateralSupply: squads.Any, UserLiquidity: squads.Pin(ata), LiquidityTokenProgram: squads.Any,
			ObligationFarmUserState: squads.Any, ReserveFarmState: squads.Any}
		refresh := []solana.Instruction{
			kamino.RefreshReserve(kamino.RefreshReserveAccounts{Reserve: reserve, LendingMarket: market, Pyth: r.PythPrice,
				SwitchboardPrice: r.SwitchboardPriceAggregator, SwitchboardTWAP: r.SwitchboardTWAPAggregator, Scope: r.ScopePriceFeed}),
			kamino.RefreshObligation(market, obligation, deposited...),
		}
		depositBefore := []solana.Instruction{spl.CreateIdempotentATA(payer, vault, r.LiquidityMint, r.LiquidityTokenProgram)}
		if !farmUser.IsZero() && state[2] == nil {
			depositBefore = append(depositBefore, kamino.InitObligationFarmsForReserve(kamino.ObligationFarmsInitAccounts{
				Payer: payer, Owner: vault, Obligation: obligation, LendingMarketAuthority: authority, Reserve: reserve,
				ReserveFarmState: r.FarmCollateral, ObligationFarmUserState: farmUser, LendingMarket: market}, 0))
		}
		metadataInit := kamino.UserMetadataInitAccounts{Owner: vault, FeePayer: vault, UserMetadata: metadata}
		obligationInit := kamino.ObligationInitAccounts{Owner: vault, FeePayer: vault, Obligation: obligation, LendingMarket: market,
			OwnerUserMetadata: metadata}

		return Product{Name: "klend:" + reserve.String(), VaultIndex: vaultIndex, Ops: []Op{
			{
				Name: "init user metadata",
				Allowed: kamino.InitUserMetadataAllowed(kamino.UserMetadataInitAllowed{Owner: squads.Pin(vault), FeePayer: squads.Pin(vault),
					UserMetadata: squads.Pin(metadata)}, solana.PublicKey{}, squads.Pinned),
				Inner: kamino.InitUserMetadata(metadataInit, solana.PublicKey{}),
				Done:  state[0] != nil,
			},
			{
				Name: "init obligation",
				Allowed: kamino.InitObligationAllowed(kamino.ObligationInitAllowed{Owner: squads.Pin(vault), FeePayer: squads.Pin(vault),
					Obligation: squads.Pin(obligation), LendingMarket: squads.Pin(market), Seed1: squads.Pin(solana.PublicKey{}),
					Seed2: squads.Pin(solana.PublicKey{}), OwnerUserMetadata: squads.Pin(metadata)}, 0, 0, squads.Pinned),
				Inner: kamino.InitObligation(obligationInit, 0, 0),
				Done:  state[1] != nil,
			},
			{
				Name:    fmt.Sprintf("deposit %d", amount),
				Allowed: kamino.DepositV2Allowed(allowed, squads.Unpinned),
				Inner:   kamino.DepositV2(collateral, amount),
				Before:  append(depositBefore, refresh...),
			},
			{
				Name:    fmt.Sprintf("withdraw %d collateral (the deposit)", minted),
				Allowed: kamino.WithdrawV2Allowed(allowed, squads.Unpinned),
				Inner:   kamino.WithdrawV2(collateral, minted),
				Before:  refresh,
			},
		}}, nil
	}
}
