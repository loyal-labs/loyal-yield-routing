package chain

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/mr-tron/base58"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// A node refuses a transaction newer than the version a reader accepts; an
// execution read takes every version and keeps the inner instructions.
func TestExecutionReadsNewerVersionsWithInnerInstructions(t *testing.T) {
	payer, loaded := solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey()
	tx, err := solana.NewTransaction([]solana.Instruction{solana.NewInstruction(solana.SystemProgramID, solana.AccountMetaSlice{solana.Meta(payer).SIGNER().WRITE()}, nil)}, solana.Hash{1}, solana.TransactionPayer(payer))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	var found any = map[string]any{
		"slot": 900, "version": 1,
		"transaction": []string{base64.StdEncoding.EncodeToString(wire), "base64"},
		"meta": map[string]any{
			"err": nil, "fee": 5000, "preBalances": []uint64{10, 0, 1}, "postBalances": []uint64{5, 0, 1},
			"innerInstructions": []any{map[string]any{"index": 0, "instructions": []any{map[string]any{"programIdIndex": 1, "accounts": []int{0, 2}, "data": base58.Encode([]byte{7, 8})}}}},
			"loadedAddresses":   map[string]any{"writable": []string{loaded.String()}, "readonly": []string{}},
		},
	}
	client := serve(t, func(req request) (int, any) {
		var opts struct {
			MaxSupportedTransactionVersion uint64 `json:"maxSupportedTransactionVersion"`
		}
		_ = json.Unmarshal(req.Params[1], &opts)
		if opts.MaxSupportedTransactionVersion < 1 {
			return http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": 0, "error": map[string]any{"code": -32015, "message": "Transaction version (1) is not supported by the requesting client"}}
		}
		return http.StatusOK, result(found)
	})
	read, err := client.Execution(context.Background(), solana.Signature{1}, rpc.CommitmentConfirmed)
	if err != nil {
		t.Fatal(err)
	}
	if read.Slot != 900 || read.Fee != 5000 || len(read.Keys) != 3 || read.Keys[2] != loaded || read.Transaction.Message.AccountKeys[0] != payer {
		t.Fatalf("execution %+v", read.Receipt)
	}
	inner := read.Inner[0].Instructions[0]
	if read.Inner[0].Index != 0 || read.Keys[inner.ProgramIDIndex] != solana.SystemProgramID || string(inner.Data) != string([]byte{7, 8}) {
		t.Fatalf("inner instructions %+v", read.Inner)
	}
	found = nil
	if _, err := client.Execution(context.Background(), solana.Signature{1}, rpc.CommitmentConfirmed); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown signature = %v, want ErrNotFound", err)
	}
}
