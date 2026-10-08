package worker

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/ata"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/config"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/earn"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/solanarpc"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/stream"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/subscription"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/watch"
)

const (
	kaminoVerificationFailureThreshold = 3
	// kaminoVerificationBatchInterval is Rust's DIRTY_VERIFICATION_BATCH_INTERVAL.
	kaminoVerificationBatchInterval = 100 * time.Millisecond
)

type Runtime struct {
	cfg           config.Config
	logger        *slog.Logger
	facts         *engine.Facts
	neon          *pgxpool.Pool
	timescale     *pgxpool.Pool
	rpc           *solanarpc.Client
	connector     stream.Connector
	watchLoader   *watch.Loader
	kaminoStore   *kamino.Store
	kaminoCatalog *kamino.CatalogClient
	kamino        *kamino.Handler
	ata           *ata.Handler
	earnStore     *earn.Store
	earn          *earn.Handler
	earnApp       *earn.Application
	apy           *earn.APYRefresher
	handler       *DurableHandler
}

func New(ctx context.Context, cfg config.Config, logger *slog.Logger, facts *engine.Facts) (*Runtime, error) {
	startup, cancelStartup := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStartup()
	rpc := solanarpc.New(cfg.SolanaRPCURL, 30*time.Second)
	if err := validateWatchNamespace(startup, cfg.Cluster, rpc); err != nil {
		return nil, err
	}
	neon, err := db.Open(startup, cfg.NeonDatabaseURL, 8)
	if err != nil {
		return nil, fmt.Errorf("connect Neon: %w", err)
	}
	timescale, err := db.Open(startup, cfg.TimescaleDatabaseURL, 8)
	if err != nil {
		neon.Close()
		return nil, fmt.Errorf("connect Timescale: %w", err)
	}
	kaminoStore := kamino.NewStore(timescale, "kamino")
	kaminoCatalog := kamino.NewCatalogClient(cfg.KaminoAPIBase, 30*time.Second)
	kaminoHandler := kamino.NewHandler(kaminoStore, rpc, logger, 400, false)
	ataHandler := ata.NewHandler(timescale, cfg.ATAStream, rpc)
	earnStore := earn.NewStore(neon)
	earnHandler := earn.NewHandler(earnStore, cfg.Cluster)
	delegate, err := solanago.PublicKeyFromBase58(cfg.EarnMaxDelegate)
	if err != nil {
		neon.Close()
		timescale.Close()
		return nil, errors.New("EARN_MAX_DELEGATE must be a Solana public key")
	}
	handler := &DurableHandler{Kamino: kaminoHandler, ATA: ataHandler, Earn: earnHandler, Facts: facts}
	earnApp, err := earn.NewApplication(startup, neon, rpc, cfg.Cluster, delegate, facts, logger, handler.streamAlive)
	if err != nil {
		neon.Close()
		timescale.Close()
		return nil, fmt.Errorf("start Earn application: %w", err)
	}
	handler.EarnApp = earnApp
	var apy *earn.APYRefresher
	if len(cfg.APYRiskProfiles) > 0 {
		var strategies []earn.APYStrategy
		for _, profile := range cfg.APYRiskProfiles {
			strategy, ok := earn.APYStrategyForRiskProfile(profile)
			if !ok {
				neon.Close()
				timescale.Close()
				return nil, fmt.Errorf("unsupported Earn APY risk profile %s", profile)
			}
			strategies = append(strategies, strategy)
		}
		apy = earn.NewAPYRefresher(timescale, neon, strategies)
	}
	runtime := &Runtime{cfg: cfg, logger: logger, facts: facts, neon: neon, timescale: timescale, rpc: rpc, connector: stream.GRPCConnector{Endpoint: cfg.LaserStreamEndpoint, APIKey: cfg.HeliusAPIKey}, watchLoader: watch.NewLoader(neon, cfg.Cluster), kaminoStore: kaminoStore, kaminoCatalog: kaminoCatalog, kamino: kaminoHandler, ata: ataHandler, earnStore: earnStore, earn: earnHandler, earnApp: earnApp, apy: apy, handler: handler}
	return runtime, nil
}

func (r *Runtime) Close() {
	if r.neon != nil {
		r.neon.Close()
	}
	if r.timescale != nil {
		r.timescale.Close()
	}
}

