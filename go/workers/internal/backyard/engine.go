package backyard

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

// FixedRouteKey identifies the only authorized route this runtime composes.
const FixedRouteKey = productionRouteKey

// SelectorMode chooses the economic selector collector beside the serialized
// transaction loop: off, read-only shadow rankings, or live selector authority.
type SelectorMode string

const (
	SelectorOff    SelectorMode = ""
	SelectorShadow SelectorMode = "shadow"
	SelectorLive   SelectorMode = "live"
)

// EngineConfig is the explicit injected runtime for one separately credentialed
// Backyard engine instance. The loyal-engine command constructs it; Database,
// RPC, and signing credentials are all Backyard-scoped.
type EngineConfig struct {
	Database *Database
	RPC      *RPCClient
	// Credentials is the Backyard delegated executor capability. It stays
	// inside the engine instance; observers and planners never receive it.
	Credentials Credentials
	Config      Config
	// Owner is the platform-neutral scope+instance+release lease identity.
	Owner string
	// Out receives the human-readable lifecycle lines; Logger the structured
	// events; Facts the family health facts. Logger and Facts may be nil.
	Out    io.Writer
	Logger *slog.Logger
	Facts  *engine.Facts
	// Selector and its Timescale economic feed run beside, never inside, the
	// transaction loop. Neither collector sends transactions.
	Selector     SelectorMode
	TimescaleURL string
	// JupiterAPIKey selects the keyed Jupiter API; empty uses the keyless one.
	JupiterAPIKey string
}

// Engine is the concrete runtime adapter for process composition.
type Engine struct {
	worker   *Worker
	leases   routeLeaser
	database *Database
	rpc      *RPCClient
	owner    string
	config   Config
	out      io.Writer
	runtime  EngineConfig
}

// NewEngine validates the complete injected runtime or fails closed. A missing
// dependency, an unowned lease identity, or a capability that does not match
// the pinned delegated executor is a startup failure, not a degraded mode.
func NewEngine(config EngineConfig) (*Engine, error) {
	if !ValidLeaseOwner(config.Owner) {
		return nil, fmt.Errorf("Backyard engine requires a platform-neutral lease owner")
	}
	switch config.Selector {
	case SelectorOff:
	case SelectorShadow, SelectorLive:
		if config.TimescaleURL == "" {
			return nil, fmt.Errorf("Backyard selector requires its Timescale economic feed")
		}
	default:
		return nil, fmt.Errorf("unknown Backyard selector mode")
	}
	worker, err := NewWorker(config.Database, config.RPC, config.Config, config.Credentials)
	if err != nil {
		return nil, err
	}
	if config.Out == nil {
		config.Out = io.Discard
	}
	return &Engine{worker: worker, leases: config.Database, database: config.Database, rpc: config.RPC, owner: config.Owner, config: config.Config, out: config.Out, runtime: config}, nil
}

// Run starts the selector collector, acquires the route fence, serializes
// lifecycle ticks, and releases the exact fencing token on shutdown.
// Cancellation surfaces as context.Canceled so the owning process runtime can
// join the lane without recording a failure.
func (e *Engine) Run(ctx context.Context) error {
	if e == nil || e.worker == nil || e.leases == nil {
		return fmt.Errorf("Backyard engine is not constructed")
	}
	backyardEvents = newEvents(e.runtime.Logger, e.runtime.Facts)
	jupiterAPIKey = e.runtime.JupiterAPIKey
	if e.runtime.Selector != SelectorOff {
		// The feed's route inventory is scoped to the SAME reviewed manifest
		// the selector evaluates (worker.manifest): installed lanes plus the
		// candidate AUTO route only when the manifest carries a valid binding.
		feed, err := NewEconomicFeedOnManifest(ctx, e.runtime.TimescaleURL, e.worker.manifest)
		if err != nil {
			return err
		}
		stop := e.runSelector(ctx, feed, e.runtime.Selector == SelectorLive)
		defer stop()
	}
	if _, err := fmt.Fprintf(e.out,
		"backyard-rwa-worker: starting serialized confirmed lifecycle route=%s lease_owner=%s manifest_sha256=%s\n",
		e.worker.routeKey, e.owner, e.worker.manifest.SHA256,
	); err != nil {
		return err
	}
	backyardEvents.workerStart(e.owner, e.worker.manifest.SHA256)
	return e.worker.Run(ctx, e.leases, e.owner, e.config)
}

