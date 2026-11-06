package backyard

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

const investmentExitExpensive = "investment_exit_too_expensive"
const investmentExitUnavailable = "investment_exit_unavailable"
const investmentExitSlotsChanged = "investment_exit_quote_slots_changed"
const investmentExitRouteChanged = "investment_exit_quote_route_changed"

// This is a current liquidity screen, not a promise of a future liquidation price.
// It never supplies withdrawal authority or replaces the entry profitability budget.
type investmentExit struct {
	CollateralRaw           uint64    `json:"collateralRaw"`
	ReferenceUSDCRaw        uint64    `json:"referenceUsdcRaw"`
	QuotedUSDCRaw           uint64    `json:"quotedUsdcRaw"`
	RepaymentUSDCRaw        uint64    `json:"repaymentUsdcRaw"`
	MinimumEquityRaw        uint64    `json:"minimumEquityRaw"`
	ObservedAt              time.Time `json:"observedAt"`
	Slot                    int64     `json:"slot"`
	CashAfterRepaymentRaw   uint64    `json:"cashAfterRepaymentRaw"`
	LastPortionUSDCRaw      uint64    `json:"lastPortionUsdcRaw"`
	LastPortionReferenceRaw uint64    `json:"lastPortionReferenceRaw"`
}

func investmentExitRequired(v *Vault, d Decision) bool {
	if v == nil || !v.CheckInvestmentExits || !requiresNAVForAdd(d) || d.Reason == termKeepAliveReason {
		return false
	}
	return d.Action == SwapStableToCollateralStep || d.Action == SwapDebtToCollateralStep || stepKind(d) == stepUp
}

func investmentSum(a, b uint64) (uint64, error) {
	if a > math.MaxInt64 || b > math.MaxInt64-a {
		return 0, staleHold(investmentExitUnavailable)
	}
	return a + b, nil
}

func investmentExitFloor(raw uint64) uint64 {
	// Round the loss down, so rounding cannot expand the 0.2% allowance.
	return raw - new(big.Int).Quo(new(big.Int).Mul(new(big.Int).SetUint64(raw), new(big.Int).SetUint64(executionPriceLossBPS)), big.NewInt(10_000)).Uint64()
}

// Both admission and preparation call this with newly observed accounts. Shared
// collateral custody is counted once, and every held lane using that mint counts.
func (p lanePricer) priceInvestmentExit(ctx context.Context, o Observation, route RuntimeRoute, accounts []ConfirmedAccount, slot int64, collateralAdded, debtAdded uint64) (investmentExit, error) {
	result, err := p.quoteInvestmentExit(ctx, o, route, accounts, slot, collateralAdded, debtAdded)
	var hold *PreSendHold
	if ctx.Err() == nil && errors.As(err, &hold) && holdCause(hold) == investmentExitSlotsChanged {
		// One new set of quotes can straddle a slot boundary; never compare
		// different banks or retry a price rejection as an availability error.
		return p.quoteInvestmentExit(ctx, o, route, accounts, slot, collateralAdded, debtAdded)
	}
	return result, err
}

