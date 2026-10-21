package backyard

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// ObserveConfirmedRouteSnapshot extends the bridge snapshot with the route's
// position and custody, read from the view. It reads no policy: a build reads
// the policies it executes through, and Squads checks them on chain. Only a
// stale reserve's valuation refresh simulates through RPC, at the view slot.
//
// minSlot is the highest slot the route's own operations landed at: nothing
// plans from a view that has not seen its own transactions.
func ObserveConfirmedRouteSnapshot(ctx context.Context, rpc *chain.Client, view *View, manifest RouteManifest, state *vaultState, minSlot int64, inputs targetInputs) (Observation, []ConfirmedAccount, error) {
	if rpc == nil || view == nil {
		return Observation{}, nil, fmt.Errorf("RPC client and account view are required")
	}
	v := manifest.vault
	return observeConfirmedRouteSnapshotWithAccounts(ctx, manifest, routeObservationRuntime{
		minSlot: minSlot,
		inputs:  inputs,
		state:   state,
		read: func(ctx context.Context, addresses []string, minSlot int64) (int64, []ConfirmedAccount, []programAccount, error) {
			// A null strategy receipt must reach the integrity classifier
			// instead of failing the batch as a required absent account.
			return view.readWithReceipts(ctx, v, addresses, minSlot, append(optionalLifecycleObligations(v, addresses), v.strategyReceipt)...)
		},
		refreshValuation: func(ctx context.Context, routes []RuntimeRoute, addresses []string, minSlot int64) (int64, []ConfirmedAccount, error) {
			return simulateRouteValuationRefresh(ctx, v, rpc, routes, addresses, minSlot)
		},
		now: func() time.Time { return time.Now().UTC() },
	})
}

func applyProgramIdentityObservation(observation *Observation, identity programIdentityObservation) {
	if observation == nil {
		return
	}
	observation.Snapshot.ProgramIdentityKnown = identity.Verified
	observation.Snapshot.VoltrProgramDeploySlot = identity.VoltrProgramDeploySlot
	observation.Snapshot.AdaptorProgramDeploySlot = identity.AdaptorProgramDeploySlot
}

// A full K-Lend withdrawal closes its obligation account, so the obligation is
// optional in a read.
func optionalLifecycleObligations(v *Vault, addresses []string) []string {
	present := map[string]bool{}
	for _, address := range addresses {
		present[address] = true
	}
	var optional []string
	for _, lane := range earnLaneIDs(true) {
		route, err := v.route(lane)
		if err == nil && present[route.Kamino.Obligation] {
			optional = append(optional, route.Kamino.Obligation)
		}
	}
	return optional
}

type routeObservationBatch struct {
	Slot           int64
	ObservationID  string
	ManifestSHA256 string
	Accounts       []ConfirmedAccount
}

type routeObservationRuntime struct {
	refreshValuation func(context.Context, []RuntimeRoute, []string, int64) (int64, []ConfirmedAccount, error)
	read             func(context.Context, []string, int64) (int64, []ConfirmedAccount, []programAccount, error)
	minSlot          int64
	now              func() time.Time
	inputs           targetInputs
	state            *vaultState
}

