package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/autodeposit"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleetexec"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/multiply"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/solana"
	"github.com/mr-tron/base58"
)

type retailConfig struct {
	databaseURL, timescaleURL, rpcURL, timescaleSchema, proxyPath, proxyHash string
	slotDuration                                                             time.Duration
	delegate, feePayer                                                       ed25519.PrivateKey
	crossMintEnabled                                                         bool
	crossMintMaxSlippageBPS, crossMintMaxValueLossBPS                        uint16
	jupiterBuildURL, jupiterAPIKey                                           string
	lookup                                                                   retailLookupConfig
}

// Configuration is scoped to this capability. Legacy/background writer flags
// are not authority to start a new retail engine; active is an explicit opt-in.
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
		{"RETAIL_TIMESCALE_SCHEMA", &cfg.timescaleSchema, required}, {"RETAIL_KLEND_PROXY_PATH", &cfg.proxyPath, required}, {"RETAIL_KLEND_PROXY_SHA256", &cfg.proxyHash, required},
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
	cfg.slotDuration, err = time.ParseDuration(os.Getenv("RETAIL_SLOT_DURATION"))
	if err != nil || cfg.slotDuration <= 0 || cfg.slotDuration > 10*time.Second {
		return cfg, errors.New("RETAIL_SLOT_DURATION must be positive and at most ten seconds")
	}
	if len(cfg.proxyHash) != 64 {
		return cfg, errors.New("RETAIL_KLEND_PROXY_SHA256 must be a SHA-256 digest")
	}
	digest, err := hex.DecodeString(cfg.proxyHash)
	if err != nil || len(digest) != 32 {
		return cfg, errors.New("RETAIL_KLEND_PROXY_SHA256 must be a SHA-256 digest")
	}
	cfg.proxyHash = strings.ToLower(cfg.proxyHash)
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
	if err := cfg.fleetConfig().Validate(); err != nil {
		return cfg, retailError("fleet configuration", err)
	}
	return cfg, nil
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
	return fleet.Config{DatabaseURL: c.databaseURL, TimescaleURL: c.timescaleURL, TimescaleSchema: c.timescaleSchema, RPCURL: c.rpcURL, Cluster: "mainnet-beta", Mode: fleet.ModePublish, PollInterval: time.Second, SlotDuration: c.slotDuration, KLendProxyPath: c.proxyPath, KLendProxySHA256: c.proxyHash, DelegatedSigner: base58.Encode(c.delegate[32:]), RevalidationOwner: "retail", RevalidationLeaseTTL: 30 * time.Second, RevalidationPollInterval: 250 * time.Millisecond, RevalidationConcurrency: 16, RevalidationComputeLimit: 1_400_000, RevalidatorEnabled: true, FusedExecute: true, CrossMintEnabled: c.crossMintEnabled, CrossMintMaxValueLossBPS: c.crossMintMaxValueLossBPS, CrossMintMaxSlippageBPS: c.crossMintMaxSlippageBPS, JupiterBuildURL: c.jupiterBuildURL, JupiterAPIKey: c.jupiterAPIKey}
}

// The outer diagnostic retains error identity for cancellation and inspection
// without rendering DSNs, provider URLs, signing material or returned payloads.
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

// retailFamilies parses RETAIL_FAMILIES: the families this process writes.
// Each cutover moves one family from its stopped Rust worker to Go.
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

