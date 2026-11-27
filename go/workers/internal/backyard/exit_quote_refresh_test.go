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

	t.Run("steady_size_difference_above_realized_does_not_reconfirm_with_baseline", func(t *testing.T) {
		now := time.Now().UTC()
		realized := makeCost(19_500_000, now.Add(-time.Hour))
		realized.Source = exitCostRealized
		prior := makeCost(63_200_000, now.Add(-20*time.Second))
		prior.scope = nil // persisted DB row has no scope, still supplies baseline BPS
		calls := 0
		q := exitQuoter{now: func() time.Time { return now }, price: func(_ context.Context, _ heldExitBasis, _ int64, at time.Time) (ExitCost, error) {
			calls++
			return makeCost(63_500_000, at), nil
		}}
		costs := q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, exitCosts{route.Lane: {Quote: &prior, Realized: &realized}})
		if calls != 1 || costs[route.Lane].Quote == nil || costs[route.Lane].Quote.BPS != 63_500_000 {
			t.Fatalf("steady quote above small realized sale re-confirmed unnecessarily: calls=%d cost=%+v", calls, costs[route.Lane].Quote)
		}
	})

	t.Run("short_venue_blink_is_held_and_recovers_without_publishing_spike", func(t *testing.T) {
		now := time.Now().UTC()
		currentBPS := microBPS(63_000_000)
		calls := 0
		var recorded []microBPS
		q := exitQuoter{
			now: func() time.Time { return now },
			record: func(_ context.Context, c ExitCost) (bool, error) {
				recorded = append(recorded, c.BPS)
				return true, nil
			},
			price: func(_ context.Context, _ heldExitBasis, _ int64, at time.Time) (ExitCost, error) {
				calls++
				return makeCost(currentBPS, at), nil
			},
		}
		costs := q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, nil)
		if calls != 1 || len(recorded) != 1 || recorded[0] != 63_000_000 {
			t.Fatalf("initial baseline failed: calls=%d recorded=%v", calls, recorded)
		}

		// Venue blinks out for ~20s: initial detection does peer confirmation (1+3),
		// holds the unconfirmed spike, and leaves the genuine 16s-old quote only up
		// to its original 30s TTL.
		currentBPS = 310_000_000
		now = now.Add(16 * time.Second)
		costs = q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, costs)
		if calls != 1+1+exitQuoteSpikeConfirmExtraCalls || len(recorded) != 1 {
			t.Fatalf("initial blink did not confirm and hold: calls=%d recorded=%v", calls, recorded)
		}
		app16 := costs.applicable(m, 77, now)
		if app16[route.Lane].Quote == nil || app16[route.Lane].Quote.BPS != 63_000_000 {
			t.Fatalf("expected original in-TTL quote while blink starts, got %+v", app16[route.Lane].Quote)
		}

		// Intermediate probe 5s later uses 1 call (no 3x retry burn) and still holds.
		now = now.Add(5 * time.Second)
		costs = q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, costs)
		if calls != 1+1+exitQuoteSpikeConfirmExtraCalls+1 || len(recorded) != 1 {
			t.Fatalf("intermediate blink probe misbehaved: calls=%d recorded=%v", calls, recorded)
		}

		// Once the original 30s TTL elapses (t=31s), valuation fails closed
		// (unpriced) rather than extending a stale baseline.
		now = now.Add(10 * time.Second)
		costs = q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, costs)
		if app31 := costs.applicable(m, 77, now); app31[route.Lane].Quote != nil {
			t.Fatalf("stale baseline leaked past exitQuoteInterval during pending spike: %+v", app31[route.Lane].Quote)
		}
		if len(recorded) != 1 {
			t.Fatalf("unconfirmed blink was recorded: %v", recorded)
		}

		// Venue recovers at t=36s: immediate recovery without ever publishing 310 bps.
		currentBPS = 63_500_000
		now = now.Add(5 * time.Second)
		costs = q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, costs)
		app36 := costs.applicable(m, 77, now)
		if app36[route.Lane].Quote == nil || app36[route.Lane].Quote.BPS != 63_500_000 || len(recorded) != 2 || recorded[1] != 63_500_000 {
			t.Fatalf("blink did not recover cleanly: quote=%+v recorded=%v", app36[route.Lane].Quote, recorded)
		}
	})

	t.Run("sustained_real_liquidity_drop_publishes_after_persistence", func(t *testing.T) {
		now := time.Now().UTC()
		currentBPS := microBPS(29_000_000)
		calls := 0
		var recorded []microBPS
		q := exitQuoter{
			now: func() time.Time { return now },
			record: func(_ context.Context, c ExitCost) (bool, error) {
				recorded = append(recorded, c.BPS)
				return true, nil
			},
			price: func(_ context.Context, _ heldExitBasis, _ int64, at time.Time) (ExitCost, error) {
				calls++
				return makeCost(currentBPS, at), nil
			},
		}
		costs := q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, nil)

		// Genuine market crash: every probe across 75s returns 300 bps.
		currentBPS = 300_000_000
		now = now.Add(exitQuoteInterval + time.Second)
		costs = q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, costs)
		if app := costs.applicable(m, 77, now); app[route.Lane].Quote != nil || len(recorded) != 1 {
			t.Fatalf("unpersisted crash published early: quote=%+v recorded=%v", app[route.Lane].Quote, recorded)
		}

		// Continuous probes at +20s, +40s, +60s stay pending (< 75s).
		for _, step := range []time.Duration{20 * time.Second, 20 * time.Second, 20 * time.Second} {
			now = now.Add(step)
			costs = q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, costs)
			if app := costs.applicable(m, 77, now); app[route.Lane].Quote != nil || len(recorded) != 1 {
				t.Fatalf("crash confirmed before persistence elapsed: quote=%+v recorded=%v", app[route.Lane].Quote, recorded)
			}
		}

		// At +75s, final confirmation runs peer checks and publishes true 300 bps.
		now = now.Add(15 * time.Second)
		callsBeforeConfirm := calls
		costs = q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, costs)
		app := costs.applicable(m, 77, now)
		if app[route.Lane].Quote == nil || app[route.Lane].Quote.BPS != 300_000_000 || len(recorded) != 2 || recorded[1] != 300_000_000 {
			t.Fatalf("sustained crash was not published after persistence: quote=%+v recorded=%v", app[route.Lane].Quote, recorded)
		}
		if calls-callsBeforeConfirm != 1+exitQuoteSpikeConfirmExtraCalls {
			t.Fatalf("confirming tick did not run peer confirmation: deltaCalls=%d", calls-callsBeforeConfirm)
		}

		// Subsequent normal tick at 300 bps makes 1 call and publishes immediately.
		callsBeforeSteady := calls
		now = now.Add(exitQuoteInterval + time.Second)
		costs = q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, costs)
		if calls-callsBeforeSteady != 1 || costs.applicable(m, 77, now)[route.Lane].Quote.BPS != 300_000_000 {
			t.Fatalf("post-confirm steady tick re-triggered spike confirmation: deltaCalls=%d", calls-callsBeforeSteady)
		}

		// Liquidity recovery (300 -> 30 bps) publishes immediately with 1 call.
		callsBeforeRecovery := calls
		currentBPS = 30_000_000
		now = now.Add(exitQuoteInterval + time.Second)
		costs = q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, costs)
		if calls-callsBeforeRecovery != 1 || costs.applicable(m, 77, now)[route.Lane].Quote.BPS != 30_000_000 {
			t.Fatalf("downward recovery was delayed: deltaCalls=%d cost=%+v", calls-callsBeforeRecovery, costs[route.Lane].Quote)
		}
	})

	t.Run("confirming_tick_recovers_if_peer_heals_at_boundary", func(t *testing.T) {
		now := time.Now().UTC()
		step := 0
		q := exitQuoter{now: func() time.Time { return now }, price: func(_ context.Context, _ heldExitBasis, _ int64, at time.Time) (ExitCost, error) {
			step++
			if step == 1 {
				return makeCost(29_000_000, at), nil
			}
			// Initial spike (4 calls) + 3 intermediate probes (3 calls) + first call on confirming tick (1 call) = 8 calls.
			// Second call on confirming tick (#9) heals to 31 bps.
			if step >= 9 {
				return makeCost(31_000_000, at), nil
			}
			return makeCost(300_000_000, at), nil
		}}
		costs := q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, nil)
		now = now.Add(exitQuoteInterval + time.Second)
		costs = q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, costs)
		for range 3 {
			now = now.Add(20 * time.Second)
			costs = q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, costs)
		}
		now = now.Add(15 * time.Second)
		costs = q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, costs)
		app := costs.applicable(m, 77, now)
		if app[route.Lane].Quote == nil || app[route.Lane].Quote.BPS != 31_000_000 {
			t.Fatalf("confirming tick did not recover healed peer: quote=%+v step=%d", app[route.Lane].Quote, step)
		}
	})

	t.Run("gap_beyond_max_gap_resets_persistence_window", func(t *testing.T) {
		now := time.Now().UTC()
		currentBPS := microBPS(29_000_000)
		q := exitQuoter{now: func() time.Time { return now }, price: func(_ context.Context, _ heldExitBasis, _ int64, at time.Time) (ExitCost, error) {
			return makeCost(currentBPS, at), nil
		}}
		costs := q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, nil)
		currentBPS = 300_000_000
		now = now.Add(exitQuoteInterval + time.Second)
		costs = q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, costs)
		// Jump past exitQuoteSpikePersistDuration in a single gap > exitQuoteSpikeMaxGap:
		// an isolated probe 80s later must restart the window rather than confirm immediately.
		now = now.Add(exitQuoteSpikePersistDuration + 5*time.Second)
		costs = q.quote(t.Context(), m, 77, accounts, []string{route.Lane}, costs)
		if app := costs.applicable(m, 77, now); app[route.Lane].Quote != nil {
			t.Fatalf("stale isolated spike confirmed across gap > maxGap: %+v", app[route.Lane].Quote)
		}
	})
}
