package config

import (
	"strings"
	"testing"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()
	for name, value := range map[string]string{
		"LASERSTREAM_ENDPOINT": "https://example.invalid",
		"HELIUS_API_KEY":       "fixture",
		"EARN_MAX_DELEGATE":    "11111111111111111111111111111111",
		"SOLANA_RPC_URL":       "https://rpc.invalid",
		"NEON_DATABASE_URL":    "postgresql://fixture",
		"TIMESCALEDB_URL":      "postgresql://fixture",
	} {
		t.Setenv(name, value)
	}
}

func TestFromEnvRequiresEveryProductionDependency(t *testing.T) {
	for _, name := range []string{"LASERSTREAM_ENDPOINT", "HELIUS_API_KEY", "EARN_MAX_DELEGATE", "SOLANA_RPC_URL", "NEON_DATABASE_URL", "TIMESCALEDB_URL"} {
		t.Setenv(name, "")
	}
	_, err := FromEnv()
	if err == nil {
		t.Fatal("missing production dependencies were accepted")
	}
	for _, name := range []string{"EARN_MAX_DELEGATE", "HELIUS_API_KEY", "LASERSTREAM_ENDPOINT", "NEON_DATABASE_URL", "SOLANA_RPC_URL", "TIMESCALEDB_URL"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("missing-variable error omitted %s: %v", name, err)
		}
	}
}

func TestFromEnvRejectsInvalidOperationalIntervals(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LASERSTREAM_PROGRESS_TIMEOUT_SECONDS", "not-a-number")
	if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "LASERSTREAM_PROGRESS_TIMEOUT_SECONDS") {
		t.Fatalf("invalid timeout error = %v", err)
	}
}

func TestFromEnvBuildsStrictProductionConfig(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("PORT", "9999")
	t.Setenv("SOLANA_CLUSTER", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddress != ":9999" || cfg.ReplayOverlapSlots != 32 || cfg.ReconciliationWorkers != 4 || cfg.Cluster != "mainnet-beta" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestFromEnvNormalizesProductionClusterAliases(t *testing.T) {
	for _, alias := range []string{"mainnet", "mainnet-beta", "mainnet_beta", "mainnetbeta", " MAINNET "} {
		t.Run(alias, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("SOLANA_CLUSTER", alias)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Cluster != "mainnet-beta" {
				t.Fatalf("cluster = %q, want mainnet-beta", cfg.Cluster)
			}
		})
	}
}

func TestBridgeEnvironmentIsAStrictAllowlist(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SOLANA_CLUSTER", "mainnet-beta")
	t.Setenv("POLICY_KEYPAIR", "signing-capability")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cfg.BridgeEnvironment(), "\n")
	for _, forbidden := range []string{"POLICY_KEYPAIR", "signing-capability", "HELIUS_API_KEY"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("bridge environment leaked %s: %s", forbidden, joined)
		}
	}
	for _, required := range []string{"NEON_DATABASE_URL=postgresql://fixture", "SOLANA_RPC_URL=https://rpc.invalid", "SOLANA_CLUSTER=mainnet-beta", "EARN_MAX_DELEGATE=11111111111111111111111111111111", "TIMESCALEDB_URL=postgresql://fixture", "EARN_RECONCILIATION_CONCURRENCY=4"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("bridge environment omitted %s: %s", required, joined)
		}
	}
}

func TestFromEnvDefaultsBridgeStartupAndReadyBounds(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BridgeStartupTimeout <= 0 || cfg.EarnReadyMaxAge <= 0 {
		t.Fatalf("bridge startup %s / ready max age %s, want positive defaults", cfg.BridgeStartupTimeout, cfg.EarnReadyMaxAge)
	}
}

func TestFromEnvRejectsUnrepresentableBounds(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"LASERSTREAM_HANDOFF_TIMEOUT_SECONDS", "18446744073709551615"},
		{"EARN_DOMAIN_BRIDGE_STARTUP_TIMEOUT_SECONDS", "9223372037"},
		{"EARN_RECONCILIATION_CONCURRENCY", "65"},
		{"AUTODEPOSIT_RECONCILIATION_CONCURRENCY", "18446744073709551615"},
		{"LASERSTREAM_REPLAY_OVERLAP_SLOTS", "9223372036854775808"},
		{"EARN_MAX_DELEGATE", "not-a-public-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(tc.name, tc.value)
			if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("invalid bound accepted or unnamed: %v", err)
			}
		})
	}
}
