package backyard

import (
	"fmt"
	"strings"
	"time"
)

type Action string

const (
	Hold                       Action = "HOLD"
	RecoverTransaction         Action = "RECOVER_TRANSACTION"
	VoltrAllocateToSquads      Action = "VOLTR_ALLOCATE_TO_SQUADS"
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
	InitializeKaminoObligation Action = "INITIALIZE_KAMINO_OBLIGATION"
)

type OperationStatus string

const (
	Decided         OperationStatus = "decided"
	Built           OperationStatus = "built"
	Simulated       OperationStatus = "simulated"
	Signed          OperationStatus = "signed"
	BroadcastIntent OperationStatus = "broadcast_intent"
	Confirmed       OperationStatus = "confirmed"
	Reconciled      OperationStatus = "reconciled"
	Failed          OperationStatus = "failed"
	Reconciling     OperationStatus = "reconciling"
	ManualRecovery  OperationStatus = "manual_recovery"
	Held            OperationStatus = "held"
)

func (s OperationStatus) unsigned() bool {
	return s == Decided || s == Built || s == Simulated
}

func (s OperationStatus) signedUnlanded() bool {
	return s == Signed || s == BroadcastIntent
}

func (s OperationStatus) preBroadcast() bool {
	return s.unsigned() || s == Signed
}

type debtCustodyRaw int64

// StageAllowance is what Squads lets the stage policy move now: RemainingRaw
// until ResetAt (unix seconds, lastReset plus period), MaxRaw after it.
type StageAllowance struct {
	RemainingRaw int64
	MaxRaw       int64
	ResetAt      int64
	ObservedAt   int64
}

func (a *StageAllowance) cap(idle int64) int64 {
	switch {
	case a == nil:
		return idle
	case a.ObservedAt >= a.ResetAt:
		return min(idle, a.MaxRaw)
	}
	return min(idle, a.RemainingRaw)
}

type Snapshot struct {
	ValuationSource             string `json:",omitempty"`
	ValuationSlot               int64  `json:",omitempty"`
	ObservationID               string
	Slot                        int64
	RouteKind                   string
	ManualReason                string
	Nonterminal                 OperationStatus
	HasAmbiguousSubmission      bool
	WithdrawalDemandRaw         int64
	SquadsIdleRaw               int64
	CollateralIdleRaw           int64
	CollateralIdleValueRaw      int64
	MinimumCollateralDepositRaw int64
	DebtIdleRaw                 debtCustodyRaw
	PayoffDebtRaw               int64
	RouteLane                   string
	StrategyKey                 string
	// HeldLanes is every lane holding exposure in this batch, in registry
	// order and comma separated ("" when none). RouteLane is the working
	// lane: the one lane this per-lane view describes.
	HeldLanes    string              `json:",omitempty"`
	OverflowRoom *overflowBorrowRoom `json:",omitempty"`
	// DebtTermEndUnix is KLend's term-debt maturity: the last borrow plus the
	// reserve's debt term, or the market-wide maturity when earlier.
	DebtTermEndUnix                int64 `json:",omitempty"`
	ChainUnix                      int64 `json:",omitempty"`
	VoltrStrategyIdleRaw           int64
	VoltrIdleRaw                   int64
	HasPosition                    bool
	Obligation                     obligationPresence
	PositionCollateralRaw          int64
	PositionDebtRaw                int64
	PositionCollateralValueRaw     int64
	PositionDebtValueRaw           int64
	StrategyNAVRaw                 int64
	TotalVaultNAVRaw               int64
	PriorReportedNAVRaw            int64
	PriorReportUpdatedUnix         int64
	ReportSequence                 int64
	ReportSnapshotDigest           string
	VoltrTotalValueRaw             int64
	VoltrReceiptCustodyTrackedRaw  int64
	LockedProfitDegradationSeconds int64
	LastUpdatedLockedProfitRaw     int64
	LastLockedProfitReportUnix     int64
	VoltrHighWaterMarkBits         [16]byte
	FeeAccumulatorRaw              int64
	LPSupplyInclFeesRaw            int64
	ManagerPerformanceFeeBPS       int64
	AdminPerformanceFeeBPS         int64
	ManagerManagementFeeBPS        int64
	AdminManagementFeeBPS          int64
	RedemptionFeeBPS               int64
	IssuanceFeeBPS                 int64
	ProtocolPerformanceFeeBPS      int64
	ProtocolManagementFeeBPS       int64
	StagedAmountRaw                int64
	StagedAmountKnown              bool
	Trustful                       bool  `json:",omitempty"`
	ApprovedAdminPerformanceFeeBPS int64 `json:",omitempty"`
	// Phase 2 monitor inputs. MonitorsArmed is set only when the serialized
	// worker merged a coherent confirmed route NAV batch, so hand-built unit
	// snapshots keep their existing decisions. The journal, ticket, and
	// program-identity fields are filled by the production observe path.
	MonitorsArmed                    bool
	TicketLastConsumedSequenceRaw    int64
	JournalSequenceKnown             bool
	JournalReconciledSequenceRaw     int64
	JournalArmedNAVKnown             bool
	JournalArmedNAVRaw               int64
	JournalArmedNAVReturnDataMissing bool
	JournalArmedNAVMalformed         bool
	StageTransient                   bool
	StrategyReceiptIntegrityFault    bool
	ProgramIdentityKnown             bool
	VoltrProgramDeploySlot           int64
	AdaptorProgramDeploySlot         int64
	LTVBPS                           int64
	LiquidationThresholdBPS          int64
	MaxLTVBPS                        int64
	Fresh                            bool
	CapacityRaw                      int64
	PolicyLimitRaw                   int64
	MaxTargetLTVEntryRaw             int64
	BorrowUtilizationBlocked         bool
	LeverageTargetLevel              float64
	LeverageUpApproved               bool
	BorrowCapacityKnown              bool
	AdditionalDebtRoomRaw            uint64
	LeverageBorrowRaw                [len(leverageLadder)]uint64
	BorrowDebtPriceSF                [16]byte
	BorrowUSDCPriceSF                [16]byte
	BorrowDebtDecimals               uint8
	BorrowFeeRate                    uint64
	LeverageDesiredRaw               [len(leverageLadder)]uint64
	BorrowUtilizationHeadroomRaw     uint64
	TopupDepositRoomRaw              int64
	CapitalMutated                   bool
	PostMutationNAVRequired          bool
	LastReportAgeSeconds             int64
	StageAllowance                   *StageAllowance `json:",omitempty"`
	Target                           LaneTarget      `json:",omitempty"`
	TargetInputs                     string          `json:",omitempty"`
}

