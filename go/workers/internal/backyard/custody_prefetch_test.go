package backyard

import (
	"context"
	"errors"
	"testing"
)

// A5 step 4: the pre-decision journal read runs while the spend is prepared.
// The finisher must give the serial proof for the same inputs and refuse a
// custody or config change after the read.
func TestPrefetchedOwnershipProofMatchesSerialAndCatchesLaterChanges(t *testing.T) {
	cfg := custodyAttributionConfig()
	spend := custodyAttributionRepayExpected(3_100_000_000, 600_000_000, 6_000_000_000, 8_500_000_000)
	lease := RouteLease{RouteKey: cfg.RouteKey, Owner: "owner", FencingToken: 7}
	inputs := sharedCustodyProofInputs{
		planning: &routePlanningState{routeKey: cfg.RouteKey, generation: 11, lease: &lease},
		evidence: sharedCustodyAttributionEvidence{Rows: []custodyAttributionRow{
			custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 200),
			custodyAttributionFundingRow(t, "auto-fund", "sig-fund", 100),
		}},
	}
	ctx := context.Background()
	proof, err := finishPrefetchedOwnershipProof(ctx, cfg, cfg, spend, inputs, nil, 3_100_000_000, 300, nil)
	if err != nil || proof.ExcludedOperation != "" || proof.Generation != 11 {
		t.Fatalf("prefetched proof refused or malformed: %v %+v", err, proof)
	}
	serialInputs := inputs
	serialInputs.spend = sharedCustodySpendRaw(spend, cfg)
	serial, err := finishSharedCustodySpendProof(ctx, cfg, spend, serialInputs, 3_100_000_000, 300, nil)
	if err != nil || serial.Digest != proof.Digest {
		t.Fatal("prefetched proof differs from the serial proof", err)
	}
	// Custody moved after the journal read: the prepared balance no longer
	// matches the journal tip.
	if _, err := finishPrefetchedOwnershipProof(ctx, cfg, cfg, custodyAttributionRepayExpected(3_100_000_001, 600_000_000, 6_000_000_000, 8_500_000_000), inputs, nil, 3_100_000_001, 300, nil); custodyAttributionHoldReason(t, err) != "custody_attribution_balance_mismatch" {
		t.Fatalf("custody change after the prefetch accepted: %v", err)
	}
	// A journal row newer than the prepared observation.
	if _, err := finishPrefetchedOwnershipProof(ctx, cfg, cfg, spend, inputs, nil, 3_100_000_000, 150, nil); custodyAttributionHoldReason(t, err) != "custody_attribution_snapshot_drift" {
		t.Fatalf("observation older than the journal accepted: %v", err)
	}
	other := cfg
	other.Lane = "Ethena/USDe/PYUSD"
	if _, err := finishPrefetchedOwnershipProof(ctx, cfg, other, spend, inputs, nil, 3_100_000_000, 300, nil); custodyAttributionHoldReason(t, err) != "custody_attribution_proof_drift" {
		t.Fatalf("prefetch for another custody config accepted: %v", err)
	}
	readErr := errors.New("lease lost")
	if _, err := finishPrefetchedOwnershipProof(ctx, cfg, cfg, spend, inputs, readErr, 3_100_000_000, 300, nil); !errors.Is(err, readErr) {
		t.Fatalf("journal read error dropped: %v", err)
	}
	if proof, err := finishPrefetchedOwnershipProof(ctx, cfg, cfg, custodyAttributionFundingExpected(10_000_000_000, 8_000_000_000, nil), inputs, nil, 0, 300, nil); err != nil || proof.SpendRaw != 0 {
		t.Fatal("zero-spend operation produced a proof", err)
	}
}

func TestPreDecisionSeamUsesThePrefetchedProof(t *testing.T) {
	manifest := embeddedTestManifest(t)
	cfg := autoSharedPYUSDAttributionConfig(autoAUTOPYUSD, productionRouteKey)
	effects := custodyAttributionRepayExpected(3_100_000_000, 600_000_000, 6_000_000_000, 8_500_000_000)
	decision := Decision{Action: DeleverRouteStep, StrategyKey: cfg.Lane, AmountRaw: 2_500_000_000, IdempotencyKey: "k", Reason: "r"}
	proof := custodyAdmissionProofFixture(t, cfg, productionRouteKey, effects, 3_100_000_000, 300, 1, 7, "worker")
	serialCalls, prefetchedCalls := 0, 0
	w := &Worker{routeKey: productionRouteKey, manifest: manifest, runtime: tickRuntime{custodyOwnershipProof: func(context.Context, RouteManifest, sharedCustodyAttributionConfig, ExpectedEffects, uint64, int64) (sharedCustodyAdmissionProof, error) {
		serialCalls++
		return proof, nil
	}}}
	prefetched := custodyProofFinisher(func(_ context.Context, got sharedCustodyAttributionConfig, _ ExpectedEffects, raw uint64, slot int64) (sharedCustodyAdmissionProof, error) {
		prefetchedCalls++
		if got != cfg || raw != 3_100_000_000 || slot != 300 {
			t.Fatalf("prefetched finisher got drifted inputs: %+v %d %d", got, raw, slot)
		}
		return proof, nil
	})
	observation := Observation{Snapshot: Snapshot{DebtIdleRaw: 3_100_000_000, Slot: 300}}
	if err := w.observePreDecisionCustodyOwnershipProof(context.Background(), &observation, decision, effects, prefetched); err != nil {
		t.Fatal(err)
	}
	if prefetchedCalls != 1 || serialCalls != 0 || observation.carriedCustodyOwnershipProof() == nil {
		t.Fatalf("prefetched proof not used: prefetched=%d serial=%d", prefetchedCalls, serialCalls)
	}
	// Only AUTO actions that can debit the PYUSD custody prefetch.
	s := Snapshot{DebtIdleRaw: 1}
	for _, c := range []struct {
		d    Decision
		want bool
	}{
		{Decision{Action: SwapDebtToUSDCStep, StrategyKey: autoAUTOPYUSD.Lane}, true},
		{Decision{Action: DeleverRouteStep, StrategyKey: autoAUTOPYUSD.Lane}, true},
		{Decision{Action: SwapDebtToCollateralStep, StrategyKey: autoAUTOPYUSD.Lane}, true},
		{Decision{Action: ReportNAV, StrategyKey: autoAUTOPYUSD.Lane}, false},
		{Decision{Action: SwapStableToCollateralStep, StrategyKey: autoAUTOPYUSD.Lane}, false},
		{Decision{Action: SwapDebtToUSDCStep, StrategyKey: "Ethena/USDe/PYUSD"}, false},
	} {
		if custodyProofPrefetchAction(c.d, s) != c.want {
			t.Fatalf("prefetch choice for %+v", c.d)
		}
	}
	if custodyProofPrefetchAction(Decision{Action: SwapDebtToUSDCStep, StrategyKey: autoAUTOPYUSD.Lane}, Snapshot{}) {
		t.Fatal("prefetch with an empty PYUSD custody")
	}
}
