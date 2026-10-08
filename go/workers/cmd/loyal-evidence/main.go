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
	kind := flag.String("kind", "", "fleet, fleet-wave or backyard saved decision")
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
	decode := json.NewDecoder(input)
	decode.DisallowUnknownFields()
	var result any
	switch *kind {
	case "fleet":
		var in struct {
			Snapshot       fleet.MarketSnapshot
			Position       fleet.VaultPosition
			Source, Target string
		}
		if err := decode.Decode(&in); err != nil {
			return err
		}
		result = fleet.Plan(in.Snapshot, in.Position, in.Source, in.Target)
	case "fleet-wave":
		var in struct {
			Snapshot    fleet.MarketSnapshot
			Vaults      []fleet.FleetVault
			Limits      *fleet.WaveLimits
			EvaluatedAt time.Time
		}
		if err := decode.Decode(&in); err != nil {
			return err
		}
		if in.EvaluatedAt.IsZero() {
			return errors.New("fleet-wave requires an explicit evaluatedAt clock")
		}
		limits := fleet.DefaultWaveLimits()
		if in.Limits != nil {
			limits = *in.Limits
		}
		result, err = fleet.PlanFleetWithLimitsAt(in.Snapshot, in.Vaults, limits, in.EvaluatedAt)
		if err != nil {
			return err
		}
	case "backyard":
		var in backyard.Snapshot
		if err := decode.Decode(&in); err != nil {
			return err
		}
		result = backyard.Decide(in)
	default:
		return errors.New("unsupported decision kind")
	}
	var trailing any
	if err := decode.Decode(&trailing); err != io.EOF {
		return errors.New("snapshot has trailing JSON")
	}
	return json.NewEncoder(os.Stdout).Encode(result)
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
