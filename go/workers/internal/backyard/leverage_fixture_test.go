package backyard

import (
	"encoding/binary"
	"math/big"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

// leverage175Accounts turns the controlled OnRe fixture into a 1.75x
// position: collateral value C, debt D = (1.75-1)/1.75 * C (LTV 42.9%).
// Fixture prices: ONyc (9 dp) $1, USDC debt (6 dp) $2 per raw-unit math, so
// debt raw = collateral value in USDC-6 / 2.
func leverage175Fixture(t *testing.T) (Observation, RouteManifest, *chain.Client, *jupiter.Client, []ConfirmedAccount, RuntimeRoute) {
	t.Helper()
	o, m, rpc, client, accounts := usdcReturnFixtureForLane(t, onreONycUSDC)
	route, _ := runtimeRoute(onreONycUSDC)
	c := accountAt(accounts, route.Kamino.CollateralReserve).Data
	c[kaminoLoanToValueOffset], c[kaminoReserveConfigOffset+17] = 66, 75
	// Align the Kamino debt price ($1) with the Jupiter mock's ONyc->USDC
	// rate (1 ONyc = 1 USDC).
	putScaledFraction(accountAt(accounts, route.Kamino.DebtReserve).Data[248:264], new(big.Int).Lsh(big.NewInt(1), 60))
	// 100 ONyc (1e11 raw at 9 dp) = $100; debt 42.857% = 42_857_142 USDC-6.
	obligation := accountAt(accounts, route.Kamino.Obligation).Data
	binary.LittleEndian.PutUint64(obligation[128:136], 100_000_000_000)
	debt := uint64(42_857_142)
	putScaledFraction(obligation[1296:1312], new(big.Int).Lsh(new(big.Int).SetUint64(debt), 60))
	supply := accountAt(accounts, route.Kamino.CollateralReserve).Data
	binary.LittleEndian.PutUint64(supply[224:232], 1_000_000_000_000)
	binary.LittleEndian.PutUint64(supply[2592:2600], 1_000_000_000_000)
	binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralLiquiditySupply).Data[64:72], 1_000_000_000_000)
	binary.LittleEndian.PutUint64(accountAt(accounts, route.DebtCustody).Data[64:72], 0)
	// The debt reserve has lent at least our debt.
	putScaledFraction(accountAt(accounts, route.Kamino.DebtReserve).Data[232:248], new(big.Int).Lsh(new(big.Int).SetUint64(debt), 60))
	o.Snapshot.HasPosition = true
	o.Snapshot.PositionCollateralRaw, o.Snapshot.PositionCollateralValueRaw = 100_000_000_000, 100_000_000
	o.Snapshot.PositionDebtRaw, o.Snapshot.PositionDebtValueRaw = int64(debt), int64(debt)
	o.Snapshot.SquadsIdleRaw, o.Snapshot.LTVBPS, o.Snapshot.LiquidationThresholdBPS, o.Snapshot.MaxLTVBPS = 0, 4285, 7500, 7500
	return o, m, rpc, client, accounts, route
}

// targeted is s with target() applied over s alone, its stored level the
// snapshot's own level fields: the working lane's view as the observation
// returns it.
func targeted(s Snapshot) Snapshot {
	return targetedWith(s, targetInputs{})
}

// targetedWith is targeted with explicit exits and selector result; the
// snapshot's level fields stay its level unless in sets one.
func targetedWith(s Snapshot, in targetInputs) Snapshot {
	if s.HeldLanes == "" && (s.HasPosition || s.CollateralIdleRaw > 0) {
		s.HeldLanes = s.RouteLane
	}
	levels := map[string]laneLevel{}
	for lane, level := range in.Levels {
		levels[lane] = level
	}
	if _, ok := levels[s.RouteLane]; !ok && s.LeverageTargetLevel > 0 {
		levels[s.RouteLane] = laneLevel{Level: s.LeverageTargetLevel, UpApproved: s.LeverageUpApproved}
	}
	in.Levels = levels
	_, t := target([]Snapshot{s}, in)
	s.Target = t
	return s
}