// facts is the family health surface; metrics is the joined /metrics lane.
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
	// One writer per family: hold each family's session lock on the direct
	// database before any lane starts, and stop every lane if one is lost.
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
	// Every family lands its signed rows through the same send path.
	landRPC, err := solana.NewLandRPC(cfg.rpcURL, 15*time.Second)
	if err != nil {
		return retailError("landing RPC", err)
	}
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	proxy, err := fleet.NewKLendProxy(cfg.proxyPath, cfg.proxyHash)
	if err != nil {
		return retailError("pinned KLend helper", err)
	}
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
	if err := aStore.RequireSchema(startup); err != nil {
		return retailError("Autodeposit schema", err)
	}
	cStore, err := fleet.NewStoreFromPool(yieldPool)
	if err != nil {
		return retailError("fleet store", err)
	}
	if err := db.RequireTables(startup, yieldPool, "loyal_yield.fleet_planning_clusters", "loyal_yield.optimizer_epochs", "loyal_yield.rebalance_opportunities", "loyal_yield.rebalance_decisions", "loyal_yield.vault_reserve_positions_current", "loyal_yield.lookup_table_addresses", "loyal_yield.lookup_table_families", "loyal_yield.lookup_table_operations", "loyal_yield.lookup_table_provisioning_requests", "loyal_yield.lookup_table_provisioning_request_consumers", "loyal_yield.lookup_table_vault_bindings", "loyal_yield.route_lookup_tables"); err != nil {
		return retailError("fleet schema", err)
	}
	evidence, err := fleet.NewMarketEvidenceStoreFromPool(marketPool, cfg.timescaleSchema)
	if err != nil {
		return retailError("market evidence", err)
	}
	if err := db.RequireTables(startup, marketPool, cfg.timescaleSchema+".supported_reserves", cfg.timescaleSchema+".latest_verified_reserve_updates"); err != nil {
		return retailError("market schema", err)
	}
	dStore, err := fleetexec.NewStore(startup, yieldPool)
	if err != nil {
		return retailError("fleet executor schema", err)
	}
	gStore, err := multiply.NewStoreFromPool(startup, yieldPool)
	if err != nil {
		return retailError("Multiply schema", err)
	}
	gRPC := multiply.NewLiveRPCSurface(cfg.rpcURL)
	gExecutor, err := multiply.NewExecutorWithFeePayerContext(startup, gRPC, cfg.feePayer, cfg.delegate)
	if err != nil {
		return retailError("mainnet genesis", err)
	}
	chain, err := autodeposit.NewRPCChain(cfg.rpcURL)
	if err != nil {
		return retailError("Autodeposit chain", err)
	}
	rentRPC, err := backyard.NewRPCClient(cfg.rpcURL)
	if err != nil {
		return retailError("rent RPC", err)
	}
	wires, err := autodeposit.NewSweepWireBuilderWithSetup(proxy, cfg.delegate, chain.ReadAccountsWithOptional, rentRPC.MinimumBalanceForRentExemption)
	if err != nil {
		return retailError("Autodeposit wires", err)
	}
	controller, err := autodeposit.NewController(autodeposit.ControllerDependencies{Store: aStore, Chain: chain, Wires: wires, Facts: facts})
	if err != nil {
		return retailError("Autodeposit controller", err)
	}
	lookupRPC, err := fleetexec.NewLookupRPC(cfg.rpcURL, 10*time.Second)
	if err != nil {
		return retailError("lookup RPC", err)
	}
	lookupWorker, err := fleetexec.NewLookupWorker(dStore, lookupRPC, fleetexec.LookupWorkerConfig{
		Cluster: "mainnet-beta", Owner: owner, LeaseTTL: 30 * time.Second,
		TickDeadline: 20 * time.Second, PollInterval: time.Second,
		Budget: cfg.lookup.budget, ReconcileOnly: !cfg.lookup.active, Facts: facts,
		OnHealth: func(err error) {
			if err != nil {
				log.Print("retail lookup writer requires attention")
			}
		},
	}, cfg.lookup.managerKey())
	if err != nil {
		return retailError("lookup writer", err)
	}
	if err := dStore.RequireLookupSchema(startup); err != nil {
		return retailError("lookup writer schema", err)
	}
	// Retain the source provisioner's growth reservation (8) and vault cohort
	// limit (16). This lane receives no manager key or broadcast capability.
	lookupPlanner, err := fleetexec.NewLookupPlanner(dStore, lookupRPC, fleetexec.LookupPlannerConfig{
		Cluster: "mainnet-beta", Owner: owner, LeaseTTL: 30 * time.Second,
		TickDeadline: 20 * time.Second, PollInterval: time.Second,
		CatalogInterval: time.Minute, GrowthReservation: 8, MaximumVaultCohort: 16,
		ReconcileOnly: !cfg.lookup.active, Facts: facts,
		OnHealth: func(err error) {
			if err != nil {
				log.Print("retail lookup planner requires attention")
			}
		},
	})
	if err != nil {
		return retailError("lookup planner", err)
	}
	aWorker, err := autodeposit.NewWorker(autodeposit.WorkerDependencies{Store: aStore, Executor: controller, Facts: facts, OnError: func(error) { log.Print("retail autodeposit tick failed") }, OnAlert: func(autodeposit.ExecutorFailureAlert) {
		log.Print("retail autodeposit execution requires attention")
	}})
	if err != nil {
		return retailError("Autodeposit worker", err)
	}
	artifactRPC, err := autodeposit.NewArtifactRPC(cfg.rpcURL)
	if err != nil {
		return retailError("Autodeposit artifact RPC", err)
	}
	artifactReader := &autodeposit.ArtifactProofReader{Wires: wires, History: artifactRPC}
	artifacts := &autodeposit.ArtifactReconciler{Store: aStore, Reader: artifactReader}
	control := &autodeposit.ControlReconciler{Store: aStore, Reader: wires, Artifacts: artifacts, OnError: func(error) { log.Print("retail autodeposit control requires attention") }, PollInterval: time.Second, LeaseDuration: 120 * time.Second}
	fleetRPC := fleet.NewRPCClient(cfg.rpcURL)
	cConfig := cfg.fleetConfig()
	cConfig.RevalidationOwner = owner
	planner, err := fleet.NewWorker(cConfig, cStore, fleetRPC, facts)
	if err != nil {
		return retailError("fleet planner", err)
	}
	if err := planner.SetMarketEvidence(evidence); err != nil {
		return retailError("fleet evidence binding", err)
	}
	// Compilation/verification remains available for recovery with rollout off.
	// Only the planner and D controller receive fresh cross-mint enablement.
	revalidator, err := fleet.NewRevalidator(cStore, fleetRPC, proxy, fleet.RevalidatorConfig{Owner: owner, DelegatedSigner: cConfig.DelegatedSigner, LeaseTTL: cConfig.RevalidationLeaseTTL, ComputeLimit: cConfig.RevalidationComputeLimit, SlotDuration: cfg.slotDuration, FusedExecute: true, CrossMintEnabled: true, CrossMintMaxValueLossBPS: cfg.crossMintMaxValueLossBPS, CrossMintMaxSlippageBPS: cfg.crossMintMaxSlippageBPS, JupiterBuildURL: cfg.jupiterBuildURL, JupiterAPIKey: cfg.jupiterAPIKey})
	if err != nil {
		return retailError("fused fleet preparation", err)
	}
	// Fused preparation belongs to the executor. C.SetRevalidator would run the
	// incompatible durable Cycle path and must never be installed here.
	executionRPC, err := fleetexec.NewRPCAdapter(cfg.rpcURL, 15*time.Second)
	if err != nil {
		return retailError("fleet execution RPC", err)
	}
	executor, err := fleetexec.NewWorker(fleetexec.Config{Cluster: cConfig.Cluster, Owner: owner, LeaseTTL: 30 * time.Second, BatchSize: 20, TickInterval: 750 * time.Millisecond, SlotDuration: cfg.slotDuration, Facts: facts}, dStore, landRPC, executionRPC, fleetexec.DelegateSigner{FeePayer: cfg.delegate})
	if err != nil {
		return retailError("fleet executor", err)
	}
	if err := executor.SetFreshRevalidator(revalidator); err != nil {
		return retailError("fleet fresh execution binding", err)
	}
	crossMint, err := composeRetailCrossMint(startup, cfg, owner, dStore, revalidator, executionRPC, evidence, facts)
	if err != nil {
		return retailError("cross-mint runtime", err)
	}
	observation, err := multiply.NewLiveObservationReader(gRPC)
	if err != nil {
		return retailError("Multiply observation", err)
	}
	multiplyWorker, err := multiply.NewWorker(multiply.WorkerDeps{Store: gStore, Observer: observation, Executor: gExecutor, Quotes: multiply.NewLiveQuoteClient(), WorkerID: owner, Chain: landRPC, Facts: facts})
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
			lanes = append(lanes, planner, executor, crossMint)
		case engine.FamilyLookup:
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