func (r *Runtime) Run(ctx context.Context) error {
	appCtx, stopApp := context.WithCancel(ctx)
	var appDone sync.WaitGroup
	appDone.Add(1)
	go func() { defer appDone.Done(); r.earnApp.RunConsumers(appCtx, r.cfg.ReconciliationWorkers) }()
	if r.apy != nil {
		appDone.Add(1)
		go func() { defer appDone.Done(); r.refreshEarnAPY(appCtx) }()
	}
	defer func() { stopApp(); appDone.Wait() }()
	startup, cancelStartup := context.WithTimeout(ctx, r.passTimeout())
	defer cancelStartup()
	currentWatch, targets, seedSlot, err := r.loadAndSeed(startup)
	if err != nil {
		return err
	}
	watchObservationConsumer := r.earn.ConsumerName() + ":watch-observation"
	watchObservationSlot, err := r.earnStore.ReplayCursor(startup, watchObservationConsumer)
	if err != nil {
		return err
	}
	// Only initialization belongs to this deadline. The stream and the
	// binding recovery it may need are owned by the process context.
	cancelStartup()
	appDone.Add(1)
	go func() { defer appDone.Done(); r.refreshSupportedReserveCatalog(appCtx) }()
	manager, plan, watchObservationSlot, err := r.startSession(ctx, currentWatch, targets, seedSlot, 0, watchObservationConsumer, watchObservationSlot)
	if err != nil {
		return err
	}
	defer func() {
		if manager != nil {
			manager.Close()
		}
	}()
	if err = currentWatch.AnchorNewEarnBindings(nil, plan.from); err != nil {
		return err
	}
	sessionStarted := time.Now()
	r.logger.Info("combined LaserStream worker started", "fromSlot", plan.from, "kaminoReserves", len(targets), "ataTargets", len(currentWatch.ATAs), "earnVaults", len(currentWatch.Vaults))
	watchCtx, cancelWatch := context.WithCancel(ctx)
	watchRefresh, watchDone := startWatchRefreshSignals(watchCtx, r.cfg.NeonDatabaseURL, r.cfg.WatchRefresh, r.logger)
	defer func() { cancelWatch(); <-watchDone }()
	// Rust's monitor loop: the confirmed_refresh tick only requests a safety
	// sweep, and a 100 ms batch tick verifies whatever the stream or the sweep
	// marked dirty, one batch at a time.
	sweepTicker := time.NewTicker(r.cfg.VerifyRefresh)
	defer sweepTicker.Stop()
	batchTicker := time.NewTicker(kaminoVerificationBatchInterval)
	defer batchTicker.Stop()
	progressTicker := time.NewTicker(5 * time.Second)
	defer progressTicker.Stop()
	verificationFailures := 0
	var verificationFailingSince time.Time
	var backoff reconnectBackoff
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-manager.Errors():
			r.facts.Failed(engine.FamilyObserver, "combined_stream_stopped")
			r.handler.lastSlotAt.Store(0)
			r.logger.Error("combined LaserStream session stopped; reconnecting from durable frontier", "event", "laserstream_worker_session_failed", "error", err)
			frontier := manager.ActiveFrontier()
			manager.Close()
			var resumeErr error
			manager, watchObservationSlot, resumeErr = r.resumeSession(ctx, currentWatch, targets, frontier, &backoff, watchObservationConsumer, watchObservationSlot)
			if resumeErr != nil {
				return resumeErr
			}
			sessionStarted = time.Now()
		case <-watchRefresh:
			refreshErr := func() error {
				passCtx, cancelPass := context.WithTimeout(ctx, r.passTimeout())
				defer cancelPass()
				scanBoundary := manager.ActiveFrontier()
				if scanBoundary == 0 {
					scanBoundary = watchObservationSlot
				}
				nextWatch, nextTargets, loadErr := r.load(passCtx)
				if loadErr != nil {
					r.facts.Failed(engine.FamilyObserver, "watch_refresh")
					r.logger.Error("failed to refresh combined LaserStream watch set", "error", loadErr)
					return nil
				}
				if anchorErr := nextWatch.AnchorNewEarnBindings(currentWatch, watchObservationSlot); anchorErr != nil {
					return anchorErr
				}
				if retainErr := nextWatch.RetainPreviousEarnBindings(currentWatch); retainErr != nil {
					return retainErr
				}
				if recovered, recoveryErr := r.recoverNewEarnBindings(passCtx, currentWatch, nextWatch); recoveryErr != nil {
					r.facts.Failed(engine.FamilyObserver, "earn_binding_rpc_recovery")
					r.logger.Error("failed to recover newly discovered Earn bindings; old stream retained", "event", "earn_binding_rpc_recovery_failed", "error", recoveryErr)
					return nil
				} else if recovered > 0 {
					r.logger.Info("recovered newly discovered Earn binding state from confirmed RPC", "insertedJobs", recovered)
				}
				if recovered, gapErr := r.recoverEarnMaxGaps(passCtx, nextWatch); gapErr != nil {
					r.facts.Failed(engine.FamilyObserver, "earn_max_rpc_gap_recovery")
					r.logger.Error("Earn MAX RPC gap recovery failed", "event", "earn_max_rpc_gap_recovery_failed", "error", gapErr)
					return nil
				} else if recovered > 0 {
					r.logger.Info("enqueued Earn MAX RPC gap updates", "insertedJobs", recovered)
				}
				if nextWatch.Fingerprint() == currentWatch.Fingerprint() && targetFingerprint(nextTargets) == targetFingerprint(targets) {
					if passCtx.Err() != nil {
						return nil
					}
					r.kamino.SetTargets(nextTargets)
					targets = nextTargets
					if scanBoundary > watchObservationSlot {
						if cursorErr := r.earnStore.AdvanceReplayCursor(passCtx, watchObservationConsumer, scanBoundary); cursorErr != nil {
							return fmt.Errorf("persist watch observation: %w", cursorErr)
						}
						watchObservationSlot = scanBoundary
					}
					return nil
				}
				bindingStart, anchorErr := nextWatch.NewEarnBindingStart(currentWatch)
				if anchorErr != nil {
					return anchorErr
				}
				if bindingStart != nil {
					// New bindings were just read from confirmed state above, so
					// an anchor older than the provider window needs no replay;
					// requesting it would fail every handoff with OutOfRange.
					current, slotErr := r.rpc.Slot(passCtx, "confirmed")
					if slotErr != nil {
						r.facts.Failed(engine.FamilyObserver, "filter_handoff")
						r.logger.Error("combined filter-set handoff could not read the current slot; old stream retained", "error", slotErr)
						return nil
					}
					start := clampToReplayWindow(*bindingStart, current)
					bindingStart = &start
				}
				r.ata.SetTargets(nextWatch.ATAs)
				r.earn.SetWatchSet(nextWatch)
				r.kamino.SetTargets(nextTargets)
				requested := manager.ActiveFrontier()
				if bindingStart != nil && *bindingStart < requested {
					requested = *bindingStart
				}
				replacement, buildErr := r.request(nextWatch, nextTargets, requested)
				if buildErr != nil {
					return buildErr
				}
				if handoffErr := manager.Handoff(passCtx, replacement); handoffErr != nil {
					r.facts.Failed(engine.FamilyObserver, "filter_handoff")
					r.logger.Error("combined filter-set handoff failed; old stream retained", "error", handoffErr)
					r.ata.SetTargets(currentWatch.ATAs)
					r.earn.SetWatchSet(currentWatch)
					r.kamino.SetTargets(targets)
					return nil
				}
				if scanBoundary > watchObservationSlot {
					if cursorErr := r.earnStore.AdvanceReplayCursor(passCtx, watchObservationConsumer, scanBoundary); cursorErr != nil {
						return fmt.Errorf("persist watch observation after handoff: %w", cursorErr)
					}
					watchObservationSlot = scanBoundary
				}
				currentWatch, targets = nextWatch, nextTargets
				r.logger.Info("combined filter-set handoff promoted", "frontier", manager.ActiveFrontier(), "kaminoReserves", len(targets), "ataTargets", len(currentWatch.ATAs), "earnVaults", len(currentWatch.Vaults))
				return nil
			}()
			if refreshErr != nil {
				return refreshErr
			}
		case <-sweepTicker.C:
			r.kamino.RequestSafetySweep()
		case <-batchTicker.C:
			ran, verifyErr := r.verifyPass(ctx)
			if !ran {
				continue
			}
			if verifyErr != nil {
				if verificationFailures == 0 {
					verificationFailingSince = time.Now()
				}
				verificationFailures++
				r.facts.Failed(engine.FamilyObserver, "kamino_confirmed_verification")
				if terminalErr := persistentVerificationError(verificationFailures, time.Since(verificationFailingSince), r.verificationFailureBudget(), verifyErr); terminalErr != nil {
					r.logger.Error("Kamino confirmed-state verification exhausted retries; restarting", "event", "kamino_confirmed_verification_stalled", "consecutiveFailures", verificationFailures, "error", verifyErr)
					return terminalErr
				}
				r.logger.Warn("Kamino confirmed-state verification failed", "event", "kamino_confirmed_verification_failed", "consecutiveFailures", verificationFailures, "error", verifyErr)
			} else {
				verificationFailures = 0
			}
		case <-progressTicker.C:
			frontier := manager.ActiveFrontier()
			if (frontier == 0 && time.Since(sessionStarted) > r.cfg.ProgressTimeout) || r.handler.stalled(r.cfg.ProgressTimeout) {
				r.facts.Failed(engine.FamilyObserver, "stream_progress_stalled")
				return fmt.Errorf("combined LaserStream progress stalled at slot %d", frontier)
			}
		}
	}
}

