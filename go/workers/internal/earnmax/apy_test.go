package earnmax

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestShareAPYLastPointIsTrailingWeek(t *testing.T) {
	var days []DailyStat
	for i := range 8 {
		days = append(days, DailyStat{Day: time.Date(2026, 10, 10+i, 0, 0, 0, 0, time.UTC).Format(time.DateOnly), TVLRaw: 1_000_000 + float64(i)*100, LPRaw: 1_000_000})
	}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	got := ShareAPY(days, &LiveShare{LP: 1_000_000, TVL: 997_600}, now)
	if got.Bps == nil {
		t.Fatal("headline undefined")
	}
	last := got.History[len(got.History)-1]
	if last.APYBps != *got.Bps {
		t.Fatalf("last point %d, headline %d", last.APYBps, *got.Bps)
	}
}

func TestShareAPYHistoryStartsWhereAFullWindowExists(t *testing.T) {
	var days []DailyStat
	for i := range 10 {
		days = append(days, DailyStat{Day: time.Date(2026, 10, 10+i, 0, 0, 0, 0, time.UTC).Format(time.DateOnly), TVLRaw: 1_000_000 + float64(i)*100, LPRaw: 1_000_000})
	}
	got := ShareAPY(days, nil, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	if first := got.History[0].ObservedAt; first != "2026-10-17T23:59:59Z" {
		t.Fatalf("first history point %s, want 2026-10-17T23:59:59Z", first)
	}
}

func TestPublicAPYShowsTheCurrentRateNotTheRealizedDip(t *testing.T) {
	realizedDip, current := int64(-207), int64(1631)
	realized := APY{Bps: &realizedDip, History: []HistoryPoint{
		{APYBps: 1558, ObservedAt: "2026-10-23T23:59:59Z", day: "2026-10-23"},
		{APYBps: 1670, ObservedAt: "2026-10-24T23:59:59Z", day: "2026-10-24"},
		{APYBps: 127, ObservedAt: "2026-10-25T23:59:59Z", day: "2026-10-25"},
		{APYBps: -207, ObservedAt: "2026-10-26T02:59:00Z", day: "2026-10-26"},
	}}
	rate := CurrentRate{Bps: &current, Days: []RateDay{{
		Day: "2026-10-25", StartMs: time.Date(2026, 10, 25, 7, 0, 0, 0, time.UTC).UnixMilli(),
		APYBps: 1631, ObservedAtMs: time.Date(2026, 10, 26, 3, 10, 0, 0, time.UTC).UnixMilli(),
	}}}
	got := publicAPY(realized, rate)
	if got.APYBps == nil {
		t.Fatal("headline undefined, want 1631")
	}
	if *got.APYBps != 1631 {
		t.Fatalf("headline %d, want 1631", *got.APYBps)
	}
	want := []string{"1558@2026-10-23T23:59:59Z", "1670@2026-10-24T23:59:59Z", "1631@2026-10-26T03:10:00Z"}
	var history []string
	for _, p := range got.History {
		history = append(history, fmt.Sprintf("%d@%s", p.APYBps, p.ObservedAt))
	}
	if !slices.Equal(history, want) {
		t.Fatalf("history %q, want %q", history, want)
	}
	if dates := rnHistory(got.History); dates[2].Date != "2026-10-25" {
		t.Fatalf("app history date %s, want 2026-10-25", dates[2].Date)
	}
}
