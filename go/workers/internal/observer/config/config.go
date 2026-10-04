package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"
)

type Config struct {
	LaserStreamEndpoint   string
	HeliusAPIKey          string
	EarnMaxDelegate       string
	SolanaRPCURL          string
	NeonDatabaseURL       string
	AppsDatabaseURL       string
	TimescaleDatabaseURL  string
	KaminoAPIBase         string
	Cluster               string
	ATAStream             string
	HTTPAddress           string
	BridgeBinary          string
	BridgeStartupTimeout  time.Duration
	EarnReadyMaxAge       time.Duration
	ReplayOverlapSlots    uint64
	WatchRefresh          time.Duration
	VerifyRefresh         time.Duration
	ProgressTimeout       time.Duration
	HandoffTimeout        time.Duration
	ReconciliationWorkers int
	AutodepositWorkers    int
}

// BridgeEnvironment builds the complete environment of the Earn domain bridge
// child. It is a strict allowlist: the child receives only observer data-plane
// configuration and telemetry endpoints. The worker process environment is
// never inherited. The public Earn delegate is verifier input; POLICY_KEYPAIR
// and the Helius API key are excluded.
func (c Config) BridgeEnvironment() []string {
	return []string{
		"NEON_DATABASE_URL=" + c.NeonDatabaseURL,
		"TIMESCALEDB_URL=" + c.TimescaleDatabaseURL,
		"EARN_MAX_DELEGATE=" + c.EarnMaxDelegate,
		"EARN_RECONCILIATION_CONCURRENCY=" + strconv.Itoa(c.ReconciliationWorkers),
		"AUTODEPOSIT_RECONCILIATION_CONCURRENCY=" + strconv.Itoa(c.AutodepositWorkers),
		"SOLANA_RPC_URL=" + c.SolanaRPCURL,
		"SOLANA_CLUSTER=" + c.Cluster,
		"KAMINO_API_BASE=" + c.KaminoAPIBase,
		"LASERSTREAM_ENDPOINT=" + c.LaserStreamEndpoint,
		"EARN_BRIDGE_OBSERVER_CONSUMER=1",
		"RUST_LOG=" + envOr("RUST_LOG", "info"),
		"OTEL_EXPORTER_OTLP_ENDPOINT=" + os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
	}
}

