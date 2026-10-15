package backyard

import (
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/hastra"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/solana-foundation/solana-go/v2"
)

// lanePolicy is how a lane's KLend legs are authorized on chain.
type lanePolicy byte

const (
	// policyBasic: the four shared basic families (USDC debt from Squads cash,
	// one constraint per lane in every family; policy.go basicPolicy).
	policyBasic lanePolicy = iota + 1
	// policyAuto: one autoPolicy literal of the lane's own carries every leg.
	policyAuto
	// policySplit: one split KLend policy per leg, capped at the vault cap.
	policySplit
)

// earnLane is one Earn Max loop Backyard holds installed on-chain policies
// for: its chain facts (route) and how it may be used. The route is a
// template: market, reserves, mints and programs, with no account of a vault;
// routeFor fills in the accounts one Squads vault owns.
type earnLane struct {
	route  RuntimeRoute
	policy lanePolicy
	// basicSwapLeg is a basic lane's constraint in the basic swap families.
	basicSwapLeg byte
	// jupiterDexes keeps the lane's Jupiter quotes inside the venues it was
	// reviewed against; empty quotes every venue.
	jupiterDexes string
	// exitOnly: a held position on the lane is still observed, valued and
	// exited (withdrawals, unwinds, payoff), but the lane is never scored,
	// entered, funded anew or levered.
	exitOnly bool
	// initializer: the lane's obligation is created through an installed
	// initializer policy. Otherwise the obligation already exists on chain
	// (owner KLend, verified 2026-10-10) and no initializer policy is
	// installed: a missing obligation holds.
	initializer bool
	// overflowOf names the lane this one is the overflow of: it shares that
	// lane's collateral reserve and custody, is never a selector or tranche
	// destination, and is entered only by an overflow move from it when that
	// lane's next leverage step finds no borrow room (selector_overflow.go).
	overflowOf string
	// maxLevel is the lane's cap: the highest leverage level it may hold
	// (leverage_level.go), set per lane from its lender limits and its
	// borrow pool (owner decision 2026-10-14); a level of the ladder above 1,
	// checked when the list is built. Its LTV must sit under the lane's hard
	// LTV (hardLTVBPS, from the chain at observation): a lane whose hard LTV
	// is not above its cap's LTV neither enters nor levers up, and still
	// delevers at its hard LTV (target.go).
	maxLevel float64
}

// earnLanes is the one list of Backyard's lanes and their chain facts, which
// every vault shares (owner decision 2026-10-13). Every
// lane predicate (scored, entered, funded, levered, observed, exited), the
// account view, the policy literals and the observer's pinned reserves read
// it. Registry order matters: a custody two lanes share belongs to the first
// of them, and a flat vault's view prefers the earliest lane. Earn Max holds
// positions and moves to whichever lane gives the best achievable net APY;
// economics decide, not a hand list (owner decision 2026-10-10: every
// policy-covered loop is checked).
var earnLanes = laneCapsOnLadder([]earnLane{
	// Maple: 88% max LTV, ~90% liquidation; its USDC borrow pool is thin and
	// its spread often negative, so 1.5x (33.3%).
	{route: mapleSyrupUSDCUSDCTemplate, policy: policyBasic, basicSwapLeg: basicSwapSyrup, jupiterDexes: "Manifest", initializer: true, maxLevel: 1.5},
	// OnRe: 66% max LTV, 75% liquidation (hard LTV 63%, where a down step
	// still has withdraw room); 2.5x = 60%.
	{route: onreONycUSDCRouteTemplate, policy: policyBasic, basicSwapLeg: basicSwapONycPrime, initializer: true, maxLevel: 2.5},
	// AUTO: 78% max LTV, 80% liquidation (hard LTV 70%); 2.5x = 60%, 3x
	// (66.7%) once a few weeks of data support it.
	{route: autoAUTOPYUSDTemplate, policy: policyAuto, initializer: true, maxLevel: 2.5},
	// Prime: not researched past today's 1.75x.
	{route: primePRIMEUSDCTemplate, policy: policyBasic, basicSwapLeg: basicSwapONycPrime, initializer: true, maxLevel: 1.75},
	{route: primePRIMEPYUSDTemplate, policy: policySplit, maxLevel: 1.75},
	{route: primePRIMEUSDSTemplate, policy: policySplit, maxLevel: 1.75},
	// Ethena is exit-only. Kamino's yield feed reports ~4.75% underlyingApy for
	// USDe, but USDe's price does not grow, so a holder never earns it; and
	// both its debt reserves sit at borrowLimit 0. Its installed policies stay
	// installed so a held position stays observable and exitable.
	{route: ethenaUSDePYUSDTemplate, policy: policySplit, exitOnly: true, maxLevel: 1.75},
	// AUTO's overflow lane (owner decision 2026-10-11): when AUTO/AUTO/PYUSD
	// has no PYUSD borrow room, AUTO moves into this second obligation and
	// levers with wYLDS, converted through Hastra.
	// Its cap stays at today's 1.75x.
	{route: autoAUTOWYLDSTemplate, policy: policyAuto, initializer: true, overflowOf: autoAUTOPYUSDTemplate.Lane, maxLevel: 1.75},
})

