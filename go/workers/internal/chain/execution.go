package chain

import (
	"context"
	"errors"
	"fmt"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// Execution is a landed transaction with the instructions it ran, for
// readers that must prove what a transaction did rather than only land it.
type Execution struct {
	Receipt
	Transaction *solana.Transaction
	// Inner lists the instructions each top-level instruction invoked,
	// compiled against Keys.
	Inner []rpc.InnerInstruction
}

// anyVersion accepts every transaction version the node serves: a reader
// pinned to an older version fails forever on a newer transaction.
const anyVersion = 255

// Execution reads signature's transaction at commitment (confirmed or
// finalized). ErrNotFound means the cluster has no record of it there.
func (c *Client) Execution(ctx context.Context, signature solana.Signature, commitment rpc.CommitmentType) (Execution, error) {
	version := uint64(anyVersion)
	out, err := c.rpc.GetTransaction(ctx, signature, &rpc.GetTransactionOpts{Encoding: solana.EncodingBase64, Commitment: commitment, MaxSupportedTransactionVersion: &version})
	if errors.Is(err, rpc.ErrNotFound) {
		return Execution{}, ErrNotFound
	}
	if err != nil {
		return Execution{}, failed("getTransaction", err)
	}
	if out.Slot == 0 || out.Meta == nil || out.Transaction == nil {
		return Execution{}, errors.New("getTransaction: incomplete transaction")
	}
	tx, err := out.Transaction.GetTransaction()
	if err != nil {
		return Execution{}, fmt.Errorf("getTransaction: decode: %w", err)
	}
	meta := out.Meta
	loaded := meta.LoadedAddresses
	keys := append(append(append([]solana.PublicKey(nil), tx.Message.AccountKeys...), loaded.Writable...), loaded.ReadOnly...)
	execution := Execution{Transaction: tx, Inner: meta.InnerInstructions, Receipt: Receipt{
		Slot: out.Slot, Err: meta.Err, Fee: meta.Fee, Wire: out.Transaction.GetBinary(), Logs: meta.LogMessages,
		Keys: keys, LoadedWritable: loaded.Writable, LoadedReadonly: loaded.ReadOnly,
		PreLamports: meta.PreBalances, PostLamports: meta.PostBalances,
	}}
	if execution.Pre, err = tokenBalances(keys, meta.PreTokenBalances); err != nil {
		return Execution{}, err
	}
	if execution.Post, err = tokenBalances(keys, meta.PostTokenBalances); err != nil {
		return Execution{}, err
	}
	return execution, nil
}
