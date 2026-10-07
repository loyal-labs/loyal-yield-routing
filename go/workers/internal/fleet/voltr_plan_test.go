package fleet

import (
	"encoding/json"
	"testing"
	"time"
)

// voltrFixtureEpoch covers all four markets with fresh evidence; apys are
// main 300, onre 500, prime 400, maple 200 bps.
func voltrFixtureEpoch(t *testing.T, r VoltrRoute, now time.Time) ImmutableMarketEpoch {
	t.Helper()
	expires := now.Add(time.Hour)
	e := ImmutableMarketEpoch{MintCoverage: []MarketMintCoverage{{Mint: USDCMint, Complete: true, ExpiresAt: &expires}}}
	for i, s := range r.Strategies {
		market := s.LendingMarket
		e.Reserves = append(e.Reserves, MarketEpochReserve{Reserve: s.Reserve, Market: &market, LiquidityMint: USDCMint, EconomicExpiresAt: expires,
			AvailableAmountRaw: "500000000000", TotalSupplyAmountRaw: "100000000000000", SupplyAPYBPS: []int64{300, 500, 400, 200}[i], TargetEligible: true})
	}
	return e
}

func TestPlanVoltrRestoresWithdrawalLiquidityBeforeAnythingElse(t *testing.T) {
	r, err := LoadVoltrRoute()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	o := VoltrObservation{ContextSlot: 900, TotalValueRaw: 100e6, IdleRaw: 10e6, PositionsRaw: [4]uint64{40e6, 0, 0, 50e6}, PendingRaw: 30e6,
		EarliestRedeem: uint64(now.Add(10 * time.Minute).Unix()), Receipts: "r", State: "s", Addresses: "a"}
	v, err := PlanVoltr(r, o, voltrFixtureEpoch(t, r, now), 7, nil, now)
	if err != nil || v == nil {
		t.Fatalf("plan %v %v", v, err)
	}
	// The 20 USDC shortfall comes from the lowest-yield market that can cover it.
	var plan map[string]any
	_ = json.Unmarshal(v.Plan, &plan)
	if v.Class != "withdrawal_restoration" || v.AmountRaw != 20e6 || plan["strategy_id"] != "maple" || plan["operation"] != "withdraw" ||
		v.SourceReserve == nil || *v.SourceReserve != r.Strategies[3].Reserve || v.TargetReserve != "voltr_idle:"+r.Vault || v.EdgeBPS != 0 ||
		v.ServiceDeadline == nil || !v.ExpiresAt.Equal(*v.ServiceDeadline) {
		t.Fatalf("restoration %+v plan %v", v, plan)
	}
	if plan["intent_sha256"] != r.IntentSHA256(3, "withdraw", 20e6, 900, "r", "s", "a") {
		t.Fatal("intent does not bind the observation")
	}
	// Covered receipts and capital already on its best markets: nothing to do.
	o.PendingRaw, o.IdleRaw, o.PositionsRaw = 0, 0, [4]uint64{0, 100e6, 0, 0}
	if v, err := PlanVoltr(r, o, voltrFixtureEpoch(t, r, now), 7, nil, now); err != nil || v != nil {
		t.Fatalf("settled vault planned %+v %v", v, err)
	}
	// Idle capital goes to the best market with room.
	o.IdleRaw, o.PositionsRaw = 100e6, [4]uint64{}
	if v, err := PlanVoltr(r, o, voltrFixtureEpoch(t, r, now), 7, nil, now); err != nil || v == nil || v.Class != "idle_allocation" || v.TargetReserve != r.Strategies[1].Reserve || v.AmountRaw != 100e6 || v.TargetAPYBPS != 500 {
		t.Fatalf("idle allocation %+v %v", v, err)
	}
	// Missing market coverage defers instead of planning blind.
	epoch := voltrFixtureEpoch(t, r, now)
	epoch.Reserves = epoch.Reserves[1:]
	if _, err := PlanVoltr(r, o, epoch, 7, nil, now); err != errVoltrMarketCoverage {
		t.Fatalf("partial epoch: %v", err)
	}
}
