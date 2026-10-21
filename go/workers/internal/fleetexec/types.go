package fleetexec

import "errors"

// SubmissionState mirrors loyal_yield.signed_route_submissions.submission_state.
// The legacy Rust confirmer and the schema CHECK own this vocabulary.
type SubmissionState string

const (
	StateSigned                SubmissionState = "signed"
	StateSubmitted             SubmissionState = "submitted"
	StateConfirmed             SubmissionState = "confirmed"
	StateReconciliationPending SubmissionState = "reconciliation_pending"
	StateExpiryCheckPending    SubmissionState = "expiry_check_pending"
	StateEffectAmbiguous       SubmissionState = "effect_ambiguous"
	StateReconciled            SubmissionState = "reconciled"
	StateExpired               SubmissionState = "expired"
	StateFailed                SubmissionState = "failed"
)

func (s SubmissionState) Terminal() bool {
	return s == StateReconciled || s == StateExpired || s == StateFailed
}

// Movement legs reuse the exact vocabulary the schema CHECK owns
// (signed_route_submissions_movement_leg_check): a same-mint route is a
// single "route" leg; a cross-mint movement carries withdraw/swap/deposit
// legs. Any other value is rejected by the database, never remapped here.
const (
	LegRoute    = "route"
	LegWithdraw = "withdraw"
	LegSwap     = "swap"
	LegDeposit  = "deposit"
)

// Leg purposes are likewise schema-owned.
const (
	PurposeOptimizeYield  = "optimize_yield"
	PurposeRecoverSource  = "recover_source"
	PurposeFallbackTarget = "fallback_target"
)

var (
	ErrStaleOwner        = errors.New("confirmation lease owner or fencing token is stale")
	ErrRouteContended    = errors.New("signed route already contended by another submission")
	ErrNotClaimable      = errors.New("submission state does not permit this transition")
	ErrConflictLeaseHeld = errors.New("route account conflict lease held by other work")
)
