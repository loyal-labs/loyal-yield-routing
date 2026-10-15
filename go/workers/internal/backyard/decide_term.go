package backyard

import (
	"math/big"
)

// Term debt (KLend v1.25, decoded in programs/kamino). A borrow on a term
// reserve matures DebtTermSeconds after its obligation slot's last borrow,
// and every borrow resets that clock; a reserve may also carry a market-wide
// DebtMaturityTimestamp. Past maturity KLend refuses more borrowing and the
// debt becomes liquidatable, ramping to the whole debt over three days, with
// no rate jump.
//
// Backyard keeps a held term lane alive with the minimum up step before its
// maturity (no update_obligation_config, no rollover): an ordinary atomic
// borrow, conversion and deposit. A lane it cannot keep alive goes to debt 0 before maturity: target()'s
// term rule, from the batch's own term end and Clock, needs no operator.
const (
	// termKeepAliveSeconds: under this much time to maturity a held term
	// lane borrows again.
	termKeepAliveSeconds int64 = 3 * 24 * 3600
	// termDeleverSeconds: under this, a lane that cannot keep alive delevers.
	termDeleverSeconds  int64 = 24 * 3600
	termKeepAliveReason       = "term_keepalive"
	// termKeepAliveBorrowRaw is the keep-alive borrow: KLend's term restarts
	// on any borrow, so the smallest borrow the lane takes is enough. It
	// needs no economic approval: it keeps the existing position, it does
	// not lever it.
	termKeepAliveBorrowRaw = leverageMinimumBorrowRaw
	// termMaturityUnknown marks term debt whose borrow slot carries no borrow
	// time: its maturity cannot be known, so nothing borrows or plans on it.
	termMaturityUnknown int64 = -1
)

// applyDebtTerm stamps the working lane's maturity and the batch's chain
// time into its snapshot.
func applyDebtTerm(s *Snapshot, accounts []ConfirmedAccount, route RuntimeRoute) error {
	s.ChainUnix = clockUnixTimestamp(accounts)
	end, err := observedDebtTermEnd(accounts, route)
	if err != nil {
		return err
	}
	s.DebtTermEndUnix = end
	return nil
}

// termSecondsLeft is the time from the batch's chain clock to the working
// lane's maturity; false without term debt or a known clock.
func termSecondsLeft(s Snapshot) (int64, bool) {
	if s.PositionDebtRaw <= 0 || s.DebtTermEndUnix <= 0 || s.ChainUnix <= 0 {
		return 0, false
	}
	return s.DebtTermEndUnix - s.ChainUnix, true
}

// termDebtUnknown: the lane owes term debt whose maturity is unknown.
func termDebtUnknown(s Snapshot) bool {
	return s.PositionDebtRaw > 0 && s.DebtTermEndUnix == termMaturityUnknown
}

// termKeepAliveRoom reports whether the keep-alive borrow fits this batch:
// the debt reserve's fee-inclusive room, a maturity still ahead (KLend
// refuses a borrow at or past it), and an instant LTV, the borrow and its
// fee valued with the liability margin, within the lane cap's LTV band (a
// position within leverageUpNearBPS of a level is at it).
func termKeepAliveRoom(s Snapshot) bool {
	left, ok := termSecondsLeft(s)
	if !ok || left <= 0 || !s.BorrowCapacityKnown || kaminoReceiveWithinRoom(s.AdditionalDebtRoomRaw, s.BorrowFeeRate) < termKeepAliveBorrowRaw ||
		s.PositionCollateralValueRaw <= 0 || s.PositionDebtValueRaw < 0 {
		return false
	}
	fee, err := kaminoBorrowFeeAtRate(s.BorrowFeeRate, termKeepAliveBorrowRaw)
	if err != nil {
		return false
	}
	added, err := capacityBorrowValue(s, termKeepAliveBorrowRaw+fee, true)
	if err != nil {
		return false
	}
	after := new(big.Int).Add(big.NewInt(s.PositionDebtValueRaw), new(big.Int).SetUint64(added))
	after.Mul(after, big.NewInt(10_000))
	return after.Cmp(new(big.Int).Mul(big.NewInt(s.PositionCollateralValueRaw), big.NewInt(laneCeilingLTVBPS(s.RouteLane)+leverageUpNearBPS))) <= 0
}

// termDeleverDue: the lane owes term debt it can no longer keep alive (under
// a day left without keep-alive room) or that has matured.
func termDeleverDue(s Snapshot) bool {
	if !earnActiveLane(s.RouteLane) || !s.HasPosition {
		return false
	}
	left, ok := termSecondsLeft(s)
	return ok && (left <= 0 || left < termDeleverSeconds && !termKeepAliveRoom(s))
}

// termDays is days to maturity for logs and the gauge; false without term
// debt.
func termDays(s Snapshot) (float64, bool) {
	left, ok := termSecondsLeft(s)
	return float64(left) / 86_400, ok
}