func observeConfirmedRouteSnapshotWithAccounts(ctx context.Context, manifest RouteManifest, runtime routeObservationRuntime) (Observation, []ConfirmedAccount, error) {
	if runtime.read == nil || runtime.now == nil || runtime.state == nil {
		return Observation{}, nil, fmt.Errorf("route observation runtime is incomplete")
	}
	v := manifest.vault
	selectedRoute, err := manifest.activeRuntimeRoute()
	if err != nil {
		return Observation{}, nil, err
	}
	addresses := routeFixedAddresses(v)
	slot, accounts, receipts, err := runtime.read(ctx, addresses, runtime.minSlot)
	if err != nil {
		return Observation{}, nil, err
	}
	demand, fingerprint, err := decodeConfirmedWithdrawalDemand(v, receipts)
	if err != nil {
		return Observation{}, nil, err
	}
	if strategyReceiptIntegrityFault(accountAt(accounts, v.strategyReceipt)) {
		o := receiptIntegrityObservation(slot, selectedRoute)
		o.Snapshot.Trustful = v.trustful()
		return o, accounts, nil
	}
	preferred := flatViewLane(selectedRoute.Lane, runtime.inputs)
	held, valued, err := observedLanes(v, accounts, preferred)
	if err != nil {
		return Observation{ObservedAt: runtime.now(), Snapshot: Snapshot{ObservationID: sha256Bytes([]byte(fmt.Sprintf("selector-ownership:%d:%s", slot, err.Error()))), Slot: slot, RouteKind: RouteKind, RouteLane: selectedRoute.Lane, ManualReason: err.Error()}}, accounts, nil
	}
	viewLanes := valued
	var destination []RuntimeRoute
	if len(held) == 0 {
		viewLanes = append([]string{preferred}, slices.DeleteFunc(slices.Clone(valued), func(l string) bool { return l == preferred })...)
	} else if lane := selectorDestination(runtime.inputs); lane != "" && !slices.Contains(valued, lane) {
		route, err := v.route(lane)
		if err != nil {
			return Observation{}, nil, err
		}
		destination = []RuntimeRoute{route}
	}
	first, err := v.route(viewLanes[0])
	if err != nil {
		return Observation{}, nil, err
	}
	routes, err := navRoutes(v, first, viewLanes)
	if err != nil {
		return Observation{}, nil, err
	}
	if runtime.refreshValuation == nil {
		return Observation{}, nil, fmt.Errorf("route observation has no valuation refresh")
	}
	refreshExhausted := false
	{
		refreshRoutes := withOverflowRoutes(v, append(slices.Clone(routes), destination...))
		captureAddresses := selectorValuationAddresses(v, refreshRoutes, addresses)
		refreshedSlot, refreshedAccounts, refreshErr := runtime.refreshValuation(ctx, refreshRoutes, captureAddresses, slot)
		if len(destination) > 0 && refreshErr == nil {
			viewLanes = append(slices.Clone(valued), destination[0].Lane)
		}
		if refreshSimulationFailed(refreshErr) && len(refreshRoutes) > len(routes) {
			// Kamino refuses to refresh an overflow lane or a selector
			// destination nothing holds: only that lane is unknown (an
			// overflow's room, applyOverflowRoom; a destination has no view,
			// so no cash enters it). The valued lanes' capture stands without
			// it, and its refusal never counts toward the vault's latch.
			captureAddresses = selectorValuationAddresses(v, routes, addresses)
			refreshedSlot, refreshedAccounts, refreshErr = runtime.refreshValuation(ctx, routes, captureAddresses, slot)
		}
		if refreshErr == nil {
			runtime.state.refreshFailures.Store(0)
			if err := validateRouteValuationCapture(refreshedSlot, refreshedAccounts, captureAddresses, slot); err != nil {
				return Observation{}, nil, err
			}
			if strategyReceiptIntegrityFault(accountAt(refreshedAccounts, v.strategyReceipt)) {
				return Observation{}, nil, confirmedObservationUnavailable(fmt.Errorf("valuation capture breaks the strategy receipt"))
			}
			freshHeld, freshValued, heldErr := observedLanes(v, refreshedAccounts, preferred)
			if heldErr != nil || strings.Join(freshHeld, ",") != strings.Join(held, ",") || strings.Join(freshValued, ",") != strings.Join(valued, ",") {
				return Observation{}, nil, confirmedObservationUnavailable(fmt.Errorf("valuation capture moved the held lanes"))
			}
			slot, accounts = refreshedSlot, refreshedAccounts
			if _, _, receipts, err = runtime.read(ctx, nil, slot); err != nil {
				return Observation{}, nil, err
			}
			if demand, fingerprint, err = decodeConfirmedWithdrawalDemand(v, receipts); err != nil {
				return Observation{}, nil, err
			}
		} else {
			detail := ""
			if te := holdTransactionError(refreshErr); te != "" {
				detail = " transactionError=" + te
			}
			_, _ = fmt.Fprintf(os.Stderr, "backyard-rwa-worker: reserve valuation refresh failed lane=%s: %v%s\n", first.Lane, refreshErr, detail)
			if transientValuationRefreshFailure(refreshErr) {
				return Observation{}, nil, confirmedObservationUnavailable(fmt.Errorf("reserve valuation refresh unavailable: %w", refreshErr))
			}
			// Vlad-approved 09-25 after two one-off latches (16:13, 21:26) whose
			// refresh passed on the next probe: a Kamino-rejected refresh retries
			// until refreshSimulationLatchAfter refreshes in a row fail (worker
			// ticks and selector samples share the count, so it latches sooner,
			// never later).
			if refreshSimulationFailed(refreshErr) &&
				runtime.state.refreshFailures.Add(1) < refreshSimulationLatchAfter {
				return Observation{}, nil, confirmedObservationUnavailable(fmt.Errorf("reserve valuation refresh rejected, retrying: %w", refreshErr))
			}
			refreshExhausted = true
			runtime.state.events.noteCause(holdDetail(refreshErr))
		}
	}
	views := make([]Observation, 0, len(viewLanes))
	for _, lane := range viewLanes {
		route, err := v.route(lane)
		if err != nil {
			return Observation{}, nil, err
		}
		view, hold, err := observeLaneView(ctx, manifest, runtime, slot, accounts, route, held, valued, demand, fingerprint, refreshExhausted)
		if err != nil || hold != nil {
			if hold != nil {
				return *hold, accounts, nil
			}
			return Observation{}, nil, err
		}
		views = append(views, view)
	}
	snapshots := make([]Snapshot, len(views))
	for i := range views {
		level := runtime.inputs.Levels[views[i].Snapshot.RouteLane]
		views[i].Snapshot.LeverageTargetLevel, views[i].Snapshot.LeverageUpApproved = level.Level, level.UpApproved && level.Level > 1
		snapshots[i] = views[i].Snapshot
	}
	index, laneTarget := target(snapshots, runtime.inputs)
	working := views[index]
	working.Snapshot.Target = laneTarget
	working.Snapshot.TargetInputs = targetSummary(snapshots, runtime.inputs)
	working.Snapshot.ObservationID = sha256Bytes([]byte(fmt.Sprintf("%s|target:%+v|inputs:%s", working.Snapshot.ObservationID, laneTarget, working.Snapshot.TargetInputs)))
	working.routeBatch.ObservationID = working.Snapshot.ObservationID
	working.lanes = snapshots
	return working, accounts, nil
}

