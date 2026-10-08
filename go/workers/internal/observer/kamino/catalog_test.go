package kamino

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCatalogEnrichesExactReserveIdentity(t *testing.T) {
	market, mint := "market", "mint"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/kamino-market/market/reserves/metrics":
			fmt.Fprint(w, `[{"reserve":"reserve","liquidityToken":"USDC","liquidityTokenMint":"mint","supplyApy":"0.05","borrowApy":0.08,"totalSupplyUsd":"100","totalBorrowUsd":20}]`)
		case "/slots/duration":
			fmt.Fprint(w, `{"recentSlotDurationInMs":"412.5"}`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	client := NewCatalogClient(server.URL, time.Second)
	targets, err := client.Enrich(context.Background(), []Target{{Reserve: "reserve", Market: &market, LiquidityMint: &mint}})
	if err != nil {
		t.Fatal(err)
	}
	if targets[0].APISupplyAPY == nil || *targets[0].APISupplyAPY != 0.05 || targets[0].Symbol == nil || *targets[0].Symbol != "USDC" {
		t.Fatalf("enriched target = %+v", targets[0])
	}
	duration, err := client.SlotDuration(context.Background())
	if err != nil || duration != 412.5 {
		t.Fatalf("slot duration = %f, %v", duration, err)
	}
}

func TestCatalogRejectsChangedMintIdentity(t *testing.T) {
	market, mint := "market", "expected"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"reserve":"reserve","liquidityTokenMint":"different"}]`)
	}))
	defer server.Close()
	_, err := NewCatalogClient(server.URL, time.Second).Enrich(context.Background(), []Target{{Reserve: "reserve", Market: &market, LiquidityMint: &mint}})
	if err == nil {
		t.Fatal("changed Kamino mint identity was accepted")
	}
}

// Rust watches the Backyard AUTO collateral and PYUSD debt reserves as pinned
// supplemental targets appended after the API catalog (loyal-kamino-data
// supplemental_observation_targets). The Kamino metrics API is never asked for
// them, so their absence there must not drop them from the watched set.
func TestObservationTargetsWatchBackyardAutoReservesWithoutAPILookup(t *testing.T) {
	market, mint := "market", "mint"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/kamino-market/market/reserves/metrics" {
			t.Errorf("supplemental reserve resolved through the API: %s", request.URL.Path)
			http.NotFound(w, request)
			return
		}
		fmt.Fprint(w, `[{"reserve":"reserve","liquidityTokenMint":"mint","supplyApy":0.05}]`)
	}))
	defer server.Close()
	targets, err := NewCatalogClient(server.URL, time.Second).ObservationTargets(context.Background(), []Target{{Reserve: "reserve", Market: &market, LiquidityMint: &mint}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"reserve": "mint",
		"G85AgoBdW8zSQBq5i4E8aBLCDdRYGgK44CzU1d1NdBzX": "GNE6oDS6jHrfaV3GQVVCCp37fDnT7PiPuewMKBj2bqNm",
		"6A8D3ExQ4CdiZTBmij7MScUeKsgs6mSHksYzJbiY61FM": "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo",
	}
	if len(targets) != len(want) {
		t.Fatalf("watched reserves = %+v, want %d", targets, len(want))
	}
	for _, target := range targets {
		if target.LiquidityMint == nil || want[target.Reserve] != *target.LiquidityMint {
			t.Fatalf("watched reserve %s has mint %v", target.Reserve, target.LiquidityMint)
		}
		if target.Reserve != "reserve" && (target.Market == nil || *target.Market != "Btu8835QDYgdTnMJJBSidbfQhrZzryZbMhCpty6h6Xdk" || target.MarketName == nil || *target.MarketName != "Auto Market" || target.APISupplyAPY != nil) {
			t.Fatalf("Backyard AUTO reserve identity = %+v", target)
		}
	}
}

func TestObservationTargetsRejectConflictingSupplementalIdentity(t *testing.T) {
	market, mint := "Btu8835QDYgdTnMJJBSidbfQhrZzryZbMhCpty6h6Xdk", "different-mint"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"reserve":"G85AgoBdW8zSQBq5i4E8aBLCDdRYGgK44CzU1d1NdBzX","liquidityTokenMint":"different-mint"}]`)
	}))
	defer server.Close()
	if _, err := NewCatalogClient(server.URL, time.Second).ObservationTargets(context.Background(), []Target{{Reserve: "G85AgoBdW8zSQBq5i4E8aBLCDdRYGgK44CzU1d1NdBzX", Market: &market, LiquidityMint: &mint}}); err == nil {
		t.Fatal("reserve with two observation identities was accepted")
	}
}
