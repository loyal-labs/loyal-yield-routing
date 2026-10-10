package policy

import (
	"context"
	"fmt"
	"math"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// KLend is the product of one smart-account vault lending in one KLend
// reserve through its vanilla obligation: create the user metadata and the
// obligation, deposit amount of liquidity, withdraw all of it. The vault signs
// and pays rent for what it owns; payer funds the vault's ATA and the
// obligation farm outside the policy, as the Earn route does.
func KLend(c *chain.Client, settings solana.PublicKey, vaultIndex uint8, reserve, payer solana.PublicKey, amount uint64) Build {
	return func(ctx context.Context) (Product, error) {
		vault, _, err := squads.SmartAccountAddress(settings, vaultIndex)
		if err != nil {
			return Product{}, err
		}
		_, read, err := c.Accounts(ctx, []solana.PublicKey{reserve}, rpc.CommitmentConfirmed, 0)
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
		var farmUser solana.PublicKey
		if !r.FarmCollateral.IsZero() {
			if farmUser, err = kamino.ObligationFarmUserState(r.FarmCollateral, obligation); err != nil {
				return Product{}, err
			}
		}
		_, state, err := c.Accounts(ctx, []solana.PublicKey{metadata, obligation, farmUser}, rpc.CommitmentConfirmed, 0)
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

		collateral := kamino.CollateralAccounts{Owner: vault, Obligation: obligation, LendingMarket: market, LendingMarketAuthority: authority,
			Reserve: reserve, LiquidityMint: r.LiquidityMint, LiquiditySupply: r.LiquiditySupply, CollateralMint: r.CollateralMint,
			CollateralSupply: r.CollateralSupply, UserLiquidity: ata, LiquidityTokenProgram: r.LiquidityTokenProgram,
			ObligationFarmUserState: farmUser, ReserveFarmState: r.FarmCollateral}
		allowed := kamino.CollateralAllowed{Owner: one(vault), Obligation: one(obligation), LendingMarket: one(market),
			LendingMarketAuthority: one(authority), Reserve: one(reserve), LiquidityMint: one(r.LiquidityMint),
			LiquiditySupply: one(r.LiquiditySupply), CollateralMint: one(r.CollateralMint), CollateralSupply: one(r.CollateralSupply),
			UserLiquidity: one(ata), LiquidityTokenProgram: one(r.LiquidityTokenProgram),
			ObligationFarmUserState: one(farmUser), ReserveFarmState: one(r.FarmCollateral)}
		if r.FarmCollateral.IsZero() {
			allowed.ObligationFarmUserState, allowed.ReserveFarmState = one(kamino.ProgramID), one(kamino.ProgramID)
		}
		refresh := func(reserves ...solana.PublicKey) []solana.Instruction {
			return []solana.Instruction{
				kamino.RefreshReserve(kamino.RefreshReserveAccounts{Reserve: reserve, LendingMarket: market, Pyth: r.PythPrice,
					SwitchboardPrice: r.SwitchboardPriceAggregator, SwitchboardTWAP: r.SwitchboardTWAPAggregator, Scope: r.ScopePriceFeed}),
				kamino.RefreshObligation(market, obligation, reserves...),
			}
		}
		depositBefore := []solana.Instruction{spl.CreateIdempotentATA(payer, vault, r.LiquidityMint, r.LiquidityTokenProgram)}
		if !farmUser.IsZero() && state[2] == nil {
			depositBefore = append(depositBefore, kamino.InitObligationFarmsForReserve(kamino.InitObligationFarmsAccounts{
				Payer: payer, Owner: vault, Obligation: obligation, LendingMarketAuthority: authority, Reserve: reserve,
				ReserveFarmState: r.FarmCollateral, ObligationFarmUserState: farmUser, LendingMarket: market}, 0))
		}

		return Product{Name: "klend:" + reserve.String(), VaultIndex: vaultIndex, Ops: []Op{
			{
				Name:    "init user metadata",
				Allowed: kamino.InitUserMetadataAllowed(kamino.UserMetadataInit[[]solana.PublicKey]{Owner: one(vault), FeePayer: one(vault), UserMetadata: one(metadata)}, solana.PublicKey{}),
				Inner:   kamino.InitUserMetadata(vault, vault, metadata, solana.PublicKey{}),
				Done:    state[0] != nil,
			},
			{
				Name: "init obligation",
				Allowed: kamino.InitObligationAllowed(kamino.ObligationInit[[]solana.PublicKey]{Owner: one(vault), FeePayer: one(vault), Obligation: one(obligation),
					LendingMarket: one(market), Seed1: one(solana.PublicKey{}), Seed2: one(solana.PublicKey{}), OwnerUserMetadata: one(metadata)}, 0, 0),
				Inner: kamino.InitObligation(vault, vault, obligation, market, solana.PublicKey{}, solana.PublicKey{}, metadata, 0, 0),
				Done:  state[1] != nil,
			},
			{
				Name:    fmt.Sprintf("deposit %d", amount),
				Allowed: kamino.DepositV2Allowed(allowed),
				Inner:   kamino.DepositV2(collateral, amount),
				Before:  append(depositBefore, refresh(deposited...)...),
			},
			{
				Name:    "withdraw all",
				Allowed: kamino.WithdrawV2Allowed(allowed),
				Inner:   kamino.WithdrawV2(collateral, math.MaxUint64),
				Before:  refresh(deposited...),
			},
		}}, nil
	}
}

func one(key solana.PublicKey) []solana.PublicKey { return []solana.PublicKey{key} }
