package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/ata"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/config"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/earn"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/observability"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/solanarpc"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/stream"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/subscription"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/watch"
)

const kaminoVerificationFailureThreshold = 3

type Runtime struct {
	cfg           config.Config
	logger        *slog.Logger
	health        *observability.Health
	metrics       *observability.Metrics
	neon          *pgxpool.Pool
	timescale     *pgxpool.Pool
	rpc           *solanarpc.Client
	watchLoader   *watch.Loader
	kaminoStore   *kamino.Store
	kaminoCatalog *kamino.CatalogClient
	kamino        *kamino.Handler
	ata           *ata.Handler
	earnStore     *earn.Store
	earn          *earn.Handler
	bridge        *earn.Bridge
	handler       *DurableHandler
}

func New(ctx context.Context, cfg config.Config, logger *slog.Logger, health *observability.Health, metrics *observability.Metrics) (*Runtime, error) {
	neon, err := pgxpool.New(ctx, cfg.NeonDatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect Neon: %w", err)
	}
	timescale, err := pgxpool.New(ctx, cfg.TimescaleDatabaseURL)
	if err != nil {
		neon.Close()
		return nil, fmt.Errorf("connect Timescale: %w", err)
	}
	if err = neon.Ping(ctx); err != nil {
		neon.Close()
		timescale.Close()
		return nil, fmt.Errorf("ping Neon: %w", err)
	}
	if err = timescale.Ping(ctx); err != nil {
		neon.Close()
		timescale.Close()
		return nil, fmt.Errorf("ping Timescale: %w", err)
	}
	rpc := solanarpc.New(cfg.SolanaRPCURL, 30*time.Second)
	kaminoStore := kamino.NewStore(timescale, "kamino")
	kaminoCatalog := kamino.NewCatalogClient(cfg.KaminoAPIBase, 30*time.Second)
	kaminoHandler := kamino.NewHandler(kaminoStore, rpc, logger, 400, false)
	ataHandler := ata.NewHandler(timescale, cfg.ATAStream, rpc)
	earnStore := earn.NewStore(neon)
	earnHandler := earn.NewHandler(earnStore, cfg.Cluster)
	bridge, err := earn.StartBridge(ctx, logger, os.Getenv("EARN_DOMAIN_BRIDGE_BINARY"))
	if err != nil {
		neon.Close()
		timescale.Close()
		return nil, err
	}
	runtime := &Runtime{cfg: cfg, logger: logger, health: health, metrics: metrics, neon: neon, timescale: timescale, rpc: rpc, watchLoader: watch.NewLoader(neon, cfg.Cluster), kaminoStore: kaminoStore, kaminoCatalog: kaminoCatalog, kamino: kaminoHandler, ata: ataHandler, earnStore: earnStore, earn: earnHandler, bridge: bridge}
	runtime.handler = &DurableHandler{Kamino: kaminoHandler, ATA: ataHandler, Earn: earnHandler, Bridge: bridge, Health: health, Metrics: metrics}
	return runtime, nil
}

func (r *Runtime) Close() {
	if r.bridge != nil {
		_ = r.bridge.Close()
	}
	if r.neon != nil {
		r.neon.Close()
	}
	if r.timescale != nil {
		r.timescale.Close()
	}
}

