package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

type voltrReplay struct {
	Opportunity    *fleet.VoltrOpportunity
	OpportunityKey *string
}

// replayVoltr feeds decide the saved-evidence bytes an operator captures: the
// epoch goes through its Rust-compatible MarshalJSON, not a Go struct copy.
func replayVoltr(t *testing.T, o fleet.VoltrObservation, epoch fleet.ImmutableMarketEpoch, now time.Time) voltrReplay {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"Observation": o, "Epoch": epoch, "OptimizerEpochRowID": 41, "VaultID": 7, "LastOptimization": nil, "EvaluatedAt": now})
	if err != nil {
		t.Fatal(err)
	}
	result, err := decide("voltr", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var out voltrReplay
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The Earn adapter replays one confirmed Main -> OnRe movement through this
// kind: zero demand must select the yield leg out of the lower-yield market,
// and a positive withdrawal demand on the same positions must displace it with
// restoration before any optimization.
func TestVoltrReplayRestoresWithdrawalsBeforeOptimizing(t *testing.T) {
	r, err := fleet.LoadVoltrRoute()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	expires := now.Add(time.Hour)
	epoch := fleet.ImmutableMarketEpoch{MintCoverage: []fleet.MarketMintCoverage{{Mint: fleet.USDCMint, Complete: true, ExpiresAt: &expires}}}
	for i, s := range r.Strategies {
		market := s.LendingMarket
		// Main 300, OnRe 500, Prime and Maple ineligible.
		epoch.Reserves = append(epoch.Reserves, fleet.MarketEpochReserve{Reserve: s.Reserve, Market: &market, LiquidityMint: fleet.USDCMint, EconomicExpiresAt: expires,
			AvailableAmountRaw: "500000000000", TotalSupplyAmountRaw: "100000000000000", SupplyAPYBPS: []int64{300, 500, 400, 200}[i], TargetEligible: i < 2})
	}
	o := fleet.VoltrObservation{ContextSlot: 900, TotalValueRaw: 100e6, PositionsRaw: [4]uint64{100e6}, Receipts: "r", State: "s", Addresses: "a"}

	normal := replayVoltr(t, o, epoch, now)
	var plan map[string]any
	if normal.Opportunity == nil || json.Unmarshal(normal.Opportunity.Plan, &plan) != nil {
		t.Fatalf("zero-demand replay planned nothing: %+v", normal)
	}
	if v := normal.Opportunity; v.Class != "yield_optimization" || plan["strategy_id"] != "main" || plan["operation"] != "withdraw" ||
		v.SourceReserve == nil || *v.SourceReserve != r.Strategies[0].Reserve || v.TargetReserve != "voltr_idle:"+r.Vault ||
		v.AmountRaw != 100e6 || v.SourceAPYBPS != 300 || v.TargetAPYBPS != 500 {
		t.Fatalf("zero-demand leg %+v plan %v", v, plan)
	}
	if normal.OpportunityKey == nil || *normal.OpportunityKey != fleet.VoltrOpportunityKey(r.Cluster, 41, *normal.Opportunity) {
		t.Fatalf("replay key %v does not bind the durable epoch row", normal.OpportunityKey)
	}

	o.PendingRaw, o.EarliestRedeem = 30e6, uint64(now.Add(10*time.Minute).Unix())
	probe := replayVoltr(t, o, epoch, now)
	if v := probe.Opportunity; v == nil || v.Class != "withdrawal_restoration" || v.AmountRaw != 30e6 || v.EdgeBPS != 0 || v.AnnualGainUSDMicros != 0 {
		t.Fatalf("positive demand did not restore first: %+v", v)
	}

	// Demand already covered by idle still blocks optimization.
	o.IdleRaw, o.TotalValueRaw = 30e6, 130e6
	if covered := replayVoltr(t, o, epoch, now); covered.Opportunity != nil || covered.OpportunityKey != nil {
		t.Fatalf("covered demand still optimized: %+v", covered.Opportunity)
	}
}