func (p lanePricer) quoteInvestmentExit(ctx context.Context, o Observation, route RuntimeRoute, accounts []ConfirmedAccount, slot int64, collateralAdded, debtAdded uint64) (investmentExit, error) {
	r := investmentExit{ObservedAt: time.Now().UTC(), Slot: slot}
	if p.client == nil || collateralAdded == 0 || slot <= 0 || o.Snapshot.Slot <= 0 || slot < o.Snapshot.Slot || slot-o.Snapshot.Slot > observationLagSlots() || !freshAt(r.ObservedAt, o.ObservedAt, exitQuoteInterval) {
		return r, staleHold(investmentExitUnavailable)
	}
	prices, err := batchLanePrices(accounts, route)
	if err != nil {
		return r, staleHoldFrom(err, investmentExitUnavailable)
	}
	lanes := []string{route.Lane}
	seen := map[string]bool{route.Lane: true}
	for _, lane := range o.Snapshot.heldLanes() {
		other, err := p.manifest.route(lane)
		if err != nil {
			return r, staleHoldFrom(err, investmentExitUnavailable)
		}
		if other.Kamino.CollateralMint == route.Kamino.CollateralMint && !seen[lane] {
			lanes, seen[lane] = append(lanes, lane), true
		}
	}
	if len(lanes) > 1 {
		b := o.routeBatch
		if b == nil || b.Slot != o.Snapshot.Slot || b.ObservationID != o.Snapshot.ObservationID || b.ManifestSHA256 != p.manifest.SHA256 {
			return r, staleHold(investmentExitUnavailable)
		}
	}
	r.CollateralRaw = collateralAdded
	var usdcDebt, tokenDebt, redemptionCollateral, redemptionValue uint64
	debtRoute, debtPrices := route, prices
	for i, lane := range lanes {
		other, _ := p.manifest.route(lane)
		batch, batchSlot := accounts, slot
		if i != 0 {
			b := o.routeBatch
			batch, batchSlot = b.Accounts, b.Slot
		}
		state, err := readExitStepState(p.manifest.vault, batch, other, batchSlot)
		if err != nil {
			return r, staleHoldFrom(err, investmentExitUnavailable)
		}
		held, err := state.collateral.redeemLiquidityRaw(state.position.CollateralDepositedRaw)
		if err != nil {
			return r, staleHoldFrom(err, investmentExitUnavailable)
		}
		if i == 0 {
			held, err = investmentSum(held, state.collateralBalance)
		}
		if err == nil {
			r.CollateralRaw, err = investmentSum(r.CollateralRaw, held)
		}
		if err != nil {
			return r, err
		}
		debt := uint64(0)
		if state.position.DebtRaw > 0 {
			bound, err := decodeKaminoPayoffWindow(batch, other, batchSlot, 1)
			if err != nil {
				return r, staleHoldFrom(err, investmentExitUnavailable)
			}
			debt = bound.UpperDebtRaw
		}
		if i == 0 && debtAdded > 0 {
			// Include fees passed by the caller and an interest bound for a fresh
			// execution window, just as for the already borrowed balance.
			reserve, err := kamino.DecodeReserve(chainAccount(accountAt(batch, other.Kamino.DebtReserve), other.Kamino.DebtReserve))
			if err != nil || reserve.SupportedInterestTerms() != nil {
				return r, staleHoldFrom(err, investmentExitUnavailable)
			}
			rate, err := kaminoMaximumBorrowRate(reserve)
			if err != nil {
				return r, staleHoldFrom(err, investmentExitUnavailable)
			}
			elapsed, year := uint64(observationLagSlots()+1), uint64(63_072_000)
			if reserve.InterestRateBasis == kamino.TrueAPRBasis {
				elapsed, year = uint64(kaminoPayoffWindowSeconds), 31_536_000
			}
			added, err := upperKaminoCompoundedDebtSF(new(big.Int).Lsh(new(big.Int).SetUint64(debtAdded), kamino.FractionBits), rate, elapsed, year)
			if err == nil {
				debt, err = investmentSum(debt, added)
			}
			if err != nil {
				return r, staleHoldFrom(err, investmentExitUnavailable)
			}
		}
		if debt == 0 {
			continue
		}
		if hastraLane(other) {
			// Redeem just enough AUTO to repay wYLDS; sell the remaining AUTO.
			// wYLDS itself has no immediate USDC exit.
			amount, value, e := p.investmentRedemption(ctx, batch, other, batchSlot, debt)
			if e != nil {
				return r, e
			}
			redemptionCollateral, err = investmentSum(redemptionCollateral, amount)
			if err == nil {
				redemptionValue, err = investmentSum(redemptionValue, value)
			}
		} else if other.Kamino.DebtMint == fleet.USDCMint {
			usdcDebt, err = investmentSum(usdcDebt, debt)
		} else {
			// Different debt assets need a stateful, multi-route replay.
			if tokenDebt > 0 && debtRoute.Kamino.DebtMint != other.Kamino.DebtMint {
				return r, staleHold(investmentExitUnavailable)
			}
			debtRoute = other
			debtPrices, err = batchLanePrices(batch, other)
			if err == nil {
				tokenDebt, err = investmentSum(tokenDebt, debt)
			}
		}
		if err != nil {
			return r, staleHoldFrom(err, investmentExitUnavailable)
		}
	}
	r.RepaymentUSDCRaw, err = investmentSum(usdcDebt, redemptionValue)
	if err != nil {
		return r, err
	}
	if tokenDebt > 0 {
		mark, err := valueBetweenTokenRaw(tokenDebt, debtPrices.debtDecimals, 6, debtPrices.debt, debtPrices.usdc, true)
		if err != nil {
			return r, staleHoldFrom(err, investmentExitUnavailable)
		}
		r.RepaymentUSDCRaw, err = investmentSum(r.RepaymentUSDCRaw, mark)
		if err != nil {
			return r, err
		}
	}
	r.ReferenceUSDCRaw, err = valueBetweenTokenRaw(r.CollateralRaw, prices.collateralDecimals, 6, prices.collateral, prices.usdc, false)
	if err != nil || r.ReferenceUSDCRaw == 0 || r.ReferenceUSDCRaw > math.MaxInt64 {
		return r, staleHoldFrom(err, investmentExitUnavailable)
	}
	portion, err := executionTokenLimit(prices, route, route.Kamino.CollateralMint)
	if err != nil || portion == 0 {
		return r, staleHoldFrom(err, investmentExitUnavailable)
	}
	if redemptionCollateral >= r.CollateralRaw {
		return r, staleHold(investmentExitExpensive)
	}
	liquidCollateral := r.CollateralRaw - redemptionCollateral
	portion = min(portion, liquidCollateral)
	action, lane := SwapCollateralToStableStep, route.Lane
	if tokenDebt > 0 {
		action, lane = SwapCollateralToDebtStep, debtRoute.Lane
	}
	whole, err := p.investmentExitQuoteEvidence(ctx, lane, action, liquidCollateral, slot)
	if err != nil {
		return r, err
	}
	prefix := investmentQuote{}
	if portion < liquidCollateral {
		prefix, err = p.investmentExitQuoteEvidence(ctx, lane, action, liquidCollateral-portion, slot)
		if err != nil {
			return r, err
		}
		if whole.slot != prefix.slot {
			return r, staleHold(investmentExitSlotsChanged)
		}
		if !slices.Equal(whole.path, prefix.path) || prefix.output >= whole.output {
			return r, staleHold(investmentExitRouteChanged)
		}
	}
	cash, tail := whole.output, whole.output-prefix.output
	if tokenDebt > 0 {
		// Repay in the borrowed asset. Only the remainder crosses into USDC.
		reference, e := valueBetweenTokenRaw(liquidCollateral, prices.collateralDecimals, debtPrices.debtDecimals, prices.collateral, debtPrices.debt, false)
		tailReference, te := valueBetweenTokenRaw(portion, prices.collateralDecimals, debtPrices.debtDecimals, prices.collateral, debtPrices.debt, false)
		if e != nil || te != nil {
			return r, staleHold(investmentExitUnavailable)
		}
		if whole.output <= tokenDebt || whole.output < investmentExitFloor(reference) || tail < investmentExitFloor(tailReference) {
			return r, staleHold(investmentExitExpensive)
		}
		remainder, e := p.investmentExitQuoteEvidence(ctx, lane, SwapDebtToUSDCStep, whole.output-tokenDebt, slot)
		if e != nil {
			return r, e
		}
		// Quoting both legs against unchanged pools is valid only when the
		// second leg does not reuse liquidity consumed by the first.
		if whole.slot != remainder.slot {
			return r, staleHold(investmentExitSlotsChanged)
		}
		if !investmentPathsDisjoint(whole.path, remainder.path) {
			return r, staleHold(investmentExitRouteChanged)
		}
		cash = remainder.output
		if prefix.output > tokenDebt {
			beforeLast, e := p.investmentExitQuoteEvidence(ctx, lane, SwapDebtToUSDCStep, prefix.output-tokenDebt, slot)
			if e != nil {
				return r, e
			}
			if remainder.slot != beforeLast.slot {
				return r, staleHold(investmentExitSlotsChanged)
			}
			if !slices.Equal(remainder.path, beforeLast.path) || beforeLast.output >= cash {
				return r, staleHold(investmentExitRouteChanged)
			}
			tail = cash - beforeLast.output
		} else {
			// The last portion straddles repayment; its full value includes
			// the loan extinguished, while only the surplus becomes cash.
			repaid, e := valueBetweenTokenRaw(tokenDebt-prefix.output, debtPrices.debtDecimals, 6, debtPrices.debt, debtPrices.usdc, false)
			if e != nil {
				return r, staleHoldFrom(e, investmentExitUnavailable)
			}
			tail, e = investmentSum(cash, repaid)
			if e != nil {
				return r, e
			}
		}
	}
	if cash <= usdcDebt {
		return r, staleHold(investmentExitExpensive)
	}
	r.CashAfterRepaymentRaw = cash - usdcDebt
	r.QuotedUSDCRaw, err = investmentSum(r.CashAfterRepaymentRaw, r.RepaymentUSDCRaw)
	if err != nil {
		return r, err
	}
	r.LastPortionUSDCRaw = tail
	r.LastPortionReferenceRaw, err = valueBetweenTokenRaw(portion, prices.collateralDecimals, 6, prices.collateral, prices.usdc, false)
	if err != nil {
		return r, staleHoldFrom(err, investmentExitUnavailable)
	}
	floor := investmentExitFloor(r.ReferenceUSDCRaw)
	if r.QuotedUSDCRaw < floor || r.RepaymentUSDCRaw >= floor || tail < investmentExitFloor(r.LastPortionReferenceRaw) {
		return r, staleHold(investmentExitExpensive)
	}
	if !freshAt(time.Now(), o.ObservedAt, exitQuoteInterval) {
		return r, staleHold(investmentExitUnavailable)
	}
	r.MinimumEquityRaw = floor - r.RepaymentUSDCRaw
	return r, nil
}

