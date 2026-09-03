package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	DatabaseURL     string
	TimescaleURL    string
	TimescaleSchema string
	RPCURL          string
	Cluster         string
	Mode            Mode
	VaultID         int64
	Source          ReserveIdentity
	Target          ReserveIdentity
	PollInterval    time.Duration
	SlotDuration    time.Duration
}

func ConfigFromEnvironment() (Config, error) {
	config := Config{
		DatabaseURL:     os.Getenv("NEON_DATABASE_URL"),
		TimescaleURL:    valueOr(os.Getenv("TIMESCALE_DATABASE_URL"), os.Getenv("TIMESCALEDB_URL")),
		TimescaleSchema: valueOr(os.Getenv("KAMINO_TIMESCALE_SCHEMA"), "kamino"),
		RPCURL:          os.Getenv("SOLANA_RPC_URL"),
		Cluster:         valueOr(os.Getenv("KAMINO_FLEET_CLUSTER"), "mainnet-beta"),
		Mode:            Mode(valueOr(os.Getenv("KAMINO_FLEET_MODE"), string(ModeShadow))),
		PollInterval:    durationOr(os.Getenv("KAMINO_FLEET_POLL_INTERVAL"), time.Second),
	}
	var err error
	if configured := os.Getenv("KAMINO_FLEET_SLOT_DURATION"); configured != "" {
		config.SlotDuration = durationOr(configured, 0)
	} else {
		config.SlotDuration, err = fetchKaminoSlotDuration(
			context.Background(),
			valueOr(os.Getenv("KAMINO_API_BASE"), "https://api.kamino.finance"),
			10*time.Second,
		)
		if err != nil {
			return Config{}, fmt.Errorf("load Kamino slot duration: %w", err)
		}
	}
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
	if c.DatabaseURL == "" || c.TimescaleURL == "" || c.VaultID <= 0 {
		return fmt.Errorf("Neon and Timescale database URLs and positive vault ID are required")
	}
	if !validSQLIdentifier(c.TimescaleSchema) {
		return fmt.Errorf("canonical Timescale schema is required")
	}
	u, err := url.Parse(c.RPCURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("confirmed Solana RPC URL is required")
	}
	if c.Cluster == "" || strings.TrimSpace(c.Cluster) != c.Cluster || c.Mode != ModeShadow && c.Mode != ModePublish {
		return fmt.Errorf("canonical cluster and a shadow or publish mode are required")
	}
	if c.Cluster == "mainnet-beta" && c.Mode == ModePublish {
		return fmt.Errorf("mainnet publish is blocked until Rust-compatible epoch and revalidation parity is verified")
	}
	if c.PollInterval <= 0 || c.SlotDuration <= 0 {
		return fmt.Errorf("poll and slot durations are invalid")
	}
	if c.Source.Address == "" || c.Target.Address == "" || c.Source.Address == c.Target.Address ||
		c.Source.Market == "" || c.Target.Market == "" ||
		c.Source.Mint != USDCMint || c.Target.Mint != USDCMint {
		return fmt.Errorf("phase 1 requires two distinct Kamino reserves for the pinned USDC mint")
	}
	for _, value := range []string{c.Source.Address, c.Target.Address, c.Source.Market, c.Target.Market, c.Source.Mint} {
		if _, err := decodePublicKey(value); err != nil {
			return fmt.Errorf("invalid Solana identity: %w", err)
		}
	}
	return nil
}

func fetchKaminoSlotDuration(ctx context.Context, baseURL string, timeout time.Duration) (time.Duration, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return 0, fmt.Errorf("invalid Kamino API base URL")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/slots/duration", nil)
	if err != nil {
		return 0, err
	}
	client := &http.Client{Timeout: timeout}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return 0, fmt.Errorf("Kamino slot duration status %s", response.Status)
	}
	var payload struct {
		Recent float64 `json:"recentSlotDurationInMs"`
		Median float64 `json:"medianSlotDurationMs"`
		Slot   float64 `json:"slotDurationMs"`
		Value  float64 `json:"duration"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return 0, err
	}
	milliseconds := payload.Recent
	if milliseconds <= 0 {
		milliseconds = payload.Median
	}
	if milliseconds <= 0 {
		milliseconds = payload.Slot
	}
	if milliseconds <= 0 {
		milliseconds = payload.Value
	}
	if milliseconds <= 0 {
		return 0, fmt.Errorf("slot duration response has no positive duration")
	}
	return time.Duration(milliseconds * float64(time.Millisecond)), nil
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
