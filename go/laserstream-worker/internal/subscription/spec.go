package subscription

import (
	"errors"
	"fmt"
	"sort"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
)

const (
	KaminoReserves              = "kamino_reserves"
	BalanceSweepWalletATAs      = "balance_sweep_wallet_atas"
	EarnMaxPolicyTransactions   = "earn_max_policy_transactions"
	StreamProgress              = "stream_progress"
	SquadsSmartAccountProgramID = "SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG"
)

type AccountFilter struct {
	Addresses           []string
	RequireTxnSignature bool
}

type Spec struct {
	FromSlot      uint64
	Accounts      map[string]AccountFilter
	PolicyProgram string
}

// Build creates the single combined request used by the Go worker. Earn's role
// labels are supplied in Accounts alongside the two required core labels.
func Build(spec Spec) (*pb.SubscribeRequest, error) {
	if spec.FromSlot == 0 {
		return nil, errors.New("combined LaserStream from_slot must be non-zero")
	}
	accounts := make(map[string]*pb.SubscribeRequestFilterAccounts, len(spec.Accounts))
	for label, filter := range spec.Accounts {
		if label == "" {
			return nil, errors.New("account filter label must not be empty")
		}
		addresses := append([]string(nil), filter.Addresses...)
		sort.Strings(addresses)
		addresses = compact(addresses)
		if len(addresses) == 0 {
			return nil, fmt.Errorf("account filter %q is empty and would subscribe to all accounts", label)
		}
		requireSignature := filter.RequireTxnSignature
		accounts[label] = &pb.SubscribeRequestFilterAccounts{
			Account:              addresses,
			NonemptyTxnSignature: &requireSignature,
		}
	}
	for _, required := range []string{KaminoReserves, BalanceSweepWalletATAs} {
		if _, ok := accounts[required]; !ok {
			return nil, fmt.Errorf("required combined account filter %q is missing", required)
		}
	}

	policyProgram := spec.PolicyProgram
	if policyProgram == "" {
		policyProgram = SquadsSmartAccountProgramID
	}
	vote := false
	failed := false
	filterByCommitment := true
	confirmed := pb.CommitmentLevel_CONFIRMED
	return &pb.SubscribeRequest{
		Accounts: accounts,
		Transactions: map[string]*pb.SubscribeRequestFilterTransactions{
			EarnMaxPolicyTransactions: {
				Vote:           &vote,
				Failed:         &failed,
				AccountInclude: []string{policyProgram},
			},
		},
		Slots: map[string]*pb.SubscribeRequestFilterSlots{
			StreamProgress: {FilterByCommitment: &filterByCommitment},
		},
		Commitment: &confirmed,
		FromSlot:   pointer(spec.FromSlot),
	}, nil
}

func compact(values []string) []string {
	if len(values) < 2 {
		return values
	}
	write := 1
	for read := 1; read < len(values); read++ {
		if values[read] == values[write-1] {
			continue
		}
		values[write] = values[read]
		write++
	}
	return values[:write]
}

func pointer(value uint64) *uint64 { return &value }
