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
		"HELIUS_API_KEY":    "fixture",
		"SOLANA_RPC_URL":    "https://rpc.invalid",
		"NEON_DATABASE_URL": "postgresql://fixture",
		"TIMESCALEDB_URL":   "postgresql://fixture",
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

func TestFromEnvSelectsEarnAPYProfiles(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := FromEnv()
	if err != nil || len(cfg.APYRiskProfiles) != 1 || cfg.APYRiskProfiles[0] != "safe" {
		t.Fatalf("default APY profiles = %v, %v", cfg.APYRiskProfiles, err)
	}
	t.Setenv("DISABLE_EARN_APY_REFRESH", "true")
	if cfg, err = FromEnv(); err != nil || len(cfg.APYRiskProfiles) != 0 {
		t.Fatalf("disabled APY refresh still selected %v, %v", cfg.APYRiskProfiles, err)
	}
	t.Setenv("DISABLE_EARN_APY_REFRESH", "yes")
	if _, err = FromEnv(); err == nil {
		t.Fatal("non-boolean DISABLE_EARN_APY_REFRESH accepted")
	}
}

func TestFromEnvRejectsUnrepresentableBounds(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"LASERSTREAM_HANDOFF_TIMEOUT_SECONDS", "18446744073709551615"},
		{"EARN_RECONCILIATION_CONCURRENCY", "65"},
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

// The Apps hourly crons own the read-model tables until the Phase 2 handover,
// and Rust never wrote them: the observer must not become a second writer
// unless explicitly enabled. The Earn APY writer, which Rust ran, stays on.
func TestFromEnvLeavesReadModelsToAppsUnlessEnabled(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := FromEnv()
	if err != nil || cfg.ReadModelsEnabled || len(cfg.APYRiskProfiles) == 0 {
		t.Fatalf("default read models=%v APY=%v err=%v; want read models off, APY on", cfg.ReadModelsEnabled, cfg.APYRiskProfiles, err)
	}
	t.Setenv("OBSERVER_READ_MODELS_ENABLED", "true")
	if cfg, err = FromEnv(); err != nil || !cfg.ReadModelsEnabled {
		t.Fatalf("explicit enable ignored: %v %v", cfg.ReadModelsEnabled, err)
	}
	t.Setenv("OBSERVER_READ_MODELS_ENABLED", "yes")
	if _, err = FromEnv(); err == nil || !strings.Contains(err.Error(), "OBSERVER_READ_MODELS_ENABLED") {
		t.Fatalf("non-boolean OBSERVER_READ_MODELS_ENABLED accepted: %v", err)
	}
}
