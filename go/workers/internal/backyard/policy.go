package backyard

import (
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/policy"
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
// swap policy serves the conversions of its edges, named in constraint order
// (swapsName), whatever lane asks for them.
type policyKey struct {
	lane   string
	action Action
	leg    kaminoPrimeUSDCLeg
	family BasicPolicyFamily
	swaps  string
	// hastra names the Hastra conversion policy by its conversions in
	// constraint order (hastraName).
	hastra string
}

// conversion is one swap a policy admits: the from symbol into the to symbol.
type conversion struct{ from, to string }

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
	for _, route := range lanesWithPolicy(policyAuto) {
		auto, err := autoPolicy(route)
		add(policyKey{lane: route.Lane}, auto, err)
	}
	conversions, err := hastraPolicy()
	add(policyKey{hastra: hastraName()}, conversions, err)
	for _, family := range []BasicPolicyFamily{BasicCollateralLifecycle, BasicDebtLifecycle, BasicSwapRoutesA, BasicSwapRoutesB} {
		policy, err := basicPolicy(family)
		add(policyKey{family: family}, policy, err)
	}
	for _, route := range lanesWithPolicy(policyBasic) {
		policy, err := initializerPolicy(route)
		add(policyKey{lane: route.Lane, action: InitializeKaminoObligation}, policy, err)
	}
	for _, route := range lanesWithPolicy(policySplit) {
		for _, leg := range kaminoLegs {
			policy, err := kaminoLegPolicy(route, leg)
			add(policyKey{lane: route.Lane, leg: leg}, policy, err)
		}
	}
	catalog, err := catalogSwapEdges()
	if err != nil {
		return nil, err
	}
	for _, edges := range catalog {
		add(policyKey{swaps: swapsName(edges)}, swapPolicy(edges...), nil)
	}
	for action, policy := range bridgePolicies() {
		add(policyKey{action: action}, policy, nil)
	}
	return out, first
}

// PolicyLiteral is the Backyard policy the runtime holds under name, the name
// a hold gives it (policyKey's String), so `loyal-engine policy apply
// backyard` installs exactly the literal the runtime looks for.
func PolicyLiteral(name string) (squads.Policy, error) {
	literals, err := backyardPolicies()
	if err != nil {
		return squads.Policy{}, err
	}
	var names []string
	for key, literal := range literals {
		if key.String() == name {
			return literal, nil
		}
		names = append(names, key.String())
	}
	slices.Sort(names)
	return squads.Policy{}, fmt.Errorf("no Backyard policy %q; there are %q", name, names)
}

func (l kaminoPrimeUSDCLeg) String() string {
	return map[kaminoPrimeUSDCLeg]string{kaminoLegDeposit: "deposit", kaminoLegWithdraw: "withdraw", kaminoLegBorrow: "borrow", kaminoLegRepay: "repay"}[l]
}

