package backyardrwa

import "fmt"

func CanTransition(from, to OperationStatus) bool {
	if from == Failed || from == ManualRecovery || from == Reconciled || from == Held {
		return false
	}
	allowed := map[OperationStatus]OperationStatus{Decided: Built, Built: Simulated, Simulated: Signed, Signed: BroadcastIntent, BroadcastIntent: Submitted, Submitted: Confirmed, Confirmed: Reconciling, Reconciling: Reconciled}
	if to == Failed {
		return from == Decided || from == Built || from == Simulated || from == Signed
	}
	if to == Reconciling {
		return from == Confirmed
	}
	return allowed[from] == to
}

func IsNonterminal(status OperationStatus) bool {
	switch status {
	case Decided, Built, Simulated, Signed, BroadcastIntent, Submitted, Confirmed, Reconciling:
		return true
	default:
		return false
	}
}

// WithdrawalPreemptsOpenLoop is the pre-broadcast safety fence. Any newly
// observed receipt cancels an OPEN operation in every state where no send could
// yet have happened. Submitted/ambiguous work is recovered by signature only.
func WithdrawalPreemptsOpenLoop(action Action, status OperationStatus, withdrawalDemandRaw int64) bool {
	if (action != OpenPrimeUSDCStep && action != SwapUSDCToPrimeStep && action != VoltrAllocateToSquads) || withdrawalDemandRaw <= 0 {
		return false
	}
	switch status {
	case Decided, Built, Simulated, Signed:
		return true
	default:
		return false
	}
}

// RecoveryWire returns only the persisted signed bytes. It never rebuilds or re-signs.
func RecoveryWire(status OperationStatus, wire []byte) ([]byte, error) {
	if status != Submitted && status != BroadcastIntent {
		return nil, fmt.Errorf("operation is not recoverable: %s", status)
	}
	if len(wire) == 0 {
		return nil, fmt.Errorf("missing persisted signed wire")
	}
	return append([]byte(nil), wire...), nil
}
