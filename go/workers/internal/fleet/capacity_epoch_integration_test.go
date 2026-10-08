package fleet

import (
	"fmt"
	"testing"
	"time"
)

// An executor admission advances the target frontier from fresh RPC while the
// planner still plans from an older verified market epoch. Rust treats that
// planner epoch as a harmless no-op (capacity.rs
// refresh_target_capacity_from_market_epoch); it must not fail the planning
// cycle, move the frontier backwards, or relax the executor's own check.
func TestPlannerEpochOlderThanExecutorCapacityFrontierIsNoop(t *testing.T) {
	s, ctx := waitingALTStore(t)
	cluster := fmt.Sprintf("capacity-epoch-%d", time.Now().UnixNano())
	reserve := manifestKey(70)
	frontier := func() (supply, slot int64) {
		t.Helper()
		if err := s.pool.QueryRow(ctx, `SELECT observed_supply_usd_micros,observed_slot FROM loyal_yield.target_capacity_frontiers WHERE cluster=$1 AND target_reserve=$2 AND liquidity_mint=$3`, cluster, reserve, USDCMint).Scan(&supply, &slot); err != nil {
			t.Fatal(err)
		}
		return supply, slot
	}
	if err := s.RefreshTargetCapacity(ctx, cluster, reserve, USDCMint, 9_000_000_000, 2_000); err != nil {
		t.Fatal(err)
	}
	epoch := ImmutableMarketEpoch{Reserves: []MarketEpochReserve{{Reserve: reserve, LiquidityMint: USDCMint, TargetEligible: true, TotalSupplyUSDMicros: 8_000_000_000, Slot: 1_500}}}
	if err := s.RefreshCapacityEpoch(ctx, cluster, epoch); err != nil {
		t.Fatalf("an older planner epoch failed the planning cycle: %v", err)
	}
	if supply, slot := frontier(); supply != 9_000_000_000 || slot != 2_000 {
		t.Fatalf("older planner epoch moved the frontier to supply=%d slot=%d", supply, slot)
	}
	if err := s.RefreshTargetCapacity(ctx, cluster, reserve, USDCMint, 8_000_000_000, 1_500); err == nil {
		t.Fatal("executor admission accepted an observation older than the durable frontier")
	}
	epoch.Reserves[0].Slot = 2_500
	if err := s.RefreshCapacityEpoch(ctx, cluster, epoch); err != nil {
		t.Fatal(err)
	}
	if supply, slot := frontier(); supply != 8_000_000_000 || slot != 2_500 {
		t.Fatalf("newer planner epoch did not advance the frontier: supply=%d slot=%d", supply, slot)
	}
}