func flatViewLane(preferred string, in targetInputs) string {
	if lane := selectorDestination(in); lane != "" {
		return lane
	}
	return preferred
}

func selectorDestination(in targetInputs) string {
	if r := in.Entry; r != nil && (r.Action == "ENTER" || r.Action == "CANARY_ENTER" || selectorRotates(*r)) && earnActiveLane(r.DestinationLane) {
		if _, exiting := in.Exits[r.DestinationLane]; !exiting {
			return r.DestinationLane
		}
	}
	return ""
}

func observedLanes(v *Vault, accounts []ConfirmedAccount, preferred string) ([]string, []string, error) {
	held, err := observedSelectorHeldLanes(v, accounts, earnLaneIDs(true), preferred)
	if err != nil {
		return nil, nil, err
	}
	valued, err := navLanes(v, accounts, held)
	return held, valued, err
}

func withOverflowRoutes(v *Vault, routes []RuntimeRoute) []RuntimeRoute {
	out := append([]RuntimeRoute(nil), routes...)
	for _, route := range routes {
		lane, ok := overflowLaneOf(route.Lane)
		if !ok || slices.ContainsFunc(out, func(r RuntimeRoute) bool { return r.Lane == lane }) {
			continue
		}
		if overflow, err := v.route(lane); err == nil {
			out = append(out, overflow)
		}
	}
	return out
}

