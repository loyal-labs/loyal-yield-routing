package earnmax

import (
	"context"
	"sort"
	"strconv"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"golang.org/x/sync/errgroup"
)

const dustRaw USDCRaw = 10_000

type ActivityItem struct {
	ConfirmedSlot string  `json:"confirmedSlot"`
	Action        string  `json:"action"`
	AmountRaw     *string `json:"amountRaw"`
	ID            string  `json:"id"`
	RequestedRaw  *string `json:"requestedRaw"`
	Signature     string  `json:"signature"`
	Status        string  `json:"status"`
	Timestamp     string  `json:"timestamp"`
}

type PerformancePoint struct {
	EquityUsd float64 `json:"equityUsd"`
	Timestamp string  `json:"timestamp"`
}

type Activity struct {
	Operations  []ActivityItem     `json:"operations"`
	Performance []PerformancePoint `json:"performance"`
}

func action(e Entry) string {
	switch e.(type) {
	case Deposit:
		return "deposit"
	case Request:
		return "withdraw_request"
	case Claim:
		return "claim"
	}
	panic("earnmax: unknown history entry")
}

func amounts(h History, p Position) []*USDCRaw {
	out := make([]*USDCRaw, len(h.Entries))
	var laterClaim *USDCRaw
	for i, e := range h.Entries {
		switch e := e.(type) {
		case Deposit:
			out[i] = &e.Amount
		case Claim:
			out[i] = &e.Paid
			laterClaim = &e.Paid
		case Request:
			out[i], laterClaim = laterClaim, nil
			if out[i] == nil && p.Withdrawal != nil {
				out[i] = &p.Withdrawal.Payout
			}
		}
	}
	return out
}

func rawString[T ~int64](v *T) *string {
	if v == nil {
		return nil
	}
	s := strconv.FormatInt(int64(*v), 10)
	return &s
}

func earned(h History, p Position) *USDCRaw {
	if !h.Complete {
		return nil
	}
	total := p.Value
	if p.Withdrawal != nil {
		total += p.Withdrawal.Payout
	}
	for _, e := range h.Entries {
		switch e := e.(type) {
		case Claim:
			total += e.Paid
		case Deposit:
			total -= e.Amount
		}
	}
	if total < 0 && -total < dustRaw {
		total = 0
	}
	return &total
}

func activity(h History, p Position, dayEnd []DayEnd, now time.Time) Activity {
	paid := amounts(h, p)
	out := Activity{Operations: make([]ActivityItem, len(h.Entries)), Performance: []PerformancePoint{}}
	for i, e := range h.Entries {
		f := e.flow()
		item := ActivityItem{
			ConfirmedSlot: strconv.FormatUint(f.Slot, 10), Action: action(e), AmountRaw: rawString(paid[i]),
			ID: f.Signature.String() + ":" + action(e), Signature: f.Signature.String(), Status: "confirmed", Timestamp: iso(f.At),
		}
		if r, ok := e.(Request); ok {
			item.RequestedRaw = rawString(r.Requested)
		}
		out.Operations[i] = item
	}
	var principal USDCRaw
	for i := len(h.Entries) - 1; i >= 0; i-- {
		switch h.Entries[i].(type) {
		case Deposit:
			principal += valueOr(paid[i])
		case Request:
			principal -= valueOr(paid[i])
		default:
			continue
		}
		out.Performance = append(out.Performance, PerformancePoint{EquityUsd: float64(max(principal, 0)) / 1e6, Timestamp: iso(h.Entries[i].flow().At)})
	}
	if len(out.Performance) == 0 {
		return out
	}
	opened := out.Performance[0].Timestamp
	if h.Complete {
		for _, day := range dayEnd {
			at := iso(day.At)
			if at <= opened {
				continue
			}
			lpThen := p.LP
			for _, e := range h.Entries {
				if f := e.flow(); iso(f.At) > at {
					lpThen -= f.LPDelta
				}
			}
			if lpThen < 0 {
				continue
			}
			out.Performance = append(out.Performance, PerformancePoint{EquityUsd: float64(lpThen) * day.Price / 1e6, Timestamp: at})
		}
	}
	out.Performance = append(out.Performance, PerformancePoint{EquityUsd: float64(p.Value) / 1e6, Timestamp: iso(now)})
	sort.SliceStable(out.Performance, func(i, j int) bool { return out.Performance[i].Timestamp < out.Performance[j].Timestamp })
	return out
}

func valueOr(v *USDCRaw) USDCRaw {
	if v == nil {
		return 0
	}
	return *v
}

func (r *Reader) Activity(ctx context.Context, settings solana.PublicKey) (Activity, error) {
	a, err := AuthorityOf(settings)
	if err != nil {
		return Activity{}, err
	}
	now := r.now()
	var (
		position Position
		history  History
		apy      APY
	)
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() (err error) { position, err = r.Position(groupCtx, a, now); return err })
	group.Go(func() (err error) { history, err = r.History(groupCtx, a); return err })
	group.Go(func() error { apy = r.APY(); return nil })
	if err := group.Wait(); err != nil {
		return Activity{}, err
	}
	return activity(history, position, apy.DayEnd, r.now()), nil
}
