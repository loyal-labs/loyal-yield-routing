package backyard

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

const (
	executionTradeValueRaw = int64(1_000_000_000)
	executionPriceLossBPS  = uint64(20)
	executionTradeInterval = time.Minute
)

// Loan emergencies keep their existing execution path, including recovery of
// already signed transactions. Ordinary sales and new investments may wait.
func protectedExecution(v *Vault, s Snapshot, d Decision) bool {
	if v == nil || !v.ProtectExecution || d.Reason == hardLTVDeleverReason || d.Reason == termKeepAliveReason {
		return false
	}
	switch s.Target.Rule {
	case ruleHardLTV, ruleTermMaturity, ruleOperatorExit:
		return false
	}
	return ordinaryWithdrawalCostRequired(s, d) || requiresNAVForAdd(d) || d.Reason == stepResidueReason
}

func executionTrade(d Decision) bool {
	switch d.Action {
	case SwapStableToCollateralStep, SwapCollateralToStableStep, SwapCollateralToDebtStep, SwapDebtToCollateralStep, SwapDebtToUSDCStep, SwapUSDCToDebtStep:
		return true
	case OpenRouteStep, DeleverRouteStep:
		return stepKind(d) != notStep && stepKind(d) != stepMove
	}
	return false
}

// This is derived from the operation journal, so restarting cannot reset the
// delay. Failed broadcasts count too: retries must not burn fees every tick.
const latestExecutionTradeSQL = `SELECT max(updated_at) FROM loyal_yield.multiply_operations
	WHERE route_key=$1 AND status IN ('reconciled','failed') AND broadcast_intent_at IS NOT NULL
	AND expected_effects->'expectedEffects'->>'kind' IN ('cross-mint-swap','atomic-step')`

func (d *Database) executionReady(ctx context.Context, route string, now time.Time) error {
	var last pgtype.Timestamptz
	if err := d.pool.QueryRow(ctx, latestExecutionTradeSQL, route).Scan(&last); err != nil {
		return err
	}
	if last.Valid && now.Before(last.Time.Add(executionTradeInterval)) {
		return staleHold("execution_waiting_interval")
	}
	return nil
}

func executionTokenLimit(p lanePrices, route RuntimeRoute, mint string) (uint64, error) {
	switch mint {
	case fleet.USDCMint:
		return uint64(executionTradeValueRaw), nil
	case route.Kamino.CollateralMint:
		return valueBetweenTokenRaw(uint64(executionTradeValueRaw), 6, p.collateralDecimals, p.usdc, p.collateral, false)
	case route.Kamino.DebtMint:
		return valueBetweenTokenRaw(uint64(executionTradeValueRaw), 6, p.debtDecimals, p.usdc, p.debt, false)
	}
	return 0, staleHold("execution_price_unavailable")
}

func executionEntryLimit(v *Vault, lane string) uint64 {
	if v.selectorPolicy().unleveredEntry(lane) {
		return uint64(executionTradeValueRaw)
	}
	leverageBPS := uint64(laneMaxLevel(lane) * 10_000)
	return uint64(executionTradeValueRaw) * 10_000 / max(leverageBPS, 10_000)
}

func boundedExecutionDecision(v *Vault, o Observation, d Decision) (Decision, error) {
	s := o.Snapshot
	if v != nil && v.LeverageSlippageBPS > 0 && (stepKind(d) == stepUp && d.Reason != termKeepAliveReason || d.Reason == leverageDownPartialReason || d.Reason == debtZeroReason && s.Target.Rule == ruleLevel) {
		d.MaxSlippageBPS = min(effectiveJupiterSlippage(d.MaxSlippageBPS), v.LeverageSlippageBPS)
	}
	if !protectedExecution(v, s, d) {
		return d, nil
	}
	if d.Action == VoltrAllocateToSquads && uint64(d.AmountRaw) > executionEntryLimit(v, d.StrategyKey) {
		// A stored large quote must be repriced, not silently resized.
		return d, staleHold("execution_entry_needs_smaller_quote")
	}
	// Return proceeds already collected before waiting for another sale.
	if ordinaryWithdrawalCostRequired(s, d) && s.SquadsIdleRaw > 0 && s.Target.Outbound {
		if amount := s.StageAllowance.cap(s.SquadsIdleRaw); amount > 0 && s.netKnown(time.Now()) {
			d.Action, d.Reason, d.AmountRaw = StageSquadsToVoltr, outboundStageReason, amount
			d.MaxSlippageBPS = 0
			d.IdempotencyKey += fmt.Sprintf(":cash:%d", amount)
			return d, nil
		}
	}
	if !executionTrade(d) {
		return d, nil
	}
	if o.routeBatch == nil {
		return d, staleHold("execution_price_unavailable")
	}
	route, err := v.route(d.StrategyKey)
	if err != nil {
		return d, err
	}
	p, err := batchLanePrices(o.routeBatch.Accounts, route)
	if err != nil {
		return d, err
	}
	limit := uint64(math.MaxInt64)
	switch d.Action {
	case SwapStableToCollateralStep, SwapUSDCToDebtStep:
		limit = uint64(executionTradeValueRaw)
	case SwapDebtToCollateralStep, SwapDebtToUSDCStep, OpenRouteStep:
		limit, err = executionTokenLimit(p, route, route.Kamino.DebtMint)
		if d.Action == OpenRouteStep && err == nil && !sharedUSDCDebt(d.StrategyKey) {
			limit -= min(limit, uint64(max(int64(s.DebtIdleRaw), 0)))
		}
	case SwapCollateralToStableStep, SwapCollateralToDebtStep:
		limit, err = executionTokenLimit(p, route, route.Kamino.CollateralMint)
	case DeleverRouteStep:
		if d.Reason == releaseReason {
			if s.PositionCollateralValueRaw <= 0 || laneEquityRaw(s) <= 0 {
				return d, staleHold("execution_price_unavailable")
			}
			n := new(big.Int).Mul(big.NewInt(executionTradeValueRaw), big.NewInt(laneEquityRaw(s)))
			n.Quo(n, big.NewInt(s.PositionCollateralValueRaw))
			if !n.IsUint64() {
				return d, staleHold("execution_price_unavailable")
			}
			limit = n.Uint64()
		}
	}
	if err != nil {
		return d, err
	}
	if limit == 0 {
		return d, staleHold("execution_trade_below_minimum")
	}
	if uint64(d.AmountRaw) > limit {
		d.AmountRaw = int64(limit)
		d.IdempotencyKey += fmt.Sprintf(":tranche:%d", limit)
	}
	return d, nil
}