func observeLaneView(ctx context.Context, manifest RouteManifest, runtime routeObservationRuntime, slot int64, accounts []ConfirmedAccount, route RuntimeRoute, held, valued []string, demand int64, fingerprint string, refreshExhausted bool) (Observation, *Observation, error) {
	v := manifest.vault
	position, err := observeKaminoFromFixedAccounts(slot, accounts, route.Kamino)
	if err != nil {
		position, err = observeKaminoWithCashFallback(v, slot, accounts, route)
	}
	healthHold := func(err error, lane string) (*Observation, error) {
		now := runtime.now()
		if hold, ok := kaminoHealthHold(runtime.state, err, slot, now, lane); ok {
			if retry := staleHealthRetry(runtime.state, hold, err, refreshExhausted, now); retry != nil {
				return nil, retry
			}
			return &hold, nil
		}
		return nil, err
	}
	if err != nil {
		hold, err := healthHold(err, route.Lane)
		return Observation{}, hold, err
	}
	routes, err := navRoutes(v, route, valued)
	if err != nil {
		return Observation{}, nil, err
	}
	navAccounts, err := selectRouteNAVAccountsForRoutes(v, accounts, routes)
	if err != nil {
		return Observation{}, nil, err
	}
	nav, err := ComputeRouteNAVForRoutes(slot, navAccounts, manifest, nil, routes)
	if err != nil {
		hold, err := healthHold(err, route.Lane)
		return Observation{}, hold, err
	}
	runtime.state.staleSince.Store(0)
	collateralMint, err := decodeBase58PublicKey(route.Kamino.CollateralMint)
	if err != nil {
		return Observation{}, nil, err
	}
	authority, err := decodeBase58PublicKey(v.squads)
	if err != nil {
		return Observation{}, nil, err
	}
	prime, err := DecodeTokenCustody(accountAt(accounts, route.CollateralCustody).Owner, accountAt(accounts, route.CollateralCustody).Data, collateralMint, authority)
	if err != nil {
		return Observation{}, nil, err
	}
	if prime.Raw > math.MaxInt64 || position.CollateralDepositedRaw > math.MaxInt64 || position.DebtRaw > math.MaxInt64 {
		return Observation{}, nil, fmt.Errorf("PRIME/USDC state exceeds signed decision range")
	}
	idle, err := decodePinnedUSDC(accountAt(accounts, v.idleATA), v.idleAuth)
	if err != nil {
		return Observation{}, nil, err
	}
	strategy, err := decodePinnedUSDC(accountAt(accounts, v.strategyATA), v.strategyAuth)
	if err != nil {
		return Observation{}, nil, err
	}
	squads, err := decodePinnedUSDC(accountAt(accounts, v.cash), v.squads)
	if err != nil {
		return Observation{}, nil, err
	}
	var ticket observedReportTicket
	if !v.trustful() {
		var ticketErr error
		if ticket, ticketErr = decodeObservedReportTicket(v, accountAt(accounts, reportTicketPDA)); ticketErr != nil {
			return Observation{}, nil, fmt.Errorf("decode report ticket: %w", ticketErr)
		}
	}
	if nav.Custodies.VoltrIdleRaw != idle.Raw || nav.Custodies.StrategyUSDCraw != strategy.Raw || nav.Custodies.SquadsUSDCraw != squads.Raw || nav.Custodies.SquadsPRIMEraw != prime.Raw {
		return Observation{}, nil, fmt.Errorf("route NAV custody differs inside fixed confirmed account batch")
	}
	if idle.Raw > math.MaxInt64 || strategy.Raw > math.MaxInt64 || squads.Raw > math.MaxInt64 {
		return Observation{}, nil, fmt.Errorf("bridge custody exceeds signed decision range")
	}
	ltv, err := observedLTVBPS(position)
	if err != nil {
		return Observation{}, nil, err
	}
	stateHash := sha256.Sum256([]byte(fmt.Sprintf("%s|voltr-idle:%d|strategy-idle:%d|squads-idle:%d", fingerprint, idle.Raw, strategy.Raw, squads.Raw)))
	base := Observation{ObservedAt: runtime.now(), Snapshot: Snapshot{ObservationID: fmt.Sprintf("%x", stateHash[:]), Slot: slot, RouteKind: RouteKind, Fresh: true, WithdrawalDemandRaw: demand, VoltrIdleRaw: int64(idle.Raw), VoltrStrategyIdleRaw: int64(strategy.Raw), SquadsIdleRaw: int64(squads.Raw), Trustful: v.trustful(), ApprovedAdminPerformanceFeeBPS: v.AdminPerformanceFeeBPS}}
	base.Snapshot.PrimeIdleRaw = int64(prime.Raw)
	base.Snapshot.CollateralIdleRaw = int64(prime.Raw)
	if (route.Kamino.DebtMint != bridgeUSDC || basicLane(route.Lane)) && prime.Raw > 0 && withdrawalShortfallRaw(base.Snapshot) == 0 {
		minimum, err := kaminoDepositMinimum(accounts, route, slot, math.MaxInt64)
		if err != nil && !basicLane(route.Lane) {
			return Observation{}, nil, err
		}
		// A missing pilot entry bound must not suppress accounting or a
		// risk/unwind observation. Zero explicitly holds only new deposits.
		if err == nil {
			base.Snapshot.MinimumCollateralDepositRaw = math.MaxInt64 - int64(minimum) + 1
		}
	}
	base.Snapshot.RouteLane = route.Lane
	base.Snapshot.StrategyKey = route.Lane
	base.Snapshot.HeldLanes = strings.Join(held, ",")
	base.Snapshot.TicketLastConsumedSequenceRaw = int64(ticket.LastConsumedSequence)
	base.Snapshot.HasPosition = position.HasPosition
	// The position view and the NAV view decode the obligation independently
	// out of the same confirmed batch; they must agree on whether the account
	// even exists. An absent obligation is carried forward explicitly instead
	// of being silently folded into a flat position.
	if position.Obligation != nav.Obligation {
		return Observation{}, nil, fmt.Errorf("route NAV and position disagree on the obligation account")
	}
	base.Snapshot.Obligation = position.Obligation
	base.Snapshot.PositionCollateralRaw = int64(position.CollateralDepositedRaw)
	base.Snapshot.PositionDebtRaw = int64(position.DebtRaw)
	if earnHeldLane(route.Lane) && position.DebtRaw > 0 {
		bound, err := decodeKaminoPayoffWindow(accounts, route, slot, kaminoPayoffSteps)
		if err != nil {
			return Observation{}, nil, err
		}
		if bound.UpperDebtRaw > math.MaxInt64 {
			return Observation{}, nil, fmt.Errorf("payoff bound exceeds decision range")
		}
		base.Snapshot.PayoffDebtRaw = int64(bound.UpperDebtRaw)
	}
	base.Snapshot.PositionCollateralValueRaw = int64(nav.PositionCollateralValue)
	base.Snapshot.PositionDebtValueRaw = int64(nav.PositionDebtValue)
	base.Snapshot.StrategyNAVRaw = int64(nav.StrategyNAVRaw)
	base.Snapshot.LTVBPS = ltv
	base.Snapshot.LiquidationThresholdBPS, base.Snapshot.MaxLTVBPS = position.LiquidationThresholdBPS, position.MaxLTVBPS
	entryUSDC, err := routeEntryCapacityUSDC(position, accounts, route)
	if err != nil {
		return Observation{}, nil, err
	}
	if entryUSDC > math.MaxInt64 {
		return Observation{}, nil, fmt.Errorf("PRIME/USDC entry capacity exceeds signed decision range")
	}
	room, err := topupDepositRoomUSDC(accounts, route)
	if err != nil {
		return Observation{}, nil, err
	}
	// B2 1x entry: while the lane's debt reserve blocks borrowing, an
	// A B2 lane entry is a debt-free deposit, bounded by collateral
	// deposit room instead of the (zero) leveraged pair capacity. Never on
	// the cash-only fallback, which has no reserve prices (entry closed).
	// The observation ID keeps the pair capacity, so price-driven room
	// changes do not churn it.
	pairEntryUSDC := entryUSDC
	if entryUSDC == 0 && earnActiveLane(route.Lane) && position.BorrowUtilizationBlocked && position.LiquidationThresholdBPS > 0 && littleInt(position.CollateralPriceSF[:]).Sign() > 0 {
		entryUSDC = room
	}
	if earnActiveLane(route.Lane) && position.LiquidationThresholdBPS > 0 && littleInt(position.CollateralPriceSF[:]).Sign() > 0 {
		entryUSDC = room
		applyBorrowCapacity(&base.Snapshot, position, accounts, route)
	}
	if err := applyDebtTerm(&base.Snapshot, accounts, route); err != nil {
		return Observation{}, nil, err
	}
	base.Snapshot.TopupDepositRoomRaw = int64(room)
	base.Snapshot.CapacityRaw = int64(entryUSDC)
	base.Snapshot.MaxTargetLTVEntryRaw = int64(entryUSDC)
	base.Snapshot.BorrowUtilizationBlocked = position.BorrowUtilizationBlocked
	if earnActiveLane(route.Lane) && position.LiquidationThresholdBPS > 0 && littleInt(position.CollateralPriceSF[:]).Sign() > 0 {
		if err := applyOverflowRoom(v, &base.Snapshot, accounts, route); err != nil {
			fmt.Fprintf(os.Stderr, "backyard-rwa-worker: overflow room unknown lane=%s: %v\n", route.Lane, err)
		}
	}
	base.Snapshot.PolicyLimitRaw = int64(strategyTwoBridgeLegCapRaw)
	observedAt := runtime.now()
	if err := applyRouteNAVSnapshot(&base.Snapshot, nav, observedAt); err != nil {
		return Observation{}, nil, err
	}
	base.Snapshot.ObservationID = routeEconomicObservationID(
		base.Snapshot.ObservationID, prime.Raw, position.CollateralDepositedRaw, position.DebtRaw,
		position.BorrowUtilizationBlocked,
		nav.StrategyNAVRaw, nav.PriorReportedNAVRaw, pairEntryUSDC,
	)
	if route.Kamino.DebtMint != bridgeUSDC || basicLane(route.Lane) {
		digest := sha256.Sum256([]byte(fmt.Sprintf("%s|lane:%s|idle-debt:%d|payoff-debt:%d|idle-collateral-value:%d|position-debt-value:%d|minimum-deposit:%d", base.Snapshot.ObservationID, route.Lane, nav.Custodies.SquadsDebtRaw, base.Snapshot.PayoffDebtRaw, base.Snapshot.CollateralIdleValueRaw, base.Snapshot.PositionDebtValueRaw, base.Snapshot.MinimumCollateralDepositRaw)))
		base.Snapshot.ObservationID = fmt.Sprintf("%x", digest[:])
	}
	if nav.OtherLanesState != "" {
		digest := sha256.Sum256([]byte(fmt.Sprintf("%s|held:%s|other-lanes:%s", base.Snapshot.ObservationID, base.Snapshot.HeldLanes, nav.OtherLanesState)))
		base.Snapshot.ObservationID = fmt.Sprintf("%x", digest[:])
	}
	base.ObservedAt = observedAt
	base.ValuationSource, base.ValuationSlot = "confirmed", slot
	if len(accounts) > 0 && accounts[0].ValuationSource != "" {
		base.ValuationSource, base.ValuationSlot = accounts[0].ValuationSource, accounts[0].ValuationSlot
	}
	base.Snapshot.ValuationSource, base.Snapshot.ValuationSlot = base.ValuationSource, base.ValuationSlot
	if base.ValuationSource != "confirmed" {
		base.Snapshot.ObservationID = sha256Bytes([]byte(fmt.Sprintf("%s|valuation:%s|liquidation:%d", base.Snapshot.ObservationID, base.ValuationSource, base.Snapshot.LiquidationThresholdBPS)))
	}
	base.routeBatch = &routeObservationBatch{Slot: slot, ObservationID: base.Snapshot.ObservationID, ManifestSHA256: manifest.SHA256, Accounts: accounts}
	return base, nil, nil
}

