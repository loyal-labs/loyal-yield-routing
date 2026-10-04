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
	"net"
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
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/observability"
	"github.com/mr-tron/base58"
)

type retailConfig struct {
	databaseURL, timescaleURL, rpcURL, timescaleSchema, httpAddress, proxyPath, proxyHash string
	slotDuration                                                                          time.Duration
	delegate, feePayer                                                                    ed25519.PrivateKey
}

// Configuration is scoped to this capability. Legacy/background writer flags
// are not authority to start a new retail engine; active is an explicit opt-in.
func loadRetailConfig() (retailConfig, error) {
	var cfg retailConfig
	if os.Getenv("RETAIL_MODE") != "active" {
		return cfg, errors.New("RETAIL_MODE must explicitly be active; saved replay uses loyal-evidence")
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
		if enabled {
			return cfg, errors.New("retail cross-mint execution is not wired")
		}
	}
	for _, field := range []struct {
		name string
		out  *string
	}{
		{"RETAIL_DATABASE_URL", &cfg.databaseURL}, {"RETAIL_TIMESCALE_DATABASE_URL", &cfg.timescaleURL}, {"RETAIL_SOLANA_RPC_URL", &cfg.rpcURL},
		{"RETAIL_TIMESCALE_SCHEMA", &cfg.timescaleSchema}, {"RETAIL_HTTP_ADDRESS", &cfg.httpAddress}, {"RETAIL_KLEND_PROXY_PATH", &cfg.proxyPath}, {"RETAIL_KLEND_PROXY_SHA256", &cfg.proxyHash},
	} {
		value, err := required(field.name)
		if err != nil {
			return cfg, err
		}
		*field.out = value
	}
	rpc, err := url.Parse(cfg.rpcURL)
	if err != nil || rpc.Host == "" || (rpc.Scheme != "http" && rpc.Scheme != "https") {
		return cfg, errors.New("RETAIL_SOLANA_RPC_URL must be an absolute HTTP or HTTPS URL")
	}
	if _, _, err := net.SplitHostPort(cfg.httpAddress); err != nil {
		return cfg, errors.New("RETAIL_HTTP_ADDRESS must contain host and port")
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
		material, err := required(field.name)
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
	return fleet.Config{DatabaseURL: c.databaseURL, TimescaleURL: c.timescaleURL, TimescaleSchema: c.timescaleSchema, RPCURL: c.rpcURL, Cluster: "mainnet-beta", Mode: fleet.ModePublish, PollInterval: time.Second, SlotDuration: c.slotDuration, KLendProxyPath: c.proxyPath, KLendProxySHA256: c.proxyHash, DelegatedSigner: base58.Encode(c.delegate[32:]), RevalidationOwner: "retail", RevalidationLeaseTTL: 30 * time.Second, RevalidationPollInterval: 250 * time.Millisecond, RevalidationConcurrency: 16, RevalidationComputeLimit: 1_400_000, RevalidatorEnabled: true, FusedExecute: true}
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

func retailHealth() *observability.Health {
	health := observability.NewHealth()
	for _, family := range []string{"autodeposit-control", "autodeposit", "fleet-planner", "fleet-executor", "multiply"} {
		health.SetDomainReady(family, false)
	}
	// Each autonomous family opens its gate only after its own healthy cycle;
	// the aggregate separately enforces freshness for every family.
	return health
}

func runRetailLanes(ctx context.Context, lanes ...engine.Lane) error {
	return retailError("lanes", engine.Run(ctx, lanes...))
}

func runRetail(ctx context.Context, owner, release string) error {
	if ctx == nil {
		return errors.New("retail requires caller context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	parts := strings.Split(owner, ":")
	if len(parts) != 4 || parts[0] != "worker" || parts[1] != "retail" || parts[3] != release {
		return errors.New("retail requires scoped instance and immutable release")
	}
	if _, err := engine.InstanceOwner("retail", parts[2], release); err != nil {
		return retailError("instance identity", err)
	}
	cfg, err := loadRetailConfig()
	if err != nil {
		return err
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
	controller, err := autodeposit.NewController(autodeposit.ControllerDependencies{Store: aStore, Chain: chain, Wires: wires})
	if err != nil {
		return retailError("Autodeposit controller", err)
	}
	health := retailHealth()
	readiness := newRetailReadiness(health)
	aWorker, err := autodeposit.NewWorker(autodeposit.WorkerDependencies{Store: aStore, Executor: controller, OnError: func(error) { health.SetDomainReady("autodeposit", false); log.Print("retail autodeposit tick failed") }, OnAlert: func(autodeposit.ExecutorFailureAlert) {
		health.SetDomainReady("autodeposit", false)
		log.Print("retail autodeposit execution requires attention")
	}})
	if err != nil {
		return retailError("Autodeposit worker", err)
	}
	aWorker.SetRuntimeReporter(readiness.reporter("autodeposit"))
	artifactRPC, err := autodeposit.NewArtifactRPC(cfg.rpcURL)
	if err != nil {
		return retailError("Autodeposit artifact RPC", err)
	}
	artifacts := &autodeposit.ArtifactReconciler{Store: aStore, Reader: &autodeposit.ArtifactProofReader{Wires: wires, History: artifactRPC}}
	control := &autodeposit.ControlReconciler{Store: aStore, Reader: wires, Artifacts: artifacts, RuntimeChain: chain, OnError: func(error) { log.Print("retail autodeposit control requires attention") }, PollInterval: time.Second, LeaseDuration: 120 * time.Second}
	control.SetRuntimeReporter(readiness.reporter("autodeposit-control"))
	fleetRPC := fleet.NewRPCClient(cfg.rpcURL)
	cConfig := cfg.fleetConfig()
	cConfig.RevalidationOwner = owner
	planner, err := fleet.NewWorker(cConfig, cStore, fleetRPC)
	if err != nil {
		return retailError("fleet planner", err)
	}
	planner.SetRuntimeReporter(readiness.reporter("fleet-planner"))
	if err := planner.SetMarketEvidence(evidence); err != nil {
		return retailError("fleet evidence binding", err)
	}
	revalidator, err := fleet.NewRevalidator(cStore, fleetRPC, proxy, fleet.RevalidatorConfig{Owner: owner, DelegatedSigner: cConfig.DelegatedSigner, LeaseTTL: cConfig.RevalidationLeaseTTL, ComputeLimit: cConfig.RevalidationComputeLimit, SlotDuration: cfg.slotDuration, FusedExecute: true})
	if err != nil {
		return retailError("fused fleet preparation", err)
	}
	// Fused preparation belongs to the executor. C.SetRevalidator would run the
	// incompatible durable Cycle path and must never be installed here.
	executionRPC, err := fleetexec.NewRPCAdapter(cfg.rpcURL, 15*time.Second)
	if err != nil {
		return retailError("fleet execution RPC", err)
	}
	executor, err := fleetexec.NewWorker(fleetexec.Config{Cluster: cConfig.Cluster, Owner: owner, LeaseTTL: 30 * time.Second, BatchSize: 20, TickInterval: 750 * time.Millisecond, SlotDuration: cfg.slotDuration}, dStore, executionRPC, executionRPC, fleetexec.DelegateSigner{FeePayer: cfg.delegate})
	if err != nil {
		return retailError("fleet executor", err)
	}
	executor.SetRuntimeReporter(readiness.reporter("fleet-executor"))
	if err := executor.SetFreshRevalidator(revalidator); err != nil {
		return retailError("fleet fresh execution binding", err)
	}
	observation, err := multiply.NewLiveObservationReader(gRPC)
	if err != nil {
		return retailError("Multiply observation", err)
	}
	multiplyWorker, err := multiply.NewWorker(multiply.WorkerDeps{Store: gStore, Observer: observation, Executor: gExecutor, Quotes: multiply.NewLiveQuoteClient(), WorkerID: owner})
	if err != nil {
		return retailError("Multiply worker", err)
	}
	multiplyWorker.SetRuntimeReporter(readiness.reporter("multiply"))
	if err := startup.Err(); err != nil {
		return err
	}
	server, err := engine.ListenHTTP(cfg.httpAddress, health.Handler(30*time.Second))
	if err != nil {
		return retailError("health listener", err)
	}
	defer server.Close()
	return runRetailLanes(ctx, control, aWorker, planner, executor, multiplyWorker, readiness, server)
}
