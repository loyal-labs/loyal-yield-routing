package backyard

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

type exitCostSource string

const (
	exitCostQuote    exitCostSource = "quote"
	exitCostRealized exitCostSource = "realized"
)

type microBPS uint64

const microBPSWhole microBPS = 10_000_000_000

const exitQuoteInterval = time.Hour

type ExitCost struct {
	Lane                string
	BPS                 microBPS
	QuotedCollateralRaw uint64
	MarkSF              kamino.ScaledFraction
	Source              exitCostSource
	Slot                int64
	At                  time.Time
}

func newExitCost(c ExitCost) (ExitCost, error) {
	if !earnHeldLane(c.Lane) || c.BPS == 0 || c.BPS >= microBPSWhole || c.QuotedCollateralRaw == 0 ||
		littleInt(c.MarkSF[:]).Sign() <= 0 || c.Slot <= 0 || c.At.IsZero() ||
		(c.Source != exitCostQuote && c.Source != exitCostRealized) {
		return ExitCost{}, fmt.Errorf("invalid_exit_cost")
	}
	return c, nil
}

func exitCostOf(lane string, source exitCostSource, soldRaw, markValueRaw, returnedRaw uint64, mark kamino.ScaledFraction, slot int64, at time.Time) (ExitCost, error) {
	if markValueRaw == 0 || returnedRaw >= markValueRaw {
		return ExitCost{}, fmt.Errorf("exit_cost_not_positive")
	}
	bps := new(big.Int).Mul(new(big.Int).SetUint64(markValueRaw-returnedRaw), new(big.Int).SetUint64(uint64(microBPSWhole)))
	bps.Add(bps, new(big.Int).SetUint64(markValueRaw-1)).Quo(bps, new(big.Int).SetUint64(markValueRaw))
	if !bps.IsUint64() {
		return ExitCost{}, fmt.Errorf("invalid_exit_cost")
	}
	return newExitCost(ExitCost{Lane: lane, BPS: microBPS(bps.Uint64()), QuotedCollateralRaw: soldRaw, MarkSF: mark, Source: source, Slot: slot, At: at})
}

func (c ExitCost) fresh(heldRaw uint64, mark kamino.ScaledFraction) bool {
	return c.Source == exitCostQuote && c.QuotedCollateralRaw >= heldRaw && c.MarkSF == mark
}

func (c ExitCost) liabilityRaw(valueRaw uint64) (uint64, error) {
	cost := new(big.Int).Mul(new(big.Int).SetUint64(valueRaw), new(big.Int).SetUint64(uint64(c.BPS)))
	cost.Quo(cost, new(big.Int).SetUint64(uint64(microBPSWhole)))
	if !cost.IsUint64() || cost.Uint64() > valueRaw {
		return 0, fmt.Errorf("exit cost exceeds the collateral value")
	}
	return cost.Uint64(), nil
}

func (c ExitCost) evidence() string {
	return fmt.Sprintf("exit:%s:%s:%d:%d:%x:%d", c.Lane, c.Source, c.BPS, c.QuotedCollateralRaw, c.MarkSF, c.Slot)
}

type laneExitCosts struct {
	Quote, Realized *ExitCost
}

type exitCosts map[string]laneExitCosts

func (e exitCosts) at(lane string, heldRaw uint64, mark kamino.ScaledFraction) (ExitCost, error) {
	costs := e[lane]
	if costs.Quote != nil && costs.Quote.fresh(heldRaw, mark) {
		return *costs.Quote, nil
	}
	if costs.Realized != nil {
		return *costs.Realized, nil
	}
	return ExitCost{}, fmt.Errorf("lane %s has no fresh exit quote and no realized exit cost", lane)
}

func (e exitCosts) quoteDue(b heldExitBasis, lastAttempt, now time.Time) bool {
	quote := e[b.st.route.Lane].Quote
	return (quote == nil || !quote.fresh(b.heldRaw, b.mark)) && now.Sub(lastAttempt) >= exitQuoteInterval
}

