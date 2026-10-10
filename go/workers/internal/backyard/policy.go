package backyard

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
)

// backyardVaultIndex is the smart account every Backyard policy executes as:
// Settings' vault 0, bridgeVault.
const backyardVaultIndex uint8 = 0

// vaultPolicy is the Backyard vault's policy of constraints, in leg order,
// with no spending limits.
func vaultPolicy(constraints ...squads.InstructionConstraintView) squads.Policy {
	return squads.Policy{VaultIndex: backyardVaultIndex, Constraints: constraints}
}

// policyKey is what the runtime looks a Backyard policy up by: the route lane
// it serves (empty when lanes share it) and, within that, the action it
// executes, the leg of a split KLend policy, or the basic family. A catalog
// swap policy serves the two conversions of its edges, in constraint order,
// whatever lane asks for them.
type policyKey struct {
	lane   string
	action Action
	leg    kaminoPrimeUSDCLeg
	family BasicPolicyFamily
	swaps  [2]conversion
}

// conversion is one swap a policy admits: the from symbol into the to symbol.
type conversion struct{ from, to string }

// splitKaminoRoutes are the routes with one split KLend policy per leg.
var splitKaminoRoutes = []RuntimeRoute{ethenaUSDePYUSD, primePRIMEPYUSD, primePRIMEUSDS}

var kaminoLegs = []kaminoPrimeUSDCLeg{kaminoLegDeposit, kaminoLegWithdraw, kaminoLegBorrow, kaminoLegRepay}

// backyardPolicies is every policy Backyard executes through, by what the
// runtime looks it up by.
func backyardPolicies() (map[policyKey]squads.Policy, error) {
	out := map[policyKey]squads.Policy{}
	var first error
	add := func(key policyKey, policy squads.Policy, err error) {
		if err != nil && first == nil {
			first = fmt.Errorf("%+v: %w", key, err)
		}
		out[key] = policy
	}
	auto, err := autoPolicy(autoAUTOPYUSD)
	add(policyKey{lane: autoAUTOPYUSD.Lane}, auto, err)
	for _, family := range []BasicPolicyFamily{BasicCollateralLifecycle, BasicDebtLifecycle, BasicSwapRoutesA, BasicSwapRoutesB} {
		policy, err := basicPolicy(family)
		add(policyKey{family: family}, policy, err)
	}
	for _, lane := range selectorLanes {
		route, err := runtimeRoute(lane)
		if err != nil {
			return nil, err
		}
		policy, err := initializerPolicy(route)
		add(policyKey{lane: lane, action: InitializeKaminoObligation}, policy, err)
	}
	legacy, err := runtimeRoute(RouteID)
	if err != nil {
		return nil, err
	}
	for _, route := range append([]RuntimeRoute{legacy}, splitKaminoRoutes...) {
		for _, leg := range kaminoLegs {
			policy, err := kaminoLegPolicy(route, leg)
			add(policyKey{lane: route.Lane, leg: leg}, policy, err)
		}
	}
	forward, err := primeUSDCForwardPolicy()
	add(policyKey{lane: RouteID, action: SwapUSDCToPrimeStep}, forward, err)
	swaps, err := primeUSDCSwapPolicy()
	add(policyKey{lane: RouteID, action: SwapPrimeToUSDCStep}, swaps, err)
	catalog, err := catalogSwapEdges()
	if err != nil {
		return nil, err
	}
	for _, edges := range catalog {
		add(policyKey{swaps: [2]conversion{edges[0].conversion(), edges[1].conversion()}}, swapPolicy(edges[:]...), nil)
	}
	for action, policy := range bridgePolicies() {
		add(policyKey{action: action}, policy, nil)
	}
	return out, first
}

func (l kaminoPrimeUSDCLeg) String() string {
	return map[kaminoPrimeUSDCLeg]string{kaminoLegDeposit: "deposit", kaminoLegWithdraw: "withdraw", kaminoLegBorrow: "borrow", kaminoLegRepay: "repay"}[l]
}

