package backyardrwa

import (
	"fmt"
	"time"
)

type Action string

const (
	Hold                  Action = "HOLD"
	RecoverTransaction    Action = "RECOVER_TRANSACTION"
	VoltrAllocateToSquads Action = "VOLTR_ALLOCATE_TO_SQUADS"
	SwapUSDCToPrimeStep   Action = "SWAP_USDC_TO_PRIME_STEP"
	SwapPrimeToUSDCStep   Action = "SWAP_PRIME_TO_USDC_STEP"
	OpenPrimeUSDCStep     Action = "OPEN_PRIME_USDC_STEP"
	DeleverPrimeUSDCStep  Action = "DELEVER_PRIME_USDC_STEP"
	// Typed routes use these canonical actions. The PRIME names above remain
	// accepted for historical journal and signed-wire recovery.
	SwapStableToCollateralStep Action = "SWAP_STABLE_TO_COLLATERAL_STEP"
	SwapCollateralToStableStep Action = "SWAP_COLLATERAL_TO_STABLE_STEP"
	OpenRouteStep              Action = "OPEN_ROUTE_STEP"
	DeleverRouteStep           Action = "DELEVER_ROUTE_STEP"
	SwapDebtToCollateralStep   Action = "SWAP_DEBT_TO_COLLATERAL_STEP"
	SwapCollateralToDebtStep   Action = "SWAP_COLLATERAL_TO_DEBT_STEP"
	SwapUSDCToDebtStep         Action = "SWAP_USDC_TO_DEBT_STEP"
	SwapDebtToUSDCStep         Action = "SWAP_DEBT_TO_USDC_STEP"
	StageSquadsToVoltr         Action = "STAGE_SQUADS_TO_VOLTR"
	VoltrRestoreIdle           Action = "VOLTR_RESTORE_IDLE"
	ReportNAV                  Action = "REPORT_NAV"
	HoldManualRecovery         Action = "HOLD_MANUAL_RECOVERY"
	PolicySetupPrefund         Action = "POLICY_SETUP_PREFUND"
	PolicySetupCreate          Action = "POLICY_SETUP_CREATE"
	InitializeKaminoObligation Action = "INITIALIZE_KAMINO_OBLIGATION"
)

type OperationStatus string

const (
	Decided         OperationStatus = "decided"
	Built           OperationStatus = "built"
	Simulated       OperationStatus = "simulated"
	Signed          OperationStatus = "signed"
	BroadcastIntent OperationStatus = "broadcast_intent"
	Submitted       OperationStatus = "submitted"
	Confirmed       OperationStatus = "confirmed"
	Reconciled      OperationStatus = "reconciled"
	Failed          OperationStatus = "failed"
	Reconciling     OperationStatus = "reconciling"
	ManualRecovery  OperationStatus = "manual_recovery"
	Held            OperationStatus = "held"
)

