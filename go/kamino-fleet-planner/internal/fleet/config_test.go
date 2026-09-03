package fleet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestConfigBlocksUnverifiedMainnetPublish(t *testing.T) {
	config := Config{
		DatabaseURL: "postgres://example", TimescaleURL: "postgres://evidence", TimescaleSchema: "kamino", RPCURL: "https://rpc.example", Cluster: "mainnet-beta",
		Mode: ModePublish, VaultID: 1, PollInterval: time.Second, SlotDuration: 314 * time.Millisecond,
		Source: ReserveIdentity{Address: testIdentity(1), Market: testIdentity(2), Mint: USDCMint},
		Target: ReserveIdentity{Address: testIdentity(3), Market: testIdentity(4), Mint: USDCMint},
	}
	if err := config.Validate(); err == nil {
		t.Fatal("unverified mainnet publication was accepted")
	}
	config.Mode = ModeShadow
	if err := config.Validate(); err != nil {
		t.Fatalf("mainnet shadow mode was rejected: %v", err)
	}
}

func TestFetchKaminoSlotDurationMatchesMonitorInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/slots/duration" {
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"recentSlotDurationInMs":314}`))
	}))
	defer server.Close()

	duration, err := fetchKaminoSlotDuration(context.Background(), server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if duration != 314*time.Millisecond {
		t.Fatalf("unexpected duration %s", duration)
	}
}