func (r *Runtime) load(ctx context.Context) (*watch.Set, []kamino.Target, error) {
	if err := validateWatchNamespace(ctx, r.cfg.Cluster, r.rpc); err != nil {
		return nil, nil, err
	}
	set, err := r.watchLoader.Load(ctx)
	if err != nil {
		return nil, nil, err
	}
	targets, err := r.kaminoStore.LoadTargets(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(targets) == 0 {
		return nil, nil, errors.New("no active Kamino reserve targets")
	}
	targets, err = r.kaminoCatalog.ObservationTargets(ctx, targets)
	if err != nil {
		return nil, nil, fmt.Errorf("refresh Kamino observation catalog: %w", err)
	}
	return set, targets, nil
}
func (r *Runtime) loadAndSeed(ctx context.Context) (*watch.Set, []kamino.Target, uint64, error) {
	set, targets, err := r.load(ctx)
	if err != nil {
		return nil, nil, 0, err
	}
	// Rust never starts from an arbitrarily old catalog: a complete API
	// catalog is committed before the monitor seeds, and any failure aborts
	// startup. load validated the cluster namespace first; an exact refresh
	// cannot change the identities it selected, only renew fetched_at.
	if _, err := r.publishSupportedReserves(ctx); err != nil {
		return nil, nil, 0, fmt.Errorf("publish supported Kamino reserves before observer startup: %w", err)
	}
	r.ata.SetTargets(set.ATAs)
	r.earn.SetWatchSet(set)
	r.kamino.SetTargets(targets)
	if recovered, gapErr := r.recoverEarnMaxGaps(ctx, set); gapErr != nil {
		r.facts.Failed(engine.FamilyObserver, "earn_max_rpc_gap_recovery")
		r.logger.Error("Earn MAX RPC gap recovery failed", "event", "earn_max_rpc_gap_recovery_failed", "error", gapErr)
		return nil, nil, 0, fmt.Errorf("recover initial Earn MAX gaps: %w", gapErr)
	} else if recovered > 0 {
		r.logger.Info("enqueued Earn MAX RPC gap updates", "insertedJobs", recovered)
	}
	if slotDuration, durationErr := r.kaminoCatalog.SlotDuration(ctx); durationErr != nil {
		r.logger.Warn("failed to refresh Kamino slot duration; using 400ms fallback", "error", durationErr)
	} else {
		r.kamino.SetSlotDuration(slotDuration)
	}
	kaminoSlot, err := r.kamino.Seed(ctx)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("seed Kamino state: %w", err)
	}
	ataSlot, err := r.ata.Seed(ctx)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("seed ATA state: %w", err)
	}
	if ataSlot > 0 && ataSlot < kaminoSlot {
		kaminoSlot = ataSlot
	}
	return set, targets, kaminoSlot, nil
}

