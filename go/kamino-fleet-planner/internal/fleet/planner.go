package fleet

import (
	"fmt"
	"math/big"
	"time"
)

const (
	minimumSupplyUSDMicros   = int64(100_000_000_000)
	minimumNotionalUSDMicros = int64(1_000_000)
	holdingHorizonSeconds    = int64(30 * 24 * 60 * 60)
	secondsPerYear           = int64(365 * 24 * 60 * 60)
	confidencePPM            = int64(950_000)
	expectedServiceMillis    = int64(15_000)
	estimatedCostUSDMicros   = int64(100_000)
)

func Plan(snapshot MarketSnapshot, position VaultPosition, sourceAddress, targetAddress string) Decision {
	return planSource(snapshot, position, sourceAddress, targetAddress, false)
}

// planIdleDeposit is read-only shadow evaluation, not an executable route.
func planIdleDeposit(snapshot MarketSnapshot, position VaultPosition, targetAddress string) Decision {
	return planSource(snapshot, position, "", targetAddress, true)
}

func planSource(snapshot MarketSnapshot, position VaultPosition, sourceAddress, targetAddress string, idle bool) Decision {
	source, sourceOK := snapshot.Reserves[sourceAddress]
	target, targetOK := snapshot.Reserves[targetAddress]
	routeKind := "same_mint"
	targetMint := position.Mint
	estimatedCost := estimatedCostUSDMicros
	serviceMillis := expectedServiceMillis
	if targetOK {
		targetMint = target.Mint
		if target.Mint != position.Mint {
			routeKind = "cross_mint_jupiter"
			estimatedCost = estimatedCostUSDMicros * 3
			serviceMillis = expectedServiceMillis * 3
		}
	}
	if idle {
		routeKind = "idle_vault_deposit"
		// Match the retained observer's default idle-deposit cost estimate.
		estimatedCost = 500_000
	}
	anchored := true
	if routeKind == "cross_mint_jupiter" {
		position, anchored = crossMintRecoveryAnchoredPosition(position)
	}
	decision := Decision{
		Reason: "ineligible", RouteKind: routeKind, VaultID: position.VaultID, SourceSnapshotID: position.SnapshotID,
		MarketSlot: snapshot.Slot, SourceReserve: sourceAddress, TargetReserve: targetAddress,
		Mint: targetMint, SourceMint: position.Mint, TargetMint: targetMint, AmountRaw: position.AmountRaw, PrincipalUSDMicros: position.AmountRaw,
		SnapshotHash: snapshot.Hash, ObservedAt: snapshot.ObservedAt,
		EstimatedCostUSDMicros: estimatedCost, HoldingHorizonSeconds: holdingHorizonSeconds,
		ConfidencePPM: confidencePPM,
	}
	ineligible := func(reason string) Decision { decision.Reason = reason; return decision }
	if idle {
		expiry, ok := snapshot.MintExpiresAt[position.Mint]
		if !ok || !expiry.After(time.Now().Add(minimumPublicationLifetime)) {
			return ineligible("idle_market_evidence_lifetime_too_short")
		}
	}
	if !anchored {
		return ineligible("invalid_cross_mint_recovery_anchor")
	}
	if position.BlockedReason != "" {
		return ineligible(position.BlockedReason)
	}
	if !targetOK || !isEarnStableMint(target.Mint) ||
		(idle && target.Mint != position.Mint) ||
		(!idle && (!sourceOK || source.Address != position.SourceReserve || source.Market != position.Market ||
			source.Mint != position.Mint || !isEarnStableMint(source.Mint))) {
		return ineligible("identity_mismatch")
	}
	if position.VaultID <= 0 || position.AmountRaw <= 0 ||
		(!idle && (position.SnapshotID <= 0 ||
			(position.SourceAmountSemantics == amountSemanticsKaminoCollateralDeposited && position.SourceCollateralAmountRaw <= 0) ||
			position.SourceAmountSemantics != amountSemanticsKaminoCollateralDeposited && position.SourceAmountSemantics != amountSemanticsRedeemableLiquidity)) {
		return ineligible("unsupported_source_amount_evidence")
	}
	if position.AmountRaw < minimumNotionalUSDMicros {
		return ineligible("below_minimum_notional")
	}
	if (!idle && source.TotalSupplyUSDMicros < minimumSupplyUSDMicros) || target.TotalSupplyUSDMicros < minimumSupplyUSDMicros {
		return ineligible("below_minimum_reserve_supply")
	}
	if target.LastUpdateStale {
		return ineligible("target_explicitly_stale")
	}
	if target.EconomicLifetimeMillis < minimumPublicationLifetime.Milliseconds() {
		return ineligible("target_economic_evidence_lifetime_too_short")
	}
	if source.SupplyAPYBPS < 0 || source.SupplyAPYBPS >= 5_000 || target.SupplyAPYBPS < 0 || target.SupplyAPYBPS >= 5_000 {
		return ineligible("invalid_apy")
	}

	if position.TargetCommittedInflowUSDMicros < 0 || position.TargetCommittedOutflowUSDMicros < 0 ||
		position.SourceCommittedInflowUSDMicros < 0 || position.SourceCommittedOutflowUSDMicros < 0 {
		return ineligible("invalid_capacity_projection")
	}
	capacity := target.TotalSupplyUSDMicros / 50
	if capacity < 4_000_000 {
		capacity = 4_000_000
	}
	targetAPY, hasCapacity, ok := capacityAdjustedTargetAPY(
		target,
		position.TargetCommittedInflowUSDMicros,
		position.TargetCommittedOutflowUSDMicros,
		position.AmountRaw,
	)
	if !ok {
		return ineligible("capacity_arithmetic_overflow")
	}
	if !hasCapacity {
		return ineligible("target_capacity_exhausted")
	}
	// Idle tokens earn zero; do not invent a source reserve or its capacity.
	sourceAPY := int64(0)
	if !idle {
		sourceProjectedSupply, valid := sumInt64(source.TotalSupplyUSDMicros, position.SourceCommittedInflowUSDMicros, -position.SourceCommittedOutflowUSDMicros)
		if !valid {
			return ineligible("capacity_arithmetic_overflow")
		}
		if sourceProjectedSupply <= 0 {
			return ineligible("invalid_capacity_projection")
		}
		sourceAPY, valid = mulDivInt64(source.SupplyAPYBPS, source.TotalSupplyUSDMicros, sourceProjectedSupply)
		if !valid {
			return ineligible("capacity_arithmetic_overflow")
		}
	}
	edge := targetAPY - sourceAPY
	if edge < 1 {
		return ineligible("below_minimum_edge")
	}
	gross, ok := mulMulDivInt64(
		position.AmountRaw,
		edge,
		holdingHorizonSeconds,
		10_000,
		secondsPerYear,
	)
	if !ok {
		return ineligible("economic_arithmetic_overflow")
	}
	expected, ok := mulDivInt64(gross, confidencePPM, 1_000_000)
	if !ok {
		return ineligible("economic_arithmetic_overflow")
	}
	guardedCost := estimatedCost*12_500/10_000 + 50_000
	net := expected - guardedCost
	if net < 100_000 {
		return ineligible("below_minimum_net_gain")
	}
	allowedFee, ok := mulDivInt64(net, 50_000, 1_000_000)
	if !ok {
		return ineligible("economic_arithmetic_overflow")
	}
	if allowedFee < 5_000 {
		return ineligible("fee_floor_exceeds_economic_cap")
	}
	feeCap := allowedFee
	if feeCap > 50_000 {
		feeCap = 50_000
	}
	lostPerHour, ok := mulDivInt64(position.AmountRaw, edge, 10_000*8_760)
	if !ok {
		return ineligible("economic_arithmetic_overflow")
	}
	annual, ok := mulDivInt64(lostPerHour, 8_760, 1)
	if !ok || annual <= 0 {
		return ineligible("invalid_annual_gain")
	}
	priority, ok := mulDivInt64(lostPerHour, confidencePPM*1_000, 1_000_000*serviceMillis)
	if !ok {
		return ineligible("economic_arithmetic_overflow")
	}
	if priority < 1 {
		priority = 1
	}
	decision.Eligible, decision.Reason = true, "eligible"
	decision.SourceAPYBPS, decision.TargetAPYBPS, decision.EdgeBPS = sourceAPY, targetAPY, edge
	decision.AnnualYieldGainUSDMicros, decision.ExpectedNetGainUSDMicros = annual, net
	decision.EconomicPriority, decision.EstimatedCostLamports = priority, feeCap
	decision.TargetCapacityUSDMicros = capacity
	return decision
}

