package fleet

import (
	"fmt"
	"testing"
)

// Synthetic sparse permitted graphs, not production capacity measurements.
// Eight selected moves bound every case; normal tests do not run benchmarks.
func BenchmarkWaveOracleSparsePermittedGraph(b *testing.B) {
	for _, vaultCount := range []int{256, 4096} {
		for _, targetCount := range []int{1, 4, 16} {
			b.Run(fmt.Sprintf("vaults=%d/targets=%d/maxmoves=8", vaultCount, targetCount), func(b *testing.B) {
				snapshot := oracleSnapshot()
				oracleReserve(&snapshot, "source", USDCMint, 200, 2_000_000_000_000_000)
				targets := make([]string, targetCount)
				for target := range targets {
					targets[target] = fmt.Sprintf("target%02d", target)
					oracleReserve(&snapshot, targets[target], USDCMint, int64(700+target), 2_000_000_000_000_000)
				}
				vaults := make([]FleetVault, vaultCount)
				for vault := range vaults {
					vaults[vault] = oracleVault(snapshot, int64(vault+1), "source", 1_000_000_000, targets...)
				}
				limits := WaveLimits{8, 8_000_000_000, 8, 8}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					plan, err := PlanFleetWithLimitsAt(snapshot, vaults, limits, snapshot.ObservedAt)
					if err != nil || len(plan.Opportunities) != 8 {
						b.Fatalf("bounded sparse probe selected=%d err=%v", len(plan.Opportunities), err)
					}
				}
				b.ReportMetric(float64(vaultCount*targetCount), "permitted-edges/op")
			})
		}
	}
}
