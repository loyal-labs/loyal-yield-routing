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
