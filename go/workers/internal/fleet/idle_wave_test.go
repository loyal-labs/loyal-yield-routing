package fleet

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func idleWaveFixture() (MarketSnapshot, []FleetVault, time.Time) {
	snapshot, vaults := schedulingFixture(3)
	now := snapshot.ObservedAt
	snapshot.MintExpiresAt = map[string]time.Time{USDCMint: now.Add(5 * time.Minute)}
	for name, r := range snapshot.Reserves {
		r.TotalSupplyUSDMicros = 1_000_000_000_000
		snapshot.Reserves[name] = r
	}
	for i := range vaults {
		vaults[i].Position.AmountRaw = 9_000_000_000
		vaults[i].Position.SourceCollateralAmountRaw = 9_000_000_000
	}
	vaults[0] = asIdleSource(vaults[0])
	return snapshot, vaults, now
}

func asIdleSource(v FleetVault) FleetVault {
	v.IdleTokenAccount = "idle-" + v.Position.VaultPubkey
	v.Position.SourceReserve, v.Position.Market, v.Position.SourceAmountSemantics = "", "", "idle_vault_liquidity"
	v.Position.SourceCollateralAmountRaw, v.Position.SnapshotID = 0, 0
	return v
}

func TestShadowIdleSharesCapacityWithoutBecomingPublishable(t *testing.T) {
	snapshot, vaults, now := idleWaveFixture()
	plan, err := PlanFleetShadowAt(snapshot, vaults, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Opportunities) != 2 || plan.Opportunities[0].Decision.RouteKind != "idle_vault_deposit" {
		t.Fatalf("joint admission selected=%d rejected=%v", len(plan.Opportunities), plan.Rejections)
	}
	total := int64(0)
	for _, o := range plan.Opportunities {
		total += o.Decision.PrincipalUSDMicros
	}
	if total != 18_000_000_000 {
		t.Fatalf("idle and reserve capacity were not shared: %d", total)
	}
	idle := plan.Opportunities[0]
	if idle.Decision.SourceReserve != "" || idle.Decision.SourceAPYBPS != 0 {
		t.Fatal("idle invented a withdrawal or source yield")
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(idle.ExecutionPlan, &wire); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"source_reserve", "source_amount_semantics", "source_collateral_amount_raw", "redeemable_source_liquidity_amount_raw"} {
		if string(wire[key]) != "null" {
			t.Fatalf("idle wire has reserve evidence at %s", key)
		}
	}
	if string(wire["source_kind"]) != `"idle_vault_usdc"` || string(wire["route_amount_semantics"]) != `"idle_vault_liquidity"` {
		t.Fatal("retained idle wire semantics drifted")
	}
	if _, err := PlanFleet(snapshot, vaults); err == nil {
		t.Fatal("idle diagnostic entered executable planning")
	}
	// An unconnected Store must reject before it can reach any DB operation.
	if _, err := (&Store{}).Publish(context.Background(), "localnet", ImmutableMarketEpoch{}, vaults[0].Position, idle.Decision); err == nil {
		t.Fatal("idle diagnostic became publishable")
	}
}

func TestShadowIdleAndReserveOfOneVaultCompete(t *testing.T) {
	snapshot, vaults, now := idleWaveFixture()
	reserve := vaults[1]
	reserve.Position.VaultID = vaults[0].Position.VaultID
	reserve.Position.VaultPubkey = vaults[0].Position.VaultPubkey
	for _, idleWins := range []bool{true, false} {
		idle := vaults[0]
		if !idleWins {
			idle.Position.AmountRaw = 1_000_000_000
		}
		plan, err := PlanFleetShadowAt(snapshot, []FleetVault{reserve, idle}, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Opportunities) != 1 {
			t.Fatalf("one vault admitted %d sources", len(plan.Opportunities))
		}
		if (plan.Opportunities[0].Decision.RouteKind == "idle_vault_deposit") != idleWins {
			t.Fatal("source selection did not follow economics")
		}
	}
}

func TestShadowIdleSingleReserveAndLifetimeFence(t *testing.T) {
	snapshot, vaults, now := idleWaveFixture()
	delete(snapshot.Reserves, "source")
	plan, err := PlanFleetShadowAt(snapshot, vaults[:1], now)
	if err != nil || len(plan.Opportunities) != 1 {
		t.Fatalf("idle-only frontier rejected: %v", err)
	}
	for _, at := range []time.Time{snapshot.MintExpiresAt[USDCMint].Add(-minimumPublicationLifetime), snapshot.MintExpiresAt[USDCMint]} {
		plan, err = PlanFleetShadowAt(snapshot, vaults[:1], at)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Opportunities) != 0 {
			t.Fatal("expired idle evidence admitted")
		}
	}
}
