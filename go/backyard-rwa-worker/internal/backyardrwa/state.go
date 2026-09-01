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
	OpenPrimeUSDCStep     Action = "OPEN_PRIME_USDC_STEP"
	DeleverPrimeUSDCStep  Action = "DELEVER_PRIME_USDC_STEP"
	StageSquadsToVoltr    Action = "STAGE_SQUADS_TO_VOLTR"
	VoltrRestoreIdle      Action = "VOLTR_RESTORE_IDLE"
	ReportNAV             Action = "REPORT_NAV"
	HoldManualRecovery    Action = "HOLD_MANUAL_RECOVERY"
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
	ObservationID           string
	Slot                    int64
	RouteKind               string
	ManualReason            string
	Nonterminal             OperationStatus
	HasAmbiguousSubmission  bool
	WithdrawalDemandRaw     int64
	SquadsIdleRaw           int64
	VoltrStrategyIdleRaw    int64
	VoltrIdleRaw            int64
	HasPosition             bool
	LTVBPS                  int64
	LiquidationThresholdBPS int64
	NetAPYBPS               int64
	Fresh                   bool
	CapacityRaw             int64
	PolicyLimitRaw          int64
	MaxTargetLTVEntryRaw    int64
	PolicyReady             bool
	ExitBuildable           bool
	CapitalMutated          bool
	LastReportAgeSeconds    int64
}

type Decision struct {
	Action         Action
	Reason         string
	AmountRaw      int64
	IdempotencyKey string
}

func (d Decision) Validate() error {
	if d.Reason == "" || d.IdempotencyKey == "" || d.AmountRaw < 0 {
		return fmt.Errorf("incomplete decision")
	}
	switch d.Action {
	case Hold, RecoverTransaction, VoltrAllocateToSquads, OpenPrimeUSDCStep,
		DeleverPrimeUSDCStep, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV,
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
	ID       string
	RouteKey string
	Cycle    int64
	Decision Decision
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
	ConfirmationSlot int64
	Failed           bool
}
