package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/stream"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

// runBackyard is LOYAL_WORKER_SCOPE=backyard: the separately credentialed
// Backyard family. The engine's one database session holds the family lock, Backyard's
// one-writer lock (backyard.OpenDatabase). A second process waits for it
// before it reads or writes anything; nothing below re-checks it.
func runBackyard(ctx context.Context, owner string, facts *engine.Facts, metrics engine.Lane) error {
	cfg, err := backyardRuntimeConfig()
	if err != nil {
		return err
	}
	material, err := engine.Credential("BACKYARD_POLICY_KEYPAIR")
	if err != nil {
		return err
	}
	credentials, err := backyard.ParseCredentials(material)
	if err != nil {
		return err
	}
	selector, err := backyardSelectorMode()
	if err != nil {
		return err
	}
	laserstream := stream.GRPCConnector{Endpoint: strings.TrimSpace(os.Getenv("BACKYARD_LASERSTREAM_ENDPOINT"))}
	if laserstream.Endpoint == "" {
		return errors.New("BACKYARD_LASERSTREAM_ENDPOINT is required")
	}
	if laserstream.APIKey, err = engine.Credential("BACKYARD_HELIUS_API_KEY"); err != nil {
		return err
	}
	database, err := backyard.OpenDatabase(ctx, engine.DirectDSN(cfg.DatabaseURL))
	if err != nil {
		return err
	}
	defer database.Close()
	cluster, err := chain.New(cfg.RPCURL, 15*time.Second)
	if err != nil {
		return err
	}
	view, err := backyard.OpenView(ctx, cluster, laserstream)
	if err != nil {
		return err
	}
	lane, err := backyard.NewEngine(backyard.EngineConfig{
		Database: database, RPC: cluster, View: view, Credentials: credentials, Config: backyard.DefaultConfig(), Owner: owner,
		Out: os.Stdout, Logger: slog.Default(), Facts: facts, Jupiter: cfg.Jupiter,
		Selector: selector,
	})
	if err != nil {
		return err
	}
	return engine.Run(ctx, lane, metrics)
}

func backyardRuntimeConfig() (backyard.RuntimeConfig, error) {
	var cfg backyard.RuntimeConfig
	var err error
	if cfg.DatabaseURL, err = engine.Credential("BACKYARD_DATABASE_URL"); err != nil {
		return cfg, err
	}
	if cfg.RPCURL, err = engine.Credential("BACKYARD_SOLANA_RPC_URL"); err != nil {
		return cfg, err
	}
	cfg.Jupiter = backyardJupiter(optionalCredential("JUPITER_API_KEY"))
	return cfg, cfg.Validate()
}

// backyardJupiter uses the keyed API when a key is configured. The keyless
// lite endpoint allows too few requests for one selector round (about 18
// parallel quote calls; 2026-09-25). Both serve the same swap/v1 API.
func backyardJupiter(key string) *jupiter.Client {
	base, key := jupiter.LiteBase, strings.TrimSpace(key)
	if key != "" {
		base = jupiter.KeyedBase
	}
	client, _ := jupiter.NewClient(base, key, &http.Client{Timeout: 20 * time.Second})
	return client
}

func optionalCredential(name string) string {
	value, err := engine.Credential(name)
	if err != nil {
		return ""
	}
	return value
}

func backyardSelectorMode() (backyard.SelectorMode, error) {
	switch os.Getenv("BACKYARD_RWA_SELECTOR_LIVE") {
	case "", "0":
		return backyard.SelectorOff, nil
	case "1":
		return backyard.SelectorLive, nil
	}
	return "", errors.New("BACKYARD_RWA_SELECTOR_LIVE must be 0 or 1")
}

const backyardUsage = `usage: loyal-engine backyard [in-flight | simulate-step --vault <vault> --lane <lane> --dir up|down --amount-raw <n> | selector-evaluate --vault <vault> [--execute] | clear-hold --vault <vault> --reason "<text>" | exit-lane --vault <vault> --lane <lane> (--by <operator> [--equity-zero] | --clear)]`

