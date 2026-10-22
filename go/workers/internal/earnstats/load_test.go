package earnstats

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEarnStatsSubtractBeforeConvertingToUSD(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	snapshot := snapshot{refreshedAt: now, series: json.RawMessage(`[
 {"aumRaw":"9007199254740993","weekStart":"2026-10-05","weekEnd":"2026-10-11"},
 {"aumRaw":"9007199254740992","weekStart":"2026-10-12","weekEnd":"2026-10-18"}
]`)}
	stats, err := snapshot.publicStats(now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.AumDeltaVsPriorWeekUSD == nil || *stats.AumDeltaVsPriorWeekUSD != -0.000001 {
		t.Fatalf("lost the one-unit decrease above JS safe integer range: %+v", stats)
	}
	if stats.AumSeries[0].Value != stats.AumSeries[1].Value {
		t.Fatal("fixture must exercise values that round to the same JS number")
	}
}

func TestEarnStatsRejectStaleAndInvalidSnapshots(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, series string
		age          time.Duration
		fail         bool
	}{
		{"five minutes remains fresh", "[]", 5 * time.Minute, false},
		{"one millisecond past expiry", "[]", 5*time.Minute + time.Millisecond, true},
		{"future timestamp", "[]", -time.Second, false},
		{"missing series", "null", 0, true},
		{"invalid amount", `[{"aumRaw":"unknown","weekStart":"2026-10-05","weekEnd":"2026-10-11"}]`, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stats, err := (snapshot{refreshedAt: now.Add(-tc.age), series: json.RawMessage(tc.series)}).publicStats(now)
			if (err != nil) != tc.fail {
				t.Fatalf("stats=%+v error=%v", stats, err)
			}
			if !tc.fail && (stats.AumDeltaVsPriorWeekUSD != nil || stats.AumSeries == nil) {
				t.Fatal("an empty history must expose an unknown delta and an empty array")
			}
		})
	}
}
