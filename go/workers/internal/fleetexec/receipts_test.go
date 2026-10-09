package fleetexec

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// anchorReceiptFixture is the finalized receipt of one persisted same-mint
// attempt and that attempt's durable record.
func anchorReceiptFixture(t *testing.T) (chain.Receipt, SubmissionRecord) {
	t.Helper()
	fixture := mustSignedFixture(t)
	message, err := base64.StdEncoding.DecodeString(fixture.MessageB64)
	if err != nil {
		t.Fatal(err)
	}
	messageHash := sha256.Sum256(message)
	record := SubmissionRecord{
		ID:                41,
		Signature:         fixture.SignatureB58,
		MessageHash:       hex.EncodeToString(messageHash[:]),
		ConfirmedSlot:     int64Ptr(443_023_824),
		SignedTransaction: mustDecodeB64(t, fixture.SignedWireB64),
	}
	return chain.Receipt{Slot: 443_023_824, Wire: mustDecodeB64(t, fixture.SignedWireB64)}, record
}

func TestReceiptIdentityRequiresExactPersistedAttempt(t *testing.T) {
	receipt, record := anchorReceiptFixture(t)
	if err := VerifyReceiptIdentity(receipt, record, 443_023_824); err != nil {
		t.Fatalf("exact receipt rejected: %v", err)
	}
	other := record
	other.Signature = testSignature("another attempt").String()
	if err := VerifyReceiptIdentity(receipt, other, 443_023_824); err == nil {
		t.Fatalf("receipt with a different signature must be rejected")
	}
	slot := receipt
	slot.Slot = 443_023_823
	if err := VerifyReceiptIdentity(slot, record, 443_023_824); err == nil {
		t.Fatalf("receipt from a different slot must be rejected")
	}
	failed := receipt
	failed.Err = map[string]any{"InstructionError": []any{0, map[string]any{"Custom": 1}}}
	if err := VerifyReceiptIdentity(failed, record, 443_023_824); err == nil {
		t.Fatalf("failed receipt must be rejected")
	}
	tampered := receipt
	tampered.Wire = append([]byte(nil), receipt.Wire...)
	tampered.Wire[len(tampered.Wire)-1] ^= 1
	if err := VerifyReceiptIdentity(tampered, record, 443_023_824); err == nil {
		t.Fatalf("receipt with different transaction bytes must be rejected")
	}
	message := record
	message.MessageHash = strings.Repeat("0", 64)
	if err := VerifyReceiptIdentity(receipt, message, 443_023_824); err == nil {
		t.Fatalf("receipt with a different message must be rejected")
	}
}

func mustDecodeB64(t *testing.T, raw string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
