package autodeposit

import (
	"errors"
	"slices"
)

type OperationKind string

const (
	OperationPull  OperationKind = "pull"
	OperationTopUp OperationKind = "top_up"
)

type AttemptState string

const (
	AttemptPrepared  AttemptState = "prepared"
	AttemptSubmitted AttemptState = "submitted"
	AttemptConfirmed AttemptState = "confirmed"
	AttemptFailed    AttemptState = "failed"
	AttemptExpired   AttemptState = "expired"
)

type DurableAttempt struct {
	ID                       int64
	ClaimToken               string
	OperationKind            OperationKind
	AttemptNumber            int
	ExecutionID              *int64
	AmountRaw                int64
	SourcePreBalanceRaw      int64
	DestinationPreBalanceRaw int64
	Signature                string
	SignedTransactionBase64  string
	SignedTransactionSHA256  string
	RecentBlockhash          string
	LastValidBlockHeight     int64
	State                    AttemptState
	BroadcastCount           int
	ConfirmedSlot            *int64
}

type AttemptObservation struct {
	State         AttemptState
	ConfirmedSlot *int64
	Err           error
}

var ErrOwnershipLost = errors.New("autodeposit claim ownership lost")

type EffectAmbiguousError struct{ Detail string }

func (e *EffectAmbiguousError) Error() string {
	return "autodeposit transaction effect is ambiguous: " + e.Detail
}

// ClaimHoldingPullAttemptStates are the attempt states in which funds may
// already have moved out of the wallet, so the claim stays owned and a
// restarted worker resumes it.
var ClaimHoldingPullAttemptStates = []string{string(AttemptPrepared), string(AttemptSubmitted), string(AttemptConfirmed)}

func AttemptHoldsClaim(state AttemptState) bool {
	return slices.Contains(ClaimHoldingPullAttemptStates, string(state))
}

func AttemptAllowsSafeRequeue(state AttemptState) bool {
	return state == AttemptFailed || state == AttemptExpired
}

type Settlement struct {
	Attempt DurableAttempt
}

type TopUpRecoveryAction string

const (
	TopUpReconcilePersisted TopUpRecoveryAction = "reconcile_persisted"
	TopUpPrepareOrRequeue   TopUpRecoveryAction = "prepare_or_requeue"
	TopUpEffectAmbiguous    TopUpRecoveryAction = "effect_ambiguous"
)

// ClassifyDirectTopUpRecovery decides a resumed deposit leg from the vault
// balance and the persisted attempt, never from a fresh spend decision.
//
// existingAttemptState nil means no top-up attempt row exists yet.
// persistedSourcePreBalanceRaw is the vault balance snapshot that attempt was
// prepared with; confirmedSiblingDepositsSinceRaw discounts later sibling
// deposits on the same vault that cannot already be inside that snapshot.
func ClassifyDirectTopUpRecovery(existingAttemptState *AttemptState, vaultAmountRaw, plannedAmountRaw int64, persistedSourcePreBalanceRaw *int64, confirmedSiblingDepositsSinceRaw int64) TopUpRecoveryAction {
	if existingAttemptState != nil && AttemptHoldsClaim(*existingAttemptState) {
		return TopUpReconcilePersisted
	}
	var expectedIfNotLanded *int64
	if persistedSourcePreBalanceRaw != nil {
		adjusted, ok := subChecked(*persistedSourcePreBalanceRaw, confirmedSiblingDepositsSinceRaw)
		if ok {
			expectedIfNotLanded = &adjusted
		}
	}
	if vaultAmountRaw < plannedAmountRaw ||
		(existingAttemptState != nil &&
			AttemptAllowsSafeRequeue(*existingAttemptState) &&
			expectedIfNotLanded != nil &&
			vaultAmountRaw < *expectedIfNotLanded) {
		return TopUpEffectAmbiguous
	}
	return TopUpPrepareOrRequeue
}
