package backyard

import (
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
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
	collateralLeg := vaultCollateral(pinned(route.Kamino.CollateralReserve), pinned(route.CollateralCustody))
	debtLeg := vaultDebt(pinned(route.Kamino.Market), pinned(route.DebtCustody))
	out[autoDeposit] = kamino.DepositV2Allowed(collateralLeg, squads.Unpinned)
	out[autoWithdraw] = kamino.WithdrawV2Allowed(collateralLeg, squads.Unpinned)
	out[autoBorrow] = kamino.BorrowV2Allowed(debtLeg, squads.Unpinned)
	out[autoRepay] = kamino.RepayV2Allowed(debtLeg, squads.Unpinned)
	out[autoSwapToCollateral] = vaultSwap(pinned(bridgeSquadsATA, route.DebtCustody), pinned(route.CollateralCustody))
	out[autoSwapFromCollateral] = vaultSwap(pinned(route.CollateralCustody), pinned(bridgeSquadsATA, route.DebtCustody))
	out[autoSwapDebtToUSDC] = vaultSwap(pinned(route.DebtCustody), pinned(bridgeSquadsATA))
	var err error
	out[autoInitialize], err = obligationInitAllowed(route)
	return out, err
}

// The basic families' legs, in the order of each family's policy. The swap
// families pair the same two edges: SwapRoutesA swaps out of USDC (or another
// stable) and SwapRoutesB back into it.
const (
	basicDeposit = iota // CollateralLifecycle
	basicWithdraw
	basicCollateralLegs
)

const (
	basicBorrow = iota // DebtLifecycle
	basicRepay
	basicDebtLegs
)

const (
	basicSwapONycPrime  = iota // USDC or USDS with ONyc or PRIME
	basicSwapPrimeSyrup        // USDC or PYUSD with PRIME or syrupUSDC
	basicSwapLegs
)

