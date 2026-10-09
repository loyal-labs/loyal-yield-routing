package fleetexec

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func uint64Ptr(v uint64) *uint64 { return &v }

// anchorReceiptFixture mirrors the same-mint route receipt: the exact
// withdraw-then-deposit movement of one liquidity amount between two reserve
// vault accounts, plus the untouched vault token account.
func anchorReceiptFixture(t *testing.T) (*TransactionReceipt, SubmissionRecord) {
	t.Helper()
	fixture := mustSignedFixture(t)
	message, err := base64.StdEncoding.DecodeString(fixture.MessageB64)
	if err != nil {
		t.Fatal(err)
	}
	messageHash := hex.EncodeToString(func() []byte { sum := sha256.Sum256(message); return sum[:] }())
	addresses := []string{fixture.FeePayer, "source-liquidity-vault", "target-liquidity-vault"}
	record := SubmissionRecord{
		ID:                41,
		Signature:         fixture.SignatureB58,
		MessageHash:       messageHash,
		ConfirmedSlot:     int64Ptr(443_023_824),
		SignedTransaction: mustDecodeB64(t, fixture.SignedWireB64),
	}
	receipt := &TransactionReceipt{
		Slot:              443_023_824,
		accountAddresses:  append([]string(nil), addresses...),
		Signature:         fixture.SignatureB58,
		MessageB64:        fixture.MessageB64,
		SignedTransaction: mustDecodeB64(t, fixture.SignedWireB64),
		TokenDeltas: []TokenDelta{
			{Account: "vault-token-account", Mint: "usdc", Decimals: 6, PreRaw: uint64Ptr(1_000), PostRaw: uint64Ptr(1_000)},
			{Account: "source-liquidity-vault", Mint: "usdc", Decimals: 6, PreRaw: uint64Ptr(9_000_000_000), PostRaw: uint64Ptr(9_501_835_024)},
			{Account: "target-liquidity-vault", Mint: "usdc", Decimals: 6, PreRaw: uint64Ptr(12_000_000_000), PostRaw: uint64Ptr(11_498_164_976)},
		},
	}
	bound, err := receipt.WithAccountAddresses(addresses)
	if err != nil {
		t.Fatal(err)
	}
	return bound, record
}

func TestReceiptIdentityRequiresExactPersistedAttempt(t *testing.T) {
	receipt, record := anchorReceiptFixture(t)
	if err := VerifyReceiptIdentity(receipt, record, 443_023_824); err != nil {
		t.Fatalf("exact receipt rejected: %v", err)
	}
	mutated := *receipt
	mutated.Signature = "DifferentSignatureButAlsoBase58"
	if err := VerifyReceiptIdentity(&mutated, record, 443_023_824); err == nil {
		t.Fatalf("receipt with a different signature must be rejected")
	}
	slot := *receipt
	slot.Slot = 443_023_823
	if err := VerifyReceiptIdentity(&slot, record, 443_023_824); err == nil {
		t.Fatalf("receipt from a different slot must be rejected")
	}
	failed := *receipt
	failed.Err = `{"InstructionError":{"0":"Custom","1":1}}`
	if err := VerifyReceiptIdentity(&failed, record, 443_023_824); err == nil {
		t.Fatalf("failed receipt must be rejected")
	}
	tampered := *receipt
	tampered.MessageB64 = base64.StdEncoding.EncodeToString(append([]byte{0x80, 1, 0, 1}, make([]byte, 120)...))
	if err := VerifyReceiptIdentity(&tampered, record, 443_023_824); err == nil {
		t.Fatalf("receipt with different message bytes must be rejected")
	}
	if err := VerifyReceiptIdentity(nil, record, 443_023_824); err == nil {
		t.Fatalf("missing receipt must be rejected")
	}
}

func TestReceiptDeltasResolveDurableAccountIndices(t *testing.T) {
	_, err := (&TransactionReceipt{}).WithAccountAddresses(nil)
	if err == nil {
		t.Fatalf("reconciliation without index evidence must be refused")
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
