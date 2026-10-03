package autodeposit

import (
	"context"
	"errors"
	"fmt"
)

// OperationKind is one leg of an autodeposit: the wallet pull, then the Kamino
// top-up. They are separate chain transactions by contract.
type OperationKind string

const (
	OperationPull  OperationKind = "pull"
	OperationTopUp OperationKind = "top_up"
)

// AttemptState mirrors loyal_yield.balance_sweep_transaction_attempts.attempt_state.
type AttemptState string

const (
	AttemptPrepared  AttemptState = "prepared"
	AttemptSubmitted AttemptState = "submitted"
	AttemptConfirmed AttemptState = "confirmed"
	AttemptFailed    AttemptState = "failed"
	AttemptExpired   AttemptState = "expired"
	AttemptUnknown   AttemptState = "unknown"
	AttemptAmbiguous AttemptState = "ambiguous"
)

// DurableAttempt is one persisted signed transaction. Its wire identity —
// signature, exact bytes, hash, blockhash and validity window — is immutable
// once stored; only state, broadcast count and observation fields advance.
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

// AttemptObservation is one chain reading of a persisted signature.
type AttemptObservation struct {
	State         AttemptState
	ConfirmedSlot *int64
	Err           error
}

// ErrOwnershipLost reports that another executor owns the claim now.
var ErrOwnershipLost = errors.New("autodeposit claim ownership lost")

// ErrEffectAmbiguous reports that a deposit's chain effect cannot be proven
// either way; submitting another deposit would risk double spending.
type EffectAmbiguousError struct{ Detail string }

func (e *EffectAmbiguousError) Error() string {
	return "autodeposit transaction effect is ambiguous: " + e.Detail
}

// AttemptHoldsClaim reports whether the attempt state still owns the claim's
// funds: the wire may already have landed, so the custody cannot be re-claimed.
func AttemptHoldsClaim(state AttemptState) bool {
	switch state {
	case AttemptPrepared, AttemptSubmitted, AttemptConfirmed, AttemptUnknown, AttemptAmbiguous:
		return true
	default:
		return false
	}
}

// AttemptAllowsSafeRequeue reports whether a conclusive failure leaves the
// claim's funds provably unmoved, so a fresh attempt with a new signature may be
// prepared.
func AttemptAllowsSafeRequeue(state AttemptState) bool {
	return state == AttemptFailed || state == AttemptExpired
}

// AlertForAttemptState is the only attempt state that pages: an ambiguous
// effect survives blockhash expiry and no code can clear it.
func AlertForAttemptState(state AttemptState) *ExecutorFailureAlert {
	if state != AttemptAmbiguous {
		return nil
	}
	return &ExecutorFailureAlert{
		Code:      "autodeposit_transaction_effect_ambiguous",
		Operation: "reconcile_autodeposit_transaction",
		Summary:   "autodeposit transaction effect remains ambiguous after blockhash expiry",
	}
}

// AttemptDependencies are the consumer-provided effects of settlement. The
// family owns the decision order; the owner of the RPC client and store owns
// the implementations, so this package never touches a network or a database.
type AttemptDependencies interface {
	Observe(ctx context.Context, attempt DurableAttempt) (AttemptObservation, error)
	// BroadcastExact resubmits the persisted bytes and returns the signature the
	// cluster acknowledged.
	BroadcastExact(ctx context.Context, attempt DurableAttempt) (string, error)
	RecordBroadcast(ctx context.Context, attempt DurableAttempt) (DurableAttempt, error)
	RecordObservation(ctx context.Context, attempt DurableAttempt, observation AttemptObservation) (DurableAttempt, error)
}

// Settlement is the outcome of resolving one persisted attempt.
type Settlement struct {
	Attempt     DurableAttempt
	Observation AttemptObservation
	Broadcasted bool
}

func observationIsTerminal(observation AttemptObservation) bool {
	return observation.State != AttemptUnknown
}

// SettleDurableAttempt resolves one persisted signed attempt without ever
// constructing a replacement. Re-broadcast is safe because it uses the
// immutable bytes whose deterministic signature is already stored; anything else
// would be a new spend decided from a balance delta.
//
// Observe first: a terminal observation records and stops. Otherwise rebroadcast
// the exact bytes, verify the cluster returned the persisted signature, and only
// then record the broadcast; a broadcast that errors falls back to a fresh
// observation so an already-landed submission is not mislabeled as failed.
func SettleDurableAttempt(ctx context.Context, attempt DurableAttempt, dependencies AttemptDependencies) (Settlement, error) {
	if attempt.Signature == "" || attempt.SignedTransactionBase64 == "" {
		return Settlement{}, errors.New("durable attempt has no persisted wire identity")
	}
	observation, err := dependencies.Observe(ctx, attempt)
	if err != nil {
		return Settlement{}, err
	}
	if observationIsTerminal(observation) {
		recorded, err := dependencies.RecordObservation(ctx, attempt, observation)
		if err != nil {
			return Settlement{}, err
		}
		return Settlement{Attempt: recorded, Observation: observation}, nil
	}

	broadcasted := false
	returnedSignature, err := dependencies.BroadcastExact(ctx, attempt)
	if err == nil {
		if returnedSignature != attempt.Signature {
			err = fmt.Errorf("RPC returned signature %s, expected persisted signature %s", returnedSignature, attempt.Signature)
		}
	}
	if err == nil {
		recorded, recordErr := dependencies.RecordBroadcast(ctx, attempt)
		if recordErr != nil {
			return Settlement{}, recordErr
		}
		attempt = recorded
		broadcasted = true
		observation, err = dependencies.Observe(ctx, attempt)
		if err != nil {
			return Settlement{}, err
		}
	} else {
		retryObservation, observeErr := dependencies.Observe(ctx, attempt)
		if observeErr != nil {
			return Settlement{}, observeErr
		}
		observation = retryObservation
		if observation.State == AttemptUnknown && observation.Err == nil {
			observation.Err = err
		}
	}
	recorded, err := dependencies.RecordObservation(ctx, attempt, observation)
	if err != nil {
		return Settlement{}, err
	}
	return Settlement{Attempt: recorded, Observation: observation, Broadcasted: broadcasted}, nil
}

// TopUpRecoveryAction classifies what a resumed claim does about its deposit leg.
type TopUpRecoveryAction string

const (
	// TopUpReconcilePersisted: the persisted top-up still holds the claim, so
	// reconcile its signature instead of preparing another deposit.
	TopUpReconcilePersisted TopUpRecoveryAction = "reconcile_persisted"
	// TopUpPrepareOrRequeue: no live attempt holds the claim and the vault balance
	// is consistent with the deposit not having landed; prepare or requeue.
	TopUpPrepareOrRequeue TopUpRecoveryAction = "prepare_or_requeue"
	// TopUpEffectAmbiguous: the vault balance is lower than the plan can explain,
	// so the deposit may have landed in a way nothing recorded. Refuse.
	TopUpEffectAmbiguous TopUpRecoveryAction = "effect_ambiguous"
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
