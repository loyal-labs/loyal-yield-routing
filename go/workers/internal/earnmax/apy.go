package earnmax

import (
	"context"
	"math"
	"time"
)

const (
	apyCacheFor        = 5 * time.Minute
	currentApyCacheFor = time.Minute
	apyTimeout         = 3 * time.Second
	apyWindowDays      = 7
	dayMs              = 86_400_000
)

type APYPoint struct {
	APYBps *int64 `json:"apyBps"`
	Date   string `json:"date"`
}

type HistoryPoint struct {
	APYBps     int64  `json:"apyBps"`
	ObservedAt string `json:"observedAt"`
}

type DayEnd struct {
	At    time.Time
	Price float64
}

type APY struct {
	Bps        *int64
	WindowDays *int64
	History    []HistoryPoint
	DayEnd     []DayEnd
}

type LiveShare struct {
	LP, TVL float64
}

type sharePoint struct {
	atMs  int64
	date  string
	price float64
}

func ShareAPY(days []DailyStat, live *LiveShare, now time.Time) APY {
	nowMs := now.UnixMilli()
	points := []sharePoint{}
	for _, day := range days {
		at, err := time.Parse(time.RFC3339, day.Day+"T23:59:59Z")
		if day.TVLRaw > 0 && day.LPRaw > 0 && err == nil {
			points = append(points, sharePoint{atMs: min(at.UnixMilli(), nowMs), date: day.Day, price: day.TVLRaw / day.LPRaw})
		}
	}
	today := now.UTC().Format(time.DateOnly)
	out := APY{History: []HistoryPoint{}, DayEnd: []DayEnd{}}
	for _, p := range points {
		if p.date < today {
			out.DayEnd = append(out.DayEnd, DayEnd{At: time.UnixMilli(p.atMs).UTC(), Price: p.price})
		}
	}
	if live != nil && live.TVL > 0 && live.LP > 0 {
		if n := len(points); n > 0 && points[n-1].date == today {
			points = points[:n-1]
		}
		points = append(points, sharePoint{atMs: nowMs, date: today, price: live.TVL / live.LP})
	}
	annualize := func(from, to sharePoint) (bps *int64, defined bool) {
		years := float64(to.atMs-from.atMs) / (365 * dayMs)
		if years <= 0 {
			return nil, false
		}
		growth := (math.Pow(to.price/from.price, 1/years) - 1) * 10_000
		if math.IsInf(growth, 0) || math.IsNaN(growth) || math.Abs(growth) >= math.MaxInt64 {
			return nil, true
		}
		rounded := roundHalfUp(growth)
		return &rounded, true
	}
	trailing := func(i int) (bps *int64, windowDays int64, full, defined bool) {
		latest, from := points[i], points[0]
		for j := i; j >= 0; j-- {
			if latest.atMs-points[j].atMs >= apyWindowDays*dayMs-dayMs/2 {
				from, full = points[j], true
				break
			}
		}
		if latest.atMs-from.atMs < dayMs/2 {
			return nil, 0, false, false
		}
		bps, defined = annualize(from, latest)
		return bps, min(apyWindowDays, roundHalfUp(float64(latest.atMs-from.atMs)/dayMs)), full, defined
	}
	for i := range points {
		bps, window, full, defined := trailing(i)
		if !defined {
			continue
		}
		if i == len(points)-1 {
			out.Bps, out.WindowDays = bps, &window
		}
		if bps != nil && full {
			out.History = append(out.History, HistoryPoint{APYBps: *bps, ObservedAt: time.UnixMilli(points[i].atMs).UTC().Format(time.RFC3339)})
		}
	}
	return out
}

func (a APY) RNHistory() []APYPoint {
	out := make([]APYPoint, 0, len(a.History))
	for _, p := range a.History {
		bps := p.APYBps
		out = append(out, APYPoint{APYBps: &bps, Date: p.ObservedAt[:len(time.DateOnly)]})
	}
	return out
}

type EarnMaxApy struct {
	APYBps        *int64         `json:"apyBps"`
	APYWindowDays *int64         `json:"apyWindowDays"`
	CurrentApyBps *int64         `json:"currentApyBps"`
	History       []HistoryPoint `json:"history" nullable:"false"`
}

func (r *Reader) PublicAPY() EarnMaxApy {
	apy := r.APY()
	current := r.currentApy()
	return EarnMaxApy{APYBps: apy.Bps, APYWindowDays: apy.WindowDays, CurrentApyBps: current, History: apy.History}
}

func (r *Reader) currentApy() *int64 {
	value, _, _ := r.current.Get(func(ctx context.Context) (*int64, error) {
		return r.CurrentApyBps(ctx, r.now())
	})
	return value
}

func (r *Reader) APY() APY {
	value, have, _ := r.apy.Get(func(ctx context.Context) (APY, error) {
		return loadAPY(ctx, r.now())
	})
	if !have {
		return APY{History: []HistoryPoint{}, DayEnd: []DayEnd{}}
	}
	return value
}

func loadAPY(ctx context.Context, now time.Time) (APY, error) {
	price := make(chan *LiveShare, 1)
	go func() {
		share, err := FetchSharePrice(ctx)
		if err != nil || share.PerLP == 0 || share.TotalValue == nil || *share.TotalValue == 0 {
			price <- nil
			return
		}
		price <- &LiveShare{LP: *share.TotalValue / (share.PerLP / 1000), TVL: *share.TotalValue}
	}()
	days, err := FetchDailyStats(ctx)
	live := <-price
	if err != nil {
		return APY{}, err
	}
	return ShareAPY(days, live, now), nil
}
