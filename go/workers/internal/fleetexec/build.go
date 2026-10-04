package fleetexec

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	solanasdk "github.com/gagliardetto/solana-go"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/mr-tron/base58"
)

// SolanaPacketLimit is the network MTU for a versioned transaction.
const SolanaPacketLimit = fleet.SolanaPacketLimit

// defaultComputeLimit mirrors the fleet planner's own compile bound.
const defaultComputeLimit = uint64(1_400_000)

// WireIdentity is the exact signed transaction evidence durably persisted
// before any broadcast. The bytes are immutable once written; recovery can
// only reuse them for their protocol-specific proof, never rebuild them.
type WireIdentity struct {
	SignedTransaction     []byte
	SignedTransactionHash string
	MessageHash           string
	TransactionSignature  string
	RecentBlockhash       string
	LastValidBlockHeight  int64
}

// DelegateSigner is the explicit delegated fee-payer key dependency. The
// Squads policy wraps every value-moving instruction as a virtual signer, so
// the delegated fee payer is the only required offline signature.
type DelegateSigner struct {
	FeePayer ed25519.PrivateKey
}

// SignPreparedRoute turns a fleet-compiled unsigned v0 wire into exact signed
// bytes without altering the message. It re-proves message identity, packet
// bounds, required-signer authority and the recent blockhash against the
// preparation before signing, so the persisted wire is exactly what the
// planner simulated.
func (s DelegateSigner) SignPreparedRoute(tx fleet.PreparedTransaction, lastValidBlockHeight int64) (WireIdentity, error) {
	if len(s.FeePayer) != ed25519.PrivateKeySize {
		return WireIdentity{}, errors.New("missing delegated fee-payer signing key")
	}
	if lastValidBlockHeight <= 0 {
		return WireIdentity{}, errors.New("missing last valid block height")
	}
	if tx.PacketBytes <= 0 || tx.PacketBytes > SolanaPacketLimit || len(tx.UnsignedWire) != tx.PacketBytes {
		return WireIdentity{}, fmt.Errorf("prepared packet %d exceeds %d bytes", tx.PacketBytes, SolanaPacketLimit)
	}
	unsignedHash := sha256.Sum256(tx.UnsignedWire)
	if hex.EncodeToString(unsignedHash[:]) != tx.WireSHA256 {
		return WireIdentity{}, errors.New("unsigned wire hash does not match the prepared evidence")
	}
	messageHash := sha256.Sum256(tx.Message)
	if hex.EncodeToString(messageHash[:]) != tx.MessageSHA256 {
		return WireIdentity{}, errors.New("prepared message hash does not match the prepared evidence")
	}
	decoded, err := solanasdk.TransactionFromBytes(tx.UnsignedWire)
	if err != nil {
		return WireIdentity{}, fmt.Errorf("decode prepared transaction: %w", err)
	}
	if decoded.Message.GetVersion() != solanasdk.MessageVersionV0 {
		return WireIdentity{}, errors.New("prepared message is not a v0 message")
	}
	if len(decoded.Signatures) != 1 || decoded.Message.Header.NumRequiredSignatures != 1 {
		return WireIdentity{}, errors.New("delegated execution signs exactly the fee payer")
	}
	if !decoded.Signatures[0].IsZero() {
		return WireIdentity{}, errors.New("unsigned wire signature slot is not zeroed")
	}
	canonical, err := decoded.MarshalBinary()
	if err != nil || !bytes.Equal(canonical, tx.UnsignedWire) {
		return WireIdentity{}, errors.New("prepared transaction is not canonical")
	}
	message, err := decoded.Message.MarshalBinary()
	if err != nil || !bytes.Equal(message, tx.Message) {
		return WireIdentity{}, errors.New("unsigned wire message differs from the prepared message")
	}
	if len(decoded.Message.AccountKeys) == 0 {
		return WireIdentity{}, errors.New("message declares no static account keys")
	}
	if decoded.Message.Header.NumReadonlySignedAccounts != 0 || int(decoded.Message.Header.NumReadonlyUnsignedAccounts) > len(decoded.Message.AccountKeys)-1 {
		return WireIdentity{}, errors.New("prepared header does not bind a writable fee payer or valid static key roles")
	}
	accountCount := len(decoded.Message.AccountKeys)
	for _, lookup := range decoded.Message.AddressTableLookups {
		accountCount += len(lookup.WritableIndexes) + len(lookup.ReadonlyIndexes)
	}
	if accountCount > 256 {
		return WireIdentity{}, errors.New("prepared message exceeds the account index space")
	}
	for _, instruction := range decoded.Message.Instructions {
		if int(instruction.ProgramIDIndex) >= accountCount {
			return WireIdentity{}, errors.New("prepared instruction program index is outside message accounts")
		}
		for _, index := range instruction.Accounts {
			if int(index) >= accountCount {
				return WireIdentity{}, errors.New("prepared instruction account index is outside message accounts")
			}
		}
	}
	payer := decoded.Message.AccountKeys[0].String()
	blockhash := decoded.Message.RecentBlockhash.String()
	expectedPayer := ed25519.PublicKey(s.FeePayer[32:])
	if payer != base58.Encode(expectedPayer) {
		return WireIdentity{}, fmt.Errorf("prepared route fee payer %s is not the delegated signer", payer)
	}
	if tx.FeeLamports == 0 || tx.ComputeLimit == 0 || tx.ComputeLimit > defaultComputeLimit {
		return WireIdentity{}, errors.New("prepared fee or compute limit is invalid")
	}
	if len(tx.WritableAccounts) == 0 {
		return WireIdentity{}, errors.New("prepared route lacks writable account evidence")
	}
	// The stored public half must be the public half of the stored seed; a
	// key assembled from divergent halves would sign as an unexpected
	// authority even though its own public half is what gets compared.
	if !bytes.Equal(ed25519.NewKeyFromSeed(s.FeePayer.Seed())[32:], s.FeePayer[32:]) {
		return WireIdentity{}, errors.New("delegated signing key's public half does not match its seed")
	}

	signature := ed25519.Sign(s.FeePayer, message)
	if !ed25519.Verify(ed25519.PublicKey(s.FeePayer[32:]), message, signature) {
		return WireIdentity{}, errors.New("produced signature does not verify against the message")
	}
	signed := append([]byte{0x01}, signature...)
	signed = append(signed, tx.Message...)
	signedHash := sha256.Sum256(signed)
	return WireIdentity{
		SignedTransaction:     signed,
		SignedTransactionHash: hex.EncodeToString(signedHash[:]),
		MessageHash:           tx.MessageSHA256,
		TransactionSignature:  base58.Encode(signature),
		RecentBlockhash:       blockhash,
		LastValidBlockHeight:  lastValidBlockHeight,
	}, nil
}
