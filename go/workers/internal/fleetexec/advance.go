package fleetexec

// legalTransition is the same-mint durable state machine shared with the
// legacy Rust confirmer. A signed or sent row lands, fails on chain, or
// expires; expiry_check_pending rows were written by a Rust confirmer before
// a swap and resolve the same way.
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
