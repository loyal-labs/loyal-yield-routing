package fleetexec

import (
	"errors"
	"slices"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

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
	return slices.Contains(fleet.SubmissionTerminalStates, string(s))
}

type MovementLeg string

const (
	LegRoute    MovementLeg = "route"
	LegWithdraw MovementLeg = "withdraw"
	LegSwap     MovementLeg = "swap"
	LegDeposit  MovementLeg = "deposit"
)

type LegPurpose string

const (
	PurposeOptimizeYield  LegPurpose = "optimize_yield"
	PurposeRecoverSource  LegPurpose = "recover_source"
	PurposeFallbackTarget LegPurpose = "fallback_target"
)

var (
	ErrStaleOwner        = errors.New("confirmation lease owner or fencing token is stale")
	ErrRouteContended    = errors.New("signed route already contended by another submission")
	ErrNotClaimable      = errors.New("submission state does not permit this transition")
	ErrConflictLeaseHeld = errors.New("route account conflict lease held by other work")
)