// A whole pass gets one budget, including its SQL and every RPC batch. Leave
// at least half the progress window for control/error processing between passes.
// refreshEarnAPY writes hourly Earn APY snapshots until ctx ends.
func (r *Runtime) refreshEarnAPY(ctx context.Context) {
	for {
		written, err := r.apy.Refresh(ctx, time.Now())
		if err != nil {
			r.facts.Failed(engine.FamilyObserver, "earn_apy_refresh")
			r.logger.Error("Earn APY snapshot refresh failed", "event", "earn_apy_refresh_stalled", "error", err)
		} else {
			r.logger.Info("refreshed Earn APY snapshots", "inserted_or_updated", written)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(earn.APYRefreshInterval):
		}
	}
}

func (r *Runtime) passTimeout() time.Duration {
	return min(r.cfg.ProgressTimeout/2, 30*time.Second)
}

// verifyPass runs one dirty confirmed-read batch; false means none was due.
func (r *Runtime) verifyPass(ctx context.Context) (bool, error) {
	passCtx, cancel := context.WithTimeout(ctx, r.passTimeout())
	defer cancel()
	return r.kamino.VerifyDirty(passCtx)
}

// verificationFailureBudget is how long confirmed reads may keep failing
// before the observer restarts: the failure threshold in safety sweeps, so
// the 100 ms batch retry cadence cannot turn a short RPC outage terminal.
func (r *Runtime) verificationFailureBudget() time.Duration {
	return kaminoVerificationFailureThreshold * r.cfg.VerifyRefresh
}

// refreshSupportedReserveCatalog is Rust's spawn_supported_reserve_catalog_refresh:
// the observer owns kamino.supported_reserves and renews fetched_at, which the
// planners admit for at most 300 s, every CatalogRefresh until ctx ends. A
// failed pass is reported and the next tick retries, as in Rust.
func (r *Runtime) refreshSupportedReserveCatalog(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.CatalogRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		passCtx, cancel := context.WithTimeout(ctx, r.passTimeout())
		count, err := r.publishSupportedReserves(passCtx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			r.facts.Failed(engine.FamilyObserver, "kamino_catalog_refresh")
			r.logger.Warn("supported Kamino reserve catalog refresh failed; the committed catalog was not renewed", "event", "kamino_catalog_refresh_failed", "error", err)
			continue
		}
		r.logger.Info("supported Kamino reserve catalog refreshed", "catalogCount", count)
	}
}

func (r *Runtime) publishSupportedReserves(ctx context.Context) (int, error) {
	records, err := r.kaminoCatalog.SupportedReserves(ctx)
	if err != nil {
		return 0, err
	}
	if err := r.kaminoStore.RefreshSupportedReserves(ctx, records); err != nil {
		return 0, err
	}
	return len(records), nil
}

