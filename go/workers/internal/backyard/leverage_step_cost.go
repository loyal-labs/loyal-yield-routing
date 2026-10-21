package backyard

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

// leverageStepCost is the expected cost of one leverage up step at the
// batch's central prices: what the vault's capital loses by
// taking the step (lanePricer.priceStep). Execution bounds (the conversion's
// minimum output) stay on the step itself; none of them is a cost.
//
// The step is eventually repaid, so its unwind is charged at entry: release
// the increment's collateral, sell it back to debt, repay. An up step that
// only pays for its way in would look profitable while the level it buys is
// never left for free, and the spread rule's down moves sell exactly this
// increment.
type leverageStepCost struct {
	BorrowFeeMicros     usdMicros `json:"borrowFeeMicros"`
	EntrySwapCostMicros usdMicros `json:"entrySwapLossMicros"`
	ExitSwapCostMicros  usdMicros `json:"exitSwapLossMicros"`
	NetworkMicros       usdMicros `json:"networkMicros"`
	TotalMicros         usdMicros `json:"totalMicros"`
}

type usdMicros int64

func sumMicros(values ...usdMicros) (usdMicros, error) {
	var total usdMicros
	for _, value := range values {
		if value < 0 || total > math.MaxInt64-value {
			return 0, staleHold("invalid_budget_accounting")
		}
		total += value
	}
	return total, nil
}

type leverageStepPricer func(borrowRaw uint64) (leverageStepCost, error)

type leverageStepLegs struct {
	BorrowFeeMicros usdMicros
	// Entry: BorrowRaw debt in, its quoted collateral out.
	EntryInMicros, EntryOutMicros usdMicros
	// Exit: the entry's quoted collateral in, its quoted debt out.
	ExitInMicros, ExitOutMicros usdMicros
	NetworkMicros               usdMicros
}

func (l leverageStepLegs) cost() (leverageStepCost, error) {
	out := leverageStepCost{BorrowFeeMicros: l.BorrowFeeMicros, NetworkMicros: l.NetworkMicros,
		EntrySwapCostMicros: l.EntryInMicros - l.EntryOutMicros, ExitSwapCostMicros: l.ExitInMicros - l.ExitOutMicros}
	if l.BorrowFeeMicros < 0 || l.NetworkMicros <= 0 || l.EntryInMicros <= 0 || l.EntryOutMicros <= 0 || l.ExitInMicros <= 0 || l.ExitOutMicros <= 0 {
		return out, staleHold(leverageReasonCostUnavailable)
	}
	fees, err := sumMicros(out.BorrowFeeMicros, out.NetworkMicros)
	if err != nil {
		return out, staleHoldFrom(err, leverageReasonCostUnavailable)
	}
	out.TotalMicros = max(fees+out.EntrySwapCostMicros+out.ExitSwapCostMicros, 0)
	return out, nil
}

func (c leverageStepCost) plus(d leverageStepCost) (leverageStepCost, error) {
	var out leverageStepCost
	for _, v := range []struct {
		into *usdMicros
		a, b usdMicros
	}{
		{&out.BorrowFeeMicros, c.BorrowFeeMicros, d.BorrowFeeMicros},
		{&out.EntrySwapCostMicros, c.EntrySwapCostMicros, d.EntrySwapCostMicros},
		{&out.ExitSwapCostMicros, c.ExitSwapCostMicros, d.ExitSwapCostMicros},
		{&out.NetworkMicros, c.NetworkMicros, d.NetworkMicros},
		{&out.TotalMicros, c.TotalMicros, d.TotalMicros},
	} {
		if v.a > 0 && v.b > math.MaxInt64-v.a || v.a < 0 && v.b < math.MinInt64-v.a {
			return out, staleHold(leverageReasonCostUnavailable)
		}
		*v.into = v.a + v.b
	}
	return out, nil
}

type costStage string

const (
	stageInputs     costStage = "inputs"
	stagePolicies   costStage = "policies"
	stageEntryQuote costStage = "entry_quote"
	stageExitQuote  costStage = "exit_quote"
	stageStepBuild  costStage = "step_build"
	stagePrice      costStage = "price"
	stageNetworkFee costStage = "network_fee"
	stageDeadline   costStage = "deadline"
)

type leverageCostOutage struct {
	stage costStage
	err   error
}

func (e *leverageCostOutage) Error() string {
	return leverageReasonCostUnavailable + "(" + string(e.stage) + ")"
}
func (e *leverageCostOutage) Unwrap() error { return e.err }

func costOutage(stage costStage, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		stage = stageDeadline
	}
	return &leverageCostOutage{stage: stage, err: err}
}

func leverageCostOutageReason(err error) string {
	var outage *leverageCostOutage
	if errors.As(err, &outage) {
		return outage.Error()
	}
	return leverageReasonCostUnavailable
}

const leverageStepPriceTimeout = 15 * time.Second

func productionLeverageStepPricer(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, m RouteManifest, o Observation, s Snapshot) leverageStepPricer {
	return func(borrowRaw uint64) (leverageStepCost, error) {
		ctx, cancel := context.WithTimeout(ctx, leverageStepPriceTimeout)
		defer cancel()
		route, err := m.route(s.RouteLane)
		if err != nil || o.routeBatch == nil {
			return leverageStepCost{}, costOutage(stageInputs, staleHold(leverageReasonCostUnavailable))
		}
		pricer, err := newLanePricer(ctx, rpc, view, client, m, s.Slot)
		if err != nil {
			return leverageStepCost{}, err
		}
		topology, err := observedObligationTopology(o.routeBatch.Accounts, route)
		if err != nil {
			return leverageStepCost{}, costOutage(stageInputs, err)
		}
		return pricer.priceStep(ctx, o.routeBatch.Accounts, route, topology, s.Slot, borrowRaw)
	}
}
