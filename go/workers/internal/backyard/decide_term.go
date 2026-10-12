package backyard

import (
	"encoding/binary"
	"math/big"
)

// Term debt (KLend v1.25, decoded in programs/kamino). A borrow on a term
// reserve matures DebtTermSeconds after its obligation slot's last borrow,
// and every borrow resets that clock; a reserve may also carry a market-wide
// DebtMaturityTimestamp. Past maturity KLend refuses more borrowing and the
// debt becomes liquidatable, ramping to the whole debt over three days, with
// no rate jump.
//
// Backyard keeps a held term lane alive with a normal borrow before its
// maturity (no update_obligation_config, no rollover): the borrowed cash then
// converts to collateral and redeposits through the lane's ordinary loop
// legs. A lane it cannot keep alive goes to debt 0 before maturity: target()'s
// term rule, from the batch's own term end and Clock, needs no operator.
const (
	// termKeepAliveSeconds: under this much time to maturity a held term
	// lane borrows again.
	termKeepAliveSeconds int64 = 3 * 24 * 3600
	// termDeleverSeconds: under this, a lane that cannot keep alive delevers.
	termDeleverSeconds int64 = 24 * 3600
	// termAlertSeconds: the days-to-maturity alert (loyal.rules.yml).
	termAlertSeconds    int64 = 2 * 24 * 3600
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

// observedDebtTermEnd is a lane's borrow maturity from one batch: its
// obligation's term end (wyldsTermEnd), or the debt reserve's market-wide
// maturity when that is earlier. Zero when the obligation is absent, owes
// nothing there, or the reserve has no term; termMaturityUnknown when term
// debt has no borrow time.
func observedDebtTermEnd(accounts []ConfirmedAccount, route RuntimeRoute) (int64, error) {
	account := accountAt(accounts, route.Kamino.Obligation)
	if account.Address != route.Kamino.Obligation || account.Lamports == 0 {
		return 0, nil
	}
	reserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.DebtReserve), route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		return 0, err
	}
	terms := reserve.terms
	if terms.DebtTermSeconds == 0 && terms.DebtMaturityTimestamp == 0 {
		return 0, nil
	}
	obligation, err := decodeKaminoObligation(account, route.Kamino)
	if err != nil || obligation.debtRaw == 0 {
		return 0, err
	}
	end, ok, err := wyldsTermEnd(accounts, route)
	if err != nil {
		return 0, err
	}
	if terms.DebtTermSeconds != 0 && (!ok || end > 1<<62) {
		return termMaturityUnknown, nil
	}
	if market := terms.DebtMaturityTimestamp; market != 0 && market <= 1<<62 && (!ok || market < end) {
		end = market
	}
	return int64(end), nil
}

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
// fee valued with the liability margin, within the leverage cap.
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
	return after.Cmp(new(big.Int).Mul(big.NewInt(s.PositionCollateralValueRaw), big.NewInt(leverageMaxLTVBPS))) <= 0
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

// validateTermKeepAliveBorrow is the prepare- and build-time bound of a
// keep-alive borrow: exactly the keep-alive size, on term debt still ahead
// of its maturity, inside the reserve's fee-inclusive room and the
// position's risk caps (protocol LTV, 50% instant LTV with the fee).
func validateTermKeepAliveBorrow(position KaminoPosition, accounts []ConfirmedAccount, route RuntimeRoute, receive uint64) error {
	if receive != termKeepAliveBorrowRaw || position.DebtRaw == 0 {
		return budgetHold("term_keepalive_amount_invalid")
	}
	// Only term debt still ahead of its maturity is kept alive.
	end, err := observedDebtTermEnd(accounts, route)
	if err != nil {
		return err
	}
	if now := clockUnixTimestamp(accounts); end <= 0 || now <= 0 || end <= now {
		return budgetHold("term_keepalive_not_term_debt")
	}
	room, err := kaminoAdditionalDebtRoom(accounts, route)
	if err != nil {
		return err
	}
	debt := accountAt(accounts, route.Kamino.DebtReserve)
	if len(debt.Data) < kaminoReserveConfigOffset+48 {
		return budgetHold("term_keepalive_capacity_shrank")
	}
	rate := binary.LittleEndian.Uint64(debt.Data[kaminoReserveConfigOffset+40:])
	capped, err := leverageStepWithinRisk(position, accounts, route, min(receive, kaminoReceiveWithinRoom(room, rate)), rate)
	if err != nil || capped != receive {
		return budgetHoldFrom(err, "term_keepalive_capacity_shrank")
	}
	return nil
}

// termDays is days to maturity for logs and the gauge; false without term
// debt.
func termDays(s Snapshot) (float64, bool) {
	left, ok := termSecondsLeft(s)
	return float64(left) / 86_400, ok
}