func (p lanePricer) investmentRedemption(ctx context.Context, accounts []ConfirmedAccount, route RuntimeRoute, slot int64, debt uint64) (uint64, uint64, error) {
	if p.view == nil {
		return 0, 0, staleHold(investmentExitUnavailable)
	}
	price, now, quoteSlot, err := hastraPriceAt(ctx, p.view, slot)
	if err != nil || !landable(price, now) || quoteSlot < slot || quoteSlot-slot > min(observationLagSlots(), adaptorMaxReportAgeSlots) {
		return 0, 0, staleHoldFrom(err, investmentExitUnavailable)
	}
	amount, err := price.AUTOForWYLDS(debt)
	if err != nil || amount == math.MaxUint64 {
		return 0, 0, staleHoldFrom(err, investmentExitUnavailable)
	}
	out, err := price.WYLDSForAUTO(amount)
	if err == nil && out < debt {
		amount++
		out, err = price.WYLDSForAUTO(amount)
	}
	if err != nil || out < debt {
		return 0, 0, staleHoldFrom(err, investmentExitUnavailable)
	}
	prices, err := batchLanePrices(accounts, route)
	if err != nil {
		return 0, 0, staleHoldFrom(err, investmentExitUnavailable)
	}
	value, err := valueBetweenTokenRaw(debt, prices.debtDecimals, 6, prices.debt, prices.usdc, false)
	reference, e := valueBetweenTokenRaw(amount, prices.collateralDecimals, 6, prices.collateral, prices.usdc, false)
	if err != nil || e != nil {
		return 0, 0, staleHold(investmentExitUnavailable)
	}
	if value < investmentExitFloor(reference) {
		return 0, 0, staleHold(investmentExitExpensive)
	}
	return amount, value, nil
}

