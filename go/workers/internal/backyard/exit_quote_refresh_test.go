package backyard

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

func TestConcurrentHeldValuationsSharePendingQuote(t *testing.T) {
	for name, cancelWaiter := range map[string]bool{"shared result": false, "cancelled waiter": true} {
		t.Run(name, func(t *testing.T) {
			m := readyWorkerManifest(t)
			route, accounts := onreEntryNAVFixture(t, 0, 2_000_000_000)
			accounts = append(accounts, clockFixture())
			now := time.Now()
			started, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			var calls atomic.Int32
			q := exitQuoter{now: func() time.Time { return now }, price: func(ctx context.Context, b heldExitBasis, slot int64, at time.Time) (ExitCost, error) {
				if calls.Add(1) == 1 {
					close(started)
				}
				select {
				case <-ctx.Done():
					return ExitCost{}, ctx.Err()
				case <-release:
					return exitCostOf(route.Lane, exitCostQuote, b.heldRaw, b.markValue, b.markValue, b.mark, slot, at)
				}
			}}
			request := func(ctx context.Context) <-chan exitCosts {
				result := make(chan exitCosts, 1)
				go func() { result <- q.quote(ctx, m, 77, accounts, []string{route.Lane}, nil) }()
				return result
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			owner := request(ctx)
			<-started
			waitCtx, stopWait := context.WithCancel(ctx)
			defer stopWait()
			waiter := request(waitCtx)
			select {
			case <-waiter:
				t.Fatal("concurrent valuation returned unpriced while its quote was in flight")
			case <-time.After(25 * time.Millisecond):
			}
			if cancelWaiter {
				stopWait()
				select {
				case costs := <-waiter:
					if costs[route.Lane].Quote != nil {
						t.Fatal("cancelled waiter fabricated a quote")
					}
				case <-ctx.Done():
					t.Fatal("cancelled waiter waited for the upstream request")
				}
			}
			unblock()
			results := []<-chan exitCosts{owner}
			if !cancelWaiter {
				results = append(results, waiter)
			}
			basis, _, err := readHeldExitBasis(m, accounts, route, 77)
			if err != nil {
				t.Fatal(err)
			}
			for _, result := range results {
				select {
				case costs := <-result:
					if _, ok := costs.applicable(m, 77, now).at(route.Lane, basis.heldRaw, basis.mark); !ok {
						t.Fatal("completed acquisition did not price the held position")
					}
				case <-ctx.Done():
					t.Fatal("quote acquisition did not finish")
				}
			}
			if calls.Load() != 1 {
				t.Fatal("concurrent valuations duplicated the upstream request", calls.Load())
			}
		})
	}
}

func TestDeferredDiscoveryDoesNotDelayHeldPositionValuation(t *testing.T) {
	m := readyWorkerManifest(t)
	route, accounts := onreEntryNAVFixture(t, 0, 2_000_000_000)
	accounts = append(accounts, clockFixture())
	now, calls := time.Now(), 0
	q := exitQuoter{now: func() time.Time { return now }, price: func(_ context.Context, b heldExitBasis, slot int64, at time.Time) (ExitCost, error) {
		calls++
		if calls == 1 {
			return ExitCost{}, &jupiter.DeferredError{Reason: "budget"}
		}
		return exitCostOf(route.Lane, exitCostQuote, b.heldRaw, b.markValue, b.markValue, b.mark, slot, at)
	}}
	q.quote(jupiter.Background(t.Context()), m, 77, accounts, []string{route.Lane}, nil)
	costs := q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, nil)
	if calls != 2 || costs[route.Lane].Quote == nil {
		t.Fatal("local discovery deferral suppressed a fresh held-position quote", calls, costs)
	}
}

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
