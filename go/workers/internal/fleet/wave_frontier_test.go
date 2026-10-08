package fleet

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func frozenWaveFixture() (MarketSnapshot, []FleetVault, WaveLimits) {
	snapshot := oracleSnapshot()
	oracleReserve(&snapshot, "source", USDCMint, 100, 1_000_000_000_000)
	oracleReserve(&snapshot, "same", USDCMint, 700, 1_000_000_000_000)
	oracleReserve(&snapshot, "cross", USDTMint, 3000, 1_000_000_000_000)
	vaults := make([]FleetVault, 3)
	for i := range vaults {
		v := oracleVault(snapshot, int64(i+1), "source", 4_000_000_000, "same")
		v.CrossMintTargets = map[string]CrossMintPolicyBindings{"cross": oracleCrossPolicy(v)}
		v.CrossMintMaxValueLossBPS = 50
		v.CommittedInflows = map[string]int64{"cross": 5_000_000_000}
		v.CommittedOutflows = map[string]int64{"source": 2_000_000_000}
		vaults[i] = v
	}
	return snapshot, vaults, WaveLimits{3, 20_000_000_000, 3, 3}
}

func TestWaveFrozenFrontierConcurrentPurityAndOwnedOutputBindings(t *testing.T) {
	snapshot, vaults, limits := frozenWaveFixture()
	before, err := json.Marshal(struct {
		Snapshot MarketSnapshot
		Vaults   []FleetVault
	}{snapshot, vaults})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := PlanFleetWithLimitsAt(snapshot, vaults, limits, snapshot.ObservedAt)
	if err != nil || len(baseline.Opportunities) != 3 {
		t.Fatalf("owned cross-mint fixture selected=%d error=%v", len(baseline.Opportunities), err)
	}
	type result struct {
		plan FleetPlan
		err  error
	}
	const readers = 8
	results := make(chan result, readers)
	for range readers {
		go func() {
			plan, err := PlanFleetWithLimitsAt(snapshot, vaults, limits, snapshot.ObservedAt)
			results <- result{plan, err}
		}()
	}
	var independent FleetPlan
	for range readers {
		result := <-results
		if result.err != nil || !reflect.DeepEqual(baseline, result.plan) {
			t.Fatalf("immutable parallel frontier changed complete publication: %v", result.err)
		}
		independent = result.plan
	}
	after, err := json.Marshal(struct {
		Snapshot MarketSnapshot
		Vaults   []FleetVault
	}{snapshot, vaults})
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("planning mutated source metadata, permissions, amounts or committed frontier")
	}
	for i := range baseline.Opportunities {
		a, b := baseline.Opportunities[i].Decision.PolicyBindings, independent.Opportunities[i].Decision.PolicyBindings
		if a == nil || b == nil || a == b || a.Swap.PolicyAccount == "" {
			t.Fatal("published cross-mint binding lacks independent owned identity")
		}
		original := *b
		a.Swap.ManifestFingerprint = "mutated returned decision"
		if *b != original {
			t.Fatal("separate planning call shares mutable output policy bindings")
		}
		inputBinding := vaults[int(baseline.Opportunities[i].Decision.VaultID)-1].CrossMintTargets["cross"]
		if inputBinding != original {
			t.Fatal("returned binding aliases caller permission metadata")
		}
	}
}

func TestWaveFrozenFrontierFullPublicationPermissionPermutationInvariant(t *testing.T) {
	snapshot, vaults, limits := frozenWaveFixture()
	baseline, err := PlanFleetWithLimitsAt(snapshot, vaults, limits, snapshot.ObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	for i := range vaults {
		// Duplicate permissions and absent catalog entries cannot create an
		// extra source/target edge, conflict admission or reservation charge.
		vaults[i].AllowedTargets = []string{"missing", "same", "same", "cross"}
	}
	vaults[0], vaults[2] = vaults[2], vaults[0]
	permuted, err := PlanFleetWithLimitsAt(snapshot, vaults, limits, snapshot.ObservedAt)
	if err != nil || !reflect.DeepEqual(baseline, permuted) {
		t.Fatalf("permission/loader permutation changed decisions, canonical JSON, identity or rejections: %v", err)
	}
}
