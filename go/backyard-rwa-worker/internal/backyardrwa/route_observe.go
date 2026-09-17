package backyardrwa

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"time"
)

// ObserveConfirmedRouteSnapshot extends the bridge snapshot with the fixed
// PRIME/USDC position, PRIME custody, and exact installed policy bytes. A
// manifest entry alone never makes a route ready: every referenced policy is
// read at the same confirmed slot and matched by owner and data hash.
func ObserveConfirmedRouteSnapshot(ctx context.Context, rpc *RPCClient, manifest RouteManifest) (Observation, error) {
	observation, _, err := observeConfirmedRouteSnapshotWithRPCAccounts(ctx, rpc, manifest)
	return observation, err
}

func observeConfirmedRouteSnapshotWithRPCAccounts(ctx context.Context, rpc *RPCClient, manifest RouteManifest) (Observation, []ConfirmedAccount, error) {
	if rpc == nil {
		return Observation{}, nil, fmt.Errorf("RPC client is required")
	}
	return observeConfirmedRouteSnapshotWithAccounts(ctx, manifest, routeObservationRuntime{
		confirmedSlot: rpc.ConfirmedSlot,
		receipts: func(ctx context.Context, minSlot int64) (int64, []programAccount, error) {
			return rpc.getVoltrWithdrawalReceiptAccounts(ctx, bridgeVoltrProgram, bridgeVoltrVault, minSlot)
		},
		accounts: func(ctx context.Context, addresses []string, minSlot int64) (int64, []ConfirmedAccount, error) {
			optional := optionalLifecycleObligations(addresses)
			for _, candidate := range addresses {
				// A null strategy receipt must reach the integrity classifier
				// instead of failing the batch as a required absent account.
				if candidate == bridgeStrategyReceipt {
					optional = append(optional, candidate)
					break
				}
			}
			if len(optional) > 0 {
				return rpc.GetMultipleAccountsWithOptional(ctx, addresses, minSlot, optional...)
			}
			return rpc.GetMultipleAccounts(ctx, addresses, minSlot)
		},
		refreshValuation: rpc.simulateRouteValuationRefresh,
		finalizedReceipt: rpc.strategyReceiptFinalized,
		now:              func() time.Time { return time.Now().UTC() },
	})
}

// observeConfirmedRouteSnapshotWithRPCAccountsAndEnrichment is the construction
// refresh seam. It requires the same journal and verified-identity merge as the
// outer production observation before any monitor sees this snapshot.
func observeConfirmedRouteSnapshotWithRPCAccountsAndEnrichment(
	ctx context.Context,
	rpc *RPCClient,
	manifest RouteManifest,
	enrich func(context.Context, *Observation) error,
) (Observation, []ConfirmedAccount, error) {
	if enrich == nil {
		return Observation{}, nil, fmt.Errorf("route observation enrichment is required")
	}
	observation, accounts, err := observeConfirmedRouteSnapshotWithRPCAccounts(ctx, rpc, manifest)
	if err != nil {
		return observation, accounts, err
	}
	if enrich != nil {
		if err := enrich(ctx, &observation); err != nil {
			return Observation{}, nil, err
		}
	}
	return observation, accounts, nil
}

func applyProgramIdentityObservation(observation *Observation, identity programIdentityObservation) {
	if observation == nil {
		return
	}
	observation.Snapshot.ProgramIdentityKnown = identity.Verified
	observation.Snapshot.VoltrProgramDeploySlot = identity.VoltrProgramDeploySlot
	observation.Snapshot.AdaptorProgramDeploySlot = identity.AdaptorProgramDeploySlot
}

// A full K-Lend withdrawal closes its obligation account. Prefer the selected
// Phase 2 obligation when both route families are observed so terminal custody
// swaps can continue after the close; retain the Phase 1 fallback for its own
// zero-position lifecycle.
func optionalLifecycleObligations(addresses []string) []string {
	selected := mapleSyrupUSDCUSDC.Kamino.Obligation
	optional := make([]string, 0, 2)
	for _, candidate := range addresses {
		if candidate == selected {
			optional = append(optional, selected)
			break
		}
	}
	for _, candidate := range addresses {
		if candidate == kaminoPrimeUSDCObligation {
			optional = append(optional, kaminoPrimeUSDCObligation)
			break
		}
	}
	for _, obligation := range []string{"4LnCFir7Qc99GhjGHLcwtkfweyAMu37u5QE1zTupKsei", autoAUTOPYUSD.Kamino.Obligation, ethenaUSDePYUSD.Kamino.Obligation, primePRIMEPYUSD.Kamino.Obligation, primePRIMEUSDS.Kamino.Obligation} {
		for _, candidate := range addresses {
			if candidate == obligation {
				optional = append(optional, obligation)
				break
			}
		}
	}
	return optional
}

