package main

import (
	"bytes"
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
	if selector != backyard.SelectorOff {
		if cfg.TimescaleURL, err = engine.Credential("BACKYARD_TIMESCALE_DATABASE_URL"); err != nil {
			return err
		}
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
		Selector: selector, TimescaleURL: cfg.TimescaleURL,
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

const backyardUsage = `usage: loyal-engine backyard [in-flight | selector-evaluate [--execute] | clear-hold --reason "<text>" | commit-unwind-intent --lane <lane> --reason <reason> --observation-id <id> --max-collateral-raw <n> --max-debt-raw <n> --evidence-id <sha256> [--confirmation-file <json>] [--execute] | settle-manual-restore --operation <operation id> --signature <signature> [--execute]]`

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
		cfg.TimescaleURL = optionalCredential("BACKYARD_TIMESCALE_DATABASE_URL")
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
	case command == "commit-unwind-intent":
		request, execute, err := parseUnwindIntentFlags(rest)
		if err != nil {
			return err
		}
		// Dry-run by default; --execute takes a short route lease. A release
		// failure after a durable commit still prints the committed intent.
		result, err := backyard.RunUnwindIntentCommit(ctx, cfg.DatabaseURL, request, execute)
		if err != nil && errors.Is(err, backyard.ErrUnwindIntentCommittedReleaseUnconfirmed) {
			_ = json.NewEncoder(out).Encode(result)
		}
		return encode(result, err)
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

// parseUnwindIntentFlags is syntactic only; value shape and lane authority
// stay with the shared unwind-intent validation.
func parseUnwindIntentFlags(args []string) (backyard.UnwindIntentCommitRequest, bool, error) {
	request, execute := backyard.UnwindIntentCommitRequest{}, false
	usage := `usage: loyal-engine backyard commit-unwind-intent --lane <lane> --reason <economic_rotation|withdrawal_shortfall|hard_ltv_reduction> --observation-id <id> --max-collateral-raw <n> --max-debt-raw <n> --evidence-id <sha256> [--confirmation-file <json>] [--execute]`
	texts := map[string]*string{"--lane": &request.Lane, "--reason": &request.Reason, "--observation-id": &request.ObservationID, "--evidence-id": &request.EvidenceID}
	numbers := map[string]*int64{"--max-collateral-raw": &request.MaxCollateralRaw, "--max-debt-raw": &request.MaxDebtRaw}
	seen := make(map[string]bool)
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if seen[arg] {
			return request, execute, fmt.Errorf("commit-unwind-intent: duplicate argument %q", arg)
		}
		seen[arg] = true
		if arg == "--execute" {
			execute = true
			continue
		}
		if index+1 >= len(args) {
			return request, execute, fmt.Errorf("commit-unwind-intent: %s requires a value\n%s", arg, usage)
		}
		if arg == "--confirmation-file" {
			confirmation, err := readDebtClearConfirmation(args[index+1])
			if err != nil {
				return request, execute, err
			}
			request.Confirmation = confirmation
		} else if target, ok := numbers[arg]; ok {
			value, err := strconv.ParseInt(args[index+1], 10, 64)
			if err != nil {
				return request, execute, fmt.Errorf("commit-unwind-intent: %s requires an integer\n%s", arg, usage)
			}
			*target = value
		} else if target, ok := texts[arg]; ok {
			*target = args[index+1]
		} else {
			return request, execute, fmt.Errorf("commit-unwind-intent: unexpected argument %q\n%s", arg, usage)
		}
		index++
	}
	if request.Lane == "" || request.Reason == "" || request.ObservationID == "" || request.EvidenceID == "" {
		return request, execute, fmt.Errorf("commit-unwind-intent: --lane, --reason, --observation-id and --evidence-id are required\n%s", usage)
	}
	if execute && request.Confirmation == nil {
		return request, execute, errors.New("commit-unwind-intent: --execute requires --confirmation-file acknowledging full debt repayment")
	}
	return request, execute, nil
}

// The file records an explicit privileged-operator attestation, not proof of
// human identity. The domain layer validates its bounds before any DB mutation.
func readDebtClearConfirmation(path string) (*backyard.DebtClearConfirmation, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("commit-unwind-intent: cannot read confirmation file")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(data) > 4096 {
		return nil, errors.New("commit-unwind-intent: confirmation file exceeds 4096 bytes or cannot be read")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var confirmation *backyard.DebtClearConfirmation
	if err := decoder.Decode(&confirmation); err != nil || confirmation == nil {
		return nil, errors.New("commit-unwind-intent: invalid confirmation JSON")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, errors.New("commit-unwind-intent: confirmation file must contain exactly one JSON object")
	}
	return confirmation, nil
}
