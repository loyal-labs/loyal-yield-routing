package backyardrwa

import (
	"encoding/json"
	"testing"
)

func TestPhase3InspectionResolvesProductionBindingsWithoutExecution(t *testing.T) {
	encoded, err := InspectPhase3Runtime([]string{PhaseOneLaneID, SelectedRouteID, "AUTO/AUTO/PYUSD", "Ethena/USDe/PYUSD", "Prime/PRIME/PYUSD", "Prime/PRIME/USDS", "unknown/asset/debt"})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		ReadOnly bool `json:"readOnly"`
		Lanes    []struct {
			Lane     string       `json:"lane"`
			Resolved bool         `json:"resolved"`
			Binding  RuntimeRoute `json:"binding"`
		} `json:"lanes"`
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	if !result.ReadOnly || len(result.Lanes) != 7 || result.Lanes[6].Resolved {
		t.Fatalf("inspection did not distinguish production bindings from an unknown lane: %s", encoded)
	}
	for _, row := range result.Lanes[:6] {
		bound, err := runtimeRoute(row.Lane)
		if err != nil || !row.Resolved || row.Binding.Kamino != bound.Kamino || row.Binding.DebtCustody != bound.DebtCustody {
			t.Fatalf("inspection diverged from production resolver for %s", row.Lane)
		}
	}
}
