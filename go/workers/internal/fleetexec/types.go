// Package fleetexec owns durable fleet route execution and recovery.
//
// It is the signing capability boundary for fleet planning: internal/fleet
// compiles and simulates unsigned routes and never receives a private key, and
// this package never plans. Execution persists exact signed wire before any
// broadcast, recovers ambiguous sends through the protocol-specific proof for
// that state, and keeps target capacity reserved until newer market telemetry
// reflects the movement.
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

// Terminal reports whether the submission can no longer change. Terminal
// transitions also release the target-capacity reservation through schema
// triggers, so they are only ever taken with durable proof.
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

func legalMovementLeg(leg string) bool {
	switch leg {
	case LegRoute, LegWithdraw, LegSwap, LegDeposit:
		return true
	}
	return false
}

func legalLegPurpose(purpose string) bool {
	switch purpose {
	case PurposeOptimizeYield, PurposeRecoverSource, PurposeFallbackTarget:
		return true
	}
	return false
}

var (
	// ErrStaleOwner means the caller lost the confirmation lease or its
	// fencing token is behind; the row must be re-claimed, never re-advanced.
	ErrStaleOwner = errors.New("confirmation lease owner or fencing token is stale")
	// ErrRouteContended means another live submission already owns this
	// opportunity, semantic key or signature. Never spend twice.
	ErrRouteContended = errors.New("signed route already contended by another submission")
	// ErrNotClaimable means the transition is illegal for the durable state.
	ErrNotClaimable = errors.New("submission state does not permit this transition")
	// ErrConflictLeaseHeld means a legacy route account conflict lease still
	// holds a writable account for different work.
	ErrConflictLeaseHeld = errors.New("route account conflict lease held by other work")
)