type investmentHop struct {
	Pool, Input, Output string
	BPS                 int
}
type investmentQuote struct {
	output uint64
	slot   uint64
	path   []investmentHop
}

// Compare the same acyclic routing and allocations, independent of the order
// Jupiter lists parallel branches. Different allocations still need a new quote.
func investmentQuotePath(q jupiter.Quote) []investmentHop {
	if q.InputMint == "" || q.OutputMint == "" || q.InputMint == q.OutputMint || len(q.RoutePlan) == 0 || len(q.RoutePlan) > jupiterMaxRoutePlanLeg {
		return nil
	}
	var path []investmentHop
	seen := map[string]bool{}
	incoming, outgoing := map[string]int{}, map[string]int{}
	for _, raw := range q.RoutePlan {
		var step struct {
			Percent int `json:"percent"`
			BPS     int `json:"bps"`
			Swap    struct {
				Pool   string `json:"ammKey"`
				Input  string `json:"inputMint"`
				Output string `json:"outputMint"`
			} `json:"swapInfo"`
		}
		if json.Unmarshal(raw, &step) != nil {
			return nil
		}
		// The integer percent can be rounded; basis points are authoritative.
		bps := step.BPS
		if bps == 0 {
			bps = step.Percent * 100
		}
		if bps <= 0 || bps > 10_000 || step.Percent < 0 || step.Percent > 100 ||
			step.Swap.Pool == "" || seen[step.Swap.Pool] || step.Swap.Input == "" || step.Swap.Output == "" || step.Swap.Input == step.Swap.Output {
			return nil
		}
		path = append(path, investmentHop{step.Swap.Pool, step.Swap.Input, step.Swap.Output, bps})
		seen[step.Swap.Pool] = true
		incoming[step.Swap.Output]++
		outgoing[step.Swap.Input] += bps
	}
	if incoming[q.InputMint] != 0 || outgoing[q.OutputMint] != 0 || incoming[q.OutputMint] == 0 {
		return nil
	}
	for _, bps := range outgoing {
		if bps != 10_000 {
			return nil
		}
	}
	queue, visited := []string{q.InputMint}, 0
	for len(queue) > 0 {
		input := queue[0]
		queue = queue[1:]
		visited++
		if input != q.OutputMint && outgoing[input] == 0 {
			return nil
		}
		for _, hop := range path {
			if hop.Input == input {
				incoming[hop.Output]--
				if incoming[hop.Output] == 0 {
					queue = append(queue, hop.Output)
				}
			}
		}
	}
	if visited != len(incoming)+1 {
		return nil
	}
	slices.SortFunc(path, func(a, b investmentHop) int { return strings.Compare(a.Pool, b.Pool) })
	return path
}

