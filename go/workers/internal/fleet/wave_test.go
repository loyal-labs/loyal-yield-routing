package fleet

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

func schedulingFixture(count int) (MarketSnapshot, []FleetVault) {
	snapshot, position := eligibleFixture()
	position.AmountRaw = 1_000_000_000
	position.SourceCollateralAmountRaw = position.AmountRaw
	position.PolicyAuthority = "tenant"
	vaults := make([]FleetVault, count)
	for i := range vaults {
		p := position
		p.VaultID, p.PolicyID = int64(i+1), int64(i+1)
		p.VaultPubkey = fmt.Sprintf("vault-%d", i)
		vaults[i] = FleetVault{Position: p, AllowedTargets: []string{"target"}}
	}
	return snapshot, vaults
}

func TestWaveLimitsIndependentlyBoundAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits WaveLimits
		want   int
	}{
		{"opportunities", WaveLimits{2, 1_000_000_000_000, 10, 10}, 2},
		{"notional exact boundary", WaveLimits{10, 2_000_000_000, 10, 10}, 2},
		{"notional below boundary", WaveLimits{10, 1_999_999_999, 10, 10}, 1},
		{"tenant", WaveLimits{10, 1_000_000_000_000, 2, 10}, 2},
		{"shared reserve conflict", WaveLimits{10, 1_000_000_000_000, 10, 2}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, vaults := schedulingFixture(3)
			plan, err := PlanFleetWithLimits(snapshot, vaults, tc.limits)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Opportunities) != tc.want || len(plan.Rejections) != 3-tc.want {
				t.Fatalf("selected=%d want=%d rejected=%v", len(plan.Opportunities), tc.want, plan.Rejections)
			}
			// Reverse input order: economics and the normalized source identity, not
			// loader iteration order, must determine which vaults are admitted.
			vaults[0], vaults[2] = vaults[2], vaults[0]
			reverse, err := PlanFleetWithLimits(snapshot, vaults, tc.limits)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(plan, reverse) {
				t.Fatal("wave depends on loader order")
			}
		})
	}
}

