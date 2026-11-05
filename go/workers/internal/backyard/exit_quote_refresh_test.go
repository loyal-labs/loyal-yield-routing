package backyard

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestExitQuoteRefreshMatchingHoldings(t *testing.T) {
	for _, coverage := range []uint64{1, 2} {
		for _, realized := range []bool{false, true} {
			t.Run(fmt.Sprintf("coverage=%d/realized=%v", coverage, realized), func(t *testing.T) {
				manifest := readyWorkerManifest(t)
				route, accounts := onreEntryNAVFixture(t, 0, 2_000_000_000)
				accounts = append(accounts, clockFixture())
				basis, held, err := readHeldExitBasis(manifest, accounts, route, 77)
				if err != nil || !held {
					t.Fatalf("held basis: held=%v err=%v", held, err)
				}
				now := time.Unix(kaminoFixtureUnix, 0).UTC()
				old, err := exitCostOf(route.Lane, exitCostQuote, basis.heldRaw*coverage, basis.markValue, basis.markValue-basis.markValue/1_000, basis.mark, 77, now)
				if err != nil {
					t.Fatal(err)
				}
				costs := exitCosts{route.Lane: {Quote: &old}}
				if realized {
					sale := old
					sale.Source = exitCostRealized
					costs[route.Lane] = laneExitCosts{Quote: &old, Realized: &sale}
				}
				queries, records := 0, 0
				quoter := exitQuoter{
					price: func(_ context.Context, b heldExitBasis, slot int64, at time.Time) (ExitCost, error) {
						queries++
						return exitCostOf(route.Lane, exitCostQuote, b.heldRaw, b.markValue, b.markValue-b.markValue/500, b.mark, slot, at)
					},
					record: func(context.Context, ExitCost) (bool, error) { records++; return true, nil },
					now:    func() time.Time { return now },
				}
				tick := func() {
					costs = quoter.quote(context.Background(), manifest, 77, accounts, []string{route.Lane}, costs)
				}
				now = now.Add(exitQuoteInterval - time.Nanosecond)
				tick()
				if queries != 0 || records != 0 {
					t.Fatal("matching quote refreshed before the interval")
				}
				now = now.Add(time.Nanosecond)
				tick()
				updated := costs[route.Lane].Quote
				if queries != 1 || records != 1 || updated == nil || updated.BPS == old.BPS || !updated.At.Equal(now) || updated.QuotedCollateralRaw != basis.heldRaw {
					t.Fatalf("expired matching quote not replaced: queries=%d records=%d quote=%+v", queries, records, updated)
				}
				tick()
				if queries != 1 {
					t.Fatal("successful refresh repeated at the same instant")
				}
				now = now.Add(exitQuoteInterval)
				tick()
				if queries != 2 || records != 2 {
					t.Fatal("replacement quote never refreshed again")
				}
			})
		}
	}
}

func TestExitQuoteRefreshPreservesUnpricedRetries(t *testing.T) {
	for _, mismatch := range []string{"missing", "size", "mark"} {
		for _, realized := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/realized=%v", mismatch, realized), func(t *testing.T) {
				manifest := readyWorkerManifest(t)
				route, accounts := onreEntryNAVFixture(t, 0, 2_000_000_000)
				accounts = append(accounts, clockFixture())
				basis, held, err := readHeldExitBasis(manifest, accounts, route, 77)
				if err != nil || !held {
					t.Fatalf("held basis: held=%v err=%v", held, err)
				}
				now := time.Unix(kaminoFixtureUnix, 0).UTC()
				old := ExitCost{Lane: route.Lane, Source: exitCostQuote, BPS: 1, QuotedCollateralRaw: basis.heldRaw, MarkSF: basis.mark, Slot: 77, At: now}
				var laneCosts laneExitCosts
				if mismatch != "missing" {
					laneCosts.Quote = &old
					if mismatch == "size" {
						old.QuotedCollateralRaw--
					} else {
						old.MarkSF = kaminoOne()
					}
				}
				if realized {
					sale := old
					sale.Source = exitCostRealized
					laneCosts.Realized = &sale
				}
				costs := exitCosts{route.Lane: laneCosts}
				queries := 0
				quoter := exitQuoter{
					price: func(context.Context, heldExitBasis, int64, time.Time) (ExitCost, error) {
						queries++
						return ExitCost{}, errors.New("quote unavailable")
					},
					now:       func() time.Time { return now },
					attempted: map[string]time.Time{route.Lane: now},
				}
				for range 2 {
					costs = quoter.quote(context.Background(), manifest, 77, accounts, []string{route.Lane}, costs)
				}
				if !realized && queries != 2 || realized && queries != 0 {
					t.Fatalf("unpriced retry policy changed: queries=%d realized=%v", queries, realized)
				}
				if realized {
					now = now.Add(exitQuoteInterval)
					quoter.quote(context.Background(), manifest, 77, accounts, []string{route.Lane}, costs)
					if queries != 1 {
						t.Fatal("realized fallback retry did not resume after the interval")
					}
				}
			})
		}
	}
}