func (k policyKey) String() string {
	switch {
	case k.hastra != "":
		return k.hastra + " hastra"
	case k.swaps != "":
		return k.swaps + " swap"
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
// constraint there: the AUTO lane's KLend policy, a basic family, or the
// route's split policy for the leg.
func kaminoPolicyLeg(route RuntimeRoute, leg kaminoPrimeUSDCLeg) (policyKey, byte) {
	switch {
	case autoRoute(route.Lane):
		return policyKey{lane: route.Lane}, map[kaminoPrimeUSDCLeg]byte{kaminoLegDeposit: policy.LoopDeposit, kaminoLegWithdraw: policy.LoopWithdraw,
			kaminoLegBorrow: policy.LoopBorrow, kaminoLegRepay: policy.LoopRepay}[leg]
	case basicLane(route.Lane) && (leg == kaminoLegDeposit || leg == kaminoLegWithdraw):
		return policyKey{family: BasicCollateralLifecycle}, map[kaminoPrimeUSDCLeg]byte{kaminoLegDeposit: basicDeposit, kaminoLegWithdraw: basicWithdraw}[leg]
	case basicLane(route.Lane):
		return policyKey{family: BasicDebtLifecycle}, map[kaminoPrimeUSDCLeg]byte{kaminoLegBorrow: basicBorrow, kaminoLegRepay: basicRepay}[leg]
	default:
		return policyKey{lane: route.Lane, leg: leg}, splitLeg
	}
}

// initializerPolicyLeg is the policy that creates lane's obligation and its
// constraint there.
func initializerPolicyLeg(lane string) (policyKey, byte) {
	if autoRoute(lane) {
		return policyKey{lane: lane}, policy.LoopInitialize
	}
	return policyKey{lane: lane, action: InitializeKaminoObligation}, splitLeg
}

// jupiterPolicyLeg is the policy lane swaps action through and the edge's
// constraint there.
func jupiterPolicyLeg(lane string, action Action) (policyKey, byte, error) {
	switch {
	case catalogJupiterRoute(lane):
		edges, leg, err := catalogEdge(action, lane)
		return policyKey{swaps: swapsName(edges)}, leg, err
	case basicLane(lane):
		_, _, source, _, err := jupiterEdgeForRoute(action, lane)
		if err != nil {
			return policyKey{}, 0, err
		}
		family := BasicSwapRoutesB
		if source == bridgeSquadsATA {
			family = BasicSwapRoutesA
		}
		l, _ := findEarnLane(lane)
		return policyKey{family: family}, l.basicSwapLeg, nil
	default:
		return policyKey{}, 0, fmt.Errorf("lane %q has no Jupiter policy for %s", lane, action)
	}
}

// autoPolicy is an AUTO lane's KLend policy: the klend-loop policy
// (policy.LoopConstraints) on its collateral and debt reserves. The KLend legs
// pin the vault, an obligation it owns, and the reserve (or market) and
// custody they move; the initializer pins every account of the lane's one
// obligation. KLend checks the rest.
func autoPolicy(route RuntimeRoute) (squads.Policy, error) {
	k := route.Kamino
	out, err := policy.LoopConstraints(kaminoKey(k.Vault), kaminoKey(k.Market), kaminoKey(k.CollateralReserve), kaminoKey(k.CollateralMint),
		kaminoKey(route.CollateralTokenProgram), kaminoKey(k.DebtMint), kaminoKey(route.DebtTokenProgram))
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
	basicSwapONycPrime = iota // USDC with ONyc or PRIME
	basicSwapSyrup            // USDC with syrupUSDC
	basicSwapLegs
)

// basicPolicy is one basic family's policy, shared by the three runtime lanes
// (OnRe, Prime, Maple, in that order in every pin). Its KLend legs pin the
// vault, an obligation it owns, each lane's collateral reserve (or market) and
// the custodies; its swaps pin the vault's custodies of their direction and no
// fee (vaultSwap).
func basicPolicy(family BasicPolicyFamily) (squads.Policy, error) {
	onre, prime, maple := onreONycUSDCRoute, primePRIMEUSDC, mapleSyrupUSDCUSDC
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
		usdc := []swapAsset{usdcAsset()}
		collaterals := [basicSwapLegs][]swapAsset{{collateralAsset(onre), collateralAsset(prime)}, {collateralAsset(maple)}}
		var out [basicSwapLegs]squads.InstructionConstraintView
		for leg := range out {
			if family == BasicSwapRoutesA {
				out[leg] = vaultSwap(usdc, collaterals[leg])
			} else {
				out[leg] = vaultSwap(collaterals[leg], usdc)
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

// catalogSwapEdges are the Jupiter policies the catalog lanes, AUTO among
// them, swap through, each its edges in constraint order. Each policy's create
// fits one transaction.
func catalogSwapEdges() ([][]swapEdge, error) {
	a, err := routeSwapAssets()
	if err != nil {
		return nil, err
	}
	return [][]swapEdge{
		{{a.usdc, a.onyc}, {a.usdc, a.prime}},
		{{a.usdc, a.usde}, {a.usds, a.onyc}},
		{{a.prime, a.usdc}, {a.prime, a.usds}},
		{{a.usdc, a.usdg}, {a.usdc, a.pyusd}},
		{{a.pyusd, a.prime}, {a.pyusd, a.syrup}},
		{{a.pyusd, a.auto}, {a.pyusd, a.usde}},
		{{a.prime, a.usdg}, {a.prime, a.pyusd}},
		{{a.usde, a.usdg}, {a.usde, a.pyusd}},
		{{a.pyusd, a.usdc}, {a.pyusd, a.usds}},
		{{a.usds, a.prime}, {a.usds, a.syrup}},
		{{a.usde, a.usdc}, {a.usde, a.usds}},
		{{a.usdc, a.usds}, {a.usdg, a.pyusd}},
		{{a.usds, a.usdc}, {a.pyusd, a.usdg}},
		{{a.usdc, a.auto}, {a.auto, a.usdc}, {a.auto, a.pyusd}},
	}, nil
}

// catalogSwap is the catalog policy's edges with the edge that swaps the from
// symbol into the to symbol, and that edge's constraint index.
func catalogSwap(from, to string) ([]swapEdge, byte, error) {
	policies, err := catalogSwapEdges()
	if err != nil {
		return nil, 0, err
	}
	for _, policy := range policies {
		for index, edge := range policy {
			if edge.conversion() == (conversion{from, to}) {
				return policy, byte(index), nil
			}
		}
	}
	return nil, 0, fmt.Errorf("no catalog swap policy swaps %s into %s", from, to)
}

// swapsName names a catalog swap policy by its conversions in constraint
// order, as "USDC->ONyc/USDC->PRIME".
func swapsName(edges []swapEdge) string {
	names := make([]string, len(edges))
	for i, edge := range edges {
		names[i] = edge.from.symbol + "->" + edge.to.symbol
	}
	return strings.Join(names, "/")
}

// swapAsset is one token a swap policy moves: the vault's custody of it, its
// mint and its token program.
type swapAsset struct {
	symbol                 string
	custody, mint, program solana.PublicKey
}

// programAccounts are the accounts a shared route may move a through on
// Jupiter's side: each program authority's token account of it, or the
// vault's own account for PYUSD. The swap API routes PYUSD through no
// program authority's token account: every V2 shared-accounts answer with
// PYUSD in or out (8 edges, testdata/jupiter-v2-swap-instructions.json) puts
// the vault's own PYUSD account in that slot, while USDG, with the same
// Token-2022 extensions, goes through the authority's. Jupiter still keeps the
// output in the destination (policy's TestSharedRouteV2PYUSDTamperOnMainnet).
func (a swapAsset) programAccounts() []solana.PublicKey {
	if a.mint == kaminoKey(autoAUTOPYUSD.Kamino.DebtMint) {
		return []solana.PublicKey{a.custody}
	}
	return jupiter.AuthorityTokenAccounts(jupiter.APIAuthorities, a.mint, a.program)
}

type swapAssets struct{ usdc, prime, usds, pyusd, auto, usde, syrup, onyc, usdg swapAsset }

// usdgMint is Global Dollar, which only the catalog swap policies name.
const usdgMint = "2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH"

func asset(symbol, custody, mint, program string) swapAsset {
	return swapAsset{symbol: symbol, custody: kaminoKey(custody), mint: kaminoKey(mint), program: kaminoKey(program)}
}

func usdcAsset() swapAsset { return asset("USDC", bridgeSquadsATA, bridgeUSDC, classicTokenProgram) }

func collateralAsset(r RuntimeRoute) swapAsset {
	return asset(r.CollateralSymbol, r.CollateralCustody, r.Kamino.CollateralMint, r.CollateralTokenProgram)
}

func debtAsset(r RuntimeRoute) swapAsset {
	return asset(r.DebtSymbol, r.DebtCustody, r.Kamino.DebtMint, r.DebtTokenProgram)
}

func routeSwapAssets() (swapAssets, error) {
	usdg, err := spl.AssociatedTokenAddress(kaminoKey(bridgeVault), kaminoKey(usdgMint), solana.Token2022ProgramID)
	if err != nil {
		return swapAssets{}, err
	}
	return swapAssets{
		usdc: usdcAsset(), prime: collateralAsset(primePRIMEPYUSD), usds: debtAsset(primePRIMEUSDS), pyusd: debtAsset(autoAUTOPYUSD),
		auto: collateralAsset(autoAUTOPYUSD), usde: collateralAsset(ethenaUSDePYUSD), syrup: collateralAsset(mapleSyrupUSDCUSDC),
		onyc: collateralAsset(onreONycUSDCRoute), usdg: asset("USDG", usdg.String(), usdgMint, token2022Program),
	}, nil
}

// swapEdge is one conversion a swap policy admits: the vault's custody of from
// into its custody of to.
type swapEdge struct{ from, to swapAsset }

func (e swapEdge) conversion() conversion { return conversion{e.from.symbol, e.to.symbol} }

func (e swapEdge) allowed(data ...squads.DataConstraintView) squads.InstructionConstraintView {
	return vaultSwap([]swapAsset{e.from}, []swapAsset{e.to}, data...)
}

// swapPolicy is the vault's policy of swap edges, in constraint order.
func swapPolicy(edges ...swapEdge) squads.Policy {
	constraints := make([]squads.InstructionConstraintView, len(edges))
	for i, edge := range edges {
		constraints[i] = edge.allowed()
	}
	return vaultPolicy(constraints...)
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
	return kamino.OwnCollateralAllowed(kaminoKey(bridgeVault), reserve, custody)
}

// vaultDebt is a borrow or repayment by the vault on any obligation it owns,
// in market and into or from custody; KLend checks the rest.
func vaultDebt(market, custody squads.Slot) kamino.LiquidityAllowed {
	return kamino.OwnDebtAllowed(kaminoKey(bridgeVault), market, custody)
}

// hastraConversions are the conversions of the Hastra policy, by leg.
var hastraConversions = [policy.HastraLegs]conversion{policy.HastraStakeDeposit: {"wYLDS", "AUTO"}, policy.HastraStakeRedeem: {"AUTO", "wYLDS"},
	policy.HastraMintDeposit: {"USDC", "wYLDS"}}

// hastraName names the Hastra policy by its conversions in constraint order,
// as "wYLDS->AUTO/AUTO->wYLDS/USDC->wYLDS".
func hastraName() string {
	names := make([]string, len(hastraConversions))
	for i, c := range hastraConversions {
		names[i] = c.from + "->" + c.to
	}
	return strings.Join(names, "/")
}

// hastraPolicy is the vault's Hastra conversions between its own associated
// token accounts, every account pinned (policy.HastraConstraints): the
// amount is free, by the owner's no-limits choice for conversions.
func hastraPolicy() (squads.Policy, error) {
	out, _, _, err := policy.HastraConstraints(kaminoKey(bridgeVault))
	return vaultPolicy(out[:]...), err
}

// vaultSwap is a shared_accounts_route_v2 by the vault from its custody of
// any from asset into its custody of any to asset, through any program
// authority the swap API routes through, with no fee: output reaches only a
// to custody. The price is not bounded: in_amount, quoted_out_amount,
// slippage and the route are free, by the owner's no-limits choice for swaps
// (jupiter.SharedAccountsRouteV2Allowed).
func vaultSwap(from, to []swapAsset, data ...squads.DataConstraintView) squads.InstructionConstraintView {
	side := func(assets []swapAsset) (custody, programAccount, mint, program squads.Slot) {
		var custodies, programAccounts, mints, programs []solana.PublicKey
		for _, a := range assets {
			custodies, mints = append(custodies, a.custody), append(mints, a.mint)
			programAccounts = append(programAccounts, a.programAccounts()...)
			if !slices.Contains(programs, a.program) {
				programs = append(programs, a.program)
			}
		}
		return squads.Pin(custodies...), squads.Pin(programAccounts...), squads.Pin(mints...), squads.Pin(programs...)
	}
	source, programSource, sourceMint, sourceProgram := side(from)
	destination, programDestination, destinationMint, destinationProgram := side(to)
	return jupiter.SharedAccountsRouteV2Allowed(jupiter.SharedRouteV2Allowed{ProgramAuthority: squads.Pin(jupiter.Authorities(jupiter.APIAuthorities)...),
		User: squads.Pin(kaminoKey(bridgeVault)), Source: source, ProgramSource: programSource, ProgramDestination: programDestination,
		Destination: destination, SourceMint: sourceMint, DestinationMint: destinationMint, SourceTokenProgram: sourceProgram,
		DestinationTokenProgram: destinationProgram}, data...)
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