func (r *Runtime) Run(ctx context.Context) error {
	currentWatch, targets, seedSlot, err := r.loadAndSeed(ctx)
	if err != nil {
		return err
	}
	watchObservationConsumer := r.earn.ConsumerName() + ":watch-observation"
	watchObservationSlot, err := r.earnStore.ReplayCursor(ctx, watchObservationConsumer)
	if err != nil {
		return err
	}
	firstWatchObservation := watchObservationSlot == 0
	fromSlot, err := r.replayStart(ctx, seedSlot, currentWatch.ObservationStartSlot, watchObservationSlot)
	if err != nil {
		return err
	}
	if err = currentWatch.AnchorNewEarnBindings(nil, fromSlot); err != nil {
		return err
	}
	if firstWatchObservation {
		recovered, recoveryErr := r.recoverNewEarnBindings(ctx, nil, currentWatch)
		if recoveryErr != nil {
			return fmt.Errorf("recover initial Earn bindings: %w", recoveryErr)
		}
		r.logger.Info("recovered initial Earn binding state from confirmed RPC", "insertedJobs", recovered)
		watchObservationSlot = fromSlot
		if err = r.earnStore.AdvanceReplayCursor(ctx, watchObservationConsumer, watchObservationSlot); err != nil {
			return fmt.Errorf("persist initial watch observation: %w", err)
		}
	}
	request, err := r.request(currentWatch, targets, fromSlot)
	if err != nil {
		return err
	}
	r.health.ResetProgress()
	manager := stream.NewManager(stream.GRPCConnector{Endpoint: r.cfg.LaserStreamEndpoint, APIKey: r.cfg.HeliusAPIKey}, r.handler, stream.Config{ReplayOverlapSlots: r.cfg.ReplayOverlapSlots, HandoffTimeout: r.cfg.HandoffTimeout})
	if err = manager.Start(ctx, request); err != nil {
		return err
	}
	defer manager.Close()
	r.health.SetConnected(true)
	r.health.SetReady(true)
	sessionStarted := time.Now()
	r.logger.Info("combined LaserStream worker started", "fromSlot", fromSlot, "kaminoReserves", len(targets), "ataTargets", len(currentWatch.ATAs), "earnVaults", len(currentWatch.Vaults))
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	watchRefresh := startWatchRefreshSignals(watchCtx, r.cfg.NeonDatabaseURL, r.cfg.WatchRefresh, r.logger)
	verifyTicker := time.NewTicker(r.cfg.VerifyRefresh)
	defer verifyTicker.Stop()
	progressTicker := time.NewTicker(5 * time.Second)
	defer progressTicker.Stop()
	earnHealthTicker := time.NewTicker(60 * time.Second)
	defer earnHealthTicker.Stop()
	verificationFailures := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-r.bridge.Done():
			if err == nil {
				err = errors.New("earn domain bridge exited")
			}
			r.metrics.RecordFailure(ctx, "earn_domain_bridge_stopped")
			r.health.Fatal(err)
			return fmt.Errorf("earn domain bridge stopped: %w", err)
		case err := <-manager.Errors():
			r.metrics.RecordFailure(ctx, "combined_stream_stopped")
			r.metrics.RecordReconnect(ctx)
			r.health.SetConnected(false)
			r.health.ResetProgress()
			r.logger.Error("combined LaserStream session stopped; reconnecting from durable frontier", "event", "laserstream_worker_session_failed", "error", err)
			frontier := manager.ActiveFrontier()
			manager.Close()
			from := request.GetFromSlot()
			if frontier > 0 {
				from = subtract(frontier, r.cfg.ReplayOverlapSlots)
			}
			var buildErr error
			request, buildErr = r.request(currentWatch, targets, from)
			if buildErr != nil {
				return buildErr
			}
			manager = stream.NewManager(stream.GRPCConnector{Endpoint: r.cfg.LaserStreamEndpoint, APIKey: r.cfg.HeliusAPIKey}, r.handler, stream.Config{ReplayOverlapSlots: r.cfg.ReplayOverlapSlots, HandoffTimeout: r.cfg.HandoffTimeout})
			if startErr := retryStart(ctx, manager, request, r.logger); startErr != nil {
				return startErr
			}
			r.health.SetConnected(true)
			r.health.SetReady(true)
			sessionStarted = time.Now()
		case <-watchRefresh:
			scanBoundary := manager.ActiveFrontier()
			if scanBoundary == 0 {
				scanBoundary = watchObservationSlot
			}
			nextWatch, nextTargets, loadErr := r.load(ctx)
			if loadErr != nil {
				r.metrics.RecordFailure(ctx, "watch_refresh")
				r.logger.Error("failed to refresh combined LaserStream watch set", "error", loadErr)
				continue
			}
			if anchorErr := nextWatch.AnchorNewEarnBindings(currentWatch, watchObservationSlot); anchorErr != nil {
				return anchorErr
			}
			if retainErr := nextWatch.RetainPreviousEarnBindings(currentWatch); retainErr != nil {
				return retainErr
			}
			if recovered, recoveryErr := r.recoverNewEarnBindings(ctx, currentWatch, nextWatch); recoveryErr != nil {
				r.metrics.RecordFailure(ctx, "earn_binding_rpc_recovery")
				r.logger.Error("failed to recover newly discovered Earn bindings; old stream retained", "event", "earn_binding_rpc_recovery_failed", "error", recoveryErr)
				continue
			} else if recovered > 0 {
				r.logger.Info("recovered newly discovered Earn binding state from confirmed RPC", "insertedJobs", recovered)
			}
			if recovered, gapErr := r.recoverEarnMaxGaps(ctx, nextWatch); gapErr != nil {
				r.metrics.RecordFailure(ctx, "earn_max_rpc_gap_recovery")
				r.logger.Error("Earn MAX RPC gap recovery failed", "event", "earn_max_rpc_gap_recovery_failed", "error", gapErr)
			} else if recovered > 0 {
				r.logger.Info("enqueued Earn MAX RPC gap updates", "insertedJobs", recovered)
			}
			if nextWatch.Fingerprint() == currentWatch.Fingerprint() && targetFingerprint(nextTargets) == targetFingerprint(targets) {
				r.kamino.SetTargets(nextTargets)
				targets = nextTargets
				if scanBoundary > watchObservationSlot {
					if cursorErr := r.earnStore.AdvanceReplayCursor(ctx, watchObservationConsumer, scanBoundary); cursorErr != nil {
						return fmt.Errorf("persist watch observation: %w", cursorErr)
					}
					watchObservationSlot = scanBoundary
				}
				continue
			}
			r.ata.SetTargets(nextWatch.ATAs)
			r.earn.SetWatchSet(nextWatch)
			r.kamino.SetTargets(nextTargets)
			requested := manager.ActiveFrontier()
			bindingStart, anchorErr := nextWatch.NewEarnBindingStart(currentWatch)
			if anchorErr != nil {
				return anchorErr
			}
			if bindingStart != nil && *bindingStart < requested {
				requested = *bindingStart
			}
			replacement, buildErr := r.request(nextWatch, nextTargets, requested)
			if buildErr != nil {
				return buildErr
			}
			if handoffErr := manager.Handoff(ctx, replacement); handoffErr != nil {
				r.metrics.RecordHandoff(ctx, "failed")
				r.logger.Error("combined filter-set handoff failed; old stream retained", "error", handoffErr)
				r.ata.SetTargets(currentWatch.ATAs)
				r.earn.SetWatchSet(currentWatch)
				r.kamino.SetTargets(targets)
				continue
			}
			if scanBoundary > watchObservationSlot {
				if cursorErr := r.earnStore.AdvanceReplayCursor(ctx, watchObservationConsumer, scanBoundary); cursorErr != nil {
					return fmt.Errorf("persist watch observation after handoff: %w", cursorErr)
				}
				watchObservationSlot = scanBoundary
			}
			r.metrics.RecordHandoff(ctx, "promoted")
			request = replacement
			currentWatch, targets = nextWatch, nextTargets
			r.logger.Info("combined filter-set handoff promoted", "frontier", manager.ActiveFrontier(), "kaminoReserves", len(targets), "ataTargets", len(currentWatch.ATAs), "earnVaults", len(currentWatch.Vaults))
		case <-verifyTicker.C:
			if verifyErr := r.kamino.Verify(ctx); verifyErr != nil {
				verificationFailures++
				r.metrics.RecordFailure(ctx, "kamino_confirmed_verification")
				if terminalErr := persistentVerificationError(verificationFailures, verifyErr); terminalErr != nil {
					r.health.Fatal(terminalErr)
					r.logger.Error("Kamino confirmed-state verification exhausted retries; restarting", "event", "kamino_confirmed_verification_stalled", "consecutiveFailures", verificationFailures, "error", verifyErr)
					return terminalErr
				}
				r.logger.Warn("Kamino confirmed-state verification failed", "event", "kamino_confirmed_verification_failed", "consecutiveFailures", verificationFailures, "error", verifyErr)
			} else {
				verificationFailures = 0
			}
		case <-earnHealthTicker.C:
			healthCtx, cancelHealth := context.WithTimeout(ctx, 5*time.Second)
			cursor, pending, failed, oldest, healthErr := r.earnStore.Health(healthCtx, r.earn.ConsumerName())
			cancelHealth()
			if healthErr != nil {
				r.metrics.RecordFailure(ctx, "earn_reconciliation_health")
				r.logger.Error("failed to load Earn reconciliation health", "event", "earn_reconciliation_health_snapshot_failed", "error", healthErr)
			} else {
				r.metrics.EarnPending.Set(float64(pending))
				r.metrics.EarnFailed.Set(float64(failed))
				r.metrics.EarnOldestAge.Set(float64(oldest))
				r.health.DomainProgress("earn", cursor)
				if failed > 0 {
					r.logger.Error("Earn reconciliation jobs remain failed and pending", "event", "earn_reconciliation_job_failed", "failedPendingJobs", failed, "oldestPendingAgeSeconds", oldest)
				}
			}
		case <-progressTicker.C:
			frontier := manager.ActiveFrontier()
			if (frontier == 0 && time.Since(sessionStarted) > r.cfg.ProgressTimeout) || r.health.Stale(r.cfg.ProgressTimeout) {
				r.metrics.RecordFailure(ctx, "stream_progress_stalled")
				r.health.Fatal(fmt.Errorf("no durable LaserStream progress for %s", r.cfg.ProgressTimeout))
				return fmt.Errorf("combined LaserStream progress stalled at slot %d", frontier)
			}
		}
	}
}

