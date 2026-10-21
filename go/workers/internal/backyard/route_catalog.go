package backyard

import (
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/hastra"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/solana-foundation/solana-go/v2"
)

type lanePolicy byte

const (
	policyBasic lanePolicy = iota + 1
	policyAuto
	policySplit
)

type earnLane struct {
	route        RuntimeRoute
	policy       lanePolicy
	basicSwapLeg byte
	jupiterDexes string
	exitOnly     bool
	initializer  bool
	overflowOf   string
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
// every vault shares. Every lane predicate (scored, entered, funded, levered,
// observed, exited), the account view, the policy literals and the observer's
// pinned reserves read it. Registry order matters: a custody two lanes share
// belongs to the first of them, and a flat vault's view prefers the earliest
// lane.
var earnLanes = laneCapsOnLadder([]earnLane{
	// Each cap is the highest level whose LTV sits under the lane's hard LTV
	// (owner decision 2026-10-15: every lane to its most reasonable point);
	// lender limits read from the reserves on 2026-10-15. The spread rule, not
	// the cap, keeps a thin or negative-spread lane low.
	// Maple: 88% max LTV, 90% liquidation (hard LTV 80%); 3x = 66.7%. Its
	// USDC borrow pool is thin and its spread often under 2.5 pts, so the
	// rule rarely takes it past 1.75x.
	{route: mapleSyrupUSDCUSDCTemplate, policy: policyBasic, basicSwapLeg: basicSwapSyrup, jupiterDexes: "Manifest", initializer: true, maxLevel: 3},
	// OnRe: 66% max LTV, 75% liquidation (hard LTV 63%, where a down step
	// still has withdraw room); 2.5x = 60%. 3x (66.7%) sits over the hard
	// LTV, so 2.5x is its ceiling.
	{route: onreONycUSDCRouteTemplate, policy: policyBasic, basicSwapLeg: basicSwapONycPrime, initializer: true, maxLevel: 2.5},
	// AUTO: 78% max LTV, 80% liquidation (hard LTV 70%); 3x = 66.7%.
	{route: autoAUTOPYUSDTemplate, policy: policyAuto, initializer: true, maxLevel: 3},
	// Prime: 88% max LTV, 91% liquidation (hard LTV 81%); 3x = 66.7%. Its
	// spreads sit near or under zero, so the rule holds it at 1x.
	{route: primePRIMEUSDCTemplate, policy: policyBasic, basicSwapLeg: basicSwapONycPrime, initializer: true, maxLevel: 3},
	{route: primePRIMEPYUSDTemplate, policy: policySplit, maxLevel: 3},
	{route: primePRIMEUSDSTemplate, policy: policySplit, maxLevel: 3},
	{route: ethenaUSDePYUSDTemplate, policy: policySplit, exitOnly: true, maxLevel: 1.75},
	{route: autoAUTOWYLDSTemplate, policy: policyAuto, initializer: true, overflowOf: autoAUTOPYUSDTemplate.Lane, maxLevel: 3},
})

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

func (l earnLane) routeFor(owner solana.PublicKey) RuntimeRoute {
	r := l.route
	k := &r.Kamino
	k.Vault = owner.String()
	k.Obligation = mustDerive(kamino.ObligationAddress(kamino.MultiplyObligationTag, kamino.DefaultObligationID, owner, kaminoKey(k.Market), kaminoKey(k.CollateralMint), kaminoKey(k.DebtMint)))
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

func mustDerive(key solana.PublicKey, err error) string {
	if err != nil {
		panic("derive Backyard address: " + err.Error())
	}
	return key.String()
}

func earnActiveLane(lane string) bool {
	l, ok := findEarnLane(lane)
	return ok && !l.exitOnly
}

func earnHeldLane(lane string) bool {
	_, ok := findEarnLane(lane)
	return ok
}

func earnInitializerLane(lane string) bool {
	l, ok := findEarnLane(lane)
	return ok && l.initializer
}

func earnLaneIDs(held bool) []string {
	out := make([]string, 0, len(earnLanes))
	for _, l := range earnLanes {
		if held || !l.exitOnly {
			out = append(out, l.route.Lane)
		}
	}
	return out
}

func lanesWithPolicy(v *Vault, policy lanePolicy) []RuntimeRoute {
	var out []RuntimeRoute
	for i, l := range earnLanes {
		if l.policy == policy {
			out = append(out, v.routes[i])
		}
	}
	return out
}

func basicLane(lane string) bool {
	l, ok := findEarnLane(lane)
	return ok && l.policy == policyBasic
}

func autoRoute(lane string) bool {
	l, ok := findEarnLane(lane)
	return ok && l.policy == policyAuto
}

func catalogJupiterRoute(lane string) bool {
	return earnHeldLane(lane) && !basicLane(lane)
}

type ObservationReserve struct {
	Market, MarketName, Reserve, LiquidityMint string
}

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

var usdcReference = primePRIMEUSDCTemplate.Kamino

var mapleSyrupUSDCUSDCTemplate = RuntimeRoute{
	Lane: MapleSyrupUSDCLaneID, Protocol: "Maple", CollateralSymbol: "syrupUSDC", DebtSymbol: "USDC",
	Kamino: KaminoObservationConfig{
		Program:           kamino.ProgramID.String(),
		Market:            fleet.KaminoMapleMarket,
		MarketAuthority:   "6QbtpY2jDNcncRFmVf343NThnCdaY8gCAsYATPnYQR9g",
		CollateralReserve: "AwCyCPZYJSZ93xcVKNK7jR8e1BHzJXq1D4bReNuh9woY",
		CollateralMint:    "AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj",
		DebtReserve:       "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo",
		DebtMint:          fleet.USDCMint,
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
		Market:            fleet.KaminoOnReMarket,
		MarketAuthority:   "FsvTiXTUFDc4aLbrov4PrvDTjXCWCniL1dxTUkZ1T2ss",
		CollateralReserve: "6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ",
		CollateralMint:    "5Y8NV33Vv7WbnLfq3zBcKSdYPrk7g2KoiQoe7M2tcxp5",
		DebtReserve:       "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z",
		DebtMint:          fleet.USDCMint,
	},
	CollateralLiquiditySupply: "9YuHgsPVGgWrkpsaRZmeZCV2uXweMEn6TEAcusQKRjgG",
	CollateralReceiptMint:     "CtzvqjvpxJDXyraDjP2QrEr8b1xvGvxADRV7w29qrmxd",
	CollateralReceiptSupply:   "2c42iUaea3QVLvSPQHUBZBwqdvpiQo5vmeMePq9qx8eo",
	DebtLiquiditySupply:       "8BkQTZsT8ssKMU643De4iiV5Wf3pENdUFTsdtHPueKjB",
	DebtFeeReceiver:           "5iLRav31Y7DJwM6bZ7s92jqvV3zd1wZMcp4mYeKXh8cj",
	CollateralTokenProgram:    classicTokenProgram, DebtTokenProgram: classicTokenProgram,
	DebtFarm: "7vNfe1qX8iDxP5p3A4fosrjLqdn1YjmmGcZZkG2b4APF",
}

var primePRIMEUSDCTemplate = RuntimeRoute{
	Lane: PrimeUSDCLaneID, Protocol: "Prime", CollateralSymbol: "PRIME", DebtSymbol: "USDC",
	Kamino: KaminoObservationConfig{
		Program: kamino.ProgramID.String(),
		Market:  fleet.KaminoFigureMarket, MarketAuthority: "9SLBVnPz8dRGvafST6zNBZYSSt3HtdU68XQLGR13t3uM",
		CollateralReserve: "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh", CollateralMint: "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7",
		DebtReserve: "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu", DebtMint: fleet.USDCMint,
	},
	CollateralLiquiditySupply: "FkSkbRU5A6JXRXo5uaFwCS7jQ6jHYa1DxFtfpXfTz352", CollateralReceiptMint: "FMKBCGqipyj5dm9C58Rb9ZWYeneDzrxd3YaL6amgZ8gW",
	CollateralReceiptSupply: "Eg4wKFWc8aGfAqrcmYu3paz2afY5VqJMo17K95Y4VqFN", DebtLiquiditySupply: "H6JUwz8c61eQnYUx8avGXydKztKPyGvgWAUjmZUPS3BC",
	DebtFeeReceiver: "BzSw9sWTxUumr2wHhDiezkaLy3QZQS1KT4a9Fz8GvAQ6", CollateralTokenProgram: classicTokenProgram, DebtTokenProgram: classicTokenProgram,
}

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
	r.Kamino.DebtReserve, r.Kamino.DebtMint = "7SzMWArC8WAenndXFmRyfvcvrNPodqUFkmPrmmoRZvn4", fleet.USDSMint
	r.DebtLiquiditySupply, r.DebtFeeReceiver = "5tP1kDJBYnjtrpUaRQhsrU1Y28ahiJVjz8p9mbqJFpz5", "DjmdtvsvctUXCZ32y6UGdCEvXPTds6Ci7LFnVhw5HaQY"
	return r
}()

const pyusdMint = fleet.PYUSDMint

var autoAUTOPYUSDTemplate = RuntimeRoute{
	Lane: "AUTO/AUTO/PYUSD", Protocol: "AUTO", CollateralSymbol: "AUTO", DebtSymbol: "PYUSD",
	Kamino: KaminoObservationConfig{
		Program:           kamino.ProgramID.String(),
		Market:            fleet.KaminoAutoMarket,
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

const onreONycUSDC = "OnRe/ONyc/USDC"

const autoAUTOWYLDSLane = "AUTO/AUTO/wYLDS"

var ethenaUSDePYUSDTemplate = RuntimeRoute{
	Lane: "Ethena/USDe/PYUSD", Protocol: "Ethena", CollateralSymbol: "USDe", DebtSymbol: "PYUSD",
	Kamino: KaminoObservationConfig{
		Program:           kamino.ProgramID.String(),
		Market:            fleet.KaminoEthenaMarket,
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
