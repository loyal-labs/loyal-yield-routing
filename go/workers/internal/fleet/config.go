package fleet

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL                        string
	TimescaleURL                       string
	TimescaleSchema                    string
	RPCURL                             string
	Cluster                            string
	PollInterval                       time.Duration
	SlotDuration                       time.Duration
	RevalidatorEnabled                 bool
	DelegatedSigner, RevalidationOwner string
	RevalidationLeaseTTL               time.Duration
	RevalidationPollInterval           time.Duration
	RevalidationConcurrency            int
	RevalidationComputeLimit           uint64
	CrossMintEnabled                   bool
	CrossMintMaxValueLossBPS           uint16
	CrossMintMaxSlippageBPS            uint16
	JupiterBuildURL, JupiterAPIKey     string
	EnabledStableMints                 []string
	FusedExecute                       bool
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
	if c.Cluster == "" || strings.TrimSpace(c.Cluster) != c.Cluster {
		return fmt.Errorf("canonical cluster is required")
	}
	if c.PollInterval <= 0 || c.SlotDuration <= 0 {
		return fmt.Errorf("poll and slot durations are invalid")
	}
	enabledMintCount := len(c.EnabledStableMints)
	if enabledMintCount == 0 {
		enabledMintCount = len(earnStableMints)
	}
	seenMints := map[string]bool{}
	for _, mint := range c.EnabledStableMints {
		if !isEarnStableMint(mint) || seenMints[mint] {
			return fmt.Errorf("enabled stable mints must be unique members of the Earn registry")
		}
		seenMints[mint] = true
	}
	if c.CrossMintEnabled {
		if enabledMintCount < 2 {
			return fmt.Errorf("cross-mint planning requires at least two enabled stable mints")
		}
		if c.CrossMintMaxValueLossBPS == 0 || c.CrossMintMaxValueLossBPS > 1_000 {
			return fmt.Errorf("cross-mint maximum value loss must be in 1..=1000 bps")
		}
		if c.CrossMintMaxSlippageBPS == 0 || c.CrossMintMaxSlippageBPS > 1_000 {
			return fmt.Errorf("cross-mint maximum slippage must be in 1..=1000 bps")
		}
		buildURL := c.JupiterBuildURL
		if buildURL == "" {
			buildURL = "https://api.jup.ag/swap/v2/build"
		}
		jupiterURL, err := url.Parse(buildURL)
		if err != nil || jupiterURL.Scheme != "https" || jupiterURL.Host == "" || jupiterURL.User != nil {
			return fmt.Errorf("JUPITER_SWAP_BUILD_URL must be an absolute HTTPS URL")
		}
		if c.DelegatedSigner == "" {
			return fmt.Errorf("cross-mint planning requires the delegated signer identity")
		}
		if _, err := decodePublicKey(c.DelegatedSigner); err != nil {
			return fmt.Errorf("invalid cross-mint delegated signer: %w", err)
		}
	}
	if c.RevalidatorEnabled {
		if c.DelegatedSigner == "" || c.RevalidationOwner == "" || c.RevalidationLeaseTTL < time.Second || c.RevalidationPollInterval <= 0 || c.RevalidationConcurrency <= 0 || c.RevalidationConcurrency > 256 || c.RevalidationComputeLimit == 0 || c.RevalidationComputeLimit > defaultComputeLimit {
			return fmt.Errorf("revalidator requires a delegated signer, owner, valid lease, concurrency, poll interval, and compute limit")
		}
		if _, err := decodePublicKey(c.DelegatedSigner); err != nil {
			return fmt.Errorf("invalid revalidator delegated signer: %w", err)
		}
	}
	return nil
}
