package backyard

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// entryQuoteFixture is a selector ENTER quote into lane: the shape target()
// reads from the selector's latest result.
func entryQuoteFixture(now time.Time, lane string, amount int64) MoveQuote {
	return MoveQuote{MinimumIdleRaw: uint64(amount), BorrowReceiveRaw: uint64(amount / 2), SourceLane: SelectedRouteID, DestinationLane: lane, ObservationID: "quoted-source", EquityRaw: amount, CostRaw: 1, ObservedAt: now.Add(-time.Second), EvidenceID: sha256Bytes([]byte("complete-recipe")), SampleSlot: 42, ValidThroughSlot: 74}
}

// autoEntryQuoteFixture is entryQuoteFixture into the AUTO lane: the PYUSD
// debt borrow legs carry the observed bounded debt price evidence the quote
// checks require on non-USDC debt lanes.
func autoEntryQuoteFixture(now time.Time, amount int64, price *BudgetPrice) MoveQuote {
	q := entryQuoteFixture(now, autoAUTOPYUSD.Lane, amount)
	q.SourceLane, q.BorrowFeeRaw, q.DebtPrice = autoAUTOPYUSD.Lane, 1_000, copyDebtPrice(price)
	return q
}

// storeTestSelectorEnter stores a selector result that enters quote's lane:
// target()'s flat-vault entry input.
func storeTestSelectorEnter(t *testing.T, ctx context.Context, db *Database, key string, quote MoveQuote) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"result": SelectorResult{Action: "ENTER", Reason: "persistent_net_benefit", DestinationLane: quote.DestinationLane, EquityRaw: quote.EquityRaw, SelectedQuote: &quote}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{selector}',$2::jsonb,true) WHERE route_key=$1`, key, raw); err != nil {
		t.Fatal(err)
	}
}
