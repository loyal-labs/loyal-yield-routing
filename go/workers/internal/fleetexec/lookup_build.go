package fleetexec

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	sdk "github.com/solana-foundation/solana-go/v2"
)

const lookupProgram = "AddressLookupTab1e1111111111111111111111111"
const lookupMaximumChunk = 20

// Official ABI reference: locked solana-address-lookup-table-interface 2.2.2,
// instruction.rs ProgramInstruction bincode enum and its four public builders.
// The independent Rust harness compares these bytes and account vectors and
// executes Go packets through the actual ALT program. No Rust writer is needed.
func lookupInstructions(intent LookupIntent) ([]sdk.Instruction, error) {
	if intent.Cluster == "" || intent.OperationID <= 0 || intent.FamilyID <= 0 || intent.TableID <= 0 || intent.Generation < 0 || intent.MutationEpoch < 0 || intent.Payer != intent.Authority || intent.TableAddress == intent.Authority {
		return nil, errors.New("lookup-table immutable ownership is incomplete")
	}
	keys := make(map[string]sdk.PublicKey)
	for _, address := range append([]string{intent.TableAddress, intent.Authority, intent.Payer}, append(append([]string{}, intent.Prefix...), intent.Extension...)...) {
		key, err := sdk.PublicKeyFromBase58(address)
		if err != nil {
			return nil, errors.New("lookup-table intent contains an invalid public key")
		}
		keys[address] = key
	}
	if len(intent.Extension) > lookupMaximumChunk || len(intent.Prefix)+len(intent.Extension) > 256 {
		return nil, errors.New("lookup-table extension exceeds chunk or capacity")
	}
	seen := make(map[string]bool)
	for _, address := range append(append([]string{}, intent.Prefix...), intent.Extension...) {
		if seen[address] {
			return nil, errors.New("lookup-table prefix/suffix contains duplicate membership")
		}
		seen[address] = true
	}
	if (intent.Kind == LookupCreate || intent.Kind == LookupRollover) != (intent.RecentSlot != nil) {
		return nil, errors.New("lookup-table recent slot does not match create intent")
	}
	if intent.Kind != LookupClose && (intent.Recipient != "" || intent.ExpectedDeactivationSlot != nil) {
		return nil, errors.New("lookup-table non-close intent contains cleanup refund state")
	}
	table, authority, payer := keys[intent.TableAddress], keys[intent.Authority], keys[intent.Payer]
	program := sdk.MustPublicKeyFromBase58(lookupProgram)
	makeInstruction := func(tag uint32, data []byte, accounts sdk.AccountMetaSlice) sdk.Instruction {
		prefix := make([]byte, 4)
		binary.LittleEndian.PutUint32(prefix, tag)
		return sdk.NewInstruction(program, accounts, append(prefix, data...))
	}
	extend := func() sdk.Instruction {
		data := make([]byte, 8)
		binary.LittleEndian.PutUint64(data, uint64(len(intent.Extension)))
		for _, address := range intent.Extension {
			key := keys[address]
			data = append(data, key[:]...)
		}
		return makeInstruction(2, data, sdk.AccountMetaSlice{sdk.Meta(table).WRITE(), sdk.Meta(authority).SIGNER(), sdk.Meta(payer).WRITE().SIGNER(), sdk.Meta(sdk.SystemProgramID)})
	}
	switch intent.Kind {
	case LookupCreate, LookupRollover:
		if len(intent.Prefix) != 0 {
			return nil, errors.New("lookup-table create cannot replace a populated prefix")
		}
		var recent [8]byte
		binary.LittleEndian.PutUint64(recent[:], *intent.RecentSlot)
		derived, bump, err := sdk.FindProgramAddress([][]byte{authority[:], recent[:]}, program)
		if err != nil || derived != table {
			return nil, errors.New("lookup-table create address differs from its reserved PDA")
		}
		ix := makeInstruction(0, append(recent[:], bump), sdk.AccountMetaSlice{sdk.Meta(table).WRITE(), sdk.Meta(authority), sdk.Meta(payer).WRITE().SIGNER(), sdk.Meta(sdk.SystemProgramID)})
		out := []sdk.Instruction{ix}
		if len(intent.Extension) > 0 {
			out = append(out, extend())
		}
		return out, nil
	case LookupExtend:
		if len(intent.Extension) == 0 {
			return nil, errors.New("lookup-table extend has no genuinely missing suffix")
		}
		return []sdk.Instruction{extend()}, nil
	case LookupDeactivate:
		if len(intent.Extension) != 0 {
			return nil, errors.New("lookup-table deactivation cannot extend membership")
		}
		return []sdk.Instruction{makeInstruction(3, nil, sdk.AccountMetaSlice{sdk.Meta(table).WRITE(), sdk.Meta(authority).SIGNER()})}, nil
	case LookupClose:
		if len(intent.Extension) != 0 || intent.Recipient != intent.Authority || intent.ExpectedDeactivationSlot == nil || *intent.ExpectedDeactivationSlot == ^uint64(0) {
			return nil, errors.New("lookup-table close lacks frozen cooldown or standard manager refund")
		}
		return []sdk.Instruction{makeInstruction(4, nil, sdk.AccountMetaSlice{sdk.Meta(table).WRITE(), sdk.Meta(authority).SIGNER(), sdk.Meta(authority).WRITE()})}, nil
	default:
		return nil, errors.New("lookup-table verification never builds a mutation")
	}
}