// laneCapsOnLadder is lanes, each cap a level of the ladder above 1; any
// other cap is a programming error and stops the binary at start.
func laneCapsOnLadder(lanes []earnLane) []earnLane {
	for _, l := range lanes {
		if i, ok := leverageRungIndex(l.maxLevel); !ok || i == 0 {
			panic(fmt.Sprintf("earn lane %s: cap %v is not a leverage level above 1", l.route.Lane, l.maxLevel))
		}
	}
	return lanes
}

func findEarnLane(lane string) (earnLane, bool) {
	for _, l := range earnLanes {
		if l.route.Lane == lane {
			return l, true
		}
	}
	return earnLane{}, false
}

// routeFor is the lane's chain facts with the accounts owner, a Squads
// vault, holds on it: its custody of each mint (an associated token account)
// and its one Multiply obligation (tag 1, id 0, seeded by the two mints) with
// that obligation's farm states.
func (l earnLane) routeFor(owner solana.PublicKey) RuntimeRoute {
	r := l.route
	k := &r.Kamino
	k.Vault = owner.String()
	k.Obligation = mustDerive(kamino.ObligationAddress(1, 0, owner, kaminoKey(k.Market), kaminoKey(k.CollateralMint), kaminoKey(k.DebtMint)))
	r.CollateralCustody = mustDerive(spl.AssociatedTokenAddress(owner, kaminoKey(k.CollateralMint), kaminoKey(r.CollateralTokenProgram)))
	r.DebtCustody = mustDerive(spl.AssociatedTokenAddress(owner, kaminoKey(k.DebtMint), kaminoKey(r.DebtTokenProgram)))
	if r.CollateralFarm != "" {
		r.ObligationCollateralFarm = mustDerive(kamino.ObligationFarmUserState(kaminoKey(r.CollateralFarm), kaminoKey(k.Obligation)))
	}
	if r.DebtFarm != "" {
		r.ObligationDebtFarm = mustDerive(kamino.ObligationFarmUserState(kaminoKey(r.DebtFarm), kaminoKey(k.Obligation)))
	}
	return r
}

// mustDerive is a derived address of a checked-in identity.
func mustDerive(key solana.PublicKey, err error) string {
	if err != nil {
		panic("derive Backyard address: " + err.Error())
	}
	return key.String()
}

// earnActiveLane reports a lane the selector scores and may enter, fund and
// lever.
func earnActiveLane(lane string) bool {
	l, ok := findEarnLane(lane)
	return ok && !l.exitOnly
}

// earnHeldLane reports a lane whose position is observed, valued and exited:
// every registry lane, exit-only ones included.
func earnHeldLane(lane string) bool {
	_, ok := findEarnLane(lane)
	return ok
}

// earnInitializerLane reports a registry lane whose obligation is created
// through an installed initializer policy.
func earnInitializerLane(lane string) bool {
	l, ok := findEarnLane(lane)
	return ok && l.initializer
}

// earnLaneIDs lists the registry lanes in registry order; exit-only lanes
// only when held is set.
func earnLaneIDs(held bool) []string {
	out := make([]string, 0, len(earnLanes))
	for _, l := range earnLanes {
		if held || !l.exitOnly {
			out = append(out, l.route.Lane)
		}
	}
	return out
}

// lanesWithPolicy are v's routes authorized by policy, in registry order.
func lanesWithPolicy(v *Vault, policy lanePolicy) []RuntimeRoute {
	var out []RuntimeRoute
	for i, l := range earnLanes {
		if l.policy == policy {
			out = append(out, v.routes[i])
		}
	}
	return out
}

// basicLane reports a registry lane served by the shared basic policy
// families (USDC debt, Squads USDC custody); every other registry lane swaps
// through the catalog edges.
func basicLane(lane string) bool {
	l, ok := findEarnLane(lane)
	return ok && l.policy == policyBasic
}

