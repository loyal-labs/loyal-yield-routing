package backyard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

type selectorEvaluateFeed interface {
	Refresh(context.Context) error
	Snapshot() ([]LaneEconomics, string)
}

type selectorEvaluateDeps struct {
	manifest    RouteManifest
	databaseURL string
	newFeed     func(context.Context, RouteManifest) (selectorEvaluateFeed, error)
	observe     func(context.Context, *Database, RouteManifest) (Observation, error)
	evaluate    func(context.Context, *Database, []LaneEconomics) (SelectorResult, error)
}

func RunSelectorEvaluate(ctx context.Context, out io.Writer, config RuntimeConfig, vault string, execute bool) error {
	if config.RPCURL == "" || config.DatabaseURL == "" {
		return fmt.Errorf("BACKYARD_SOLANA_RPC_URL and BACKYARD_DATABASE_URL are required for selector evaluation")
	}
	v, err := findVault(vault)
	if err != nil {
		return err
	}
	manifest, state := v.manifest, &vaultState{}
	rpc, err := chain.New(config.RPCURL, 15*time.Second)
	if err != nil {
		return budgetHoldFrom(err, "selector_evaluate_rpc_unavailable")
	}
	view, err := OpenView(ctx, rpc, nil)
	if err != nil {
		return budgetHoldFrom(err, "selector_evaluate_rpc_unavailable")
	}
	return runSelectorEvaluate(ctx, out, execute, selectorEvaluateDeps{
		manifest:    manifest,
		databaseURL: config.DatabaseURL,
		newFeed: func(ctx context.Context, manifest RouteManifest) (selectorEvaluateFeed, error) {
			return NewEconomicFeed(view)
		},
		observe: func(ctx context.Context, db *Database, manifest RouteManifest) (Observation, error) {
			return observeSelectorShadow(ctx, db, rpc, view, manifest, state, newProgramIdentityWatcher(viewProgramIdentity(view)).observe)
		},
		evaluate: func(ctx context.Context, db *Database, markets []LaneEconomics) (SelectorResult, error) {
			return db.evaluateSelector(ctx, rpc, view, config.Jupiter, manifest, state, markets, newProgramIdentityWatcher(viewProgramIdentity(view)).observe, DefaultSelectorPolicy())
		},
	})
}

func runSelectorEvaluate(ctx context.Context, out io.Writer, execute bool, deps selectorEvaluateDeps) error {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	request, requestErr := readPilotCanaryEntryRequest(time.Now().UTC())
	if execute && requestErr != nil {
		return requestErr
	}
	if execute && request == nil {
		return budgetHold("selector_evaluate_requires_canary_request")
	}
	if !execute {
		return selectorEvaluateDryRun(ctx, out, deps)
	}
	return selectorEvaluateExecute(ctx, out, deps)
}

