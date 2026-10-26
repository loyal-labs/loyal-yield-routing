package webapp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestEarningsReadAnswersAtOnceWhileOtherRebuildsHoldEverySlot(t *testing.T) {
	for range cap(earningsRefreshLockSlots) {
		earningsRefreshLockSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for range cap(earningsRefreshLockSlots) {
			<-earningsRefreshLockSlots
		}
	})
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	generatedAt := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	unverified := &earningsSnapshotRecord{generatedAt: generatedAt, payload: EarningsRangeSet{Outcome: "ready", Freshness: "fresh"}}
	read := func(snapshot *earningsSnapshotRecord) (EarningsRangeSet, time.Duration, error) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		deps := earningsReadDeps{
			loadSnapshot: func(context.Context, earningsReadInput, string) (*earningsSnapshotRecord, error) {
				return snapshot, nil
			},
			loadSourceRevision: func(context.Context, earningsReadInput) (string, error) { return "revision", nil },
			now:                func() time.Time { return generatedAt.Add(time.Hour) },
		}
		input := earningsReadInput{cluster: "mainnet", settings: "settings", timezone: "UTC", vaultIndex: 1, walletAddress: "wallet"}
		start := time.Now()
		payload, err := readCachedEarnEarningsRangeSet(ctx, nil, input, func(func(context.Context)) {}, deps)
		return payload, time.Since(start), err
	}

	payload, took, err := read(unverified)
	if err != nil || took > 200*time.Millisecond || payload.Freshness != "stale" || payload.StaleReason == nil || *payload.StaleReason != "refresh_in_progress" {
		t.Fatalf("with a stored snapshot: took %s err=%v freshness=%q, want the snapshot served stale for refresh_in_progress at once", took, err, payload.Freshness)
	}

	_, took, err = read(nil)
	var unavailable *earningsUnavailableError
	if !errors.As(err, &unavailable) || unavailable.code != "earnings_unavailable" || took > 200*time.Millisecond {
		t.Fatalf("without a snapshot: took %s err=%v, want earnings_unavailable at once", took, err)
	}
	if !strings.Contains(logs.String(), `"msg":"earnings-read unavailable"`) || !strings.Contains(logs.String(), `"cause":"refresh_in_progress"`) {
		t.Fatalf("logs %s, want earnings-read unavailable with cause refresh_in_progress", logs.String())
	}
}
