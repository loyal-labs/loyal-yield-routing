package chain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
	"github.com/solana-foundation/solana-go/v2/rpc/jsonrpc"
)

type Execution struct {
	Receipt
	Transaction *solana.Transaction
	Inner       []rpc.InnerInstruction
	BlockTime   *time.Time
}

// anyVersion accepts every transaction version the node serves: a reader
// pinned to an older version fails forever on a newer transaction.
const anyVersion = 255

func (c *Client) Execution(ctx context.Context, signature solana.Signature, commitment rpc.CommitmentType) (Execution, error) {
	version := uint64(anyVersion)
	out, err := c.rpc.GetTransaction(ctx, signature, &rpc.GetTransactionOpts{Encoding: solana.EncodingBase64, Commitment: commitment, MaxSupportedTransactionVersion: &version})
	if errors.Is(err, rpc.ErrNotFound) {
		return Execution{}, ErrNotFound
	}
	if err != nil {
		return Execution{}, failed("getTransaction", err)
	}
	return decodeExecution(out)
}

// Executions reads signatures in one JSON-RPC batch; results keep input order and a transaction the node lacks is nil.
func (c *Client) Executions(ctx context.Context, signatures []solana.Signature, commitment rpc.CommitmentType) ([]*Execution, error) {
	if len(signatures) == 0 {
		return nil, nil
	}
	opts := map[string]any{"encoding": solana.EncodingBase64, "commitment": commitment, "maxSupportedTransactionVersion": uint64(anyVersion)}
	requests := make(jsonrpc.RPCRequests, len(signatures))
	for i, signature := range signatures {
		requests[i] = jsonrpc.NewRequest("getTransaction", signature, opts)
	}
	responses, err := c.batch.CallBatch(ctx, requests)
	if err != nil {
		return nil, failed("getTransaction", err)
	}
	byID := make([]*jsonrpc.RPCResponse, len(signatures))
	for _, response := range responses {
		if response == nil {
			return nil, errors.New("getTransaction: invalid batch response")
		}
		id, ok := response.ID.(json.Number)
		index, err := id.Int64()
		if !ok || err != nil || index < 0 || int(index) >= len(byID) || byID[index] != nil {
			return nil, errors.New("getTransaction: invalid batch response")
		}
		byID[index] = response
	}
	executions := make([]*Execution, len(signatures))
	for i, response := range byID {
		if response == nil {
			return nil, errors.New("getTransaction: incomplete batch response")
		}
		if response.Error != nil {
			return nil, failed("getTransaction", response.Error)
		}
		var out *rpc.GetTransactionResult
		if err := response.GetObject(&out); err != nil {
			return nil, failed("getTransaction", err)
		}
		if out == nil {
			continue
		}
		execution, err := decodeExecution(out)
		if err != nil {
			return nil, err
		}
		executions[i] = &execution
	}
	return executions, nil
}

func decodeExecution(out *rpc.GetTransactionResult) (Execution, error) {
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
		ReturnProgram: meta.ReturnData.ProgramId, ReturnData: meta.ReturnData.Data.Content,
	}}
	if out.BlockTime != nil {
		at := out.BlockTime.Time().UTC()
		execution.BlockTime = &at
	}
	if execution.Pre, err = tokenBalances(keys, meta.PreTokenBalances); err != nil {
		return Execution{}, err
	}
	if execution.Post, err = tokenBalances(keys, meta.PostTokenBalances); err != nil {
		return Execution{}, err
	}
	return execution, nil
}
