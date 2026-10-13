package backyard

import (
	"context"
	"errors"
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

// leverageStepPricer prices the step that borrows borrowRaw more debt. An
// error is an unknown cost, never zero.
type leverageStepPricer func(borrowRaw uint64) (leverageStepCost, error)

// leverageStepLegs is one step's priced evidence: the conversion quotes of
// the up step and of the down step that unwinds it, at central values, and
// the two transactions' network fees.
type leverageStepLegs struct {
	BorrowFeeMicros int64
	// Entry: BorrowRaw debt in, its quoted collateral out.
	EntryInMicros, EntryOutMicros int64
	// Exit: the entry's quoted collateral in, its quoted debt out.
	ExitInMicros, ExitOutMicros int64
	NetworkMicros               int64
}

// cost is the step's expected cost. A conversion that quotes above the
// central price is not a credit: each swap's expected loss floors at zero.
func (l leverageStepLegs) cost() (leverageStepCost, error) {
	out := leverageStepCost{BorrowFeeMicros: l.BorrowFeeMicros, NetworkMicros: l.NetworkMicros,
		EntrySwapLossMicros: max(l.EntryInMicros-l.EntryOutMicros, 0), ExitSwapLossMicros: max(l.ExitInMicros-l.ExitOutMicros, 0)}
	if l.BorrowFeeMicros < 0 || l.NetworkMicros <= 0 || l.EntryInMicros <= 0 || l.EntryOutMicros <= 0 || l.ExitInMicros <= 0 || l.ExitOutMicros <= 0 {
		return out, budgetHold("leverage_cost_unavailable")
	}
	var err error
	out.TotalMicros, err = budgetSum(out.BorrowFeeMicros, out.EntrySwapLossMicros, out.ExitSwapLossMicros, out.NetworkMicros)
	if err != nil {
		return out, budgetHoldFrom(err, "leverage_cost_unavailable")
	}
	return out, nil
}

// leverageCostStages is the closed set of pricing stages an outage names.
// The cause in a log line or metric label is one of these, never raw RPC,
// HTTP or URL text.
var leverageCostStages = map[string]bool{"inputs": true, "policies": true, "blockhash": true, "entry_quote": true, "exit_quote": true, "price": true, "network_fee": true, "deadline": true}

// leverageCostOutage is a step that could not be priced, at one stage.
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

// leverageCostOutageReason is the sanitized reason of a pricing failure:
// leverage_cost_unavailable(<stage>) for a known stage, the bare reason
// otherwise.
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

// productionLeverageStepPricer prices a held lane's step from o's batch:
// the up and down steps the executor would assemble for it (priceStep), with
// the policies and SOL price read once, on the first step it prices.
func productionLeverageStepPricer(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, m RouteManifest, o Observation, s Snapshot) leverageStepPricer {
	var pricer *lanePricer
	return func(borrowRaw uint64) (leverageStepCost, error) {
		ctx, cancel := context.WithTimeout(ctx, leverageStepPriceTimeout)
		defer cancel()
		route, err := runtimeRoute(s.RouteLane)
		if err != nil || o.routeBatch == nil {
			return leverageStepCost{}, costOutage("inputs", budgetHold("leverage_cost_unavailable"))
		}
		if pricer == nil {
			p, err := newLanePricer(ctx, rpc, view, client, m, s.Slot)
			if err != nil {
				return leverageStepCost{}, err
			}
			pricer = &p
		}
		var topology []string
		if s.PositionCollateralRaw > 0 {
			topology = append(topology, route.Kamino.CollateralReserve)
		}
		if s.PositionDebtRaw > 0 {
			topology = append(topology, route.Kamino.DebtReserve)
		}
		return pricer.priceStep(ctx, o.routeBatch.Accounts, route, topology, s.Slot, borrowRaw)
	}
}
