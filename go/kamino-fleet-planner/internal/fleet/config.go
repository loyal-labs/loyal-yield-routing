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
	DatabaseURL                        string
	TimescaleURL                       string
	TimescaleSchema                    string
	RPCURL                             string
	Cluster                            string
	Mode                               Mode
	VaultID                            int64
	Source                             ReserveIdentity
	Target                             ReserveIdentity
	PollInterval                       time.Duration
	SlotDuration                       time.Duration
	RevalidatorEnabled                 bool
	KLendProxyPath, KLendProxySHA256   string
	DelegatedSigner, RevalidationOwner string
	RevalidationLeaseTTL               time.Duration
	RevalidationComputeLimit           uint64
	FusedExecute                       bool
}

func ConfigFromEnvironment() (Config, error) {
	config := Config{
		DatabaseURL:              os.Getenv("NEON_DATABASE_URL"),
		TimescaleURL:             valueOr(os.Getenv("TIMESCALE_DATABASE_URL"), os.Getenv("TIMESCALEDB_URL")),
		TimescaleSchema:          valueOr(os.Getenv("KAMINO_TIMESCALE_SCHEMA"), "kamino"),
		RPCURL:                   os.Getenv("SOLANA_RPC_URL"),
		Cluster:                  valueOr(os.Getenv("KAMINO_FLEET_CLUSTER"), "mainnet-beta"),
		Mode:                     Mode(valueOr(os.Getenv("KAMINO_FLEET_MODE"), string(ModeShadow))),
		PollInterval:             durationOr(os.Getenv("KAMINO_FLEET_POLL_INTERVAL"), time.Second),
		KLendProxyPath:           os.Getenv("KAMINO_KLEND_PROXY_PATH"),
		KLendProxySHA256:         strings.ToLower(os.Getenv("KAMINO_KLEND_PROXY_SHA256")),
		DelegatedSigner:          os.Getenv("KAMINO_FLEET_DELEGATED_SIGNER"),
		RevalidationOwner:        valueOr(os.Getenv("KAMINO_FLEET_REVALIDATION_OWNER"), "loyal-kamino-fleet-planner"),
		RevalidationLeaseTTL:     durationOr(os.Getenv("KAMINO_FLEET_REVALIDATION_LEASE_TTL"), 30*time.Second),
		RevalidationComputeLimit: uint64Or(os.Getenv("KAMINO_FLEET_COMPUTE_LIMIT"), defaultComputeLimit),
		FusedExecute:             boolOr(os.Getenv("KAMINO_FLEET_FUSED_EXECUTE"), true),
	}
	for _, name := range []string{"KAMINO_FLEET_REVALIDATOR_ENABLED", "KAMINO_FLEET_FUSED_EXECUTE"} {
		if value := os.Getenv(name); value != "" {
			if _, err := strconv.ParseBool(value); err != nil {
				return Config{}, fmt.Errorf("%s must be a boolean", name)
			}
		}
	}
	config.RevalidatorEnabled = boolOr(os.Getenv("KAMINO_FLEET_REVALIDATOR_ENABLED"), config.Mode == ModePublish)
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
	// Legacy single-vault/source/target settings remain parseable for a staged
	// deployment, but they no longer scope planning. The worker always loads
	// the complete migrated fleet and immutable reserve catalog.
	if value := os.Getenv("KAMINO_FLEET_VAULT_ID"); value != "" {
		config.VaultID, err = strconv.ParseInt(value, 10, 64)
		if err != nil || config.VaultID <= 0 {
			return Config{}, fmt.Errorf("KAMINO_FLEET_VAULT_ID must be a positive integer when set")
		}
	}
	if value := os.Getenv("KAMINO_FLEET_SOURCE_RESERVE"); value != "" {
		if err := json.Unmarshal([]byte(value), &config.Source); err != nil {
			return Config{}, fmt.Errorf("decode KAMINO_FLEET_SOURCE_RESERVE: %w", err)
		}
	}
	if value := os.Getenv("KAMINO_FLEET_TARGET_RESERVE"); value != "" {
		if err := json.Unmarshal([]byte(value), &config.Target); err != nil {
			return Config{}, fmt.Errorf("decode KAMINO_FLEET_TARGET_RESERVE: %w", err)
		}
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (c Config) Validate() error {
	if c.DatabaseURL == "" || c.TimescaleURL == "" {
		return fmt.Errorf("Neon and Timescale database URLs are required")
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
	if c.RevalidatorEnabled {
		if c.KLendProxyPath == "" || len(c.KLendProxySHA256) != 64 || !isHex(c.KLendProxySHA256) || c.DelegatedSigner == "" || c.RevalidationOwner == "" || c.RevalidationLeaseTTL < time.Second || c.RevalidationComputeLimit == 0 || c.RevalidationComputeLimit > defaultComputeLimit {
			return fmt.Errorf("revalidator requires a digest-pinned KLend proxy, delegated signer, owner, valid lease, and compute limit")
		}
		if _, err := decodePublicKey(c.DelegatedSigner); err != nil {
			return fmt.Errorf("invalid revalidator delegated signer: %w", err)
		}
	}
	legacyConfigured := c.Source.Address != "" || c.Target.Address != ""
	if legacyConfigured {
		if c.Source.Address == "" || c.Target.Address == "" || c.Source.Address == c.Target.Address || c.Source.Market == "" || c.Target.Market == "" || c.Source.Mint != USDCMint || c.Target.Mint != USDCMint {
			return fmt.Errorf("legacy reserve scope must be complete when set")
		}
		for _, value := range []string{c.Source.Address, c.Target.Address, c.Source.Market, c.Target.Market, c.Source.Mint} {
			if _, err := decodePublicKey(value); err != nil {
				return fmt.Errorf("invalid Solana identity: %w", err)
			}
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

func boolOr(value string, fallback bool) bool {
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false
	}
	return parsed
}

func uint64Or(value string, fallback uint64) uint64 {
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
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
