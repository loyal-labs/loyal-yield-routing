package backyard

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/solana-foundation/solana-go/v2"
)

// Every Backyard transaction is one shape: a v1 message (SIMD-0385) with the
// pinned delegate as its one signer at key 0, every key static (v1 has no
// lookup tables), the compute budget in the message config, and the 64-byte
// signature after the message.
const (
	// maxTransactionBytes is the cluster's v1 size cap, signature included.
	maxTransactionBytes = solana.MaxTransactionSizeV1
	// maxTransactionKeys is the most addresses a v1 message carries.
	maxTransactionKeys = solana.MaxAddressesV1
	// maxComputeUnits is the runtime's per-transaction compute ceiling.
	maxComputeUnits = 1_400_000
	// instructionComputeUnits is what a legacy transaction's default meter
	// gave each instruction; a v1 message has no default (unset is zero).
	instructionComputeUnits = 200_000
	// loadedAccountsDataBytes is the runtime's 64 MiB loaded-data ceiling:
	// what a transaction loads is mostly program code, which changes with
	// every upgrade, so a measured limit would fail after one.
	loadedAccountsDataBytes = 64 << 20
	// squadsExecutionHeapBytes is the heap the deployed Squads ELF needs to
	// execute a policy: what it allocates grows with the policy and with the
	// inner instruction's accounts, so the runtime's default 32 KiB fits some
	// transactions and not others. It failed the AUTO lanes' eight-constraint
	// KLend policy and the Hastra policy in LiteSVM, and on mainnet the
	// looping vault's first Maple entry swap through SwapRoutesA (2026-10-14:
	// Squads wrote 2,992 bytes at heap offset 31,188 after Jupiter returned,
	// 34,180 > 32,768). 64 KiB is the tested value.
	squadsExecutionHeapBytes = 64 * 1024
	// autoSwapComputeUnits: the live AUTO swap consumed 202,842 units, past
	// one instruction's default meter.
	autoSwapComputeUnits = 300_000
)

// v1Budget is a transaction's compute budget, carried in its message config.
type v1Budget struct {
	units, heap uint32
}

// executionBudget is the budget of a lane's transaction of n instructions:
// the meter a legacy transaction had (200,000 a instruction, at most the
// runtime ceiling), at least an AUTO swap's need on the AUTO lanes, and the
// heap Squads needs: every Backyard transaction executes through a Squads
// policy.
func executionBudget(lane string, n int) v1Budget {
	units := uint32(min(maxComputeUnits, instructionComputeUnits*max(n, 1)))
	if autoCollateralLane(lane) {
		units = max(units, autoSwapComputeUnits)
	}
	return v1Budget{units: units, heap: squadsExecutionHeapBytes}
}

// compileV1Message compiles instructions into the one message shape, payer
// first and the other keys in role order (writable signers, read-only
// signers, writable, read-only), each group in first-use order with every
// instruction's program before its accounts. capture keys ride along as
// read-only keys so an unsigned simulation can return them. It refuses more
// than maxTransactionKeys keys; whether the signed transaction fits
// maxTransactionBytes is checkedUnsignedMessage's.
func compileV1Message(payer, blockhash publicKey, instructions []compiledInstruction, budget v1Budget, capture ...publicKey) ([]byte, error) {
	if len(instructions) == 0 || len(instructions) > solana.MaxInstructionsV1 || budget.units == 0 || budget.units > maxComputeUnits {
		return nil, fmt.Errorf("invalid v1 message inputs")
	}
	accounts := []accountMeta{{key: payer, signer: true, writable: true}}
	for _, ix := range instructions {
		pushOrMergeMeta(&accounts, accountMeta{key: ix.program})
		for _, a := range ix.accounts {
			pushOrMergeMeta(&accounts, a)
		}
	}
	for _, key := range capture {
		pushOrMergeMeta(&accounts, accountMeta{key: key})
	}
	if len(accounts) > maxTransactionKeys {
		return nil, fmt.Errorf("v1 message names %d keys, at most %d", len(accounts), maxTransactionKeys)
	}
	sort.SliceStable(accounts[1:], func(i, j int) bool { return accountRank(accounts[i+1]) < accountRank(accounts[j+1]) })
	var required, readonlySigned, readonlyUnsigned byte
	index := map[publicKey]byte{}
	for i, a := range accounts {
		index[a.key] = byte(i)
		if a.signer {
			required++
			if !a.writable {
				readonlySigned++
			}
		} else if !a.writable {
			readonlyUnsigned++
		}
	}
	if accounts[0].key != payer || required != 1 {
		return nil, fmt.Errorf("v1 message must have the payer as its one signer")
	}
	config := solana.TransactionConfig{}.WithComputeUnitLimit(budget.units).WithLoadedAccountsDataSizeLimit(loadedAccountsDataBytes)
	if budget.heap != 0 {
		config = config.WithHeapSize(budget.heap)
	}
	message := []byte{0x81, required, readonlySigned, readonlyUnsigned}
	message = binary.LittleEndian.AppendUint32(message, uint32(config.Mask()))
	message = append(message, blockhash[:]...)
	message = append(message, byte(len(instructions)), byte(len(accounts)))
	for _, a := range accounts {
		message = append(message, a.key[:]...)
	}
	message = binary.LittleEndian.AppendUint32(message, budget.units)
	message = binary.LittleEndian.AppendUint32(message, loadedAccountsDataBytes)
	if budget.heap != 0 {
		message = binary.LittleEndian.AppendUint32(message, budget.heap)
	}
	for _, ix := range instructions {
		if len(ix.accounts) > 255 || len(ix.data) > 0xFFFF {
			return nil, fmt.Errorf("v1 instruction is too large")
		}
		message = append(message, index[ix.program], byte(len(ix.accounts)))
		message = binary.LittleEndian.AppendUint16(message, uint16(len(ix.data)))
	}
	for _, ix := range instructions {
		for _, a := range ix.accounts {
			message = append(message, index[a.key])
		}
		message = append(message, ix.data...)
	}
	return message, nil
}

