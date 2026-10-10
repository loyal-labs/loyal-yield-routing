package backyard

import "testing"

// With the off-chain budget deleted, a Kamino leg's only per-transaction
// bound below the protocol is the compiler's per-mint cap: the cap itself
// builds and compiles, one raw unit more is refused with its own reason
// before any signer, on both the collateral and the debt mint.
func TestKaminoLegAboveItsMintCapIsRefused(t *testing.T) {
	m := basicPolicyFixtureManifest(t)
	route, err := runtimeRoute(onreONycUSDC)
	if err != nil {
		t.Fatal(err)
	}
	blockhash := LatestBlockhash{Blockhash: bridgeSettings, LastValidBlockHeight: 99}
	for _, c := range []struct {
		leg  kaminoPrimeUSDCLeg
		mint string
	}{{kaminoLegWithdraw, route.Kamino.CollateralMint}, {kaminoLegRepay, route.Kamino.DebtMint}} {
		limit, ok := positionLegCapRaw[c.mint]
		if !ok {
			t.Fatalf("leg %d mint %s has no cap", c.leg, c.mint)
		}
		r, err := m.kaminoPacketForRoute(DeleverRouteStep, c.leg, limit, blockhash, route.Lane)
		if err != nil {
			t.Fatalf("leg %d at its cap refused: %v", c.leg, err)
		}
		r.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
		if _, err = m.compileKaminoMessage(r, mustKey(bridgeDelegate)); err != nil {
			t.Fatalf("leg %d at its cap did not compile: %v", c.leg, err)
		}
		_, err = m.kaminoPacketForRoute(DeleverRouteStep, c.leg, limit+1, blockhash, route.Lane)
		assertBudgetHold(t, err, "position_leg_cap_exceeded")
	}
	assertBudgetHold(t, checkPositionLegCap(bridgeVault, 1), "position_leg_cap_exceeded")
}