type Snapshot struct {
	ObservationID          string
	Slot                   int64
	RouteKind              string
	ManualReason           string
	Nonterminal            OperationStatus
	HasAmbiguousSubmission bool
	WithdrawalDemandRaw    int64
	SquadsIdleRaw          int64
	PrimeIdleRaw           int64
	// CollateralIdleRaw is the observed lane's idle collateral amount.
	// PrimeIdleRaw is retained for historical snapshot compatibility.
	CollateralIdleRaw int64
	// Same-batch, rounded-down bridge-USDC NAV value, used only to select a
	// plausible funding source. The executable quote minimum remains the gate.
	CollateralIdleValueRaw int64
	// Smallest input admitted by the current reserve-derived rounding bound.
	// Remainders below this stay in custody for exit, not repeated deposits.
	MinimumCollateralDepositRaw int64
	// DebtIdleRaw is in the selected debt mint's raw units. SquadsIdleRaw
	// remains bridge USDC, even when the lane borrows PYUSD/USDG/USDS.
	DebtIdleRaw int64
	// PayoffDebtRaw includes the current finite interest window for typed
	// debt lanes, including shared USDC. Observation and final-send validation independently recompute it.
	PayoffDebtRaw int64
	RouteLane     string
	StrategyKey   string
	// Unwind is an admitted full exit, independent of the user's claim amount.
	Unwind bool
	// Expired debt envelope needs fresh same-source exit admission, not a manual latch.
	UnwindRefreshRequired bool
	// PilotActive comes only from validated persisted budget authority.
	// Transaction admission rechecks that authority under the route lock.
	InitializationPolicyReady bool
	PilotActive               bool
	// PilotBaselineKnown marks a validated pilot activation whose archived
	// finalized flat evidence explains the ticket's consumed sequence before
	// this worker's journal has any reconciled ticket-consuming operation: the
	// approved operator cleanup consumed the report ticket to reach the flat
	// baseline. M8 requires PilotActive alongside this flag and then compares
	// the ticket exactly against PilotBaselineTicketSequenceRaw — the sequence
	// archived in that evidence. It is a bookkeeping fact, not a journal row:
	// it carries no NAV, arms nothing, and never explains any sequence other
	// than its own.
	PilotBaselineKnown             bool
	PilotBaselineTicketSequenceRaw int64
	SelectorEntryPaused            bool
	// Exact equity authorized by a current durable selector quote.
	SelectorEntryEquityRaw int64
	SelectorBorrowRaw      uint64
	CutoverDrain           bool
	VoltrStrategyIdleRaw   int64
	VoltrIdleRaw           int64
	HasPosition            bool
	// ObligationPresent is the observed existence of the lane's Kamino
	// obligation account, and ObligationPresenceKnown is set only by the
	// production observe path, so hand-built unit snapshots keep their existing
	// decisions. A missing obligation cannot receive a deposit: entry planning
	// holds on it instead of allocating or swapping collateral into a deposit
	// Kamino would refuse.
	ObligationPresent          bool
	ObligationPresenceKnown    bool
	PositionCollateralRaw      int64
	PositionDebtRaw            int64
	PositionCollateralValueRaw int64
	PositionDebtValueRaw       int64
	StrategyNAVRaw             int64
	TotalVaultNAVRaw           int64
	PriorReportedNAVRaw        int64
	PriorReportUpdatedUnix     int64
	ReportSequence             int64
	ReportSnapshotDigest       string
	// Voltr book reads from the same confirmed batch. VoltrTotalValueRaw closes
	// the M1 identity together with the custody Voltr books itself
	// (VoltrReceiptCustodyTrackedRaw, receipt offset 128); the observed strategy
	// custody ATA balance (VoltrStrategyIdleRaw) is deliberately absent from
	// that identity because a stage in flight moves Squads cash into the ATA
	// without invoking Voltr.
	VoltrTotalValueRaw             int64
	VoltrReceiptCustodyTrackedRaw  int64
	LockedProfitDegradationSeconds int64
	LastUpdatedLockedProfitRaw     int64
	LastLockedProfitReportUnix     int64
	// FeeAccumulatorRaw is the un-harvested LP fee Voltr has accrued, and
	// LPSupplyInclFeesRaw is the supply those fees are bounded against. The
	// performance-fee terms must both stay zero until they are calibrated.
	FeeAccumulatorRaw        int64
	LPSupplyInclFeesRaw      int64
	ManagerPerformanceFeeBPS int64
	AdminPerformanceFeeBPS   int64
	// StagedAmountRaw is the amount of the most recent reconciled
	// STAGE_SQUADS_TO_VOLTR operation for this route, with StagedAmountKnown
	// false when the journal has no such operation. A restore must debit
	// exactly this amount out of custody; anything else is a custody mismatch.
	StagedAmountRaw   int64
	StagedAmountKnown bool
	// Phase 2 monitor inputs. MonitorsArmed is set only when the serialized
	// worker merged a coherent confirmed route NAV batch, so hand-built unit
	// snapshots keep their existing decisions. The journal, ticket, and
	// program-identity fields are filled by the production observe path.
	MonitorsArmed                 bool
	TicketLastConsumedSequenceRaw int64
	JournalSequenceKnown          bool
	JournalReconciledSequenceRaw  int64
	JournalArmedNAVKnown          bool
	JournalArmedNAVRaw            int64
	// JournalArmedNAVReturnDataMissing and JournalArmedNAVMalformed record a
	// reconciled ticket-consuming operation that carries no usable adaptor
	// return data: both are durable holds, never a silent disarm.
	JournalArmedNAVReturnDataMissing bool
	JournalArmedNAVMalformed         bool
	// StageTransient is true when a reconciled stage is newer than the last
	// ticket-consuming operation, so nonzero custody is the expected stage leg.
	StageTransient bool
	// StrategyReceiptIntegrityFault records a confirmed batch whose strategy
	// receipt is absent, foreign-owned, or the wrong length: an observed
	// integrity failure that holds durably instead of failing the tick before
	// any decision exists.
	StrategyReceiptIntegrityFault bool
	ProgramIdentityKnown          bool
	VoltrProgramDeploySlot        int64
	AdaptorProgramDeploySlot      int64
	LTVBPS                        int64
	LiquidationThresholdBPS       int64
	Fresh                         bool
	CapacityRaw                   int64
	PolicyLimitRaw                int64
	MaxTargetLTVEntryRaw          int64
	BorrowUtilizationBlocked      bool
	PolicyReady                   bool
	ExitBuildable                 bool
	CapitalMutated                bool
	PostMutationNAVRequired       bool
	LastReportAgeSeconds          int64
}

