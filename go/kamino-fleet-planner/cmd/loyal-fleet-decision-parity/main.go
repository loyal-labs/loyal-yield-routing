package main

// A separate diagnostic: reads the same policy-eligible reserve-source frontier
// as the Rust producer, but runs the actual Go fleet planner. No DB/RPC/signing.
import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/loyal-labs/loyal-yield-routing/go/kamino-fleet-planner/internal/fleet"
	"os"
	"time"
)

func fatal(err any) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
func main() {
	path := os.Getenv("FLEET_DECISION_FIXTURE")
	raw, err := os.ReadFile(path)
	if err != nil {
		fatal(err)
	}
	var input struct {
		SchemaVersion int `json:"schemaVersion"`
		Cases         []struct {
			Name     string `json:"name"`
			Reserves map[string]struct {
				Mint                         string
				Supply, APY, Inflow, Outflow int64
			} `json:"reserves"`
			Vaults []struct {
				ID                 int64
				Source             string
				Targets            []string
				Amount, Collateral int64
				Tenant             string
			} `json:"vaults"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(raw, &input); err != nil || input.SchemaVersion != 1 {
		fatal(fmt.Errorf("invalid fixture: %v", err))
	}
	results := []any{}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range input.Cases {
		snapshot := fleet.MarketSnapshot{OptimizerEpochID: 7, Slot: 1000, ObservedAt: now, ExpiresAt: now.Add(5 * time.Minute), Reserves: map[string]fleet.ReserveState{}}
		inflows, outflows := map[string]int64{}, map[string]int64{}
		for name, r := range c.Reserves {
			snapshot.Reserves[name] = fleet.ReserveState{ReserveIdentity: fleet.ReserveIdentity{Address: name, Market: "market-" + name, Mint: r.Mint}, Slot: 1000, SupplyAPYBPS: r.APY, TotalSupplyUSDMicros: r.Supply, EconomicLifetimeMillis: 120000}
			inflows[name], outflows[name] = r.Inflow, r.Outflow
		}
		vaults := []fleet.FleetVault{}
		for _, v := range c.Vaults {
			source := c.Reserves[v.Source]
			fv := fleet.FleetVault{Position: fleet.VaultPosition{VaultID: v.ID, SnapshotID: v.ID, VaultPubkey: fmt.Sprint("vault-", v.ID), PolicyID: 1, SourceReserve: v.Source, Market: "market-" + v.Source, Mint: source.Mint, AmountRaw: v.Amount, SourceCollateralAmountRaw: v.Collateral, SourceAmountSemantics: "kamino_obligation_collateral_deposited_amount"}, CommittedInflows: inflows, CommittedOutflows: outflows, CrossMintTargets: map[string]fleet.CrossMintPolicyBindings{}, CrossMintMaxValueLossBPS: 50}
			for _, target := range v.Targets {
				if c.Reserves[target].Mint == source.Mint {
					fv.AllowedTargets = append(fv.AllowedTargets, target)
				} else {
					// Capability admission is an explicit fixture assumption, not tested here.
					fv.CrossMintTargets[target] = fleet.CrossMintPolicyBindings{}
				}
			}
			vaults = append(vaults, fv)
		}
		plan, err := fleet.PlanFleet(snapshot, vaults)
		if err != nil {
			fatal(fmt.Errorf("%s: %w", c.Name, err))
		}
		selected := []any{}
		for _, o := range plan.Opportunities {
			d := o.Decision
			selected = append(selected, map[string]any{"vaultId": d.VaultID, "source": d.SourceReserve, "target": d.TargetReserve, "route": d.RouteKind, "amount": d.AmountRaw, "sourceApy": d.SourceAPYBPS, "targetApy": d.TargetAPYBPS, "edge": d.EdgeBPS, "netGain": d.ExpectedNetGainUSDMicros, "priority": d.EconomicPriority, "feeCap": d.EstimatedCostLamports})
		}
		results = append(results, map[string]any{"name": c.Name, "selected": selected})
	}
	hash := sha256.Sum256(raw)
	output, err := json.Marshal(map[string]any{"schemaVersion": 1, "implementation": "go", "fixtureSha256": hex.EncodeToString(hash[:]), "cases": results})
	if err != nil {
		fatal(err)
	}
	if err = os.WriteFile(os.Getenv("FLEET_DECISION_OUTPUT"), output, 0600); err != nil {
		fatal(err)
	}
}
