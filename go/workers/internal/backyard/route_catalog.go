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
// for: its chain facts (route) and how it may be used.
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
}

// earnLanes is the one list of Backyard's lanes and their chain facts. Every
// lane predicate (scored, entered, funded, levered, observed, exited), the
// account view, the policy literals and the observer's pinned reserves read
// it. Registry order matters: a custody two lanes share belongs to the first
// of them, and a flat vault's view prefers the earliest lane. Earn Max holds
// positions and moves to whichever lane gives the best achievable net APY;
// economics decide, not a hand list (owner decision 2026-10-10: every
// policy-covered loop is checked).
var earnLanes = []earnLane{
	{route: mapleSyrupUSDCUSDC, policy: policyBasic, basicSwapLeg: basicSwapSyrup, jupiterDexes: "Manifest", initializer: true},
	{route: onreONycUSDCRoute, policy: policyBasic, basicSwapLeg: basicSwapONycPrime, initializer: true},
	{route: autoAUTOPYUSD, policy: policyAuto, initializer: true},
	{route: primePRIMEUSDC, policy: policyBasic, basicSwapLeg: basicSwapONycPrime, initializer: true},
	{route: primePRIMEPYUSD, policy: policySplit},
	{route: primePRIMEUSDS, policy: policySplit},
	// Ethena is exit-only. Kamino's yield feed reports ~4.75% underlyingApy for
	// USDe, but USDe's price does not grow, so a holder never earns it; and
	// both its debt reserves sit at borrowLimit 0. Its installed policies stay
	// installed so a held position stays observable and exitable.
	{route: ethenaUSDePYUSD, policy: policySplit, exitOnly: true},
	// AUTO's overflow lane (owner decision 2026-10-11): when AUTO/AUTO/PYUSD
	// has no PYUSD borrow room, AUTO moves into this second obligation and
	// levers with wYLDS, converted through Hastra.
	{route: autoAUTOWYLDS, policy: policyAuto, initializer: true, overflowOf: autoAUTOPYUSD.Lane},
}

func findEarnLane(lane string) (earnLane, bool) {
	for _, l := range earnLanes {
		if l.route.Lane == lane {
			return l, true
		}
	}
	return earnLane{}, false
}

