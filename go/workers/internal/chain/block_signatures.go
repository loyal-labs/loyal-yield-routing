package chain

import (
	"context"
	"errors"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// FinalizedBlockSignatures provides canonical transaction ordering within a slot.
func (c *Client) FinalizedBlockSignatures(ctx context.Context, slot uint64) ([]solana.Signature, time.Time, error) {
	version, rewards := uint64(anyVersion), false
	block, err := c.rpc.GetBlockWithOpts(ctx, slot, &rpc.GetBlockOpts{
		Commitment: rpc.CommitmentFinalized, TransactionDetails: rpc.TransactionDetailsSignatures,
		MaxSupportedTransactionVersion: &version, Rewards: &rewards,
	})
	if err != nil {
		return nil, time.Time{}, failed("getBlock", err)
	}
	if block == nil || len(block.Signatures) == 0 || block.BlockTime == nil {
		return nil, time.Time{}, errors.New("finalized block signature list is missing")
	}
	return block.Signatures, block.BlockTime.Time().UTC(), nil
}