// laserStreamReplaySlots is LASERSTREAM_MAX_REPLAY_SLOTS in the Rust monitor
// (balance-sweep-ata-monitor main.rs). LaserStream replays about 216,000
// slots; an older from_slot fails every subscribe attempt with OutOfRange.
const laserStreamReplaySlots uint64 = 200_000

// clampToReplayWindow is Rust's clamp_laserstream_replay_start: a start older
// than the provider window begins at the window edge, and the caller recovers
// the skipped range from confirmed account state instead of replay.
func clampToReplayWindow(start, current uint64) uint64 {
	if current > laserStreamReplaySlots && start < current-laserStreamReplaySlots {
		return current - laserStreamReplaySlots
	}
	return start
}

// streamPlan is where one LaserStream session starts: requested is the
// continuity point, from is requested clamped into the provider window. watch
// is the watch-observation continuity the plan used; fresh means no durable
// Earn continuity exists at all (neither this observer's watch observation
// nor an Earn cursor).
type streamPlan struct {
	requested, from, watch uint64
	fresh                  bool
}

// gap reports a continuity point the provider can no longer replay.
func (p streamPlan) gap() bool { return p.from > p.requested }

// planSession chooses the from_slot of every LaserStream session, the first
// one and each reconnect, so their replay rules cannot diverge. A session that
// delivered slots is continued from its durable frontier; otherwise (process
// start, or a session that failed before its first slot) from the durable
// cursors, as Rust's resume_from_durable_cursor does. seed is the Kamino/ATA
// seed slot and is zero after process start.
func (r *Runtime) planSession(ctx context.Context, seed, frontier, watchCursor uint64) (streamPlan, error) {
	ctx, cancel := context.WithTimeout(ctx, r.passTimeout())
	defer cancel()
	current, err := r.rpc.Slot(ctx, "confirmed")
	if err != nil {
		return streamPlan{}, err
	}
	var requested uint64
	continuity := watchCursor
	if frontier > 0 {
		requested = min(subtract(frontier, r.cfg.ReplayOverlapSlots), current)
	} else {
		earnCursor, err := r.earnStore.ReplayCursor(ctx, r.earn.ConsumerName())
		if err != nil {
			return streamPlan{}, err
		}
		policyCursor, err := r.earnStore.ProjectionCursor(ctx, earn.PolicyProjectionConsumer)
		if err != nil {
			return streamPlan{}, err
		}
		// Rust never wrote a watch observation; it continued from its Earn
		// cursor alone. Without this observer's own observation, that cursor
		// is the continuity, so continuing Rust is not a first observation.
		if continuity == 0 {
			continuity = earnCursor
		}
		if requested, err = selectReplayStart(current, seed, earnCursor, policyCursor, continuity, r.cfg.ReplayOverlapSlots); err != nil {
			return streamPlan{}, err
		}
	}
	return streamPlan{requested: requested, from: clampToReplayWindow(requested, current), watch: continuity, fresh: continuity == 0}, nil
}

// startSession opens one LaserStream session through planSession. Bindings
// are read from confirmed state (recoverWatchState) only for a real gap: a
// start clamped into the provider window, or no durable Earn continuity at
// all. Continuing durable cursors (Rust's included) is covered by replay, as
// it was for Rust, and that continuity becomes the recorded watch
// observation. It returns the started manager, the plan and the watch
// observation cursor.
func (r *Runtime) startSession(ctx context.Context, set *watch.Set, targets []kamino.Target, seed, frontier uint64, watchConsumer string, watchCursor uint64) (*stream.Manager, streamPlan, uint64, error) {
	plan, err := r.planSession(ctx, seed, frontier, watchCursor)
	if err != nil {
		return nil, plan, watchCursor, err
	}
	if plan.gap() {
		r.logger.Error("LaserStream cursor is outside the replay window; recovering every Earn binding from confirmed account state", "event", "laserstream_replay_window_exceeded", "requestedFromSlot", plan.requested, "clampedFromSlot", plan.from, "skippedSlots", plan.from-plan.requested)
	}
	request, err := r.request(set, targets, plan.from)
	if err != nil {
		return nil, plan, watchCursor, err
	}
	switch {
	case plan.fresh || plan.gap():
		recovered, err := r.recoverWatchState(ctx, set, watchConsumer, plan.from)
		if err != nil {
			return nil, plan, watchCursor, err
		}
		r.logger.Info("recovered Earn binding state from confirmed RPC", "insertedJobs", recovered, "freshEarnState", plan.fresh, "replayGap", plan.gap())
		watchCursor = max(watchCursor, plan.from)
	case watchCursor == 0:
		cursorCtx, cancel := context.WithTimeout(ctx, r.passTimeout())
		err := r.earnStore.AdvanceReplayCursor(cursorCtx, watchConsumer, plan.watch)
		cancel()
		if err != nil {
			return nil, plan, watchCursor, fmt.Errorf("record continued watch observation: %w", err)
		}
		watchCursor = plan.watch
	}
	manager := stream.NewManager(r.connector, r.handler, stream.Config{ReplayOverlapSlots: r.cfg.ReplayOverlapSlots, HandoffTimeout: r.cfg.HandoffTimeout})
	if err := manager.Start(ctx, request); err != nil {
		manager.Close()
		return nil, plan, watchCursor, err
	}
	return manager, plan, watchCursor, nil
}

