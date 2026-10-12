package main

import (
	"context"
	"encoding/json"
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
// Backyard family. BACKYARD_DATABASE_URL must be the direct (non-pooler) DSN:
// it also holds the family lock.
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
	// Backyard plans from a LaserStream view of its own accounts.
	laserstream := stream.GRPCConnector{Endpoint: strings.TrimSpace(os.Getenv("BACKYARD_LASERSTREAM_ENDPOINT"))}
	if laserstream.Endpoint == "" {
		return errors.New("BACKYARD_LASERSTREAM_ENDPOINT is required")
	}
	if laserstream.APIKey, err = engine.Credential("BACKYARD_HELIUS_API_KEY"); err != nil {
		return err
	}
	lost, err := engine.HoldFamily(ctx, cfg.DatabaseURL, engine.FamilyBackyard)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		select {
		case <-lost:
			cancel(errBackyardLockLost)
		case <-ctx.Done():
		}
	}()
	cluster, err := chain.New(cfg.RPCURL, 15*time.Second)
	if err != nil {
		return err
	}
	// Strategy-two cutover gate: never run while a legacy seed 62-65 policy
	// still exists at finalized commitment (see the strategy-two runbook).
	if _, err := backyard.AssertLegacyPoliciesRetired(ctx, cluster); err != nil {
		return err
	}
	view, err := backyard.OpenView(ctx, cluster, laserstream)
	if err != nil {
		return err
	}
	database, err := backyard.OpenDatabase(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer database.Close()
	lane, err := backyard.NewEngine(backyard.EngineConfig{
		Database: database, RPC: cluster, View: view, Credentials: credentials, Config: backyard.DefaultConfig(), Owner: owner,
		Out: os.Stdout, Logger: slog.Default(), Facts: facts, Jupiter: cfg.Jupiter,
		Selector: selector,
	})
	if err != nil {
		return err
	}
	err = engine.Run(ctx, lane, metrics)
	if cause := context.Cause(ctx); errors.Is(cause, errBackyardLockLost) {
		return cause
	}
	return err
}

// errBackyardLockLost: another holder may now own the family; this process
// must exit with an error so its supervisor restarts it into the lock wait.
var errBackyardLockLost = errors.New("backyard family lock lost")

// backyardRuntimeConfig reads the Backyard database and RPC credentials shared
// by the engine and the one-shot operator commands.
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

// optionalCredential is for keys whose absence has a defined meaning: without
// JUPITER_API_KEY Backyard uses the keyless Jupiter endpoint.
func optionalCredential(name string) string {
	value, err := engine.Credential(name)
	if err != nil {
		return ""
	}
	return value
}

// backyardSelectorMode reads BACKYARD_RWA_SELECTOR_LIVE, 0 or 1.
func backyardSelectorMode() (backyard.SelectorMode, error) {
	switch os.Getenv("BACKYARD_RWA_SELECTOR_LIVE") {
	case "", "0":
		return backyard.SelectorOff, nil
	case "1":
		return backyard.SelectorLive, nil
	}
	return "", errors.New("BACKYARD_RWA_SELECTOR_LIVE must be 0 or 1")
}

const backyardUsage = `usage: loyal-engine backyard [in-flight | simulate-step --lane <lane> --dir up|down --amount-raw <n> | selector-evaluate [--execute] | clear-hold --reason "<text>" | exit-lane --lane <lane> (--by <operator> [--equity-zero] | --clear) | settle-manual-restore --operation <operation id> --signature <signature> [--execute]]`

