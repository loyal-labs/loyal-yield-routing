package earn

import (
	"testing"
	"time"
)

func TestHourlyAPYIsTimeWeightedNetOfFee(t *testing.T) {
	end := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	outputStart := end.Add(-time.Hour)
	queryStart := outputStart.Add(-apyWindow)
	supported := map[string]string{"best": "usdc", mainUSDCReserve: "usdc"}
	rows := []reserveUpdate{
		{observedAt: queryStart, reserve: "best", mint: "usdc", totalSupply: 1e6, supply: 0.05},
		{observedAt: queryStart, reserve: mainUSDCReserve, mint: "usdc", totalSupply: 1e6, supply: 0.04},
		// Halfway through the trailing window the best reserve turns stale and
		// stops counting; the remaining eligible reserve sets the strategy APY.
		{observedAt: queryStart.Add(apyWindow / 2), reserve: "best", mint: "usdc", stale: true, totalSupply: 1e6, supply: 0.05},
		{observedAt: queryStart.Add(apyWindow / 2), reserve: "unsupported", mint: "usdc", totalSupply: 1e6, supply: 0.3},
	}
	snapshots := computeSnapshots(end, 1, outputStart, queryStart, rows, supported)
	if len(snapshots) != 1 || !snapshots[0].sampleHour.Equal(end) || !snapshots[0].windowStart.Equal(end.Add(-apyWindow)) {
		t.Fatalf("snapshots = %+v", snapshots)
	}
	// First half max(0.05, 0.04), second half 0.04: weighted 0.045, minus 1 bps.
	if snapshots[0].loyalBPS != 449 || snapshots[0].mainBPS != 400 {
		t.Fatalf("loyal=%d main=%d, want 449/400", snapshots[0].loyalBPS, snapshots[0].mainBPS)
	}
	if computeSnapshots(end, 1, end, queryStart, rows, supported) != nil {
		t.Fatal("an empty output range produced snapshots")
	}
}