// reconnectBackoff paces every LaserStream reconnect with the Rust ATA/Earn
// monitor policy (balance-sweep-ata-monitor lib.rs reconnect_backoff and
// run_laserstream_loop): 500 ms doubling per reconnect up to 30 s, counted
// whether the failed session had started or not. Rust restarted the count
// only by rebuilding the session; here it restarts once a session moved the
// durable frontier past the previous failure, so a session that starts and
// dies on the same update keeps backing off instead of hammering LaserStream.
type reconnectBackoff struct {
	delay    time.Duration
	failedAt uint64
}

const (
	reconnectBaseDelay = 500 * time.Millisecond
	reconnectMaxDelay  = 30 * time.Second
)

// next returns the wait before reconnecting after a session that reached
// frontier (zero when it delivered no slot).
func (b *reconnectBackoff) next(frontier uint64) time.Duration {
	if b.delay == 0 || frontier > b.failedAt {
		b.delay = reconnectBaseDelay
	} else {
		b.delay = min(b.delay*2, reconnectMaxDelay)
	}
	b.failedAt = max(b.failedAt, frontier)
	return b.delay
}

// resumeSession replaces a failed session. Every attempt waits its backoff
// first and plans afresh, so an outage longer than the provider window cannot
// keep requesting a slot that has left it, and a clamped plan recovers
// bindings before the stream resumes.
func (r *Runtime) resumeSession(ctx context.Context, set *watch.Set, targets []kamino.Target, frontier uint64, backoff *reconnectBackoff, watchConsumer string, watchCursor uint64) (*stream.Manager, uint64, error) {
	for attempt := 1; ; attempt++ {
		delay := backoff.next(frontier)
		select {
		case <-ctx.Done():
			return nil, watchCursor, ctx.Err()
		case <-time.After(delay):
		}
		manager, _, cursor, err := r.startSession(ctx, set, targets, 0, frontier, watchConsumer, watchCursor)
		watchCursor = cursor
		if err == nil {
			return manager, watchCursor, nil
		}
		if ctx.Err() != nil {
			return nil, watchCursor, ctx.Err()
		}
		r.logger.Error("LaserStream reconnect failed", "attempt", attempt, "waited", delay, "error", err)
	}
}

// selectReplayStart takes the oldest durable cursor, as Rust's
// laserstream_replay_start_slot does per stream. Earn observation anchors
// (route_policies.last_seen_slot) are not continuity cursors: most predate the
// provider window, and the bindings they anchor are recovered from confirmed
// state when no cursor covers them. A zero seed or cursor is absent.
func selectReplayStart(current, seed, earnCursor, policyCursor, watchCursor, overlap uint64) (uint64, error) {
	var starts []uint64
	if seed > 0 {
		starts = append(starts, subtract(seed, overlap))
	}
	if earnCursor > 0 {
		starts = append(starts, subtract(earnCursor, overlap))
	}
	if policyCursor > 0 {
		starts = append(starts, subtract(policyCursor, overlap))
	}
	if watchCursor > 0 {
		starts = append(starts, subtract(watchCursor, overlap))
	} else {
		// First deployment has no durable watch scan yet. Bound discovery gaps
		// independently of ahead Earn/projection cursors.
		starts = append(starts, subtract(current, max(overlap, 10_000)))
	}
	requested := current
	for _, start := range starts {
		if start > 0 && start < requested {
			requested = start
		}
	}
	if requested == 0 {
		return 0, errors.New("combined replay start resolved to zero")
	}
	return requested, nil
}

func (r *Runtime) request(set *watch.Set, targets []kamino.Target, from uint64) (*pb.SubscribeRequest, error) {
	accounts := make(map[string]subscription.AccountFilter, len(set.Channels)+1)
	reserves := make([]string, len(targets))
	for index, target := range targets {
		reserves[index] = target.Reserve
	}
	accounts[subscription.KaminoReserves] = subscription.AccountFilter{Addresses: reserves}
	for channel, addresses := range set.Channels {
		if len(addresses) > 0 {
			accounts[channel] = subscription.AccountFilter{Addresses: addresses, RequireTxnSignature: true}
		}
	}
	return subscription.Build(subscription.Spec{FromSlot: from, Accounts: accounts})
}

