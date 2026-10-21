package fleetexec

func legalTransition(from, to SubmissionState) bool {
	switch from {
	case StateSigned, StateSubmitted, StateExpiryCheckPending:
		return to == StateConfirmed || to == StateExpired || to == StateFailed
	case StateConfirmed:
		return to == StateReconciliationPending
	case StateReconciliationPending:
		return to == StateReconciled || to == StateFailed || to == StateReconciliationPending
	}
	return false
}
