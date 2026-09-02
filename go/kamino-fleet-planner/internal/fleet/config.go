package fleet

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Mode string

const (
	ModeShadow  Mode = "shadow"
	ModePublish Mode = "publish"
)

type Config struct {
	DatabaseURL  string
	RPCURL       string
	Cluster      string
	Mode         Mode
	VaultID      int64
	Source       ReserveIdentity
	Target       ReserveIdentity
	PollInterval time.Duration
	SlotDuration time.Duration
}

func ConfigFromEnvironment() (Config, error) {
	config := Config{
		DatabaseURL:  os.Getenv("NEON_DATABASE_URL"),
		RPCURL:       os.Getenv("SOLANA_RPC_URL"),
		Cluster:      valueOr(os.Getenv("KAMINO_FLEET_CLUSTER"), "mainnet-beta"),
		Mode:         Mode(valueOr(os.Getenv("KAMINO_FLEET_MODE"), string(ModeShadow))),
		PollInterval: durationOr(os.Getenv("KAMINO_FLEET_POLL_INTERVAL"), time.Second),
		SlotDuration: durationOr(os.Getenv("KAMINO_FLEET_SLOT_DURATION"), 400*time.Millisecond),
	}
	var err error
	config.VaultID, err = strconv.ParseInt(os.Getenv("KAMINO_FLEET_VAULT_ID"), 10, 64)
	if err != nil {
		return Config{}, fmt.Errorf("KAMINO_FLEET_VAULT_ID must be a positive integer")
	}
	if err := json.Unmarshal([]byte(os.Getenv("KAMINO_FLEET_SOURCE_RESERVE")), &config.Source); err != nil {
		return Config{}, fmt.Errorf("decode KAMINO_FLEET_SOURCE_RESERVE: %w", err)
	}
	if err := json.Unmarshal([]byte(os.Getenv("KAMINO_FLEET_TARGET_RESERVE")), &config.Target); err != nil {
		return Config{}, fmt.Errorf("decode KAMINO_FLEET_TARGET_RESERVE: %w", err)
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (c Config) Validate() error {
	if c.DatabaseURL == "" || c.VaultID <= 0 {
		return fmt.Errorf("database URL and positive vault ID are required")
	}
	u, err := url.Parse(c.RPCURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("confirmed Solana RPC URL is required")
	}
	if c.Cluster == "" || strings.TrimSpace(c.Cluster) != c.Cluster || c.Mode != ModeShadow && c.Mode != ModePublish {
		return fmt.Errorf("canonical cluster and a shadow or publish mode are required")
	}
	if c.PollInterval <= 0 || c.SlotDuration <= 0 {
		return fmt.Errorf("poll and slot durations are invalid")
	}
	if c.Source.Address == "" || c.Target.Address == "" || c.Source.Address == c.Target.Address ||
		c.Source.Market == "" || c.Source.Market != c.Target.Market ||
		c.Source.Mint != USDCMint || c.Target.Mint != USDCMint {
		return fmt.Errorf("phase 1 requires two distinct reserves in one market for the pinned USDC mint")
	}
	for _, value := range []string{c.Source.Address, c.Target.Address, c.Source.Market, c.Source.Mint} {
		if _, err := decodePublicKey(value); err != nil {
			return fmt.Errorf("invalid Solana identity: %w", err)
		}
	}
	return nil
}

func valueOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func durationOr(value string, fallback time.Duration) time.Duration {
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0
	}
	return parsed
}
