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
		"EARN_MAX_DELEGATE":    "delegate",
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
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddress != ":9999" || cfg.ReplayOverlapSlots != 32 || cfg.ReconciliationWorkers != 4 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}