func investmentPathsDisjoint(a, b []investmentHop) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	for _, x := range a {
		for _, y := range b {
			if x.Pool == y.Pool {
				return false
			}
		}
	}
	return true
}

func (p lanePricer) investmentExitQuote(ctx context.Context, lane string, action Action, amount uint64, slot int64) (uint64, error) {
	q, err := p.investmentExitQuoteEvidence(ctx, lane, action, amount, slot)
	return q.output, err
}

func (p lanePricer) investmentExitQuoteEvidence(ctx context.Context, lane string, action Action, amount uint64, slot int64) (investmentQuote, error) {
	if conversionVenueHastra(Decision{Action: action, StrategyKey: lane}) {
		if p.view == nil {
			return investmentQuote{}, staleHold(investmentExitUnavailable)
		}
		edge, err := hastraEdgeForRoute(p.manifest.vault, action, lane)
		if err != nil {
			return investmentQuote{}, staleHoldFrom(err, investmentExitUnavailable)
		}
		price, now, quoteSlot, err := hastraPriceAt(ctx, p.view, slot)
		if err != nil || !landable(price, now) || quoteSlot < slot || quoteSlot-slot > min(observationLagSlots(), adaptorMaxReportAgeSlots) {
			return investmentQuote{}, staleHoldFrom(err, investmentExitUnavailable)
		}
		out, err := hastraOutput(edge.leg, price, amount)
		if err != nil || out == 0 {
			return investmentQuote{}, staleHoldFrom(err, investmentExitUnavailable)
		}
		return investmentQuote{output: out, slot: uint64(quoteSlot), path: []investmentHop{{Pool: "hastra", Input: edge.sourceMint, Output: edge.destinationMint, BPS: 10_000}}}, nil
	}
	input, output, _, _, err := jupiterEdgeForRoute(p.manifest.vault, action, lane)
	if err != nil {
		return investmentQuote{}, staleHoldFrom(err, investmentExitUnavailable)
	}
	maxAccounts, err := jupiterSwapMaxAccounts(p.manifest.vault, lane, action)
	if err != nil {
		return investmentQuote{}, staleHoldFrom(err, investmentExitUnavailable)
	}
	request := jupiter.QuoteRequest{InputMint: input, OutputMint: output, Amount: amount, SlippageBPS: withdrawalMaxSlippageBPS, MaxAccounts: maxAccounts, InstructionVersion: "V2"}
	if l, ok := findEarnLane(lane); ok {
		request.Dexes = l.jupiterDexes
	}
	quote, err := p.client.Quote(ctx, request)
	if err != nil {
		return investmentQuote{}, staleHoldFrom(err, investmentExitUnavailable)
	}
	out, _, err := validateJupiterQuoteForRoute(p.manifest.vault, quote, action, amount, lane, withdrawalMaxSlippageBPS)
	lag := uint64(min(observationLagSlots(), adaptorMaxReportAgeSlots))
	if err != nil || quote.ContextSlot == 0 || slot <= 0 || quote.ContextSlot > uint64(slot)+lag || quote.ContextSlot < uint64(slot) && uint64(slot)-quote.ContextSlot > lag {
		return investmentQuote{}, staleHoldFrom(err, investmentExitUnavailable)
	}
	path := investmentQuotePath(quote)
	if len(path) == 0 {
		return investmentQuote{}, staleHold(investmentExitRouteChanged)
	}
	return investmentQuote{output: out, slot: quote.ContextSlot, path: path}, nil
}

