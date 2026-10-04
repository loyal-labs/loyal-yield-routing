package autodeposit

import "fmt"

// ExecutorResult is the family's durable execution outcome, independent of process status.
type ExecutorResult string

const (
	ResultUnknown                ExecutorResult = ""
	ResultCompleted              ExecutorResult = "completed"
	ResultDeferred               ExecutorResult = "deferred"
	ResultRecoveryPending        ExecutorResult = "recovery_pending"
	ResultNotActionable          ExecutorResult = "not_actionable"
	ResultNoop                   ExecutorResult = "noop"
	ResultFailed                 ExecutorResult = "failed"
	ResultKaminoTopUpFailed      ExecutorResult = "kamino_top_up_failed"
	ResultYieldPersistenceFailed ExecutorResult = "yield_persistence_failed"
	ResultPreflightBlocked       ExecutorResult = "preflight_blocked"
	ResultFeePayerExhausted      ExecutorResult = "fee_payer_exhausted"
	ResultTransactionEffectAmbig ExecutorResult = "transaction_effect_ambiguous"
	ResultIdleHandoffFailed      ExecutorResult = "idle_handoff_failed"
	ResultDependencyUnavailable  ExecutorResult = "dependency_unavailable"
)

// ExecutorFailureAlert is the operator-facing meaning of an execution outcome.
type ExecutorFailureAlert struct {
	Code      string
	Operation string
	Summary   string
	Retryable bool
	// SelfRecovering marks an outcome whose executor already scheduled its own
	// retry: visible at WARN, never paging.
	SelfRecovering bool
}

// ExecutorFailureAlertFor returns the alert an outcome deserves, or nil when the
// outcome reports a correct decision rather than a fault. A target whose vault is
// confirmed empty, or whose missing token delegate was safely quarantined, has
// nothing the executor can act on; paging on it trains real failures to be
// ignored.
func ExecutorFailureAlertFor(result ExecutorResult) *ExecutorFailureAlert {
	switch result {
	case ResultKaminoTopUpFailed:
		return &ExecutorFailureAlert{
			Code:      "kamino_top_up_failed",
			Operation: "top_up_autodeposit_to_kamino",
			Summary:   "autodeposit pull succeeded but Kamino top-up failed",
		}
	case ResultYieldPersistenceFailed:
		return &ExecutorFailureAlert{
			Code:      "yield_persistence_failed",
			Operation: "persist_autodeposit_yield_position",
			Summary:   "autodeposit top-up succeeded but yield persistence failed",
		}
	// The route itself is unexecutable and no funds moved. Waiting cannot clear
	// it, so it must not page as a lookup-table or top-up fault.
	case ResultPreflightBlocked:
		return &ExecutorFailureAlert{
			Code:      "autodeposit_preflight_blocked",
			Operation: "preflight_autodeposit_route",
			Summary:   "autodeposit route preflight blocked before any funds moved",
			Retryable: true,
		}
	// Names the remedy rather than the symptom: the answer is to send SOL.
	case ResultFeePayerExhausted:
		return &ExecutorFailureAlert{
			Code:      "autodeposit_fee_payer_exhausted",
			Operation: "fund_autodeposit_fee_payer",
			Summary:   "autodeposit fee payer is out of SOL; top up the delegated signer",
			Retryable: true,
		}
	case ResultTransactionEffectAmbig:
		return &ExecutorFailureAlert{
			Code:      "autodeposit_transaction_effect_ambiguous",
			Operation: "reconcile_autodeposit_transaction",
			Summary:   "autodeposit transaction effect remains ambiguous after blockhash expiry",
		}
	case ResultIdleHandoffFailed:
		return &ExecutorFailureAlert{
			Code:      "autodeposit_idle_handoff_failed",
			Operation: "publish_autodeposit_idle_vault_balance",
			Summary:   "confirmed autodeposit pull could not be published to idle-vault recovery",
			Retryable: true,
		}
	case ResultDependencyUnavailable:
		return &ExecutorFailureAlert{
			Code:           "autodeposit_dependency_unavailable",
			Operation:      "retry_autodeposit_after_dependency_recovers",
			Summary:        "autodeposit dependency returned a transient server error; execution will retry",
			Retryable:      true,
			SelfRecovering: true,
		}
	case ResultNotActionable, ResultCompleted, ResultDeferred, ResultRecoveryPending, ResultNoop:
		return nil
	default:
		return genericExecutorAlert()
	}
}

func genericExecutorAlert() *ExecutorFailureAlert {
	return &ExecutorFailureAlert{
		Code:      "autodeposit_executor_failed",
		Operation: "execute_eligible_autodeposit_target",
		Summary:   "autodeposit executor exited unsuccessfully",
		Retryable: true,
	}
}

// ExecutorOutcome is one scan's executor tallies.
type ExecutorOutcome struct {
	TargetsScanned            int
	ExecutionsAttempted       int
	ExecutionsCompleted       int
	ExecutionsDeferred        int
	ExecutionsRecoveryPending int
	ExecutionsNoop            int
	ExecutionsUnknown         int
	ExecutionsFailed          int
	ExecutionsNotActionable   int
	StaleRequestedSlotsFailed int64
	StaleClaimsReleased       int64
}

// RecordExecutorResult tallies a family outcome and returns its operator alert.
func (o *ExecutorOutcome) RecordExecutorResult(result ExecutorResult) *ExecutorFailureAlert {
	switch result {
	case ResultCompleted:
		o.ExecutionsCompleted++
	case ResultDeferred:
		o.ExecutionsDeferred++
	case ResultRecoveryPending:
		o.ExecutionsRecoveryPending++
	case ResultNotActionable:
		o.ExecutionsNotActionable++
	case ResultNoop:
		o.ExecutionsNoop++
	case ResultUnknown:
		o.ExecutionsUnknown++
	default:
		o.ExecutionsFailed++
	}
	return ExecutorFailureAlertFor(result)
}

// ResidualOpenLotReason is the legacy classification value written for lots the
// trigger derives. It is a storage vocabulary, not scheduler behavior.
const SurplusLotClassificationDBValue = "unknown"

// ConsumerName is the projection-offset consumer this family owns. Legacy claim
// staleness checks read the same row, so it must not be renamed per process.
const ConsumerName = "balance_sweep_autodeposit_trigger"

// USDCMint is the only mint the Autodeposit family schedules today.
const USDCMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

// Claim-holding pull attempt states: an attempt in one of these states means
// funds may already have moved out of the wallet, so the claim stays owned.
var ClaimHoldingPullAttemptStates = []string{"prepared", "submitted", "confirmed", "unknown", "ambiguous"}

// AutomaticPullRecoveryStates are the states a restarted worker resumes on its
// own. Ambiguous is excluded on purpose: an ambiguous broadcast requires an
// operator, never an automatic resend.
var AutomaticPullRecoveryStates = []string{"prepared", "submitted", "confirmed", "unknown"}

// RequestedSlotTimeoutReason is written to a slot whose request was never
// picked up before the legacy 15-minute selection deadline.
const (
	StaleRequestedSlotSeconds = 15 * 60
	RequestedSlotTimeoutError = "Autodeposit request timed out before worker selection."
)

func (a ExecutorFailureAlert) String() string {
	return fmt.Sprintf("%s (%s)", a.Code, a.Operation)
}