// autoRoute reports a lane that runs every KLend leg through one autoPolicy
// literal of its own.
func autoRoute(lane string) bool {
	l, ok := findEarnLane(lane)
	return ok && l.policy == policyAuto
}

// catalogJupiterRoute reports a registry lane that swaps through the catalog
// Jupiter policies: every registry lane outside the basic families.
func catalogJupiterRoute(lane string) bool {
	return earnHeldLane(lane) && !basicLane(lane)
}

// ObservationReserve is one Kamino reserve a registry lane borrows from or
// deposits into.
type ObservationReserve struct {
	Market, MarketName, Reserve, LiquidityMint string
}

// ObservationReserves are the registry lanes' collateral and debt reserves,
// each once, in registry order: what the Kamino reserve observer pins for
// Backyard. A market is named by its lanes' protocol.
func ObservationReserves() []ObservationReserve {
	var out []ObservationReserve
	seen := map[string]bool{}
	for _, l := range earnLanes {
		k := l.route.Kamino
		for _, r := range [][2]string{{k.CollateralReserve, k.CollateralMint}, {k.DebtReserve, k.DebtMint}} {
			if !seen[r[0]] {
				seen[r[0]] = true
				out = append(out, ObservationReserve{Market: k.Market, MarketName: l.route.Protocol + " Market", Reserve: r[0], LiquidityMint: r[1]})
			}
		}
	}
	return out
}

// usdcReference is the KLend USDC reserve Backyard prices USDC by: the
// Prime/PRIME/USDC lane's debt reserve.
var usdcReference = primePRIMEUSDCTemplate.Kamino

var mapleSyrupUSDCUSDCTemplate = RuntimeRoute{
	Lane: SelectedRouteID, Protocol: "Maple", CollateralSymbol: "syrupUSDC", DebtSymbol: "USDC",
	Kamino: KaminoObservationConfig{
		Program:           kamino.ProgramID.String(),
		Market:            "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y",
		MarketAuthority:   "6QbtpY2jDNcncRFmVf343NThnCdaY8gCAsYATPnYQR9g",
		CollateralReserve: "AwCyCPZYJSZ93xcVKNK7jR8e1BHzJXq1D4bReNuh9woY",
		CollateralMint:    "AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj",
		DebtReserve:       "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo",
		DebtMint:          bridgeUSDC,
	},
	CollateralLiquiditySupply: "8Se5SK1Tty2bH4EQVrKW8hwr9Lc9E2cEbkaN59DpcB6i",
	CollateralReceiptMint:     "9gQ8M4WiFepY9skYntJZ5N3joa3RByiPqao61gMfmGMu",
	CollateralReceiptSupply:   "21GK6yHS3MKhTnF5pN5FuSmnpLiyPXTDrpxxbqMEoX58",
	DebtLiquiditySupply:       "BBcwMNSMyhhBnYE9pevEvkxKHGzTafMP9v3j7Kk7nAWM",
	DebtFeeReceiver:           "HH7GLnRcGHJrdkEueVVj7mccNUjnSeWobDmtu9cHLkJV",
	CollateralTokenProgram:    classicTokenProgram, DebtTokenProgram: classicTokenProgram,
	DebtFarm: "87gUNr8LwYJCT25HjPEHnrfBBjwEMAjfqCfnKcJNqy9Y",
}

var onreONycUSDCRouteTemplate = RuntimeRoute{
	Lane: onreONycUSDC, Protocol: "OnRe", CollateralSymbol: "ONyc", DebtSymbol: "USDC",
	Kamino: KaminoObservationConfig{
		Program:           kamino.ProgramID.String(),
		Market:            "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8",
		MarketAuthority:   "FsvTiXTUFDc4aLbrov4PrvDTjXCWCniL1dxTUkZ1T2ss",
		CollateralReserve: "6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ",
		CollateralMint:    "5Y8NV33Vv7WbnLfq3zBcKSdYPrk7g2KoiQoe7M2tcxp5",
		DebtReserve:       "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z",
		DebtMint:          bridgeUSDC,
	},
	CollateralLiquiditySupply: "9YuHgsPVGgWrkpsaRZmeZCV2uXweMEn6TEAcusQKRjgG",
	CollateralReceiptMint:     "CtzvqjvpxJDXyraDjP2QrEr8b1xvGvxADRV7w29qrmxd",
	CollateralReceiptSupply:   "2c42iUaea3QVLvSPQHUBZBwqdvpiQo5vmeMePq9qx8eo",
	DebtLiquiditySupply:       "8BkQTZsT8ssKMU643De4iiV5Wf3pENdUFTsdtHPueKjB",
	DebtFeeReceiver:           "5iLRav31Y7DJwM6bZ7s92jqvV3zd1wZMcp4mYeKXh8cj",
	CollateralTokenProgram:    classicTokenProgram, DebtTokenProgram: classicTokenProgram,
	DebtFarm: "7vNfe1qX8iDxP5p3A4fosrjLqdn1YjmmGcZZkG2b4APF",
}

