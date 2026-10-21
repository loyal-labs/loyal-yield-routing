package backyard

func CanTransition(from, to OperationStatus) bool {
	if from == Failed || from == ManualRecovery || from == Reconciled || from == Held {
		return false
	}
	allowed := map[OperationStatus]OperationStatus{Decided: Built, Built: Simulated, Simulated: Signed, Signed: BroadcastIntent, BroadcastIntent: Confirmed, Confirmed: Reconciling, Reconciling: Reconciled}
	if to == Failed {
		return from == Decided || from == Built || from == Simulated || from == Signed ||
			from == BroadcastIntent
	}
	if to == Reconciling {
		return from == Confirmed
	}
	return allowed[from] == to
}

func IsNonterminal(status OperationStatus) bool {
	switch status {
	case Decided, Built, Simulated, Signed, BroadcastIntent, Confirmed, Reconciling:
		return true
	default:
		return false
	}
}
