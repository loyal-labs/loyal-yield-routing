package backyardrwa

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrSelectorEvaluationCommittedReleaseUnconfirmed reports that the locked
// evaluation committed a selector action (ENTER/CANARY_ENTER/SWITCH) and only
// the command's own lease release could not be confirmed. The report with the
// committed action is already on the output; never treat the action as unsent.
var ErrSelectorEvaluationCommittedReleaseUnconfirmed = errors.New("selector_evaluate_committed_lease_release_unconfirmed")

// selectorEvaluateFeed is the feed surface the command needs. The production
// EconomicFeed satisfies it unchanged; tests substitute a stub.
type selectorEvaluateFeed interface {
	Refresh(context.Context) error
	Snapshot() ([]LaneEconomics, string)
	Close()
}

// selectorEvaluateDeps carries the command's only injection points. Production
// wires the real constructors over the embedded manifest; tests wire the same
// reviewed-manifest fixtures and the real locked persistence. There is no
// second command mode behind these seams — both dry-run and execute use the
// SAME manifest inventory, lane predicates and evaluation.
type selectorEvaluateDeps struct {
	manifest    RouteManifest
	databaseURL string
	newFeed     func(context.Context, RouteManifest) (selectorEvaluateFeed, error)
	observe     func(context.Context, *Database, RouteManifest) (Observation, error)
	evaluate    func(context.Context, *Database, []LaneEconomics) (SelectorResult, error)
}

// RunSelectorEvaluate is the one-shot entry seam. The production selector
// evaluation (evaluateSelector) has exactly one caller in Run — the continuous
// live sample loop, which additionally requires the TimescaleDB economic
// feed. This command performs the SAME evaluation once:
//
//   - default (dry-run): the read-only shadow path — production readers, no
//     lease, no database writes (the SQL connection itself rejects writes) —
//     over the SAME manifest inventory and lane predicates as execute, plus an
//     explicit canary-request report. It never consumes the request, collects
//     no quotes, and is a ranking diagnostic only, never an executable canary
//     preflight.
//   - --execute: feed refresh, one snapshot, one locked evaluateSelector call
//     under its own short route lease (the locked persistence filters every
//     write on the lease owner/fencing token), released before exit.
//
// The canary request stays the operator authorization either way: expiring,
// single-use, bounded, and validated through the same manifest both here and
// inside the evaluation.
func RunSelectorEvaluate(ctx context.Context, out io.Writer, execute bool) error {
	config := RuntimeConfigFromEnvironment()
	if config.RPCURL == "" || config.DatabaseURL == "" {
		return fmt.Errorf("SOLANA_RPC_URL and NEON_DATABASE_URL are required for selector evaluation")
	}
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return err
	}
	rpc, err := NewRPCClient(config.RPCURL)
	if err != nil {
		return budgetHold("selector_evaluate_rpc_unavailable")
	}
	return runSelectorEvaluate(ctx, out, execute, selectorEvaluateDeps{
		manifest:    manifest,
		databaseURL: config.DatabaseURL,
		newFeed: func(ctx context.Context, manifest RouteManifest) (selectorEvaluateFeed, error) {
			return NewEconomicFeedOnManifest(ctx, os.Getenv("TIMESCALEDB_URL"), manifest)
		},
		observe: func(ctx context.Context, db *Database, manifest RouteManifest) (Observation, error) {
			return observeSelectorShadow(ctx, db, rpc, manifest, newProgramIdentityWatcher(rpc).observe)
		},
		evaluate: func(ctx context.Context, db *Database, markets []LaneEconomics) (SelectorResult, error) {
			return db.evaluateSelector(ctx, rpc, manifest, markets, newProgramIdentityWatcher(rpc).observe, DefaultSelectorPolicy())
		},
	})
}

