package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LASERSTREAM_ENDPOINT", "https://example.invalid")
	t.Setenv("EARN_MAX_DELEGATE", "11111111111111111111111111111111")
	// Secrets arrive as systemd credential files, never as environment.
	dir := t.TempDir()
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	for name, value := range map[string]string{
		"HELIUS_API_KEY":             "fixture",
		"SOLANA_RPC_URL":             "https://rpc.invalid",
		"NEON_DATABASE_URL":          "postgresql://fixture",
		"OBSERVER_APPS_DATABASE_URL": "postgresql://apps-fixture",
		"TIMESCALEDB_URL":            "postgresql://fixture",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFromEnvRequiresEveryProductionDependency(t *testing.T) {
	t.Setenv("LASERSTREAM_ENDPOINT", "")
	t.Setenv("EARN_MAX_DELEGATE", "")
	t.Setenv("CREDENTIALS_DIRECTORY", t.TempDir())
	t.Setenv("HELIUS_API_KEY", "environment-is-not-a-credential")
	_, err := FromEnv()
	if err == nil {
		t.Fatal("missing production dependencies were accepted")
	}
	for _, name := range []string{"EARN_MAX_DELEGATE", "HELIUS_API_KEY", "LASERSTREAM_ENDPOINT", "NEON_DATABASE_URL", "SOLANA_RPC_URL", "TIMESCALEDB_URL", "OBSERVER_APPS_DATABASE_URL"} {
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
	t.Setenv("SOLANA_CLUSTER", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HeliusAPIKey != "fixture" || cfg.ReplayOverlapSlots != 32 || cfg.ReconciliationWorkers != 4 || cfg.Cluster != "mainnet-beta" {
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
	for _, forbidden := range []string{"POLICY_KEYPAIR", "signing-capability", "HELIUS_API_KEY", "OBSERVER_APPS_DATABASE_URL", "apps-fixture"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("bridge environment leaked %s: %s", forbidden, joined)
		}
	}
	for _, required := range []string{"NEON_DATABASE_URL=postgresql://fixture", "SOLANA_RPC_URL=https://rpc.invalid", "SOLANA_CLUSTER=mainnet-beta", "EARN_MAX_DELEGATE=11111111111111111111111111111111", "TIMESCALEDB_URL=postgresql://fixture", "EARN_RECONCILIATION_CONCURRENCY=4", "AUTODEPOSIT_RECONCILIATION_CONCURRENCY=0"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("bridge environment omitted %s: %s", required, joined)
		}
	}
}

func TestObserverCannotStartCompetingAutodepositConsumer(t *testing.T) {
	for _, value := range []string{"", "0", "4", "-1", "invalid"} {
		t.Run(value, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("AUTODEPOSIT_RECONCILIATION_CONCURRENCY", value)
			cfg, err := FromEnv()
			if value != "" && value != "0" {
				if err == nil || !strings.Contains(err.Error(), "AUTODEPOSIT_RECONCILIATION_CONCURRENCY") {
					t.Fatalf("competing family ownership accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, setting := range cfg.BridgeEnvironment() {
				if strings.HasPrefix(setting, "AUTODEPOSIT_RECONCILIATION_CONCURRENCY=") && setting != "AUTODEPOSIT_RECONCILIATION_CONCURRENCY=0" {
					t.Fatalf("bridge gained Autodeposit ownership: %s", setting)
				}
			}
		})
	}
}

func TestFromEnvDefaultsBridgeStartupAndReadyBounds(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BridgeStartupTimeout <= 0 {
		t.Fatalf("bridge startup %s, want positive default", cfg.BridgeStartupTimeout)
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
