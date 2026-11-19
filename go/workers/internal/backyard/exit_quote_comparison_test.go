package backyard

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

func TestHeldExitQuoteComparesHistoricalLotWithoutChangingNAV(t *testing.T) {
	// November 19 AUTO/wYLDS: 3,520.838807 held, 1,632.010167 sold at 2.503902 bps.
	const fullRaw, historicalRaw = 3_520_838_807, 1_632_010_167
	const fullBPS, historicalBPS = 7_896_418, 2_503_902
	const slot = 77
	m, accounts := multiLaneFixture(t, func(accounts *[]ConfirmedAccount) {
		r := fundLane(t, accounts, autoAUTOWYLDSLane, 0, 0)
		binary.LittleEndian.PutUint64(accountAt(*accounts, r.CollateralCustody).Data[64:72], fullRaw)
	})
	route, err := m.route(autoAUTOWYLDSLane)
	if err != nil {
		t.Fatal(err)
	}
	basis, held, err := readHeldExitBasis(m, accounts, route, slot)
	if err != nil || !held || basis.heldRaw != fullRaw {
		t.Fatalf("historical fixture: amount %d, held %v, err %v", basis.heldRaw, held, err)
	}
	prices, err := batchLanePrices(accounts, route)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                string
		realizedRaw         uint64
		realized, matched   microBPS
		fault, event, level string
		wantCalls           int
	}{
		{"historical sale to wYLDS", historicalRaw, historicalBPS, 0, "debt proceeds", "backyard_exit_quote_different_pair", "WARN", 1},
		{"costlier recent debt sale with earlier USDC receipt", historicalRaw, 9_000_000, 7_805_225, "earlier cash", "backyard_exit_quote_outlier", "ERROR", 2},
		{"lookup failure with immaterial ratio", historicalRaw, 9_000_000, 0, "lookup failed", "", "", 1},
		{"lookup failure with breached ratio", historicalRaw, historicalBPS, 0, "lookup failed", "backyard_exit_quote_comparison_unavailable", "ERROR", 1},
		{"historical proceeds unknown", historicalRaw, historicalBPS, 0, "unknown proceeds", "backyard_exit_quote_comparison_unavailable", "ERROR", 1},
		{"USDC sale with both current quotes above 3x", historicalRaw, historicalBPS, 7_805_225, "", "backyard_exit_quote_outlier", "ERROR", 2},
		{"only full holding above 3x", historicalRaw, historicalBPS, 7_511_706, "", "backyard_exit_quote_size_difference", "INFO", 2},
		{"equal lot", fullRaw, historicalBPS, fullBPS, "", "backyard_exit_quote_outlier", "ERROR", 1},
		{"nearly equal lot above 3x", fullRaw - 1, historicalBPS, 7_511_707, "", "backyard_exit_quote_outlier", "ERROR", 2},
		{"comparison deferred", historicalRaw, historicalBPS, 0, "budget", "backyard_exit_quote_comparison_unavailable", "ERROR", 2},
		{"comparison covered wrong lot", historicalRaw, historicalBPS, 0, "wrong size", "backyard_exit_quote_comparison_unavailable", "ERROR", 2},
		{"free historical exit", historicalRaw, 0, 0, "", "", "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			realized := ExitCost{Lane: route.Lane, ProceedsMint: fleet.USDCMint, Source: exitCostRealized, QuotedCollateralRaw: tc.realizedRaw, MarkSF: basis.mark, BPS: tc.realized, Slot: slot - 1, At: now.Add(-time.Hour)}
			expectedReferenceSlot := int64(slot - 1)
			if tc.fault == "earlier cash" {
				realized.ProceedsMint = route.Kamino.DebtMint
				expectedReferenceSlot--
			}
			var log bytes.Buffer
			var calls, writes, lookups int
			q := exitQuoter{
				now:    func() time.Time { return now },
				events: newEvents(slog.New(slog.NewJSONHandler(&log, nil)), nil, "rwa"),
				price: func(ctx context.Context, b heldExitBasis, slot int64, at time.Time) (ExitCost, error) {
					calls++
					c := ExitCost{Source: exitCostQuote, Lane: route.Lane, BPS: fullBPS, QuotedCollateralRaw: b.heldRaw, MarkSF: b.mark, At: at, Slot: slot,
						scope: &exitQuoteScope{vault: m.vault.squads, manifest: m.SHA256, usdc: b.usdc, path: quotePath(m, route.Lane)}}
					if calls == 1 {
						if b.heldRaw != fullRaw {
							t.Fatalf("first quote at %d, want full holding", b.heldRaw)
						}
						return c, nil
					}
					value, err := prices.collateralMicros(tc.realizedRaw)
					deadline, bounded := ctx.Deadline()
					if err != nil || !bounded || time.Until(deadline) > 1500*time.Millisecond || b.heldRaw != tc.realizedRaw || b.markValue != uint64(value) {
						t.Fatalf("comparison quote at %d, mark value %d, deadline %v (err %v)", b.heldRaw, b.markValue, deadline, err)
					}
					c.BPS = tc.matched
					switch tc.fault {
					case "budget":
						return ExitCost{}, &jupiter.DeferredError{Reason: "budget"}
					case "wrong size":
						c.QuotedCollateralRaw++
					}
					return c, nil
				},
				referenceSale: func(ctx context.Context, sale ExitCost) (ExitCost, error) {
					lookups++
					if _, ok := ctx.Deadline(); !ok || sale.Slot != realized.Slot || sale.QuotedCollateralRaw != tc.realizedRaw {
						t.Fatalf("unbounded or incorrect historical receipt lookup: %+v", sale)
					}
					switch tc.fault {
					case "earlier cash":
						sale.Slot, sale.BPS, sale.ProceedsMint = expectedReferenceSlot, historicalBPS, fleet.USDCMint
					case "debt proceeds":
						sale.ProceedsMint = route.Kamino.DebtMint
					case "unknown proceeds":
						sale.ProceedsMint = ""
					case "lookup failed":
						return ExitCost{}, context.DeadlineExceeded
					}
					return sale, nil
				},
				record: func(_ context.Context, c ExitCost) (bool, error) {
					writes++
					if c.QuotedCollateralRaw != fullRaw || c.BPS != fullBPS {
						t.Fatal("historical-lot quote reached storage", c)
					}
					return true, nil
				},
			}
			costs := q.quote(t.Context(), m, slot, accounts, []string{route.Lane}, exitCosts{route.Lane: {Realized: &realized}})
			for _, saved := range []exitCosts{costs, q.cached} {
				c, ok := saved.applicable(m, slot, now).at(route.Lane, fullRaw, basis.mark)
				if !ok || c.BPS != fullBPS {
					t.Fatalf("full-size NAV quote changed: %+v", c)
				}
			}
			if costs[route.Lane].Realized != &realized || calls != tc.wantCalls || writes != 1 || lookups != 1 {
				t.Fatalf("history %v, calls %d, writes %d, lookups %d", costs[route.Lane].Realized, calls, writes, lookups)
			}
			if tc.event == "" {
				if log.Len() != 0 {
					t.Fatalf("unexpected diagnostic: %s", &log)
				}
				return
			}
			var event struct {
				Msg, Level   string
				QuoteRaw     uint64 `json:"quote_raw"`
				RealizedRaw  uint64 `json:"realized_raw"`
				Slot         int64
				RealizedSlot int64    `json:"realized_slot"`
				MatchedBPS   microBPS `json:"matched_micro_bps"`
			}
			if err := json.Unmarshal(log.Bytes(), &event); err != nil || event.Msg != tc.event || event.Level != tc.level || event.QuoteRaw != fullRaw ||
				event.RealizedRaw != tc.realizedRaw || event.Slot != slot || event.RealizedSlot != expectedReferenceSlot || event.MatchedBPS != tc.matched {
				t.Fatalf("diagnostic: %s (err %v)", &log, err)
			}
		})
	}
}
