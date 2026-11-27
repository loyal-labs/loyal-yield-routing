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

func TestExitQuoteConfirmsSpikeAgainstSplitBrainPeersWithoutHidingRealDrop(t *testing.T) {
	m := readyWorkerManifest(t)
	route, accounts := onreEntryNAVFixture(t, 0, 2_000_000_000)
	accounts = append(accounts, clockFixture())
	basis, held, err := readHeldExitBasis(m, accounts, route, 77)
	if err != nil || !held {
		t.Fatal(err, held)
	}
	makeCost := func(bps microBPS, at time.Time) ExitCost {
		return ExitCost{
			Lane: route.Lane, Source: exitCostQuote, BPS: bps,
			QuotedCollateralRaw: basis.heldRaw, MarkSF: basis.mark, Slot: 77, At: at,
			scope: &exitQuoteScope{vault: m.vault.squads, manifest: m.SHA256, usdc: basis.usdc, path: quotePath(m, route.Lane)},
		}
	}

	t.Run("normal_tick_makes_no_extra_calls", func(t *testing.T) {
		now := time.Now().UTC()
		calls := 0
		q := exitQuoter{now: func() time.Time { return now }, price: func(_ context.Context, _ heldExitBasis, _ int64, at time.Time) (ExitCost, error) {
			calls++
			return makeCost(30_000_000, at), nil // 30 bps
		}}
		costs := q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, nil)
		now = now.Add(exitQuoteInterval + time.Second)
		costs = q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, costs)
		if calls != 2 || costs[route.Lane].Quote.BPS != 30_000_000 {
			t.Fatalf("normal tick made extra calls: calls=%d cost=%+v", calls, costs[route.Lane].Quote)
		}
	})

	t.Run("split_brain_spike_recovers_healthy_peer_and_stops_early", func(t *testing.T) {
		now := time.Now().UTC()
		sequence := []microBPS{
			29_000_000,  // initial healthy quote (29 bps)
			910_000_000, // degraded node (910 bps)
			173_000_000, // still degraded peer (173 bps, still >= 50 bps above 29 bps)
			29_500_000,  // healthy peer (29.5 bps) -> stop early before 4th call
			25_000_000,  // must not be called
		}
		calls := 0
		q := exitQuoter{now: func() time.Time { return now }, price: func(_ context.Context, _ heldExitBasis, _ int64, at time.Time) (ExitCost, error) {
			bps := sequence[calls]
			calls++
			return makeCost(bps, at), nil
		}}
		costs := q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, nil)
		now = now.Add(exitQuoteInterval + time.Second)
		costs = q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, costs)
		if calls != 4 || costs[route.Lane].Quote.BPS != 29_500_000 {
			t.Fatalf("spike did not recover healthy peer early: calls=%d cost=%+v", calls, costs[route.Lane].Quote)
		}
	})

	t.Run("cold_start_outlier_against_realized_confirms_peer", func(t *testing.T) {
		now := time.Now().UTC()
		realized := makeCost(19_500_000, now.Add(-time.Hour))
		realized.Source = exitCostRealized
		sequence := []microBPS{173_000_000, 29_000_000}
		calls := 0
		q := exitQuoter{now: func() time.Time { return now }, price: func(_ context.Context, _ heldExitBasis, _ int64, at time.Time) (ExitCost, error) {
			bps := sequence[calls]
			calls++
			return makeCost(bps, at), nil
		}}
		costs := q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, exitCosts{route.Lane: {Realized: &realized}})
		if calls != 2 || costs[route.Lane].Quote.BPS != 29_000_000 {
			t.Fatalf("cold-start outlier did not confirm peer: calls=%d cost=%+v", calls, costs[route.Lane].Quote)
		}
	})

	t.Run("sustained_real_liquidity_drop_publishes_immediately", func(t *testing.T) {
		now := time.Now().UTC()
		calls := 0
		q := exitQuoter{now: func() time.Time { return now }, price: func(_ context.Context, _ heldExitBasis, _ int64, at time.Time) (ExitCost, error) {
			calls++
			if calls == 1 {
				return makeCost(29_000_000, at), nil
			}
			// Genuine market crash: every peer quotes 300 bps.
			return makeCost(300_000_000, at), nil
		}}
		costs := q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, nil)
		now = now.Add(exitQuoteInterval + time.Second)
		costs = q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, costs)
		if calls != 1+1+exitQuoteSpikeConfirmExtraCalls || costs[route.Lane].Quote.BPS != 300_000_000 {
			t.Fatalf("real liquidity drop was hidden or under-confirmed: calls=%d cost=%+v", calls, costs[route.Lane].Quote)
		}
	})
}