func TestWaveTenantLimitDoesNotBlockOtherTenants(t *testing.T) {
	snapshot, vaults := schedulingFixture(4)
	vaults[3].Position.PolicyAuthority = "other-tenant"
	plan, err := PlanFleetWithLimits(snapshot, vaults, WaveLimits{10, 1_000_000_000_000, 2, 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Opportunities) != 3 || plan.Opportunities[2].Decision.VaultID != 4 || plan.Rejections[3] != "tenant_limit" {
		t.Fatalf("wrong tenant admission: selected=%d rejected=%v", len(plan.Opportunities), plan.Rejections)
	}
}

// dilutionRescoreFixture builds five same-mint reserves and three same-mint
// reallocation candidates. v1 (A->B) is the initial root, but its 20e9 inflow
// into B dilutes B's supplied yield, which raises v2 (B->C)'s priority above
// v3 (D->E). Only a per-selection rescore of the remaining candidates sees it.
func dilutionRescoreFixture() (MarketSnapshot, []FleetVault) {
	now := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	reserves := map[string]ReserveState{}
	for _, r := range []struct {
		key  string
		apy  int64
		hash string
	}{{"A", 1000, "ha"}, {"B", 2000, "hb"}, {"C", 3000, "hc"}, {"D", 1000, "hd"}, {"E", 2010, "he"}} {
		reserves[r.key] = ReserveState{
			ReserveIdentity: ReserveIdentity{Address: r.key, Market: "market", Mint: USDCMint},
			Slot:            500, SupplyAPYBPS: r.apy,
			TotalSupplyUSDMicros:   1_000_000_000_000,
			EconomicLifetimeMillis: 600_000, DataHash: r.hash,
		}
	}
	snapshot := MarketSnapshot{Slot: 500, ObservedAt: now, Hash: "snapshot", Reserves: reserves}
	vaults := []FleetVault{
		{Position: dilutionPosition(1, "A", now), AllowedTargets: []string{"B"}},
		{Position: dilutionPosition(2, "B", now), AllowedTargets: []string{"C"}},
		{Position: dilutionPosition(3, "D", now), AllowedTargets: []string{"E"}},
	}
	vaults[0].Position.AmountRaw = 20_000_000_000
	vaults[0].Position.SourceCollateralAmountRaw = 20_000_000_000
	return snapshot, vaults
}

func dilutionPosition(id int64, source string, now time.Time) VaultPosition {
	return VaultPosition{VaultID: id, PolicyID: id, VaultPubkey: fmt.Sprintf("vault-%d", id),
		SourceReserve: source, Market: "market", Mint: USDCMint,
		AmountRaw: 10_000_000_000, SourceCollateralAmountRaw: 10_000_000_000,
		SourceAmountSemantics: amountSemanticsKaminoCollateralDeposited,
		SnapshotID:            7, ObservedSlot: 499, ObservedAt: now, PolicyAuthority: "tenant"}
}

func TestWaveRescoresCandidatesWhosePriorityImprovesAfterSelection(t *testing.T) {
	for _, order := range [][]int{{0, 1, 2}, {2, 1, 0}, {1, 2, 0}} {
		snapshot, vaults := dilutionRescoreFixture()
		permuted := []FleetVault{vaults[order[0]], vaults[order[1]], vaults[order[2]]}
		plan, err := PlanFleetWithLimitsAt(snapshot, permuted, WaveLimits{2, 1_000_000_000_000, 10, 10}, snapshot.ObservedAt)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Opportunities) != 2 {
			t.Fatalf("order=%v selected=%d rejections=%v", order, len(plan.Opportunities), plan.Rejections)
		}
		if plan.Opportunities[0].Decision.VaultID != 1 || plan.Opportunities[1].Decision.VaultID != 2 {
			t.Fatalf("order=%v chose vaults %d,%d; want 1,2", order,
				plan.Opportunities[0].Decision.VaultID, plan.Opportunities[1].Decision.VaultID)
		}
		// Exact amounts are preserved through the rescore.
		if plan.Opportunities[0].Decision.PrincipalUSDMicros != 20_000_000_000 ||
			plan.Opportunities[1].Decision.PrincipalUSDMicros != 10_000_000_000 {
			t.Fatalf("order=%v principals %d,%d", order,
				plan.Opportunities[0].Decision.PrincipalUSDMicros, plan.Opportunities[1].Decision.PrincipalUSDMicros)
		}
	}
}

func TestWaveReconsidersInitiallyUneconomicPermittedTarget(t *testing.T) {
	snapshot, vaults := dilutionRescoreFixture()
	target := snapshot.Reserves["C"]
	target.SupplyAPYBPS = 2000
	snapshot.Reserves["C"] = target
	if before := Plan(snapshot, vaults[1].Position, "B", "C"); before.Eligible {
		t.Fatalf("fixture did not begin below the economic gate: %+v", before)
	}
	plan, err := PlanFleetAt(snapshot, vaults[:2], snapshot.ObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Opportunities) != 2 || plan.Opportunities[0].Decision.VaultID != 1 || plan.Opportunities[1].Decision.VaultID != 2 {
		t.Fatalf("permitted candidate lost before its source yield improved: %+v", plan)
	}
	if plan.Opportunities[1].Decision.AmountRaw != vaults[1].Position.AmountRaw {
		t.Fatal("rescore changed the authorized source amount")
	}
}

func TestLargeReserveCapacityBoundary(t *testing.T) {
	snapshot, position := eligibleFixture()
	target := snapshot.Reserves["target"]
	target.TotalSupplyUSDMicros = 1_000_000_000_000_000 // $1B, $20M frontier.
	snapshot.Reserves["target"] = target
	for _, tc := range []struct {
		amount int64
		want   bool
	}{
		{4_000_000_000_001, true}, {5_000_000_000_000, true},
		{20_000_000_000_000, true}, {20_000_000_000_001, false},
	} {
		position.AmountRaw = tc.amount
		position.SourceCollateralAmountRaw = tc.amount
		d := Plan(snapshot, position, "source", "target")
		if d.Eligible != tc.want {
			t.Fatalf("amount=%d eligible=%v reason=%s", tc.amount, d.Eligible, d.Reason)
		}
		if !tc.want && d.Reason != "target_capacity_exhausted" {
			t.Fatalf("unexpected boundary rejection: %s", d.Reason)
		}
	}
}
