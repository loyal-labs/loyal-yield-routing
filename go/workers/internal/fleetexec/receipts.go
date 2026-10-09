package fleetexec

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// VerifyReceiptIdentity proves the chain receipt is exactly the durably
// persisted attempt: same message bytes, same leading signature, same slot,
// and a successful on-chain result. Anything else cannot confirm the route.
func VerifyReceiptIdentity(receipt *TransactionReceipt, record SubmissionRecord, confirmedSlot int64) error {
	if receipt == nil {
		return fmt.Errorf("receipt is missing")
	}
	if receipt.Signature != record.Signature {
		return fmt.Errorf("receipt signature %q differs from persisted identity", receipt.Signature)
	}
	if receipt.Slot != confirmedSlot {
		return fmt.Errorf("receipt slot %d differs from confirmed slot %d", receipt.Slot, confirmedSlot)
	}
	if receipt.Err != "" {
		return fmt.Errorf("receipt reports chain failure %s", receipt.Err)
	}
	message, err := base64.StdEncoding.DecodeString(receipt.MessageB64)
	if err != nil {
		return fmt.Errorf("decode receipt message: %w", err)
	}
	if len(record.SignedTransaction) == 0 || !bytes.Equal(receipt.SignedTransaction, record.SignedTransaction) {
		return fmt.Errorf("receipt transaction bytes differ from the persisted signed wire")
	}
	messageHash := sha256.Sum256(message)
	if hex.EncodeToString(messageHash[:]) != record.MessageHash {
		return fmt.Errorf("receipt message hash differs from the persisted signed wire")
	}
	return nil
}