func receiptIntegrityObservation(slot int64, route RuntimeRoute) Observation {
	fingerprint := sha256.Sum256([]byte(fmt.Sprintf("strategy-receipt-integrity|%s|%d", route.Lane, slot)))
	return Observation{ObservedAt: time.Now().UTC(), Snapshot: Snapshot{
		ObservationID: fmt.Sprintf("%x", fingerprint[:]), Slot: slot, RouteKind: RouteKind, Fresh: true,
		RouteLane: route.Lane, StrategyKey: route.Lane, MonitorsArmed: true,
		StrategyReceiptIntegrityFault: true,
	}}
}

func routeFixedAddresses(v *Vault) []string {
	addresses := []string{reportTicketPDA, budgetClockAddress}
	for _, r := range v.routes {
		addresses = append(append(addresses, pinnedRouteNAVAddressesForRoute(v, r)...), r.Kamino.Market, r.Kamino.CollateralReserve, r.Kamino.DebtReserve,
			r.CollateralLiquiditySupply, r.DebtLiquiditySupply, r.DebtFeeReceiver, r.Kamino.Obligation, r.CollateralCustody)
	}
	addresses = uniqueNonzero(addresses)
	sort.Strings(addresses)
	return addresses
}

func observeKaminoWithCashFallback(v *Vault, slot int64, accounts []ConfirmedAccount, route RuntimeRoute) (KaminoPosition, error) {
	position, originalErr := observeKaminoFromFixedAccounts(slot, accounts, route.Kamino)
	if originalErr == nil {
		return position, nil
	}
	if _, health := kaminoHealthReason(originalErr); !health {
		return KaminoPosition{}, originalErr
	}
	custodies, err := decodeRouteNAVCustodiesForRoute(v, accounts, route)
	if err != nil {
		return KaminoPosition{}, err
	}
	if custodies.SquadsPRIMEraw != 0 || custodies.SquadsDebtRaw != 0 {
		return KaminoPosition{}, originalErr
	}
	account := accountAt(accounts, route.Kamino.Obligation)
	if account.Address != route.Kamino.Obligation {
		return KaminoPosition{}, fmt.Errorf("cash-only obligation observation missing")
	}
	obligation := decodedKaminoObligation{}
	if account.Lamports != 0 {
		obligation, err = decodeKaminoObligation(account, route.Kamino)
		if err != nil {
			return KaminoPosition{}, err
		}
	}
	if obligation.hasPosition || obligation.refreshedSlot > slot {
		return KaminoPosition{}, originalErr
	}
	for _, binding := range [][2]string{{route.Kamino.CollateralReserve, route.Kamino.CollateralMint}, {route.Kamino.DebtReserve, route.Kamino.DebtMint}} {
		reserve, err := decodeKaminoReserve(accountAt(accounts, binding[0]), binding[1], route.Kamino)
		if err != nil {
			return KaminoPosition{}, err
		}
		if reserve.refreshedSlot > slot {
			return KaminoPosition{}, originalErr
		}
	}
	return KaminoPosition{Slot: slot, RefreshedSlot: obligation.refreshedSlot, Obligation: observedObligation(account.Lamports != 0), BorrowUtilizationBlocked: true}, nil
}

