package fleetexec

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	sdk "github.com/solana-foundation/solana-go/v2"
)

// feeOnlyPrepared compiles a v0 message the way the revalidator does for a
// fee-only payer: the payer first and writable, the delegate a read-only signer.
func feeOnlyPrepared(t *testing.T, payer, delegate ed25519.PrivateKey) fleet.PreparedTransaction {
	t.Helper()
	ix := sdk.NewInstruction(sdk.SystemProgramID, sdk.AccountMetaSlice{sdk.Meta(sdk.PublicKeyFromBytes(delegate[32:])).SIGNER()}, []byte{9})
	tx, err := sdk.NewTransaction([]sdk.Instruction{ix}, sdk.Hash{7}, sdk.TransactionPayer(sdk.PublicKeyFromBytes(payer[32:])))
	if err != nil {
		t.Fatal(err)
	}
	tx.Message.SetVersion(sdk.MessageVersionV0)
	wire, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	message, err := tx.Message.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	mh, wh := sha256.Sum256(message), sha256.Sum256(wire)
	return fleet.PreparedTransaction{Message: message, UnsignedWire: wire, MessageSHA256: hex.EncodeToString(mh[:]), WireSHA256: hex.EncodeToString(wh[:]),
		PacketBytes: len(wire), WritableAccounts: []string{sdk.PublicKeyFromBytes(payer[32:]).String()}, FeeLamports: 10_000, ComputeLimit: 200_000}
}

func TestFeeOnlyPayerSignsFirstAndDelegateSecond(t *testing.T) {
	delegate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, 32))
	payer := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{4}, 32))
	prepared := feeOnlyPrepared(t, payer, delegate)
	if _, err := (DelegateSigner{FeePayer: delegate}).SignPreparedRoute(prepared, 900); err == nil {
		t.Fatal("a fee-only payer the signer does not hold was accepted")
	}
	wire, err := (DelegateSigner{FeePayer: delegate, FeeOnly: []ed25519.PrivateKey{payer}}).SignPreparedRoute(prepared, 900)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := sdk.TransactionFromBytes(wire.SignedTransaction)
	if err != nil || len(tx.Signatures) != 2 || tx.VerifySignatures() != nil {
		t.Fatalf("two-signer wire invalid: %v", err)
	}
	if tx.Signatures[0].String() != wire.TransactionSignature || !ed25519.Verify(ed25519.PublicKey(payer[32:]), prepared.Message, tx.Signatures[0][:]) {
		t.Fatal("the transaction identity must be the fee payer's signature")
	}
}
