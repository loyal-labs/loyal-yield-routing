package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/autodeposit"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleetexec"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/multiply"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/mr-tron/base58"
)

type retailConfig struct {
	databaseURL, timescaleURL, rpcURL, timescaleSchema string
	delegate, feePayer                                 ed25519.PrivateKey
	feeOnly                                            []ed25519.PrivateKey
	crossMintEnabled                                   bool
	crossMintMaxSlippageBPS, crossMintMaxValueLossBPS  uint16
	jupiterBuildURL, jupiterAPIKey                     string
	lookup                                             retailLookupConfig
	sweepNotifier                                      *autodeposit.SweepNotifier
}

func loadRetailConfig() (retailConfig, error) {
	var cfg retailConfig
	if os.Getenv("RETAIL_MODE") != "active" {
		return cfg, errors.New("RETAIL_MODE must explicitly be active; saved replay uses loyal-evidence")
	}
	var lookupErr error
	cfg.lookup, lookupErr = loadRetailLookupConfig()
	if lookupErr != nil {
		return cfg, lookupErr
	}
	for _, name := range []string{"RETAIL_CROSS_MINT_ENABLED", "EARN_ROUTER_ENABLE_CROSS_MINT_JUPITER"} {
		value := os.Getenv(name)
		if value == "" {
			continue
		}
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return cfg, fmt.Errorf("%s must be a boolean", name)
		}
		if name == "RETAIL_CROSS_MINT_ENABLED" {
			cfg.crossMintEnabled = enabled
		} else if enabled && !cfg.crossMintEnabled {
			return cfg, errors.New("cross-mint requires explicit RETAIL_CROSS_MINT_ENABLED=true")
		}
	}
	cfg.jupiterBuildURL = strings.TrimSpace(os.Getenv("RETAIL_JUPITER_BUILD_URL"))
	if cfg.jupiterBuildURL == "" {
		cfg.jupiterBuildURL = "https://api.jup.ag/swap/v2/build"
	}
	var err error
	if cfg.crossMintEnabled {
		if cfg.jupiterAPIKey, err = engine.Credential("RETAIL_JUPITER_API_KEY"); err != nil {
			return cfg, err
		}
	}
	if _, err := fleet.NewJupiterBuildClient(cfg.jupiterBuildURL, cfg.jupiterAPIKey); err != nil {
		return cfg, errors.New("RETAIL_JUPITER_BUILD_URL must be absolute HTTPS without user info")
	}
	for _, field := range []struct {
		name string
		out  *uint16
	}{{"RETAIL_CROSS_MINT_MAX_SLIPPAGE_BPS", &cfg.crossMintMaxSlippageBPS}, {"RETAIL_CROSS_MINT_MAX_VALUE_LOSS_BPS", &cfg.crossMintMaxValueLossBPS}} {
		value := strings.TrimSpace(os.Getenv(field.name))
		if value == "" {
			*field.out = 50
			continue
		}
		parsed, err := strconv.ParseUint(value, 10, 16)
		if err != nil || parsed == 0 || parsed > 1000 {
			return cfg, fmt.Errorf("%s must be in 1..1000", field.name)
		}
		*field.out = uint16(parsed)
	}
	for _, field := range []struct {
		name string
		out  *string
		read func(string) (string, error)
	}{
		{"RETAIL_DATABASE_URL", &cfg.databaseURL, engine.Credential}, {"RETAIL_TIMESCALE_DATABASE_URL", &cfg.timescaleURL, engine.Credential}, {"RETAIL_SOLANA_RPC_URL", &cfg.rpcURL, engine.Credential},
		{"RETAIL_TIMESCALE_SCHEMA", &cfg.timescaleSchema, required},
	} {
		value, err := field.read(field.name)
		if err != nil {
			return cfg, err
		}
		*field.out = value
	}
	rpc, err := url.Parse(cfg.rpcURL)
	if err != nil || rpc.Host == "" || (rpc.Scheme != "http" && rpc.Scheme != "https") {
		return cfg, errors.New("RETAIL_SOLANA_RPC_URL must be an absolute HTTP or HTTPS URL")
	}
	for _, field := range []struct {
		name string
		out  *ed25519.PrivateKey
	}{{"RETAIL_DELEGATE_KEYPAIR", &cfg.delegate}, {"RETAIL_FEE_PAYER_KEYPAIR", &cfg.feePayer}} {
		material, err := engine.Credential(field.name)
		if err != nil {
			return cfg, err
		}
		key, err := parseRetailKey(material)
		if err != nil {
			return cfg, fmt.Errorf("invalid %s", field.name)
		}
		*field.out = key
	}
	// The current A/D official wires have one delegated signer/fee payer. G can
	// represent two signatures, but composition cannot invent that support in A/D.
	if !bytes.Equal(cfg.delegate, cfg.feePayer) {
		return cfg, errors.New("retail Autodeposit and fleet require delegate and fee payer to be the same key")
	}
	if cfg.feeOnly, err = loadFeeOnlyPayers(cfg.delegate); err != nil {
		return cfg, err
	}
	if err := cfg.fleetConfig().Validate(); err != nil {
		return cfg, retailError("fleet configuration", err)
	}
	endpoint, secret := optionalCredential("RETAIL_SWEEP_NOTIFY_ENDPOINT"), optionalCredential("RETAIL_SWEEP_NOTIFY_SECRET")
	if (endpoint == "") != (secret == "") {
		return cfg, errors.New("RETAIL_SWEEP_NOTIFY_ENDPOINT and RETAIL_SWEEP_NOTIFY_SECRET must both be set or both be absent")
	}
	if endpoint != "" {
		if cfg.sweepNotifier, err = autodeposit.NewSweepNotifier(endpoint, secret); err != nil {
			return cfg, errors.New("RETAIL_SWEEP_NOTIFY_ENDPOINT must be an absolute HTTPS URL without user info")
		}
	}
	return cfg, nil
}