func observeKaminoFromFixedAccounts(slot int64, accounts []ConfirmedAccount, config KaminoObservationConfig) (KaminoPosition, error) {
	obligationAccount := accountAt(accounts, config.Obligation)
	obligation := decodedKaminoObligation{}
	var err error
	if obligationAccount.Lamports != 0 {
		obligation, err = decodeKaminoObligation(obligationAccount, config)
		if err != nil {
			return KaminoPosition{}, err
		}
	}
	collateral, err := decodeKaminoReserve(accountAt(accounts, config.CollateralReserve), config.CollateralMint, config)
	if err != nil {
		return KaminoPosition{}, err
	}
	debt, err := decodeKaminoReserve(accountAt(accounts, config.DebtReserve), config.DebtMint, config)
	if err != nil {
		return KaminoPosition{}, err
	}
	marketEmergency, err := decodeKaminoMarketEmergency(accountAt(accounts, config.Market), config)
	if err != nil {
		return KaminoPosition{}, err
	}
	if err := validateKaminoReserveHealth(slot, marketEmergency, obligation, collateral, debt); err != nil {
		return KaminoPosition{}, err
	}
	observedUnix := clockUnixTimestamp(accounts)
	if observedUnix <= 0 {
		return KaminoPosition{}, fmt.Errorf("confirmed batch has no usable Clock sysvar")
	}
	if err := validateKaminoOracleAge(observedUnix, collateral, debt); err != nil {
		return KaminoPosition{}, err
	}
	oracles := uniqueNonzero(append(collateral.oracles, debt.oracles...))
	if len(oracles) == 0 {
		return KaminoPosition{}, fmt.Errorf("Kamino reserve has no configured oracle")
	}
	redeemable, err := collateral.redeemLiquidityRaw(obligation.collateralDepositedRaw)
	if err != nil {
		return KaminoPosition{}, err
	}
	capacity, err := entryCapacityDebtRaw(collateral, debt)
	if err != nil {
		return KaminoPosition{}, err
	}
	borrowUtilizationBlocked, err := borrowingBlockedByUtilization(debt)
	if err != nil {
		return KaminoPosition{}, err
	}
	debtRaw, err := obligation.debtAtReserveRate(debt)
	if err != nil {
		return KaminoPosition{}, err
	}
	return KaminoPosition{Slot: slot, RefreshedSlot: obligation.refreshedSlot, HasPosition: obligation.hasPosition, Obligation: observedObligation(obligationAccount.Lamports != 0), CollateralDepositedRaw: obligation.collateralDepositedRaw, DebtRaw: debtRaw, RedeemablePrimeRaw: redeemable, CollateralPriceSF: collateral.marketPriceSF, DebtPriceSF: debt.marketPriceSF, CollateralDecimals: collateral.mintDecimals, DebtDecimals: debt.mintDecimals, Oracles: oracles, LiquidationThresholdBPS: int64(collateral.liquidationThresholdPct) * 100, MaxLTVBPS: cmp.Or(obligation.borrowLimitBPS, int64(collateral.loanToValuePct)*100), EntryCapacityRaw: capacity, BorrowUtilizationBlocked: borrowUtilizationBlocked}, nil
}

func routeEntryCapacityUSDC(position KaminoPosition, accounts []ConfirmedAccount, route RuntimeRoute) (uint64, error) {
	if earnActiveLane(route.Lane) && position.LiquidationThresholdBPS > 0 {
		return topupDepositRoomUSDC(accounts, route)
	}
	if route.Kamino.DebtMint == bridgeUSDC {
		if basicLane(route.Lane) {
			return kaminoPairEntryCapacity(position, accounts, route)
		}
		return position.EntryCapacityRaw, nil
	}
	reference := usdcReference
	usdc, err := decodeKaminoReserve(accountAt(accounts, reference.DebtReserve), bridgeUSDC, reference)
	if err != nil {
		return 0, err
	}
	if usdc.mintDecimals != 6 || usdc.refreshedSlot > position.Slot {
		return 0, fmt.Errorf("entry USDC reference drifted")
	}
	return valueBetweenTokenRaw(position.EntryCapacityRaw, position.DebtDecimals, 6, position.DebtPriceSF, usdc.marketPriceSF, false)
}

