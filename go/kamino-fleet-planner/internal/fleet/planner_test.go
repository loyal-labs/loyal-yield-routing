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
	position := VaultPosition{VaultID: 1, SourceReserve: "source", Market: "market", Mint: USDCMint, AmountRaw: 10_000_000_000_000, SourceCollateralAmountRaw: 9_000_000_000_000, SourceAmountSemantics: "kamino_collateral_deposited", SnapshotID: 7, ObservedSlot: 499, ObservedAt: now}
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
		{"expiring evidence", func(s *MarketSnapshot, _ *VaultPosition) {
			reserve := s.Reserves["target"]
			reserve.EconomicLifetimeMillis = 69_999
			s.Reserves["target"] = reserve
		}, "economic_evidence_lifetime_too_short"},
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
