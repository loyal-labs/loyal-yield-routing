package fleet

import (
	"encoding/json"
	"testing"
	"time"
)

func crossMintCapacityObservationFixture(now time.Time) (CrossMintActivationPreparation, RevalidationLease) {
	return CrossMintActivationPreparation{ObservedAt: now.Add(-time.Second), ObservedSlot: 1000, SourceAPYBPS: 100, TargetAPYBPS: 900, TargetObservedSupplyUSDMicros: 100_000_000}, RevalidationLease{Cluster: "capacity-fixture", TargetReserve: testIdentity(62), TargetLiquidityMint: USDTMint, ExecutionPlan: json.RawMessage(`{"source_apy_bps":100,"observed_target_apy_bps":900}`)}
}

func TestCrossMintActivationObservationKeepsFreshAndFrozenEconomicsSeparate(t *testing.T) {
	now := time.Now()
	out, lease := crossMintCapacityObservationFixture(now)
	if err := validateCrossMintActivationObservation(out, lease, now); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*CrossMintActivationPreparation, *RevalidationLease){
		"changed source APY":  func(p *CrossMintActivationPreparation, _ *RevalidationLease) { p.SourceAPYBPS++ },
		"changed target APY":  func(p *CrossMintActivationPreparation, _ *RevalidationLease) { p.TargetAPYBPS++ },
		"waiting ALT":         func(p *CrossMintActivationPreparation, _ *RevalidationLease) { p.WaitingALT = true },
		"missing observation": func(p *CrossMintActivationPreparation, _ *RevalidationLease) { p.ObservedAt = time.Time{} },
		"future observation":  func(p *CrossMintActivationPreparation, _ *RevalidationLease) { p.ObservedAt = now.Add(time.Nanosecond) },
		"expired observation": func(p *CrossMintActivationPreparation, _ *RevalidationLease) {
			p.ObservedAt = now.Add(-15*time.Second - time.Nanosecond)
		},
		"zero slot":       func(p *CrossMintActivationPreparation, _ *RevalidationLease) { p.ObservedSlot = 0 },
		"negative supply": func(p *CrossMintActivationPreparation, _ *RevalidationLease) { p.TargetObservedSupplyUSDMicros = -1 },
		"missing source": func(_ *CrossMintActivationPreparation, l *RevalidationLease) {
			l.ExecutionPlan = json.RawMessage(`{"observed_target_apy_bps":900}`)
		},
		"null source": func(_ *CrossMintActivationPreparation, l *RevalidationLease) {
			l.ExecutionPlan = json.RawMessage(`{"source_apy_bps":null,"observed_target_apy_bps":900}`)
		},
		"missing target": func(_ *CrossMintActivationPreparation, l *RevalidationLease) {
			l.ExecutionPlan = json.RawMessage(`{"source_apy_bps":100}`)
		},
		"quoted economics": func(_ *CrossMintActivationPreparation, l *RevalidationLease) {
			l.ExecutionPlan = json.RawMessage(`{"source_apy_bps":"100","observed_target_apy_bps":900}`)
		},
		"fractional economics": func(_ *CrossMintActivationPreparation, l *RevalidationLease) {
			l.ExecutionPlan = json.RawMessage(`{"source_apy_bps":100.5,"observed_target_apy_bps":900}`)
		},
		"SQL overflow": func(_ *CrossMintActivationPreparation, l *RevalidationLease) {
			l.ExecutionPlan = json.RawMessage(`{"source_apy_bps":9223372036854775808,"observed_target_apy_bps":900}`)
		},
		"generic plan": func(_ *CrossMintActivationPreparation, l *RevalidationLease) {
			l.ExecutionPlan = json.RawMessage(`{"trusted":true}`)
		},
		"malformed plan": func(_ *CrossMintActivationPreparation, l *RevalidationLease) {
			l.ExecutionPlan = json.RawMessage(`{"source_apy_bps":`)
		},
		"negative matching source": func(p *CrossMintActivationPreparation, l *RevalidationLease) {
			p.SourceAPYBPS = -1
			l.ExecutionPlan = json.RawMessage(`{"source_apy_bps":-1,"observed_target_apy_bps":900}`)
		},
		"negative matching target": func(p *CrossMintActivationPreparation, l *RevalidationLease) {
			p.TargetAPYBPS = -1
			l.ExecutionPlan = json.RawMessage(`{"source_apy_bps":100,"observed_target_apy_bps":-1}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, l := out, lease
			mutate(&p, &l)
			if err := validateCrossMintActivationObservation(p, l, now); err == nil {
				t.Fatal("invalid or changed source economics accepted")
			}
		})
	}
	out.ObservedAt = now.Add(-15 * time.Second)
	out.TargetObservedSupplyUSDMicros = 0
	if err := validateCrossMintActivationObservation(out, lease, now); err != nil {
		t.Fatal("exact evidence boundary or empty reserve supply rejected:", err)
	}
	lease.ExecutionPlan = json.RawMessage(`{"source_apy_bps":0,"observed_target_apy_bps":0,"other_immutable_fields":{}}`)
	out.SourceAPYBPS, out.TargetAPYBPS = 0, 0
	if err := validateCrossMintActivationObservation(out, lease, now); err != nil {
		t.Fatal("legitimate zero APY was confused with missing economics:", err)
	}
}
