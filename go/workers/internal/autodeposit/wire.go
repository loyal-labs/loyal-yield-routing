package autodeposit

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/solana-foundation/solana-go/v2"
)

const solanaPacketBytes = 1232

type accountMeta struct {
	key              solana.PublicKey
	signer, writable bool
}
type compiledInstruction struct {
	program  solana.PublicKey
	accounts []accountMeta
	data     []byte
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