func loadFeeOnlyPayers(delegate ed25519.PrivateKey) ([]ed25519.PrivateKey, error) {
	const name = "YIELD_ROUTE_FEE_PAYER_KEYPAIRS"
	raw, err := os.ReadFile(filepath.Join(os.Getenv("CREDENTIALS_DIRECTORY"), name))
	if errors.Is(err, fs.ErrNotExist) || err == nil && strings.TrimSpace(string(raw)) == "" {
		return nil, nil
	}
	var encoded []string
	if err != nil || json.Unmarshal(raw, &encoded) != nil {
		return nil, fmt.Errorf("%s must be a JSON array of encoded keypairs", name)
	}
	keys := make([]ed25519.PrivateKey, 0, len(encoded))
	seen := map[string]bool{base58.Encode(delegate[32:]): true}
	for _, material := range encoded {
		key, err := parseRetailKey(material)
		if err != nil {
			return nil, fmt.Errorf("invalid %s entry", name)
		}
		public := base58.Encode(key[32:])
		if seen[public] {
			return nil, fmt.Errorf("%s repeats a key or the delegate", name)
		}
		seen[public] = true
		keys = append(keys, key)
	}
	return keys, nil
}

func (c retailConfig) feeOnlyPublicKeys() []string {
	out := make([]string, 0, len(c.feeOnly))
	for _, key := range c.feeOnly {
		out = append(out, base58.Encode(key[32:]))
	}
	return out
}

func parseRetailKey(material string) (ed25519.PrivateKey, error) {
	material = strings.TrimSpace(material)
	if len(material) > 1024 {
		return nil, errors.New("invalid key material")
	}
	var raw []byte
	var err error
	if strings.HasPrefix(material, "[") {
		err = json.Unmarshal([]byte(material), &raw)
	} else if len(material) == 64 || len(material) == 128 {
		raw, err = hex.DecodeString(material)
		if err != nil {
			raw, err = base58.Decode(material)
		}
	} else {
		raw, err = base58.Decode(material)
	}
	if err != nil {
		return nil, errors.New("invalid key material")
	}
	if len(raw) == ed25519.SeedSize {
		return ed25519.NewKeyFromSeed(raw), nil
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid key material")
	}
	key := ed25519.NewKeyFromSeed(raw[:ed25519.SeedSize])
	if !bytes.Equal(key, raw) {
		return nil, errors.New("invalid key material")
	}
	return key, nil
}

func (c retailConfig) fleetConfig() fleet.Config {
	return fleet.Config{DatabaseURL: c.databaseURL, TimescaleURL: c.timescaleURL, TimescaleSchema: c.timescaleSchema, RPCURL: c.rpcURL, Cluster: "mainnet-beta", PollInterval: 15 * time.Second, DelegatedSigner: base58.Encode(c.delegate[32:]), RevalidationOwner: "retail", RevalidationLeaseTTL: 30 * time.Second, RevalidationComputeLimit: 1_400_000, CrossMintEnabled: c.crossMintEnabled, CrossMintMaxValueLossBPS: c.crossMintMaxValueLossBPS, CrossMintMaxSlippageBPS: c.crossMintMaxSlippageBPS, JupiterBuildURL: c.jupiterBuildURL, JupiterAPIKey: c.jupiterAPIKey}
}

type retailStageFailure struct {
	stage string
	cause error
}

func (e *retailStageFailure) Error() string { return "retail " + e.stage + " failed" }
func (e *retailStageFailure) Unwrap() error { return e.cause }
func retailError(stage string, cause error) error {
	if cause == nil {
		return nil
	}
	return &retailStageFailure{stage: stage, cause: cause}
}