func selectorEvaluateDryRun(ctx context.Context, out io.Writer, deps selectorEvaluateDeps) error {
	cfg, err := pgxpool.ParseConfig(deps.databaseURL)
	if err != nil {
		return budgetHoldFrom(err, "selector_evaluate_database_configuration_invalid")
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "5000"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	database := &Database{pool: pool}
	if err != nil {
		return budgetHoldFrom(err, "selector_evaluate_journal_unavailable")
	}
	defer database.Close()
	feed, err := deps.newFeed(ctx, deps.manifest)
	if err != nil {
		return budgetHoldFrom(err, "selector_evaluate_feed_unavailable")
	}
	feedErr := feed.Refresh(ctx)
	observation, err := deps.observe(ctx, database, deps.manifest)
	if err != nil {
		return budgetHoldFrom(err, "selector_evaluate_observation_unavailable")
	}
	markets, _ := feed.Snapshot()
	selection := SelectOpportunity(SelectorInput{Now: time.Now().UTC(), Snapshot: observation.Snapshot, Lanes: observation.lanes, Markets: markets, Policy: DefaultSelectorPolicy()}, SelectorState{})
	request, requestErr := readPilotCanaryEntryRequest(time.Now().UTC())
	report := struct {
		Mode                string         `json:"mode"`
		Note                string         `json:"note"`
		CanaryRequest       any            `json:"canaryRequest"`
		CanaryRequestError  string         `json:"canaryRequestError,omitempty"`
		FeedFailure         string         `json:"feedFailure,omitempty"`
		NextLifecycleAction Decision       `json:"nextLifecycleAction"`
		Selection           SelectorResult `json:"selection"`
	}{"no_quote_diagnostic", "ranking diagnostic only: no quotes are collected, nothing is executable, not an executable canary preflight", nil, "", "", Decide(observation.Snapshot), selection}
	if requestErr != nil {
		report.CanaryRequestError = invalidPilotCanaryRequest
	} else if request != nil {
		report.CanaryRequest = *request
	}
	if feedErr != nil {
		report.FeedFailure = "selector_economic_feed_refresh_unavailable"
	}
	return json.NewEncoder(out).Encode(report)
}

const (
	invalidPilotCanaryRequest      = "invalid_pilot_canary_request"
	invalidSelectorRouteState      = "invalid_selector_route_state"
	pilotCanaryHistoryFull         = "pilot_canary_history_full"
	pilotCanaryRequestIDReused     = "pilot_canary_request_id_reused"
	selectorEvaluationNotCurrent   = "selector_evaluation_not_current"
	selectorFeeEvidenceUnavailable = "selector_fee_evidence_unavailable"
)

var selectorEvaluateAdmissionHoldCodes = map[string]bool{
	duplicateMoveQuote:             true,
	invalidPilotCanaryRequest:      true,
	invalidSelectorRouteState:      true,
	pilotCanaryHistoryFull:         true,
	pilotCanaryRequestIDReused:     true,
	selectorEvaluationNotCurrent:   true,
	selectorFeeEvidenceUnavailable: true,
}

const sqlstateQueryCanceled = "57014"

func sanitizedSelectorEvaluateFailure(err error) string {
	var hold *PreSendHold
	if errors.As(err, &hold) {
		if selectorEvaluateAdmissionHoldCodes[holdCause(hold)] {
			return holdCause(hold)
		}
		return "selector_evaluate_unavailable"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "selector_evaluate_timeout"
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case sqlstateQueryCanceled:
			return "selector_evaluate_query_cancelled"
		}
		return "selector_evaluate_unavailable"
	}
	return "selector_evaluate_unavailable"
}

func selectorEvaluateExecute(ctx context.Context, out io.Writer, deps selectorEvaluateDeps) error {
	database, err := openOperatorWriter(ctx, deps.databaseURL)
	if errors.Is(err, engine.ErrFamilyHeld) {
		return budgetHoldFrom(err, "selector_evaluate_writer_lock_unavailable")
	}
	if err != nil {
		return budgetHoldFrom(err, "selector_evaluate_database_unavailable")
	}
	defer database.Close()
	feed, err := deps.newFeed(ctx, deps.manifest)
	if err != nil {
		return budgetHoldFrom(err, "selector_evaluate_feed_unavailable")
	}
	attemptStart := time.Now().UTC()
	feedErr := feed.Refresh(ctx)
	markets, _ := feed.Snapshot()
	result, evalErr := deps.evaluate(ctx, database, markets)
	if evalErr != nil {
		feedFailure := ""
		if feedErr != nil {
			feedFailure = "selector_economic_feed_refresh_unavailable"
		}
		_ = json.NewEncoder(out).Encode(struct {
			Mode             string  `json:"mode"`
			AttemptSeconds   float64 `json:"attemptSeconds"`
			FeedFailure      string  `json:"feedFailure,omitempty"`
			EvaluatorFailure string  `json:"evaluatorFailure"`
		}{"locked_execute_failure", time.Since(attemptStart).Seconds(), feedFailure, sanitizedSelectorEvaluateFailure(evalErr)})
		return budgetHold("selector_evaluate_unavailable")
	}
	report := struct {
		Mode        string         `json:"mode"`
		FeedFailure string         `json:"feedFailure,omitempty"`
		Result      SelectorResult `json:"result"`
	}{"locked_execute", "", result}
	if feedErr != nil {
		report.FeedFailure = "selector_economic_feed_refresh_unavailable"
	}
	if _, err = fmt.Fprintf(out, "backyard-rwa-worker: selector action=%s source=%s destination=%s\n", result.Action, result.SourceLane, result.DestinationLane); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(report)
}
