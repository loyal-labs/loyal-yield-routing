package autodeposit

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

const solanaPacketBytes = 1232

// sdkInstruction is the one conversion from a route instruction to the
// solana-go instruction the transaction compiles.
func sdkInstruction(ix fleet.RouteInstruction) (solana.Instruction, error) {
	program, err := solana.PublicKeyFromBase58(ix.Program)
	if err != nil {
		return nil, fmt.Errorf("instruction program %q: %w", ix.Program, err)
	}
	metas := make(solana.AccountMetaSlice, len(ix.Accounts))
	for i, a := range ix.Accounts {
		key, err := solana.PublicKeyFromBase58(a.Address)
		if err != nil {
			return nil, fmt.Errorf("instruction account %q: %w", a.Address, err)
		}
		metas[i] = &solana.AccountMeta{PublicKey: key, IsSigner: a.Signer, IsWritable: a.Writable}
	}
	return solana.NewInstruction(program, metas, ix.Data), nil
}

// transferRecurring is the Subscriptions pull of amount from the wallet's
// token account to the vault's: tag, u64 amount, delegator, mint
// (crates/loyal-actions/src/protocols.rs).
func transferRecurring(amount uint64, wallet, vault, mint solana.PublicKey, delegation, walletATA, vaultATA string) (fleet.RouteInstruction, error) {
	authority, err := subscriptionAuthorityKey(wallet[:], mint[:])
	if err != nil {
		return fleet.RouteInstruction{}, err
	}
	event, err := subscriptionEventAuthorityKey()
	if err != nil {
		return fleet.RouteInstruction{}, err
	}
	data := make([]byte, 73)
	data[0] = subscriptionsTransferRecurring
	binary.LittleEndian.PutUint64(data[1:9], amount)
	copy(data[9:41], wallet[:])
	copy(data[41:], mint[:])
	return fleet.RouteInstruction{Step: "autodeposit", Program: SubscriptionsProgramID, Data: data, Accounts: []fleet.InstructionAccount{
		{Address: delegation, Writable: true}, {Address: base58Key(authority[:])},
		{Address: walletATA, Writable: true}, {Address: vaultATA, Writable: true},
		{Address: mint.String()}, {Address: splTokenID}, {Address: vault.String(), Signer: true},
		{Address: base58Key(event[:])}, {Address: SubscriptionsProgramID},
	}}, nil
}

// mustKey is reserved for pinned constants or caller-validated keys.
func mustKey(value string) solana.PublicKey {
	key, err := solana.PublicKeyFromBase58(value)
	if err != nil {
		panic(fmt.Sprintf("invalid pinned or validated public key: %s", value))
	}
	return key
}
func base58Key(raw []byte) string { return solana.PublicKeyFromBytes(raw).String() }
func findProgramAddress(seeds [][]byte, program string) ([32]byte, error) {
	key, err := solana.PublicKeyFromBase58(program)
	if err != nil {
		return [32]byte{}, err
	}
	derived, _, err := solana.FindProgramAddress(seeds, key)
	return derived, err
}

type decodedMessage struct {
	signature    string
	blockhash    string
	accounts     []solana.PublicKey
	instructions []decodedInstruction
}

type decodedInstruction struct {
	program  solana.PublicKey
	accounts []solana.PublicKey
	data     []byte
}

func persistedWireTransaction(attempt DurableAttempt) (*solana.Transaction, error) {
	wire, err := base64StdDecode(attempt.SignedTransactionBase64)
	if err != nil {
		return nil, err
	}
	if _, err = chain.OwnSignedWire(wire, attempt.SignedTransactionSHA256); err != nil {
		return nil, err
	}
	tx, err := solana.TransactionFromBytes(wire)
	if err != nil {
		return nil, err
	}
	if err = validateSignedMessageHeader(tx); err != nil {
		return nil, err
	}
	canonical, err := tx.MarshalBinary()
	if err != nil || !bytes.Equal(canonical, wire) {
		return nil, errors.New("persisted packet is not canonical")
	}
	if len(tx.Signatures) == 0 || tx.Signatures[0].String() != attempt.Signature || tx.Message.RecentBlockhash.String() != attempt.RecentBlockhash {
		return nil, errors.New("persisted signature or blockhash differs from exact packet")
	}
	if err = tx.VerifySignatures(); err != nil {
		return nil, err
	}
	return tx, nil
}

func validateSignedMessageHeader(tx *solana.Transaction) error {
	version := tx.Message.GetVersion()
	header := tx.Message.Header
	required := int(header.NumRequiredSignatures)
	keys := len(tx.Message.AccountKeys)
	if version != solana.MessageVersionLegacy && version != solana.MessageVersionV0 || required == 0 || required != len(tx.Signatures) || required > keys || int(header.NumReadonlySignedAccounts) >= required || int(header.NumReadonlyUnsignedAccounts) > keys-required {
		return errors.New("signed packet header violates signer/fee payer ownership")
	}
	seen := map[solana.PublicKey]bool{}
	for _, key := range tx.Message.AccountKeys {
		if seen[key] {
			return errors.New("signed packet repeats static account key")
		}
		seen[key] = true
	}
	return nil
}

func decodeSignedWireMessageTransaction(tx *solana.Transaction) (decodedMessage, error) {
	var out decodedMessage
	if len(tx.Message.GetAddressTableLookups()) > 0 && !tx.Message.IsResolved() {
		return out, errors.New("v0 packet requires verified address table resolution")
	}
	out.signature = tx.Signatures[0].String()
	out.blockhash = tx.Message.RecentBlockhash.String()
	out.accounts = tx.Message.AccountKeys
	for _, ix := range tx.Message.Instructions {
		if int(ix.ProgramIDIndex) >= len(out.accounts) {
			return out, errors.New("instruction program index out of range")
		}
		instruction := decodedInstruction{program: out.accounts[ix.ProgramIDIndex], data: append([]byte(nil), ix.Data...)}
		for _, index := range ix.Accounts {
			if int(index) >= len(out.accounts) {
				return out, errors.New("instruction account index out of range")
			}
			instruction.accounts = append(instruction.accounts, out.accounts[index])
		}
		out.instructions = append(out.instructions, instruction)
	}
	return out, nil
}
func mustSHA256(raw []byte) []byte { sum := sha256.Sum256(raw); return sum[:] }
func keyEqual(key solana.PublicKey, address string) bool {
	expected, err := solana.PublicKeyFromBase58(address)
	return err == nil && key == expected
}