// runBackyardOperator is the one-shot operator surface of the Backyard
// family. It reads the same BACKYARD_* credentials as the engine.
func runBackyardOperator(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 1 && args[0] == "in-flight" {
		// The release gate (deploy/hetzner/activate-backyard.sh): prints the
		// number of operations a restart would resume. Needs only the DB.
		url, err := engine.Credential("BACKYARD_DATABASE_URL")
		if err != nil {
			return err
		}
		count, err := backyard.CountNonterminal(ctx, url, backyard.FixedRouteKey)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, count)
		return err
	}
	if len(args) > 0 && args[0] == "simulate-step" {
		// Read-only: builds one atomic leverage step from live chain state and
		// simulates it unsigned. Needs only the RPC (and Jupiter for a swap).
		lane, up, amount, err := parseSimulateStepFlags(args[1:])
		if err != nil {
			return err
		}
		url, err := engine.Credential("BACKYARD_SOLANA_RPC_URL")
		if err != nil {
			return err
		}
		return backyard.RunSimulateStep(ctx, out, url, backyardJupiter(optionalCredential("JUPITER_API_KEY")), lane, up, amount)
	}
	cfg, err := backyardRuntimeConfig()
	if err != nil {
		return err
	}
	encode := func(value any, err error) error {
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(value)
	}
	if len(args) == 0 {
		return errors.New(backyardUsage)
	}
	switch command, rest := args[0], args[1:]; {
	case command == "selector-evaluate" && (len(rest) == 0 || len(rest) == 1 && rest[0] == "--execute"):
		// --execute performs exactly one locked evaluation under its own
		// short route lease; the committed entry is durable route state.
		return backyard.RunSelectorEvaluate(ctx, out, cfg, len(rest) == 1)
	case command == "clear-hold":
		// The only operator path that lifts a durable manual recovery stop.
		if len(rest) != 2 || rest[0] != "--reason" || rest[1] == "" {
			return errors.New(`usage: loyal-engine backyard clear-hold --reason "<text>"`)
		}
		result, err := backyard.ClearManualRecoveryHold(ctx, cfg.DatabaseURL, backyard.FixedRouteKey, rest[1])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, result)
		return err
	case command == "exit-lane":
		// The operator exit row: the lane's target debt is 0 (and with
		// --equity-zero its equity too). The worker delevers it by atomic
		// steps on its next ticks; --clear removes the row.
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
	case command == "settle-manual-restore":
		request, execute, err := parseSettleManualRestoreFlags(rest)
		if err != nil {
			return err
		}
		result, err := backyard.RunManualRestoreReprocess(ctx, cfg.DatabaseURL, cfg.RPCURL, request, execute)
		if err != nil {
			_ = json.NewEncoder(out).Encode(result)
			return err
		}
		return encode(result, nil)
	}
	return errors.New(backyardUsage)
}

const simulateStepUsage = `usage: loyal-engine backyard simulate-step --lane <lane> --dir up|down --amount-raw <n>`

// parseSimulateStepFlags is syntactic only; the lane and amount are checked
// where the step is built.
func parseSimulateStepFlags(args []string) (lane string, up bool, amount uint64, err error) {
	dir := ""
	for index := 0; index < len(args); index++ {
		if index+1 >= len(args) {
			return "", false, 0, fmt.Errorf("simulate-step: %s requires a value\n%s", args[index], simulateStepUsage)
		}
		value := args[index+1]
		switch args[index] {
		case "--lane":
			lane = value
		case "--dir":
			dir = value
		case "--amount-raw":
			if amount, err = strconv.ParseUint(value, 10, 64); err != nil {
				return "", false, 0, fmt.Errorf("simulate-step: --amount-raw requires an integer\n%s", simulateStepUsage)
			}
		default:
			return "", false, 0, fmt.Errorf("simulate-step: unexpected argument %q\n%s", args[index], simulateStepUsage)
		}
		index++
	}
	if lane == "" || (dir != "up" && dir != "down") || amount == 0 {
		return "", false, 0, fmt.Errorf("simulate-step: --lane, --dir up|down and a positive --amount-raw are required\n%s", simulateStepUsage)
	}
	return lane, dir == "up", amount, nil
}

// parseSettleManualRestoreFlags is syntactic only; every authority check
// stays with the settlement package.
func parseSettleManualRestoreFlags(args []string) (backyard.ManualRestoreReprocessRequest, bool, error) {
	request, execute := backyard.ManualRestoreReprocessRequest{}, false
	usage := `usage: loyal-engine backyard settle-manual-restore --operation <operation id> --signature <signature> [--execute]`
	for index := 0; index < len(args); index++ {
		var target *string
		switch args[index] {
		case "--execute":
			execute = true
			continue
		case "--operation":
			target = &request.OperationID
		case "--signature":
			target = &request.Signature
		default:
			return request, execute, fmt.Errorf("settle-manual-restore: unexpected argument %q\n%s", args[index], usage)
		}
		if index+1 >= len(args) {
			return request, execute, fmt.Errorf("settle-manual-restore: %s requires a value\n%s", args[index], usage)
		}
		*target = args[index+1]
		index++
	}
	if request.OperationID == "" || request.Signature == "" {
		return request, execute, fmt.Errorf("settle-manual-restore: --operation and --signature are required\n%s", usage)
	}
	return request, execute, nil
}

const exitLaneUsage = `usage: loyal-engine backyard exit-lane --lane <lane> (--by <operator> [--equity-zero] | --clear)`

// parseExitLaneFlags is syntactic only; the lane is checked where the row is
// written.
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
		case "--lane", "--by":
			if index+1 >= len(args) {
				return request, fmt.Errorf("exit-lane: %s requires a value\n%s", args[index], exitLaneUsage)
			}
			if args[index] == "--lane" {
				request.Lane = args[index+1]
			} else {
				request.RequestedBy = args[index+1]
			}
			index++
		default:
			return request, fmt.Errorf("exit-lane: unexpected argument %q\n%s", args[index], exitLaneUsage)
		}
	}
	if request.Lane == "" || request.Clear == (request.RequestedBy != "") || (request.Clear && request.EquityZero) {
		return request, fmt.Errorf("exit-lane: --lane and either --by or --clear are required\n%s", exitLaneUsage)
	}
	return request, nil
}