func (p lanePricer) protectInvestment(ctx context.Context, d Decision, o Observation, request any) error {
	if !investmentExitRequired(p.manifest.vault, d) {
		return nil
	}
	b := o.routeBatch
	if b == nil || b.Slot != o.Snapshot.Slot || b.ObservationID != o.Snapshot.ObservationID || b.ManifestSHA256 != p.manifest.SHA256 {
		return staleHold(investmentExitUnavailable)
	}
	route, err := p.manifest.route(d.StrategyKey)
	if err != nil {
		return err
	}
	var collateral, debt uint64
	switch r := request.(type) {
	case JupiterSwapRequest:
		collateral = r.QuotedOutputRaw
	case HastraConversionRequest:
		collateral = r.OutputRaw
	case StepRequest:
		collateral = conversionQuotedOutput(r)
		prices, err := batchLanePrices(b.Accounts, route)
		if err != nil {
			return staleHoldFrom(err, investmentExitUnavailable)
		}
		fee, err := kaminoBorrowFeeAtRate(prices.debtBorrowFeeSF, r.Open.AmountRaw)
		if err != nil {
			return err
		}
		debt, err = investmentSum(r.Open.AmountRaw, fee)
		if err != nil {
			return err
		}
	default:
		return staleHold(investmentExitUnavailable)
	}
	_, err = p.priceInvestmentExit(ctx, o, route, b.Accounts, b.Slot, collateral, debt)
	return err
}
