package fleet

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

func TestEarnStablecoinsMatchGoldenFixture(t *testing.T) {
	raw, err := os.ReadFile("../../../../crates/loyal-actions/fixtures/earn_universe_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Stablecoins []struct {
			Mint         string `json:"mint"`
			TokenProgram string `json:"tokenProgram"`
		} `json:"stablecoins"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	var want []EarnStablecoin
	for _, stable := range fixture.Stablecoins {
		want = append(want, EarnStablecoin{Mint: stable.Mint, TokenProgram: stable.TokenProgram})
	}
	if got := EarnStablecoins(); !slices.Equal(got, want) {
		t.Fatalf("Earn stablecoins differ from the Rust registry: got %v want %v", got, want)
	}
}
