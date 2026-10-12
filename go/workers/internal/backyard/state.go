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
	Submitted       OperationStatus = "submitted"
	Confirmed       OperationStatus = "confirmed"
	Reconciled      OperationStatus = "reconciled"
	Failed          OperationStatus = "failed"
	Reconciling     OperationStatus = "reconciling"
	ManualRecovery  OperationStatus = "manual_recovery"
	Held            OperationStatus = "held"
)

type Snapshot struct {
	ValuationSource        string `json:",omitempty"`
	ValuationSlot          int64  `json:",omitempty"`
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
	// HeldLanes is every lane holding exposure in this batch, in registry
	// order and comma separated ("" when none). RouteLane is the working
	// lane: the one lane this per-lane view describes. OtherLanesNAVRaw is
	// the part of StrategyNAVRaw the other held lanes carry.
	HeldLanes        string `json:",omitempty"`
	OtherLanesNAVRaw int64  `json:",omitempty"`
	// OverflowRoom* is, on a lane that has an overflow lane, that lane's
	// debt reserve borrow room this batch (raw debt units, and its bridge
	// USDC value, floored); unknown unless Known.
	OverflowRoomRaw      uint64 `json:",omitempty"`
	OverflowRoomValueRaw int64  `json:",omitempty"`
	OverflowRoomKnown    bool   `json:",omitempty"`
	// OverflowObligationPresent: the overflow lane's obligation exists, from
	// the same capture as its room.
	OverflowObligationPresent bool `json:",omitempty"`
	// DebtTermEndUnix is the working lane's borrow maturity on a term debt
	// reserve (KLend's last borrow plus the reserve's debt term, or the
	// market-wide maturity when earlier); zero without term debt.
	// ChainUnix is the batch's Clock time.
	DebtTermEndUnix      int64 `json:",omitempty"`
	ChainUnix            int64 `json:",omitempty"`
	VoltrStrategyIdleRaw int64
	VoltrIdleRaw         int64
	HasPosition          bool
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
	// FeeAccumulatorRaw is unharvested fee LP; LPSupplyInclFeesRaw includes it
	// and dead weight. Their ratio is warning-only. All eight same-batch fee
	// terms must match the exact approved tuple before new decisions.
	// Economic selection alone requires the coherent BOOK HWM baseline.
	// Fixed-size bytes preserve Snapshot comparability; NAV/withdrawals ignore it.
	VoltrHighWaterMarkBits    [16]byte
	VoltrHighWaterMarkKnown   bool
	FeeAccumulatorRaw         int64
	LPSupplyInclFeesRaw       int64
	ManagerPerformanceFeeBPS  int64
	AdminPerformanceFeeBPS    int64
	ManagerManagementFeeBPS   int64
	AdminManagementFeeBPS     int64
	RedemptionFeeBPS          int64
	IssuanceFeeBPS            int64
	ProtocolPerformanceFeeBPS int64
	ProtocolManagementFeeBPS  int64
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
	// LeverageTargetLevel is the lane's stored level (1, 1.5 or 1.75; 0 when
	// none is stored) and LeverageUpApproved whether the selector's
	// economics approve the up step toward it: target()'s level inputs.
	LeverageTargetLevel float64
	LeverageUpApproved  bool
	// Same-batch exact receive ceilings for the next 1.5x/1.75x step.
	// Zero known room holds borrowing, never targets repayment of existing debt.
	BorrowCapacityKnown   bool
	AdditionalDebtRoomRaw uint64
	LeverageBorrow150Raw  uint64
	BorrowDebtPriceSF     [16]byte
	BorrowUSDCPriceSF     [16]byte
	BorrowDebtDecimals    uint8
	BorrowFeeRate         uint64
	LeverageBorrow175Raw  uint64
	// LeverageDesired150Raw/175Raw are the whole step to the level inside
	// the position's risk limits, not bounded by reserve room: the size an
	// up move is priced and approved at.
	LeverageDesired150Raw uint64
	LeverageDesired175Raw uint64
	// BorrowUtilizationHeadroomRaw is the debt reserve's room to its
	// utilization borrow limit (MaxUint64 without a limit).
	BorrowUtilizationHeadroomRaw uint64
	// TopupDepositRoomRaw is the collateral reserve's remaining deposit limit,
	// valued in bridge USDC (floored, less a 1% price margin). It sizes a
	// plan B3 top-up; zero or unknown allocates nothing.
	TopupDepositRoomRaw     int64
	CapitalMutated          bool
	PostMutationNAVRequired bool
	LastReportAgeSeconds    int64
	// Target is where target() puts this lane this tick (target.go), and
	// TargetInputs every candidate lane's stored inputs it read.
	Target       LaneTarget `json:",omitempty"`
	TargetInputs string     `json:",omitempty"`
}

// heldLanes is HeldLanes as a list.
func (s Snapshot) heldLanes() []string {
	if s.HeldLanes == "" {
		return nil
	}
	return strings.Split(s.HeldLanes, ",")
}

// laneNAVRaw is the working lane's own share of StrategyNAVRaw: its
// position, custodies and the Squads cash it works with.
func laneNAVRaw(s Snapshot) int64 {
	return s.StrategyNAVRaw - s.OtherLanesNAVRaw
}

type Decision struct {
	Action         Action
	Reason         string
	AmountRaw      int64
	IdempotencyKey string
	StrategyKey    string
}

// validateSelectorInitializerDecision is the initializer decision shape: a
// registry lane with an installed initializer policy, zero amount, exact
// reason, nonempty idempotency key.
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

// Observation is one coherent bank read. Principal is chain state; reserve
// valuations may use the explicitly identified unsigned refresh source.
type Observation struct {
	planning *routePlanningState
	// Tick-local account evidence, intentionally excluded from persisted JSON.
	// It is produced only by the coherent route observer and never cached by Worker.
	routeBatch *routeObservationBatch
	// policies is where each Backyard literal is installed, read once by the
	// build this observation was prepared for (prepare*, a selector sample):
	// never by the tick's observation, never persisted or cached.
	policies installedPolicies
	// lanes are every candidate lane's view of this batch, in registry
	// order, with their stored levels: target()'s input. Snapshot is the
	// working lane's.
	lanes           []Snapshot
	ValuationSource string `json:",omitempty"`
	ValuationSlot   int64  `json:",omitempty"`
	Snapshot        Snapshot
	ObservedAt      time.Time
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