type routeObservationRuntime struct {
	refreshValuation func(context.Context, RuntimeRoute, []string, int64) (int64, []ConfirmedAccount, error)
	confirmedSlot    func(context.Context) (int64, error)
	receipts         func(context.Context, int64) (int64, []programAccount, error)
	accounts         func(context.Context, []string, int64) (int64, []ConfirmedAccount, error)
	finalizedReceipt func(context.Context, int64) (int64, []ConfirmedAccount, error)
	now              func() time.Time
}

func observeConfirmedRouteSnapshot(ctx context.Context, manifest RouteManifest, runtime routeObservationRuntime) (Observation, error) {
	observation, _, err := observeConfirmedRouteSnapshotWithAccounts(ctx, manifest, runtime)
	return observation, err
}

func observeConfirmedRouteSnapshotWithAccounts(ctx context.Context, manifest RouteManifest, runtime routeObservationRuntime) (Observation, []ConfirmedAccount, error) {
	if runtime.confirmedSlot == nil || runtime.receipts == nil || runtime.accounts == nil || runtime.finalizedReceipt == nil || runtime.now == nil {
		return Observation{}, nil, fmt.Errorf("route observation runtime is incomplete")
	}
	minimumSlot, err := runtime.confirmedSlot(ctx)
	if err != nil {
		return Observation{}, nil, err
	}
	selectedRoute, err := manifest.activeRuntimeRoute()
	if err != nil {
		return Observation{}, nil, err
	}
	addresses := routeFixedAddresses(manifest)
	for attempt := 0; attempt < maxConfirmedObservationAttempts; attempt++ {
		route := selectedRoute
		beforeSlot, beforeReceipts, err := runtime.receipts(ctx, minimumSlot)
		if err != nil {
			return Observation{}, nil, err
		}
		beforeDemand, beforeFingerprint, err := decodeConfirmedWithdrawalDemand(beforeReceipts)
		if err != nil {
			return Observation{}, nil, err
		}
		slot, accounts, err := runtime.accounts(ctx, addresses, beforeSlot)
		if err != nil {
			return Observation{}, nil, err
		}
		// A confirmed batch whose strategy receipt is foreign-owned or the wrong
		// length is an observed integrity failure, not a transport fault. A null
		// receipt only becomes one at finalized commitment: at confirmed
		// commitment it can be a replication artifact and stays a retryable tick
		// error. Transport failures above stay errors.
		receipt := accountAt(accounts, bridgeStrategyReceipt)
		if strategyReceiptIntegrityFault(receipt) {
			if strategyReceiptAbsent(receipt) {
				finalSlot, finalReceipts, err := runtime.finalizedReceipt(ctx, slot)
				if err != nil {
					return Observation{}, nil, err
				}
				if finalSlot < slot {
					return Observation{}, nil, fmt.Errorf("finalized strategy receipt read predates the confirmed batch")
				}
				if !strategyReceiptAbsent(accountAt(finalReceipts, bridgeStrategyReceipt)) {
					return Observation{}, nil, fmt.Errorf("strategy receipt absent at confirmed commitment but present at finalized commitment")
				}
			}
			integrity := receiptIntegrityObservation(slot, route)
			return integrity, accounts, nil
		}
		cutoverDrain := false
		if manifest.selectorObservation {
			route, err = observedSelectorRoute(accounts, selectedRoute.Lane)
			if err != nil {
				return Observation{ObservedAt: runtime.now(), Snapshot: Snapshot{ObservationID: sha256Bytes([]byte(fmt.Sprintf("selector-ownership:%d:%s", slot, err.Error()))), Slot: slot, RouteKind: RouteKind, RouteLane: selectedRoute.Lane, ManualReason: err.Error()}}, accounts, nil
			}
		} else if route.Lane == SelectedRouteID {
			legacyRoute, legacyErr := runtimeRoute(RouteID)
			if legacyErr != nil {
				return Observation{}, nil, legacyErr
			}
			legacyPosition, legacyErr := observeKaminoWithCashFallback(ctx, runtime.accounts, slot, accounts, legacyRoute)
			if legacyErr != nil {
				return Observation{}, nil, fmt.Errorf("verify legacy PRIME cutover state: %w", legacyErr)
			}
			legacyCustody, legacyErr := decodePinnedPrime(accountAt(accounts, kaminoPrimeCustody))
			if legacyErr != nil {
				return Observation{}, nil, fmt.Errorf("verify legacy PRIME custody: %w", legacyErr)
			}
			if legacyPrimeExposure(legacyPosition, legacyCustody.Raw) {
				route, legacyErr = runtimeRoute(RouteID)
				if legacyErr != nil {
					return Observation{}, nil, legacyErr
				}
				cutoverDrain = true
			}
		}
		// Reserve refresh changes valuation inputs only. Capture the complete
		// bank again so custody, obligations, receipts and Clock are coherent
		// with those refreshed inputs, including after a landed swap.
		position, reserveErr := observeKaminoFromFixedAccounts(ctx, runtime.accounts, slot, accounts, route.Kamino)
		if errors.Is(reserveErr, errKaminoReserveStale) && runtime.refreshValuation != nil {
			captureAddresses := addresses
			if manifest.selectorObservation {
				captureAddresses = selectorValuationPolicyAddresses(manifest, route, selectorValuationAddresses(route, addresses))
			}
			refreshedSlot, refreshedAccounts, refreshErr := runtime.refreshValuation(ctx, route, captureAddresses, slot)
			if refreshErr == nil {
				if err := validateRouteValuationCapture(refreshedSlot, refreshedAccounts, captureAddresses, slot); err != nil {
					return Observation{}, nil, err
				}
				if strategyReceiptIntegrityFault(accountAt(refreshedAccounts, bridgeStrategyReceipt)) {
					// Restart through the established confirmed/finalized receipt
					// classifier rather than promoting a simulated absence.
					minimumSlot = refreshedSlot
					continue
				}
				if manifest.selectorObservation {
					freshRoute, routeErr := observedSelectorRoute(refreshedAccounts, selectedRoute.Lane)
					if routeErr != nil || freshRoute.Lane != route.Lane {
						minimumSlot = refreshedSlot
						continue
					}
				}
				slot, accounts = refreshedSlot, refreshedAccounts
			}
			// On unavailable refresh, cash-only accounting still works. Any
			// noncash exposure retains the original fail-closed health hold.
		}
		err = reserveErr
		if err != nil {
			position, err = observeKaminoWithCashFallback(ctx, runtime.accounts, slot, accounts, route)
		}
		if err != nil {
			// A stale, paused, or emergency Kamino state is a decision input,
			// not a broken observer: the tick holds with the audited reason.
			if hold, ok := KaminoHealthHoldObservation(err, slot, runtime.now()); ok {
				return hold, accounts, nil
			}
			return Observation{}, nil, err
		}
		afterSlot, afterReceipts, err := runtime.receipts(ctx, slot)
		if err != nil {
			return Observation{}, nil, err
		}
		afterDemand, afterFingerprint, err := decodeConfirmedWithdrawalDemand(afterReceipts)
		if err != nil {
			return Observation{}, nil, err
		}
		if !stableReceiptFence(beforeSlot, slot, afterSlot, beforeDemand, afterDemand, beforeFingerprint, afterFingerprint) {
			minimumSlot = maxSlot(beforeSlot, maxSlot(slot, afterSlot))
			continue
		}
		navAccounts, err := selectRouteNAVAccountsForRoute(accounts, route)
		if err != nil {
			return Observation{}, nil, err
		}
		nav, err := ComputeRouteNAVForRoute(slot, navAccounts, manifest, nil, route)
		if err != nil {
			if hold, ok := KaminoHealthHoldObservation(err, slot, runtime.now()); ok {
				return hold, accounts, nil
			}
			return Observation{}, nil, err
		}
		collateralMint, err := decodeBase58PublicKey(route.Kamino.CollateralMint)
		if err != nil {
			return Observation{}, nil, err
		}
		authority, err := decodeBase58PublicKey(bridgeVault)
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
		idle, err := decodePinnedUSDC(accountAt(accounts, bridgeIdleATA), bridgeIdleAuthority)
		if err != nil {
			return Observation{}, nil, err
		}
		strategy, err := decodePinnedUSDC(accountAt(accounts, bridgeStrategyATA), bridgeStrategyAuth)
		if err != nil {
			return Observation{}, nil, err
		}
		squads, err := decodePinnedUSDC(accountAt(accounts, bridgeSquadsATA), bridgeVault)
		if err != nil {
			return Observation{}, nil, err
		}
		ticket, ticketErr := decodeObservedReportTicket(accountAt(accounts, reportTicketPDA))
		if ticketErr != nil {
			return Observation{}, nil, fmt.Errorf("decode report ticket: %w", ticketErr)
		}
		if nav.Custodies.VoltrIdleRaw != idle.Raw || nav.Custodies.StrategyUSDCraw != strategy.Raw || nav.Custodies.SquadsUSDCraw != squads.Raw || nav.Custodies.SquadsPRIMEraw != prime.Raw {
			return Observation{}, nil, fmt.Errorf("route NAV custody differs inside fixed confirmed account batch")
		}
		if idle.Raw > math.MaxInt64 || strategy.Raw > math.MaxInt64 || squads.Raw > math.MaxInt64 {
			return Observation{}, nil, fmt.Errorf("bridge custody exceeds signed decision range")
		}
		ready, exit := liveRuntimePolicyReadiness(manifest, route, accounts)
		ltv, err := observedLTVBPS(position)
		if err != nil {
			return Observation{}, nil, err
		}
		stateHash := sha256.Sum256([]byte(fmt.Sprintf("%s|voltr-idle:%d|strategy-idle:%d|squads-idle:%d", beforeFingerprint, idle.Raw, strategy.Raw, squads.Raw)))
		base := Observation{ObservedAt: runtime.now(), Snapshot: Snapshot{ObservationID: fmt.Sprintf("%x", stateHash[:]), Slot: slot, RouteKind: RouteKind, Fresh: true, WithdrawalDemandRaw: beforeDemand, VoltrIdleRaw: int64(idle.Raw), VoltrStrategyIdleRaw: int64(strategy.Raw), SquadsIdleRaw: int64(squads.Raw)}}
		base.Snapshot.PrimeIdleRaw = int64(prime.Raw)
		base.Snapshot.CollateralIdleRaw = int64(prime.Raw)
		if (route.Kamino.DebtMint != bridgeUSDC || selectorLane(route.Lane)) && prime.Raw > 0 && !cutoverDrain && beforeDemand == 0 {
			minimum, err := kaminoDepositMinimum(accounts, route, slot, math.MaxInt64)
			if err != nil && !selectorLane(route.Lane) {
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
		base.Snapshot.CutoverDrain = cutoverDrain
		base.Snapshot.TicketLastConsumedSequenceRaw = int64(ticket.LastConsumedSequence)
		base.Snapshot.HasPosition = position.HasPosition
		// The position view and the NAV view decode the obligation independently
		// out of the same confirmed batch; they must agree on whether the account
		// even exists. An absent obligation is carried forward explicitly instead
		// of being silently folded into a flat position.
		if position.ObligationPresent != nav.ObligationPresent {
			return Observation{}, nil, fmt.Errorf("route NAV and position disagree on the obligation account")
		}
		base.Snapshot.ObligationPresent = position.ObligationPresent
		base.Snapshot.ObligationPresenceKnown = true
		base.Snapshot.PositionCollateralRaw = int64(position.CollateralDepositedRaw)
		base.Snapshot.PositionDebtRaw = int64(position.DebtRaw)
		if positionReturnRoute(route.Lane) && position.DebtRaw > 0 {
			// Include NAV -> release -> NAV -> funding -> NAV -> payoff in
			// planning. Each actual wire still has its own short freshness gate.
			bound, err := decodeKaminoPayoffWindow(accounts, route, slot, 6)
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
		base.Snapshot.LiquidationThresholdBPS = position.LiquidationThresholdBPS
		entryUSDC, err := routeEntryCapacityUSDC(position, accounts, route)
		if err != nil {
			return Observation{}, nil, err
		}
		if entryUSDC > math.MaxInt64 {
			return Observation{}, nil, fmt.Errorf("PRIME/USDC entry capacity exceeds signed decision range")
		}
		base.Snapshot.CapacityRaw = int64(entryUSDC)
		base.Snapshot.MaxTargetLTVEntryRaw = int64(entryUSDC)
		base.Snapshot.BorrowUtilizationBlocked = position.BorrowUtilizationBlocked
		base.Snapshot.PolicyLimitRaw = int64(strategyTwoBridgeLegCapRaw)
		base.Snapshot.PolicyReady = ready
		base.Snapshot.ExitBuildable = exit
		observedAt := runtime.now()
		if err := applyRouteNAVSnapshot(&base.Snapshot, nav, observedAt); err != nil {
			return Observation{}, nil, err
		}
		base.Snapshot.ObservationID = routeEconomicObservationID(
			base.Snapshot.ObservationID, prime.Raw, position.CollateralDepositedRaw, position.DebtRaw,
			ready, exit, position.BorrowUtilizationBlocked,
			nav.StrategyNAVRaw, nav.PriorReportedNAVRaw, entryUSDC,
		)
		if route.Kamino.DebtMint != bridgeUSDC || selectorLane(route.Lane) {
			digest := sha256.Sum256([]byte(fmt.Sprintf("%s|lane:%s|idle-debt:%d|payoff-debt:%d|idle-collateral-value:%d|position-debt-value:%d|minimum-deposit:%d", base.Snapshot.ObservationID, route.Lane, nav.Custodies.SquadsDebtRaw, base.Snapshot.PayoffDebtRaw, base.Snapshot.CollateralIdleValueRaw, base.Snapshot.PositionDebtValueRaw, base.Snapshot.MinimumCollateralDepositRaw)))
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
		return base, accounts, nil
	}
	return Observation{}, nil, confirmedObservationUnavailable(fmt.Errorf("confirmed receipt fence did not stabilize around fixed account batch"))
}

// receiptIntegrityObservation is the only observation a batch with a broken
// strategy receipt can support: a monitors-armed snapshot whose single fact is
// the integrity fault itself, so Decide persists the hold and nothing else
// about the book is implied.
func receiptIntegrityObservation(slot int64, route RuntimeRoute) Observation {
	fingerprint := sha256.Sum256([]byte(fmt.Sprintf("strategy-receipt-integrity|%s|%d", route.Lane, slot)))
	return Observation{ObservedAt: time.Now().UTC(), Snapshot: Snapshot{
		ObservationID: fmt.Sprintf("%x", fingerprint[:]), Slot: slot, RouteKind: RouteKind, Fresh: true,
		RouteLane: route.Lane, StrategyKey: route.Lane, MonitorsArmed: true,
		StrategyReceiptIntegrityFault: true,
	}}
}

func legacyPrimeExposure(position KaminoPosition, custodyRaw uint64) bool {
	return custodyRaw != 0 || position.HasPosition || position.CollateralDepositedRaw != 0 || position.DebtRaw != 0
}

func stableReceiptFence(beforeSlot, fixedSlot, afterSlot, beforeDemand, afterDemand int64, beforeFingerprint, afterFingerprint string) bool {
	return beforeSlot > 0 && beforeSlot <= fixedSlot && fixedSlot <= afterSlot && beforeDemand == afterDemand && beforeFingerprint != "" && beforeFingerprint == afterFingerprint
}

func routeFixedAddresses(manifest RouteManifest) []string {
	route, err := manifest.activeRuntimeRoute()
	if err != nil {
		return nil
	}
	addressSet := map[string]struct{}{reportTicketPDA: {}, route.Kamino.CollateralReserve: {}, route.Kamino.DebtReserve: {}, kaminoPrimeLiquiditySupply: {}, kaminoUSDCLiquiditySupply: {}, kaminoCollateralReserve: {}, kaminoDebtReserve: {}, kaminoPrimeCustody: {}, kaminoPrimeUSDCObligation: {}}
	if manifest.selectorObservation {
		for _, lane := range selectorLanes {
			other, _ := runtimeRoute(lane)
			for _, address := range pinnedRouteNAVAddressesForRoute(other) {
				addressSet[address] = struct{}{}
			}
			for _, address := range []string{other.Kamino.Market, other.CollateralLiquiditySupply, other.DebtLiquiditySupply, other.DebtFeeReceiver, other.Kamino.Obligation, other.CollateralCustody} {
				addressSet[address] = struct{}{}
			}
		}
	}

	// All deposit rounding bounds use the Clock from the same custody/reserve
	// batch, including retained PRIME/Maple consumers.
	addressSet[budgetClockAddress] = struct{}{}
	// The selected Phase 2 route can still require a legacy PRIME cutover
	// drain. That observer now validates PRIME's lending market as part of the
	// same confirmed batch, so pin the legacy market even when Maple is active.
	if legacy, err := pinnedKaminoObservationConfig(); err == nil {
		addressSet[legacy.Market] = struct{}{}
	}
	addressSet[route.DebtFeeReceiver] = struct{}{}
	if catalogJupiterRoute(route.Lane) {
		pins, err := catalogRoutePolicyPins(route, manifest)
		if err != nil {
			return nil
		}
		for address := range pins {
			addressSet[address] = struct{}{}
		}
		addressSet[route.CollateralLiquiditySupply] = struct{}{}
		addressSet[route.DebtLiquiditySupply] = struct{}{}
	}
	if route.Lane == SelectedRouteID {
		addressSet[mapleSyrupUSDCUSDC.CollateralLiquiditySupply] = struct{}{}
		addressSet[mapleSyrupUSDCUSDC.DebtLiquiditySupply] = struct{}{}
	}
	for _, address := range pinnedRouteNAVAddressesForRoute(route) {
		addressSet[address] = struct{}{}
	}
	for address := range manifest.runtimePolicyObservationSet() {
		addressSet[address] = struct{}{}
	}
	for _, address := range route.PolicyAccounts {
		addressSet[address] = struct{}{}
	}
	if route.BasicPolicy {
		for _, address := range manifest.PolicyCatalog.PolicyAccounts {
			addressSet[address] = struct{}{}
		}
	} else if route.Lane == SelectedRouteID {
		for _, address := range mapleKaminoPolicyAccounts() {
			addressSet[address] = struct{}{}
		}
	}
	addresses := make([]string, 0, len(addressSet))
	for address := range addressSet {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	return addresses
}

func liveRuntimePolicyReadiness(manifest RouteManifest, route RuntimeRoute, accounts []ConfirmedAccount) (bool, bool) {
	if catalogJupiterRoute(route.Lane) {
		pins, err := catalogRoutePolicyPins(route, manifest)
		if err != nil {
			return false, false
		}
		for address, pin := range pins {
			account := accountAt(accounts, address)
			if account.Owner != bridgeSquadsProgram || account.Executable || account.Lamports == 0 ||
				!maskedPolicyDigestMatches(account.Data, pin.mask, pin.digest) {
				return false, false
			}
		}
		return true, true
	}
	if route.Lane == RouteID {
		return manifest.livePrimeUSDCPolicyReadiness(accounts)
	}
	if route.BasicPolicy {
		families := []BasicPolicyFamily{BasicCollateralLifecycle, BasicDebtLifecycle, BasicSwapRoutesA, BasicSwapRoutesB}
		ready := true
		for _, family := range families {
			binding, hash, err := manifest.basicPolicyBinding(family)
			if err != nil {
				return false, false
			}
			account := accountAt(accounts, binding.Policy)
			if account.Owner != bridgeSquadsProgram || account.Executable || account.Lamports == 0 || sha256Bytes(account.Data) != hash {
				ready = false
			}
		}
		return ready, ready
	}
	for action, address := range route.PolicyAccounts {
		account := accountAt(accounts, address)
		if account.Owner != bridgeSquadsProgram || account.Executable || account.Lamports == 0 || sha256Bytes(account.Data) != route.PolicyHashes[action] {
			return false, false
		}
	}
	for address, hash := range mapleKaminoPolicyHashes() {
		account := accountAt(accounts, address)
		if account.Owner != bridgeSquadsProgram || account.Executable || account.Lamports == 0 || sha256Bytes(account.Data) != hash {
			return false, false
		}
	}
	complete := len(route.PolicyAccounts) == 4 && len(mapleKaminoPolicyHashes()) == 4
	return complete, complete
}

func observePrimeUSDCFromFixedAccounts(ctx context.Context, accountsReader func(context.Context, []string, int64) (int64, []ConfirmedAccount, error), slot int64, accounts []ConfirmedAccount) (KaminoPosition, error) {
	config, err := pinnedKaminoObservationConfig()
	if err != nil {
		return KaminoPosition{}, err
	}
	return observeKaminoFromFixedAccounts(ctx, accountsReader, slot, accounts, config)
}

// Market unavailability closes entry, but cannot invalidate independently
// observed USDC. This fallback is restricted to a structurally valid, empty
// USDC lane. Every nonzero collateral/debt holding retains the original hold.
func observeKaminoWithCashFallback(ctx context.Context, reader func(context.Context, []string, int64) (int64, []ConfirmedAccount, error), slot int64, accounts []ConfirmedAccount, route RuntimeRoute) (KaminoPosition, error) {
	position, originalErr := observeKaminoFromFixedAccounts(ctx, reader, slot, accounts, route.Kamino)
	if originalErr == nil {
		return position, nil
	}
	if _, health := kaminoHealthReason(originalErr); !health || route.Kamino.DebtMint != bridgeUSDC {
		return KaminoPosition{}, originalErr
	}
	custodies, err := decodeRouteNAVCustodiesForRoute(accounts, route)
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
	return KaminoPosition{Slot: slot, RefreshedSlot: obligation.refreshedSlot, ObligationPresent: account.Lamports != 0, BorrowUtilizationBlocked: true}, nil
}

func observeKaminoFromFixedAccounts(ctx context.Context, accountsReader func(context.Context, []string, int64) (int64, []ConfirmedAccount, error), slot int64, accounts []ConfirmedAccount, config KaminoObservationConfig) (KaminoPosition, error) {
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
	// Audit U5 / monitor M5: the position view refuses the same unhealthy
	// reserve states the NAV refuses, plus oracle staleness against chain time
	// read from this batch's own Clock sysvar.
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
	oracleSlot, oracleAccounts, err := accountsReader(ctx, oracles, slot)
	if err != nil {
		return KaminoPosition{}, err
	}
	if oracleSlot < slot {
		return KaminoPosition{}, fmt.Errorf("oracle validation predates fixed account batch")
	}
	for _, oracle := range oracleAccounts {
		if oracle.Executable || oracle.Lamports == 0 || len(oracle.Data) == 0 {
			return KaminoPosition{}, fmt.Errorf("invalid configured oracle %s", oracle.Address)
		}
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
	return KaminoPosition{Slot: slot, RefreshedSlot: obligation.refreshedSlot, HasPosition: obligation.hasPosition, ObligationPresent: obligationAccount.Lamports != 0, CollateralDepositedRaw: obligation.collateralDepositedRaw, DebtRaw: debtRaw, RedeemablePrimeRaw: redeemable, CollateralPriceSF: collateral.marketPriceSF, DebtPriceSF: debt.marketPriceSF, CollateralDecimals: collateral.mintDecimals, DebtDecimals: debt.mintDecimals, Oracles: oracles, LiquidationThresholdBPS: int64(collateral.liquidationThresholdPct) * 100, EntryCapacityRaw: capacity, BorrowUtilizationBlocked: borrowUtilizationBlocked}, nil
}

// Capacity is originally debt-denominated. The entry planner spends bridge
// USDC, so normalize at observed prices and floor rather than assume a peg.
// The caller has already validated this same batch's NAV/refresh dependencies.
func routeEntryCapacityUSDC(position KaminoPosition, accounts []ConfirmedAccount, route RuntimeRoute) (uint64, error) {
	if route.Kamino.DebtMint == bridgeUSDC {
		if selectorLane(route.Lane) {
			return kaminoPairEntryCapacity(position, accounts, route)
		}
		return position.EntryCapacityRaw, nil
	}
	reference, err := pinnedKaminoObservationConfig()
	if err != nil {
		return 0, err
	}
	usdc, err := decodeKaminoReserve(accountAt(accounts, reference.DebtReserve), bridgeUSDC, reference)
	if err != nil {
		return 0, err
	}
	if usdc.mintDecimals != 6 || usdc.refreshedSlot > position.Slot {
		return 0, fmt.Errorf("entry USDC reference drifted")
	}
	return valueBetweenTokenRaw(position.EntryCapacityRaw, position.DebtDecimals, 6, position.DebtPriceSF, usdc.marketPriceSF, false)
}

// routeEconomicObservationID deliberately excludes Slot, the stateless adaptor
// report sequence (which equals Slot), and the slot-bearing NAV digest. A later
// confirmed read of unchanged money state must remain the same decision input;
// the durable operation journal supplies the lifecycle epoch for actionable
// idempotency.
func routeEconomicObservationID(
	bridgeObservationID string,
	primeRaw, collateralRaw, debtRaw uint64,
	policyReady, exitBuildable, borrowUtilizationBlocked bool,
	strategyNAVRaw, priorReportedNAVRaw, capacityRaw uint64,
) string {
	stateHash := sha256.Sum256([]byte(fmt.Sprintf(
		"%s|prime:%d|collateral:%d|debt:%d|policy:%t|exit:%t|borrow-utilization-blocked:%t|strategy-nav:%d|reported-nav:%d|capacity:%d",
		bridgeObservationID, primeRaw, collateralRaw, debtRaw, policyReady, exitBuildable,
		borrowUtilizationBlocked, strategyNAVRaw, priorReportedNAVRaw, capacityRaw,
	)))
	return fmt.Sprintf("%x", stateHash[:])
}

func applyRouteNAVSnapshot(snapshot *Snapshot, nav RouteNAVSnapshot, now time.Time) error {
	if snapshot == nil || snapshot.Slot <= 0 || nav.Slot != snapshot.Slot || now.IsZero() || now.Unix() < 0 ||
		nav.StrategyNAVRaw > math.MaxInt64 || nav.TotalVaultNAVRaw > math.MaxInt64 || nav.PriorReportedNAVRaw > math.MaxInt64 ||
		nav.Report.Sequence > math.MaxInt64 || nav.Custodies.SquadsDebtRaw > math.MaxInt64 || nav.PrimeIdleValueRaw > math.MaxInt64 ||
		nav.PriorReportUpdatedTS > math.MaxInt64 || nav.Report.Sequence != nav.Report.ObservedSlot ||
		nav.Report.ObservedSlot != uint64(nav.Slot) || nav.Report.NAVAfterRaw != nav.StrategyNAVRaw ||
		nav.Voltr.TotalValueRaw > math.MaxInt64 || nav.Receipt.CustodyTrackedRaw > math.MaxInt64 ||
		nav.Voltr.FeeAccumulatorRaw() > math.MaxInt64 || nav.Voltr.LPSupplyInclFeesRaw(nav.LPSupplyRaw) > math.MaxInt64 ||
		nav.Voltr.LockedProfitDegradationSeconds > math.MaxInt64 || nav.Voltr.LastUpdatedLockedProfitRaw > math.MaxInt64 ||
		nav.Voltr.LastLockedProfitReportUnix > math.MaxInt64 ||
		nav.Report.SnapshotDigest != nav.SnapshotDigest || !sha256Pattern.MatchString(nav.SnapshotDigest) {
		return fmt.Errorf("route NAV cannot be merged into the confirmed snapshot")
	}
	currentUnix := now.Unix()
	lastUpdated := int64(nav.PriorReportUpdatedTS)
	age := currentUnix - lastUpdated
	if lastUpdated > currentUnix {
		// Wall-clock skew must not produce a negative age (which the decision
		// engine treats as incoherent) or force a spurious report.
		age = 0
	}
	// A capital mutation is a journal fact, never a valuation comparison: the
	// production observe path sets this from reconciled bridge mutations newer
	// than the last reconciled report, so a NAV move can never explain itself.
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
	snapshot.FeeAccumulatorRaw = int64(nav.Voltr.FeeAccumulatorRaw())
	snapshot.LPSupplyInclFeesRaw = int64(nav.Voltr.LPSupplyInclFeesRaw(nav.LPSupplyRaw))
	snapshot.ManagerPerformanceFeeBPS = int64(nav.Voltr.ManagerPerformanceFeeBPS)
	snapshot.AdminPerformanceFeeBPS = int64(nav.Voltr.AdminPerformanceFeeBPS)
	// Reaching this point means every identity, book, custody, receipt, and
	// reserve input decoded coherently from one confirmed batch, so the
	// fail-closed monitors may gate the decisions planned from it.
	snapshot.MonitorsArmed = true
	return nil
}

func decodePinnedPrime(account ConfirmedAccount) (DecodedTokenCustody, error) {
	mint, err := decodeBase58PublicKey(kaminoPrimeMint)
	if err != nil {
		return DecodedTokenCustody{}, err
	}
	authority, err := decodeBase58PublicKey(bridgeVault)
	if err != nil {
		return DecodedTokenCustody{}, err
	}
	if account.Address != kaminoPrimeCustody || account.Owner != bridgeTokenProgram || account.Executable || account.Lamports == 0 {
		return DecodedTokenCustody{}, fmt.Errorf("PRIME custody envelope drifted")
	}
	return DecodeTokenCustody(account.Owner, account.Data, mint, authority)
}

// runtimePolicyObservationSet lists the policy accounts whose presence the
// runtime observation must request, regardless of digest.
func (m RouteManifest) runtimePolicyObservationSet() map[string]string {
	wanted, _ := m.requiredPrimeUSDCPolicyHashes()
	return wanted
}

// requiredPrimeUSDCPolicyHashes maps each policy account to the digest its
// bytes must hash to, plus the masked-byte spans that digest excludes. Only
// the bridge policies carry a mask; the rest compare as the raw digest.
func (m RouteManifest) requiredPrimeUSDCPolicyHashes() (map[string]string, map[string][][2]int64) {
	wanted := map[string]string{}
	masks := map[string][][2]int64{}
	for _, binding := range m.RuntimeBindings.BridgePolicies {
		if validSHA256(binding.NormalizedDigest) {
			if prior, exists := wanted[binding.Account]; !exists || prior == binding.NormalizedDigest {
				wanted[binding.Account] = binding.NormalizedDigest
				masks[binding.Account] = binding.MaskedByteRanges
			} else {
				wanted[binding.Account] = ""
			}
		}
	}
	for _, binding := range m.RuntimeBindings.PrimeUSDC.Packets {
		if _, err := decodeKey(binding.Policy); err == nil && validSHA256(binding.PolicyAccountDataSHA256) {
			if prior, exists := wanted[binding.Policy]; !exists || prior == binding.PolicyAccountDataSHA256 {
				wanted[binding.Policy] = binding.PolicyAccountDataSHA256
			} else {
				wanted[binding.Policy] = ""
			}
		}
	}
	for _, binding := range m.RuntimeBindings.PrimeUSDC.SwapPolicies {
		if _, err := decodeKey(binding.Policy); err == nil && validSHA256(binding.PolicyAccountDataSHA256) {
			if prior, exists := wanted[binding.Policy]; !exists || prior == binding.PolicyAccountDataSHA256 {
				wanted[binding.Policy] = binding.PolicyAccountDataSHA256
			} else {
				wanted[binding.Policy] = ""
			}
		}
	}
	return wanted, masks
}

func (m RouteManifest) livePrimeUSDCPolicyReadiness(accounts []ConfirmedAccount) (bool, bool) {
	wanted, masks := m.requiredPrimeUSDCPolicyHashes()
	installed := map[string]bool{}
	for address, hash := range wanted {
		account := accountAt(accounts, address)
		installed[address] = hash != "" && account.Address == address && account.Owner == bridgeSquadsProgram && !account.Executable && account.Lamports > 0 && maskedPolicyDigestMatches(account.Data, masks[address], hash)
	}
	kaminoReady := len(m.RuntimeBindings.PrimeUSDC.Packets) == 4
	bridgeReady := len(m.RuntimeBindings.BridgePolicies) == 4
	for _, binding := range m.RuntimeBindings.BridgePolicies {
		if !installed[binding.Account] {
			bridgeReady = false
		}
	}
	seenLegs := map[kaminoPrimeUSDCLeg]bool{}
	for _, binding := range m.RuntimeBindings.PrimeUSDC.Packets {
		if !installed[binding.Policy] {
			kaminoReady = false
			continue
		}
		data, err := decodeManifestPacketData(binding.DataBase64)
		if err != nil {
			kaminoReady = false
			continue
		}
		leg := manifestPacketLeg(data)
		expectedAction := OpenPrimeUSDCStep
		if leg == kaminoLegRepay || leg == kaminoLegWithdraw {
			expectedAction = DeleverPrimeUSDCStep
		}
		if leg == 0 || seenLegs[leg] || binding.Action != expectedAction || binding.PolicyConstraintIndex != 0 {
			kaminoReady = false
		} else {
			seenLegs[leg] = true
		}
	}
	forward, reverse := false, false
	if binding, err := m.jupiterPolicy(SwapUSDCToPrimeStep); err == nil {
		forward = installed[binding.Policy]
	}
	if binding, err := m.jupiterPolicy(SwapPrimeToUSDCStep); err == nil {
		reverse = installed[binding.Policy]
	}
	return bridgeReady && kaminoReady && forward, bridgeReady && kaminoReady && reverse
}

func decodeManifestPacketData(value string) ([]byte, error) {
	// primeUSDCPacket performs the full account-vector validation at build time;
	// readiness only needs the frozen discriminator to prove all four legs exist.
	data, err := base64Strict(value)
	if err != nil || len(data) != 16 {
		return nil, fmt.Errorf("invalid manifest packet data")
	}
	return data, nil
}

func base64Strict(value string) ([]byte, error) {
	return base64.StdEncoding.Strict().DecodeString(value)
}

func manifestPacketLeg(data []byte) kaminoPrimeUSDCLeg {
	if len(data) < 8 {
		return 0
	}
	switch {
	case bytesEqual(data[:8], kaminoDepositCollateral):
		return kaminoLegDeposit
	case bytesEqual(data[:8], kaminoBorrowUSDC):
		return kaminoLegBorrow
	case bytesEqual(data[:8], kaminoRepayUSDC):
		return kaminoLegRepay
	case bytesEqual(data[:8], kaminoWithdrawCollateral):
		return kaminoLegWithdraw
	default:
		return 0
	}
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