// topupDepositRoomUSDC values the collateral reserve's remaining deposit limit
// in bridge USDC from the same confirmed batch, floored and less a 1% price
// margin, so a plan B3 top-up never sizes a deposit the reserve refuses.
func topupDepositRoomUSDC(accounts []ConfirmedAccount, route RuntimeRoute) (uint64, error) {
	collateral, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil || collateral.totalLiquiditySF == nil || collateral.depositLimitRaw == 0 {
		return 0, err
	}
	deposited, err := ceilScaledBigFraction(collateral.totalLiquiditySF)
	if err != nil || deposited >= collateral.depositLimitRaw {
		return 0, err
	}
	usdcReserve, usdcConfig := route.Kamino.DebtReserve, route.Kamino
	if route.Kamino.DebtMint != bridgeUSDC {
		usdcConfig = usdcReference
		usdcReserve = usdcConfig.DebtReserve
	}
	usdc, err := decodeKaminoReserve(accountAt(accounts, usdcReserve), bridgeUSDC, usdcConfig)
	if err != nil || usdc.mintDecimals != 6 {
		return 0, fmt.Errorf("top-up USDC reference unavailable")
	}
	room, err := valueBetweenTokenRaw(collateral.depositLimitRaw-deposited, collateral.mintDecimals, 6, collateral.marketPriceSF, usdc.marketPriceSF, false)
	if err != nil {
		return 0, err
	}
	return min(room/100*99, math.MaxInt64), nil
}

// routeEconomicObservationID deliberately excludes Slot, the stateless adaptor
// report sequence (which equals Slot), and the slot-bearing NAV digest. A later
// confirmed read of unchanged money state must remain the same decision input;
// the durable operation journal supplies the lifecycle epoch for actionable
// idempotency.
func routeEconomicObservationID(
	bridgeObservationID string,
	primeRaw, collateralRaw, debtRaw uint64,
	borrowUtilizationBlocked bool,
	strategyNAVRaw, priorReportedNAVRaw, capacityRaw uint64,
) string {
	stateHash := sha256.Sum256([]byte(fmt.Sprintf(
		"%s|prime:%d|collateral:%d|debt:%d|borrow-utilization-blocked:%t|strategy-nav:%d|reported-nav:%d|capacity:%d",
		bridgeObservationID, primeRaw, collateralRaw, debtRaw,
		borrowUtilizationBlocked, strategyNAVRaw, priorReportedNAVRaw, capacityRaw,
	)))
	return fmt.Sprintf("%x", stateHash[:])
}

func applyRouteNAVSnapshot(snapshot *Snapshot, nav RouteNAVSnapshot, now time.Time) error {
	fees, supply, err := nav.Voltr.LPTotalsRaw(nav.LPSupplyRaw)
	if err != nil {
		return err
	}
	if snapshot == nil || snapshot.Slot <= 0 || nav.Slot != snapshot.Slot || now.IsZero() || now.Unix() < 0 ||
		nav.StrategyNAVRaw > math.MaxInt64 || nav.TotalVaultNAVRaw > math.MaxInt64 || nav.PriorReportedNAVRaw > math.MaxInt64 ||
		nav.Report.Sequence > math.MaxInt64 || nav.Custodies.SquadsDebtRaw > math.MaxInt64 || nav.PrimeIdleValueRaw > math.MaxInt64 ||
		nav.PriorReportUpdatedTS > math.MaxInt64 || nav.Report.Sequence != nav.Report.ObservedSlot ||
		nav.Report.ObservedSlot != uint64(nav.Slot) || nav.Report.NAVAfterRaw != nav.StrategyNAVRaw ||
		nav.Voltr.TotalValueRaw > math.MaxInt64 || nav.Receipt.CustodyTrackedRaw > math.MaxInt64 ||
		nav.Voltr.LockedProfitDegradationSeconds > math.MaxInt64 || nav.Voltr.LastUpdatedLockedProfitRaw > math.MaxInt64 ||
		nav.Voltr.LastLockedProfitReportUnix > math.MaxInt64 ||
		nav.Report.SnapshotDigest != nav.SnapshotDigest || !sha256Pattern.MatchString(nav.SnapshotDigest) {
		return fmt.Errorf("route NAV cannot be merged into the confirmed snapshot")
	}
	currentUnix := now.Unix()
	lastUpdated := int64(nav.PriorReportUpdatedTS)
	age := currentUnix - lastUpdated
	if lastUpdated > currentUnix {
		age = 0
	}
	snapshot.LastReportAgeSeconds = age
	snapshot.TotalVaultNAVRaw = int64(nav.TotalVaultNAVRaw)
	snapshot.PriorReportedNAVRaw = int64(nav.PriorReportedNAVRaw)
	snapshot.PriorReportUpdatedUnix = lastUpdated
	snapshot.ReportSequence = int64(nav.Report.Sequence)
	snapshot.ReportSnapshotDigest = nav.Report.SnapshotDigest
	snapshot.DebtIdleRaw = int64(nav.Custodies.SquadsDebtRaw)
	snapshot.CollateralIdleValueRaw = int64(nav.PrimeIdleValueRaw)
	snapshot.VoltrTotalValueRaw = int64(nav.Voltr.TotalValueRaw)
	snapshot.VoltrReceiptCustodyTrackedRaw = int64(nav.Receipt.CustodyTrackedRaw)
	snapshot.LockedProfitDegradationSeconds = int64(nav.Voltr.LockedProfitDegradationSeconds)
	snapshot.LastUpdatedLockedProfitRaw = int64(nav.Voltr.LastUpdatedLockedProfitRaw)
	snapshot.LastLockedProfitReportUnix = int64(nav.Voltr.LastLockedProfitReportUnix)
	snapshot.VoltrHighWaterMarkBits = nav.Voltr.HighWaterMarkBits
	snapshot.FeeAccumulatorRaw = int64(fees)
	snapshot.LPSupplyInclFeesRaw = int64(supply)
	snapshot.ManagerPerformanceFeeBPS = int64(nav.Voltr.ManagerPerformanceFeeBPS)
	snapshot.AdminPerformanceFeeBPS = int64(nav.Voltr.AdminPerformanceFeeBPS)
	snapshot.ManagerManagementFeeBPS = int64(nav.Voltr.ManagerManagementFeeBPS)
	snapshot.AdminManagementFeeBPS = int64(nav.Voltr.AdminManagementFeeBPS)
	snapshot.RedemptionFeeBPS = int64(nav.Voltr.RedemptionFeeBPS)
	snapshot.IssuanceFeeBPS = int64(nav.Voltr.IssuanceFeeBPS)
	snapshot.ProtocolPerformanceFeeBPS = int64(nav.Voltr.ProtocolPerformanceFeeBPS)
	snapshot.ProtocolManagementFeeBPS = int64(nav.Voltr.ProtocolManagementFeeBPS)
	snapshot.MonitorsArmed = true
	return nil
}

