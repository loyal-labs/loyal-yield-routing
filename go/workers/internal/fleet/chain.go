package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	solana "github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// AccountReader is the account read retail decisions are made from.
type AccountReader interface {
	Accounts(ctx context.Context, keys []solana.PublicKey, commitment rpc.CommitmentType, minContextSlot uint64) (uint64, []*chain.Account, error)
}

// ReadAccounts reads addresses at commitment from a node at or past minSlot.
// An absent account is nil; whether that is allowed is the caller's decision.
func ReadAccounts(ctx context.Context, c AccountReader, addresses []string, commitment rpc.CommitmentType, minSlot int64) (int64, []*chain.Account, error) {
	keys := make([]solana.PublicKey, len(addresses))
	for i, address := range addresses {
		key, err := solana.PublicKeyFromBase58(address)
		if err != nil {
			return 0, nil, fmt.Errorf("account address %q: %w", address, err)
		}
		keys[i] = key
	}
	slot, accounts, err := c.Accounts(ctx, keys, commitment, uint64(max(minSlot, 0)))
	return int64(slot), accounts, err
}

// SimulateExact runs wire as signed, without signature checks or a
// replacement blockhash, at commitment on a node at or past minSlot. A
// transaction the cluster ran and failed is evidence, not an error.
func SimulateExact(ctx context.Context, c *chain.Client, wire []byte, commitment rpc.CommitmentType, minSlot int64) (SimulationEvidence, error) {
	hash := sha256.Sum256(wire)
	evidence := SimulationEvidence{WireSHA256: hex.EncodeToString(hash[:])}
	floor := uint64(max(minSlot, 0))
	simulated, err := c.Simulate(ctx, wire, rpc.SimulateTransactionOpts{Commitment: commitment, MinContextSlot: &floor})
	var failure *chain.SimulationError
	if errors.As(err, &failure) {
		text, _ := json.Marshal(failure.Err)
		evidence.Slot, evidence.Error = int64(failure.Slot), string(text)
		return evidence, nil
	}
	if err != nil {
		return evidence, err
	}
	evidence.Slot, evidence.UnitsConsumed, evidence.Succeeded = int64(simulated.Slot), simulated.Units, true
	return evidence, nil
}

// recentPriorityFee is the 75th percentile of the prices recent transactions
// writing the first 128 of writable paid, zero when none did.
func recentPriorityFee(ctx context.Context, c *chain.Client, writable []string) (uint64, error) {
	keys := make([]solana.PublicKey, 0, min(len(writable), 128))
	for _, address := range writable[:min(len(writable), 128)] {
		key, err := solana.PublicKeyFromBase58(address)
		if err != nil {
			return 0, fmt.Errorf("writable account %q: %w", address, err)
		}
		keys = append(keys, key)
	}
	fees, err := c.PriorityFees(ctx, keys)
	if err != nil || len(fees) == 0 {
		return 0, err
	}
	slices.Sort(fees)
	return fees[(len(fees)*75+99)/100-1], nil
}
