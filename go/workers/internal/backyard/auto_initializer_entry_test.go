package backyard

import (
	"time"
)

// entryQuoteFixture is a selector ENTER quote into lane: the shape target()
// reads from the selector's latest result.
func entryQuoteFixture(now time.Time, lane string, amount int64) MoveQuote {
	exit := ExitCost{Lane: lane, BPS: 1, QuotedCollateralRaw: uint64(amount), MarkSF: kaminoOne(), Source: exitCostQuote, Slot: 42, At: now.Add(-time.Second)}
	return parBorrow(MoveQuote{MinimumIdleRaw: uint64(amount), BorrowReceiveRaw: uint64(amount / 2), SourceLane: MapleSyrupUSDCLaneID, DestinationLane: lane, ObservationID: "quoted-source", EquityRaw: amount, CostRaw: 1, ObservedAt: now.Add(-time.Second), EvidenceID: sha256Bytes([]byte("complete-recipe")), SampleSlot: 42, ValidThroughSlot: 74, DestinationExit: &exit})
}

// parBorrow values q's up step at par: its liability is receive plus fee and
// its proceeds the receive, as a USDC-priced debt values them.
func parBorrow(q MoveQuote) MoveQuote {
	q.BorrowValueRaw, q.ProceedsValueRaw, q.DepositValueRaw = q.BorrowReceiveRaw+q.BorrowFeeRaw, q.BorrowReceiveRaw, uint64(q.EquityRaw)
	return q
}

// autoEntryQuoteFixture is entryQuoteFixture into the AUTO lane, its PYUSD
// borrow valued at par.
func autoEntryQuoteFixture(now time.Time, amount int64) MoveQuote {
	q := entryQuoteFixture(now, autoAUTOPYUSD.Lane, amount)
	q.SourceLane, q.BorrowFeeRaw = autoAUTOPYUSD.Lane, 1_000
	return parBorrow(q)
}