// runSelector collects economics off the transaction loop. Live acceptance is
// fenced against its pre-observation version and existing pilot; shadow
// records rankings only. The returned stop joins the collector.
func (e *Engine) runSelector(ctx context.Context, feed *EconomicFeed, live bool) func() {
	database, rpc, worker, out := e.database, e.rpc, e.worker, e.out
	feedCtx, cancelFeed := context.WithCancel(ctx)
	feedDone := make(chan struct{})
	shadowIdentity := newProgramIdentityWatcher(rpc).observe
	// Sample diagnostics are change-only: each line prints when its fixed
	// sanitized shape changes and stays silent while that shape persists,
	// so neither a persistent outage nor a stable hold floods the log on
	// the sample cadence.
	lastSampleAction, lastSampleReason, lastSampleCandidates := "", "", ""
	lastEvaluateFailure, lastShadowFailure := "", ""
	levWatch, levWatchSummary := &leverageWatch{}, time.Time{}
	levDecisionLog := &leverageDecisionLog{}
	apyThrottle := &currentAPYThrottle{}
	// Economic collection stays off the transaction loop. Live acceptance
	// is fenced against its pre-observation version and existing pilot;
	// shadow records rankings only. Neither collector sends transactions.
	go func() {
		defer close(feedDone)
		interval := time.Minute
		if live {
			interval = selectorLiveSampleInterval
		}
		runSelectorSamples(feedCtx, interval, func(ctx context.Context) {
			_ = feed.Refresh(ctx)
			if live {
				markets, _ := feed.Snapshot()
				result, observed, err := database.evaluateSelectorObserved(ctx, rpc, worker.manifest, markets, shadowIdentity, DefaultSelectorPolicy())
				if err != nil {
					// Closed-set sanitized code only: raw RPC/DB errors may
					// carry service URLs. Change-only keeps a persistent
					// outage at one line.
					code := sanitizedSelectorEvaluateFailure(err)
					if code != lastEvaluateFailure {
						lastEvaluateFailure = code
						_, _ = fmt.Fprintf(out, "backyard-rwa-worker: selector sample unavailable (%s); retaining current authority\n", code)
						backyardEvents.selectorUnavailable(code)
					}
					return
				}
				lastEvaluateFailure = ""
				// B2 watch-only: log lines, never a decision input.
				for _, line := range levWatch.observe(markets, result.SourceLane, result.EquityRaw, time.Since(levWatchSummary) >= time.Hour, func(lane string) bool { return worker.manifest.selectorEntryFundingLane(lane, false) }, observed.Snapshot) {
					_, _ = fmt.Fprintln(out, line)
				}
				if time.Since(levWatchSummary) >= time.Hour {
					levWatchSummary = time.Now()
					// Display only: the same summary numbers, for the admin app.
					publishLeverageWatch(ctx, time.Now(), levWatch, observed, func(ctx context.Context, summary LeverageWatchSummary, version int64) error {
						return database.RecordLeverageWatch(ctx, productionRouteKey, summary, version)
					}, func(format string, args ...any) { _, _ = fmt.Fprintf(out, format, args...) })
				}
				// B2 option 1: store the funded lane's level target. It
				// never runs beside a selector move, unwind or open
				// operation, and changes no money by itself.
				if decision, ok := decideLeverageTarget(observed.Snapshot, result, markets, DefaultSelectorPolicy()); ok && observed.planning != nil {
					if levDecisionLog.due(time.Now(), decision, observed.Snapshot.LeverageTargetLevel) {
						_, _ = fmt.Fprintln(out, decision.logLine())
					}
					if decision.changesTarget(observed.Snapshot.LeverageTargetLevel) || decision.BorrowRaw != observed.Snapshot.LeverageApprovedBorrowRaw || decision.SourceDebtRaw != observed.Snapshot.LeverageSourceDebtRaw || observed.Snapshot.LeverageBorrowOperationID != "" {
						target := LeverageTarget{Lane: decision.Lane, Level: decision.Next, SpreadBPS: decision.SpreadBPS, DecidedAt: time.Now().UTC(), BorrowRaw: decision.BorrowRaw, SourceDebtRaw: decision.SourceDebtRaw}
						if err := database.RecordLeverageTarget(ctx, productionRouteKey, target, observed.planning.generation); err != nil {
							_, _ = fmt.Fprintf(out, "backyard-rwa-worker: leverage target not stored: %s\n", sanitizedSelectorEvaluateFailure(err))
						}
					}
				}
				// Display only: the position's current net APY for the web app.
				publishCurrentAPY(ctx, time.Now(), apyThrottle, observed, markets, func(ctx context.Context, value CurrentAPY, version int64) error {
					return database.RecordCurrentAPY(ctx, productionRouteKey, value, version)
				}, func(format string, args ...any) { _, _ = fmt.Fprintf(out, format, args...) })
				if result.Action == "ENTER" || result.Action == "CANARY_ENTER" || result.Action == "SWITCH" {
					worker.notifySelectorCommit(result.Action)
					_, _ = fmt.Fprintf(out, "backyard-rwa-worker: selector action=%s source=%s destination=%s\n", result.Action, result.SourceLane, result.DestinationLane)
					lastSampleAction, lastSampleReason, lastSampleCandidates = "", "", ""
					return
				}
				// Change-only held-sample diagnostics: the action, its fixed
				// selector reason, and each considered candidate's lane and
				// blocked reason in pure-selector order, capped at four. A
				// candidate's blocked reason is the selector's own fixed
				// vocabulary — never a rate, evidence ID, or raw error — so
				// a newly deferred source or a silently missing candidate
				// lane is actually visible in the log.
				candidates := make([]string, 0, 4)
				for i, c := range result.Candidates {
					if i == 4 {
						break
					}
					candidates = append(candidates, c.Lane+":"+c.BlockedReason)
				}
				candidateDetail := strings.Join(candidates, ",")
				if result.Action != lastSampleAction || result.Reason != lastSampleReason || candidateDetail != lastSampleCandidates {
					lastSampleAction, lastSampleReason, lastSampleCandidates = result.Action, result.Reason, candidateDetail
					_, _ = fmt.Fprintf(out, "backyard-rwa-worker: selector sample action=%s reason=%s source=%s candidates=%s\n", result.Action, result.Reason, result.SourceLane, candidateDetail)
				}
				return
			}
			observation, err := observeSelectorShadow(ctx, database, rpc, worker.manifest, shadowIdentity)
			if err != nil {
				if code := sanitizedSelectorEvaluateFailure(err); code != lastShadowFailure {
					lastShadowFailure = code
					_, _ = fmt.Fprintf(out, "backyard-rwa-worker: selector shadow observation unavailable (%s)\n", code)
				}
				return
			}
			markets, _ := feed.Snapshot()
			// The shadow's selection resolves its lane authorities through
			// the SAME reviewed manifest as the feed above, so the funded
			// candidate lane is observed and ranked instead of silently
			// dropped by the installed-only closure.
			if _, err := database.RecordSelectorShadowOnManifest(ctx, worker.routeKey, worker.manifest, observation, markets); err != nil {
				if code := sanitizedSelectorEvaluateFailure(err); code != lastShadowFailure {
					lastShadowFailure = code
					_, _ = fmt.Fprintf(out, "backyard-rwa-worker: selector shadow record unavailable (%s)\n", code)
				}
				return
			}
			lastShadowFailure = ""
		})
	}()
	return func() { cancelFeed(); <-feedDone; feed.Close() }
}
