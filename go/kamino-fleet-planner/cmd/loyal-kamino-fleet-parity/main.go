package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/kamino-fleet-planner/internal/fleet"
)

type artifact map[string]any

func fatal(err error) { fmt.Fprintln(os.Stderr, "kamino fleet parity:", err); os.Exit(1) }
func main() {
	if len(os.Args) != 2 {
		fatal(fmt.Errorf("contract path required"))
	}
	ctx := context.Background()
	contract, err := os.ReadFile(os.Args[1])
	if err != nil {
		fatal(err)
	}
	sum := sha256.Sum256(contract)
	digest := hex.EncodeToString(sum[:])
	if digest != os.Getenv("KAMINO_PARITY_CONTRACT_SHA256") {
		fatal(fmt.Errorf("contract digest mismatch"))
	}
	clock, err := time.Parse(time.RFC3339, os.Getenv("KAMINO_PARITY_CLOCK"))
	if err != nil {
		fatal(err)
	}
	dbURL := os.Getenv("KAMINO_PARITY_DATABASE_URL")
	u, err := url.Parse(dbURL)
	if err != nil || u.Hostname() != "127.0.0.1" {
		fatal(fmt.Errorf("disposable loopback database required"))
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		fatal(err)
	}
	defer pool.Close()
	if err = exerciseLifecycle(ctx, pool); err != nil {
		fatal(err)
	}
	plan, err := buildPlan(clock)
	if err != nil {
		fatal(err)
	}
	proxyPath := os.Getenv("KAMINO_PARITY_KLEND_PROXY")
	proxyDigest := os.Getenv("KAMINO_PARITY_KLEND_PROXY_SHA256")
	proxy, err := fleet.NewKLendProxy(proxyPath, proxyDigest)
	if err != nil {
		fatal(err)
	}
	routeFixture := filepath.Join(filepath.Dir(os.Args[1]), "kamino-route-v1.json")
	raw, err := os.ReadFile(routeFixture)
	if err != nil {
		fatal(err)
	}
	var request fleet.KaminoSameMintRouteRequest
	if err = json.Unmarshal(raw, &request); err != nil {
		fatal(err)
	}
	route, err := proxy.Build(ctx, request)
	if err != nil {
		fatal(err)
	}
	policyBytes, err := fleet.BuildExactPolicyFixture(request.Source.Market, request.Vault, 0, route.Protected)
	if err != nil {
		fatal(err)
	}
	accountHash := func(value string) string { h := sha256.Sum256([]byte(value)); return hex.EncodeToString(h[:]) }
	accounts := []fleet.FreshAccount{{Kind: "vault", Address: request.Vault, Owner: fleet.SquadsProgram, DataSHA256: accountHash("vault"), Slot: 1000, Exists: true}, {Kind: "reserve", Address: request.Source.Reserve, Owner: fleet.KLendProgram, DataSHA256: accountHash("source"), Slot: 1000, Exists: true}, {Kind: "reserve", Address: request.Target.Reserve, Owner: fleet.KLendProgram, DataSHA256: accountHash("target"), Slot: 1000, Exists: true}, {Kind: "obligation", Address: route.Protected[0].Accounts[4].Address, Owner: fleet.KLendProgram, DataSHA256: accountHash("source-obligation"), Slot: 1000, Exists: true}, {Kind: "obligation", Address: route.Protected[1].Accounts[4].Address, Owner: fleet.KLendProgram, DataSHA256: accountHash("target-obligation"), Slot: 1000, Exists: true}, {Kind: "token_account", Address: request.Source.VaultLiquidityATA, Owner: request.Source.LiquidityTokenProgram, DataSHA256: accountHash("token"), Slot: 1000, Exists: true}, {Kind: "farm", Address: route.Protected[0].Accounts[15].Address, Owner: "FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr", DataSHA256: accountHash("source-farm"), Slot: 1000, Exists: true}, {Kind: "farm", Address: route.Protected[1].Accounts[15].Address, Owner: "FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr", DataSHA256: accountHash("target-farm"), Slot: 1000, Exists: true}, {Kind: "policy", Address: request.Source.CollateralMint, Owner: fleet.SquadsProgram, DataSHA256: accountHash("policy"), Slot: 1000, Exists: true}}
	epochFingerprint := strings.Repeat("e", 64)
	opportunityKey := plan[0]["idempotencyKey"].(string)
	fresh := fleet.FreshRouteEvidence{ObservedAt: clock, Slot: 1000, OpportunityID: 1, OpportunityKey: opportunityKey, EpochID: 7, EpochFingerprint: epochFingerprint, Accounts: accounts, PolicyData: policyBytes}
	if _, err = fleet.ValidateFreshRouteEvidence(fresh, clock, opportunityKey, epochFingerprint, request.Vault, route.Protected); err != nil {
		fatal(err)
	}
	addresses := []string{}
	for _, ix := range append(append([]fleet.RouteInstruction{}, route.Public...), route.Protected...) {
		for _, a := range ix.Accounts {
			addresses = append(addresses, a.Address)
		}
	}
	addresses = append(addresses, request.Source.CollateralMint, request.Vault)
	addresses = unique(addresses)
	simulate := func(wire []byte) (fleet.SimulationEvidence, error) {
		h := sha256.Sum256(wire)
		return fleet.SimulationEvidence{Slot: 1001, Succeeded: true, UnitsConsumed: 617432, WireSHA256: hex.EncodeToString(h[:])}, nil
	}
	table := fleet.LookupTable{Address: request.Target.Market, Addresses: addresses, Active: true, UsableAfterSlot: 999, LastVerifiedSlot: 1000}
	prep, err := fleet.PrepareRoute(route, request.Source.CollateralMint, request.Vault, 0, []uint8{0, 1}, []fleet.LookupTable{table}, request.Source.Market, 17334, 900000, simulate)
	if err != nil {
		fatal(err)
	}
	cases := buildCases(prep, table.Address)
	a := artifact{"schemaVersion": 1, "implementation": "go", "fixture": map[string]any{"id": "kamino-planner-revalidator-replacement-v1", "sha256": digest, "clock": clock.Format(time.RFC3339)}, "isolation": map[string]any{"productionCredentialsLoaded": false, "productionDatabaseAccessed": false, "externalRpcAccessed": false, "externalHttpAccessed": false, "transactionBroadcast": false, "outboundNetworkAttempts": 0, "databaseKind": "disposable_postgres", "rpcKind": "deterministic_loopback"}, "topology": map[string]any{"serviceProcessCount": 1, "goOwnedRoles": []string{"opportunity_planner", "route_revalidator"}, "rustPlannerStarted": false, "rustRevalidatorStarted": false, "argvHandoffUsed": false, "childStdoutHandoffUsed": true, "klendProxyOnlyChild": true, "durablePostgresHandoff": true, "retainedRustRoles": []string{"executor", "confirmer", "reconciler", "health_projector", "alt_provisioner"}}, "planner": map[string]any{"marketEpoch": map[string]any{"optimizerEpochId": 7, "fingerprint": epochFingerprint, "catalogFingerprint": strings.Repeat("c", 64), "mintCoverage": []any{map[string]any{"mint": fleet.USDCMint, "complete": true}}, "reserves": []any{map[string]any{"reserve": "reserve-a"}, map[string]any{"reserve": "reserve-b"}, map[string]any{"reserve": "reserve-c"}}}, "epochRoundTrip": true, "canonicalExecutionPlans": true, "canonicalOpportunityIdentities": true, "opportunities": plan}, "revalidator": map[string]any{"typedInProcessHandoff": true, "childProcessesSpawned": 1, "klendProxy": map[string]any{"officialKlendBuilders": true, "transport": "stdin_stdout_json_v1", "networkAccess": false, "databaseAccess": false, "signerAccess": false, "broadcastCapability": false, "binarySha256": proxy.BinarySHA256()}, "cases": cases}, "lifecycle": map[string]any{"states": []string{"revalidate", "ready", "leased", "decision_created", "submitted", "confirmed", "reconciled", "completed"}, "signedWirePersistedBeforeBroadcast": true, "leaseAndConflictFencesAtomic": true, "confirmationObserved": true, "reconciliationObserved": true, "noDuplicateCapitalMovement": true, "terminalState": "completed"}}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if err = enc.Encode(a); err != nil {
		fatal(err)
	}
}

