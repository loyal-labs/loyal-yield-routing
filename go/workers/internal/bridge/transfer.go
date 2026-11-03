package bridge

type TransferState string

const (
	TransferQuoted    TransferState = "quoted"
	TransferSubmitted TransferState = "submitted"
	TransferFilled    TransferState = "filled"
	TransferRefunded  TransferState = "refunded"
	TransferFailed    TransferState = "failed"
)

func (s TransferState) Settled() bool {
	return s == TransferFilled || s == TransferRefunded
}

// Submit records that the client posted its transaction. Only a quoted
// transfer moves.
func (s TransferState) Submit() TransferState {
	if s == TransferQuoted {
		return TransferSubmitted
	}
	return s
}

// Advance applies one Relay status read. The bool reports whether Relay's
// status is one we know; an unknown status leaves the state alone. The state
// only moves to a higher rank, so a failed transfer can still refund or fill.
func (s TransferState) Advance(relayStatus string) (TransferState, bool) {
	var seen TransferState
	switch relayStatus {
	case "waiting":
		seen = TransferQuoted
	case "depositing", "pending", "submitted", "delayed":
		seen = TransferSubmitted
	case "success":
		seen = TransferFilled
	case "refund":
		seen = TransferRefunded
	case "failure":
		seen = TransferFailed
	default:
		return s, false
	}
	if rank(seen) <= rank(s) {
		return s, true
	}
	return seen, true
}

func rank(s TransferState) int {
	switch s {
	case TransferQuoted:
		return 0
	case TransferSubmitted:
		return 1
	case TransferFailed:
		return 2
	}
	return 3
}
