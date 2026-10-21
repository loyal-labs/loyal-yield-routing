package backyard

import (
	"math/big"
)

const (
	termKeepAliveSeconds   int64 = 3 * 24 * 3600
	termDeleverSeconds     int64 = 24 * 3600
	termKeepAliveReason          = "term_keepalive"
	termKeepAliveBorrowRaw       = leverageMinimumBorrowRaw
	// termMaturityUnknown marks term debt whose borrow slot carries no borrow
	// time: its maturity cannot be known, so nothing borrows or plans on it.
	termMaturityUnknown int64 = -1
)

func applyDebtTerm(s *Snapshot, accounts []ConfirmedAccount, route RuntimeRoute) error {
	s.ChainUnix = clockUnixTimestamp(accounts)
	end, err := observedDebtTermEnd(accounts, route)
	if err != nil {
		return err
	}
	s.DebtTermEndUnix = end
	return nil
}

func termSecondsLeft(s Snapshot) (int64, bool) {
	if s.PositionDebtRaw <= 0 || s.DebtTermEndUnix <= 0 || s.ChainUnix <= 0 {
		return 0, false
	}
	return s.DebtTermEndUnix - s.ChainUnix, true
}

func termDebtUnknown(s Snapshot) bool {
	return s.PositionDebtRaw > 0 && s.DebtTermEndUnix == termMaturityUnknown
}

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

func termDeleverDue(s Snapshot) bool {
	if !earnActiveLane(s.RouteLane) || !s.HasPosition {
		return false
	}
	left, ok := termSecondsLeft(s)
	return ok && (left <= 0 || left < termDeleverSeconds && !termKeepAliveRoom(s))
}

func termDays(s Snapshot) (float64, bool) {
	left, ok := termSecondsLeft(s)
	return float64(left) / 86_400, ok
}