type Decision struct {
	Action         Action
	Reason         string
	AmountRaw      int64
	IdempotencyKey string
	StrategyKey    string
}

func (d Decision) Validate() error {
	if d.Reason == "" || d.IdempotencyKey == "" || d.AmountRaw < 0 {
		return fmt.Errorf("incomplete decision")
	}
	if isPolicySetupAction(d.Action) {
		if d.StrategyKey != "OnRe/ONyc/USDC" || d.Reason != "phase3_policy_setup" || d.AmountRaw <= 0 {
			return fmt.Errorf("invalid policy setup decision")
		}
		return nil // Journal identity only; not a runtime lane registration.
	}
	if d.Action == InitializeKaminoObligation {
		if !selectorLane(d.StrategyKey) || d.AmountRaw != 0 || d.Reason != "multiply_obligation_missing" {
			return fmt.Errorf("invalid Multiply initialization decision")
		}
		return nil
	}
	neutral := d.Action == SwapStableToCollateralStep || d.Action == SwapCollateralToStableStep || d.Action == OpenRouteStep || d.Action == DeleverRouteStep
	catalog := false
	basic := false
	if route, err := runtimeRoute(d.StrategyKey); err == nil {
		catalog = route.Kamino.DebtMint != bridgeUSDC && len(route.KaminoPolicies) == 4
		basic = route.BasicPolicy
	}
	if neutral && d.StrategyKey != SelectedRouteID && !catalog && !basic {
		return fmt.Errorf("route-neutral action requires the selected Phase 2 strategy")
	}
	if d.StrategyKey != "" && d.StrategyKey != RouteID && d.StrategyKey != PhaseOneLaneID && d.StrategyKey != SelectedRouteID && !basic && !catalog && d.Action != HoldManualRecovery {
		return fmt.Errorf("decision strategy is not installed")
	}
	if d.Action == SwapDebtToCollateralStep || d.Action == SwapCollateralToDebtStep {
		if !catalog && !(basic && selectorLane(d.StrategyKey)) {
			return fmt.Errorf("collateral/debt conversion requires an exact runtime binding")
		}
		return nil
	}
	if d.Action == SwapUSDCToDebtStep || d.Action == SwapDebtToUSDCStep {
		if !catalog {
			return fmt.Errorf("debt conversion requires an exact non-USDC runtime binding")
		}
		return nil
	}
	switch d.Action {
	case Hold, RecoverTransaction, VoltrAllocateToSquads, SwapUSDCToPrimeStep,
		SwapPrimeToUSDCStep, OpenPrimeUSDCStep,
		DeleverPrimeUSDCStep, SwapStableToCollateralStep,
		SwapCollateralToStableStep, OpenRouteStep, DeleverRouteStep,
		StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV,
		HoldManualRecovery:
		return nil
	default:
		return fmt.Errorf("unknown decision action")
	}
}

// Observation is one coherent confirmed read. All balances and position values
// in Snapshot must come from this slot.
type Observation struct {
	Snapshot   Snapshot
	ObservedAt time.Time
}

// Operation is the durable journal identity created before transaction work.
type Operation struct {
	ID          string
	RouteKey    string
	Cycle       int64
	StrategyKey string
	Decision    Decision
}

// PersistedOperation is the durable execution state loaded before any new
// observation is allowed. SignedWire is the only wire recovery may inspect;
// recovery must never rebuild or re-sign it.
type PersistedOperation struct {
	Operation
	Status                  OperationStatus
	ExpectedEffects         []byte
	SignedWire              []byte
	SignedWireSHA256        string
	TransactionSignature    string
	RecentBlockhash         string
	LastValidBlockHeight    int64
	BroadcastIntentRecorded bool
	ConfirmedSlot           int64
}

// BuildResult is the exact signed transaction that passed simulation. It must
// be persisted before broadcast intent is recorded or the wire is submitted.
type BuildResult struct {
	MessageSHA256        string
	SignedWire           []byte
	SignedWireSHA256     string
	TransactionSignature string
	RecentBlockhash      string
	LastValidBlockHeight int64
	SimulationSlot       int64
}

// Reconciliation is independently observed after confirmation. EffectsSHA256
// identifies canonical observed balance deltas, not the RPC send response.
type Reconciliation struct {
	ConfirmedSlot int64
	EffectsSHA256 string
	Conserved     bool
}

type SimulationResult struct {
	Slot          int64
	UnitsConsumed uint64
	Logs          []string
}

type SignatureObservation struct {
	Found            bool
	Confirmed        bool
	Finalized        bool
	Settled          bool
	ProcessedOnly    bool
	ConfirmationSlot int64
	// Failed is true only for a settled (confirmed/finalized) on-chain error.
	// A processed-only failure is never reported here: it can still be forked
	// away, so it stays an observation, not a transition.
	Failed bool
}
