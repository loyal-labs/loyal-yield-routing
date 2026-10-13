package backyard

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

// SelectorMode chooses the economic selector collector beside the serialized
// transaction loop: off or live selector authority.
type SelectorMode string

const (
	SelectorOff  SelectorMode = ""
	SelectorLive SelectorMode = "live"
)

// EngineConfig is the explicit injected runtime for one separately credentialed
// Backyard engine instance. The loyal-engine command constructs it; Database,
// RPC, and signing credentials are all Backyard-scoped.
type EngineConfig struct {
	Database *Database
	RPC      *chain.Client
	// View is the LaserStream view of Backyard's accounts planning reads.
	View *View
	// Credentials is the Backyard delegated executor capability. It stays
	// inside the engine instance; observers and planners never receive it.
	Credentials Credentials
	Config      Config
	// Owner is the platform-neutral scope+instance+release identity, for logs.
	Owner string
	// Out receives the human-readable lifecycle lines; Logger the structured
	// events; Facts the family health facts. Logger and Facts may be nil.
	Out    io.Writer
	Logger *slog.Logger
	Facts  *engine.Facts
	// Selector and its economic feed run beside, never inside, the
	// transaction loop. Neither collector sends transactions.
	Selector SelectorMode
	// Jupiter is the swap/v1 client every quote goes through.
	Jupiter *jupiter.Client
}

// Engine is the concrete runtime adapter for process composition: one Worker
// per vault, each with its vault's in-process state, behind the family's one
// writer lock.
type Engine struct {
	workers  []*Worker
	states   []*vaultState
	database *Database
	rpc      *chain.Client
	view     *View
	owner    string
	config   Config
	out      io.Writer
	runtime  EngineConfig
}

// NewEngine validates the complete injected runtime or fails closed. A missing
// dependency or a capability that does not match the pinned delegated
// executor is a startup failure, not a degraded mode.
func NewEngine(config EngineConfig) (*Engine, error) {
	if config.Owner == "" {
		return nil, fmt.Errorf("Backyard engine requires an instance identity")
	}
	switch config.Selector {
	case SelectorOff, SelectorLive:
	default:
		return nil, fmt.Errorf("unknown Backyard selector mode")
	}
	if config.Out == nil {
		config.Out = io.Discard
	}
	e := &Engine{database: config.Database, rpc: config.RPC, view: config.View, owner: config.Owner, config: config.Config, out: config.Out, runtime: config}
	routeEvents := map[string]*events{}
	for _, v := range vaults {
		state := &vaultState{events: newEvents(config.Logger, config.Facts, v.Name)}
		worker, err := NewWorker(config.Database, config.RPC, config.View, config.Jupiter, config.Config, config.Credentials, v, state)
		if err != nil {
			return nil, err
		}
		e.workers, e.states = append(e.workers, worker), append(e.states, state)
		routeEvents[v.RouteKey] = state.events
	}
	if config.Jupiter == nil {
		return nil, fmt.Errorf("Backyard engine requires a Jupiter client")
	}
	config.Database.events = routeEvents
	return e, nil
}

// Run starts the selector collector and serializes lifecycle ticks. Its
// Database is the writer session that holds Backyard's one-writer lock. Cancellation surfaces as context.Canceled so the owning process runtime can
// join the lane without recording a failure.
func (e *Engine) Run(ctx context.Context) error {
	if e == nil || len(e.workers) == 0 {
		return fmt.Errorf("Backyard engine is not constructed")
	}
	if e.runtime.Selector != SelectorOff {
		// The feed observes every active registry lane.
		feed, err := NewEconomicFeed(e.view)
		if err != nil {
			return err
		}
		stop := e.runSelector(ctx, feed)
		defer stop()
	}
	lanes := make([]engine.Lane, len(e.workers))
	for i, worker := range e.workers {
		if _, err := fmt.Fprintf(e.out,
			"backyard-rwa-worker: starting serialized confirmed lifecycle vault=%s route=%s owner=%s manifest_sha256=%s\n",
			worker.manifest.vault.Name, worker.routeKey, e.owner, worker.manifest.SHA256,
		); err != nil {
			return err
		}
		e.states[i].events.workerStart(e.owner, worker.manifest.SHA256)
		lanes[i] = worker
	}
	return engine.Run(ctx, lanes...)
}

// runSelector collects economics off the transaction loop: one feed refresh
// per sample, then each vault's evaluation from it. Live acceptance is
// fenced against its pre-observation version and existing pilot. The
// returned stop joins the collector.
func (e *Engine) runSelector(ctx context.Context, feed *EconomicFeed) func() {
	out := e.out
	feedCtx, cancelFeed := context.WithCancel(ctx)
	feedDone := make(chan struct{})
	lastUnavailable := ""
	samplers := make([]*selectorSampler, len(e.workers))
	for i, worker := range e.workers {
		// Each vault samples concurrently, so each watches the program
		// identity itself.
		samplers[i] = &selectorSampler{engine: e, worker: worker, state: e.states[i], identity: newProgramIdentityWatcher(viewProgramIdentity(e.view)).observe,
			levWatch: &leverageWatch{}, levDecisionLog: &leverageDecisionLog{}, apyThrottle: &currentAPYThrottle{}}
	}
	// Economic collection stays off the transaction loop. Live acceptance
	// is fenced against its pre-observation version and existing pilot; the
	// collector sends no transactions.
	go func() {
		defer close(feedDone)
		runSelectorSamples(feedCtx, selectorLiveSampleInterval, func(ctx context.Context) {
			_ = feed.Refresh(ctx)
			markets, _ := feed.Snapshot()
			// A lane without economics (its realized yield unmeasurable) is
			// not scored; say which and why, once per change.
			if unavailable := feed.UnavailableLanes(); unavailable != lastUnavailable {
				lastUnavailable = unavailable
				if unavailable != "" {
					_, _ = fmt.Fprintf(out, "backyard-rwa-worker: selector economics unavailable %s\n", unavailable)
				}
			}
			var wg sync.WaitGroup
			for _, sampler := range samplers {
				wg.Add(1)
				go func() { defer wg.Done(); sampler.sample(ctx, feedCtx, markets) }()
			}
			wg.Wait()
		})
	}()
	return func() { cancelFeed(); <-feedDone }
}

