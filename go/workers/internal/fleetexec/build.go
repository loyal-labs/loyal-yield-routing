package fleetexec

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/mr-tron/base58"
	solanasdk "github.com/solana-foundation/solana-go/v2"
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

// DelegateSigner holds the delegated policy signer, which also pays a route's
// fee unless the vault ranks a healthy fee-only registry shard first.
// FeeOnly holds the mounted fee-only shard keys; a fee-only payer signs only for
// the fee and holds no vault, policy or ALT authority.
type DelegateSigner struct {
	FeePayer ed25519.PrivateKey
	FeeOnly  []ed25519.PrivateKey
}

// signers returns the keys for a message's required signers, in order: the
// delegate alone, or a fee-only payer followed by the delegate.
func (s DelegateSigner) signers(keys []solanasdk.PublicKey, required, readonlySigned int) ([]ed25519.PrivateKey, error) {
	delegate := base58.Encode(s.FeePayer[32:])
	switch {
	case required == 1 && readonlySigned == 0 && keys[0].String() == delegate:
		return []ed25519.PrivateKey{s.FeePayer}, nil
	case required == 2 && readonlySigned == 1 && keys[1].String() == delegate:
		for _, key := range s.FeeOnly {
			if len(key) == ed25519.PrivateKeySize && base58.Encode(key[32:]) == keys[0].String() {
				return []ed25519.PrivateKey{key, s.FeePayer}, nil
			}
		}
	}
	return nil, fmt.Errorf("prepared route fee payer %s is not the delegated signer or a configured fee-only payer", keys[0])
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
	required := int(decoded.Message.Header.NumRequiredSignatures)
	if len(decoded.Signatures) != required || required < 1 || required > 2 {
		return WireIdentity{}, errors.New("delegated execution signs the fee payer and at most the delegate")
	}
	for _, signature := range decoded.Signatures {
		if !signature.IsZero() {
			return WireIdentity{}, errors.New("unsigned wire signature slot is not zeroed")
		}
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
	if int(decoded.Message.Header.NumReadonlySignedAccounts) >= required || int(decoded.Message.Header.NumReadonlyUnsignedAccounts) > len(decoded.Message.AccountKeys)-required {
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
	blockhash := decoded.Message.RecentBlockhash.String()
	keys, err := s.signers(decoded.Message.AccountKeys, required, int(decoded.Message.Header.NumReadonlySignedAccounts))
	if err != nil {
		return WireIdentity{}, err
	}
	if tx.FeeLamports == 0 || tx.ComputeLimit == 0 || tx.ComputeLimit > defaultComputeLimit {
		return WireIdentity{}, errors.New("prepared fee or compute limit is invalid")
	}
	if len(tx.WritableAccounts) == 0 {
		return WireIdentity{}, errors.New("prepared route lacks writable account evidence")
	}
	signed := []byte{byte(len(keys))}
	var first []byte
	for _, key := range keys {
		// The stored public half must be the public half of the stored seed; a
		// key assembled from divergent halves would sign as an unexpected
		// authority even though its own public half is what gets compared.
		if !bytes.Equal(ed25519.NewKeyFromSeed(key.Seed())[32:], key[32:]) {
			return WireIdentity{}, errors.New("signing key's public half does not match its seed")
		}
		signature := ed25519.Sign(key, message)
		if !ed25519.Verify(ed25519.PublicKey(key[32:]), message, signature) {
			return WireIdentity{}, errors.New("produced signature does not verify against the message")
		}
		if first == nil {
			first = signature
		}
		signed = append(signed, signature...)
	}
	signed = append(signed, tx.Message...)
	signedHash := sha256.Sum256(signed)
	return WireIdentity{
		SignedTransaction:     signed,
		SignedTransactionHash: hex.EncodeToString(signedHash[:]),
		MessageHash:           tx.MessageSHA256,
		TransactionSignature:  base58.Encode(first),
		RecentBlockhash:       blockhash,
		LastValidBlockHeight:  lastValidBlockHeight,
	}, nil
}
