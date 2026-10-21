package fleetexec

import (
	"encoding/json"
	"time"
)

// Verify has no signer or transaction; rollover is a new physical table in a
// later generation.
type LookupKind string

const (
	LookupCreate     LookupKind = "create"
	LookupExtend     LookupKind = "extend"
	LookupVerify     LookupKind = "verify"
	LookupRollover   LookupKind = "rollover"
	LookupDeactivate LookupKind = "deactivate"
	LookupClose      LookupKind = "close"
)

type LookupLease struct {
	Owner        string
	FencingToken int64
	ExpiresAt    time.Time
}

type LookupIntent struct {
	Cluster                                   string
	OperationID, FamilyID, TableID            int64
	Kind                                      LookupKind
	TableAddress, Authority, Payer, Recipient string
	Generation                                int32
	MutationEpoch                             int64
	RecentSlot                                *uint64
	ExpectedDeactivationSlot                  *uint64
	Prefix, Extension                         []string
}

type LookupOperation struct {
	Intent                              LookupIntent
	State                               string
	Lease                               LookupLease
	FamilyState, FamilyKind, TableState string
	ManifestID, BindingID               *int64
	CatalogRevisionID                   *int64
	Context                             json.RawMessage
	Signature, MessageHash, Blockhash   *string
	LastValidBlockHeight                *int64
}

type LookupAttemptState string

const (
	LookupReconciled LookupAttemptState = "reconciled"
	LookupFailed     LookupAttemptState = "failed"
)

type LookupAttempt struct {
	SigningContextSlot                                                      int64
	Intent                                                                  LookupIntent
	Wire                                                                    WireIdentity
	BroadcastCount                                                          int
	EstimatedFeeLamports, EstimatedRentLamports, EstimatedReclaimedLamports uint64
}

type LookupSnapshot struct {
	Address, Owner                     string
	Slot                               int64
	Absent                             bool
	Lamports                           uint64
	Data                               []byte
	Authority                          string
	Addresses                          []string
	DeactivationSlot, LastExtendedSlot uint64
	LastExtendedStartIndex             uint8
	SlotHashes                         []uint64
}

type LookupReceipt struct {
	Signature                 string
	Slot                      int64
	Wire                      []byte
	Err                       string
	FeeLamports               uint64
	Addresses                 []string
	PreLamports, PostLamports []uint64
}