func buildLookupUnsigned(intent LookupIntent, blockhash string, height int64) ([]byte, []byte, error) {
	if height <= 0 {
		return nil, nil, errors.New("lookup-table blockhash expiry is unknown")
	}
	instructions, err := lookupInstructions(intent)
	if err != nil {
		return nil, nil, err
	}
	hash, err := sdk.HashFromBase58(blockhash)
	if err != nil {
		return nil, nil, errors.New("lookup-table recent blockhash is invalid")
	}
	tx, err := sdk.NewTransaction(instructions, hash, sdk.TransactionPayer(sdk.MustPublicKeyFromBase58(intent.Payer)))
	if err != nil {
		return nil, nil, err
	}
	if tx.Message.Header.NumRequiredSignatures != 1 || tx.Message.Header.NumReadonlySignedAccounts != 0 {
		return nil, nil, errors.New("lookup-table manager must be the sole writable signer")
	}
	tx.Signatures = []sdk.Signature{{}}
	wire, err := tx.MarshalBinary()
	if err != nil {
		return nil, nil, err
	}
	if len(wire) > SolanaPacketLimit {
		return nil, nil, errors.New("lookup-table mutation exceeds Solana packet limit")
	}
	message, err := tx.Message.MarshalBinary()
	return wire, message, err
}

func signLookupMutation(intent LookupIntent, blockhash string, height int64, key ed25519.PrivateKey) (WireIdentity, error) {
	if len(key) != ed25519.PrivateKeySize || !ed25519.NewKeyFromSeed(key[:ed25519.SeedSize]).Equal(key) || sdk.PublicKey(key.Public().(ed25519.PublicKey)).String() != intent.Payer {
		return WireIdentity{}, errors.New("lookup-table signer differs from the standard family manager")
	}
	wire, _, err := buildLookupUnsigned(intent, blockhash, height)
	if err != nil {
		return WireIdentity{}, err
	}
	tx, err := sdk.TransactionFromBytes(wire)
	if err != nil {
		return WireIdentity{}, err
	}
	private := sdk.PrivateKey(key)
	if _, err = tx.Sign(func(public sdk.PublicKey) *sdk.PrivateKey {
		if public == private.PublicKey() {
			return &private
		}
		return nil
	}); err != nil {
		return WireIdentity{}, err
	}
	wire, err = tx.MarshalBinary()
	if err != nil {
		return WireIdentity{}, err
	}
	message, err := tx.Message.MarshalBinary()
	if err != nil {
		return WireIdentity{}, err
	}
	wh, mh := sha256.Sum256(wire), sha256.Sum256(message)
	out := WireIdentity{SignedTransaction: wire, SignedTransactionHash: hex.EncodeToString(wh[:]), MessageHash: hex.EncodeToString(mh[:]), TransactionSignature: tx.Signatures[0].String(), RecentBlockhash: blockhash, LastValidBlockHeight: height}
	return out, proveLookupWire(intent, out)
}

func proveLookupWire(intent LookupIntent, wire WireIdentity) error {
	if _, err := chain.OwnSignedWire(wire.SignedTransaction, wire.SignedTransactionHash); err != nil {
		return err
	}
	tx, err := sdk.TransactionFromBytes(wire.SignedTransaction)
	if err != nil {
		return fmt.Errorf("lookup-table signed packet decode: %w", err)
	}
	canonical, err := tx.MarshalBinary()
	if err != nil || !bytes.Equal(canonical, wire.SignedTransaction) {
		return errors.New("lookup-table packet is not canonical")
	}
	_, expected, err := buildLookupUnsigned(intent, wire.RecentBlockhash, wire.LastValidBlockHeight)
	if err != nil {
		return err
	}
	message, err := tx.Message.MarshalBinary()
	if err != nil || !bytes.Equal(message, expected) {
		return errors.New("lookup-table packet differs from the exact frozen intent")
	}
	mh := sha256.Sum256(message)
	if wire.MessageHash != hex.EncodeToString(mh[:]) || len(tx.Signatures) != 1 || tx.Signatures[0].String() != wire.TransactionSignature || !ed25519.Verify(ed25519.PublicKey(tx.Message.AccountKeys[0][:]), message, tx.Signatures[0][:]) {
		return errors.New("lookup-table immutable signature/message binding is invalid")
	}
	return nil
}

func lookupOrderedAddressHash(addresses []string) string {
	h := sha256.New()
	var size [8]byte
	for _, address := range addresses {
		binary.LittleEndian.PutUint64(size[:], uint64(len(address)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(address))
	}
	return hex.EncodeToString(h.Sum(nil))
}