// distinctKeys is how many keys a message of instructions paid by payer
// names.
func distinctKeys(payer publicKey, instructions []compiledInstruction) int {
	seen := map[publicKey]bool{payer: true}
	for _, ix := range instructions {
		seen[ix.program] = true
		for _, a := range ix.accounts {
			seen[a.key] = true
		}
	}
	return len(seen)
}

// checkedUnsignedMessage admits the one message shape: v1 with one required
// signer, whose signed transaction fits maxTransactionBytes.
func checkedUnsignedMessage(message []byte) ([]byte, error) {
	if len(message) < 4 || message[0] != 0x81 || message[1] != 1 || len(message)+ed25519.SignatureSize > maxTransactionBytes {
		return nil, fmt.Errorf("unsigned message is not a single-signer v1 message within %d bytes", maxTransactionBytes)
	}
	return message, nil
}

// signedWire is message signed by signature: a v1 transaction is its
// message then its signatures, with no count.
func signedWire(message, signature []byte) []byte {
	return append(append([]byte(nil), message...), signature...)
}

// splitSignedWire is a one-signature wire's message and signature: a v1 wire
// (the message, then its signature), or one an older binary persisted (a
// signature count of one, the signature, then a legacy or v0 message), so a
// row in flight across the upgrade still lands and settles.
func splitSignedWire(wire []byte) ([]byte, []byte, error) {
	switch {
	case len(wire) > ed25519.SignatureSize+4 && wire[0] == 0x81 && wire[1] == 1:
		split := len(wire) - ed25519.SignatureSize
		return wire[:split], wire[split:], nil
	case len(wire) > 1+ed25519.SignatureSize && wire[0] == 1:
		return wire[1+ed25519.SignatureSize:], wire[1 : 1+ed25519.SignatureSize], nil
	}
	return nil, nil, fmt.Errorf("not a single-signature transaction")
}

// decodeV1Message is a v1 message as the cluster reads it.
func decodeV1Message(message []byte) (solana.Message, error) {
	var m solana.Message
	if len(message) == 0 || message[0] != 0x81 {
		return m, fmt.Errorf("not a v1 message")
	}
	tx, err := solana.TransactionFromBytes(signedWire(message, make([]byte, ed25519.SignatureSize)))
	if err != nil {
		return m, err
	}
	if marshaled, err := tx.Message.MarshalBinary(); err != nil || !bytes.Equal(marshaled, message) {
		return m, fmt.Errorf("v1 message is not canonical")
	}
	return tx.Message, nil
}

// messageKeys is a v1 message's keys: every account it can load.
func messageKeys(message []byte) ([]publicKey, error) {
	m, err := decodeV1Message(message)
	if err != nil {
		return nil, err
	}
	keys := make([]publicKey, len(m.AccountKeys))
	for i, key := range m.AccountKeys {
		keys[i] = publicKey(key)
	}
	return keys, nil
}
