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

type SelectorMode string

const (
	SelectorOff  SelectorMode = ""
	SelectorLive SelectorMode = "live"
)

type EngineConfig struct {
	Database    *Database
	RPC         *chain.Client
	View        *View
	Credentials Credentials
	Config      Config
	Owner       string
	Out         io.Writer
	Logger      *slog.Logger
	Facts       *engine.Facts
	Selector    SelectorMode
	Jupiter     *jupiter.Client
}

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
	exits    map[string]*exitQuoter
}

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
	routeEvents, routeExits := map[string]*events{}, map[string]*exitQuoter{}
	for _, v := range vaults {
		state := &vaultState{events: newEvents(config.Logger, config.Facts, v.Name)}
		worker, err := NewWorker(config.Database, config.RPC, config.View, config.Jupiter, config.Config, config.Credentials, v, state)
		if err != nil {
			return nil, err
		}
		e.workers, e.states = append(e.workers, worker), append(e.states, state)
		routeEvents[v.RouteKey] = state.events
		routeExits[v.RouteKey] = &exitQuoter{
			price: func(ctx context.Context, b heldExitBasis, slot int64, at time.Time) (ExitCost, error) {
				pricer, err := newLanePricer(ctx, config.RPC, config.View, config.Jupiter, v.manifest, slot)
				if err != nil {
					return ExitCost{}, err
				}
				return pricer.priceHeldExit(ctx, b, slot, at)
			},
			record: config.Database.RecordExitCost,
			now:    func() time.Time { return time.Now().UTC() },
			events: state.events,
		}
	}
	if config.Jupiter == nil {
		return nil, fmt.Errorf("Backyard engine requires a Jupiter client")
	}
	config.Database.events, config.Database.exits, e.exits = routeEvents, routeExits, routeExits
	return e, nil
}

func (e *Engine) quoteStartupExits(ctx context.Context) {
	for i, worker := range e.workers {
		v, quoter := worker.manifest.vault, e.exits[worker.routeKey]
		if quoter == nil {
			continue
		}
		quoteCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		refreshObservationLagSlots(quoteCtx, e.rpc)
		planning, err := e.database.readRoutePlanningState(quoteCtx, v.RouteKey)
		if err == nil {
			_, _, err = observeQuotingRouteSnapshot(quoteCtx, e.rpc, e.view, worker.manifest, e.states[i], planning.landedSlot, planning.inputs, quoter)
		}
		cancel()
		if err != nil {
			_, _ = fmt.Fprintf(e.out, "backyard-rwa-worker: vault=%s startup exit quotes unavailable: %s\n", v.Name, sanitizedSelectorEvaluateFailure(err))
		}
	}
}

func (e *Engine) Run(ctx context.Context) error {
	if e == nil || len(e.workers) == 0 {
		return fmt.Errorf("Backyard engine is not constructed")
	}
	e.quoteStartupExits(ctx)
	if e.runtime.Selector != SelectorOff {
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

func (e *Engine) runSelector(ctx context.Context, feed *EconomicFeed) func() {
	out := e.out
	feedCtx, cancelFeed := context.WithCancel(ctx)
	feedDone := make(chan struct{})
	lastUnavailable := ""
	samplers := make([]*selectorSampler, len(e.workers))
	for i, worker := range e.workers {
		samplers[i] = newSelectorSampler(e, worker, e.states[i])
	}
	go func() {
		defer close(feedDone)
		runSelectorSamples(feedCtx, selectorLiveSampleInterval, func(ctx context.Context) {
			_ = feed.Refresh(ctx)
			markets, _ := feed.Snapshot()
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

func newSelectorSampler(e *Engine, worker *Worker, state *vaultState) *selectorSampler {
	return &selectorSampler{engine: e, worker: worker, state: state, identity: newProgramIdentityWatcher(viewProgramIdentity(e.view)).observe,
		levWatch: &leverageWatch{}, levDecisionLog: &leverageDecisionLog{}, apyThrottle: &currentAPYThrottle{}}
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
	for _, line := range p.levWatch.observe(markets, result.SourceLane, result.EquityRaw, time.Since(p.levWatchSummary) >= time.Hour, worker.manifest.vault.AdminPerformanceFeeBPS, &observed.Snapshot) {
		_, _ = fmt.Fprintln(e.out, line)
	}
	if time.Since(p.levWatchSummary) >= time.Hour {
		p.levWatchSummary = time.Now()
		publishLeverageWatch(ctx, time.Now(), p.levWatch, observed, func(ctx context.Context, summary LeverageWatchSummary) error {
			return database.RecordLeverageWatch(ctx, routeKey, summary)
		}, logf)
	}
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
	for _, source := range observed.lanes {
		if lane, level, ok := decideOverflowLevel(source, markets, DefaultSelectorPolicy(), time.Now().UTC()); ok && observed.planning != nil && observed.planning.inputs.Levels[lane] != level {
			if err := database.RecordLaneLevel(ctx, routeKey, lane, level); err != nil {
				printf("overflow lane level not stored: %s\n", sanitizedSelectorEvaluateFailure(err))
			}
		}
	}
	publishCurrentAPY(ctx, time.Now(), p.apyThrottle, observed, markets, func(ctx context.Context, value CurrentAPY) error {
		return database.RecordCurrentAPY(ctx, routeKey, value)
	}, logf)
	if result.Action.entersOrRotates() {
		worker.notifySelectorCommit(result.Action)
		printf("selector action=%s source=%s destination=%s\n", result.Action, result.SourceLane, result.DestinationLane)
		p.lastSampleKey = ""
		return
	}
	key, line := selectorSampleLine(result)
	if key != p.lastSampleKey || time.Since(p.lastSamplePrint) >= time.Hour {
		p.lastSampleKey, p.lastSamplePrint = key, time.Now()
		_, _ = fmt.Fprintln(e.out, line)
	}
}
