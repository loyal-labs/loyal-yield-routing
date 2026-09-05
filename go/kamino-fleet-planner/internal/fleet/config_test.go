package fleet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestConfigAcceptsVerifiedMainnetPublish(t *testing.T) {
	config := Config{
		DatabaseURL: "postgres://example", TimescaleURL: "postgres://evidence", TimescaleSchema: "kamino", RPCURL: "https://rpc.example", Cluster: "mainnet-beta",
		Mode: ModePublish, VaultID: 1, PollInterval: time.Second, SlotDuration: 314 * time.Millisecond,
		Source: ReserveIdentity{Address: testIdentity(1), Market: testIdentity(2), Mint: USDCMint},
		Target: ReserveIdentity{Address: testIdentity(3), Market: testIdentity(4), Mint: USDCMint},
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("mainnet publication was rejected after the replacement gate passed: %v", err)
	}
}

func TestConfigRejectsShadowRevalidation(t *testing.T) {
	config := Config{
		DatabaseURL: "postgres://example", TimescaleURL: "postgres://evidence", TimescaleSchema: "kamino", RPCURL: "https://rpc.example", Cluster: "mainnet-beta",
		Mode: ModePublish, PollInterval: time.Second, SlotDuration: 400 * time.Millisecond,
		RevalidatorEnabled: true, KLendProxyPath: "/proxy", KLendProxySHA256: strings.Repeat("a", 64), DelegatedSigner: testIdentity(9),
		RevalidationOwner: "go", RevalidationLeaseTTL: time.Minute, RevalidationPollInterval: time.Second, RevalidationConcurrency: 1, RevalidationComputeLimit: defaultComputeLimit,
	}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	config.Mode = ModeShadow
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "shadow mode cannot enable durable revalidation") {
		t.Fatalf("unsafe shadow configuration accepted: %v", err)
	}
	config.RevalidatorEnabled = false
	if err := config.Validate(); err != nil {
		t.Fatalf("read-only shadow rejected: %v", err)
	}
}

func TestConfigRequiresCrossMintSignerAndMultipleSupportedMints(t *testing.T) {
	config := Config{
		DatabaseURL: "postgres://example", TimescaleURL: "postgres://evidence", TimescaleSchema: "kamino", RPCURL: "https://rpc.example", Cluster: "mainnet-beta",
		Mode: ModePublish, PollInterval: time.Second, SlotDuration: 400 * time.Millisecond, CrossMintEnabled: true, CrossMintMaxValueLossBPS: 50, CrossMintMaxSlippageBPS: 50,
		EnabledStableMints: []string{USDCMint},
	}
	if err := config.Validate(); err == nil {
		t.Fatal("cross-mint planning with one mint and no signer was accepted")
	}
	config.EnabledStableMints = []string{USDCMint, PYUSDMint}
	config.DelegatedSigner = testIdentity(9)
	if err := config.Validate(); err != nil {
		t.Fatalf("valid cross-mint configuration rejected: %v", err)
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