func (k policyKey) String() string {
	switch {
	case k.swaps != [2]conversion{}:
		return fmt.Sprintf("%s->%s/%s->%s swap", k.swaps[0].from, k.swaps[0].to, k.swaps[1].from, k.swaps[1].to)
	case k.family != "":
		return string(k.family)
	case k.leg != 0:
		return fmt.Sprintf("%s %s", k.lane, k.leg)
	case k.lane == "":
		return string(k.action)
	case k.action != "":
		return fmt.Sprintf("%s %s", k.lane, k.action)
	default:
		return k.lane
	}
}

// installedPolicies is, for each Backyard literal, how many accounts on
// Backyard's Settings, delegated to its executor, decode to it, and that
// account when exactly one does. A build reads it once, at its slot, and
// executes every instruction it composes through it.
type installedPolicies map[policyKey]installedPolicy

type installedPolicy struct {
	squads.Installed
	matches int
}

// observeInstalledPolicies reads every policy on Backyard's Settings at a
// slot no older than minSlot and finds each literal among them.
func observeInstalledPolicies(ctx context.Context, rpc *chain.Client, minSlot int64) (installedPolicies, error) {
	installed, err := squads.Policies(ctx, rpc, solanaKey(bridgeSettings), uint64(max(minSlot, 0)))
	if err != nil {
		return nil, unavailable(err)
	}
	return findInstalledPolicies(installed)
}

func findInstalledPolicies(installed []squads.Installed) (installedPolicies, error) {
	literals, err := backyardPolicies()
	if err != nil {
		return nil, err
	}
	out := installedPolicies{}
	for key, literal := range literals {
		found, matches := squads.FindPolicy(installed, solanaKey(bridgeSettings), solanaKey(bridgeDelegate), literal)
		out[key] = installedPolicy{found, matches}
	}
	return out, nil
}

// account is the installed account of key's literal; a literal installed
// on no account, or on more than one, holds.
func (p installedPolicies) account(key policyKey) (string, error) {
	switch p[key].matches {
	case 1:
		return p[key].Account.String(), nil
	case 0:
		return "", budgetHold(key.String() + " policy not installed")
	default:
		return "", budgetHold(key.String() + " policy installed twice")
	}
}

// bridgePolicyKeys are the Voltr bridge's four policies, which every lane
// with a bridge exit leaves through.
var bridgePolicyKeys = []policyKey{{action: VoltrAllocateToSquads}, {action: StageSquadsToVoltr}, {action: VoltrRestoreIdle}, {action: ReportNAV}}

// kaminoPolicyLeg is the policy a route executes leg through and the leg's
// constraint there: the AUTO lane's one policy, a basic family, or the
// route's split policy for the leg.
func kaminoPolicyLeg(route RuntimeRoute, leg kaminoPrimeUSDCLeg) (policyKey, byte) {
	switch {
	case route.Lane == autoAUTOPYUSD.Lane:
		return policyKey{lane: route.Lane}, map[kaminoPrimeUSDCLeg]byte{kaminoLegDeposit: autoDeposit, kaminoLegWithdraw: autoWithdraw,
			kaminoLegBorrow: autoBorrow, kaminoLegRepay: autoRepay}[leg]
	case route.BasicPolicy && (leg == kaminoLegDeposit || leg == kaminoLegWithdraw):
		return policyKey{family: BasicCollateralLifecycle}, map[kaminoPrimeUSDCLeg]byte{kaminoLegDeposit: basicDeposit, kaminoLegWithdraw: basicWithdraw}[leg]
	case route.BasicPolicy:
		return policyKey{family: BasicDebtLifecycle}, map[kaminoPrimeUSDCLeg]byte{kaminoLegBorrow: basicBorrow, kaminoLegRepay: basicRepay}[leg]
	default:
		return policyKey{lane: route.Lane, leg: leg}, splitLeg
	}
}

// initializerPolicyLeg is the policy that creates lane's obligation and its
// constraint there.
func initializerPolicyLeg(lane string) (policyKey, byte) {
	if lane == autoAUTOPYUSD.Lane {
		return policyKey{lane: lane}, autoInitialize
	}
	return policyKey{lane: lane, action: InitializeKaminoObligation}, splitLeg
}

