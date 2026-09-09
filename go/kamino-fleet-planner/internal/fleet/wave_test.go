package fleet

import (
	"fmt"
	"reflect"
	"testing"
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
