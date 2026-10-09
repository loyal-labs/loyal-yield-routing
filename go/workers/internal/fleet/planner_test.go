package fleet

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func eligibleFixture() (MarketSnapshot, VaultPosition) {
	now := time.Now().UTC()
	source := ReserveState{ReserveIdentity: ReserveIdentity{Address: "source", Market: "market", Mint: USDCMint}, Slot: 500, SupplyAPYBPS: 200, TotalSupplyUSDMicros: 2_000_000_000_000_000, EconomicLifetimeMillis: 600_000, DataHash: "a"}
	target := ReserveState{ReserveIdentity: ReserveIdentity{Address: "target", Market: "market", Mint: USDCMint}, Slot: 500, SupplyAPYBPS: 700, TotalSupplyUSDMicros: 2_000_000_000_000_000, EconomicLifetimeMillis: 600_000, DataHash: "b"}
	snapshot := MarketSnapshot{Slot: 500, ObservedAt: now, Hash: "snapshot", Reserves: map[string]ReserveState{"source": source, "target": target}}
	position := VaultPosition{VaultID: 1, SourceReserve: "source", Market: "market", Mint: USDCMint, AmountRaw: 1_000_000_000_000, SourceCollateralAmountRaw: 900_000_000_000, SourceAmountSemantics: amountSemanticsKaminoCollateralDeposited, SnapshotID: 7, ObservedSlot: 499, ObservedAt: now}
	return snapshot, position
}

func TestPlanIsPureEconomicAndCapacityAware(t *testing.T) {
	snapshot, position := eligibleFixture()
	decision := Plan(snapshot, position, "source", "target")
	if !decision.Eligible {
		t.Fatalf("expected eligible decision, got %s", decision.Reason)
	}
	if decision.TargetAPYBPS >= snapshot.Reserves["target"].SupplyAPYBPS {
		t.Fatal("target APY was not capacity-adjusted")
	}
	if decision.EdgeBPS <= 0 || decision.ExpectedNetGainUSDMicros <= 0 || decision.EconomicPriority <= 0 {
		t.Fatalf("incomplete economics: %+v", decision)
	}
	again := Plan(snapshot, position, "source", "target")
	if decision != again {
		t.Fatal("pure planner was nondeterministic")
	}
}

func TestPlanFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*MarketSnapshot, *VaultPosition)
		reason string
	}{
		{"active work", func(_ *MarketSnapshot, p *VaultPosition) { p.BlockedReason = "active_opportunity" }, "active_opportunity"},
		{"missing amount evidence", func(_ *MarketSnapshot, p *VaultPosition) { p.SourceAmountSemantics = "" }, "unsupported_source_amount_evidence"},
		{"capacity exhausted", func(s *MarketSnapshot, p *VaultPosition) {
			p.TargetCommittedInflowUSDMicros = s.Reserves["target"].TotalSupplyUSDMicros / 50
		}, "target_capacity_exhausted"},
		{"amount exceeds capacity", func(s *MarketSnapshot, p *VaultPosition) {
			p.AmountRaw = s.Reserves["target"].TotalSupplyUSDMicros/50 + 1
		}, "target_capacity_exhausted"},
		{"projection overflow", func(s *MarketSnapshot, p *VaultPosition) {
			reserve := s.Reserves["source"]
			reserve.TotalSupplyUSDMicros = int64(^uint64(0) >> 1)
			s.Reserves["source"] = reserve
			p.SourceCommittedInflowUSDMicros = 1
		}, "capacity_arithmetic_overflow"},
		{"expiring target evidence", func(s *MarketSnapshot, _ *VaultPosition) {
			reserve := s.Reserves["target"]
			reserve.EconomicLifetimeMillis = 69_999
			s.Reserves["target"] = reserve
		}, "target_economic_evidence_lifetime_too_short"},
		{"explicitly stale target", func(s *MarketSnapshot, _ *VaultPosition) {
			reserve := s.Reserves["target"]
			reserve.LastUpdateStale = true
			s.Reserves["target"] = reserve
		}, "target_explicitly_stale"},
		{"no edge", func(s *MarketSnapshot, _ *VaultPosition) {
			v := s.Reserves["target"]
			v.SupplyAPYBPS = 100
			s.Reserves["target"] = v
		}, "below_minimum_edge"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot, position := eligibleFixture()
			test.mutate(&snapshot, &position)
			decision := Plan(snapshot, position, "source", "target")
			if decision.Eligible || decision.Reason != test.reason {
				t.Fatalf("got eligible=%v reason=%s", decision.Eligible, decision.Reason)
			}
		})
	}
}

