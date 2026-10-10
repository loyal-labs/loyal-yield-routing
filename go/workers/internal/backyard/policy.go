package backyard

import (
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// The AUTO lane's legs, in the order of its one Squads policy: a leg's value
// is the constraint index it executes under.
const (
	autoDeposit = iota
	autoWithdraw
	autoBorrow
	autoRepay
	autoSwapToCollateral   // USDC or the debt into the collateral
	autoSwapFromCollateral // the collateral into USDC or the debt
	autoSwapDebtToUSDC
	autoInitialize
	autoLegs
)

// autoPolicy is the AUTO lane's policy. The KLend legs pin the vault, an
// obligation it owns, and the reserve (or market) and custody they move; the
// swaps pin the vault, their custody accounts and no platform fee; the
// initializer pins every account of the lane's one obligation. KLend and
// Jupiter check the rest themselves.
func autoPolicy(route RuntimeRoute) ([autoLegs]squads.InstructionConstraintView, error) {
	var out [autoLegs]squads.InstructionConstraintView
	keys, err := publicKeys([]string{route.Kamino.Vault, route.Kamino.Market, route.Kamino.Obligation, route.Kamino.CollateralReserve,
		route.Kamino.CollateralMint, route.Kamino.DebtMint, route.CollateralCustody, route.DebtCustody, bridgeSquadsATA})
	if err != nil {
		return out, err
	}
	vault, market, obligation, reserve, collateralMint, debtMint, collateral, debt, usdc := keys[0], keys[1], keys[2], keys[3], keys[4], keys[5], keys[6], keys[7], keys[8]
	metadata, err := kamino.UserMetadataAddress(vault)
	if err != nil {
		return out, err
	}
	pin, free := squads.Pin, squads.Any
	owned := kamino.OwnedObligation(vault)

	collateralLeg := kamino.CollateralAllowed{Owner: pin(vault), Obligation: owned, LendingMarket: free, LendingMarketAuthority: free,
		Reserve: pin(reserve), LiquidityMint: free, LiquiditySupply: free, CollateralMint: free, CollateralSupply: free,
		UserLiquidity: pin(collateral), LiquidityTokenProgram: free, ObligationFarmUserState: free, ReserveFarmState: free}
	debtLeg := kamino.LiquidityAllowed{Owner: pin(vault), Obligation: owned, LendingMarket: pin(market), LendingMarketAuthority: free,
		Reserve: free, LiquidityMint: free, LiquiditySupply: free, UserLiquidity: pin(debt), TokenProgram: free, FeeReceiver: free,
		ObligationFarmUserState: free, ReserveFarmState: free}
	swap := func(source, destination squads.Slot) squads.InstructionConstraintView {
		return jupiter.SharedAccountsRouteAllowed(jupiter.SharedRoute[squads.Slot]{TokenProgram: free, ProgramAuthority: free,
			User: pin(vault), Source: source, ProgramSource: free, ProgramDestination: free, Destination: destination,
			SourceMint: free, DestinationMint: free, PlatformFee: pin(jupiter.ProgramID), Token2022Program: free, EventAuthority: free},
			squads.Unpinned)
	}

	out[autoDeposit] = kamino.DepositV2Allowed(collateralLeg, squads.Unpinned)
	out[autoWithdraw] = kamino.WithdrawV2Allowed(collateralLeg, squads.Unpinned)
	out[autoBorrow] = kamino.BorrowV2Allowed(debtLeg, squads.Unpinned)
	out[autoRepay] = kamino.RepayV2Allowed(debtLeg, squads.Unpinned)
	out[autoSwapToCollateral] = swap(pin(usdc, debt), pin(collateral))
	out[autoSwapFromCollateral] = swap(pin(collateral), pin(usdc, debt))
	out[autoSwapDebtToUSDC] = swap(pin(debt), pin(usdc))
	out[autoInitialize] = kamino.InitObligationAllowed(kamino.ObligationInitAllowed{Owner: pin(vault), FeePayer: pin(vault),
		Obligation: pin(obligation), LendingMarket: pin(market), Seed1: pin(collateralMint), Seed2: pin(debtMint),
		OwnerUserMetadata: pin(metadata)}, 1, 0, squads.Pinned)
	return out, nil
}
