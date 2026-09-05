// This producer compares deterministic planner and same-mint wire artifacts.
// It does not claim RPC simulation, queue transitions, or retained lifecycle execution.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/kamino-fleet-planner/internal/fleet"
)

func fatal(err error) { fmt.Fprintln(os.Stderr, "kamino fleet parity:", err); os.Exit(1) }
func main() {
	if len(os.Args) != 2 {
		fatal(fmt.Errorf("contract path required"))
	}
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
	plan, err := buildPlan(clock)
	if err != nil {
		fatal(err)
	}
	proxy, err := fleet.NewKLendProxy(os.Getenv("KAMINO_PARITY_KLEND_PROXY"), os.Getenv("KAMINO_PARITY_KLEND_PROXY_SHA256"))
	if err != nil {
		fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(os.Args[1]), "kamino-route-v1.json"))
	if err != nil {
		fatal(err)
	}
	var request fleet.KaminoSameMintRouteRequest
	if err = json.Unmarshal(raw, &request); err != nil {
		fatal(err)
	}
	route, err := proxy.Build(context.Background(), request)
	if err != nil {
		fatal(err)
	}
	addresses := []string{}
	for _, ix := range append(append([]fleet.RouteInstruction{}, route.Public...), route.Protected...) {
		for _, a := range ix.Accounts {
			addresses = append(addresses, a.Address)
		}
	}
	addresses = unique(append(addresses, request.Source.CollateralMint, request.Vault))
	// PrepareRoute's callback is deliberately a stub here; no simulation result
	// is emitted as evidence. RPC behavior is tested separately by Go tests.
	simulate := func(wire []byte) (fleet.SimulationEvidence, error) {
		h := sha256.Sum256(wire)
		return fleet.SimulationEvidence{Slot: 1001, Succeeded: true, UnitsConsumed: 617432, WireSHA256: hex.EncodeToString(h[:])}, nil
	}
	table := fleet.LookupTable{Address: request.Target.Market, Addresses: addresses, Active: true, UsableAfterSlot: 999, LastVerifiedSlot: 1000}
	prep, err := fleet.PrepareRoute(route, request.Source.CollateralMint, request.Vault, 0, []uint8{0, 1}, []fleet.LookupTable{table}, request.Source.Market, 17334, 900000, simulate)
	if err != nil {
		fatal(err)
	}
	artifact := map[string]any{
		"schemaVersion": 2, "implementation": "go", "scope": "deterministic_planner_and_same_mint_wire",
		"fixture":       map[string]any{"id": "kamino-planner-revalidator-replacement-v1", "sha256": digest, "clock": clock.Format(time.RFC3339)},
		"opportunities": plan,
		"route":         map[string]any{"fingerprint": prep.RouteFingerprint, "messageHex": hex.EncodeToString(prep.Transaction.Message), "wireHex": hex.EncodeToString(prep.Transaction.UnsignedWire)},
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if err = enc.Encode(artifact); err != nil {
		fatal(err)
	}
}

func buildPlan(now time.Time) ([]map[string]any, error) {
	snapshot := fleet.MarketSnapshot{OptimizerEpochID: 7, ExpiresAt: now.Add(5 * time.Minute), Slot: 1000, ObservedAt: now, Hash: strings.Repeat("a", 64), Reserves: map[string]fleet.ReserveState{
		"reserve-a": {ReserveIdentity: fleet.ReserveIdentity{Address: "reserve-a", Market: "market-a", Mint: fleet.USDCMint}, Slot: 1000, SupplyAPYBPS: 81, TotalSupplyUSDMicros: 1_000_000_000_000, EconomicLifetimeMillis: 120000},
		"reserve-b": {ReserveIdentity: fleet.ReserveIdentity{Address: "reserve-b", Market: "market-b", Mint: fleet.USDCMint}, Slot: 1000, SupplyAPYBPS: 919, TotalSupplyUSDMicros: 1_000_000_000_000, EconomicLifetimeMillis: 120000},
		"reserve-c": {ReserveIdentity: fleet.ReserveIdentity{Address: "reserve-c", Market: "market-c", Mint: fleet.USDCMint}, Slot: 1000, SupplyAPYBPS: 500, TotalSupplyUSDMicros: 1_000_000_000_000, EconomicLifetimeMillis: 120000},
	}}
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
func unique(v []string) []string {
	m := map[string]bool{}
	out := []string{}
	for _, s := range v {
		if s != "" && !m[s] {
			m[s] = true
			out = append(out, s)
		}
	}
	return out
}
