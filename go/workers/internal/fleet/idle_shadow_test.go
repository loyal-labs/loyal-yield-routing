package fleet

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func idleTestSnapshot() MarketSnapshot {
	target := ReserveIdentity{Address: testIdentity(50), Market: testIdentity(51), Mint: USDCMint}
	return MarketSnapshot{Slot: 1000, ObservedAt: time.Now(), MintExpiresAt: map[string]time.Time{USDCMint: time.Now().Add(5 * time.Minute)}, Reserves: map[string]ReserveState{target.Address: {ReserveIdentity: target, Slot: 1000, SupplyAPYBPS: 900, TotalSupplyUSDMicros: 1_000_000_000_000_000, EconomicLifetimeMillis: 300_000}}}
}

func TestIdleShadowChecksCoverIdleOnlyAndMixedVaults(t *testing.T) {
	snapshot := idleTestSnapshot()
	target := testIdentity(50)
	all := []FleetVault{
		{Position: VaultPosition{VaultID: 1}},
		{Position: VaultPosition{VaultID: 1, Mint: USDCMint, AmountRaw: 1_000_000_000}, IdleTokenAccount: "idle-mixed", AllowedTargets: []string{target}},
		{Position: VaultPosition{VaultID: 2, Mint: USDCMint, AmountRaw: 3_000_000}, IdleTokenAccount: "idle-only", AllowedTargets: []string{target}},
		{Position: VaultPosition{VaultID: 2, Mint: USDTMint, AmountRaw: 1_000_000}, IdleTokenAccount: "idle-other-mint"},
	}
	reserves, summary := idleShadowChecks(snapshot, all)
	if len(reserves) != 1 || summary["idleSourceCount"] != 3 || summary["idleVaultCount"] != 2 || summary["idleOnlyVaultCount"] != 1 || summary["combinedSourceVaultCount"] != 2 || summary["eligibleIdleSourceCount"] != 1 {
		t.Fatalf("unexpected coverage: %v", summary)
	}
	reasons := summary["sourceReasonCounts"].(map[string]int)
	if reasons["below_minimum_net_gain"] != 1 || reasons["no_policy_eligible_target"] != 1 {
		t.Fatalf("unexpected reasons: %v", reasons)
	}
	data, _ := json.Marshal(summary)
	if strings.Contains(string(data), "idle-mixed") || strings.Contains(string(data), "idle-only") {
		t.Fatal("per-account data leaked into summary")
	}
	if summary["publishedCount"] != 0 || summary["candidateChecksOnly"] != true {
		t.Fatal("diagnostics mistaken for selected work")
	}
}

func TestIdleShadowEconomicsAndSafetyFences(t *testing.T) {
	target := testIdentity(50)
	p := VaultPosition{VaultID: 1, Mint: USDCMint, AmountRaw: 1_000_000_000}
	d := planIdleDeposit(idleTestSnapshot(), p, target)
	if !d.Eligible || d.SourceReserve != "" || d.SourceAPYBPS != 0 || d.RouteKind != "idle_vault_deposit" || d.EstimatedCostUSDMicros != 500_000 {
		t.Fatalf("unexpected idle check: %+v", d)
	}
	for _, tc := range []struct {
		name, reason string
		change       func(*MarketSnapshot, *VaultPosition)
	}{
		{"small", "below_minimum_notional", func(_ *MarketSnapshot, p *VaultPosition) { p.AmountRaw = 999_999 }},
		{"wrong_mint", "identity_mismatch", func(s *MarketSnapshot, p *VaultPosition) {
			p.Mint = USDTMint
			s.MintExpiresAt[USDTMint] = time.Now().Add(5 * time.Minute)
		}},
		{"blocked", "owned_by_other_job", func(_ *MarketSnapshot, p *VaultPosition) { p.BlockedReason = "owned_by_other_job" }},
		{"stale_target", "target_explicitly_stale", func(s *MarketSnapshot, _ *VaultPosition) {
			r := s.Reserves[target]
			r.LastUpdateStale = true
			s.Reserves[target] = r
		}},
		{"expired", "idle_market_evidence_lifetime_too_short", func(s *MarketSnapshot, _ *VaultPosition) { s.MintExpiresAt[USDCMint] = time.Now().Add(-time.Second) }},
		{"capacity", "target_capacity_exhausted", func(_ *MarketSnapshot, p *VaultPosition) { p.TargetCommittedInflowUSDMicros = 1_000_000_000_000_000 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := idleTestSnapshot()
			position := p
			tc.change(&s, &position)
			d := planIdleDeposit(s, position, target)
			if d.Eligible || d.Reason != tc.reason {
				t.Fatalf("unexpected: %+v", d)
			}
		})
	}
	if _, err := PlanFleet(idleTestSnapshot(), []FleetVault{{Position: p, IdleTokenAccount: "idle"}}); err == nil {
		t.Fatal("idle source entered executable planner")
	}
	// Nil pool proves rejection occurs before opening any write transaction.
	if _, err := (&Store{}).Publish(context.Background(), "test", ImmutableMarketEpoch{}, p, d); err == nil {
		t.Fatal("idle candidate reached publication")
	}
}