// basicPolicy is one basic family's policy, shared by the three runtime lanes
// (OnRe, Prime, Maple, in that order in every pin). Its KLend legs pin the
// vault, an obligation it owns, each lane's collateral reserve (or market) and
// the custodies; its swaps pin the vault, the custodies of their direction and
// no platform fee.
func basicPolicy(family BasicPolicyFamily) ([]squads.InstructionConstraintView, error) {
	var lanes [3]RuntimeRoute
	for i, lane := range []string{"OnRe/ONyc/USDC", PhaseOneLaneID, SelectedRouteID} {
		route, err := basicRuntimeRoute(lane)
		if err != nil {
			return nil, err
		}
		lanes[i] = route
	}
	onre, prime, maple := lanes[0], lanes[1], lanes[2]
	switch family {
	case BasicCollateralLifecycle:
		leg := vaultCollateral(pinned(onre.Kamino.CollateralReserve, prime.Kamino.CollateralReserve, maple.Kamino.CollateralReserve),
			pinned(onre.CollateralCustody, prime.CollateralCustody, maple.CollateralCustody))
		out := make([]squads.InstructionConstraintView, basicCollateralLegs)
		out[basicDeposit] = kamino.DepositV2Allowed(leg, squads.Unpinned)
		out[basicWithdraw] = kamino.WithdrawV2Allowed(leg, squads.Unpinned)
		return out, nil
	case BasicDebtLifecycle:
		leg := vaultDebt(pinned(onre.Kamino.Market, prime.Kamino.Market, maple.Kamino.Market),
			pinned(bridgeSquadsATA, primePRIMEUSDS.DebtCustody, autoAUTOPYUSD.DebtCustody))
		out := make([]squads.InstructionConstraintView, basicDebtLegs)
		out[basicBorrow] = kamino.BorrowV2Allowed(leg, squads.Unpinned)
		out[basicRepay] = kamino.RepayV2Allowed(leg, squads.Unpinned)
		return out, nil
	case BasicSwapRoutesA, BasicSwapRoutesB:
		stables := [basicSwapLegs]squads.Slot{pinned(bridgeSquadsATA, primePRIMEUSDS.DebtCustody), pinned(bridgeSquadsATA, autoAUTOPYUSD.DebtCustody)}
		collaterals := [basicSwapLegs]squads.Slot{pinned(onre.CollateralCustody, prime.CollateralCustody), pinned(prime.CollateralCustody, maple.CollateralCustody)}
		out := make([]squads.InstructionConstraintView, basicSwapLegs)
		for leg := range out {
			if family == BasicSwapRoutesA {
				out[leg] = vaultSwap(stables[leg], collaterals[leg])
			} else {
				out[leg] = vaultSwap(collaterals[leg], stables[leg])
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown basic policy family %q", family)
	}
}

// A split policy authorizes one leg of one lane as its one constraint. The
// split KLend legs (route KaminoPolicies, the PRIME/USDC packets) have no
// literal yet: they pin KLend's program and sysvar slots and bound the amount,
// which kamino.*Allowed leaves free.
const splitLeg = 0

// initializerPolicy is a Multiply lane's split policy that creates its one
// obligation.
func initializerPolicy(route RuntimeRoute) ([1]squads.InstructionConstraintView, error) {
	constraint, err := obligationInitAllowed(route)
	return [1]squads.InstructionConstraintView{constraint}, err
}

// The PRIME/USDC route's two swap policies. The forward one admits only two
// fixed route plans, which differ in their id byte; the other admits USDC into
// PRIME and PRIME back into USDC.
const (
	primeForwardPlanID1 = iota
	primeForwardPlanID2
	primeForwardLegs
)

const (
	primeSwapIn  = iota // USDC into PRIME
	primeSwapOut        // PRIME into USDC
	primeSwapLegs
)

func primeUSDCSwapPolicies() (forward [primeForwardLegs]squads.InstructionConstraintView, swaps [primeSwapLegs]squads.InstructionConstraintView, err error) {
	a, err := routeSwapAssets()
	if err != nil {
		return forward, swaps, err
	}
	// id, one step: swap variant 116 with its one byte, 100 percent, token 0 into 1.
	plan := func(id byte) squads.DataConstraintView {
		return jupiter.ArgsPrefix([]byte{id, 1, 0, 0, 0, 0x74, 0, 100, 0, 1})
	}
	for leg, id := range [primeForwardLegs]byte{primeForwardPlanID1: 1, primeForwardPlanID2: 2} {
		forward[leg] = sharedWithin(a.usdc, a.prime, append([]squads.DataConstraintView{plan(id)}, swapBounds(18)...)...).constraint
	}
	swaps[primeSwapIn] = shared(a.usdc, a.prime, 22).constraint
	swaps[primeSwapOut] = shared(a.prime, a.usdc, 18).constraint
	return forward, swaps, nil
}

// mapleSwapOutPolicy is the Maple lane's split policy that swaps syrupUSDC
// into USDC, at most one syrupUSDC (6 decimals) at a time.
func mapleSwapOutPolicy() ([1]squads.InstructionConstraintView, error) {
	a, err := routeSwapAssets()
	if err != nil {
		return [1]squads.InstructionConstraintView{}, err
	}
	return [1]squads.InstructionConstraintView{sharedWithin(a.syrup, a.usdc, jupiter.Bounds(18, 1_000_000, jupiterMaxSlippageBPS)...).constraint}, nil
}

// catalogSwapPolicies are the two-edge Jupiter policies the catalog lanes swap
// through, each its edges in constraint order, with the policy's seed. An
// edge's in_amount offset is where the route plan of the quote it was
// installed from put it.
func catalogSwapPolicies() ([][2]swapEdge, error) {
	a, err := routeSwapAssets()
	if err != nil {
		return nil, err
	}
	return [][2]swapEdge{
		{shared(a.usdc, a.onyc, 19), shared(a.usdc, a.prime, 22)},                    // 67
		{shared(a.usdc, a.syrup, 18), shared(a.usdc, a.auto, 18)},                    // 70
		{shared(a.usdc, a.usde, 18), shared(a.usds, a.onyc, 28)},                     // 73
		{shared(a.prime, a.usdc, 18), shared(a.prime, a.usds, 23)},                   // 75
		{routed(a.usdc, a.usdg, 17, 20, 21), shared(a.usdc, a.pyusd, 18)},            // 76
		{routed(a.pyusd, a.prime, 26, 18, 19), routed(a.pyusd, a.syrup, 22, 18, 19)}, // 119
		{routed(a.pyusd, a.auto, 23, 10, 15), routed(a.pyusd, a.usde, 22, 23, 22)},   // 120
		{shared(a.prime, a.usdg, 23), shared(a.prime, a.pyusd, 29)},                  // 122
		{shared(a.auto, a.usdg, 28), shared(a.auto, a.pyusd, 23)},                    // 124
		{shared(a.usde, a.usdg, 23), shared(a.usde, a.pyusd, 29)},                    // 125
		{shared(a.pyusd, a.usdc, 18), shared(a.pyusd, a.usds, 23)},                   // 128
		{shared(a.usds, a.prime, 27), shared(a.usds, a.syrup, 23)},                   // 129
		{shared(a.auto, a.usdc, 18), shared(a.auto, a.usds, 23)},                     // 133
		{shared(a.usde, a.usdc, 17), shared(a.usde, a.usds, 29)},                     // 134
		{shared(a.usdc, a.usds, 18), shared(a.usdg, a.pyusd, 24)},                    // 135
		{shared(a.usds, a.usdc, 18), shared(a.pyusd, a.usdg, 23)},                    // 136
	}, nil
}

// catalogSwapPolicy is the catalog policy with the edge that swaps the from
// symbol into the to symbol, and that edge's constraint index.
func catalogSwapPolicy(from, to string) ([]squads.InstructionConstraintView, byte, error) {
	policies, err := catalogSwapPolicies()
	if err != nil {
		return nil, 0, err
	}
	for _, policy := range policies {
		for index, edge := range policy {
			if edge.from.symbol == from && edge.to.symbol == to {
				return []squads.InstructionConstraintView{policy[0].constraint, policy[1].constraint}, byte(index), nil
			}
		}
	}
	return nil, 0, fmt.Errorf("no catalog swap policy swaps %s into %s", from, to)
}

// swapAsset is one token a swap policy moves: the vault's custody of it, its
// mint and its token program.
type swapAsset struct {
	symbol                 string
	custody, mint, program solana.PublicKey
}

type swapAssets struct{ usdc, prime, usds, pyusd, auto, usde, syrup, onyc, usdg swapAsset }

// usdgMint is Global Dollar, which only the catalog swap policies name.
const usdgMint = "2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH"

func routeSwapAssets() (swapAssets, error) {
	onre, err := basicRuntimeRoute("OnRe/ONyc/USDC")
	if err != nil {
		return swapAssets{}, err
	}
	asset := func(symbol, custody, mint, program string) swapAsset {
		return swapAsset{symbol: symbol, custody: kaminoKey(custody), mint: kaminoKey(mint), program: kaminoKey(program)}
	}
	collateral := func(r RuntimeRoute) swapAsset {
		return asset(r.CollateralSymbol, r.CollateralCustody, r.Kamino.CollateralMint, r.CollateralTokenProgram)
	}
	debt := func(r RuntimeRoute) swapAsset {
		return asset(r.DebtSymbol, r.DebtCustody, r.Kamino.DebtMint, r.DebtTokenProgram)
	}
	usdg, err := spl.AssociatedTokenAddress(kaminoKey(bridgeVault), kaminoKey(usdgMint), solana.Token2022ProgramID)
	if err != nil {
		return swapAssets{}, err
	}
	return swapAssets{
		usdc: asset("USDC", bridgeSquadsATA, bridgeUSDC, classicTokenProgram), prime: collateral(primePRIMEPYUSD),
		usds: debt(primePRIMEUSDS), pyusd: debt(autoAUTOPYUSD), auto: collateral(autoAUTOPYUSD), usde: collateral(ethenaUSDePYUSD),
		syrup: collateral(mapleSyrupUSDCUSDC), onyc: collateral(onre), usdg: asset("USDG", usdg.String(), usdgMint, token2022Program),
	}, nil
}

// swapEdge is one conversion a split swap policy admits.
type swapEdge struct {
	from, to   swapAsset
	constraint squads.InstructionConstraintView
}

// swapBounds bounds a split swap whose in_amount is at inAmountAt: at most the
// vault cap, the worker's slippage bound and no platform fee.
func swapBounds(inAmountAt uint64) []squads.DataConstraintView {
	return jupiter.Bounds(inAmountAt, bridgeCapRaw, jupiterMaxSlippageBPS)
}

// shared is the shared_accounts_route edge from the vault's custody into its
// custody, both mints pinned and each token program the edge uses.
func shared(from, to swapAsset, inAmountAt uint64) swapEdge {
	return sharedWithin(from, to, swapBounds(inAmountAt)...)
}

func sharedWithin(from, to swapAsset, bounds ...squads.DataConstraintView) swapEdge {
	free := squads.Any
	uses := func(program solana.PublicKey) squads.Slot {
		if from.program == program || to.program == program {
			return squads.Pin(program)
		}
		return free
	}
	return swapEdge{from, to, jupiter.SharedAccountsRouteAllowed(jupiter.SharedRoute[squads.Slot]{TokenProgram: uses(solana.TokenProgramID),
		ProgramAuthority: free, User: squads.Pin(kaminoKey(bridgeVault)), Source: squads.Pin(from.custody), ProgramSource: free,
		ProgramDestination: free, Destination: squads.Pin(to.custody), SourceMint: squads.Pin(from.mint), DestinationMint: squads.Pin(to.mint),
		PlatformFee: free, Token2022Program: uses(solana.Token2022ProgramID), EventAuthority: free}, squads.Unpinned, bounds...)}
}

// routed is the route edge from the vault's custody into its custody; the
// source's token program and mint are the remaining accounts at the given
// positions of the route it was installed from.
func routed(from, to swapAsset, inAmountAt uint64, sourceProgramAt, sourceMintAt int) swapEdge {
	free := squads.Any
	return swapEdge{from, to, jupiter.RouteAllowed(jupiter.Route{TokenProgram: squads.Pin(to.program),
		User: squads.Pin(kaminoKey(bridgeVault)), Source: squads.Pin(from.custody), Destination: squads.Pin(to.custody),
		DestinationAccount: free, DestinationMint: squads.Pin(to.mint), PlatformFee: free, EventAuthority: free},
		map[int]squads.Slot{sourceProgramAt: squads.Pin(from.program), sourceMintAt: squads.Pin(from.mint)}, swapBounds(inAmountAt)...)}
}

// pinned is the slot that admits only the route addresses given.
func pinned(addresses ...string) squads.Slot {
	keys := make([]solana.PublicKey, len(addresses))
	for i, address := range addresses {
		keys[i] = kaminoKey(address)
	}
	return squads.Pin(keys...)
}

// vaultCollateral is a deposit or withdrawal by the vault on any obligation it
// owns, at reserve and from or into custody; KLend checks the rest.
func vaultCollateral(reserve, custody squads.Slot) kamino.CollateralAllowed {
	vault, free := kaminoKey(bridgeVault), squads.Any
	return kamino.CollateralAllowed{Owner: squads.Pin(vault), Obligation: kamino.OwnedObligation(vault), LendingMarket: free,
		LendingMarketAuthority: free, Reserve: reserve, LiquidityMint: free, LiquiditySupply: free, CollateralMint: free,
		CollateralSupply: free, UserLiquidity: custody, LiquidityTokenProgram: free, ObligationFarmUserState: free, ReserveFarmState: free}
}

// vaultDebt is a borrow or repayment by the vault on any obligation it owns,
// in market and into or from custody; KLend checks the rest.
func vaultDebt(market, custody squads.Slot) kamino.LiquidityAllowed {
	vault, free := kaminoKey(bridgeVault), squads.Any
	return kamino.LiquidityAllowed{Owner: squads.Pin(vault), Obligation: kamino.OwnedObligation(vault), LendingMarket: market,
		LendingMarketAuthority: free, Reserve: free, LiquidityMint: free, LiquiditySupply: free, UserLiquidity: custody,
		TokenProgram: free, FeeReceiver: free, ObligationFarmUserState: free, ReserveFarmState: free}
}

// vaultSwap is a shared_accounts_route by the vault from source into
// destination with no platform fee; Jupiter checks the rest.
func vaultSwap(source, destination squads.Slot) squads.InstructionConstraintView {
	free := squads.Any
	return jupiter.SharedAccountsRouteAllowed(jupiter.SharedRoute[squads.Slot]{TokenProgram: free, ProgramAuthority: free,
		User: squads.Pin(kaminoKey(bridgeVault)), Source: source, ProgramSource: free, ProgramDestination: free, Destination: destination,
		SourceMint: free, DestinationMint: free, PlatformFee: squads.Pin(jupiter.ProgramID), Token2022Program: free, EventAuthority: free},
		squads.Unpinned)
}

// obligationInitAllowed creates the route's one Multiply obligation: every
// account pinned, the vault paying.
func obligationInitAllowed(route RuntimeRoute) (squads.InstructionConstraintView, error) {
	vault := kaminoKey(route.Kamino.Vault)
	metadata, err := kamino.UserMetadataAddress(vault)
	if err != nil {
		return squads.InstructionConstraintView{}, err
	}
	return kamino.InitObligationAllowed(kamino.ObligationInitAllowed{Owner: squads.Pin(vault), FeePayer: squads.Pin(vault),
		Obligation: pinned(route.Kamino.Obligation), LendingMarket: pinned(route.Kamino.Market), Seed1: pinned(route.Kamino.CollateralMint),
		Seed2: pinned(route.Kamino.DebtMint), OwnerUserMetadata: squads.Pin(metadata)}, 1, 0, squads.Pinned), nil
}
