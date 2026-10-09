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
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

type Config struct {
	LaserStreamEndpoint  string
	HeliusAPIKey         string
	EarnMaxDelegate      string
	SolanaRPCURL         string
	NeonDatabaseURL      string
	TimescaleDatabaseURL string
	KaminoAPIBase        string
	Cluster              string
	ATAStream            string
	// APYRiskProfiles are the published Earn APY strategies; empty when the
	// shared snapshot table is written by another environment.
	APYRiskProfiles []string
	// ReadModelsEnabled runs the product read-model passes, which write
	// earn_fleet_allocations_hourly, earn_reserve_share_prices and
	// earn_forecast_snapshots. The Apps hourly crons (earn-reserve-share-prices
	// and earn-forecast-snapshot) own those tables until the Phase 2 handover
	// retires them and flips this on; Rust never wrote them. One fact has one
	// writer, so the default is off (OBSERVER_READ_MODELS_ENABLED=true|false).
	ReadModelsEnabled  bool
	ReplayOverlapSlots uint64
	WatchRefresh       time.Duration
	// VerifyRefresh is the Kamino confirmed-read safety sweep for quiet
	// reserves (Rust --confirmed-refresh-interval-secs, default 30). Stream
	// updates are verified on their own within a 100 ms batch tick.
	VerifyRefresh time.Duration
	// CatalogRefresh renews the supported reserve catalog the planners read
	// (Rust KAMINO_SUPPORTED_RESERVE_REFRESH_INTERVAL_SECS, default 120, at
	// most 180 so a fetched_at stays inside the planners' 300 s age bound).
	CatalogRefresh        time.Duration
	ProgressTimeout       time.Duration
	HandoffTimeout        time.Duration
	ReconciliationWorkers int
}

func FromEnv() (Config, error) {
	if err := validatePositiveIntegerEnv(
		"LASERSTREAM_REPLAY_OVERLAP_SLOTS",
		"BALANCE_SWEEP_TARGET_REFRESH_SECONDS",
		"KAMINO_CONFIRMED_REFRESH_INTERVAL_SECONDS",
		"KAMINO_SUPPORTED_RESERVE_REFRESH_INTERVAL_SECONDS",
		"LASERSTREAM_PROGRESS_TIMEOUT_SECONDS",
		"LASERSTREAM_HANDOFF_TIMEOUT_SECONDS",
		"EARN_RECONCILIATION_CONCURRENCY",
	); err != nil {
		return Config{}, err
	}
	var missing []string
	credential := func(name string) string {
		value, err := engine.Credential(name)
		if err != nil {
			missing = append(missing, name)
		}
		return value
	}
	cfg := Config{
		LaserStreamEndpoint:   strings.TrimSpace(os.Getenv("LASERSTREAM_ENDPOINT")),
		HeliusAPIKey:          credential("HELIUS_API_KEY"),
		EarnMaxDelegate:       strings.TrimSpace(os.Getenv("EARN_MAX_DELEGATE")),
		SolanaRPCURL:          credential("SOLANA_RPC_URL"),
		NeonDatabaseURL:       credential("NEON_DATABASE_URL"),
		TimescaleDatabaseURL:  credential("TIMESCALEDB_URL"),
		KaminoAPIBase:         envOr("KAMINO_API_BASE", "https://api.kamino.finance"),
		Cluster:               normalizeSolanaCluster(envOr("SOLANA_CLUSTER", "mainnet-beta")),
		ATAStream:             strings.ToLower(envOr("BALANCE_SWEEP_ATA_STREAM", "production")),
		ReplayOverlapSlots:    uintEnv("LASERSTREAM_REPLAY_OVERLAP_SLOTS", 32),
		WatchRefresh:          durationEnv("BALANCE_SWEEP_TARGET_REFRESH_SECONDS", 300*time.Second),
		VerifyRefresh:         durationEnv("KAMINO_CONFIRMED_REFRESH_INTERVAL_SECONDS", 30*time.Second),
		CatalogRefresh:        durationEnv("KAMINO_SUPPORTED_RESERVE_REFRESH_INTERVAL_SECONDS", 120*time.Second),
		ProgressTimeout:       durationEnv("LASERSTREAM_PROGRESS_TIMEOUT_SECONDS", 90*time.Second),
		HandoffTimeout:        durationEnv("LASERSTREAM_HANDOFF_TIMEOUT_SECONDS", 120*time.Second),
		ReconciliationWorkers: int(uintEnv("EARN_RECONCILIATION_CONCURRENCY", 4)),
	}
	for name, value := range map[string]string{
		"LASERSTREAM_ENDPOINT": cfg.LaserStreamEndpoint,
		"EARN_MAX_DELEGATE":    cfg.EarnMaxDelegate,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return Config{}, fmt.Errorf("required configuration is missing: %s", strings.Join(missing, ", "))
	}
	if _, err := solanago.PublicKeyFromBase58(cfg.EarnMaxDelegate); err != nil {
		return Config{}, errors.New("EARN_MAX_DELEGATE must be a Solana public key")
	}
	if cfg.ATAStream != "production" {
		return Config{}, fmt.Errorf("BALANCE_SWEEP_ATA_STREAM must be production, got %q", cfg.ATAStream)
	}
	if cfg.ReplayOverlapSlots == 0 || cfg.WatchRefresh <= 0 || cfg.VerifyRefresh <= 0 || cfg.ProgressTimeout <= 0 {
		return Config{}, errors.New("LaserStream intervals and replay overlap must be positive")
	}
	if cfg.CatalogRefresh <= 0 || cfg.CatalogRefresh > 180*time.Second {
		return Config{}, errors.New("KAMINO_SUPPORTED_RESERVE_REFRESH_INTERVAL_SECONDS must be between 1 and 180")
	}
	switch strings.TrimSpace(os.Getenv("DISABLE_EARN_APY_REFRESH")) {
	case "", "false":
		for _, profile := range strings.Split(envOr("EARN_APY_RISK_PROFILES", "safe"), ",") {
			if profile = strings.TrimSpace(profile); profile != "" {
				cfg.APYRiskProfiles = append(cfg.APYRiskProfiles, profile)
			}
		}
		if len(cfg.APYRiskProfiles) == 0 {
			return Config{}, errors.New("EARN_APY_RISK_PROFILES requires at least one risk profile")
		}
	case "true":
	default:
		return Config{}, errors.New("DISABLE_EARN_APY_REFRESH must be true or false")
	}
	switch strings.TrimSpace(os.Getenv("OBSERVER_READ_MODELS_ENABLED")) {
	case "", "false":
	case "true":
		cfg.ReadModelsEnabled = true
	default:
		return Config{}, errors.New("OBSERVER_READ_MODELS_ENABLED must be true or false")
	}
	if cfg.ReconciliationWorkers < 1 {
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