type earnBindingRecovery struct {
	address string
	filters []string
}

func newEarnBindingRecoveries(previous, next *watch.Set) []earnBindingRecovery {
	existing := make(map[string]struct{})
	if previous != nil {
		for _, vault := range previous.Vaults {
			for _, account := range vault.Accounts {
				existing[vault.Environment+":"+vault.Vault+":"+account.Role+":"+account.Pubkey] = struct{}{}
			}
		}
	}
	filtersByAddress := make(map[string]map[string]struct{})
	for _, vault := range next.Vaults {
		for _, account := range vault.Accounts {
			key := vault.Environment + ":" + vault.Vault + ":" + account.Role + ":" + account.Pubkey
			if _, ok := existing[key]; ok {
				continue
			}
			channel := watch.ChannelForRole(account.Role)
			if channel == "" {
				continue
			}
			if filtersByAddress[account.Pubkey] == nil {
				filtersByAddress[account.Pubkey] = make(map[string]struct{})
			}
			filtersByAddress[account.Pubkey][channel] = struct{}{}
		}
	}
	result := make([]earnBindingRecovery, 0, len(filtersByAddress))
	for address, filterSet := range filtersByAddress {
		filters := make([]string, 0, len(filterSet))
		for filter := range filterSet {
			filters = append(filters, filter)
		}
		sort.Strings(filters)
		result = append(result, earnBindingRecovery{address: address, filters: filters})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].address < result[j].address })
	return result
}

// earnBindingRecoveryBatch is the getMultipleAccounts limit; each batch is one
// RPC read and one capture transaction under its own deadline.
const earnBindingRecoveryBatch = 100

// recoverWatchState reads every binding of set from confirmed state, then
// records the watch observation at fromSlot. Its cost grows with the watch
// set, so each batch carries a pass deadline and the whole read carries none.
// The cursor moves only after every batch is durable; a restart before then
// repeats the read under the same event keys and inserts no duplicate jobs.
func (r *Runtime) recoverWatchState(ctx context.Context, set *watch.Set, watchConsumer string, fromSlot uint64) (int64, error) {
	recovered, err := r.recoverNewEarnBindings(ctx, nil, set)
	if err != nil {
		return recovered, fmt.Errorf("recover Earn binding state: %w", err)
	}
	cursorCtx, cancel := context.WithTimeout(ctx, r.passTimeout())
	defer cancel()
	if err := r.earnStore.AdvanceReplayCursor(cursorCtx, watchConsumer, fromSlot); err != nil {
		return recovered, fmt.Errorf("persist watch observation after binding recovery: %w", err)
	}
	return recovered, nil
}

func (r *Runtime) recoverNewEarnBindings(ctx context.Context, previous, next *watch.Set) (int64, error) {
	bindings := newEarnBindingRecoveries(previous, next)
	var inserted int64
	for start := 0; start < len(bindings); start += earnBindingRecoveryBatch {
		batch := bindings[start:min(start+earnBindingRecoveryBatch, len(bindings))]
		recovered, err := r.recoverEarnBindingBatch(ctx, next, batch)
		inserted += recovered
		if err != nil {
			return inserted, err
		}
	}
	return inserted, nil
}

func (r *Runtime) recoverEarnBindingBatch(ctx context.Context, next *watch.Set, batch []earnBindingRecovery) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, r.passTimeout())
	defer cancel()
	addresses := make([]string, len(batch))
	for index, binding := range batch {
		addresses[index] = binding.address
	}
	response, err := r.rpc.MultipleAccounts(ctx, addresses, "confirmed", nil)
	if err != nil {
		return 0, fmt.Errorf("read Earn binding accounts: %w", err)
	}
	if response.Slot == 0 || len(response.Accounts) != len(addresses) {
		return 0, fmt.Errorf("earn binding recovery returned slot %d and %d/%d accounts", response.Slot, len(response.Accounts), len(addresses))
	}
	events := make([]earn.QueuedEvent, 0, len(batch))
	for index, binding := range batch {
		affected := next.AffectedVaults(binding.address)
		if len(affected) == 0 {
			return 0, fmt.Errorf("earn binding %s has no affected vault", binding.address)
		}
		eventKey, kind := bindingRecoveryEvent(binding.address, response.Accounts[index])
		// Only vaults that can apply an unsigned state read receive one; the
		// rest would fail until dead-lettered (earn.SnapshotApplicable).
		vaults := make([]watch.Vault, 0, len(affected))
		for _, vault := range affected {
			if earn.SnapshotApplicable(vault, binding.address, kind == "account_deleted") {
				vaults = append(vaults, vault)
			} else if !vault.EarnMax {
				r.logger.Warn("Earn policy closed outside the replayable range; its closing transaction is not recoverable from account state", "event", "earn_policy_closed_unreplayed", "policy", binding.address, "vault", vault.Vault)
			}
		}
		if len(vaults) == 0 {
			continue
		}
		address := binding.address
		update := earn.NormalizedUpdate{EventKey: &eventKey, Filters: binding.filters, EventKind: kind, AccountPubkey: &address, Slot: response.Slot}
		events = append(events, earn.QueuedEvent{EventKey: eventKey, Slot: response.Slot, Event: update, Vaults: vaults, Account: binding.address})
	}
	if len(events) == 0 {
		return 0, nil
	}
	outcome, err := r.earnStore.EnqueueBatch(ctx, r.earn.ConsumerName(), events)
	if err != nil {
		return 0, fmt.Errorf("enqueue Earn binding recovery: %w", err)
	}
	return outcome.InsertedJobs, nil
}