// selectorSampler is one vault's selector sample: its evaluation from the
// sample's markets and what it prints, stores and wakes. Sample diagnostics
// print when their sanitized shape changes (the held sample also hourly), so
// neither a persistent outage nor a stable hold floods the log on the sample
// cadence.
type selectorSampler struct {
	engine   *Engine
	worker   *Worker
	state    *vaultState
	identity func(context.Context) (programIdentityObservation, error)

	lastSampleKey, lastEvaluateFailure string
	lastSamplePrint, levWatchSummary   time.Time
	levWatch                           *leverageWatch
	levDecisionLog                     *leverageDecisionLog
	apyThrottle                        *currentAPYThrottle
}

func (p *selectorSampler) sample(ctx, feedCtx context.Context, markets []LaneEconomics) {
	e, worker, events := p.engine, p.worker, p.state.events
	database, rpc, view, routeKey := e.database, e.rpc, e.view, worker.routeKey
	printf := func(format string, args ...any) {
		_, _ = fmt.Fprintf(e.out, "backyard-rwa-worker: vault=%s "+format, append([]any{worker.manifest.vault.Name}, args...)...)
	}
	logf := func(format string, args ...any) { _, _ = fmt.Fprintf(e.out, format, args...) }
	result, observed, err := database.evaluateSelectorObserved(ctx, rpc, view, e.runtime.Jupiter, worker.manifest, p.state, markets, p.identity, DefaultSelectorPolicy())
	if err != nil {
		// Closed-set sanitized code only: raw RPC/DB errors may carry
		// service URLs. Change-only keeps a persistent outage at one line.
		code := sanitizedSelectorEvaluateFailure(err)
		events.selectorSampleError(err)
		if code != p.lastEvaluateFailure {
			p.lastEvaluateFailure = code
			printf("selector sample unavailable (%s); retaining current authority\n", code)
			events.selectorUnavailable(code)
		}
		return
	}
	p.lastEvaluateFailure = ""
	// B2 watch-only: log lines, never a decision input.
	for _, line := range p.levWatch.observe(markets, result.SourceLane, result.EquityRaw, time.Since(p.levWatchSummary) >= time.Hour, worker.manifest.vault.AdminPerformanceFeeBPS, observed.Snapshot) {
		_, _ = fmt.Fprintln(e.out, line)
	}
	if time.Since(p.levWatchSummary) >= time.Hour {
		p.levWatchSummary = time.Now()
		// Display only: the same summary numbers, for the admin app.
		publishLeverageWatch(ctx, time.Now(), p.levWatch, observed, func(ctx context.Context, summary LeverageWatchSummary) error {
			return database.RecordLeverageWatch(ctx, routeKey, summary)
		}, logf)
	}
	// B2 option 1: each held lane's level, target()'s level input. It
	// changes no money by itself.
	for _, lane := range observed.lanes {
		decision, ok := decideLeverageTarget(lane, result, markets, DefaultSelectorPolicy(), productionLeverageStepPricer(feedCtx, rpc, view, e.runtime.Jupiter, worker.manifest, observed, lane))
		if !ok {
			continue
		}
		if p.levDecisionLog.due(time.Now(), decision) {
			_, _ = fmt.Fprintln(e.out, decision.logLine())
		}
		if decision.outage() {
			events.leverageDecisionOutage(decision.Reason)
		}
		if decision.storeChanges(lane) {
			if err := database.RecordLaneLevel(ctx, routeKey, decision.Lane, decision.level()); err != nil {
				printf("lane level not stored: %s\n", sanitizedSelectorEvaluateFailure(err))
			}
		}
	}
	// An overflow lane its source cannot lever into takes the live cap
	// before it holds anything.
	for _, source := range observed.lanes {
		if lane, level, ok := decideOverflowLevel(source, markets, DefaultSelectorPolicy(), time.Now().UTC()); ok && observed.planning != nil && observed.planning.inputs.Levels[lane] != level {
			if err := database.RecordLaneLevel(ctx, routeKey, lane, level); err != nil {
				printf("overflow lane level not stored: %s\n", sanitizedSelectorEvaluateFailure(err))
			}
		}
	}
	// Display only: the position's current net APY for the web app.
	publishCurrentAPY(ctx, time.Now(), p.apyThrottle, observed, markets, func(ctx context.Context, value CurrentAPY) error {
		return database.RecordCurrentAPY(ctx, routeKey, value)
	}, logf)
	if result.Action == "ENTER" || result.Action == "CANARY_ENTER" {
		worker.notifySelectorCommit(result.Action)
		printf("selector action=%s source=%s destination=%s\n", result.Action, result.SourceLane, result.DestinationLane)
		p.lastSampleKey = ""
		return
	}
	// Held-sample diagnostics list every candidate with its numbers; they
	// print when the sample's shape changes (action, reason, and each
	// candidate's reason, borrow state and leverage) and hourly, so a
	// reserve that opens shows the same tick.
	key, line := selectorSampleLine(result)
	if key != p.lastSampleKey || time.Since(p.lastSamplePrint) >= time.Hour {
		p.lastSampleKey, p.lastSamplePrint = key, time.Now()
		_, _ = fmt.Fprintln(e.out, line)
	}
}
