package backyard

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

func TestExitQuoteRefreshMatchingHoldings(t *testing.T) {
	m := readyWorkerManifest(t)
	route, accounts := onreEntryNAVFixture(t, 0, 2_000_000_000)
	accounts = append(accounts, clockFixture())
	now := time.Now()
	calls := 0
	q := exitQuoter{now: func() time.Time { return now }, price: func(_ context.Context, b heldExitBasis, slot int64, at time.Time) (ExitCost, error) {
		calls++
		return exitCostOf(route.Lane, exitCostQuote, b.heldRaw, b.markValue, b.markValue, b.mark, slot, at)
	}}
	costs := q.quote(context.Background(), m, 77, accounts, []string{route.Lane}, nil)
	q.quote(context.Background(), m, 77, accounts, []string{route.Lane}, costs)
	if calls != 1 {
		t.Fatal("matching full quote re-acquired", calls)
	}
	now = now.Add(exitQuoteInterval + time.Nanosecond)
	costs = q.quote(context.Background(), m, 77, accounts, []string{route.Lane}, costs)
	if calls != 2 || !costs[route.Lane].Quote.At.Equal(now) {
		t.Fatal("expiry did not demand fresh quote", calls)
	}
}

func TestExitQuoteRefreshPreservesUnpricedRetries(t *testing.T) {
	m := readyWorkerManifest(t)
	route, accounts := onreEntryNAVFixture(t, 0, 2_000_000_000)
	accounts = append(accounts, clockFixture())
	now := time.Now()
	calls := 0
	q := exitQuoter{now: func() time.Time { return now }, price: func(context.Context, heldExitBasis, int64, time.Time) (ExitCost, error) {
		calls++
		return ExitCost{}, errors.New("unavailable")
	}}
	costs := fixtureExitCosts(m, 77, accounts)
	realized := *costs[route.Lane].Quote
	realized.Source = exitCostRealized
	costs[route.Lane] = laneExitCosts{Realized: &realized}
	for range 2 {
		q.quote(context.Background(), m, 77, accounts, []string{route.Lane}, costs)
	}
	if calls != 1 {
		t.Fatal("failed demand acquisition unbounded", calls)
	}
	now = now.Add(exitQuoteRetryInterval)
	q.quote(context.Background(), m, 77, accounts, []string{route.Lane}, costs)
	if calls != 2 {
		t.Fatal("historical receipt suppressed demand retry", calls)
	}
	accountAt(accounts, route.CollateralCustody).Data[64]++
	q.quote(context.Background(), m, 77, accounts, []string{route.Lane}, costs)
	if calls != 3 {
		t.Fatal("basis change was suppressed", calls)
	}
}

func TestExitQuoteRefreshFailurePacing(t *testing.T) {
	m := readyWorkerManifest(t)
	route, accounts := onreEntryNAVFixture(t, 0, 2_000_000_000)
	accounts = append(accounts, clockFixture())
	now := time.Now()
	calls := 0
	q := exitQuoter{now: func() time.Time { return now }, record: func(context.Context, ExitCost) (bool, error) { return false, errors.New("diagnostics storage down") }, price: func(_ context.Context, b heldExitBasis, slot int64, at time.Time) (ExitCost, error) {
		calls++
		return exitCostOf(route.Lane, exitCostQuote, b.heldRaw, b.markValue, b.markValue, b.mark, slot, at)
	}}
	costs := q.quote(context.Background(), m, 77, accounts, []string{route.Lane}, nil)
	if calls != 1 || costs[route.Lane].Quote == nil {
		t.Fatal("diagnostic write blocked authoritative local evidence")
	}
}

func TestExitQuoteRefreshInvalidTimes(t *testing.T) {
	m := readyWorkerManifest(t)
	route, accounts := onreEntryNAVFixture(t, 0, 2_000_000_000)
	accounts = append(accounts, clockFixture())
	now := time.Now()
	costs := fixtureExitCosts(m, 77, accounts)
	costs[route.Lane].Quote.At = now.Add(time.Hour)
	calls := 0
	q := exitQuoter{now: func() time.Time { return now }, price: func(context.Context, heldExitBasis, int64, time.Time) (ExitCost, error) {
		calls++
		return ExitCost{}, errors.New("unavailable")
	}}
	for range 2 {
		q.quote(context.Background(), m, 77, accounts, []string{route.Lane}, costs)
	}
	if calls != 1 {
		t.Fatal("future quote suppressed acquisition or repeated failed attempt", calls)
	}
}

func TestExitQuoteRetryHistoryExpiresAcrossChangingMarks(t *testing.T) {
	m := readyWorkerManifest(t)
	route, accounts := onreEntryNAVFixture(t, 0, 2_000_000_000)
	accounts = append(accounts, clockFixture())
	now := time.Now()
	calls := 0
	q := exitQuoter{now: func() time.Time { return now }, price: func(context.Context, heldExitBasis, int64, time.Time) (ExitCost, error) {
		calls++
		return ExitCost{}, errors.New("unavailable")
	}}
	mark := accountAt(accounts, route.Kamino.CollateralReserve).Data[248:256]
	original := binary.LittleEndian.Uint64(mark)
	for i := range 1000 {
		binary.LittleEndian.PutUint64(mark, original+uint64(i))
		now = now.Add(exitQuoteRetryInterval)
		q.quote(context.Background(), m, 77, accounts, []string{route.Lane}, nil)
		if len(q.attempted) > 1 {
			t.Fatalf("retry history retained %d expired bases at tick %d", len(q.attempted), i)
		}
	}
	if calls != 1000 {
		t.Fatal("changed marks lost independent acquisition attempts", calls)
	}
}