// capacityAdjustedTargetAPY mirrors the Rust planner's four conservative
// capacity bands. The selected band's ceiling, rather than the candidate's
// exact amount, prices the marginal target APY for the whole planning wave.
func capacityAdjustedTargetAPY(target ReserveState, committedInflow, committedOutflow, candidate int64) (int64, bool, bool) {
	cumulativeInflow, ok := sumInt64(committedInflow, candidate)
	if !ok {
		return 0, false, false
	}
	ceilings := []int64{
		minInt64(target.TotalSupplyUSDMicros/1_000, 1_000_000_000_000),
		minInt64(target.TotalSupplyUSDMicros/200, 2_000_000_000_000),
		minInt64(target.TotalSupplyUSDMicros/100, 3_000_000_000_000),
		maxInt64(minInt64(target.TotalSupplyUSDMicros/50, 4_000_000_000_000), 4_000_000),
	}
	if committedInflow > ceilings[len(ceilings)-1] {
		ceilings[len(ceilings)-1] = committedInflow
	}
	for _, ceiling := range ceilings {
		if cumulativeInflow > ceiling {
			continue
		}
		bandSupply, ok := sumInt64(target.TotalSupplyUSDMicros, ceiling)
		if !ok {
			return 0, false, false
		}
		bandAPY, ok := mulDivInt64(target.SupplyAPYBPS, target.TotalSupplyUSDMicros, bandSupply)
		if !ok {
			return 0, false, false
		}
		projectedBandSupply, ok := sumInt64(bandSupply, -committedOutflow)
		if !ok || projectedBandSupply <= 0 {
			return 0, false, ok
		}
		projectedAPY, ok := mulDivInt64(bandAPY, bandSupply, projectedBandSupply)
		if !ok {
			return 0, false, false
		}
		return minInt64(projectedAPY, target.SupplyAPYBPS), true, true
	}
	return 0, false, true
}

