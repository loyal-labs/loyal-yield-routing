package backyard

import (
	"context"
	"encoding/json"
	"math"
	"math/big"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

const (
	leverageDownApproved     = "down_move_approved"
	leverageDownNotWorthCost = "down_move_not_worth_cost"
	leverageDownPersistence  = "down_move_pending_persistence"
)

type leverageReduction struct {
	RepaidMicros     usdMicros
	CollateralMicros usdMicros
	Cost             leverageStepCost
}

type leverageDownPricer func(level float64) (leverageReduction, error)

type downApproval struct {
	MaxCostBPS  microBPS `json:"maxCostBps,omitempty"`
	ExpiresUnix int64    `json:"expiresUnix,omitempty"`
}

func (a *downApproval) UnmarshalJSON(data []byte) error {
	type wire downApproval
	var value wire
	*a = downApproval{}
	// Malformed optional permission must not disable mandatory safety exits.
	if json.Unmarshal(data, &value) == nil && downApproval(value).valid() {
		*a = downApproval(value)
	}
	return nil
}

func (a downApproval) valid() bool {
	return a == (downApproval{}) || a.MaxCostBPS > 0 && a.MaxCostBPS < microBPSWhole && a.ExpiresUnix > 0
}

func (a downApproval) active(now int64) bool {
	return a.valid() && a.MaxCostBPS > 0 && now > 0 && now < a.ExpiresUnix
}

func guardDownStepCost(approval downApproval, now int64, step StepExecutionEvidence, network usdMicros) error {
	sale := step.ExpectedEffects.Sale
	if !approval.active(now) || sale == nil || network <= 0 {
		return staleHold("down_move_approval_unavailable")
	}
	minimum, err := stepConversionOutput(step.Request.conversion())
	if err != nil {
		return err
	}
	proceeds, err := sale.proceedsValue(minimum)
	if err != nil {
		return err
	}
	cost := new(big.Int).Sub(new(big.Int).SetUint64(sale.MarkValueRaw), new(big.Int).SetUint64(proceeds))
	if cost.Sign() < 0 {
		cost.SetInt64(0)
	}
	cost.Add(cost, big.NewInt(int64(network)))
	limit := new(big.Int).Mul(new(big.Int).SetUint64(sale.MarkValueRaw), new(big.Int).SetUint64(uint64(approval.MaxCostBPS)))
	limit.Quo(limit, new(big.Int).SetUint64(uint64(microBPSWhole)))
	if cost.Cmp(limit) > 0 {
		return staleHold("down_move_cost_exceeds_approval")
	}
	return nil
}

func (out leverageDecision) priceDownStep(s Snapshot, m LaneEconomics, p SelectorPolicy, price leverageDownPricer) leverageDecision {
	// Removing an unfilled borrowing target requires no sale.
	if !levelDownDue(s, LaneTarget{Level: economicHeldLevel(s, out.Next)}) {
		return out
	}
	if p.validate() != nil || !finite(m.NativeAPY) || m.NativeAPY <= -1 || !finite(m.SupplyAPY) || m.SupplyAPY < 0 ||
		!finite(m.CurrentBorrowAPY) || m.CurrentBorrowAPY < 0 {
		return out.held(leverageReasonRateUnavailable)
	}
	if !s.MonitorsArmed || !approvedVoltrFeeTerms(s) {
		return out.held(leverageReasonFeeTermsUnavailable)
	}
	if price == nil {
		return out.held(leverageReasonCostUnavailable)
	}
	reduction, err := price(economicHeldLevel(s, out.Next))
	if err != nil || reduction.RepaidMicros <= 0 || reduction.RepaidMicros > usdMicros(s.PositionDebtValueRaw) ||
		reduction.CollateralMicros <= 0 || reduction.CollateralMicros > usdMicros(s.PositionCollateralValueRaw) || reduction.Cost.TotalMicros < 0 || reduction.Cost.TotalMicros > 1<<53 {
		return out.held(leverageCostOutageReason(err))
	}
	out.Cost = reduction.Cost
	years := p.Horizon.Hours() / (365.25 * 24)
	debt, cost := float64(reduction.RepaidMicros), float64(reduction.Cost.TotalMicros)
	// Do not assume the rest of the pool keeps utilization lower after our sale.
	interestSaved := debt * math.Expm1(math.Log1p(m.CurrentBorrowAPY)*years)
	yieldLost := (float64(reduction.CollateralMicros) + float64(reduction.Cost.NetworkMicros)) * math.Expm1((math.Log1p(m.NativeAPY)+math.Log1p(m.SupplyAPY))*years)
	benefit := interestSaved - yieldLost
	if benefit > 0 {
		benefit *= 1 - float64(s.ApprovedAdminPerformanceFeeBPS)/10_000
	}
	out.GainRaw = benefit - cost
	if !finite(out.GainRaw) {
		out.GainRaw = 0
		return out.held(leverageReasonFeeForecastUnavailable)
	}
	if out.GainRaw <= float64(p.MinimumBenefitRaw)+debt*float64(p.UncertaintyBPS)/10_000 {
		return out.held(leverageDownNotWorthCost)
	}
	feeKeep := 1 - float64(s.ApprovedAdminPerformanceFeeBPS)/10_000
	budget := (interestSaved-yieldLost)*feeKeep - float64(p.MinimumBenefitRaw) - debt*float64(p.UncertaintyBPS)/10_000
	// A per-dollar ceiling also bounds the sum of the sequence's expenses.
	// It includes network fees and is checked against each new minimum quote.
	bps := math.Floor(budget / float64(reduction.CollateralMicros) * float64(microBPSWhole))
	if !finite(bps) || bps < 1 || bps >= float64(microBPSWhole) {
		return out.held(leverageDownNotWorthCost)
	}
	out.Down = downApproval{MaxCostBPS: microBPS(bps), ExpiresUnix: s.ChainUnix + int64(p.MaxSampleGap/time.Second)}
	out.Reason = leverageDownApproved
	return out
}

func productionLeverageDownPricer(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, m RouteManifest, o Observation, s Snapshot) leverageDownPricer {
	return func(level float64) (leverageReduction, error) {
		ctx, cancel := context.WithTimeout(ctx, leverageStepPriceTimeout)
		defer cancel()
		if o.routeBatch == nil {
			return leverageReduction{}, costOutage(stageInputs, staleHold(leverageReasonCostUnavailable))
		}
		route, err := m.route(s.RouteLane)
		if err != nil {
			return leverageReduction{}, err
		}
		accounts := o.routeBatch.Accounts
		st, err := readStepState(m.vault, accounts, route, s.Slot)
		if err != nil {
			return leverageReduction{}, err
		}
		prices, err := batchLanePrices(accounts, route)
		if err != nil {
			return leverageReduction{}, err
		}
		p, err := newLanePricer(ctx, rpc, view, client, m, s.Slot)
		if err != nil {
			return leverageReduction{}, err
		}
		repay := uint64(max(stepDebtToLTVRaw(s, leverageLevelLTVBPS(level)), 0))
		if level == 1 {
			repay = uint64(max(s.PositionDebtRaw, 0))
		}
		if repay == 0 {
			return leverageReduction{}, staleHold("down_move_has_no_repayment")
		}
		sold, err := valueBetweenTokenRaw(repay, prices.debtDecimals, prices.collateralDecimals, prices.debt, prices.collateral, true)
		if err != nil || sold > math.MaxUint64/(10_000+stepSwapLossBPS) {
			return leverageReduction{}, staleHold("down_move_unpriced")
		}
		sold = sold * (10_000 + stepSwapLossBPS) / 10_000
		receipts, err := payoffReceiptsForLiquidity(st.collateral.totalLiquiditySF, st.collateral.terms.CollateralMintTotalSupply, sold)
		if err != nil || receipts == 0 || receipts > st.position.CollateralDepositedRaw {
			return leverageReduction{}, staleHold("down_move_unpriced")
		}
		limit, err := st.stepWithdrawReceipts()
		if err != nil || limit == 0 {
			return leverageReduction{}, staleHold("down_move_no_room")
		}
		// Price the whole reduction, including every bounded transaction, before
		// allowing its first sale. A full-size quote is conservative about depth.
		steps := 1 + (receipts-1)/limit
		st.collateralBalance, st.debtBalance = 0, 0
		step, _, err := assembleStep(ctx, rpc, view, client, m, p.policies, st, stepDown, receipts, sold, s.Slot)
		if err != nil {
			return leverageReduction{}, costOutage(stageExitQuote, err)
		}
		mark, err := prices.collateralMicros(sold)
		if err != nil {
			return leverageReduction{}, err
		}
		minimum, err := stepConversionOutput(step.Request.conversion())
		if err != nil {
			return leverageReduction{}, err
		}
		proceeds, err := prices.debtMicros(minimum)
		if err != nil {
			return leverageReduction{}, err
		}
		removed, err := prices.debtMicros(min(minimum, uint64(s.PositionDebtRaw)))
		if err != nil {
			return leverageReduction{}, err
		}
		message, err := compileStepMessageForDelegate(m.vault, step.Request, mustKey(backyardDelegate))
		if err != nil {
			return leverageReduction{}, err
		}
		fee, err := p.networkMicros(message)
		if err != nil || fee <= 0 || steps > uint64(math.MaxInt64/int64(fee)/2) {
			return leverageReduction{}, staleHold("down_move_fee_unpriced")
		}
		// Include a NAV report per step in the network budget.
		cost := leverageStepCost{UnwindSwapCostMicros: max(mark-proceeds, 0), NetworkMicros: fee * usdMicros(steps) * 2}
		cost.TotalMicros, err = sumMicros(cost.UnwindSwapCostMicros, cost.NetworkMicros)
		return leverageReduction{RepaidMicros: removed, CollateralMicros: mark, Cost: cost}, err
	}
}

type leverageDownWindow struct {
	Current, Next float64
	AdvantageWindow
}

func (p *selectorSampler) persistDown(d leverageDecision, policy SelectorPolicy, now time.Time) leverageDecision {
	if p.downWindows == nil {
		p.downWindows = map[string]leverageDownWindow{}
	}
	if d.Reason != leverageDownApproved {
		delete(p.downWindows, d.Lane)
		return d
	}
	old, ok := p.downWindows[d.Lane]
	w := leverageDownWindow{d.Current, d.Next, AdvantageWindow{Since: now, LastSample: now}}
	if ok && old.Current == d.Current && old.Next == d.Next && !old.Since.After(old.LastSample) &&
		!old.LastSample.After(now) && now.Sub(old.LastSample) <= policy.MaxSampleGap {
		w.Since = old.Since
	}
	p.downWindows[d.Lane] = w
	if now.Sub(w.Since) < policy.Persistence {
		return d.held(leverageDownPersistence)
	}
	return d
}