// The Prime market's PRIME collateral reserve and custody are shared by its
// three debt lanes.
var primePRIMEUSDCTemplate = RuntimeRoute{
	Lane: PhaseOneLaneID, Protocol: "Prime", CollateralSymbol: "PRIME", DebtSymbol: "USDC",
	Kamino: KaminoObservationConfig{
		Program: kamino.ProgramID.String(),
		Market:  "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA", MarketAuthority: "9SLBVnPz8dRGvafST6zNBZYSSt3HtdU68XQLGR13t3uM",
		CollateralReserve: "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh", CollateralMint: "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7",
		DebtReserve: "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu", DebtMint: bridgeUSDC,
	},
	CollateralLiquiditySupply: "FkSkbRU5A6JXRXo5uaFwCS7jQ6jHYa1DxFtfpXfTz352", CollateralReceiptMint: "FMKBCGqipyj5dm9C58Rb9ZWYeneDzrxd3YaL6amgZ8gW",
	CollateralReceiptSupply: "Eg4wKFWc8aGfAqrcmYu3paz2afY5VqJMo17K95Y4VqFN", DebtLiquiditySupply: "H6JUwz8c61eQnYUx8avGXydKztKPyGvgWAUjmZUPS3BC",
	DebtFeeReceiver: "BzSw9sWTxUumr2wHhDiezkaLy3QZQS1KT4a9Fz8GvAQ6", CollateralTokenProgram: classicTokenProgram, DebtTokenProgram: classicTokenProgram,
}

// Finalized account review at slot 444525169 matches all four installed
// policy vectors for each Prime sibling, with no farm substitution.
var primePRIMEPYUSDTemplate = func() RuntimeRoute {
	r := primePRIMEUSDCTemplate
	r.Lane, r.DebtSymbol = "Prime/PRIME/PYUSD", "PYUSD"
	r.Kamino.DebtReserve, r.Kamino.DebtMint = "3ZUAwhEtK8XWfK4fy98z4yoptm4GeyeAu21L11HPXaZ5", pyusdMint
	r.DebtTokenProgram = token2022Program
	r.DebtLiquiditySupply, r.DebtFeeReceiver = "4LF3i8grZPRbk8d6gXvzRux4rYjGd5AmqrpLLYFpPKKt", "4b9U55muKtwx9RimJSuztvyZaKWkmaoferVexgvxrYJr"
	return r
}()

var primePRIMEUSDSTemplate = func() RuntimeRoute {
	r := primePRIMEUSDCTemplate
	r.Lane, r.DebtSymbol = "Prime/PRIME/USDS", "USDS"
	r.Kamino.DebtReserve, r.Kamino.DebtMint = "7SzMWArC8WAenndXFmRyfvcvrNPodqUFkmPrmmoRZvn4", "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA"
	r.DebtLiquiditySupply, r.DebtFeeReceiver = "5tP1kDJBYnjtrpUaRQhsrU1Y28ahiJVjz8p9mbqJFpz5", "DjmdtvsvctUXCZ32y6UGdCEvXPTds6Ci7LFnVhw5HaQY"
	return r
}()

// pyusdMint is PYUSD, which every PYUSD-debt lane borrows into the vault's
// one PYUSD account.
const pyusdMint = "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"

