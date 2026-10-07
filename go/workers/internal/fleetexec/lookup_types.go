package fleetexec

import (
	"context"
	"encoding/json"
	"time"
)

// LookupKind is the existing 0017 operation vocabulary. Verify has no signer
// or transaction; rollover is a new physical table in a later generation.
type LookupKind string

const (
	LookupCreate     LookupKind = "create"
	LookupExtend     LookupKind = "extend"
	LookupVerify     LookupKind = "verify"
	LookupRollover   LookupKind = "rollover"
	LookupDeactivate LookupKind = "deactivate"
	LookupClose      LookupKind = "close"
)

// LookupLease is the source operation's owner/fencing lease, not a new lock.
type LookupLease struct {
	Owner        string
	FencingToken int64
	ExpiresAt    time.Time
}

// LookupIntent freezes only the fields which determine one ALT mutation.
// Prefix is ordered physical membership, not a set. Authority and payer must
// both be the family's standard policy signer; close refunds that same key.
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

// LookupOperation is a leased source work item. A signed operation carries
// its packet identity in the Rust columns and, when Go signed it, the exact
// bytes in operation_context.
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

// Proof outcomes for one signed packet.
type LookupAttemptState string

const (
	LookupReconciled LookupAttemptState = "reconciled"
	LookupFailed     LookupAttemptState = "failed"
)

// LookupAttempt is the signed packet a source operation row carries.
// Wire.SignedTransaction is empty for a packet the Rust provisioner signed:
// it can be resolved by its signature but never resent.
type LookupAttempt struct {
	SigningContextSlot                                                      int64
	Intent                                                                  LookupIntent
	Wire                                                                    WireIdentity
	BroadcastCount                                                          int
	EstimatedFeeLamports, EstimatedRentLamports, EstimatedReclaimedLamports uint64
}

// LookupSnapshot comes from a coherent finalized account/table+SlotHashes read.
// Absent is explicit; missing or undecodable RPC evidence never becomes absent.
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

// LookupReceipt retains actual finalized packet and SOL metadata. Every index
// is resolved against that signed packet before accounting or refund proof.
type LookupReceipt struct {
	Signature                 string
	Slot                      int64
	Wire                      []byte
	Err                       string
	FeeLamports               uint64
	Addresses                 []string
	PreLamports, PostLamports []uint64
}

// LookupChain contains only this writer's chain capabilities. Consumers which
// plan catalog/shard allocation do not receive the provisioner's signing key.
type LookupChain interface {
	LookupSnapshot(context.Context, string, int64) (LookupSnapshot, error)
	SignatureStatus(context.Context, string) (SignatureStatus, error)
	LookupFinalizedReceipt(context.Context, string) (*LookupReceipt, error)
	LookupBlockhash(context.Context) (string, int64, int64, error)
	LookupFee(context.Context, []byte) (uint64, error)
	LookupRent(context.Context, int) (uint64, error)
	LookupBalance(context.Context, string) (uint64, error)
	SimulateLookup(context.Context, []byte) error
}