// runtimeRoute is lane's chain facts from the registry.
func runtimeRoute(lane string) (RuntimeRoute, error) {
	l, ok := findEarnLane(lane)
	if !ok {
		return RuntimeRoute{}, fmt.Errorf("runtime lane %q is not installed", lane)
	}
	return l.route, nil
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

// lanesWithPolicy are the registry routes authorized by policy, in registry
// order.
func lanesWithPolicy(policy lanePolicy) []RuntimeRoute {
	var out []RuntimeRoute
	for _, l := range earnLanes {
		if l.policy == policy {
			out = append(out, l.route)
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
var usdcReference = primePRIMEUSDC.Kamino

var mapleSyrupUSDCUSDC = RuntimeRoute{
	Lane: SelectedRouteID, Protocol: "Maple", CollateralSymbol: "syrupUSDC", DebtSymbol: "USDC",
	Kamino: KaminoObservationConfig{
		Program: kamino.ProgramID.String(), Vault: bridgeVault,
		Market:            "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y",
		MarketAuthority:   "6QbtpY2jDNcncRFmVf343NThnCdaY8gCAsYATPnYQR9g",
		Obligation:        "Gtwj2FNuiPoV2mGLC5SpHZ9PCmDrHHKaHXtacRaqm8vT",
		CollateralReserve: "AwCyCPZYJSZ93xcVKNK7jR8e1BHzJXq1D4bReNuh9woY",
		CollateralMint:    "AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj",
		DebtReserve:       "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo",
		DebtMint:          bridgeUSDC,
	},
	CollateralCustody:         "CYwM28WSoYp85HrQGuaVpWy2JhKH6JJah4m65DSWUNiN",
	DebtCustody:               bridgeSquadsATA,
	CollateralLiquiditySupply: "8Se5SK1Tty2bH4EQVrKW8hwr9Lc9E2cEbkaN59DpcB6i",
	CollateralReceiptMint:     "9gQ8M4WiFepY9skYntJZ5N3joa3RByiPqao61gMfmGMu",
	CollateralReceiptSupply:   "21GK6yHS3MKhTnF5pN5FuSmnpLiyPXTDrpxxbqMEoX58",
	DebtLiquiditySupply:       "BBcwMNSMyhhBnYE9pevEvkxKHGzTafMP9v3j7Kk7nAWM",
	DebtFeeReceiver:           "HH7GLnRcGHJrdkEueVVj7mccNUjnSeWobDmtu9cHLkJV",
	CollateralTokenProgram:    classicTokenProgram, DebtTokenProgram: classicTokenProgram,
	DebtFarm:           "87gUNr8LwYJCT25HjPEHnrfBBjwEMAjfqCfnKcJNqy9Y",
	ObligationDebtFarm: "CcUorNoacydFVu7SHmhsA1qi9CcEu8K5YFvuS8unAzgr",
}

var onreONycUSDCRoute = RuntimeRoute{
	Lane: onreONycUSDC, Protocol: "OnRe", CollateralSymbol: "ONyc", DebtSymbol: "USDC",
	Kamino: KaminoObservationConfig{
		Program: kamino.ProgramID.String(), Vault: bridgeVault,
		Market:            "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8",
		MarketAuthority:   "FsvTiXTUFDc4aLbrov4PrvDTjXCWCniL1dxTUkZ1T2ss",
		Obligation:        "4LnCFir7Qc99GhjGHLcwtkfweyAMu37u5QE1zTupKsei",
		CollateralReserve: "6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ",
		CollateralMint:    "5Y8NV33Vv7WbnLfq3zBcKSdYPrk7g2KoiQoe7M2tcxp5",
		DebtReserve:       "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z",
		DebtMint:          bridgeUSDC,
	},
	CollateralCustody:         "AVX9wxDTk639eZ4KaiMA7LrLhXe7Lg6DaDDVRa1Q7Ji3",
	DebtCustody:               bridgeSquadsATA,
	CollateralLiquiditySupply: "9YuHgsPVGgWrkpsaRZmeZCV2uXweMEn6TEAcusQKRjgG",
	CollateralReceiptMint:     "CtzvqjvpxJDXyraDjP2QrEr8b1xvGvxADRV7w29qrmxd",
	CollateralReceiptSupply:   "2c42iUaea3QVLvSPQHUBZBwqdvpiQo5vmeMePq9qx8eo",
	DebtLiquiditySupply:       "8BkQTZsT8ssKMU643De4iiV5Wf3pENdUFTsdtHPueKjB",
	DebtFeeReceiver:           "5iLRav31Y7DJwM6bZ7s92jqvV3zd1wZMcp4mYeKXh8cj",
	CollateralTokenProgram:    classicTokenProgram, DebtTokenProgram: classicTokenProgram,
	DebtFarm:           "7vNfe1qX8iDxP5p3A4fosrjLqdn1YjmmGcZZkG2b4APF",
	ObligationDebtFarm: "nMqFZFPQsNwot49QAD1B76LxNV7qRG1tnbkXyTjbUAD",
}

// The Prime market's PRIME collateral reserve and custody are shared by its
// three debt lanes.
var primePRIMEUSDC = RuntimeRoute{
	Lane: PhaseOneLaneID, Protocol: "Prime", CollateralSymbol: "PRIME", DebtSymbol: "USDC",
	Kamino: KaminoObservationConfig{
		Program: kamino.ProgramID.String(), Vault: bridgeVault,
		Market: "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA", MarketAuthority: "9SLBVnPz8dRGvafST6zNBZYSSt3HtdU68XQLGR13t3uM",
		Obligation:        "9suFBUhW7D7jN141mKR49Hn1WYDHEsRnPiGhxxm7RFkv",
		CollateralReserve: "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh", CollateralMint: "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7",
		DebtReserve: "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu", DebtMint: bridgeUSDC,
	},
	CollateralCustody: "DnBnX19kFyCP3Kdhkq7uEJ6juCYEaiS6jZMSXbfCXzct", DebtCustody: bridgeSquadsATA,
	CollateralLiquiditySupply: "FkSkbRU5A6JXRXo5uaFwCS7jQ6jHYa1DxFtfpXfTz352", CollateralReceiptMint: "FMKBCGqipyj5dm9C58Rb9ZWYeneDzrxd3YaL6amgZ8gW",
	CollateralReceiptSupply: "Eg4wKFWc8aGfAqrcmYu3paz2afY5VqJMo17K95Y4VqFN", DebtLiquiditySupply: "H6JUwz8c61eQnYUx8avGXydKztKPyGvgWAUjmZUPS3BC",
	DebtFeeReceiver: "BzSw9sWTxUumr2wHhDiezkaLy3QZQS1KT4a9Fz8GvAQ6", CollateralTokenProgram: classicTokenProgram, DebtTokenProgram: classicTokenProgram,
}

// Finalized account review at slot 444525169 matches all four installed
// policy vectors for each Prime sibling, with no farm substitution.
var primePRIMEPYUSD = func() RuntimeRoute {
	r := primePRIMEUSDC
	r.Lane, r.DebtSymbol = "Prime/PRIME/PYUSD", "PYUSD"
	r.Kamino.Obligation = "GAnakFSJAhNMrH3B8PRLxHcEtWVL21xyALRiWx3baS5t"
	r.Kamino.DebtReserve, r.Kamino.DebtMint = "3ZUAwhEtK8XWfK4fy98z4yoptm4GeyeAu21L11HPXaZ5", pyusdMint
	r.DebtCustody, r.DebtTokenProgram = pyusdCustody, token2022Program
	r.DebtLiquiditySupply, r.DebtFeeReceiver = "4LF3i8grZPRbk8d6gXvzRux4rYjGd5AmqrpLLYFpPKKt", "4b9U55muKtwx9RimJSuztvyZaKWkmaoferVexgvxrYJr"
	return r
}()

var primePRIMEUSDS = func() RuntimeRoute {
	r := primePRIMEUSDC
	r.Lane, r.DebtSymbol = "Prime/PRIME/USDS", "USDS"
	r.Kamino.Obligation = "6aqRhAxxjxdoAzgsEMrCCKCEEYMoLDLRKTu5t8nRuyYu"
	r.Kamino.DebtReserve, r.Kamino.DebtMint = "7SzMWArC8WAenndXFmRyfvcvrNPodqUFkmPrmmoRZvn4", "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA"
	r.DebtCustody = "5LR9AdS7XwJjQXWkKNBXNibGNkFXqe7T2JXU2oBBwknV"
	r.DebtLiquiditySupply, r.DebtFeeReceiver = "5tP1kDJBYnjtrpUaRQhsrU1Y28ahiJVjz8p9mbqJFpz5", "DjmdtvsvctUXCZ32y6UGdCEvXPTds6Ci7LFnVhw5HaQY"
	return r
}()

// pyusdMint and pyusdCustody are PYUSD and the vault's one PYUSD account,
// shared by every PYUSD-debt lane.
const (
	pyusdMint    = "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"
	pyusdCustody = "J4YFQzxhQ3pht2RRYes5yv1spPYBqvHzxn4zMX7iriHn"
)

var autoAUTOPYUSD = RuntimeRoute{
	Lane: "AUTO/AUTO/PYUSD", Protocol: "AUTO", CollateralSymbol: "AUTO", DebtSymbol: "PYUSD",
	Kamino: KaminoObservationConfig{
		Program: kamino.ProgramID.String(), Vault: bridgeVault,
		Market:            "Btu8835QDYgdTnMJJBSidbfQhrZzryZbMhCpty6h6Xdk",
		MarketAuthority:   "2eyLWowHqsWNuRavNc5g6e8NZiypgJTHEvW2hMum3BNS",
		Obligation:        "DJhTPmvAh5xf4X3Cwfchn43psfCgXozRoNXUMJAiDS41",
		CollateralReserve: "G85AgoBdW8zSQBq5i4E8aBLCDdRYGgK44CzU1d1NdBzX",
		CollateralMint:    "GNE6oDS6jHrfaV3GQVVCCp37fDnT7PiPuewMKBj2bqNm",
		DebtReserve:       "6A8D3ExQ4CdiZTBmij7MScUeKsgs6mSHksYzJbiY61FM",
		DebtMint:          pyusdMint,
	},
	CollateralCustody:         "9tDh95ofQ7B83bHAou1XJGJNnfRTs3hLfX8uyKW6u97G",
	DebtCustody:               pyusdCustody,
	CollateralLiquiditySupply: "7W7sahTJjE7D4UPqmp8i4fd6AUm1hFjnUqNSjY78sN5F",
	CollateralReceiptMint:     "Cjh2wuFnuSiFH6j6Q8f8YvkrHgt3LYhAZhz6JbtN7xfQ",
	CollateralReceiptSupply:   "8no8zpNDwCXLjhPr5Gb9UWHfEMUxmNFUHAjbR1WDDdvc",
	DebtLiquiditySupply:       "FZPPq1U7BqpqAnV7naHThsdumZiUWJY4Q4784HQ8a3KM",
	DebtFeeReceiver:           "HFYjFt3AYazic97CdXwZctGGeAuarCud6h6baEStg8Nh",
	CollateralTokenProgram:    classicTokenProgram, DebtTokenProgram: token2022Program,
}

// autoAUTOWYLDS shares AUTO/AUTO/PYUSD's market, collateral reserve and
// collateral custody (the vault holds one AUTO account for both lanes) and
// borrows wYLDS from reserve 34GJ…, a classic SPL Token mint with no farms.
// Its obligation (tag 1, seeds AUTO and wYLDS), its debt custody (the vault's
// wYLDS associated token account) and the reserve's liquidity supply and fee
// vault are PDAs, derived here rather than written down.
var autoAUTOWYLDS = func() RuntimeRoute {
	r := autoAUTOPYUSD
	r.Lane, r.DebtSymbol = autoAUTOWYLDSLane, "wYLDS"
	r.Kamino.DebtReserve, r.Kamino.DebtMint = "34GJjXf7jGTdNL8Yw1HtqWSVvfX6JTsft3UsuFFUL7VU", hastra.WYLDSMint.String()
	r.DebtTokenProgram = classicTokenProgram
	vault, market, reserve := kaminoKey(bridgeVault), kaminoKey(r.Kamino.Market), kaminoKey(r.Kamino.DebtReserve)
	derive := func(key solana.PublicKey, err error) string {
		if err != nil {
			panic(err)
		}
		return key.String()
	}
	r.Kamino.Obligation = derive(kamino.ObligationAddress(1, 0, vault, market, kaminoKey(r.Kamino.CollateralMint), hastra.WYLDSMint))
	r.DebtCustody = derive(spl.AssociatedTokenAddress(vault, hastra.WYLDSMint, solana.TokenProgramID))
	r.DebtLiquiditySupply = derive(kamino.ReserveLiquiditySupply(reserve))
	r.DebtFeeReceiver = derive(kamino.ReserveFeeVault(reserve))
	return r
}()

// onreONycUSDC is the OnRe lane.
const onreONycUSDC = "OnRe/ONyc/USDC"

// autoAUTOWYLDSLane is AUTO's overflow lane: AUTO collateral levered with
// wYLDS term debt in AUTO's market, through its own obligation.
const autoAUTOWYLDSLane = "AUTO/AUTO/wYLDS"

var ethenaUSDePYUSD = RuntimeRoute{
	Lane: "Ethena/USDe/PYUSD", Protocol: "Ethena", CollateralSymbol: "USDe", DebtSymbol: "PYUSD",
	Kamino: KaminoObservationConfig{
		Program: kamino.ProgramID.String(), Vault: bridgeVault,
		Market:            "BJnbcRHqvppTyGesLzWASGKnmnF1wq9jZu6ExrjT7wvF",
		MarketAuthority:   "GuWEkEJb5bh8Ai2gaYmZWMTUq8MrFeoaDZ89BrQfB1FZ",
		Obligation:        "5CDZVkkC9wH3FTo4xy679qorb4xMt5cHf2nhsRcTUsQr",
		CollateralReserve: "2erD9GTGcaQbLsVSQweg3HvMpfKxScmz95raWv8H4iPN",
		CollateralMint:    "DEkqHyPN7GMRJ5cArtQFAWefqbZb33Hyf6s5iCwjEonT",
		DebtReserve:       "EDf6dGbVnCCABbNhE3mp5i1jV2JhDAVmTWb1ztij1Yhs",
		DebtMint:          pyusdMint,
	},
	CollateralCustody:         "6wSE9RKCReDiDiW4tSRiieFHSaVS2gTMFCVBB8mimtVk",
	DebtCustody:               pyusdCustody,
	CollateralLiquiditySupply: "BcqeM19i3njWEVPmQse2NAZGnTsTVdfuNYXWfmujoLff",
	CollateralReceiptMint:     "8DQhJtZVPLLSqFkaQqZDoRFRPD68xeHQkEQ69Gz7VMaK",
	CollateralReceiptSupply:   "2KHE9hunJFBhkjT355cBDfXZmptk1mx2W5NUgc27N68k",
	DebtLiquiditySupply:       "8Am2NKsHozvH9J1Ub5ntdLhGySUsD5P4yYBkcoPQ6NXQ",
	DebtFeeReceiver:           "GtXzva7jBAk2Khs8h6fFXLw6vjb82vQaSZRUz6FWMrUv",
	CollateralTokenProgram:    classicTokenProgram, DebtTokenProgram: token2022Program,
}
