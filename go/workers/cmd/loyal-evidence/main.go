package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"io"
	"os"
	"time"
)

func run() error {
	kind := flag.String("kind", "", "fleet, fleet-wave, voltr or backyard saved decision")
	path := flag.String("snapshot", "", "saved input JSON; no database or RPC")
	flag.Parse()
	if *path == "" {
		return errors.New("snapshot path required")
	}
	input, err := os.Open(*path)
	if err != nil {
		return err
	}
	defer input.Close()
	result, err := decide(*kind, input)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

// decide runs one saved decision. It never opens a database, RPC or signer.
func decide(kind string, input io.Reader) (any, error) {
	decode := json.NewDecoder(input)
	decode.DisallowUnknownFields()
	var result any
	var err error
	switch kind {
	case "fleet":
		var in struct {
			Snapshot       fleet.MarketSnapshot
			Position       fleet.VaultPosition
			Source, Target string
		}
		if err := decode.Decode(&in); err != nil {
			return nil, err
		}
		result = fleet.Plan(in.Snapshot, in.Position, in.Source, in.Target)
	case "fleet-wave":
		var in struct {
			Snapshot fleet.MarketSnapshot
			Vaults   []fleet.FleetVault
			Limits   *fleet.WaveLimits
		}
		if err := decode.Decode(&in); err != nil {
			return nil, err
		}
		limits := fleet.DefaultWaveLimits()
		if in.Limits != nil {
			limits = *in.Limits
		}
		result, err = fleet.PlanFleetWithLimits(in.Snapshot, in.Vaults, limits)
		if err != nil {
			return nil, err
		}
	case "voltr":
		// One Backyard Voltr planning call on a saved confirmed observation and
		// market epoch. OptimizerEpochRowID is the loyal_yield.optimizer_epochs
		// id the durable opportunity references, so the key equals the row's
		// idempotency_key.
		var in struct {
			Observation         fleet.VoltrObservation
			Epoch               fleet.ImmutableMarketEpoch
			OptimizerEpochRowID int64
			VaultID             int64
			LastOptimization    *time.Time
			EvaluatedAt         time.Time
		}
		if err := decode.Decode(&in); err != nil {
			return nil, err
		}
		if in.EvaluatedAt.IsZero() || in.OptimizerEpochRowID <= 0 || in.VaultID <= 0 {
			return nil, errors.New("voltr requires evaluatedAt, a positive optimizerEpochRowId and vaultId")
		}
		route, err := fleet.LoadVoltrRoute()
		if err != nil {
			return nil, err
		}
		opportunity, err := fleet.PlanVoltr(route, in.Observation, in.Epoch, in.VaultID, in.LastOptimization, in.EvaluatedAt)
		if err != nil {
			return nil, err
		}
		out := struct {
			Opportunity    *fleet.VoltrOpportunity
			OpportunityKey *string
		}{Opportunity: opportunity}
		if opportunity != nil {
			key := fleet.VoltrOpportunityKey(route.Cluster, in.OptimizerEpochRowID, *opportunity)
			out.OpportunityKey = &key
		}
		result = out
	case "backyard":
		var in backyard.Snapshot
		if err := decode.Decode(&in); err != nil {
			return nil, err
		}
		result = backyard.Decide(in)
	default:
		return nil, errors.New("unsupported decision kind")
	}
	var trailing any
	if err := decode.Decode(&trailing); err != io.EOF {
		return nil, errors.New("snapshot has trailing JSON")
	}
	return result, nil
}
func main() {
	if len(os.Args) == 2 && os.Args[1] == "--role-probe" {
		fmt.Println(`{"schemaVersion":1,"role":"evidence","networkAccessed":false,"secretsLoaded":false,"databaseMutated":false,"transactionSent":false}`)
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
