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
	decision := Decision{
		Reason: "ineligible", VaultID: position.VaultID, SourceSnapshotID: position.SnapshotID,
		MarketSlot: snapshot.Slot, SourceReserve: sourceAddress, TargetReserve: targetAddress,
		Mint: position.Mint, AmountRaw: position.AmountRaw, PrincipalUSDMicros: position.AmountRaw,
		SnapshotHash: snapshot.Hash, ObservedAt: snapshot.ObservedAt,
		EstimatedCostUSDMicros: estimatedCostUSDMicros, HoldingHorizonSeconds: holdingHorizonSeconds,
		ConfidencePPM: confidencePPM,
	}
	ineligible := func(reason string) Decision { decision.Reason = reason; return decision }
	if position.BlockedReason != "" {
		return ineligible(position.BlockedReason)
	}
	source, sourceOK := snapshot.Reserves[sourceAddress]
	target, targetOK := snapshot.Reserves[targetAddress]
	if !sourceOK || !targetOK || source.Address != position.SourceReserve || source.Market != position.Market ||
		target.Market != position.Market || source.Mint != USDCMint || target.Mint != USDCMint || position.Mint != USDCMint {
		return ineligible("identity_mismatch")
	}
	if position.VaultID <= 0 || position.SnapshotID <= 0 || position.AmountRaw <= 0 || position.SourceCollateralAmountRaw <= 0 ||
		position.SourceAmountSemantics != "kamino_collateral_deposited" && position.SourceAmountSemantics != "redeemable_liquidity_amount" {
		return ineligible("unsupported_source_amount_evidence")
	}
	if position.AmountRaw < minimumNotionalUSDMicros {
		return ineligible("below_minimum_notional")
	}
	if source.TotalSupplyUSDMicros < minimumSupplyUSDMicros || target.TotalSupplyUSDMicros < minimumSupplyUSDMicros {
		return ineligible("below_minimum_reserve_supply")
	}
	if source.EconomicLifetimeMillis < minimumPublicationLifetime.Milliseconds() || target.EconomicLifetimeMillis < minimumPublicationLifetime.Milliseconds() {
		return ineligible("economic_evidence_lifetime_too_short")
	}
	if source.SupplyAPYBPS < 0 || source.SupplyAPYBPS >= 5_000 || target.SupplyAPYBPS < 0 || target.SupplyAPYBPS >= 5_000 {
		return ineligible("invalid_apy")
	}

	if position.TargetCommittedInflowUSDMicros < 0 || position.TargetCommittedOutflowUSDMicros < 0 ||
		position.SourceCommittedInflowUSDMicros < 0 || position.SourceCommittedOutflowUSDMicros < 0 {
		return ineligible("invalid_capacity_projection")
	}
	capacity := target.TotalSupplyUSDMicros / 50
	if capacity <= 0 || position.TargetCommittedInflowUSDMicros > capacity-position.AmountRaw {
		return ineligible("target_capacity_exhausted")
	}
	targetProjectedSupply, ok := sumInt64(target.TotalSupplyUSDMicros, position.TargetCommittedInflowUSDMicros, position.AmountRaw, -position.TargetCommittedOutflowUSDMicros)
	if !ok {
		return ineligible("capacity_arithmetic_overflow")
	}
	sourceProjectedSupply, ok := sumInt64(source.TotalSupplyUSDMicros, position.SourceCommittedInflowUSDMicros, -position.SourceCommittedOutflowUSDMicros)
	if !ok {
		return ineligible("capacity_arithmetic_overflow")
	}
	if targetProjectedSupply <= 0 || sourceProjectedSupply <= 0 {
		return ineligible("invalid_capacity_projection")
	}
	targetAPY, ok := mulDivInt64(target.SupplyAPYBPS, target.TotalSupplyUSDMicros, targetProjectedSupply)
	if !ok {
		return ineligible("capacity_arithmetic_overflow")
	}
	if targetAPY > target.SupplyAPYBPS {
		targetAPY = target.SupplyAPYBPS
	}
	sourceAPY, ok := mulDivInt64(source.SupplyAPYBPS, source.TotalSupplyUSDMicros, sourceProjectedSupply)
	if !ok {
		return ineligible("capacity_arithmetic_overflow")
	}
	edge := targetAPY - sourceAPY
	if edge < 1 {
		return ineligible("below_minimum_edge")
	}
	annual, ok := mulDivInt64(position.AmountRaw, edge, 10_000)
	if !ok || annual <= 0 {
		return ineligible("invalid_annual_gain")
	}
	gross, ok := mulDivInt64(annual, holdingHorizonSeconds, secondsPerYear)
	if !ok {
		return ineligible("economic_arithmetic_overflow")
	}
	expected, ok := mulDivInt64(gross, confidencePPM, 1_000_000)
	if !ok {
		return ineligible("economic_arithmetic_overflow")
	}
	guardedCost := estimatedCostUSDMicros*12_500/10_000 + 50_000
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
	priority, ok := mulDivInt64(lostPerHour, confidencePPM*1_000, 1_000_000*expectedServiceMillis)
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

func (d Decision) Validate() error {
	if !d.Eligible {
		return fmt.Errorf("decision is not eligible: %s", d.Reason)
	}
	if d.VaultID <= 0 || d.SourceSnapshotID <= 0 || d.MarketSlot <= 0 || d.SourceReserve == "" || d.TargetReserve == "" || d.SourceReserve == d.TargetReserve ||
		d.Mint != USDCMint || d.AmountRaw <= 0 || d.PrincipalUSDMicros <= 0 || d.EdgeBPS <= 0 || d.ExpectedNetGainUSDMicros <= 0 || d.EconomicPriority <= 0 || d.SnapshotHash == "" || d.ObservedAt.IsZero() {
		return fmt.Errorf("eligible decision evidence is incomplete")
	}
	if time.Since(d.ObservedAt) > 30*time.Second {
		return fmt.Errorf("decision evidence expired")
	}
	return nil
}