// Reuse the existing NAV seal: the final wire and fresh valuation must still
// match after database admission, immediately before signing.
func sealExecutionRequest(request any, s Snapshot) any {
	a := &addAdmission{net: s.net, requestDigest: addRequestDigest(request)}
	switch r := request.(type) {
	case JupiterSwapRequest:
		r.admission = a
		return r
	case HastraConversionRequest:
		r.admission = a
		return r
	case StepRequest:
		r.admission = a
		return r
	}
	return request
}

func protectExecutionPrice(v *Vault, o Observation, d Decision, request any) (any, error) {
	if !protectedExecution(v, o.Snapshot, d) || !executionTrade(d) {
		return request, nil
	}
	if !o.Snapshot.netKnown(time.Now()) || o.routeBatch == nil {
		return nil, staleHold("execution_price_unavailable")
	}
	conversion := request
	if step, ok := request.(StepRequest); ok {
		if step.Swap == nil && step.Hastra == nil {
			return nil, staleHold("execution_conversion_unavailable")
		}
		conversion = step.conversion()
	}
	var action Action
	var amount uint64
	switch r := conversion.(type) {
	case JupiterSwapRequest:
		action, amount = r.Action, r.AmountRaw
	case HastraConversionRequest:
		action, amount = r.Action, r.AmountRaw
	default:
		return nil, staleHold("execution_conversion_unavailable")
	}
	route, err := v.route(d.StrategyKey)
	if err != nil {
		return nil, err
	}
	p, err := batchLanePrices(o.routeBatch.Accounts, route)
	if err != nil {
		return nil, err
	}
	from, to, _, _, err := jupiterEdgeForRoute(v, action, d.StrategyKey)
	if _, hastra := hastraConversionLeg(action, d.StrategyKey); hastra {
		edge, edgeErr := hastraEdgeForRoute(v, action, d.StrategyKey)
		from, to, err = edge.sourceMint, edge.destinationMint, edgeErr
	}
	if err != nil {
		return nil, err
	}
	mark := func(mint string) (*saleProceedsMark, error) {
		switch mint {
		case fleet.USDCMint:
			return nil, nil
		case route.Kamino.CollateralMint:
			return &saleProceedsMark{PriceSF: p.collateral, USDCSF: p.usdc, Decimals: p.collateralDecimals}, nil
		case route.Kamino.DebtMint:
			return &saleProceedsMark{PriceSF: p.debt, USDCSF: p.usdc, Decimals: p.debtDecimals}, nil
		}
		return nil, staleHold("execution_price_unavailable")
	}
	input, err := mark(from)
	if err != nil {
		return nil, err
	}
	value, err := (ExpectedSale{Proceeds: input}).proceedsValue(amount)
	if err != nil || value == 0 || value > uint64(executionTradeValueRaw) {
		return nil, staleHoldFrom(err, "execution_trade_limit")
	}
	output, err := mark(to)
	if err != nil {
		return nil, err
	}
	// The allowance includes the discount in the quote AND its execution
	// tolerance. It is measured per conversion, not against a user's deposit.
	target := value - value*executionPriceLossBPS/10_000
	guarded, err := floorWithdrawalConversion(conversion, ExpectedSale{Proceeds: output}, target)
	if err != nil {
		return nil, err
	}
	if step, ok := request.(StepRequest); ok {
		switch r := guarded.(type) {
		case JupiterSwapRequest:
			step.Swap = &r
		case HastraConversionRequest:
			step.Hastra = &r
		}
		guarded = step
	}
	return sealExecutionRequest(guarded, o.Snapshot), nil
}
