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
	if rpc == nil {
		return Observation{}, fmt.Errorf("RPC client is required")
	}
	return observeConfirmedRouteSnapshot(ctx, manifest, routeObservationRuntime{
		bridge:   func(ctx context.Context) (Observation, error) { return ObserveConfirmedBridgeSnapshot(ctx, rpc) },
		kamino:   rpc.ObserveKaminoPrimeUSDC,
		accounts: rpc.GetMultipleAccounts,
		now:      func() time.Time { return time.Now().UTC() },
	})
}

type routeObservationRuntime struct {
	bridge   func(context.Context) (Observation, error)
	kamino   func(context.Context) (KaminoPosition, error)
	accounts func(context.Context, []string, int64) (int64, []ConfirmedAccount, error)
	now      func() time.Time
}

func observeConfirmedRouteSnapshot(ctx context.Context, manifest RouteManifest, runtime routeObservationRuntime) (Observation, error) {
	if runtime.bridge == nil || runtime.kamino == nil || runtime.accounts == nil || runtime.now == nil {
		return Observation{}, fmt.Errorf("route observation runtime is incomplete")
	}
	for attempt := 0; attempt < maxConfirmedObservationAttempts; attempt++ {
		base, err := runtime.bridge(ctx)
		if err != nil {
			return Observation{}, err
		}
		position, err := runtime.kamino(ctx)
		if err != nil {
			return Observation{}, err
		}
		if position.Slot != base.Snapshot.Slot {
			continue
		}
		addressSet := map[string]struct{}{}
		for _, address := range pinnedRouteNAVAddresses() {
			addressSet[address] = struct{}{}
		}
		for address := range manifest.requiredPrimeUSDCPolicyHashes() {
			addressSet[address] = struct{}{}
		}
		addresses := make([]string, 0, len(addressSet))
		for address := range addressSet {
			addresses = append(addresses, address)
		}
		sort.Strings(addresses)
		slot, accounts, err := runtime.accounts(ctx, addresses, position.Slot)
		if err != nil {
			return Observation{}, err
		}
		if slot != position.Slot {
			continue
		}
		navAccounts, err := selectRouteNAVAccounts(accounts)
		if err != nil {
			return Observation{}, err
		}
		nav, err := ComputeRouteNAV(slot, navAccounts, manifest, nil)
		if err != nil {
			return Observation{}, err
		}
		prime, err := decodePinnedPrime(accountAt(accounts, kaminoPrimeCustody))
		if err != nil {
			return Observation{}, err
		}
		if prime.Raw > math.MaxInt64 || position.CollateralDepositedRaw > math.MaxInt64 || position.DebtRaw > math.MaxInt64 {
			return Observation{}, fmt.Errorf("PRIME/USDC state exceeds signed decision range")
		}
		if nav.Custodies.VoltrIdleRaw != uint64(base.Snapshot.VoltrIdleRaw) ||
			nav.Custodies.StrategyUSDCraw != uint64(base.Snapshot.VoltrStrategyIdleRaw) ||
			nav.Custodies.SquadsUSDCraw != uint64(base.Snapshot.SquadsIdleRaw) ||
			nav.Custodies.SquadsPRIMEraw != prime.Raw {
			return Observation{}, fmt.Errorf("route NAV custody differs inside one confirmed bank snapshot")
		}
		ready, exit := manifest.livePrimeUSDCPolicyReadiness(accounts)
		ltv, err := observedLTVBPS(position)
		if err != nil {
			return Observation{}, err
		}
		base.Snapshot.PrimeIdleRaw = int64(prime.Raw)
		base.Snapshot.HasPosition = position.HasPosition
		base.Snapshot.PositionCollateralRaw = int64(position.CollateralDepositedRaw)
		base.Snapshot.PositionDebtRaw = int64(position.DebtRaw)
		base.Snapshot.PositionCollateralValueRaw = int64(nav.PositionCollateralValue)
		base.Snapshot.PositionDebtValueRaw = int64(nav.PositionDebtValue)
		base.Snapshot.StrategyNAVRaw = int64(nav.StrategyNAVRaw)
		base.Snapshot.LTVBPS = ltv
		base.Snapshot.LiquidationThresholdBPS = position.LiquidationThresholdBPS
		if position.EntryCapacityRaw > math.MaxInt64 {
			return Observation{}, fmt.Errorf("PRIME/USDC entry capacity exceeds signed decision range")
		}
		base.Snapshot.CapacityRaw = int64(position.EntryCapacityRaw)
		base.Snapshot.MaxTargetLTVEntryRaw = int64(position.EntryCapacityRaw)
		base.Snapshot.PolicyLimitRaw = int64(bridgeCapRaw)
		base.Snapshot.PolicyReady = ready
		base.Snapshot.ExitBuildable = exit
		observedAt := runtime.now()
		if err := applyRouteNAVSnapshot(&base.Snapshot, nav, observedAt); err != nil {
			return Observation{}, err
		}
		base.Snapshot.ObservationID = routeEconomicObservationID(
			base.Snapshot.ObservationID, prime.Raw, position.CollateralDepositedRaw, position.DebtRaw,
			ready, exit, nav.StrategyNAVRaw, nav.PriorReportedNAVRaw, position.EntryCapacityRaw,
		)
		base.ObservedAt = observedAt
		return base, nil
	}
	return Observation{}, fmt.Errorf("confirmed bridge, Kamino, and policy reads did not align")
}

// routeEconomicObservationID deliberately excludes Slot, the stateless adaptor
// report sequence (which equals Slot), and the slot-bearing NAV digest. A later
// confirmed read of unchanged money state must remain the same decision input;
// the durable operation journal supplies the lifecycle epoch for actionable
// idempotency.
func routeEconomicObservationID(
	bridgeObservationID string,
	primeRaw, collateralRaw, debtRaw uint64,
	policyReady, exitBuildable bool,
	strategyNAVRaw, priorReportedNAVRaw, capacityRaw uint64,
) string {
	stateHash := sha256.Sum256([]byte(fmt.Sprintf(
		"%s|prime:%d|collateral:%d|debt:%d|policy:%t|exit:%t|strategy-nav:%d|reported-nav:%d|capacity:%d",
		bridgeObservationID, primeRaw, collateralRaw, debtRaw, policyReady, exitBuildable,
		strategyNAVRaw, priorReportedNAVRaw, capacityRaw,
	)))
	return fmt.Sprintf("%x", stateHash[:])
}

func applyRouteNAVSnapshot(snapshot *Snapshot, nav RouteNAVSnapshot, now time.Time) error {
	if snapshot == nil || snapshot.Slot <= 0 || nav.Slot != snapshot.Slot || now.IsZero() || now.Unix() < 0 ||
		nav.StrategyNAVRaw > math.MaxInt64 || nav.PriorReportedNAVRaw > math.MaxInt64 ||
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