func TestPlanFleetBuildsCrossMintJupiterWithImmutablePolicies(t *testing.T) {
	snapshot, position := eligibleFixture()
	target := snapshot.Reserves["target"]
	target.Mint = USDTMint
	snapshot.Reserves["target"] = target
	binding := CrossMintPolicyBindings{
		Settings: "settings", VaultPubkey: "vault", DelegatedSigner: "signer",
		Withdraw: CrossMintEarnPolicyBinding{PolicyAccount: "withdraw", SourceCommitment: "finalized"},
		Swap:     CrossMintSwapPolicyBinding{PolicyAccount: "swap", SourceShard: "classic", SourceCommitment: "finalized", MaxSlippageBPS: 25, DailySourceMintSpendingCap: 1_000_000, ManifestFingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Deposit:  CrossMintEarnPolicyBinding{PolicyAccount: "deposit", SourceCommitment: "finalized", ConstraintIndex: 1},
	}
	plan, err := PlanFleet(snapshot, []FleetVault{{Position: position, CrossMintTargets: map[string]CrossMintPolicyBindings{"target": binding}, CrossMintMaxValueLossBPS: 50}})
	if err != nil || len(plan.Opportunities) != 1 {
		t.Fatalf("cross-mint route missing: %+v %v", plan, err)
	}
	d := plan.Opportunities[0].Decision
	if d.RouteKind != "cross_mint_jupiter" || d.SourceMint != USDCMint || d.TargetMint != USDTMint || d.Mint != USDTMint || d.PolicyBindings == nil {
		t.Fatalf("cross-mint identity/policy drift: %+v", d)
	}
	var execution map[string]any
	if err := json.Unmarshal(plan.Opportunities[0].ExecutionPlan, &execution); err != nil {
		t.Fatal(err)
	}
	if execution["route_kind"] != "cross_mint_jupiter" || execution["fresh_executable_jupiter_minimum_output_required"] != true || execution["cross_mint_maximum_value_loss_bps"] != float64(50) {
		t.Fatalf("incomplete cross-mint execution contract: %s", plan.Opportunities[0].ExecutionPlan)
	}
}

func TestPlanFleetAllowsEmptyPlannableFleet(t *testing.T) {
	now := time.Now().UTC()
	snapshot := MarketSnapshot{Reserves: map[string]ReserveState{
		"source": {ReserveIdentity: ReserveIdentity{Address: "source"}},
		"target": {ReserveIdentity: ReserveIdentity{Address: "target"}},
	}}
	snapshot.ObservedAt = now
	plan, err := PlanFleet(snapshot, nil)
	if err != nil {
		t.Fatalf("empty plannable fleet must not block revalidation: %v", err)
	}
	if len(plan.Opportunities) != 0 || len(plan.Rejections) != 0 {
		t.Fatalf("unexpected empty plan: %+v", plan)
	}
}

func TestPlanAllowsStaleSourceThatRevalidatorWillRefresh(t *testing.T) {
	snapshot, position := eligibleFixture()
	source := snapshot.Reserves["source"]
	source.LastUpdateStale = true
	source.EconomicLifetimeMillis = 0
	snapshot.Reserves["source"] = source
	if decision := Plan(snapshot, position, "source", "target"); !decision.Eligible {
		t.Fatalf("stale source-only route was rejected: %s", decision.Reason)
	}
}

func TestOpportunityIdentityFencesEpochAndEconomics(t *testing.T) {
	snapshot, position := eligibleFixture()
	decision := Plan(snapshot, position, "source", "target")
	plan, err := canonicalSameMintExecutionPlan(position, decision, decision.SourceAPYBPS, decision.TargetAPYBPS, snapshot.Slot, snapshot.ObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	expires := snapshot.ObservedAt.Add(time.Minute)
	first := opportunityIdentity("mainnet-beta", 10, decision, plan, expires)
	if first == opportunityIdentity("mainnet-beta", 11, decision, plan, expires) {
		t.Fatal("optimizer epoch did not fence the identity")
	}
	decision.TargetAPYBPS++
	if first == opportunityIdentity("mainnet-beta", 10, decision, plan, expires) {
		t.Fatal("material economics did not change the identity")
	}
}

func TestCapacitySumPreservesWideCancellationAndRejectsFinalOverflow(t *testing.T) {
	for _, tc := range []struct {
		parts []int64
		want  int64
		ok    bool
	}{
		{[]int64{math.MaxInt64, 1, -1}, math.MaxInt64, true},
		{[]int64{math.MinInt64, -1, 1}, math.MinInt64, true},
		{[]int64{math.MaxInt64, 1}, 0, false},
		{[]int64{math.MinInt64, -1}, 0, false},
		{[]int64{math.MaxInt64, math.MinInt64, 1}, 0, true},
	} {
		got, ok := sumInt64(tc.parts...)
		if ok != tc.ok || ok && got != tc.want {
			t.Fatalf("sum %v = %d/%v, want %d/%v", tc.parts, got, ok, tc.want, tc.ok)
		}
	}
}
