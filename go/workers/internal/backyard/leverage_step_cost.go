package backyard

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

// leverageStepCost is the expected cost of one leverage up step, in USDC
// micros at the batch's central prices: what the vault's capital loses by
// taking the step (lanePricer.priceStep). Execution bounds (the conversion's
// minimum output) stay on the step itself; none of them is a cost.
//
// The step is eventually repaid, so its unwind is charged at entry: release
// the increment's collateral, sell it back to debt, repay. An up step that
// only pays for its way in would look profitable while the level it buys is
// never left for free, and the spread rule's down moves sell exactly this
// increment.
type leverageStepCost struct {
	BorrowFeeMicros     int64 `json:"borrowFeeMicros"`
	EntrySwapLossMicros int64 `json:"entrySwapLossMicros"`
	ExitSwapLossMicros  int64 `json:"exitSwapLossMicros"`
	NetworkMicros       int64 `json:"networkMicros"`
	TotalMicros         int64 `json:"totalMicros"`
}

type leverageStepPricer func(borrowRaw uint64) (leverageStepCost, error)

type leverageStepLegs struct {
	BorrowFeeMicros int64
	// Entry: BorrowRaw debt in, its quoted collateral out.
	EntryInMicros, EntryOutMicros int64
	// Exit: the entry's quoted collateral in, its quoted debt out.
	ExitInMicros, ExitOutMicros int64
	NetworkMicros               int64
}

// cost is the step's expected cost. Each swap's loss is signed, against
// the batch's central price: a collateral the up step buys under its mark
// gains there what the down step's sale loses. The round trip never costs
// less than zero.
func (l leverageStepLegs) cost() (leverageStepCost, error) {
	out := leverageStepCost{BorrowFeeMicros: l.BorrowFeeMicros, NetworkMicros: l.NetworkMicros,
		EntrySwapLossMicros: l.EntryInMicros - l.EntryOutMicros, ExitSwapLossMicros: l.ExitInMicros - l.ExitOutMicros}
	if l.BorrowFeeMicros < 0 || l.NetworkMicros <= 0 || l.EntryInMicros <= 0 || l.EntryOutMicros <= 0 || l.ExitInMicros <= 0 || l.ExitOutMicros <= 0 {
		return out, budgetHold("leverage_cost_unavailable")
	}
	fees, err := budgetSum(out.BorrowFeeMicros, out.NetworkMicros)
	if err != nil {
		return out, budgetHoldFrom(err, "leverage_cost_unavailable")
	}
	out.TotalMicros = max(fees+out.EntrySwapLossMicros+out.ExitSwapLossMicros, 0)
	return out, nil
}

func (c leverageStepCost) plus(d leverageStepCost) (leverageStepCost, error) {
	var out leverageStepCost
	for _, v := range []struct {
		into *int64
		a, b int64
	}{
		{&out.BorrowFeeMicros, c.BorrowFeeMicros, d.BorrowFeeMicros},
		{&out.EntrySwapLossMicros, c.EntrySwapLossMicros, d.EntrySwapLossMicros},
		{&out.ExitSwapLossMicros, c.ExitSwapLossMicros, d.ExitSwapLossMicros},
		{&out.NetworkMicros, c.NetworkMicros, d.NetworkMicros},
		{&out.TotalMicros, c.TotalMicros, d.TotalMicros},
	} {
		if v.a > 0 && v.b > math.MaxInt64-v.a || v.a < 0 && v.b < math.MinInt64-v.a {
			return out, budgetHold("leverage_cost_unavailable")
		}
		*v.into = v.a + v.b
	}
	return out, nil
}

// leverageCostStages is the closed set of pricing stages an outage names.
// The cause in a log line or metric label is one of these, never raw RPC,
// HTTP or URL text.
var leverageCostStages = map[string]bool{"inputs": true, "policies": true, "entry_quote": true, "exit_quote": true, "step_build": true, "price": true, "network_fee": true, "deadline": true}

type leverageCostOutage struct {
	stage string
	err   error
}

func (e *leverageCostOutage) Error() string { return "leverage_cost_unavailable(" + e.stage + ")" }
func (e *leverageCostOutage) Unwrap() error { return e.err }

func costOutage(stage string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		stage = "deadline"
	}
	return &leverageCostOutage{stage: stage, err: err}
}

func leverageCostOutageReason(err error) string {
	var outage *leverageCostOutage
	if errors.As(err, &outage) && leverageCostStages[outage.stage] {
		return "leverage_cost_unavailable(" + outage.stage + ")"
	}
	return "leverage_cost_unavailable"
}

// leverageStepPriceTimeout is the step pricer's own deadline. It runs after
// the selector's evaluation, which may use most of the 30 s sample deadline
// on its quote ladders (selector_evaluate_timeout several times an hour), so
// sharing that deadline starved the pricer (live 2026-10-11 04:58:55: an
// approval revoked as leverage_cost_unavailable).
const leverageStepPriceTimeout = 15 * time.Second

func productionLeverageStepPricer(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, m RouteManifest, o Observation, s Snapshot) leverageStepPricer {
	return func(borrowRaw uint64) (leverageStepCost, error) {
		ctx, cancel := context.WithTimeout(ctx, leverageStepPriceTimeout)
		defer cancel()
		route, err := m.route(s.RouteLane)
		if err != nil || o.routeBatch == nil {
			return leverageStepCost{}, costOutage("inputs", budgetHold("leverage_cost_unavailable"))
		}
		pricer, err := newLanePricer(ctx, rpc, view, client, m, s.Slot)
		if err != nil {
			return leverageStepCost{}, err
		}
		topology, err := observedObligationTopology(o.routeBatch.Accounts, route)
		if err != nil {
			return leverageStepCost{}, costOutage("inputs", err)
		}
		return pricer.priceStep(ctx, o.routeBatch.Accounts, route, topology, s.Slot, borrowRaw)
	}
}
