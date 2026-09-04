package backyardrwa

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
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
			if optional := optionalLifecycleObligation(addresses); optional != "" {
				return rpc.GetMultipleAccountsWithOptional(ctx, addresses, minSlot, optional)
			}
			return rpc.GetMultipleAccounts(ctx, addresses, minSlot)
		},
		now: func() time.Time { return time.Now().UTC() },
	})
}

// A full K-Lend withdrawal closes its obligation account. Prefer the selected
// Phase 2 obligation when both route families are observed so terminal custody
// swaps can continue after the close; retain the Phase 1 fallback for its own
// zero-position lifecycle.
func optionalLifecycleObligation(addresses []string) string {
	selected := mapleSyrupUSDCUSDC.Kamino.Obligation
	for _, candidate := range addresses {
		if candidate == selected {
			return selected
		}
	}
	for _, candidate := range addresses {
		if candidate == kaminoPrimeUSDCObligation {
			return kaminoPrimeUSDCObligation
		}
	}
	return ""
}

type routeObservationRuntime struct {
	confirmedSlot func(context.Context) (int64, error)
	receipts      func(context.Context, int64) (int64, []programAccount, error)
	accounts      func(context.Context, []string, int64) (int64, []ConfirmedAccount, error)
	now           func() time.Time
}

func observeConfirmedRouteSnapshot(ctx context.Context, manifest RouteManifest, runtime routeObservationRuntime) (Observation, error) {
	observation, _, err := observeConfirmedRouteSnapshotWithAccounts(ctx, manifest, runtime)
	return observation, err
}

