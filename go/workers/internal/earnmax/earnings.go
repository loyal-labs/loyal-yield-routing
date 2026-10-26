package earnmax

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"math/big"
	"strconv"
	"time"
)

// Performance + SellingCostChange = NetEarned. A cost increase is negative.
// These are return attribution figures, never inputs to a withdrawal.
type Earnings struct {
	PerformanceUsd       float64       `json:"performanceUsd"`
	SellingCostChangeUsd float64       `json:"sellingCostChangeUsd"`
	NetEarnedUsd         float64       `json:"netEarnedUsd"`
	Days                 []EarningsDay `json:"days" nullable:"false"`
}
type EarningsDay struct {
	Date                 string  `json:"date"`
	PerformanceUsd       float64 `json:"performanceUsd"`
	SellingCostChangeUsd float64 `json:"sellingCostChangeUsd"`
	NetEarnedUsd         float64 `json:"netEarnedUsd"`
}
type costPoint struct {
	At         time.Time `json:"at"`
	Slot       uint64    `json:"slot"`
	BeforeFlow bool      `json:"beforeFlow"`
}

// Read the actual deduction attached to the last confirmed NAV report at
// each mark/flow. The per-route nonterminal uniqueness constraint serializes reports,
// so the existing route/created_at index also orders their confirmed NAV updates.
// Missing history or ambiguous same-slot flow ordering is unknown.
func (r *Reader) exitLiabilities(ctx context.Context, points []costPoint) ([]*USDCRaw, error) {
	if r.yield == nil {
		return nil, errNoYield
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	input, err := json.Marshal(points)
	if err != nil {
		return nil, err
	}
	var raw []byte
	err = r.yield.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(
 CASE WHEN o.confirmed_slot IS NULL OR NOT COALESCE(o.expected_effects->'expectedEffects' ? 'returnData', false) OR
 ((p.point->>'beforeFlow')::boolean AND o.confirmed_slot = (p.point->>'slot')::bigint) THEN NULL ELSE
 COALESCE(o.expected_effects->'expectedEffects'->>'exitLiabilityRaw', '0') END ORDER BY p.ordinality), '[]'::jsonb)
 FROM jsonb_array_elements($2::jsonb) WITH ORDINALITY p(point, ordinality)
 LEFT JOIN LATERAL (
 SELECT confirmed_slot, expected_effects FROM loyal_yield.multiply_operations
 WHERE route_key = $1 AND status = 'reconciled'
 AND action IN ('REPORT_NAV', 'VOLTR_ALLOCATE_TO_SQUADS', 'VOLTR_RESTORE_IDLE')
 AND created_at < (p.point->>'at')::timestamptz + interval '1 second'

 AND CASE WHEN (p.point->>'slot')::bigint > 0
 THEN confirmed_slot <= (p.point->>'slot')::bigint
 ELSE updated_at <= (p.point->>'at')::timestamptz END
 ORDER BY created_at DESC, operation_id DESC LIMIT 1
 ) o ON true`, RouteKey, input).Scan(&raw)
	if err != nil {
		return nil, err
	}
	var values []*string
	if err = json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	out := make([]*USDCRaw, len(values))
	for i, v := range values {
		if v == nil {
			continue
		}
		n, err := strconv.ParseInt(*v, 10, 64)
		if err == nil && n >= 0 {
			value := USDCRaw(n)
			out[i] = &value
		}
	}
	return out, nil
}

func (r *Reader) earnings(ctx context.Context, h History, p Position, days []DayEnd, now time.Time) *Earnings {
	if !h.Complete() {
		return nil
	}
	points := make([]costPoint, 0, len(h.Entries)+len(days)+1)
	for _, e := range h.Entries {
		points = append(points, costPoint{At: e.flow().At, Slot: e.flow().Slot, BeforeFlow: true})
	}
	for _, d := range days {
		points = append(points, costPoint{At: d.At})
	}
	points = append(points, costPoint{At: now, Slot: p.ObservedSlot})
	costs, err := r.exitLiabilities(ctx, points)
	if err != nil {
		slog.Warn("Earn MAX cost history unavailable", "error", err)
		return nil
	}
	if len(costs) != len(points) {
		return nil
	}
	return attributeEarnings(h, p, days, now, costs)
}

func allocatedCost(cost *USDCRaw, lp LPRaw, supply uint64) (USDCRaw, bool) {
	if lp == 0 {
		return 0, true
	}
	if cost == nil || *cost < 0 || lp < 0 {
		return 0, false
	}
	if *cost == 0 {
		return 0, true
	}
	if supply == 0 || uint64(lp) > supply {
		return 0, false
	}
	n := new(big.Int).Mul(big.NewInt(int64(*cost)), big.NewInt(int64(lp)))
	n.Quo(n, new(big.Int).SetUint64(supply))
	return USDCRaw(n.Int64()), n.IsInt64()
}

func rawSum(values ...USDCRaw) (USDCRaw, bool) {
	n := new(big.Int)
	for _, v := range values {
		n.Add(n, big.NewInt(int64(v)))
	}
	return USDCRaw(n.Int64()), n.IsInt64()
}

func markValue(lp LPRaw, price float64) (USDCRaw, bool) {
	v := math.Round(float64(lp) * price)
	if lp < 0 || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v >= math.MaxInt64 {
		return 0, false
	}
	return USDCRaw(v), true
}

// A pending withdrawal cannot recover more than its request cap.
func pendingAllowance(cost *USDCRaw, pending *Request, supply uint64, poolValue USDCRaw) (USDCRaw, bool) {
	if supply == 0 || poolValue <= 0 {
		return 0, false
	}
	payable, ok := markValue(pending.Escrow.LP, float64(poolValue)/float64(supply))
	if !ok {
		return 0, false
	}
	reserved, ok := allocatedCost(cost, pending.Escrow.LP, supply)
	return min(reserved, max(0, *pending.Requested-payable)), ok
}

// Replay cash flows at their actual LP supply. Subtract the allowance acquired
// on deposit and add back the allowance retired on claim: flows earn nothing.
func attributeEarnings(h History, p Position, days []DayEnd, now time.Time, costs []*USDCRaw) *Earnings {
	if !h.Complete() || len(costs) != len(h.Entries)+len(days)+1 {
		return nil
	}
	order := chronological(h.Entries)
	var free LPRaw
	var pending *Request
	var principal, costBasis USDCRaw
	next := 0
	apply := func(at time.Time) bool {
		for next < len(order) {
			i := order[next]
			entry := h.Entries[i]
			f := entry.flow()
			if f.At.After(at) {
				break
			}
			next++
			if f.Slot > p.ObservedSlot {
				continue
			}
			lp, ok := rawSum(USDCRaw(free), USDCRaw(f.LPDelta))
			if !ok || lp < 0 {
				return false
			}
			free = LPRaw(lp)
			switch e := entry.(type) {
			case Deposit:
				if f.LPDelta <= 0 {
					return false
				}
				before, ok := allocatedCost(costs[i], free-f.LPDelta, f.PoolLPBefore)
				if !ok {
					return false
				}
				after, ok := allocatedCost(costs[i], free, f.PoolLPAfter)
				cost := after - before
				if !ok {
					return false
				}
				if pending != nil {
					beforePending, ok := pendingAllowance(costs[i], pending, f.PoolLPBefore, f.PoolValueBefore)
					if !ok {
						return false
					}
					afterPending, ok := pendingAllowance(costs[i], pending, f.PoolLPAfter, f.PoolValueAfter)
					if !ok {
						return false
					}
					if cost, ok = rawSum(cost, afterPending, -beforePending); !ok {
						return false
					}
				}
				if principal, ok = rawSum(principal, e.Amount); !ok {
					return false
				}
				if costBasis, ok = rawSum(costBasis, cost); !ok {
					return false
				}
			case Request:
				if pending != nil || e.Escrow == nil || e.Requested == nil {
					return false
				}
				pending = &e
			case Cancel:
				pending = nil
			case Claim:
				if e.Paid == nil || e.Escrow == nil || pending == nil || pending.Escrow == nil || *e.Escrow != *pending.Escrow {
					return false
				}
				cost, ok := allocatedCost(costs[i], e.Escrow.LP, f.PoolLPBefore)
				if !ok {
					return false
				}
				cost = min(cost, max(0, *pending.Requested-*e.Paid))
				if principal, ok = rawSum(principal, -*e.Paid); !ok {
					return false
				}
				before, ok := allocatedCost(costs[i], free, f.PoolLPBefore)
				if !ok {
					return false
				}
				after, ok := allocatedCost(costs[i], free, f.PoolLPAfter)
				if !ok {
					return false
				}
				if costBasis, ok = rawSum(costBasis, -cost, after, -before); !ok {
					return false
				}
				pending = nil
			case Missing:
				return false
			}
		}
		return true
	}
	value := func(netFree USDCRaw, price float64, supply uint64, cost *USDCRaw, current bool) (USDCRaw, USDCRaw, bool) {
		allowance, ok := allocatedCost(cost, free, supply)
		if !ok {
			return 0, 0, false
		}
		net := netFree
		if pending != nil {
			payable, ok := markValue(pending.Escrow.LP, price)
			if !ok {
				return 0, 0, false
			}
			payable = min(payable, *pending.Requested)
			if current {
				if p.Withdrawal == nil || p.Withdrawal.EscrowedLP != pending.Escrow.LP {
					return 0, 0, false
				}
				payable = p.Withdrawal.Payout
			}
			reserved, ok := allocatedCost(cost, pending.Escrow.LP, supply)
			if !ok {
				return 0, 0, false
			}
			reserved = min(reserved, max(0, *pending.Requested-payable))
			if net, ok = rawSum(net, payable); !ok {
				return 0, 0, false
			}
			if allowance, ok = rawSum(allowance, reserved); !ok {
				return 0, 0, false
			}
		}
		pnl, ok := rawSum(net, -principal)
		if !ok {
			return 0, 0, false
		}
		adjustment, ok := rawSum(allowance, -costBasis)
		return pnl, adjustment, ok
	}
	out := &Earnings{Days: []EarningsDay{}}
	var priorNet, priorCost USDCRaw
	var priorDay time.Time
	if len(order) > 0 {
		priorDay = h.Entries[order[0]].flow().At.UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	}
	appendDay := func(at time.Time, net, cost USDCRaw) bool {
		performance, ok := rawSum(net, cost, -priorNet, -priorCost)
		if !ok {
			return false
		}
		netChange, ok := rawSum(net, -priorNet)
		if !ok {
			return false
		}
		costChange, ok := rawSum(priorCost, -cost)
		if !ok {
			return false
		}
		day := at.UTC().Truncate(24 * time.Hour)
		if !day.Before(now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -29)) && (priorDay.IsZero() || day.Equal(priorDay.AddDate(0, 0, 1))) {
			out.Days = append(out.Days, EarningsDay{Date: at.UTC().Format(time.DateOnly), PerformanceUsd: float64(performance) / 1e6, SellingCostChangeUsd: float64(costChange) / 1e6, NetEarnedUsd: float64(netChange) / 1e6})
		}
		priorNet, priorCost, priorDay = net, cost, day
		return true
	}
	for i, d := range days {
		if i > 0 && !days[i-1].At.Before(d.At) {
			return nil
		}
		if !d.At.Before(now) {
			continue
		}
		if !apply(d.At) {
			return nil
		}
		marked, ok := markValue(free, d.Price)
		if !ok {
			return nil
		}
		net, cost, ok := value(marked, d.Price, d.PoolLP, costs[len(h.Entries)+i], false)
		if !ok || !appendDay(d.At, net, cost) {
			return nil
		}
	}
	if !apply(now) || free != p.LP || (pending == nil) != (p.Withdrawal == nil) {
		return nil
	}
	price := 0.0
	if p.PoolLP > 0 {
		price = float64(p.PoolValue) / float64(p.PoolLP)
	}
	net, cost, ok := value(p.Value, price, p.PoolLP, costs[len(costs)-1], true)
	if !ok || !appendDay(now, net, cost) {
		return nil
	}
	performance, ok := rawSum(net, cost)
	if !ok {
		return nil
	}
	out.PerformanceUsd = float64(performance) / 1e6
	out.SellingCostChangeUsd = -float64(cost) / 1e6
	out.NetEarnedUsd = float64(net) / 1e6
	return out
}