func (r *Runtime) load(ctx context.Context) (*watch.Set, []kamino.Target, error) {
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
	targets, err = r.kaminoCatalog.Enrich(ctx, targets)
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
	r.ata.SetTargets(set.ATAs)
	r.earn.SetWatchSet(set)
	r.kamino.SetTargets(targets)
	if recovered, gapErr := r.recoverEarnMaxGaps(ctx, set); gapErr != nil {
		r.metrics.RecordFailure(ctx, "earn_max_rpc_gap_recovery")
		r.logger.Error("Earn MAX RPC gap recovery failed", "event", "earn_max_rpc_gap_recovery_failed", "error", gapErr)
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
func (r *Runtime) replayStart(ctx context.Context, seed uint64, observationStart *uint64, watchCursor uint64) (uint64, error) {
	current, err := r.rpc.Slot(ctx, "confirmed")
	if err != nil {
		return 0, err
	}
	earnCursor, err := r.earnStore.ReplayCursor(ctx, r.earn.ConsumerName())
	if err != nil {
		return 0, err
	}
	policyCursor, err := r.earnStore.ProjectionCursor(ctx, earn.PolicyProjectionConsumer)
	if err != nil {
		return 0, err
	}
	return selectReplayStart(current, seed, earnCursor, policyCursor, watchCursor, r.cfg.ReplayOverlapSlots, observationStart)
}

func selectReplayStart(current, seed, earnCursor, policyCursor, watchCursor, overlap uint64, observationStart *uint64) (uint64, error) {
	starts := []uint64{subtract(seed, overlap)}
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
	if observationStart != nil {
		starts = append(starts, *observationStart)
	}
	result := current
	for _, start := range starts {
		if start > 0 && start < result {
			result = start
		}
	}
	if result == 0 {
		return 0, errors.New("combined replay start resolved to zero")
	}
	return result, nil
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

func (r *Runtime) recoverNewEarnBindings(ctx context.Context, previous, next *watch.Set) (int64, error) {
	bindings := newEarnBindingRecoveries(previous, next)
	var inserted int64
	for start := 0; start < len(bindings); start += 100 {
		end := min(start+100, len(bindings))
		addresses := make([]string, end-start)
		for index, binding := range bindings[start:end] {
			addresses[index] = binding.address
		}
		response, err := r.rpc.MultipleAccounts(ctx, addresses, "confirmed", nil)
		if err != nil {
			return inserted, fmt.Errorf("read newly discovered Earn accounts: %w", err)
		}
		if response.Slot == 0 || len(response.Accounts) != len(addresses) {
			return inserted, fmt.Errorf("new Earn account recovery returned slot %d and %d/%d accounts", response.Slot, len(response.Accounts), len(addresses))
		}
		for index, binding := range bindings[start:end] {
			kind := "account_deleted"
			if account := response.Accounts[index]; account != nil && account.Lamports > 0 {
				kind = "account"
			}
			eventKey := fmt.Sprintf("watch-discovery:%d:%s", response.Slot, binding.address)
			address := binding.address
			update := earn.NormalizedUpdate{EventKey: &eventKey, Filters: binding.filters, EventKind: kind, AccountPubkey: &address, Slot: response.Slot}
			vaults := next.AffectedVaults(binding.address)
			if len(vaults) == 0 {
				return inserted, fmt.Errorf("new Earn binding %s has no affected vault", binding.address)
			}
			outcome, err := r.earnStore.Enqueue(ctx, r.earn.ConsumerName(), eventKey, response.Slot, update, vaults, binding.address)
			if err != nil {
				return inserted, fmt.Errorf("enqueue new Earn binding recovery for %s: %w", binding.address, err)
			}
			inserted += outcome.InsertedJobs
		}
	}
	return inserted, nil
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

func persistentVerificationError(consecutiveFailures int, cause error) error {
	if consecutiveFailures < kaminoVerificationFailureThreshold {
		return nil
	}
	return fmt.Errorf("Kamino confirmed-state verification failed %d consecutive times: %w", consecutiveFailures, cause)
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
func retryStart(ctx context.Context, manager *stream.Manager, request *pb.SubscribeRequest, logger *slog.Logger) error {
	delay := 500 * time.Millisecond
	for attempt := 1; ; attempt++ {
		if err := manager.Start(ctx, request); err == nil {
			return nil
		} else {
			logger.Error("LaserStream reconnect failed", "attempt", attempt, "retryIn", delay, "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, 30*time.Second)
	}
}