func FromEnv() (Config, error) {
	if err := validatePositiveIntegerEnv(
		"LASERSTREAM_REPLAY_OVERLAP_SLOTS",
		"BALANCE_SWEEP_TARGET_REFRESH_SECONDS",
		"KAMINO_CONFIRMED_REFRESH_INTERVAL_SECONDS",
		"LASERSTREAM_PROGRESS_TIMEOUT_SECONDS",
		"LASERSTREAM_HANDOFF_TIMEOUT_SECONDS",
		"EARN_DOMAIN_BRIDGE_STARTUP_TIMEOUT_SECONDS",
		"EARN_RECONCILIATION_READY_MAX_AGE_SECONDS",
		"EARN_RECONCILIATION_CONCURRENCY",
		"AUTODEPOSIT_RECONCILIATION_CONCURRENCY",
	); err != nil {
		return Config{}, err
	}
	cfg := Config{
		LaserStreamEndpoint:   strings.TrimSpace(os.Getenv("LASERSTREAM_ENDPOINT")),
		HeliusAPIKey:          strings.TrimSpace(os.Getenv("HELIUS_API_KEY")),
		EarnMaxDelegate:       strings.TrimSpace(os.Getenv("EARN_MAX_DELEGATE")),
		SolanaRPCURL:          strings.TrimSpace(os.Getenv("SOLANA_RPC_URL")),
		NeonDatabaseURL:       strings.TrimSpace(os.Getenv("NEON_DATABASE_URL")),
		AppsDatabaseURL:       strings.TrimSpace(os.Getenv("OBSERVER_APPS_DATABASE_URL")),
		TimescaleDatabaseURL:  strings.TrimSpace(os.Getenv("TIMESCALEDB_URL")),
		KaminoAPIBase:         envOr("KAMINO_API_BASE", "https://api.kamino.finance"),
		Cluster:               normalizeSolanaCluster(envOr("SOLANA_CLUSTER", "mainnet-beta")),
		ATAStream:             strings.ToLower(envOr("BALANCE_SWEEP_ATA_STREAM", "production")),
		HTTPAddress:           envOr("PORT", "10000"),
		BridgeBinary:          strings.TrimSpace(os.Getenv("EARN_DOMAIN_BRIDGE_BINARY")),
		BridgeStartupTimeout:  durationEnv("EARN_DOMAIN_BRIDGE_STARTUP_TIMEOUT_SECONDS", 20*time.Second),
		EarnReadyMaxAge:       durationEnv("EARN_RECONCILIATION_READY_MAX_AGE_SECONDS", 300*time.Second),
		ReplayOverlapSlots:    uintEnv("LASERSTREAM_REPLAY_OVERLAP_SLOTS", 32),
		WatchRefresh:          durationEnv("BALANCE_SWEEP_TARGET_REFRESH_SECONDS", 300*time.Second),
		VerifyRefresh:         durationEnv("KAMINO_CONFIRMED_REFRESH_INTERVAL_SECONDS", 60*time.Second),
		ProgressTimeout:       durationEnv("LASERSTREAM_PROGRESS_TIMEOUT_SECONDS", 90*time.Second),
		HandoffTimeout:        durationEnv("LASERSTREAM_HANDOFF_TIMEOUT_SECONDS", 120*time.Second),
		ReconciliationWorkers: int(uintEnv("EARN_RECONCILIATION_CONCURRENCY", 4)),
		AutodepositWorkers:    int(uintEnv("AUTODEPOSIT_RECONCILIATION_CONCURRENCY", 4)),
	}
	if !strings.Contains(cfg.HTTPAddress, ":") {
		cfg.HTTPAddress = ":" + cfg.HTTPAddress
	}
	var missing []string
	for name, value := range map[string]string{
		"LASERSTREAM_ENDPOINT":       cfg.LaserStreamEndpoint,
		"HELIUS_API_KEY":             cfg.HeliusAPIKey,
		"EARN_MAX_DELEGATE":          cfg.EarnMaxDelegate,
		"SOLANA_RPC_URL":             cfg.SolanaRPCURL,
		"NEON_DATABASE_URL":          cfg.NeonDatabaseURL,
		"OBSERVER_APPS_DATABASE_URL": cfg.AppsDatabaseURL,
		"TIMESCALEDB_URL":            cfg.TimescaleDatabaseURL,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return Config{}, fmt.Errorf("required environment variables are missing: %s", strings.Join(missing, ", "))
	}
	if _, err := solanago.PublicKeyFromBase58(cfg.EarnMaxDelegate); err != nil {
		return Config{}, errors.New("EARN_MAX_DELEGATE must be a Solana public key")
	}
	if cfg.ATAStream != "production" && cfg.ATAStream != "staging" {
		return Config{}, fmt.Errorf("BALANCE_SWEEP_ATA_STREAM must be production or staging, got %q", cfg.ATAStream)
	}
	if cfg.ReplayOverlapSlots == 0 || cfg.WatchRefresh <= 0 || cfg.VerifyRefresh <= 0 || cfg.ProgressTimeout <= 0 {
		return Config{}, errors.New("LaserStream intervals and replay overlap must be positive")
	}
	if cfg.ReconciliationWorkers < 1 || cfg.AutodepositWorkers < 1 {
		return Config{}, errors.New("reconciliation worker counts must be positive")
	}
	return cfg, nil
}

func validatePositiveIntegerEnv(names ...string) error {
	for _, name := range names {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			continue
		}
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil || parsed == 0 {
			return fmt.Errorf("%s must be a positive integer", name)
		}
		switch {
		case strings.HasSuffix(name, "_SECONDS") && parsed > uint64(math.MaxInt64/int64(time.Second)):
			return fmt.Errorf("%s exceeds the duration limit", name)
		case strings.HasSuffix(name, "_CONCURRENCY") && parsed > 64:
			return fmt.Errorf("%s exceeds the bounded worker limit of 64", name)
		case name == "LASERSTREAM_REPLAY_OVERLAP_SLOTS" && parsed > math.MaxInt64:
			return fmt.Errorf("%s exceeds the signed slot limit", name)
		}
	}
	return nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func normalizeSolanaCluster(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "mainnet", "mainnet_beta", "mainnetbeta", "mainnet-beta":
		return "mainnet-beta"
	default:
		return normalized
	}
}

func uintEnv(name string, fallback uint64) uint64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	return time.Duration(uintEnv(name, uint64(fallback/time.Second))) * time.Second
}
