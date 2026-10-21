package kamino

import (
	"encoding/json"
	"maps"
	"os"
	"slices"
	"testing"
)

func TestPolicyUniverseMatchesGoldenFixture(t *testing.T) {
	raw, err := os.ReadFile("../../../../../crates/loyal-actions/fixtures/earn_universe_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		KaminoMarkets        map[string][]string `json:"kaminoMarkets"`
		SupportedStableMints []string            `json:"supportedStableMints"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for profile, want := range fixture.KaminoMarkets {
		if got := PolicyMarkets(profile); !slices.Equal(got, want) {
			t.Fatalf("%s markets differ from the Rust presets: got %v want %v", profile, got, want)
		}
	}
	got := slices.Sorted(maps.Keys(policySupportedMints))
	if want := slices.Sorted(slices.Values(fixture.SupportedStableMints)); !slices.Equal(got, want) {
		t.Fatalf("supported mints differ from the Rust registry: got %v want %v", got, want)
	}
}