// jupiterPolicyLeg is the policy lane swaps action through and the edge's
// constraint there. The PRIME/USDC forward policy admits two route plans,
// told apart by the plan id that follows the discriminator in data.
func jupiterPolicyLeg(lane string, action Action, data []byte) (policyKey, byte, error) {
	if lane == "" {
		lane = RouteID
	}
	switch {
	case lane == autoAUTOPYUSD.Lane:
		leg, ok := map[Action]byte{SwapStableToCollateralStep: autoSwapToCollateral, SwapDebtToCollateralStep: autoSwapToCollateral,
			SwapCollateralToStableStep: autoSwapFromCollateral, SwapCollateralToDebtStep: autoSwapFromCollateral, SwapDebtToUSDCStep: autoSwapDebtToUSDC}[action]
		if !ok {
			return policyKey{}, 0, fmt.Errorf("action %s is not an AUTO swap", action)
		}
		return policyKey{lane: lane}, leg, nil
	case catalogJupiterRoute(lane):
		edges, leg, err := catalogEdge(action, lane)
		return policyKey{swaps: [2]conversion{edges[0].conversion(), edges[1].conversion()}}, leg, err
	case selectorLane(lane):
		_, _, source, _, err := jupiterEdgeForRoute(action, lane)
		if err != nil {
			return policyKey{}, 0, err
		}
		family := BasicSwapRoutesB
		if source == bridgeSquadsATA {
			family = BasicSwapRoutesA
		}
		return policyKey{family: family}, basicSwapLeg[lane], nil
	case lane == RouteID && action == SwapUSDCToPrimeStep:
		for leg, id := range primeForwardPlanIDs {
			if len(data) > 8 && data[8] == id {
				return policyKey{lane: lane, action: action}, byte(leg), nil
			}
		}
		return policyKey{}, 0, fmt.Errorf("the forward PRIME/USDC policy admits no such route plan")
	case lane == RouteID && action == SwapPrimeToUSDCStep:
		return policyKey{lane: lane, action: action}, primeSwapOut, nil
	default:
		return policyKey{}, 0, fmt.Errorf("lane %q has no Jupiter policy for %s", lane, action)
	}
}

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
func autoPolicy(route RuntimeRoute) (squads.Policy, error) {
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
	return vaultPolicy(out[:]...), err
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

// basicSwapLeg is the swap leg each basic lane executes under; PRIME, which
// both legs admit, swaps under the first.
var basicSwapLeg = map[string]byte{"OnRe/ONyc/USDC": basicSwapONycPrime, PhaseOneLaneID: basicSwapONycPrime, SelectedRouteID: basicSwapPrimeSyrup}

// basicPolicy is one basic family's policy, shared by the three runtime lanes
// (OnRe, Prime, Maple, in that order in every pin). Its KLend legs pin the
// vault, an obligation it owns, each lane's collateral reserve (or market) and
// the custodies; its swaps pin the vault, the custodies of their direction and
// no platform fee.
func basicPolicy(family BasicPolicyFamily) (squads.Policy, error) {
	var lanes [3]RuntimeRoute
	for i, lane := range [3]string{onreONycUSDC, PhaseOneLaneID, SelectedRouteID} {
		route, err := basicRuntimeRoute(lane)
		if err != nil {
			return squads.Policy{}, err
		}
		lanes[i] = route
	}
	onre, prime, maple := lanes[0], lanes[1], lanes[2]
	switch family {
	case BasicCollateralLifecycle:
		leg := vaultCollateral(pinned(onre.Kamino.CollateralReserve, prime.Kamino.CollateralReserve, maple.Kamino.CollateralReserve),
			pinned(onre.CollateralCustody, prime.CollateralCustody, maple.CollateralCustody))
		var out [basicCollateralLegs]squads.InstructionConstraintView
		out[basicDeposit] = kamino.DepositV2Allowed(leg, squads.Unpinned)
		out[basicWithdraw] = kamino.WithdrawV2Allowed(leg, squads.Unpinned)
		return vaultPolicy(out[:]...), nil
	case BasicDebtLifecycle:
		leg := vaultDebt(pinned(onre.Kamino.Market, prime.Kamino.Market, maple.Kamino.Market),
			pinned(bridgeSquadsATA, primePRIMEUSDS.DebtCustody, autoAUTOPYUSD.DebtCustody))
		var out [basicDebtLegs]squads.InstructionConstraintView
		out[basicBorrow] = kamino.BorrowV2Allowed(leg, squads.Unpinned)
		out[basicRepay] = kamino.RepayV2Allowed(leg, squads.Unpinned)
		return vaultPolicy(out[:]...), nil
	case BasicSwapRoutesA, BasicSwapRoutesB:
		stables := [basicSwapLegs]squads.Slot{pinned(bridgeSquadsATA, primePRIMEUSDS.DebtCustody), pinned(bridgeSquadsATA, autoAUTOPYUSD.DebtCustody)}
		collaterals := [basicSwapLegs]squads.Slot{pinned(onre.CollateralCustody, prime.CollateralCustody), pinned(prime.CollateralCustody, maple.CollateralCustody)}
		var out [basicSwapLegs]squads.InstructionConstraintView
		for leg := range out {
			if family == BasicSwapRoutesA {
				out[leg] = vaultSwap(stables[leg], collaterals[leg])
			} else {
				out[leg] = vaultSwap(collaterals[leg], stables[leg])
			}
		}
		return vaultPolicy(out[:]...), nil
	default:
		return squads.Policy{}, fmt.Errorf("unknown basic policy family %q", family)
	}
}

// A split policy authorizes one leg of one lane as its one constraint.
const splitLeg = 0

// kaminoLegPolicy is a lane's split KLend policy for leg: every account the
// leg sends pinned, KLend's own slots too (squads.Pinned), and the amount at
// most the vault cap.
func kaminoLegPolicy(route RuntimeRoute, leg kaminoPrimeUSDCLeg) (squads.Policy, error) {
	collateral, debt := kaminoRouteAccounts(route, func(address string) squads.Slot { return squads.Pin(kaminoKey(address)) })
	amount := squads.DataU64(kamino.AmountOffset, squads.OpLessThanOrEqualTo, bridgeCapRaw)
	switch leg {
	case kaminoLegDeposit:
		return vaultPolicy(kamino.DepositV2Allowed(collateral, squads.Pinned, amount)), nil
	case kaminoLegWithdraw:
		return vaultPolicy(kamino.WithdrawV2Allowed(collateral, squads.Pinned, amount)), nil
	case kaminoLegBorrow:
		return vaultPolicy(kamino.BorrowV2Allowed(debt, squads.Pinned, amount)), nil
	case kaminoLegRepay:
		return vaultPolicy(kamino.RepayV2Allowed(debt, squads.Pinned, amount)), nil
	default:
		return squads.Policy{}, fmt.Errorf("unknown KLend leg %d", leg)
	}
}

// initializerPolicy is a Multiply lane's split policy that creates its one
// obligation.
func initializerPolicy(route RuntimeRoute) (squads.Policy, error) {
	constraint, err := obligationInitAllowed(route)
	return vaultPolicy(constraint), err
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

// primeForwardPlanIDs is the id byte of each forward route plan, by leg.
var primeForwardPlanIDs = [primeForwardLegs]byte{primeForwardPlanID1: 1, primeForwardPlanID2: 2}

func primeUSDCForwardPolicy() (squads.Policy, error) {
	a, err := routeSwapAssets()
	if err != nil {
		return squads.Policy{}, err
	}
	var forward [primeForwardLegs]swapEdge
	for leg, id := range primeForwardPlanIDs {
		// id, one step: swap variant 116 with its one byte, 100 percent, token 0 into 1.
		plan := jupiter.ArgsPrefix([]byte{id, 1, 0, 0, 0, 0x74, 0, 100, 0, 1})
		forward[leg] = sharedEdge(a.usdc, a.prime, 18, plan)
	}
	return swapPolicy(forward[:]...), nil
}

func primeUSDCSwapPolicy() (squads.Policy, error) {
	a, err := routeSwapAssets()
	if err != nil {
		return squads.Policy{}, err
	}
	var swaps [primeSwapLegs]swapEdge
	swaps[primeSwapIn] = shared(a.usdc, a.prime, 22)
	swaps[primeSwapOut] = shared(a.prime, a.usdc, 18)
	return swapPolicy(swaps[:]...), nil
}

// catalogSwapEdges are the two-edge Jupiter policies the catalog lanes swap
// through, each its edges in constraint order. An edge's in_amount offset is
// where the route plan of the quote it was installed from put it.
func catalogSwapEdges() ([][2]swapEdge, error) {
	a, err := routeSwapAssets()
	if err != nil {
		return nil, err
	}
	return [][2]swapEdge{
		{shared(a.usdc, a.onyc, 19), shared(a.usdc, a.prime, 22)},
		{shared(a.usdc, a.usde, 18), shared(a.usds, a.onyc, 28)},
		{shared(a.prime, a.usdc, 18), shared(a.prime, a.usds, 23)},
		{routed(a.usdc, a.usdg, 17, 20, 21), shared(a.usdc, a.pyusd, 18)},
		{routed(a.pyusd, a.prime, 26, 18, 19), routed(a.pyusd, a.syrup, 22, 18, 19)},
		{routed(a.pyusd, a.auto, 23, 10, 15), routed(a.pyusd, a.usde, 22, 23, 22)},
		{shared(a.prime, a.usdg, 23), shared(a.prime, a.pyusd, 29)},
		{shared(a.usde, a.usdg, 23), shared(a.usde, a.pyusd, 29)},
		{shared(a.pyusd, a.usdc, 18), shared(a.pyusd, a.usds, 23)},
		{shared(a.usds, a.prime, 27), shared(a.usds, a.syrup, 23)},
		{shared(a.usde, a.usdc, 17), shared(a.usde, a.usds, 29)},
		{shared(a.usdc, a.usds, 18), shared(a.usdg, a.pyusd, 24)},
		{shared(a.usds, a.usdc, 18), shared(a.pyusd, a.usdg, 23)},
	}, nil
}

// catalogSwap is the catalog policy's edges with the edge that swaps the from
// symbol into the to symbol, and that edge's constraint index.
func catalogSwap(from, to string) ([2]swapEdge, byte, error) {
	policies, err := catalogSwapEdges()
	if err != nil {
		return [2]swapEdge{}, 0, err
	}
	for _, policy := range policies {
		for index, edge := range policy {
			if edge.conversion() == (conversion{from, to}) {
				return policy, byte(index), nil
			}
		}
	}
	return [2]swapEdge{}, 0, fmt.Errorf("no catalog swap policy swaps %s into %s", from, to)
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

// swapEdge is one conversion a split swap policy admits. inAmountAt is where
// the edge's data holds in_amount: after the route plan of the quote it was
// installed from. The quoted out amount, slippage and platform fee follow it
// at jupiter's fixed offsets; its bounds read them there (jupiter.Bounds, which
// only the PRIME/USDC forward policy makes exact by pinning its route plan),
// and so does the runtime validator of the swap API's instruction.
type swapEdge struct {
	from, to   swapAsset
	inAmountAt uint64
	shared     bool // shared_accounts_route, else route
	constraint squads.InstructionConstraintView
}

func (e swapEdge) conversion() conversion { return conversion{e.from.symbol, e.to.symbol} }

// The route tail's fields, where the edge's instruction holds them.
func (e swapEdge) amountAt() int   { return int(e.inAmountAt) }
func (e swapEdge) quotedAt() int   { return e.amountAt() + jupiter.QuotedOutAfterInAmount }
func (e swapEdge) slippageAt() int { return e.amountAt() + jupiter.SlippageAfterInAmount }
func (e swapEdge) feeAt() int      { return e.amountAt() + jupiter.PlatformFeeAfterInAmount }

// swapPolicy is the vault's policy of swap edges, in constraint order.
func swapPolicy(edges ...swapEdge) squads.Policy {
	constraints := make([]squads.InstructionConstraintView, len(edges))
	for i, edge := range edges {
		constraints[i] = edge.constraint
	}
	return vaultPolicy(constraints...)
}

// swapBounds bounds a split swap whose in_amount is at inAmountAt: at most
// the vault cap, the worker's slippage bound and no platform fee.
func swapBounds(inAmountAt uint64) []squads.DataConstraintView {
	return jupiter.Bounds(inAmountAt, bridgeCapRaw, jupiterMaxSlippageBPS)
}

// shared is the shared_accounts_route edge from the vault's custody into its
// custody, at most the vault cap.
func shared(from, to swapAsset, inAmountAt uint64) swapEdge {
	return sharedEdge(from, to, inAmountAt)
}

// sharedEdge is the shared_accounts_route edge from the vault's custody into
// its custody, both mints pinned and each token program the edge uses, with
// the leading data predicates given and at most the vault cap in.
func sharedEdge(from, to swapAsset, inAmountAt uint64, leading ...squads.DataConstraintView) swapEdge {
	free := squads.Any
	uses := func(program solana.PublicKey) squads.Slot {
		if from.program == program || to.program == program {
			return squads.Pin(program)
		}
		return free
	}
	return swapEdge{from, to, inAmountAt, true, jupiter.SharedAccountsRouteAllowed(jupiter.SharedRouteAllowed{TokenProgram: uses(solana.TokenProgramID),
		ProgramAuthority: free, User: squads.Pin(kaminoKey(bridgeVault)), Source: squads.Pin(from.custody), ProgramSource: free,
		ProgramDestination: free, Destination: squads.Pin(to.custody), SourceMint: squads.Pin(from.mint), DestinationMint: squads.Pin(to.mint),
		PlatformFee: free, Token2022Program: uses(solana.Token2022ProgramID), EventAuthority: free}, append(leading, swapBounds(inAmountAt)...)...)}
}

// routed is the route edge from the vault's custody into its custody; the
// source's token program and mint are the remaining accounts at the given
// positions of the route it was installed from.
func routed(from, to swapAsset, inAmountAt uint64, sourceProgramAt, sourceMintAt int) swapEdge {
	free := squads.Any
	return swapEdge{from, to, inAmountAt, false, jupiter.RouteAllowed(jupiter.Route{TokenProgram: squads.Pin(to.program),
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
	return jupiter.SharedAccountsRouteAllowed(jupiter.SharedRouteAllowed{TokenProgram: free, ProgramAuthority: free,
		User: squads.Pin(kaminoKey(bridgeVault)), Source: source, ProgramSource: free, ProgramDestination: free, Destination: destination,
		SourceMint: free, DestinationMint: free, PlatformFee: squads.Pin(jupiter.ProgramID), Token2022Program: free, EventAuthority: free})
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
		Seed2: pinned(route.Kamino.DebtMint), OwnerUserMetadata: squads.Pin(metadata)}, 1, 0), nil
}

// The capital and NAV bridge policies run two legs in one Squads payload:
// ArmReport arms the report ticket, then the Voltr leg consumes it. A leg's
// value is the constraint index it executes under.
const (
	bridgeArmLeg = iota
	bridgeCapitalLeg
	bridgeCapitalLegs
)

// The staging policy runs one leg: the vault's USDC custody to the strategy.
const (
	bridgeStageLeg = iota
	bridgeStageLegs
)

// bridgePolicies are the Voltr bridge's four policies, by the action each
// authorizes. Every leg pins the vault, the strategy-two config and the
// report ticket it touches, bounds its amount by the per-leg cap (exactly
// zero for a NAV report) and its reported NAV by the vault's max; Voltr and
// the adaptor check the rest. Each policy also spends at most the daily USDC
// cap, a plain limit: no accumulation, per-use cap or exact quantity.
func bridgePolicies() map[Action]squads.Policy {
	pin, free := squads.Pin, squads.Any
	vault, settings, strategy, ticket := solanaKey(bridgeVault), solanaKey(bridgeSettings), solanaKey(bridgeStrategy), solanaKey(reportTicketPDA)
	usdc, custody, strategyATA := solanaKey(bridgeUSDC), solanaKey(bridgeSquadsATA), solanaKey(bridgeStrategyATA)
	limits := []squads.SpendingLimitView{{Mint: usdc, Period: 1 /* daily */, MaxPerPeriod: 200_000_000_000 /* 200k USDC */, Accumulate: false, MaxPerUse: 0, ExactQuantity: false}}
	policy := func(legs ...squads.InstructionConstraintView) squads.Policy {
		out := vaultPolicy(legs...)
		out.SpendingLimits = limits
		return out
	}

	capital := func(offset uint64) []squads.DataConstraintView {
		return []squads.DataConstraintView{squads.DataU64(offset, squads.OpGreaterThan, 0), squads.DataU64(offset, squads.OpLessThanOrEqualTo, strategyTwoBridgeLegCapRaw)}
	}
	nothing := func(offset uint64) []squads.DataConstraintView {
		return []squads.DataConstraintView{squads.DataU64(offset, squads.OpEquals, 0)}
	}
	reportArgs := binary.LittleEndian.AppendUint32([]byte{1}, bridgeReportLen) // Some(ReportV1)
	reportArgs = append(reportArgs, bridgeReportVersion)
	arm := func(operation byte, amount func(uint64) []squads.DataConstraintView) squads.InstructionConstraintView {
		data := append(amount(armAmountOffset),
			squads.DataU64(armReportArgsOffset+bridgeReportArgsPrefixLen+bridgeReportNAVOffset, squads.OpLessThanOrEqualTo, bridgeMaxNAV),
			squads.DataBytes(armReportArgsOffset, reportArgs))
		return armReportAllowed(armReport[squads.Slot]{Strategy: pin(strategy), Ticket: pin(ticket), Settings: free, Vault: free}, operation, data...)
	}
	move := func(allowed func(voltr.StrategyAllowed, []squads.AccountSlot[squads.Slot], ...squads.DataConstraintView) squads.InstructionConstraintView,
		adaptorInstruction []byte, amount func(uint64) []squads.DataConstraintView) squads.InstructionConstraintView {
		call := append(binary.LittleEndian.AppendUint32([]byte{1}, uint32(len(adaptorInstruction))), adaptorInstruction...) // Some(adaptor instruction)
		reportOffset := uint64(voltr.StrategyAdaptorCallOffset + len(call) + bridgeReportArgsPrefixLen)
		data := append(amount(voltr.StrategyAmountOffset),
			squads.DataU64(reportOffset+bridgeReportNAVOffset, squads.OpLessThanOrEqualTo, bridgeMaxNAV),
			squads.DataBytes(voltr.StrategyAdaptorCallOffset, append(call, reportArgs...)))
		accounts := voltr.StrategyAllowed{Manager: pin(vault), Protocol: free, Vault: pin(solanaKey(bridgeVoltrVault)), Strategy: pin(strategy),
			AdaptorAddReceipt: free, StrategyInitReceipt: free, VaultAssetIdleAuth: free, VaultStrategyAuth: free, AssetMint: pin(usdc),
			LPMint: free, VaultAssetIdleATA: free, VaultStrategyAssetATA: pin(strategyATA), AssetTokenProgram: pin(solana.TokenProgramID),
			AdaptorProgram: pin(solanaKey(bridgeAdaptorProgram))}
		remaining := bridgeRemainingSlots(bridgeRemaining[squads.Slot]{Settings: pin(settings), Vault: pin(vault), Custody: pin(custody), Ticket: pin(ticket)})
		return allowed(accounts, remaining, data...)
	}
	twoLegs := func(armLeg, capitalLeg squads.InstructionConstraintView) squads.Policy {
		var legs [bridgeCapitalLegs]squads.InstructionConstraintView
		legs[bridgeArmLeg], legs[bridgeCapitalLeg] = armLeg, capitalLeg
		return policy(legs[:]...)
	}
	var stage [bridgeStageLegs]squads.InstructionConstraintView
	stage[bridgeStageLeg] = spl.TransferCheckedAllowed(solana.TokenProgramID, spl.TransferAllowed{Source: pin(custody), Mint: pin(usdc),
		Destination: pin(strategyATA), Authority: pin(vault)}, 6, capital(spl.TransferCheckedAmountOffset)...)

	return map[Action]squads.Policy{
		VoltrAllocateToSquads: twoLegs(arm(reportTicketDeposit, capital), move(voltr.DepositStrategyAllowed, adaptorDepositDiscriminator, capital)),
		ReportNAV:             twoLegs(arm(reportTicketDeposit, nothing), move(voltr.DepositStrategyAllowed, adaptorDepositDiscriminator, nothing)),
		VoltrRestoreIdle:      twoLegs(arm(reportTicketWithdraw, capital), move(voltr.WithdrawStrategyAllowed, adaptorWithdrawDiscriminator, capital)),
		StageSquadsToVoltr:    policy(stage[:]...),
	}
}