func observedLTVBPS(position KaminoPosition) (int64, error) {
	if position.CollateralDecimals > 18 || position.DebtDecimals > 18 {
		return 0, fmt.Errorf("Kamino LTV decimals exceed supported scale")
	}
	if position.DebtRaw == 0 {
		return 0, nil
	}
	if position.RedeemablePrimeRaw == 0 {
		return 0, fmt.Errorf("Kamino debt has no redeemable collateral")
	}
	collateral := new(big.Int).Mul(new(big.Int).SetUint64(position.RedeemablePrimeRaw), littleInt(position.CollateralPriceSF[:]))
	debt := new(big.Int).Mul(new(big.Int).SetUint64(position.DebtRaw), littleInt(position.DebtPriceSF[:]))
	// Prices are per token, not per raw unit. Cross-multiply the decimal
	// scales before division; otherwise a 9/6 lane understates LTV 1,000x.
	debt.Mul(debt, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(position.CollateralDecimals)), nil))
	collateral.Mul(collateral, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(position.DebtDecimals)), nil))
	if collateral.Sign() <= 0 || debt.Sign() <= 0 {
		return 0, fmt.Errorf("Kamino LTV price is zero")
	}
	debt.Mul(debt, big.NewInt(10_000))
	debt.Add(debt, new(big.Int).Sub(collateral, big.NewInt(1))).Div(debt, collateral)
	if !debt.IsInt64() || debt.Int64() > 10_000 {
		return 0, fmt.Errorf("Kamino LTV is outside bounded range")
	}
	return debt.Int64(), nil
}

func kaminoHealthHold(state *vaultState, err error, slot int64, now time.Time, lane string) (Observation, bool) {
	hold, ok := KaminoHealthHoldObservation(err, slot, now)
	if ok {
		_, _ = fmt.Fprintf(os.Stderr, "backyard-rwa-worker: kamino health hold lane=%s: %v\n", lane, err)
		state.events.noteCause(err.Error())
	}
	return hold, ok
}

const refreshSimulationLatchAfter = 3

const kaminoStaleLatchWindow = 15 * time.Minute

func staleHealthRetry(state *vaultState, hold Observation, err error, refreshExhausted bool, now time.Time) error {
	if hold.Snapshot.ManualReason != "kamino_stale" || refreshExhausted {
		return nil
	}
	since := now.UnixNano()
	if !state.staleSince.CompareAndSwap(0, since) {
		if v := state.staleSince.Load(); v != 0 {
			since = v
		}
	}
	if now.Sub(time.Unix(0, since)) >= kaminoStaleLatchWindow {
		return nil
	}
	return confirmedObservationUnavailable(fmt.Errorf("kamino health hold, retrying: %w", err))
}

func refreshSimulationFailed(err error) bool {
	var hold *BudgetHold
	return errors.As(err, &hold) && holdCause(hold) == "price_refresh_simulation_failed"
}

func transientValuationRefreshFailure(err error) bool {
	var hold *BudgetHold
	if errors.As(err, &hold) {
		if holdCause(hold) == "price_refresh_simulation_unavailable" {
			return true
		}
		// BlockhashNotFound: the simulating RPC node did not know our fresh
		// blockhash, so Kamino never evaluated the refresh.
		return holdCause(hold) == "price_refresh_simulation_failed" && strings.Trim(holdTransactionError(err), `"`) == "BlockhashNotFound"
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errConfirmedObservationUnavailable)
}
