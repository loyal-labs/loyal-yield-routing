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
