package fleet

import (
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
		{"capacity exhausted", func(_ *MarketSnapshot, p *VaultPosition) {
			p.TargetCommittedInflowUSDMicros = 4_000_000_000_000
		}, "target_capacity_exhausted"},
		{"amount exceeds capacity", func(_ *MarketSnapshot, p *VaultPosition) {
			p.AmountRaw = 4_000_000_000_001
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

func TestPlanMatchesProductionRustEconomicFixture(t *testing.T) {
	now := time.Now().UTC()
	const (
		source       = "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo"
		target       = "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z"
		sourceMarket = "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y"
		targetMarket = "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8"
	)
	snapshot := MarketSnapshot{
		Slot: 443_977_358, ObservedAt: now, Hash: "production-fixture",
		Reserves: map[string]ReserveState{
			source: {ReserveIdentity: ReserveIdentity{Address: source, Market: sourceMarket, Mint: USDCMint}, Slot: 443_977_358, SupplyAPYBPS: 81, TotalSupplyUSDMicros: 3_440_032_614_297, EconomicLifetimeMillis: 300_000},
			target: {ReserveIdentity: ReserveIdentity{Address: target, Market: targetMarket, Mint: USDCMint}, Slot: 443_977_358, SupplyAPYBPS: 920, TotalSupplyUSDMicros: 73_554_854_888_416, EconomicLifetimeMillis: 300_000},
		},
	}
	position := VaultPosition{
		VaultID: 1469, SourceReserve: source, Market: sourceMarket, Mint: USDCMint,
		AmountRaw: 79_728_595, SourceCollateralAmountRaw: 75_728_931,
		SourceAmountSemantics: amountSemanticsKaminoCollateralDeposited,
		SnapshotID:            27_435_922, ObservedSlot: 443_977_220, ObservedAt: now,
	}

	decision := Plan(snapshot, position, source, target)
	if !decision.Eligible {
		t.Fatalf("production fixture was rejected: %s", decision.Reason)
	}
	if decision.SourceAPYBPS != 81 || decision.TargetAPYBPS != 919 || decision.EdgeBPS != 838 ||
		decision.AnnualYieldGainUSDMicros != 6_675_120 || decision.ExpectedNetGainUSDMicros != 346_686 ||
		decision.EconomicPriority != 48 || decision.EstimatedCostLamports != 17_334 {
		t.Fatalf("Go/Rust production economic parity drifted: %+v", decision)
	}
}

func TestEconomicKeyIgnoresSlotButNotEconomics(t *testing.T) {
	snapshot, position := eligibleFixture()
	first := Plan(snapshot, position, "source", "target")
	snapshot.Slot++
	snapshot.ObservedAt = snapshot.ObservedAt.Add(time.Second)
	second := Plan(snapshot, position, "source", "target")
	if economicKey("mainnet-beta", position, first) != economicKey("mainnet-beta", position, second) {
		t.Fatal("slot-only churn changed economic identity")
	}
	second.TargetAPYBPS++
	if economicKey("mainnet-beta", position, first) == economicKey("mainnet-beta", position, second) {
		t.Fatal("material economics did not change identity")
	}
}
