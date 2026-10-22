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
	Operations  []ActivityItem     `json:"operations" nullable:"false"`
	Performance []PerformancePoint `json:"performance" nullable:"false"`
}

func action(e Entry) (string, bool) {
	switch e.(type) {
	case Deposit:
		return "deposit", true
	case Request:
		return "withdraw_request", true
	case Claim:
		return "claim", true
	}
	return "", false
}

func chronological(entries []Entry) []int {
	order := make([]int, 0, len(entries))
	for end := len(entries); end > 0; {
		start := end - 1
		for start > 0 && entries[start-1].flow().Signature == entries[end-1].flow().Signature {
			start--
		}
		for i := start; i < end; i++ {
			order = append(order, i)
		}
		end = start
	}
	return order
}

func amounts(h History, p Position) []*USDCRaw {
	out := make([]*USDCRaw, len(h.Entries))
	var open *Request
	var openAt int
	for _, i := range chronological(h.Entries) {
		switch e := h.Entries[i].(type) {
		case Deposit:
			out[i] = &e.Amount
		case Claim:
			out[i] = e.Paid
			if open != nil && open.Escrow != nil && e.Escrow != nil && *open.Escrow == *e.Escrow {
				out[openAt] = e.Paid
			}
			open = nil
		case Request:
			open, openAt = &e, i
		case Cancel:
			if open != nil {
				out[openAt] = new(USDCRaw)
			}
			open = nil
		case Missing:
			open = nil
		}
	}
	if open != nil && open.Escrow != nil && p.Withdrawal != nil && open.Slot <= p.ObservedSlot && open.Escrow.LP == p.Withdrawal.EscrowedLP {
		out[openAt] = &p.Withdrawal.Payout
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
	if !h.Complete() {
		return nil
	}
	total := p.Value
	if p.Withdrawal != nil {
		total += p.Withdrawal.Payout
	}
	for _, e := range h.Entries {
		if e.flow().Slot > p.ObservedSlot {
			continue
		}
		switch e := e.(type) {
		case Claim:
			if e.Paid == nil {
				return nil
			}
			total += *e.Paid
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
	out := Activity{Operations: make([]ActivityItem, 0, len(h.Entries)), Performance: []PerformancePoint{}}
	for i, e := range h.Entries {
		name, ok := action(e)
		if !ok {
			continue
		}
		f := e.flow()
		item := ActivityItem{
			ConfirmedSlot: strconv.FormatUint(f.Slot, 10), Action: name, AmountRaw: rawString(paid[i]),
			ID: f.Signature.String() + ":" + name, Signature: f.Signature.String(), Status: "confirmed", Timestamp: iso(f.At),
		}
		if r, ok := e.(Request); ok {
			item.RequestedRaw = rawString(r.Requested)
		}
		out.Operations = append(out.Operations, item)
	}
	var principal USDCRaw
	known := true
	for i := len(h.Entries) - 1; i >= 0; i-- {
		switch e := h.Entries[i].(type) {
		case Deposit:
			principal += e.Amount
		case Request:
			if paid[i] == nil {
				known = false
			} else {
				principal -= *paid[i]
			}
		case Missing:
			known = false
			continue
		default:
			continue
		}
		if !known {
			continue
		}
		out.Performance = append(out.Performance, PerformancePoint{EquityUsd: float64(max(principal, 0)) / 1e6, Timestamp: iso(h.Entries[i].flow().At)})
	}
	if len(out.Performance) == 0 {
		return out
	}
	opened := out.Performance[0].Timestamp
	if h.Complete() {
		for _, day := range dayEnd {
			at := iso(day.At)
			if at <= opened {
				continue
			}
			lpThen := p.LP
			for _, e := range h.Entries {
				if f := e.flow(); iso(f.At) > at && f.Slot <= p.ObservedSlot {
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
	group.Go(func() (err error) { position, history, err = r.snapshot(groupCtx, a, now); return err })
	group.Go(func() error { apy = r.APY(); return nil })
	if err := group.Wait(); err != nil {
		return Activity{}, err
	}
	return activity(history, position, apy.DayEnd, r.now()), nil
}