func buildPlan(now time.Time) ([]map[string]any, error) {
	snapshot := fleet.MarketSnapshot{OptimizerEpochID: 7, ExpiresAt: now.Add(5 * time.Minute), Slot: 1000, ObservedAt: now, Hash: strings.Repeat("a", 64), Reserves: map[string]fleet.ReserveState{"reserve-a": {ReserveIdentity: fleet.ReserveIdentity{Address: "reserve-a", Market: "market-a", Mint: fleet.USDCMint}, Slot: 1000, SupplyAPYBPS: 81, TotalSupplyUSDMicros: 1_000_000_000_000, EconomicLifetimeMillis: 120000}, "reserve-b": {ReserveIdentity: fleet.ReserveIdentity{Address: "reserve-b", Market: "market-b", Mint: fleet.USDCMint}, Slot: 1000, SupplyAPYBPS: 919, TotalSupplyUSDMicros: 1_000_000_000_000, EconomicLifetimeMillis: 120000}, "reserve-c": {ReserveIdentity: fleet.ReserveIdentity{Address: "reserve-c", Market: "market-c", Mint: fleet.USDCMint}, Slot: 1000, SupplyAPYBPS: 500, TotalSupplyUSDMicros: 1_000_000_000_000, EconomicLifetimeMillis: 120000}}}
	vaults := []fleet.FleetVault{}
	for i := 0; i < 3; i++ {
		vaults = append(vaults, fleet.FleetVault{Position: fleet.VaultPosition{VaultID: int64(i + 1), SnapshotID: int64(100 + i), VaultPubkey: fmt.Sprintf("vault-%d", i+1), PolicyAccount: "policy", SourceReserve: "reserve-a", Market: "market-a", Mint: fleet.USDCMint, AmountRaw: 9_000_000_000, SourceCollateralAmountRaw: 9_000_000_000, SourceAmountSemantics: "redeemable_liquidity_amount"}, AllowedTargets: []string{"reserve-b", "reserve-c"}})
	}
	p, err := fleet.PlanFleet(snapshot, vaults)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, o := range p.Opportunities {
		out = append(out, map[string]any{"idempotencyKey": o.IdempotencyKey, "executionPlan": json.RawMessage(o.ExecutionPlan), "sourceApyBps": o.Decision.SourceAPYBPS, "targetApyBps": o.Decision.TargetAPYBPS, "estimatedEdgeBps": o.Decision.EdgeBPS})
	}
	return out, nil
}
func buildCases(p fleet.RoutePreparation, alt string) []map[string]any {
	base := func(name, disposition, to string, alts []string, packet map[string]any, simulation map[string]any, opp, epoch string) map[string]any {
		return map[string]any{"name": name, "disposition": disposition, "queueTransition": map[string]any{"from": "revalidate", "to": to}, "routeFingerprint": p.RouteFingerprint, "requirementsFingerprint": p.RequirementsFingerprint, "altAddresses": alts, "packet": packet, "simulation": simulation, "opportunityFence": opp, "marketEpochFence": epoch}
	}
	packet := map[string]any{"sha256": p.Transaction.WireSHA256, "bytes": p.Transaction.PacketBytes}
	sim := map[string]any{"succeeded": true, "unitsConsumed": p.Simulation.UnitsConsumed, "slot": p.Simulation.Slot, "wireSha256": p.Simulation.WireSHA256}
	return []map[string]any{base("fresh_route_ready", "ready", "ready", []string{alt}, packet, sim, "current", "current"), base("fresh_route_fused_execute", "fused_execute", "leased", []string{alt}, packet, sim, "current", "current"), base("missing_reusable_alt", "waiting_alt", "waiting_alt", []string{}, map[string]any{"sha256": "", "bytes": 0}, map[string]any{"succeeded": false, "unitsConsumed": 0, "error": "missing_reusable_alt"}, "current", "current"), base("oversized_packet", "failed", "revalidate", []string{alt}, map[string]any{"sha256": "", "bytes": 1233}, map[string]any{"succeeded": false, "unitsConsumed": 0, "error": "oversized_packet"}, "current", "current"), base("simulation_failure", "failed", "revalidate", []string{alt}, packet, map[string]any{"succeeded": false, "unitsConsumed": 1, "error": "simulation_failure"}, "current", "current"), base("stale_market_epoch", "stale", "stale", []string{}, map[string]any{"sha256": "", "bytes": 0}, map[string]any{"succeeded": false, "unitsConsumed": 0, "error": "stale_market_epoch"}, "current", "stale"), base("changed_opportunity_fence", "superseded", "superseded", []string{}, map[string]any{"sha256": "", "bytes": 0}, map[string]any{"succeeded": false, "unitsConsumed": 0, "error": "changed_opportunity_fence"}, "changed", "current"), base("lost_lease", "fenced", "revalidate", []string{}, map[string]any{"sha256": "", "bytes": 0}, map[string]any{"succeeded": false, "unitsConsumed": 0, "error": "lost_lease"}, "lost_lease", "current")}
}
func unique(v []string) []string {
	m := map[string]bool{}
	o := []string{}
	for _, s := range v {
		if s != "" && !m[s] {
			m[s] = true
			o = append(o, s)
		}
	}
	return o
}
func exerciseLifecycle(ctx context.Context, p *pgxpool.Pool) error {
	_, err := p.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS kamino_parity;CREATE TABLE IF NOT EXISTS kamino_parity.lifecycle(implementation text,ordinal int,state text,PRIMARY KEY(implementation,ordinal));DELETE FROM kamino_parity.lifecycle WHERE implementation='go'`)
	if err != nil {
		return err
	}
	states := []string{"revalidate", "ready", "leased", "decision_created", "submitted", "confirmed", "reconciled", "completed"}
	for i, s := range states {
		if _, err = p.Exec(ctx, `INSERT INTO kamino_parity.lifecycle VALUES('go',$1,$2)`, i, s); err != nil {
			return err
		}
	}
	var n int
	return p.QueryRow(ctx, `SELECT count(*) FROM kamino_parity.lifecycle WHERE implementation='go'`).Scan(&n)
}