var autoAUTOPYUSDTemplate = RuntimeRoute{
	Lane: "AUTO/AUTO/PYUSD", Protocol: "AUTO", CollateralSymbol: "AUTO", DebtSymbol: "PYUSD",
	Kamino: KaminoObservationConfig{
		Program:           kamino.ProgramID.String(),
		Market:            "Btu8835QDYgdTnMJJBSidbfQhrZzryZbMhCpty6h6Xdk",
		MarketAuthority:   "2eyLWowHqsWNuRavNc5g6e8NZiypgJTHEvW2hMum3BNS",
		CollateralReserve: "G85AgoBdW8zSQBq5i4E8aBLCDdRYGgK44CzU1d1NdBzX",
		CollateralMint:    "GNE6oDS6jHrfaV3GQVVCCp37fDnT7PiPuewMKBj2bqNm",
		DebtReserve:       "6A8D3ExQ4CdiZTBmij7MScUeKsgs6mSHksYzJbiY61FM",
		DebtMint:          pyusdMint,
	},
	CollateralLiquiditySupply: "7W7sahTJjE7D4UPqmp8i4fd6AUm1hFjnUqNSjY78sN5F",
	CollateralReceiptMint:     "Cjh2wuFnuSiFH6j6Q8f8YvkrHgt3LYhAZhz6JbtN7xfQ",
	CollateralReceiptSupply:   "8no8zpNDwCXLjhPr5Gb9UWHfEMUxmNFUHAjbR1WDDdvc",
	DebtLiquiditySupply:       "FZPPq1U7BqpqAnV7naHThsdumZiUWJY4Q4784HQ8a3KM",
	DebtFeeReceiver:           "HFYjFt3AYazic97CdXwZctGGeAuarCud6h6baEStg8Nh",
	CollateralTokenProgram:    classicTokenProgram, DebtTokenProgram: token2022Program,
}

// autoAUTOWYLDSTemplate shares AUTO/AUTO/PYUSD's market, collateral reserve and
// collateral custody (the vault holds one AUTO account for both lanes) and
// borrows wYLDS from reserve 34GJ…, a classic SPL Token mint with no farms.
// Its obligation (tag 1, seeds AUTO and wYLDS), its debt custody (the vault's
// wYLDS associated token account) and the reserve's liquidity supply and fee
// vault are PDAs, derived here rather than written down.
var autoAUTOWYLDSTemplate = func() RuntimeRoute {
	r := autoAUTOPYUSDTemplate
	r.Lane, r.DebtSymbol = autoAUTOWYLDSLane, "wYLDS"
	r.Kamino.DebtReserve, r.Kamino.DebtMint = "34GJjXf7jGTdNL8Yw1HtqWSVvfX6JTsft3UsuFFUL7VU", hastra.WYLDSMint.String()
	r.DebtTokenProgram = classicTokenProgram
	reserve := kaminoKey(r.Kamino.DebtReserve)
	r.DebtLiquiditySupply = mustDerive(kamino.ReserveLiquiditySupply(reserve))
	r.DebtFeeReceiver = mustDerive(kamino.ReserveFeeVault(reserve))
	return r
}()

// onreONycUSDC is the OnRe lane.
const onreONycUSDC = "OnRe/ONyc/USDC"

// autoAUTOWYLDSLane is AUTO's overflow lane: AUTO collateral levered with
// wYLDS term debt in AUTO's market, through its own obligation.
const autoAUTOWYLDSLane = "AUTO/AUTO/wYLDS"

var ethenaUSDePYUSDTemplate = RuntimeRoute{
	Lane: "Ethena/USDe/PYUSD", Protocol: "Ethena", CollateralSymbol: "USDe", DebtSymbol: "PYUSD",
	Kamino: KaminoObservationConfig{
		Program:           kamino.ProgramID.String(),
		Market:            "BJnbcRHqvppTyGesLzWASGKnmnF1wq9jZu6ExrjT7wvF",
		MarketAuthority:   "GuWEkEJb5bh8Ai2gaYmZWMTUq8MrFeoaDZ89BrQfB1FZ",
		CollateralReserve: "2erD9GTGcaQbLsVSQweg3HvMpfKxScmz95raWv8H4iPN",
		CollateralMint:    "DEkqHyPN7GMRJ5cArtQFAWefqbZb33Hyf6s5iCwjEonT",
		DebtReserve:       "EDf6dGbVnCCABbNhE3mp5i1jV2JhDAVmTWb1ztij1Yhs",
		DebtMint:          pyusdMint,
	},
	CollateralLiquiditySupply: "BcqeM19i3njWEVPmQse2NAZGnTsTVdfuNYXWfmujoLff",
	CollateralReceiptMint:     "8DQhJtZVPLLSqFkaQqZDoRFRPD68xeHQkEQ69Gz7VMaK",
	CollateralReceiptSupply:   "2KHE9hunJFBhkjT355cBDfXZmptk1mx2W5NUgc27N68k",
	DebtLiquiditySupply:       "8Am2NKsHozvH9J1Ub5ntdLhGySUsD5P4yYBkcoPQ6NXQ",
	DebtFeeReceiver:           "GtXzva7jBAk2Khs8h6fFXLw6vjb82vQaSZRUz6FWMrUv",
	CollateralTokenProgram:    classicTokenProgram, DebtTokenProgram: token2022Program,
}