// bindingRecoveryEvent keys a recovered binding by its address and the
// confirmed state read, never by the read slot: reading the same state again
// after a restart names the same job, which the job key then deduplicates.
func bindingRecoveryEvent(address string, account *solanarpc.Account) (string, string) {
	if account == nil || account.Lamports == 0 {
		return "watch-discovery:" + address + ":deleted", "account_deleted"
	}
	digest := sha256.New()
	var lamports [8]byte
	binary.LittleEndian.PutUint64(lamports[:], account.Lamports)
	digest.Write(lamports[:])
	digest.Write([]byte(account.Owner))
	if account.Executable {
		digest.Write([]byte{0, 1})
	} else {
		digest.Write([]byte{0, 0})
	}
	digest.Write(account.Data)
	return "watch-discovery:" + address + ":" + hex.EncodeToString(digest.Sum(nil)), "account"
}

func (r *Runtime) recoverEarnMaxGaps(ctx context.Context, set *watch.Set) (int64, error) {
	type candidate struct {
		slot               uint64
		signature, custody string
		vault              watch.Vault
	}
	var candidates []candidate
	for _, vault := range set.Vaults {
		if !vault.EarnMax || vault.ObservationStartSlot == nil {
			continue
		}
		custody, err := watch.USDCATA(vault.Vault)
		if err != nil {
			return 0, err
		}
		before := ""
		vaultCandidateStart := len(candidates)
		for {
			page, err := r.rpc.SignaturesForAddress(ctx, custody, "confirmed", before, 1_000)
			if err != nil {
				return 0, fmt.Errorf("read Earn MAX custody history for %s: %w", custody, err)
			}
			if len(page) == 0 {
				break
			}
			reachedAnchor := false
			for _, status := range page {
				if status.Slot <= *vault.ObservationStartSlot {
					reachedAnchor = true
					break
				}
				if string(status.Err) == "null" || len(status.Err) == 0 {
					candidates = append(candidates, candidate{status.Slot, status.Signature, custody, vault})
				}
			}
			if reachedAnchor || len(page) < 1_000 {
				break
			}
			if len(candidates)-vaultCandidateStart >= 10_000 {
				return 0, fmt.Errorf("earn MAX custody history exceeded 10000 signatures before anchor slot %d for %s", *vault.ObservationStartSlot, custody)
			}
			before = page[len(page)-1].Signature
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].slot == candidates[j].slot {
			return candidates[i].signature < candidates[j].signature
		}
		return candidates[i].slot < candidates[j].slot
	})
	var inserted int64
	for _, item := range candidates {
		eventKey := fmt.Sprintf("earn-max-rpc-gap:%d:%s:%s", item.slot, item.signature, item.custody)
		signature, account := item.signature, item.custody
		update := earn.NormalizedUpdate{EventKey: &eventKey, Filters: []string{watch.EarnIdleTokenAccounts}, EventKind: "account", AccountPubkey: &account, Slot: item.slot, Signature: &signature}
		outcome, err := r.earnStore.Enqueue(ctx, r.earn.ConsumerName(), eventKey, item.slot, update, []watch.Vault{item.vault}, item.custody)
		if err != nil {
			return inserted, err
		}
		inserted += outcome.InsertedJobs
	}
	return inserted, nil
}

func persistentVerificationError(consecutiveFailures int, failingFor, budget time.Duration, cause error) error {
	if consecutiveFailures < kaminoVerificationFailureThreshold || failingFor < budget {
		return nil
	}
	return fmt.Errorf("Kamino confirmed-state verification failed %d consecutive times over %s: %w", consecutiveFailures, failingFor.Round(time.Second), cause)
}

func targetFingerprint(targets []kamino.Target) string {
	values := make([]string, len(targets))
	for index, target := range targets {
		market, mint := "", ""
		if target.Market != nil {
			market = *target.Market
		}
		if target.LiquidityMint != nil {
			mint = *target.LiquidityMint
		}
		values[index] = target.Reserve + ":" + market + ":" + mint
	}
	sort.Strings(values)
	return fmt.Sprint(values)
}
func subtract(value, delta uint64) uint64 {
	if value <= delta {
		return 1
	}
	return value - delta
}
