package autodeposit

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
	ResultTransactionEffectAmbig ExecutorResult = "transaction_effect_ambiguous"
	ResultIdleHandoffFailed      ExecutorResult = "idle_handoff_failed"
	ResultDependencyUnavailable  ExecutorResult = "dependency_unavailable"
	ResultClaimTransitionFailed  ExecutorResult = "claim_transition_failed"
)

type ExecutorFailureAlert struct {
	Code           string
	Operation      string
	Summary        string
	Retryable      bool
	SelfRecovering bool
}

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
	case ResultPreflightBlocked:
		return &ExecutorFailureAlert{
			Code:      "autodeposit_preflight_blocked",
			Operation: "preflight_autodeposit_route",
			Summary:   "autodeposit route preflight blocked before any funds moved",
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
	case ResultClaimTransitionFailed:
		return &ExecutorFailureAlert{
			Code:      "autodeposit_claim_transition_failed",
			Operation: "release_autodeposit_claim",
			Summary:   "autodeposit claim could not be released before any funds moved",
			Retryable: true,
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

const SurplusLotClassificationDBValue = "unknown"

const ConsumerName = "balance_sweep_autodeposit_trigger"

const USDCMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

// Claim-holding pull attempt states: an attempt in one of these states means
// funds may already have moved out of the wallet, so the claim stays owned and
// a restarted worker resumes it.
var ClaimHoldingPullAttemptStates = []string{"prepared", "submitted", "confirmed"}

const (
	StaleRequestedSlotSeconds = 15 * 60
	RequestedSlotTimeoutError = "Autodeposit request timed out before worker selection."
)