func observeConfirmedRouteSnapshotWithAccounts(ctx context.Context, manifest RouteManifest, runtime routeObservationRuntime) (Observation, []ConfirmedAccount, error) {
	if runtime.confirmedSlot == nil || runtime.receipts == nil || runtime.accounts == nil || runtime.now == nil {
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
		cutoverDrain := false
		if route.Lane == SelectedRouteID {
			legacyPosition, legacyErr := observePrimeUSDCFromFixedAccounts(ctx, runtime.accounts, slot, accounts)
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
		position, err := observeKaminoFromFixedAccounts(ctx, runtime.accounts, slot, accounts, route.Kamino)
		if err != nil {
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
		base.Snapshot.RouteLane = route.Lane
		base.Snapshot.StrategyKey = route.Lane
		base.Snapshot.CutoverDrain = cutoverDrain
		base.Snapshot.HasPosition = position.HasPosition
		base.Snapshot.PositionCollateralRaw = int64(position.CollateralDepositedRaw)
		base.Snapshot.PositionDebtRaw = int64(position.DebtRaw)
		base.Snapshot.PositionCollateralValueRaw = int64(nav.PositionCollateralValue)
		base.Snapshot.PositionDebtValueRaw = int64(nav.PositionDebtValue)
		base.Snapshot.StrategyNAVRaw = int64(nav.StrategyNAVRaw)
		base.Snapshot.LTVBPS = ltv
		base.Snapshot.LiquidationThresholdBPS = position.LiquidationThresholdBPS
		if position.EntryCapacityRaw > math.MaxInt64 {
			return Observation{}, nil, fmt.Errorf("PRIME/USDC entry capacity exceeds signed decision range")
		}
		base.Snapshot.CapacityRaw = int64(position.EntryCapacityRaw)
		base.Snapshot.MaxTargetLTVEntryRaw = int64(position.EntryCapacityRaw)
		base.Snapshot.BorrowUtilizationBlocked = position.BorrowUtilizationBlocked
		base.Snapshot.PolicyLimitRaw = int64(bridgeCapRaw)
		base.Snapshot.PolicyReady = ready
		base.Snapshot.ExitBuildable = exit
		observedAt := runtime.now()
		if err := applyRouteNAVSnapshot(&base.Snapshot, nav, observedAt); err != nil {
			return Observation{}, nil, err
		}
		base.Snapshot.ObservationID = routeEconomicObservationID(
			base.Snapshot.ObservationID, prime.Raw, position.CollateralDepositedRaw, position.DebtRaw,
			ready, exit, position.BorrowUtilizationBlocked,
			nav.StrategyNAVRaw, nav.PriorReportedNAVRaw, position.EntryCapacityRaw,
		)
		base.ObservedAt = observedAt
		return base, accounts, nil
	}
	return Observation{}, nil, confirmedObservationUnavailable(fmt.Errorf("confirmed receipt fence did not stabilize around fixed account batch"))
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
	if route.Lane == SelectedRouteID {
		addressSet[mapleSyrupUSDCUSDC.CollateralLiquiditySupply] = struct{}{}
		addressSet[mapleSyrupUSDCUSDC.DebtLiquiditySupply] = struct{}{}
	}
	for _, address := range pinnedRouteNAVAddressesForRoute(route) {
		addressSet[address] = struct{}{}
	}
	for address := range manifest.requiredPrimeUSDCPolicyHashes() {
		addressSet[address] = struct{}{}
	}
	for _, address := range route.PolicyAccounts {
		addressSet[address] = struct{}{}
	}
	if route.Lane == SelectedRouteID {
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
	if route.Lane == RouteID {
		return manifest.livePrimeUSDCPolicyReadiness(accounts)
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
	if err := validateKaminoRefresh(obligation, collateral, debt); err != nil {
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
	return KaminoPosition{Slot: slot, RefreshedSlot: obligation.refreshedSlot, HasPosition: obligation.hasPosition, CollateralDepositedRaw: obligation.collateralDepositedRaw, DebtRaw: obligation.debtRaw, RedeemablePrimeRaw: redeemable, CollateralPriceSF: collateral.marketPriceSF, DebtPriceSF: debt.marketPriceSF, Oracles: oracles, LiquidationThresholdBPS: int64(collateral.liquidationThresholdPct) * 100, EntryCapacityRaw: capacity, BorrowUtilizationBlocked: borrowUtilizationBlocked}, nil
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
		nav.Report.Sequence > math.MaxInt64 ||
		nav.PriorReportUpdatedTS > math.MaxInt64 || nav.Report.Sequence != nav.Report.ObservedSlot ||
		nav.Report.ObservedSlot != uint64(nav.Slot) || nav.Report.NAVAfterRaw != nav.StrategyNAVRaw ||
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
	snapshot.CapitalMutated = nav.StrategyNAVRaw != nav.PriorReportedNAVRaw
	snapshot.LastReportAgeSeconds = age
	snapshot.TotalVaultNAVRaw = int64(nav.TotalVaultNAVRaw)
	snapshot.PriorReportedNAVRaw = int64(nav.PriorReportedNAVRaw)
	snapshot.PriorReportUpdatedUnix = lastUpdated
	snapshot.ReportSequence = int64(nav.Report.Sequence)
	snapshot.ReportSnapshotDigest = nav.Report.SnapshotDigest
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

func (m RouteManifest) requiredPrimeUSDCPolicyHashes() map[string]string {
	wanted := map[string]string{}
	for _, binding := range m.RuntimeBindings.BridgePolicies {
		if binding.DataSHA256 != nil && validSHA256(*binding.DataSHA256) {
			if prior, exists := wanted[binding.Account]; !exists || prior == *binding.DataSHA256 {
				wanted[binding.Account] = *binding.DataSHA256
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
	return wanted
}

func (m RouteManifest) livePrimeUSDCPolicyReadiness(accounts []ConfirmedAccount) (bool, bool) {
	installed := map[string]bool{}
	for address, hash := range m.requiredPrimeUSDCPolicyHashes() {
		account := accountAt(accounts, address)
		installed[address] = hash != "" && account.Address == address && account.Owner == bridgeSquadsProgram && !account.Executable && account.Lamports > 0 && sha256Bytes(account.Data) == hash
	}
	kaminoReady := len(m.RuntimeBindings.PrimeUSDC.Packets) == 4
	bridgeReady := len(m.RuntimeBindings.BridgePolicies) == 4
	for _, binding := range m.RuntimeBindings.BridgePolicies {
		if binding.DataSHA256 == nil || !installed[binding.Account] {
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
	if position.DebtRaw == 0 {
		return 0, nil
	}
	if position.RedeemablePrimeRaw == 0 {
		return 0, fmt.Errorf("Kamino debt has no redeemable collateral")
	}
	collateral := new(big.Int).Mul(new(big.Int).SetUint64(position.RedeemablePrimeRaw), littleInt(position.CollateralPriceSF[:]))
	debt := new(big.Int).Mul(new(big.Int).SetUint64(position.DebtRaw), littleInt(position.DebtPriceSF[:]))
	if collateral.Sign() <= 0 || debt.Sign() <= 0 {
		return 0, fmt.Errorf("Kamino LTV price is zero")
	}
	debt.Mul(debt, big.NewInt(10_000)).Div(debt, collateral)
	if !debt.IsInt64() || debt.Int64() > 10_000 {
		return 0, fmt.Errorf("Kamino LTV is outside bounded range")
	}
	return debt.Int64(), nil
}