func isEarnStableMint(mint string) bool {
	for _, supported := range earnStableMints {
		if mint == supported {
			return true
		}
	}
	return false
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func sumInt64(values ...int64) (int64, bool) {
	value := new(big.Int)
	for _, part := range values {
		value.Add(value, big.NewInt(part))
	}
	if !value.IsInt64() {
		return 0, false
	}
	return value.Int64(), true
}

// Match Rust's conservative proportional quote while leaving one collateral
// unit in the source obligation so recovery can reuse the existing obligation.
func crossMintRecoveryAnchoredPosition(position VaultPosition) (VaultPosition, bool) {
	if position.SourceCollateralAmountRaw <= 1 || position.AmountRaw <= 0 || position.SourceAmountSemantics != amountSemanticsKaminoCollateralDeposited {
		return position, false
	}
	amount, ok := mulDivInt64(position.AmountRaw, position.SourceCollateralAmountRaw-1, position.SourceCollateralAmountRaw)
	if !ok || amount <= 0 {
		return position, false
	}
	position.AmountRaw = amount
	position.SourceCollateralAmountRaw--
	return position, true
}

func mulDivInt64(left, right, divisor int64) (int64, bool) {
	if divisor == 0 {
		return 0, false
	}
	value := new(big.Int).Mul(big.NewInt(left), big.NewInt(right))
	value.Quo(value, big.NewInt(divisor))
	if !value.IsInt64() {
		return 0, false
	}
	return value.Int64(), true
}

func mulMulDivInt64(left, right, multiplier, divisor, secondDivisor int64) (int64, bool) {
	if divisor == 0 || secondDivisor == 0 {
		return 0, false
	}
	value := new(big.Int).Mul(big.NewInt(left), big.NewInt(right))
	value.Mul(value, big.NewInt(multiplier))
	denominator := new(big.Int).Mul(big.NewInt(divisor), big.NewInt(secondDivisor))
	value.Quo(value, denominator)
	if !value.IsInt64() {
		return 0, false
	}
	return value.Int64(), true
}

func (d Decision) Validate() error {
	if !d.Eligible {
		return fmt.Errorf("decision is not eligible: %s", d.Reason)
	}
	sourceMint, targetMint := d.SourceMint, d.TargetMint
	if sourceMint == "" {
		sourceMint = d.Mint
	}
	if targetMint == "" {
		targetMint = d.Mint
	}
	if d.VaultID <= 0 || d.SourceSnapshotID <= 0 || d.MarketSlot <= 0 || d.SourceReserve == "" || d.TargetReserve == "" || d.SourceReserve == d.TargetReserve ||
		!isEarnStableMint(sourceMint) || !isEarnStableMint(targetMint) || d.Mint != targetMint || d.AmountRaw <= 0 || d.PrincipalUSDMicros <= 0 || d.EdgeBPS <= 0 || d.ExpectedNetGainUSDMicros <= 0 || d.EconomicPriority <= 0 || d.SnapshotHash == "" || d.ObservedAt.IsZero() {
		return fmt.Errorf("eligible decision evidence is incomplete")
	}
	if time.Since(d.ObservedAt) > 30*time.Second {
		return fmt.Errorf("decision evidence expired")
	}
	return nil
}
