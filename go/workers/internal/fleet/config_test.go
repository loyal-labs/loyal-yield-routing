package fleet

import (
	"testing"
	"time"
)

func TestConfigAcceptsVerifiedMainnetPublish(t *testing.T) {
	config := Config{
		DatabaseURL: "postgres://example", TimescaleURL: "postgres://evidence", TimescaleSchema: "kamino", RPCURL: "https://rpc.example", Cluster: "mainnet-beta",
		PollInterval: time.Second, SlotDuration: 314 * time.Millisecond,
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("mainnet publication was rejected after the replacement gate passed: %v", err)
	}
}

func TestConfigRequiresCrossMintSignerAndMultipleSupportedMints(t *testing.T) {
	config := Config{
		DatabaseURL: "postgres://example", TimescaleURL: "postgres://evidence", TimescaleSchema: "kamino", RPCURL: "https://rpc.example", Cluster: "mainnet-beta",
		PollInterval: time.Second, SlotDuration: 400 * time.Millisecond, CrossMintEnabled: true, CrossMintMaxValueLossBPS: 50, CrossMintMaxSlippageBPS: 50,
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
