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

type TopUpRecoveryInput struct {
	ExistingAttemptState             *AttemptState
	VaultAmountRaw                   int64
	PlannedAmountRaw                 int64
	PersistedSourcePreBalanceRaw     *int64
	ConfirmedSiblingDepositsSinceRaw int64
}

// ClassifyDirectTopUpRecovery decides a resumed deposit leg from the vault balance and the persisted attempt, never a fresh spend.
func ClassifyDirectTopUpRecovery(in TopUpRecoveryInput) TopUpRecoveryAction {
	if in.ExistingAttemptState != nil && AttemptHoldsClaim(*in.ExistingAttemptState) {
		return TopUpReconcilePersisted
	}
	var expectedIfNotLanded *int64
	if in.PersistedSourcePreBalanceRaw != nil {
		adjusted, ok := subChecked(*in.PersistedSourcePreBalanceRaw, in.ConfirmedSiblingDepositsSinceRaw)
		if ok {
			expectedIfNotLanded = &adjusted
		}
	}
	if in.VaultAmountRaw < in.PlannedAmountRaw ||
		(in.ExistingAttemptState != nil &&
			AttemptAllowsSafeRequeue(*in.ExistingAttemptState) &&
			expectedIfNotLanded != nil &&
			in.VaultAmountRaw < *expectedIfNotLanded) {
		return TopUpEffectAmbiguous
	}
	return TopUpPrepareOrRequeue
}
