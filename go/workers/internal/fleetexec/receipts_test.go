package fleetexec

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func uint64Ptr(v uint64) *uint64 { return &v }

// anchorReceiptFixture mirrors the same-mint route receipt: the exact
// withdraw-then-deposit movement of one liquidity amount between two anchored
// reserve vault accounts, plus the untouched vault token account.
func anchorReceiptFixture(t *testing.T) (*TransactionReceipt, SubmissionRecord, BalanceAnchorEvidence, []string) {
	t.Helper()
	fixture := mustSignedFixture(t)
	message, err := base64.StdEncoding.DecodeString(fixture.MessageB64)
	if err != nil {
		t.Fatal(err)
	}
	messageHash := hex.EncodeToString(func() []byte { sum := sha256.Sum256(message); return sum[:] }())
	addresses := []string{fixture.FeePayer, "source-liquidity-vault", "target-liquidity-vault"}
	anchors := BalanceAnchorEvidence{
		AccountAddresses: addresses,
		Anchors: []EffectAnchor{
			{Account: "source-liquidity-vault", Mint: "usdc", ExpectedDelta: 501_835_024, Decimals: 6},
			{Account: "target-liquidity-vault", Mint: "usdc", ExpectedDelta: -501_835_024, Decimals: 6},
		},
	}
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
	return bound, record, anchors, addresses
}

func TestReceiptIdentityRequiresExactPersistedAttempt(t *testing.T) {
	receipt, record, _, _ := anchorReceiptFixture(t)
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

func TestAnchoredEffectsRequireExactMovement(t *testing.T) {
	receipt, _, anchors, _ := anchorReceiptFixture(t)
	if err := VerifyAnchoredEffects(receipt, anchors); err != nil {
		t.Fatalf("exact movement rejected: %v", err)
	}
	wrongAmount := anchors
	wrongAmount.Anchors = append([]EffectAnchor(nil), anchors.Anchors...)
	wrongAmount.Anchors[0].ExpectedDelta = 501_835_023 // quote rounding, not proof
	if err := VerifyAnchoredEffects(receipt, wrongAmount); err == nil {
		t.Fatalf("an off-by-one movement must be rejected")
	}
	swapped := anchors
	swapped.Anchors = append([]EffectAnchor(nil), anchors.Anchors...)
	swapped.Anchors[0].ExpectedDelta, swapped.Anchors[1].ExpectedDelta =
		swapped.Anchors[1].ExpectedDelta, swapped.Anchors[0].ExpectedDelta
	if err := VerifyAnchoredEffects(receipt, swapped); err == nil {
		t.Fatalf("inverted direction must be rejected")
	}
	wrongMint := anchors
	wrongMint.Anchors = append([]EffectAnchor(nil), anchors.Anchors...)
	wrongMint.Anchors[0].Mint = "other-mint"
	if err := VerifyAnchoredEffects(receipt, wrongMint); err == nil {
		t.Fatalf("an anchor with no receipt evidence must be rejected")
	}
	wrongDecimals := anchors
	wrongDecimals.Anchors = append([]EffectAnchor(nil), anchors.Anchors...)
	wrongDecimals.Anchors[1].Decimals = 9
	if err := VerifyAnchoredEffects(receipt, wrongDecimals); err == nil {
		t.Fatalf("decimal drift must be rejected")
	}
}

func TestAnchoredEffectsNeverTreatUnknownAsZero(t *testing.T) {
	receipt, _, anchors, _ := anchorReceiptFixture(t)
	unknownPre := &TransactionReceipt{
		Slot: receipt.Slot, Signature: receipt.Signature, MessageB64: receipt.MessageB64,
		accountAddresses: append([]string(nil), anchors.AccountAddresses...),
		TokenDeltas: []TokenDelta{
			{Account: "source-liquidity-vault", Mint: "usdc", Decimals: 6, PostRaw: uint64Ptr(9_501_835_024)},
			{Account: "target-liquidity-vault", Mint: "usdc", Decimals: 6, PreRaw: uint64Ptr(12_000_000_000), PostRaw: uint64Ptr(11_498_164_976)},
		},
	}
	bound, err := unknownPre.WithAccountAddresses(anchors.AccountAddresses)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAnchoredEffects(bound, anchors); err == nil {
		t.Fatalf("unknown pre-balance must not count as a zero delta")
	}
	missing := &TransactionReceipt{
		Slot: receipt.Slot, Signature: receipt.Signature, MessageB64: receipt.MessageB64,
		accountAddresses: append([]string(nil), anchors.AccountAddresses...),
		TokenDeltas:      receipt.TokenDeltas[:1],
	}
	boundMissing, err := missing.WithAccountAddresses(anchors.AccountAddresses)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAnchoredEffects(boundMissing, anchors); err == nil {
		t.Fatalf("a missing anchored account must fail reconciliation")
	}
}

func TestReceiptDeltasResolveDurableAccountIndices(t *testing.T) {
	raw := json.RawMessage(`{"account_addresses":["a","b"],"anchors":[]}`)
	var evidence BalanceAnchorEvidence
	_ = json.Unmarshal(raw, &evidence)
	_, err := (&TransactionReceipt{}).WithAccountAddresses(nil)
	if err == nil {
		t.Fatalf("reconciliation without index evidence must be refused")
	}
}

func TestReconciledEffectRecordsVerifiedEvidence(t *testing.T) {
	receipt, _, _, _ := anchorReceiptFixture(t)
	effect := reconciledEffect(receipt)
	if len(effect) == 0 {
		t.Fatalf("reconciled effect must be recorded")
	}
	var decoded struct {
		Signature string       `json:"signature"`
		Slot      int64        `json:"slot"`
		Deltas    []TokenDelta `json:"token_deltas"`
	}
	if err := json.Unmarshal(effect, &decoded); err != nil {
		t.Fatalf("decode reconciled effect: %v", err)
	}
	if decoded.Signature != receipt.Signature || decoded.Slot != receipt.Slot || len(decoded.Deltas) != len(receipt.TokenDeltas) {
		t.Fatalf("reconciled effect lost verified evidence: %+v", decoded)
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