// runSelectorEvaluate is bounded end to end (a stalled feed cannot hold the
// route lease past its TTL or hang the command), refuses an execute run
// without a present valid canary request before anything connects, and maps
// every stage failure to a fixed sanitized error: raw feed, RPC, SQL and
// configuration errors can carry credential paths or DSNs.
func runSelectorEvaluate(ctx context.Context, out io.Writer, execute bool, deps selectorEvaluateDeps) error {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	request, requestErr := readPilotCanaryEntryRequestOnManifest(time.Now().UTC(), deps.manifest)
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

// selectorEvaluateDryRun mirrors the read-only shadow path with the SAME
// manifest inventory and lane predicates as the locked execute path, so a
// reviewed candidate appears in diagnostics instead of being silently dropped
// by installed-only closures. No quotes are collected and nothing is
// executable: this is a diagnostic, never an executable canary preflight.
func selectorEvaluateDryRun(ctx context.Context, out io.Writer, deps selectorEvaluateDeps) error {
	cfg, err := pgxpool.ParseConfig(deps.databaseURL)
	if err != nil {
		return budgetHold("selector_evaluate_database_configuration_invalid")
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "5000"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	database := &Database{pool: pool}
	if err != nil {
		return budgetHold("selector_evaluate_journal_unavailable")
	}
	defer database.Close()
	feed, err := deps.newFeed(ctx, deps.manifest)
	if err != nil {
		return budgetHold("selector_evaluate_feed_unavailable")
	}
	defer feed.Close()
	feedErr := feed.Refresh(ctx)
	observation, err := deps.observe(ctx, database, deps.manifest)
	if err != nil {
		return budgetHold("selector_evaluate_observation_unavailable")
	}
	markets, _ := feed.Snapshot()
	laneAllowed := func(lane string) bool { return selectorDestinationLaneAuthorized(deps.manifest, lane) }
	fundingAllowed := func(lane string) bool { return deps.manifest.selectorEntryFundingLane(lane, false) }
	selection := selectOpportunityWithLanes(SelectorInput{Now: time.Now().UTC(), Snapshot: observation.Snapshot, Markets: markets, Policy: DefaultSelectorPolicy()}, SelectorState{}, laneAllowed, fundingAllowed)
	request, requestErr := readPilotCanaryEntryRequestOnManifest(time.Now().UTC(), deps.manifest)
	report := struct {
		Mode                string         `json:"mode"`
		Note                string         `json:"note"`
		CanaryRequest       any            `json:"canaryRequest"`
		CanaryRequestError  string         `json:"canaryRequestError,omitempty"`
		FeedFailure         string         `json:"feedFailure,omitempty"`
		NextLifecycleAction Decision       `json:"nextLifecycleAction"`
		Selection           SelectorResult `json:"selection"`
	}{"no_quote_diagnostic", "ranking diagnostic only: no quotes are collected, nothing is executable, not an executable canary preflight", nil, "", "", deps.manifest.DecideOnManifest(observation.Snapshot), selection}
	if requestErr != nil {
		report.CanaryRequestError = "invalid_pilot_canary_request"
	} else if request != nil {
		report.CanaryRequest = *request
	}
	if feedErr != nil {
		report.FeedFailure = "selector_economic_feed_refresh_unavailable"
	}
	return json.NewEncoder(out).Encode(report)
}

// selectorEvaluateAdmissionHoldCodes is the closed set of BudgetHold reasons
// the locked evaluation (selector_entry.go, pilot_canary_entry.go) can return
// to this command. Membership is the only thing the failure diagnostic echoes;
// any other reason collapses to the generic code.
var selectorEvaluateAdmissionHoldCodes = map[string]bool{
	"duplicate_move_quote":                      true,
	"invalid_pilot_canary_request":              true,
	"invalid_selector_route_state":              true,
	"pilot_canary_history_full":                 true,
	"pilot_canary_request_id_reused":            true,
	"selector_entry_allocation_bind_failed":     true,
	"selector_entry_allocation_not_bound":       true,
	"selector_entry_already_allocated":          true,
	"selector_entry_authority_mismatch":         true,
	"selector_entry_borrow_fee_exceeded":        true,
	"selector_entry_borrow_mismatch":            true,
	"selector_entry_borrow_unavailable":         true,
	"selector_entry_cash_changed":               true,
	"selector_entry_has_outstanding_exit":       true,
	"selector_entry_lane_deferred":              true,
	"selector_entry_quote_expired":              true,
	"selector_entry_quote_missing":              true,
	"selector_entry_requires_reconciled_idle":   true,
	"selector_evaluation_not_current":           true,
	"selector_finish_current_work_first":        true,
	"selector_manual_recovery_active":           true,
	"selector_requires_active_pilot":            true,
	"selector_resolve_capital_recovery_first":   true,
	"selector_state_changed_during_quote":       true,
	"selector_unwind_quote_missing":             true,
	"unwind_requires_existing_exit_reservation": true,
}

// sanitizedSelectorEvaluateFailure maps one evaluator error to a single fixed
// diagnostic code. Raw SQL, network and configuration errors can carry DSNs or
// server messages, and a hold reason is echoed only when it is one of the
// locked admission's own codes — nothing arbitrary ever reaches the output.
func sanitizedSelectorEvaluateFailure(err error) string {
	var hold *BudgetHold
	if errors.As(err, &hold) {
		if selectorEvaluateAdmissionHoldCodes[hold.Reason] {
			return hold.Reason
		}
		return "selector_evaluate_unavailable"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "selector_evaluate_timeout"
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "55P03": // lock_not_available: the admission record lock timed out
			return "selector_evaluate_lock_timeout"
		case "57014": // query_canceled
			return "selector_evaluate_query_cancelled"
		}
		return "selector_evaluate_unavailable"
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// The lease-fenced state row vanished between the lease and the
		// version read: the route fence is unavailable.
		return "selector_evaluate_route_fence_unavailable"
	}
	return "selector_evaluate_unavailable"
}

// selectorEvaluateExecute is exactly one iteration of Run's live sample body
// (worker.go live-mode branch): feed refresh, one snapshot, one locked
// evaluation — plus the route lease that the worker process contributes in the
// continuous form and this command must contribute itself. The live loop's
// notifySelectorCommit only queues a wake on its own worker's tick channel;
// there is no tick loop here, and the committed entry is durable route state
// that the next worker start reads from the journal.
func selectorEvaluateExecute(ctx context.Context, out io.Writer, deps selectorEvaluateDeps) (err error) {
	database, err := OpenDatabase(ctx, deps.databaseURL)
	if err != nil {
		return budgetHold("selector_evaluate_database_unavailable")
	}
	defer database.Close()
	feed, err := deps.newFeed(ctx, deps.manifest)
	if err != nil {
		return budgetHold("selector_evaluate_feed_unavailable")
	}
	defer feed.Close()
	// Unique owner: AcquireRouteLease never treats an unexpired lease as
	// re-entrant even for identical owner text, and the release below clears
	// exactly this owner/fencing pair.
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return budgetHold("selector_evaluate_owner_unavailable")
	}
	if _, err = database.AcquireRouteLease(ctx, productionRouteKey, "selector-evaluate:"+hex.EncodeToString(nonce[:]), 45*time.Second); err != nil {
		return budgetHold("selector_evaluate_lease_unavailable")
	}
	committed := false
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		released, releaseErr := database.ReleaseRouteLease(releaseCtx)
		// The named result carries the already-printed report past this
		// cleanup: a committed selector action stays visible and is never
		// implied unsent.
		if err == nil && (releaseErr != nil || !released) {
			if committed {
				err = ErrSelectorEvaluationCommittedReleaseUnconfirmed
			} else {
				err = budgetHold("selector_evaluate_lease_release_unconfirmed")
			}
		}
	}()
	attemptStart := time.Now().UTC()
	feedErr := feed.Refresh(ctx)
	markets, _ := feed.Snapshot()
	result, evalErr := deps.evaluate(ctx, database, markets)
	if evalErr != nil {
		// One sanitized fixed-code line before the unchanged generic hold:
		// enough to name the failing admission guard or database class
		// without ever echoing the underlying error text. A report encode
		// failure must not alter the returned error.
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
	committed = result.Action == "ENTER" || result.Action == "CANARY_ENTER" || result.Action == "SWITCH"
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