func TestExitQuoteRefreshFailurePacing(t *testing.T) {
	for _, failure := range []string{"price", "record"} {
		t.Run(failure, func(t *testing.T) {
			manifest := readyWorkerManifest(t)
			route, accounts := onreEntryNAVFixture(t, 0, 2_000_000_000)
			accounts = append(accounts, clockFixture())
			basis, held, err := readHeldExitBasis(manifest, accounts, route, 77)
			if err != nil || !held {
				t.Fatalf("held basis: held=%v err=%v", held, err)
			}
			now := time.Unix(kaminoFixtureUnix, 0).UTC()
			old, err := exitCostOf(route.Lane, exitCostQuote, basis.heldRaw, basis.markValue, basis.markValue-basis.markValue/1_000, basis.mark, 77, now.Add(-exitQuoteInterval))
			if err != nil {
				t.Fatal(err)
			}
			costs := exitCosts{route.Lane: {Quote: &old}}
			queries := 0
			fail := true
			quoter := exitQuoter{
				price: func(_ context.Context, b heldExitBasis, slot int64, at time.Time) (ExitCost, error) {
					queries++
					if fail && failure == "price" {
						return ExitCost{}, errors.New("quote unavailable")
					}
					return exitCostOf(route.Lane, exitCostQuote, b.heldRaw, b.markValue, b.markValue-b.markValue/500, b.mark, slot, at)
				},
				record: func(context.Context, ExitCost) (bool, error) {
					if fail {
						return false, errors.New("record unavailable")
					}
					return true, nil
				},
				now: func() time.Time { return now },
			}
			tick := func() {
				costs = quoter.quote(context.Background(), manifest, 77, accounts, []string{route.Lane}, costs)
			}
			tick()
			tick()
			now = now.Add(exitQuoteInterval - time.Nanosecond)
			tick()
			if queries != 1 || costs[route.Lane].Quote != &old {
				t.Fatalf("failed refresh retry was not paced: queries=%d quote=%+v", queries, costs[route.Lane].Quote)
			}
			fail = false
			now = now.Add(time.Nanosecond)
			tick()
			if queries != 2 || costs[route.Lane].Quote.BPS == old.BPS || !costs[route.Lane].Quote.At.Equal(now) {
				t.Fatalf("refresh did not recover: queries=%d quote=%+v", queries, costs[route.Lane].Quote)
			}
			tick()
			if queries != 2 {
				t.Fatal("recovered refresh repeated at the same instant")
			}
		})
	}
}

func TestExitQuoteRefreshInvalidTimes(t *testing.T) {
	now := time.Unix(kaminoFixtureUnix, 0).UTC()
	for _, tc := range []struct {
		name    string
		at      time.Time
		attempt time.Time
	}{
		{name: "zero"},
		{name: "future-quote", at: now.AddDate(100, 0, 0)},
		{name: "future-attempt", at: now.Add(-exitQuoteInterval), attempt: now.AddDate(100, 0, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := readyWorkerManifest(t)
			route, accounts := onreEntryNAVFixture(t, 0, 2_000_000_000)
			accounts = append(accounts, clockFixture())
			basis, held, err := readHeldExitBasis(manifest, accounts, route, 77)
			if err != nil || !held {
				t.Fatalf("held basis: held=%v err=%v", held, err)
			}
			old := ExitCost{Lane: route.Lane, Source: exitCostQuote, BPS: 1, QuotedCollateralRaw: basis.heldRaw, MarkSF: basis.mark, Slot: 77, At: tc.at}
			costs := exitCosts{route.Lane: {Quote: &old}}
			queries := 0
			quoter := exitQuoter{
				price: func(context.Context, heldExitBasis, int64, time.Time) (ExitCost, error) {
					queries++
					return ExitCost{}, errors.New("quote unavailable")
				},
				now:       func() time.Time { return now },
				attempted: map[string]time.Time{route.Lane: tc.attempt},
			}
			for range 2 {
				costs = quoter.quote(context.Background(), manifest, 77, accounts, []string{route.Lane}, costs)
			}
			if queries != 1 || !quoter.attempted[route.Lane].Equal(now) {
				t.Fatalf("invalid time suppressed refresh or bypassed retry pacing: queries=%d attempt=%v", queries, quoter.attempted[route.Lane])
			}
		})
	}
}