type exitQuoter struct {
	price     func(context.Context, heldExitBasis, int64, time.Time) (ExitCost, error)
	record    func(context.Context, ExitCost) (bool, error)
	now       func() time.Time
	events    *events
	attempted map[string]time.Time
}

type heldExitBasis struct {
	st                 stepState
	heldRaw, markValue uint64
	mark               kamino.ScaledFraction
}

func readHeldExitBasis(m RouteManifest, o Observation, s Snapshot) (heldExitBasis, bool, error) {
	if o.routeBatch == nil || !s.HasPosition {
		return heldExitBasis{}, false, nil
	}
	route, err := m.route(s.RouteLane)
	if err != nil {
		return heldExitBasis{}, false, err
	}
	st, err := readStepState(m.vault, o.routeBatch.Accounts, route, s.Slot)
	if err != nil || st.position.CollateralDepositedRaw == 0 {
		return heldExitBasis{}, false, err
	}
	if st.cash, err = vaultCustodyRaw(m.vault, o.routeBatch.Accounts, m.vault.cash, fleet.USDCMint, bridgeTokenProgram); err != nil {
		return heldExitBasis{}, false, err
	}
	liquidity, err := st.collateral.redeemLiquidityRaw(st.position.CollateralDepositedRaw)
	if err != nil || liquidity > math.MaxUint64-st.collateralBalance {
		return heldExitBasis{}, false, staleHoldFrom(err, "exit_quote_unavailable")
	}
	prices, err := batchLanePrices(o.routeBatch.Accounts, route)
	if err != nil {
		return heldExitBasis{}, false, err
	}
	held := liquidity + st.collateralBalance
	value, err := prices.collateralMicros(held)
	if err != nil {
		return heldExitBasis{}, false, err
	}
	return heldExitBasis{st: st, heldRaw: held, markValue: uint64(value), mark: prices.collateral}, true, nil
}

type exitCostRow struct {
	Lane                string    `json:"lane"`
	Source              string    `json:"source"`
	BPS                 uint64    `json:"bps"`
	QuotedCollateralRaw string    `json:"quotedCollateralRaw"`
	MarkSF              string    `json:"markSf"`
	Slot                int64     `json:"slot"`
	At                  time.Time `json:"at"`
}

const exitCostsSQL = `COALESCE((SELECT jsonb_agg(jsonb_build_object('lane', lane, 'source', source, 'bps', bps_micro, 'quotedCollateralRaw', quoted_collateral_raw::text, 'markSf', mark_sf::text, 'slot', slot, 'at', recorded_at)) FROM loyal_yield.backyard_lane_exit_costs), '[]'::jsonb)`

func decodeExitCosts(data []byte) (exitCosts, error) {
	var rows []exitCostRow
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, staleHold("invalid_lane_exit_costs")
	}
	out := exitCosts{}
	for _, row := range rows {
		if !earnHeldLane(row.Lane) {
			return nil, staleHold("unknown_exit_cost_lane:" + row.Lane)
		}
		quoted, okQuoted := new(big.Int).SetString(row.QuotedCollateralRaw, 10)
		mark, okMark := new(big.Int).SetString(row.MarkSF, 10)
		if !okQuoted || !okMark || !quoted.IsUint64() || mark.Sign() <= 0 || mark.BitLen() > 128 {
			return nil, staleHold("invalid_lane_exit_costs")
		}
		var sf kamino.ScaledFraction
		putScaledInt(sf[:], mark)
		cost, err := newExitCost(ExitCost{Lane: row.Lane, BPS: microBPS(row.BPS), QuotedCollateralRaw: quoted.Uint64(), MarkSF: sf, Source: exitCostSource(row.Source), Slot: row.Slot, At: row.At})
		if err != nil {
			return nil, staleHold("invalid_lane_exit_costs")
		}
		costs := out[row.Lane]
		if cost.Source == exitCostQuote {
			costs.Quote = &cost
		} else {
			costs.Realized = &cost
		}
		out[row.Lane] = costs
	}
	return out, nil
}

