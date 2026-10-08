package fleet

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/gagliardetto/solana-go"
)

// replayFixture is a recorded production planning input: one Rust-era
// optimizer epoch and the vaults Rust published moves for from it.
type replayFixture struct {
	Provenance      string               `json:"provenance"`
	SourceReserve   string               `json:"sourceReserve"`
	TargetReserve   string               `json:"targetReserve"`
	AmountSemantics string               `json:"amountSemantics"`
	PolicyMarkets   []string             `json:"policyMarkets"`
	Epoch           ImmutableMarketEpoch `json:"epoch"`
	// Positions are [amount_raw, source observed slot, Rust planning cycle].
	Positions [][3]int64 `json:"positions"`
}

func syntheticPubkey(kind string, n int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("fleet-replay-2026-10-07/%s/%d", kind, n)))
	return solana.PublicKeyFromBytes(sum[:]).String()
}

// On 2026-10-07 at 23:47:58Z the Rust planner published 128 moves from
// AYL4LM to D6q6wu (source 18.72% vs target 33.34% supply APY) in two cycles
// of 64, the per-reserve writable-conflict limit. Planning the same epoch and
// vaults through Go must select the same direction and count, cycle by cycle;
// a Go planner that stays silent on this input is a planning regression, not
// a quiet market.
func TestPlanFleetReplaysRustBurst20261007(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/fleet/rust-burst-2026-10-07.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture replayFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	epoch := fixture.Epoch
	if err := epoch.Validate(); err != nil {
		t.Fatal(err)
	}
	addresses, err := epoch.RoutableReserveAddresses()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := marketSnapshotFromEpoch(epoch, addresses...)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.ExpiresAt = epoch.ExpiresAt
	source, ok := epoch.Reserve(fixture.SourceReserve)
	if !ok || source.Market == nil {
		t.Fatal("source reserve is absent from the epoch")
	}
	// Allowed targets as LoadMigratedFleet derives them from the policy.
	var allowed []string
	for _, reserve := range epoch.Reserves {
		if reserve.LiquidityMint == source.LiquidityMint && reserve.Market != nil && contains(fixture.PolicyMarkets, *reserve.Market) && reserve.TargetEligible {
			allowed = append(allowed, reserve.Reserve)
		}
	}
	vaults := make([]FleetVault, 0, len(fixture.Positions))
	rustCycle := map[int64]int64{}
	for i, p := range fixture.Positions {
		n := i + 1
		rustCycle[int64(n)] = p[2]
		vaults = append(vaults, FleetVault{
			Position: VaultPosition{
				VaultID: int64(n), Settings: syntheticPubkey("settings", n), VaultIndex: 1, VaultPubkey: syntheticPubkey("vault", n),
				PolicyID: int64(n), PolicyAuthority: syntheticPubkey("authority", n), PolicyAccount: syntheticPubkey("policy", n),
				SourceReserve: source.Reserve, Market: *source.Market, Mint: source.LiquidityMint,
				AmountRaw: p[0], SourceCollateralAmountRaw: p[0], SourceAmountSemantics: fixture.AmountSemantics,
				SnapshotID: int64(1000 + n), ObservedSlot: p[1], ObservedAt: epoch.CapturedAt,
			},
			AllowedTargets: allowed,
		})
	}
	// LoadMigratedFleet withholds a vault with a live opportunity, so the
	// second cycle plans the vaults the first left behind.
	selected := 0
	for cycle := int64(1); cycle <= 2; cycle++ {
		plan, err := PlanFleetAt(snapshot, vaults, epoch.CapturedAt)
		if err != nil {
			t.Fatal(err)
		}
		chosen := map[int64]bool{}
		for _, o := range plan.Opportunities {
			d := o.Decision
			if d.SourceReserve != fixture.SourceReserve || d.TargetReserve != fixture.TargetReserve || d.RouteKind != "same_mint" {
				t.Fatalf("cycle %d vault %d moves %s -> %s (%s), Rust moved %s -> %s", cycle, d.VaultID, d.SourceReserve, d.TargetReserve, d.RouteKind, fixture.SourceReserve, fixture.TargetReserve)
			}
			if rustCycle[d.VaultID] != cycle {
				t.Errorf("cycle %d selected vault %d, which Rust published in cycle %d", cycle, d.VaultID, rustCycle[d.VaultID])
			}
			chosen[d.VaultID] = true
		}
		if len(chosen) != 64 {
			t.Fatalf("cycle %d selected %d moves, Rust published 64; rejections %v", cycle, len(chosen), rejectionCounts(plan.Rejections))
		}
		selected += len(chosen)
		remaining := vaults[:0:0]
		for _, vault := range vaults {
			if !chosen[vault.Position.VaultID] {
				remaining = append(remaining, vault)
			}
		}
		vaults = remaining
	}
	if selected != len(fixture.Positions) {
		t.Fatalf("Go selected %d moves over two cycles, Rust published %d", selected, len(fixture.Positions))
	}
}
