package fleetexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	sdk "github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// receiptReader reads a landed transaction's receipt.
type receiptReader interface {
	Receipt(ctx context.Context, signature sdk.Signature, commitment rpc.CommitmentType) (chain.Receipt, error)
}

// finalizedReceipt is signature's finalized receipt. chain.ErrNotFound means
// finality has not reached it yet.
func finalizedReceipt(ctx context.Context, receipts receiptReader, signature string) (chain.Receipt, error) {
	sig, err := sdk.SignatureFromBase58(signature)
	if err != nil {
		return chain.Receipt{}, fmt.Errorf("receipt signature: %w", err)
	}
	return receipts.Receipt(ctx, sig, rpc.CommitmentFinalized)
}

// VerifyReceiptIdentity proves the chain receipt is exactly the durably
// persisted attempt: same signed bytes, leading signature and message, same
// slot, and a successful on-chain result. Anything else cannot confirm the
// route.
func VerifyReceiptIdentity(receipt chain.Receipt, record SubmissionRecord, confirmedSlot int64) error {
	if int64(receipt.Slot) != confirmedSlot {
		return fmt.Errorf("receipt slot %d differs from confirmed slot %d", receipt.Slot, confirmedSlot)
	}
	if receipt.Err != nil {
		text, _ := json.Marshal(receipt.Err)
		return fmt.Errorf("receipt reports chain failure %s", text)
	}
	if len(record.SignedTransaction) == 0 || !bytes.Equal(receipt.Wire, record.SignedTransaction) {
		return fmt.Errorf("receipt transaction bytes differ from the persisted signed wire")
	}
	tx, err := sdk.TransactionFromBytes(receipt.Wire)
	if err != nil {
		return fmt.Errorf("decode receipt wire: %w", err)
	}
	if len(tx.Signatures) == 0 || tx.Signatures[0].String() != record.Signature {
		return fmt.Errorf("receipt signature differs from persisted identity %q", record.Signature)
	}
	message, err := tx.Message.MarshalBinary()
	if err != nil {
		return fmt.Errorf("encode receipt message: %w", err)
	}
	messageHash := sha256.Sum256(message)
	if hex.EncodeToString(messageHash[:]) != record.MessageHash {
		return fmt.Errorf("receipt message hash differs from the persisted signed wire")
	}
	return nil
}