func (s Snapshot) valued() bool {
	return s.ManualReason == "" && !s.StrategyReceiptIntegrityFault
}

func (s Snapshot) heldLanes() []string {
	if s.HeldLanes == "" {
		return nil
	}
	return strings.Split(s.HeldLanes, ",")
}

type obligationPresence uint8

const (
	obligationUnobserved obligationPresence = iota
	obligationAbsent
	obligationPresent
)

func observedObligation(present bool) obligationPresence {
	if present {
		return obligationPresent
	}
	return obligationAbsent
}

type overflowBorrowRoom struct {
	DebtRaw          uint64
	FlooredUSDCValue int64
	Obligation       obligationPresence
}

type Decision struct {
	Action         Action
	Reason         string
	AmountRaw      int64
	IdempotencyKey string
	StrategyKey    string
}

func validateSelectorInitializerDecision(d Decision) error {
	if !earnInitializerLane(d.StrategyKey) || d.AmountRaw != 0 || d.Reason != "multiply_obligation_missing" || d.IdempotencyKey == "" {
		return fmt.Errorf("invalid Multiply initialization decision")
	}
	return nil
}

func (d Decision) Validate() error {
	if d.Reason == "" || d.IdempotencyKey == "" || d.AmountRaw < 0 {
		return fmt.Errorf("incomplete decision")
	}
	switch d.Action {
	case InitializeKaminoObligation:
		return validateSelectorInitializerDecision(d)
	case Hold, HoldManualRecovery, RecoverTransaction, VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV:
		if d.StrategyKey != "" && !earnHeldLane(d.StrategyKey) && d.Action != HoldManualRecovery {
			return fmt.Errorf("decision strategy is not installed")
		}
		return nil
	case SwapStableToCollateralStep, SwapCollateralToStableStep, OpenRouteStep, DeleverRouteStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep:
		if !earnHeldLane(d.StrategyKey) {
			return fmt.Errorf("lane action requires a registry lane")
		}
		return nil
	case SwapUSDCToDebtStep, SwapDebtToUSDCStep:
		if !catalogJupiterRoute(d.StrategyKey) || sharedUSDCDebt(d.StrategyKey) {
			return fmt.Errorf("debt conversion requires a non-USDC debt lane")
		}
		return nil
	}
	return fmt.Errorf("unknown decision action")
}

type Observation struct {
	planning        *routePlanningState
	routeBatch      *routeObservationBatch
	policies        installedPolicies
	lanes           []Snapshot
	ValuationSource string `json:",omitempty"`
	ValuationSlot   int64  `json:",omitempty"`
	Snapshot        Snapshot
	ObservedAt      time.Time
}

type Operation struct {
	ID          string
	RouteKey    string
	Cycle       int64
	StrategyKey string
	Decision    Decision
}

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