func runBackyardOperator(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 1 && args[0] == "in-flight" {
		url, err := engine.Credential("BACKYARD_DATABASE_URL")
		if err != nil {
			return err
		}
		total := 0
		for _, routeKey := range backyard.RouteKeys() {
			count, err := backyard.CountNonterminal(ctx, url, routeKey)
			if err != nil {
				return err
			}
			total += count
		}
		_, err = fmt.Fprintln(out, total)
		return err
	}
	if len(args) > 0 && args[0] == "simulate-step" {
		vault, lane, direction, amount, err := parseSimulateStepFlags(args[1:])
		if err != nil {
			return err
		}
		url, err := engine.Credential("BACKYARD_SOLANA_RPC_URL")
		if err != nil {
			return err
		}
		return backyard.RunSimulateStep(ctx, out, url, backyardJupiter(optionalCredential("JUPITER_API_KEY")), vault, lane, direction, amount)
	}
	cfg, err := backyardRuntimeConfig()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New(backyardUsage)
	}
	switch command, rest := args[0], args[1:]; {
	case command == "selector-evaluate" && len(rest) >= 2 && rest[0] == "--vault" && (len(rest) == 2 || len(rest) == 3 && rest[2] == "--execute"):
		return backyard.RunSelectorEvaluate(ctx, out, cfg, rest[1], len(rest) == 3)
	case command == "clear-hold":
		if len(rest) != 4 || rest[0] != "--vault" || rest[2] != "--reason" || rest[3] == "" {
			return errors.New(`usage: loyal-engine backyard clear-hold --vault <vault> --reason "<text>"`)
		}
		routeKey, err := backyard.RouteKey(rest[1])
		if err != nil {
			return err
		}
		result, err := backyard.ClearManualRecoveryHold(ctx, cfg.DatabaseURL, routeKey, rest[3])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, result)
		return err
	case command == "exit-lane":
		request, err := parseExitLaneFlags(rest)
		if err != nil {
			return err
		}
		result, err := backyard.RunLaneExit(ctx, cfg.DatabaseURL, request)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, result)
		return err
	}
	return errors.New(backyardUsage)
}

const simulateStepUsage = `usage: loyal-engine backyard simulate-step --vault <vault> --lane <lane> --dir up|down --amount-raw <n>`

func parseSimulateStepFlags(args []string) (vault, lane string, direction backyard.SimulatedStep, amount uint64, err error) {
	var dir backyard.SimulatedStep
	for index := 0; index < len(args); index++ {
		if index+1 >= len(args) {
			return "", "", "", 0, fmt.Errorf("simulate-step: %s requires a value\n%s", args[index], simulateStepUsage)
		}
		value := args[index+1]
		switch args[index] {
		case "--vault":
			vault = value
		case "--lane":
			lane = value
		case "--dir":
			dir = backyard.SimulatedStep(value)
		case "--amount-raw":
			if amount, err = strconv.ParseUint(value, 10, 64); err != nil {
				return "", "", "", 0, fmt.Errorf("simulate-step: --amount-raw requires an integer\n%s", simulateStepUsage)
			}
		default:
			return "", "", "", 0, fmt.Errorf("simulate-step: unexpected argument %q\n%s", args[index], simulateStepUsage)
		}
		index++
	}
	if vault == "" || lane == "" || (dir != backyard.SimulateUp && dir != backyard.SimulateDown) || amount == 0 {
		return "", "", "", 0, fmt.Errorf("simulate-step: --vault, --lane, --dir up|down and a positive --amount-raw are required\n%s", simulateStepUsage)
	}
	return vault, lane, dir, amount, nil
}

const exitLaneUsage = `usage: loyal-engine backyard exit-lane --vault <vault> --lane <lane> (--by <operator> [--equity-zero] | --clear)`

func parseExitLaneFlags(args []string) (backyard.LaneExitRequest, error) {
	var request backyard.LaneExitRequest
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--equity-zero":
			request.EquityZero = true
			continue
		case "--clear":
			request.Clear = true
			continue
		case "--vault", "--lane", "--by":
			if index+1 >= len(args) {
				return request, fmt.Errorf("exit-lane: %s requires a value\n%s", args[index], exitLaneUsage)
			}
			*map[string]*string{"--vault": &request.Vault, "--lane": &request.Lane, "--by": &request.RequestedBy}[args[index]] = args[index+1]
			index++
		default:
			return request, fmt.Errorf("exit-lane: unexpected argument %q\n%s", args[index], exitLaneUsage)
		}
	}
	if request.Vault == "" || request.Lane == "" || request.Clear == (request.RequestedBy != "") || (request.Clear && request.EquityZero) {
		return request, fmt.Errorf("exit-lane: --vault, --lane and either --by or --clear are required\n%s", exitLaneUsage)
	}
	return request, nil
}
