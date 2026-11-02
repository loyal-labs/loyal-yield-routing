package bridge

type TransferState string

const (
	TransferQuoted    TransferState = "quoted"
	TransferSubmitted TransferState = "submitted"
	TransferFilled    TransferState = "filled"
	TransferRefunded  TransferState = "refunded"
	TransferFailed    TransferState = "failed"
)

func (s TransferState) Terminal() bool {
	return s == TransferFilled || s == TransferRefunded || s == TransferFailed
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
// status is one we know; an unknown status leaves the state alone. A terminal
// state never changes, and a quoted transfer stays quoted until the client
// submits or Relay reports an outcome.
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
	if s.Terminal() || rank(seen) <= rank(s) {
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
	}
	return 2
}