var errFamilyLockLost = errors.New("family lock lost")

func retailFamilies(value string) ([]engine.Family, error) {
	var families []engine.Family
	seen := map[engine.Family]bool{}
	for _, name := range strings.Split(value, ",") {
		family := engine.Family(strings.TrimSpace(name))
		switch family {
		case engine.FamilyAutodeposit, engine.FamilyFleet, engine.FamilyMultiply, engine.FamilyLookup:
		default:
			return nil, fmt.Errorf("RETAIL_FAMILIES has unknown retail family %q", family)
		}
		if seen[family] {
			return nil, fmt.Errorf("RETAIL_FAMILIES repeats %q", family)
		}
		seen[family] = true
		families = append(families, family)
	}
	return families, nil
}

func runRetail(ctx context.Context, owner string, facts *engine.Facts, metrics engine.Lane) error {
	if ctx == nil {
		return errors.New("retail requires caller context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, err := loadRetailConfig()
	if err != nil {
		return err
	}
	families, err := retailFamilies(os.Getenv("RETAIL_FAMILIES"))
	if err != nil {
		return err
	}
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	for _, family := range families {
		lost, err := engine.HoldFamily(ctx, cfg.databaseURL, family)
		if err != nil {
			return retailError("family lock", err)
		}
		go func(family engine.Family) {
			select {
			case <-lost:
				stop(fmt.Errorf("%s: %w", family, errFamilyLockLost))
			case <-ctx.Done():
			}
		}(family)
	}
	facts.Own(families...)
	cluster, err := chain.New(cfg.rpcURL, 15*time.Second)
	if err != nil {
		return retailError("chain RPC", err)
	}
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	yieldPool, err := db.Open(startup, cfg.databaseURL, 16)
	if err != nil {
		return retailError("Yield database", err)
	}
	defer yieldPool.Close()
	marketPool, err := db.Open(startup, cfg.timescaleURL, 8)
	if err != nil {
		return retailError("market database", err)
	}
	defer marketPool.Close()
	aStore, err := autodeposit.NewStore(yieldPool)
	if err != nil {
		return retailError("Autodeposit store", err)
	}
	cStore, err := fleet.NewStoreFromPool(yieldPool)
	if err != nil {
		return retailError("fleet store", err)
	}
	catalog, err := fleet.NewCatalogStoreFromPool(marketPool, cfg.timescaleSchema)
	if err != nil {
		return retailError("reserve catalog", err)
	}
	dStore := fleetexec.NewStore(yieldPool)
	gStore, err := multiply.NewStoreFromPool(startup, yieldPool)
	if err != nil {
		return retailError("Multiply schema", err)
	}
	gExecutor, err := multiply.NewExecutorWithFeePayerContext(startup, cluster, cfg.feePayer, cfg.delegate)
	if err != nil {
		return retailError("mainnet genesis", err)
	}
	deposits := autodeposit.NewRPCChain(cluster)
	wires, err := autodeposit.NewSweepWireBuilderWithSetup(cfg.delegate, deposits.ReadAccounts, deposits.MinimumBalanceForRentExemption)
	if err != nil {
		return retailError("Autodeposit wires", err)
	}
	controller, err := autodeposit.NewController(autodeposit.ControllerDependencies{Store: aStore, Chain: deposits, Wires: wires, Facts: facts})
	if err != nil {
		return retailError("Autodeposit controller", err)
	}
	lookupWorker, err := fleetexec.NewLookupWorker(dStore, cluster, fleetexec.LookupWorkerConfig{
		Cluster: "mainnet-beta", Owner: owner, LeaseTTL: 30 * time.Second,
		TickDeadline: 20 * time.Second, PollInterval: time.Second,
		Budget: cfg.lookup.budget, ReconcileOnly: !cfg.lookup.active, Facts: facts,
		OnHealth: laneHealth(facts, engine.FamilyLookup, "lookup_writer"),
	}, cfg.lookup.managerKey())
	if err != nil {
		return retailError("lookup writer", err)
	}
	// This lane receives no manager key or broadcast capability.
	lookupPlanner, err := fleetexec.NewLookupPlanner(dStore, cluster, fleetexec.LookupPlannerConfig{
		Cluster: "mainnet-beta", Owner: owner, LeaseTTL: 30 * time.Second,
		TickDeadline: 20 * time.Second, PollInterval: time.Second,
		CatalogInterval: time.Minute, GrowthReservation: fleetexec.SourceGrowthReservation, MaximumVaultCohort: fleetexec.SourceMaximumVaultCohort,
		ReconcileOnly: !cfg.lookup.active, Facts: facts,
		OnHealth: laneHealth(facts, engine.FamilyLookup, "lookup_planner"),
	})
	if err != nil {
		return retailError("lookup planner", err)
	}
	aWorker, err := autodeposit.NewWorker(autodeposit.WorkerDependencies{Store: aStore, Executor: controller, Facts: facts, FeePayer: controller, Notifier: cfg.sweepNotifier})
	if err != nil {
		return retailError("Autodeposit worker", err)
	}
	control := &autodeposit.ControlReconciler{Store: aStore, Reader: wires, OnError: func(err error) {
		slog.Error("retail lane tick failed", "family", engine.FamilyAutodeposit, "lane", "autodeposit_control", "error", engine.ErrorText(err))
	}, PollInterval: time.Second, LeaseDuration: 120 * time.Second}
	cConfig := cfg.fleetConfig()
	cConfig.RevalidationOwner = owner
	planner, err := fleet.NewWorker(cConfig, cStore, cluster, catalog, facts)
	if err != nil {
		return retailError("fleet planner", err)
	}
	revalidator, err := fleet.NewRevalidator(cStore, cluster, fleet.RevalidatorConfig{Owner: owner, DelegatedSigner: cConfig.DelegatedSigner, LeaseTTL: cConfig.RevalidationLeaseTTL, ComputeLimit: cConfig.RevalidationComputeLimit, CrossMintEnabled: true, CrossMintMaxValueLossBPS: cfg.crossMintMaxValueLossBPS, CrossMintMaxSlippageBPS: cfg.crossMintMaxSlippageBPS, JupiterBuildURL: cfg.jupiterBuildURL, JupiterAPIKey: cfg.jupiterAPIKey, FeeOnlyPayers: cfg.feeOnlyPublicKeys(), Metrics: facts})
	if err != nil {
		return retailError("fused fleet preparation", err)
	}
	executor, err := fleetexec.NewWorker(fleetexec.Config{Cluster: cConfig.Cluster, Owner: owner, LeaseTTL: 30 * time.Second, BatchSize: 20, TickInterval: 750 * time.Millisecond, Facts: facts, OnHealth: laneHealth(facts, engine.FamilyFleet, "executor")}, dStore, cluster, cluster, fleetexec.DelegateSigner{FeePayer: cfg.delegate, FeeOnly: cfg.feeOnly})
	if err != nil {
		return retailError("fleet executor", err)
	}
	if err := executor.SetFreshRevalidator(revalidator); err != nil {
		return retailError("fleet fresh execution binding", err)
	}
	crossMint, err := composeRetailCrossMint(startup, cfg, owner, dStore, revalidator, cluster, fleet.ChainReserves{Catalog: catalog, RPC: cluster}, facts)
	if err != nil {
		return retailError("cross-mint runtime", err)
	}
	multiplyQuotes, _ := jupiter.NewClient(jupiter.LiteBase, "", &http.Client{Timeout: 15 * time.Second})
	multiplyWorker, err := multiply.NewWorker(multiply.WorkerDeps{Store: gStore, Observer: cluster, Executor: gExecutor, Quotes: multiplyQuotes, WorkerID: owner, Chain: cluster, Facts: facts})
	if err != nil {
		return retailError("Multiply worker", err)
	}
	if err := startup.Err(); err != nil {
		return err
	}
	lanes := []engine.Lane{metrics}
	for _, family := range families {
		switch family {
		case engine.FamilyAutodeposit:
			lanes = append(lanes, control, aWorker)
		case engine.FamilyFleet:
			facts.LaneSucceeded(family, "planner")
			facts.LaneSucceeded(family, "executor")
			lanes = append(lanes, planner, executor, crossMint)
		case engine.FamilyLookup:
			facts.LaneSucceeded(family, "lookup_planner")
			facts.LaneSucceeded(family, "lookup_writer")
			lanes = append(lanes, lookupPlanner, lookupWorker)
		case engine.FamilyMultiply:
			lanes = append(lanes, multiplyWorker)
		}
	}
	err = engine.Run(ctx, lanes...)
	if cause := context.Cause(ctx); errors.Is(cause, errFamilyLockLost) {
		return cause
	}
	return retailError("lanes", err)
}

func laneHealth(facts *engine.Facts, family engine.Family, lane string) func(error) {
	return func(err error) {
		switch {
		case err == nil:
			facts.LaneSucceeded(family, lane)
		case errors.Is(err, fleetexec.ErrLookupPaused):
			slog.Info("retail lookup paused", "family", family, "lane", lane)
			facts.LaneSucceeded(family, lane)
		default:
			slog.Error("retail lane tick failed", "family", family, "lane", lane, "error", engine.ErrorText(err))
		}
	}
}