func putScaledInt(dst []byte, value *big.Int) {
	bytes := value.Bytes()
	for i := range bytes {
		dst[i] = bytes[len(bytes)-1-i]
	}
}

func (d *Database) RecordExitCost(ctx context.Context, cost ExitCost) error {
	if _, err := newExitCost(cost); err != nil {
		return err
	}
	_, err := d.pool.Exec(ctx, recordExitCostSQL, exitCostArgs(cost)...)
	return err
}

const recordExitCostSQL = `INSERT INTO loyal_yield.backyard_lane_exit_costs AS c (lane, source, bps_micro, quoted_collateral_raw, mark_sf, slot, recorded_at)
	VALUES ($1, $2, $3, $4::numeric, $5::numeric, $6, $7)
	ON CONFLICT (lane, source) DO UPDATE SET bps_micro = EXCLUDED.bps_micro, quoted_collateral_raw = EXCLUDED.quoted_collateral_raw,
		mark_sf = EXCLUDED.mark_sf, slot = EXCLUDED.slot, recorded_at = EXCLUDED.recorded_at
	WHERE EXCLUDED.slot >= c.slot AND NOT (c.source = 'quote' AND c.mark_sf = EXCLUDED.mark_sf AND c.quoted_collateral_raw > EXCLUDED.quoted_collateral_raw)`

func exitCostArgs(cost ExitCost) []any {
	return []any{cost.Lane, string(cost.Source), int64(cost.BPS), fmt.Sprint(cost.QuotedCollateralRaw), littleInt(cost.MarkSF[:]).String(), cost.Slot, cost.At}
}

type ExpectedSale struct {
	Lane          string                `json:"lane"`
	SoldRaw       uint64                `json:"soldRaw"`
	MarkValueRaw  uint64                `json:"markValueRaw"`
	MarkSF        kamino.ScaledFraction `json:"markSf"`
	OutputAccount string                `json:"outputAccount"`
}

type realizedSale struct {
	ExpectedSale
	ReturnedRaw uint64 `json:"returnedRaw"`
	Slot        int64  `json:"slot"`
}

func collateralSale(accounts []ConfirmedAccount, route RuntimeRoute, action Action, soldRaw uint64, effects ExpectedEffects) *ExpectedSale {
	if (action != SwapCollateralToStableStep && action != SwapCollateralToDebtStep) || len(effects.Accounts) != 2 ||
		effects.Accounts[1].Mint != fleet.USDCMint || soldRaw == 0 {
		return nil
	}
	prices, err := batchLanePrices(accounts, route)
	if err != nil {
		return nil
	}
	value, err := prices.collateralMicros(soldRaw)
	if err != nil || value <= 0 {
		return nil
	}
	return &ExpectedSale{Lane: route.Lane, SoldRaw: soldRaw, MarkValueRaw: uint64(value), MarkSF: prices.collateral, OutputAccount: effects.Accounts[1].Address}
}

func withStepSale(accounts []ConfirmedAccount, route RuntimeRoute, evidence StepExecutionEvidence) StepExecutionEvidence {
	switch r := evidence.Request; {
	case r.Swap != nil:
		evidence.ExpectedEffects.Sale = collateralSale(accounts, route, r.Swap.Action, r.Swap.AmountRaw, evidence.ExpectedEffects)
	case r.Hastra != nil:
		evidence.ExpectedEffects.Sale = collateralSale(accounts, route, r.Hastra.Action, r.Hastra.AmountRaw, evidence.ExpectedEffects)
	}
	return evidence
}

func (s realizedSale) exitCost(at time.Time) (ExitCost, bool) {
	cost, err := exitCostOf(s.Lane, exitCostRealized, s.SoldRaw, s.MarkValueRaw, s.ReturnedRaw, s.MarkSF, s.Slot, at)
	return cost, err == nil
}
